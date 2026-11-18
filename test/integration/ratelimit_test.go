//go:build integration

package integration

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"deltav/internal/lab"
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
		Keys   int `json:"keys"`
		Envoys int `json:"envoys"`
		Top    []struct {
			Key string `json:"key"`
		} `json:"top"`
	} `json:"status"`
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

	// With the rules gone, everything passes again, checked byte for byte.
	sendAPI(t, s, http.MethodDelete, "/api/v1/rules/gets", nil)
	sendAPI(t, s, http.MethodDelete, "/api/v1/rules/no-puts", nil)
	waitMember(t, s, "it-envoy-1", "in sync without rules", inSync)
	waitMember(t, s, "it-envoy-2", "in sync without rules", inSync)
	requireClean(t, load(t, port, nil))
}
