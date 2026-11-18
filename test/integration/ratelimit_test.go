//go:build integration

package integration

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"

	"deltav/internal/lab"
	"deltav/internal/ratelimit"
	"deltav/internal/server"
)

const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// signedGet sends one signed GET through the listener.
func signedGet(t *testing.T, port uint32, path string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), nil)
	if err != nil {
		t.Fatal(err)
	}
	lab.Sign(req, *creds, emptySHA256, time.Now())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

type ruleStatus struct {
	Name     string `json:"name"`
	Sentence string `json:"sentence"`
	Status   struct {
		AllowedRate float64 `json:"allowed_rate"`
		DeniedRate  float64 `json:"denied_rate"`
		Totals      struct {
			Allowed uint64 `json:"allowed"`
			Denied  uint64 `json:"denied"`
		} `json:"totals"`
		Keys       int `json:"keys"`
		Envoys     int `json:"envoys"`
		MostEnvoys int `json:"most_envoys"`
		Top        []struct {
			Key string `json:"key"`
		} `json:"top"`
	} `json:"status"`
	Holds bool `json:"holds"`
}

func refusals(t *testing.T, s *server.Server) []ratelimit.Refusal {
	t.Helper()
	return getAPI[struct {
		Refusals []ratelimit.Refusal `json:"refusals"`
	}](t, s, "/api/v1/refusals?limit=1000").Refusals
}

// targetStats is what the overview says of the one target.
type targetStats struct {
	Requests uint64 `json:"requests"`
	Refused  uint64 `json:"refused"`
	Status   struct {
		S2xx uint64 `json:"2xx"`
		S4xx uint64 `json:"4xx"`
		S5xx uint64 `json:"5xx"`
		S503 uint64 `json:"503"`
	} `json:"status"`
}

func targetNow(t *testing.T, s *server.Server) targetStats {
	t.Helper()
	o := getAPI[struct {
		Targets []struct {
			Stats targetStats `json:"stats"`
		} `json:"targets"`
	}](t, s, "/api/v1/overview?window=5m")
	if len(o.Targets) != 1 {
		t.Fatalf("%d targets in the overview", len(o.Targets))
	}
	return o.Targets[0].Stats
}

func ruleNow(t *testing.T, s *server.Server, name string) ruleStatus {
	t.Helper()
	for _, r := range getAPI[struct {
		Rules []ruleStatus `json:"rules"`
	}](t, s, "/api/v1/rules").Rules {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no rule %q in the API", name)
	return ruleStatus{}
}

// gets and puts run for d through the listener, unchecked: a refused
// request is the point here, not a failure.
func gets(t *testing.T, port uint32, d time.Duration, workers int) *lab.LoadResult {
	t.Helper()
	return load(t, port, func(c *lab.LoadConfig) {
		c.Requests, c.Duration, c.Concurrency, c.PutRatio, c.Size, c.Check, c.SpecialKeys = 0, d, workers, 0, 1024, false, false
	})
}

func puts(t *testing.T, port uint32, d time.Duration, workers int) *lab.LoadResult {
	t.Helper()
	return load(t, port, func(c *lab.LoadConfig) {
		c.Requests, c.Duration, c.Concurrency, c.PutRatio, c.Size, c.Check = 0, d, workers, 1, 1024, false
	})
}

// only fails the test if a load saw anything but the listed statuses, or
// any broken connection: being refused must be orderly.
func only(t *testing.T, what string, res *lab.LoadResult, statuses ...int) {
	t.Helper()
	n := 0
	for _, st := range statuses {
		n += res.Status[st]
	}
	if res.TransportErrors != 0 || n != res.Requests {
		t.Errorf("%s: %d requests, status %v, %d transport errors; want only %v\n%s",
			what, res.Requests, res.Status, res.TransportErrors, statuses, strings.Join(res.Samples, "\n"))
	}
}

// Request-rate rules against real Envoy: the script that recognizes
// requests, a quota filter per rule, and the daemon's quota service handing
// out shares.
func TestRateRulesThroughEnvoy(t *testing.T) {
	s, _, _, _, port := vastTarget(t, lab.FakeS3Config{})
	setRule := func(name string, body map[string]any) {
		t.Helper()
		sendAPI(t, s, http.MethodPut, "/api/v1/rules/"+name, body)
		waitMember(t, s, "it-envoy-1", "in sync with the rules", inSync)
	}

	// GetObject is limited to 20 a second for each access key; nothing
	// else is.
	setRule("gets", map[string]any{"operation": []string{"GetObject"}, "limit_per_second": 20, "burst": 20, "per": []string{"access_key"}})
	done := make(chan *lab.LoadResult, 1)
	go func() { done <- puts(t, port, 6*time.Second, 4) }()
	time.Sleep(time.Second) // let the PUTs create what the GETs read
	got := gets(t, port, 6*time.Second, 8)
	put := <-done
	t.Logf("20 GetObject a second for 6 s, one Envoy: GETs %v, PUTs %v", got.Status, put.Status)
	only(t, "GETs under the rule", got, 200, 503)
	requireUnlimited := func(what string, res *lab.LoadResult) {
		t.Helper()
		only(t, what, res, 200)
	}
	requireUnlimited("PUTs, which the rule does not name", put)
	// 6 seconds at 20 a second, and a burst of 20 to start with.
	if ok := got.Status[200]; ok < 100 || ok > 190 {
		t.Errorf("%d GETs were let through in 6 s; the rule allows about 140", ok)
	}
	if got.Status[503] < got.Status[200] {
		t.Errorf("8 workers asked for far more than 20 a second, yet only %d were refused", got.Status[503])
	}

	// What a refused request is told: S3's own throttling answer.
	go func() { done <- gets(t, port, 2*time.Second, 8) }()
	time.Sleep(500 * time.Millisecond)
	var refused *http.Response
	var body string
	for i := 0; i < 50 && (refused == nil || refused.StatusCode != http.StatusServiceUnavailable); i++ {
		refused, body = signedGet(t, port, "/lab-bucket/dvlab/any")
	}
	<-done
	if refused.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "<Code>SlowDown</Code>") || !strings.Contains(body, "<Message>Please reduce your request rate.</Message>") ||
		refused.Header.Get("Content-Type") != "application/xml" || refused.Header.Get("Retry-After") != "1" {
		t.Errorf("a refused request got %d, headers %v, body %q", refused.StatusCode, refused.Header, body)
	}
	// What an allowed one looks like to the backend: none of the script's
	// own headers.
	resp, _ := signedGet(t, port, "/lab-bucket?list-type=2&prefix=dvlab/")
	seen := resp.Header.Get("X-Fake-Seen-Headers")
	if resp.StatusCode != http.StatusOK || seen == "" || strings.Contains(seen, "deltav") {
		t.Errorf("a listing: status %d; the backend saw headers %q", resp.StatusCode, seen)
	}

	// The API says what Envoy reported.
	var st ruleStatus
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(300 * time.Millisecond) {
		if st = ruleNow(t, s, "gets"); st.Status.Totals.Denied > 0 {
			break
		}
	}
	if st.Status.Totals.Denied == 0 || st.Status.Totals.Allowed == 0 || st.Status.Keys != 1 || st.Status.Envoys != 1 ||
		len(st.Status.Top) != 1 || st.Status.Top[0].Key != creds.AccessKey {
		t.Errorf("the rule's use in the API: %+v", st)
	}
	t.Logf("the API: %s  allowed %d, refused %d, counted under %q", st.Sentence, st.Status.Totals.Allowed, st.Status.Totals.Denied, st.Status.Top[0].Key)
	if !st.Holds || st.Status.MostEnvoys != 1 {
		t.Errorf("one Envoy, a burst of 20: holds %v, most Envoys on a count %d", st.Holds, st.Status.MostEnvoys)
	}

	// Refused requests are not the target's requests. They never reached
	// it: the target's own counts hold what did, with no 503 among them
	// (these backends send none), and the refusals are counted apart.
	var ts targetStats
	sent := uint64(got.Requests + put.Requests)
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if ts = targetNow(t, s); ts.Requests+ts.Refused >= sent {
			break
		}
	}
	if ts.Refused < uint64(got.Status[503]) || ts.Status.S503 != 0 || ts.Status.S5xx != 0 || ts.Requests > uint64(got.Status[200]+put.Requests)+200 {
		t.Errorf("the target's counts with a rule refusing: %+v; the loads saw GETs %v, PUTs %v", ts, got.Status, put.Status)
	}
	t.Logf("the target, as the overview counts it: %d requests (%d 5xx), and apart from those %d refused by rules", ts.Requests, ts.Status.S5xx, ts.Refused)
	// And each refusal is on record: who, what, under which rule.
	rec := refusals(t, s)
	if len(rec) == 0 {
		t.Fatal("no refusal is on record")
	}
	if r := rec[0]; r.Client != "it-envoy-1" || r.Blocked || len(r.Rules) != 1 || r.Rules[0] != "gets" || r.Operation != "GetObject" ||
		r.AccessKey != creds.AccessKey || r.Bucket != "lab-bucket" || r.Status != 503 || r.At.IsZero() || time.Since(r.At) > time.Minute {
		t.Errorf("the latest refusal on record: %+v", r)
	}

	// A second Envoy on the same port. The rule's 20 a second is the
	// fleet's: the two share it, they do not have 20 each.
	startEnvoy(t, s, "it-envoy-2", envoyWorkers, true)
	waitMember(t, s, "it-envoy-2", "connected and in sync", inSync)
	time.Sleep(500 * time.Millisecond)
	got = gets(t, port, 12*time.Second, 16)
	st = ruleNow(t, s, "gets")
	t.Logf("20 GetObject a second for 12 s, two Envoys: %v; Envoys with the count open: %d", got.Status, st.Status.Envoys)
	only(t, "GETs through two Envoys", got, 200, 503)
	if st.Status.Envoys != 2 {
		t.Errorf("%d Envoys have the count open; the load should have reached both", st.Status.Envoys)
	}
	// 240 at the rate. Each Envoy may start with a burst, and until the
	// first reports are in (5 s) the split is a guess: allow for that, but
	// not for 20 a second each, which would be 480 and more.
	if ok := got.Status[200]; ok < 180 || ok > 400 {
		t.Errorf("%d GETs were let through in 12 s by two Envoys; a shared 20 a second allows about 240 to 300", ok)
	}

	// The rate is raised while requests flow. Nothing breaks, and much
	// more gets through.
	go func() { done <- gets(t, port, 10*time.Second, 16) }()
	time.Sleep(2 * time.Second)
	setRule("gets", map[string]any{"operation": []string{"GetObject"}, "limit_per_second": 500, "per": []string{"access_key"}})
	got = <-done
	t.Logf("raised to 500 a second 2 s into 10 s: %v", got.Status)
	only(t, "GETs while the rule was changed", got, 200, 503)
	if ok := got.Status[200]; ok < 1500 {
		t.Errorf("%d GETs were let through; at 500 a second for most of 10 s it should be thousands", ok)
	}

	// A blocked rule refuses everything it matches, with the status it names.
	setRule("no-puts", map[string]any{"operation": []string{"PutObject"}, "bucket": []string{"lab-bucket"}, "blocked": true, "respond": 429})
	put = puts(t, port, 2*time.Second, 4)
	t.Logf("PutObject blocked: %v", put.Status)
	only(t, "PUTs under a blocked rule", put, 429)
	if put.Requests == 0 {
		t.Error("no PUT was sent")
	}
	// A blocked rule is the script's doing, with no count at the quota
	// service; the daemon still knows of every refusal, from the log.
	var blocked ruleStatus
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if blocked = ruleNow(t, s, "no-puts"); blocked.Status.Totals.Denied >= uint64(put.Requests) {
			break
		}
	}
	if blocked.Status.Totals.Denied != uint64(put.Requests) || blocked.Status.Keys != 0 || !blocked.Holds {
		t.Errorf("the blocked rule in the API after %d refused PUTs: %+v", put.Requests, blocked)
	}
	if r := refusals(t, s)[0]; !r.Blocked || len(r.Rules) != 1 || r.Rules[0] != "no-puts" || r.Operation != "PutObject" || r.Status != 429 || r.Bucket != "lab-bucket" {
		t.Errorf("the latest refusal on record after a blocked rule: %+v", r)
	}

	// With the rules gone, everything passes again, checked byte for byte.
	sendAPI(t, s, http.MethodDelete, "/api/v1/rules/gets", nil)
	sendAPI(t, s, http.MethodDelete, "/api/v1/rules/no-puts", nil)
	waitMember(t, s, "it-envoy-1", "in sync without rules", inSync)
	waitMember(t, s, "it-envoy-2", "in sync without rules", inSync)
	requireClean(t, load(t, port, nil))
}

// oneConnection is an S3 client that keeps a single connection open, as
// S3 clients do, and knows whether each request went out on it or had to
// open another.
type oneConnection struct {
	t      *testing.T
	client *http.Client
	port   uint32
	opened int  // connections opened so far
	reused bool // the last request went out on a connection already open
}

func newOneConnection(t *testing.T, port uint32) *oneConnection {
	tr := &http.Transport{MaxConnsPerHost: 1, MaxIdleConnsPerHost: 1, IdleConnTimeout: 10 * time.Minute}
	t.Cleanup(tr.CloseIdleConnections)
	return &oneConnection{t: t, client: &http.Client{Transport: tr}, port: port}
}

// do sends one signed request and returns the status, the body, and the
// names of the request headers the backend received. unsigned are headers
// added after signing, as name, value, ...
func (c *oneConnection) do(method, path string, unsigned ...string) (int, string, string) {
	c.t.Helper()
	var body io.Reader
	hash := lab.EmptyPayloadHash
	if method == http.MethodPut {
		body, hash = strings.NewReader("held open"), lab.UnsignedPayload
	}
	req, err := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", c.port, path), body)
	if err != nil {
		c.t.Fatal(err)
	}
	lab.Sign(req, *creds, hash, time.Now())
	for i := 0; i+1 < len(unsigned); i += 2 {
		req.Header.Set(unsigned[i], unsigned[i+1])
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			if c.reused = info.Reused; !info.Reused {
				c.opened++
			}
		},
	}))
	resp, err := c.client.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header.Get(lab.HeaderSeenHeaders)
}

// A rule reaches connections that are already open. S3 clients keep their
// connections for as long as they can, so a rule that applied only to new
// ones would, for a busy client, not apply. This is why rules travel in the
// route and not in the listener.
func TestRuleChangeReachesOpenConnections(t *testing.T) {
	s, _, _, _, port := vastTarget(t, lab.FakeS3Config{})
	sync := func(what string) {
		t.Helper()
		waitMember(t, s, "it-envoy-1", what, inSync)
		time.Sleep(300 * time.Millisecond) // an ACK is Envoy's main thread; its workers follow
	}
	c := newOneConnection(t, port)
	const key = "/lab-bucket/dvlab/held-open"

	// No rules: requests pass as they came, down to a header that happens
	// to be named like the script's own.
	if status, _, _ := c.do(http.MethodPut, key); status != 200 {
		t.Fatalf("with no rules: PUT %d", status)
	}
	status, _, seen := c.do(http.MethodGet, key, "x-deltav-rl-1", "the client's own")
	if status != 200 || !strings.Contains(seen, "x-deltav-rl-1") {
		t.Fatalf("with no rules: GET %d; the backend saw headers %q", status, seen)
	}

	// A rule is added that blocks GetObject. The next GET, on the same
	// connection, is refused.
	sendAPI(t, s, http.MethodPut, "/api/v1/rules/gets", map[string]any{"operation": []string{"GetObject"}, "blocked": true})
	sync("in sync with a blocked rule")
	status, body, _ := c.do(http.MethodGet, key)
	if status != 503 || !strings.Contains(body, "<Code>SlowDown</Code>") || !c.reused {
		t.Errorf("a GET on a connection opened before GetObject was blocked: %d %q (on the old connection: %v)", status, body, c.reused)
	}
	// What the rule does not name still passes, and now the script's
	// header names are the script's own: a client's is dropped.
	status, _, seen = c.do(http.MethodHead, key, "x-deltav-rl-1", "the client's own")
	if status != 200 || strings.Contains(seen, "deltav") || !c.reused {
		t.Errorf("a HEAD under a rule that blocks GETs: %d; the backend saw headers %q (on the old connection: %v)", status, seen, c.reused)
	}

	// The rule is changed to a limit: one GET a second in total. On the
	// same connection, the first passes and the ones right behind it do not.
	sendAPI(t, s, http.MethodPut, "/api/v1/rules/gets", map[string]any{"operation": []string{"GetObject"}, "limit_per_second": 1, "burst": 1, "per": []string{"total"}, "respond": 429})
	sync("in sync with the rule as a limit")
	var statuses []int
	ok, refused := 0, 0
	for i := 0; i < 8; i++ {
		status, _, _ = c.do(http.MethodGet, key)
		statuses = append(statuses, status)
		switch status {
		case 200:
			ok++
		case 429:
			refused++
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The first of a new count passes before the daemon has answered; the
	// burst is one more.
	if ok < 1 || ok > 3 || ok+refused != len(statuses) {
		t.Errorf("8 GETs in a fifth of a second under one a second: %v", statuses)
	}

	// The rule is removed: the same connection is free again.
	sendAPI(t, s, http.MethodDelete, "/api/v1/rules/gets", nil)
	sync("in sync without the rule")
	for i := 0; i < 5; i++ {
		if status, _, _ = c.do(http.MethodGet, key); status != 200 {
			t.Errorf("GET %d after the rule was removed: %d", i+1, status)
		}
	}
	if c.opened != 1 {
		t.Errorf("the client had to open %d connections; every change should have reached the one it had", c.opened)
	}

	// An upload too is refused on the connection it arrives on. That
	// connection Envoy then closes, as a server must that answers before it
	// has read the request's body; the client opens another for its retry.
	sendAPI(t, s, http.MethodPut, "/api/v1/rules/puts", map[string]any{"operation": []string{"PutObject"}, "blocked": true})
	sync("in sync with uploads blocked")
	if status, _, _ = c.do(http.MethodPut, key); status != 503 || !c.reused {
		t.Errorf("a PUT on a connection opened before PutObject was blocked: %d (on the old connection: %v)", status, c.reused)
	}
}

// A slow rule cannot be beaten by pausing. Twelve an hour with a burst of
// three: once the three are spent, forty quiet seconds earn a client a
// seventh of a request, not three more.
func TestSlowRuleHoldsThroughAPause(t *testing.T) {
	if testing.Short() {
		t.Skip("waits 40 seconds")
	}
	s, _, _, _, port := vastTarget(t, lab.FakeS3Config{})
	sendAPI(t, s, http.MethodPut, "/api/v1/rules/slow", map[string]any{"operation": []string{"GetObject"}, "limit_per_hour": 12, "burst": 3, "per": []string{"total"}})
	waitMember(t, s, "it-envoy-1", "in sync with the rule", inSync)
	time.Sleep(300 * time.Millisecond)
	c := newOneConnection(t, port)
	if status, _, _ := c.do(http.MethodPut, "/lab-bucket/dvlab/slow"); status != 200 {
		t.Fatalf("PUT: %d", status)
	}
	round := func() (ok int) {
		for i := 0; i < 10; i++ {
			if status, _, _ := c.do(http.MethodGet, "/lab-bucket/dvlab/slow"); status == 200 {
				ok++
			}
			time.Sleep(50 * time.Millisecond)
		}
		return ok
	}
	first := round()
	// The burst, and the one that passes before the daemon has answered.
	if first < 3 || first > 4 {
		t.Errorf("%d of 10 GETs passed at first; the rule's burst is 3", first)
	}
	// Longer than the half minute after which the daemon used to give an
	// idle count up, which handed the client a new burst.
	time.Sleep(40 * time.Second)
	second := round()
	if second > 1 {
		t.Errorf("%d of 10 GETs passed after a 40 s pause; at 12 an hour the pause earned none", second)
	}
	st := ruleNow(t, s, "slow")
	t.Logf("12 an hour, burst 3: %d passed, then after 40 s of silence %d; the count is still open on %d Envoy", first, second, st.Status.Envoys)
	if st.Status.Keys != 1 {
		t.Errorf("the count was given up during the pause: %+v", st)
	}
}
