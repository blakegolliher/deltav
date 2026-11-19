package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"deltav/internal/metrics"
	"deltav/internal/vast/vasttest"
)

const vastFixtures = "../../testdata/vast/v5.5.3"

func mockVAST(t *testing.T) *vasttest.Server {
	t.Helper()
	m, err := vasttest.New(vastFixtures)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m
}

// run starts a daemon and returns it with a function that stops it. The
// poll interval is an hour, so the only polls after the first are the ones
// a test asks for.
func run(t *testing.T, stateDir string) (*Server, func()) {
	t.Helper()
	s, err := New(Config{
		GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0", StateDir: stateDir, VASTPollInterval: time.Hour,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("Run did not return after cancel")
		}
	}
	t.Cleanup(stop)
	return s, stop
}

func call(t *testing.T, s *Server, method, path string, body any) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "http://"+s.HTTPAddr()+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func addCluster(t *testing.T, s *Server, m *vasttest.Server) {
	t.Helper()
	code, body := call(t, s, "POST", "/api/v1/vast", map[string]any{
		"name": "lab", "host": m.Host(), "user": vasttest.User, "password": vasttest.Password, "insecure": true,
	})
	if code != http.StatusCreated {
		t.Fatalf("attaching the cluster: %d %s", code, body)
	}
}

func addTarget(t *testing.T, s *Server, name, pool string, listenPort int) targetJSON {
	t.Helper()
	code, body := call(t, s, "POST", "/api/v1/targets", map[string]any{
		"name": name, "cluster": "lab", "pool": pool, "port": 80, "listen_port": listenPort,
	})
	var tj targetJSON
	if code != http.StatusCreated || json.Unmarshal([]byte(body), &tj) != nil {
		t.Fatalf("adding target %s: %d %s", name, code, body)
	}
	return tj
}

func target(t *testing.T, s *Server, name string) targetJSON {
	t.Helper()
	for _, tj := range getJSON[targetsResponse](t, s, "/api/v1/targets").Targets {
		if tj.Name == name {
			return tj
		}
	}
	t.Fatalf("no target %q", name)
	return targetJSON{}
}

func perCNode(tj targetJSON) map[string]int {
	out := map[string]int{}
	for _, e := range tj.Endpoints {
		out[e.CNode]++
	}
	return out
}

func TestVASTClusterAttach(t *testing.T) {
	m := mockVAST(t)
	dir := t.TempDir()
	s, _ := run(t, dir)

	// A cluster that cannot be read is not stored.
	code, body := call(t, s, "POST", "/api/v1/vast", map[string]any{
		"name": "lab", "host": m.Host(), "user": vasttest.User, "password": "not-the-password", "insecure": true,
	})
	if code != http.StatusBadGateway || !strings.Contains(body, "login") || strings.Contains(body, "not-the-password") {
		t.Errorf("wrong password: %d %s", code, body)
	}
	// The mock's certificate is self-signed: refused unless the cluster
	// opts out of verification.
	code, body = call(t, s, "POST", "/api/v1/vast", map[string]any{
		"name": "lab", "host": m.Host(), "user": vasttest.User, "password": vasttest.Password,
	})
	if code != http.StatusBadGateway || !strings.Contains(body, "certificate") {
		t.Errorf("unverified certificate: %d %s", code, body)
	}
	for what, in := range map[string]map[string]any{
		"unknown field": {"name": "lab", "host": m.Host(), "user": "u", "password": "p", "pasword": "typo"},
		"no host":       {"name": "lab", "user": "u", "password": "p"},
	} {
		if code, body := call(t, s, "POST", "/api/v1/vast", in); code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", what, code, body)
		}
	}
	if got := getJSON[struct{ Clusters []clusterJSON }](t, s, "/api/v1/vast"); len(got.Clusters) != 0 {
		t.Fatalf("a failed attach left a cluster behind: %+v", got)
	}

	addCluster(t, s, m)
	code, body = call(t, s, "GET", "/api/v1/vast", nil)
	var list struct{ Clusters []clusterJSON }
	if code != 200 || json.Unmarshal([]byte(body), &list) != nil || len(list.Clusters) != 1 {
		t.Fatalf("GET /api/v1/vast: %d %s", code, body)
	}
	c := list.Clusters[0]
	if c.Name != "lab" || c.Host != m.Host() || c.User != vasttest.User || !c.Insecure || c.LastPoll == nil || c.LastError != "" ||
		c.CNodes != 3 || c.Pools != 48 || c.VIPs != 187 {
		t.Errorf("cluster: %+v", c)
	}
	// The password went in and never comes out.
	if strings.Contains(body, vasttest.Password) || strings.Contains(strings.ToLower(body), "password") {
		t.Errorf("the cluster listing mentions the password: %s", body)
	}
	if code, body := call(t, s, "POST", "/api/v1/vast", map[string]any{"name": "lab", "host": m.Host(), "user": "u", "password": "p"}); code != http.StatusConflict {
		t.Errorf("attaching twice: %d %s", code, body)
	}

	// It is on disk, privately, so the daemon can log in after a restart.
	st, err := os.Stat(filepath.Join(dir, "state.json"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("state file: %v, mode %v", err, st.Mode().Perm())
	}

	// Pools, with the spread of their VIPs over CNodes.
	pools := getJSON[struct{ Pools []poolJSON }](t, s, "/api/v1/vast/lab/pools").Pools
	if len(pools) != 48 {
		t.Fatalf("%d pools", len(pools))
	}
	uneven := 0
	for _, p := range pools {
		if p.Name == "main" && (p.VIPs != 6 || p.Tenant != "default" || p.Role != "PROTOCOLS" || len(p.PerCNode) != 3 || p.PerCNode["cnode-3-10"] != 2) {
			t.Errorf("pool main: %+v", p)
		}
		lo, hi := 1<<30, 0
		for _, n := range p.PerCNode {
			lo, hi = min(lo, n), max(hi, n)
		}
		if len(p.PerCNode) < 3 || lo != hi {
			uneven++
		}
	}
	if uneven == 0 {
		t.Error("the fixtures have no pool with an uneven spread; DESIGN amendment 1 has nothing to test against")
	}
	if code, _ := call(t, s, "GET", "/api/v1/vast/nope/pools", nil); code != http.StatusNotFound {
		t.Errorf("pools of an unknown cluster: %d", code)
	}

	// Detach.
	addTarget(t, s, "s3", "main", 9000)
	if code, body := call(t, s, "DELETE", "/api/v1/vast/lab", nil); code != http.StatusConflict || !strings.Contains(body, "s3") {
		t.Errorf("detaching a cluster in use: %d %s", code, body)
	}
	if code, _ := call(t, s, "DELETE", "/api/v1/targets/s3", nil); code != http.StatusNoContent {
		t.Errorf("removing the target: %d", code)
	}
	if code, _ := call(t, s, "DELETE", "/api/v1/vast/lab", nil); code != http.StatusNoContent {
		t.Errorf("detaching: %d", code)
	}
	if code, _ := call(t, s, "DELETE", "/api/v1/vast/lab", nil); code != http.StatusNotFound {
		t.Errorf("detaching twice: %d", code)
	}
	if n := m.Mutating(); n != 0 {
		t.Errorf("%d requests other than GET and the token call reached the cluster", n)
	}
}

func TestTargetsFollowTheCluster(t *testing.T) {
	m := mockVAST(t)
	s, _ := run(t, t.TempDir())
	addCluster(t, s, m)
	ctx := context.Background()

	for what, in := range map[string]struct {
		body map[string]any
		code int
		want string
	}{
		"unknown cluster": {map[string]any{"name": "x", "cluster": "nope", "pool": "main", "port": 80, "listen_port": 9001}, 404, "no cluster"},
		"unknown pool":    {map[string]any{"name": "x", "cluster": "lab", "pool": "no-such-pool", "port": 80, "listen_port": 9001}, 400, `no VIP pool \"no-such-pool\"`},
		"wrong tenant":    {map[string]any{"name": "x", "cluster": "lab", "pool": "main", "tenant": "other", "port": 80, "listen_port": 9001}, 400, `in tenant \"other\"`},
		"no listen port":  {map[string]any{"name": "x", "cluster": "lab", "pool": "main", "port": 80}, 400, "1-65535"},
		"TLS without SNI": {map[string]any{"name": "x", "cluster": "lab", "pool": "main", "port": 443, "listen_port": 9001, "tls": map[string]any{}}, 400, "server name"},
	} {
		if code, body := call(t, s, "POST", "/api/v1/targets", in.body); code != in.code || !strings.Contains(body, in.want) {
			t.Errorf("%s: %d %s", what, code, body)
		}
	}

	tj := addTarget(t, s, "s3", "main", 9000)
	if tj.Source != "vast" || tj.Cluster != "lab" || tj.Pool != "main" || tj.Port != 80 || tj.ListenPort != 9000 || tj.TLS {
		t.Errorf("new target: %+v", tj)
	}
	if tj.Status == nil || tj.Status.VIPs != 6 || tj.Status.Healthy != 6 || tj.Status.Problem != "" || tj.Status.Held {
		t.Errorf("new target status: %+v", tj.Status)
	}
	if len(tj.Endpoints) != 6 || len(perCNode(tj)) != 3 {
		t.Fatalf("new target endpoints: %+v", tj.Endpoints)
	}
	for i, e := range tj.Endpoints {
		if e.Port != 80 || e.CNode == "" || (i > 0 && e.IP <= tj.Endpoints[i-1].IP) {
			t.Errorf("endpoint %d: %+v (want port 80, a CNode, address order)", i, e)
		}
	}
	if code, body := call(t, s, "POST", "/api/v1/targets", map[string]any{"name": "s3", "cluster": "lab", "pool": "main", "port": 80, "listen_port": 9005}); code != 409 {
		t.Errorf("same name again: %d %s", code, body)
	}
	if code, body := call(t, s, "POST", "/api/v1/targets", map[string]any{"name": "s3b", "cluster": "lab", "pool": "main", "port": 80, "listen_port": 9000}); code != 409 {
		t.Errorf("same listen port again: %d %s", code, body)
	}
	// What Envoys are served: the same six, logging access to the daemon.
	served := s.XDS().Targets()
	if len(served) != 1 || served[0].Name != "s3" || len(served[0].Endpoints) != 6 || !served[0].AccessLog || served[0].Endpoints[0].CNode == "" {
		t.Fatalf("served: %+v", served)
	}
	v1 := s.XDS().Version()

	// A CNode leaves service: its two VIPs go.
	if err := m.SetCNode("cnode-3-11", "INACTIVE", true); err != nil {
		t.Fatal(err)
	}
	s.poller.pollOnce(ctx)
	tj = target(t, s, "s3")
	if n := perCNode(tj); len(tj.Endpoints) != 4 || n["cnode-3-11"] != 0 || tj.Status.VIPs != 6 || tj.Status.Healthy != 4 {
		t.Errorf("after cnode-3-11 went INACTIVE: %v, status %+v", n, tj.Status)
	}
	if s.XDS().Version() == v1 {
		t.Error("the config version did not change")
	}
	// Disabled counts the same as not ACTIVE.
	_ = m.SetCNode("cnode-3-11", "ACTIVE", false)
	s.poller.pollOnce(ctx)
	if tj = target(t, s, "s3"); len(tj.Endpoints) != 4 {
		t.Errorf("a disabled CNode's VIPs are served: %v", perCNode(tj))
	}
	_ = m.SetCNode("cnode-3-11", "ACTIVE", true)
	s.poller.pollOnce(ctx)
	if tj = target(t, s, "s3"); len(tj.Endpoints) != 6 {
		t.Errorf("after recovery: %v", perCNode(tj))
	}

	// A VIP fails over to another CNode: same endpoints, new host.
	var moved string
	for _, e := range tj.Endpoints {
		if e.CNode == "cnode-3-11" {
			moved = e.IP
			break
		}
	}
	if err := m.MoveVIP(moved, "cnode-3-12"); err != nil {
		t.Fatal(err)
	}
	s.poller.pollOnce(ctx)
	tj = target(t, s, "s3")
	if n := perCNode(tj); len(tj.Endpoints) != 6 || n["cnode-3-11"] != 1 || n["cnode-3-12"] != 3 {
		t.Errorf("after moving %s: %v", moved, n)
	}

	// VAST stops answering: nothing changes for Envoys, and the API says so.
	before := s.XDS().Version()
	lastPoll := getJSON[struct{ Clusters []clusterJSON }](t, s, "/api/v1/vast").Clusters[0].LastPoll
	m.FailNext(1)
	s.poller.pollOnce(ctx)
	c := getJSON[struct{ Clusters []clusterJSON }](t, s, "/api/v1/vast").Clusters[0]
	if c.LastError == "" || c.LastErrorAt == nil || !c.LastPoll.Equal(*lastPoll) || c.VIPs != 187 {
		t.Errorf("during an outage: %+v", c)
	}
	if tj = target(t, s, "s3"); len(tj.Endpoints) != 6 || s.XDS().Version() != before {
		t.Errorf("an outage changed what is served: %d endpoints, version %s -> %s", len(tj.Endpoints), before, s.XDS().Version())
	}
	s.poller.pollOnce(ctx)
	if c = getJSON[struct{ Clusters []clusterJSON }](t, s, "/api/v1/vast").Clusters[0]; c.LastError != "" {
		t.Errorf("after the outage: %+v", c)
	}

	// The pool loses every VIP. One odd answer is not believed: the last
	// endpoints are held for two polls, then the target is empty.
	if n := m.RemovePoolVIPs("main"); n != 6 {
		t.Fatalf("removed %d VIPs", n)
	}
	for poll := 1; poll <= 2; poll++ {
		s.poller.pollOnce(ctx)
		if tj = target(t, s, "s3"); len(tj.Endpoints) != 6 || !tj.Status.Held || tj.Status.VIPs != 0 {
			t.Errorf("poll %d after the pool emptied: %d endpoints, status %+v", poll, len(tj.Endpoints), tj.Status)
		}
		// It is polls that are counted. An operator's commands rebuild the
		// config too, any number of times between two polls, and each sees
		// the same one answer from VAST: they must not use the hold up.
		for i := 0; i < 4; i++ {
			if code, body := call(t, s, "PUT", "/api/v1/rules/meanwhile", map[string]any{"limit_per_second": 5 + i, "per": []string{"total"}}); code != 200 {
				t.Fatalf("setting a rule: %d %s", code, body)
			}
		}
		if code, _ := call(t, s, "DELETE", "/api/v1/rules/meanwhile", nil); code != 204 {
			t.Fatalf("removing a rule: %d", code)
		}
		if tj = target(t, s, "s3"); len(tj.Endpoints) != 6 || !tj.Status.Held {
			t.Errorf("after poll %d and five commands: %d endpoints, status %+v", poll, len(tj.Endpoints), tj.Status)
		}
	}
	s.poller.pollOnce(ctx)
	if tj = target(t, s, "s3"); len(tj.Endpoints) != 0 || tj.Status.Held {
		t.Errorf("third poll after the pool emptied: %d endpoints, status %+v", len(tj.Endpoints), tj.Status)
	}

	if n := m.Mutating(); n != 0 {
		t.Errorf("%d requests other than GET and the token call reached the cluster", n)
	}
	for _, path := range []string{"/api/v1/vast", "/api/v1/targets", "/api/v1/cnodes", "/api/v1/overview", "/metrics"} {
		if _, body := call(t, s, "GET", path, nil); strings.Contains(body, vasttest.Password) {
			t.Errorf("%s contains the cluster password", path)
		}
	}
}

// One address from VAST that is not an address costs that VIP, and nothing
// else: the rest of the pool is served, other targets and rules can still
// change, and the target says what it left out.
func TestUnusableVIPAddress(t *testing.T) {
	m := mockVAST(t)
	s, _ := run(t, t.TempDir())
	addCluster(t, s, m)
	ctx := context.Background()
	addTarget(t, s, "s3", "main", 9000)
	if err := m.SetPoolVIPs("main", []string{"192.0.2.1", "192.0.2.2", "not-an-address", "192.0.2.4", "", "2001:db8::6"}); err != nil {
		t.Fatal(err)
	}
	s.poller.pollOnce(ctx)
	tj := target(t, s, "s3")
	if len(tj.Endpoints) != 4 || tj.Status.VIPs != 6 || tj.Status.Unusable != 2 || tj.Status.Problem != "" || tj.Status.Held {
		t.Fatalf("with two addresses that are not addresses: %d endpoints, status %+v", len(tj.Endpoints), tj.Status)
	}
	for _, e := range tj.Endpoints {
		if e.IP == "not-an-address" || e.IP == "" {
			t.Errorf("served %+v", e)
		}
	}
	// What used to follow: nothing could be served, so nothing could change.
	before := s.XDS().Version()
	if code, body := call(t, s, "PUT", "/api/v1/rules/still-works", map[string]any{"limit_per_second": 5, "per": []string{"total"}}); code != 200 {
		t.Errorf("setting a rule: %d %s", code, body)
	}
	if s.XDS().Version() == before {
		t.Error("a rule change was not served")
	}
}

// Rate rules read a request's bucket where VAST would: in the host name, if
// that is under the cluster's DNS suffix. The daemon reads the suffix and
// the pools' own labels from the cluster and hands them to the script. If it
// cannot read the DNS settings, that fails nothing else, and it says so.
func TestBucketHostNamesComeFromTheCluster(t *testing.T) {
	m := mockVAST(t)
	dir := t.TempDir()
	s, stop := run(t, dir)
	addCluster(t, s, m)
	ctx := context.Background()
	addTarget(t, s, "s3", "main", 9000)
	if code, body := call(t, s, "PUT", "/api/v1/rules/no-archive", map[string]any{"bucket": []string{"archive"}, "blocked": true}); code != 200 {
		t.Fatalf("setting a rule: %d %s", code, body)
	}
	names := func(s *Server) string {
		t.Helper()
		_, script, _ := served(t, s, "s3")
		for _, line := range strings.Split(script, "\n") {
			if strings.HasPrefix(line, "local SUFFIXES, POOLS = ") {
				return strings.TrimPrefix(line, "local SUFFIXES, POOLS = ")
			}
		}
		t.Fatal("the script does not say what host names it knows")
		return ""
	}
	// The fixture's suffix, and its pools' labels: main among them.
	got := names(s)
	if !strings.HasPrefix(got, `{"dns1.example.com"}, {`) || !strings.Contains(got, `"main"`) || !strings.Contains(got, `"domain-1"`) {
		t.Errorf("the script knows %s", got)
	}
	tj := target(t, s, "s3")
	if len(tj.Status.BucketHosts) != 1 || tj.Status.BucketHosts[0] != "*.dns1.example.com" || tj.Status.BucketHostsProblem != "" {
		t.Errorf("status: %+v", tj.Status)
	}

	// The suffix changes on the cluster: the script follows.
	m.SetDNSSuffix("S3.Example.Net")
	s.poller.pollOnce(ctx)
	if got := names(s); !strings.HasPrefix(got, `{"s3.example.net"}, {`) {
		t.Errorf("after the suffix changed, the script knows %s", got)
	}

	// The DNS settings can no longer be read. Endpoints are served as
	// before, the names last read stay in force, and the target says why
	// they may be out of date.
	m.Forbid("dns", true)
	before := len(tj.Endpoints)
	s.poller.pollOnce(ctx)
	tj = target(t, s, "s3")
	if len(tj.Endpoints) != before || tj.Status.Problem != "" || tj.Status.Held {
		t.Errorf("with DNS unreadable: %d endpoints (were %d), status %+v", len(tj.Endpoints), before, tj.Status)
	}
	if !strings.Contains(tj.Status.BucketHostsProblem, "DNS settings could not be read") || len(tj.Status.BucketHosts) != 1 {
		t.Errorf("with DNS unreadable, status: %+v", tj.Status)
	}
	if got := names(s); !strings.HasPrefix(got, `{"s3.example.net"}, {`) {
		t.Errorf("with DNS unreadable, the script knows %s", got)
	}
	if m.Mutating() != 0 {
		t.Errorf("%d requests to VAST that were not GETs or token calls", m.Mutating())
	}

	// A restarted daemon that cannot reach VAST at all still knows them.
	stop()
	m.Close()
	s2, _ := run(t, dir)
	if got := names(s2); !strings.HasPrefix(got, `{"s3.example.net"}, {`) || !strings.Contains(got, `"main"`) {
		t.Errorf("after a restart without VAST, the script knows %s", got)
	}
}

// A TLS target must say what to verify VAST's certificate against. One
// stored by an earlier build, which let it be left out, would be refused by
// every Envoy and take the whole update with it; it is left out of what is
// served, says why, and costs the other targets nothing.
func TestTLSTargetWithNothingToVerifyAgainst(t *testing.T) {
	m := mockVAST(t)
	dir := t.TempDir()
	s, stop := run(t, dir)
	addCluster(t, s, m)
	addTarget(t, s, "plain", "main", 9000)
	tls := func(setting map[string]any) (int, string) {
		return call(t, s, "POST", "/api/v1/targets", map[string]any{"name": "secure", "cluster": "lab", "pool": "main", "port": 443, "listen_port": 9443, "tls": setting})
	}
	if code, body := tls(map[string]any{"server_name": "s3.example.com"}); code != 400 || !strings.Contains(body, "CA bundle") || !strings.Contains(body, "/etc/ssl/certs/ca-certificates.crt") {
		t.Errorf("a TLS target with no CA file and verification on: %d %s", code, body)
	}
	if code, body := tls(map[string]any{"ca_file": "/etc/deltav/ca.pem"}); code != 400 || !strings.Contains(body, "server name") {
		t.Errorf("a TLS target with no server name: %d %s", code, body)
	}
	if code, body := tls(map[string]any{"server_name": "s3.example.com", "insecure": true}); code != 200 && code != 201 {
		t.Fatalf("a TLS target with verification off: %d %s", code, body)
	}
	stop()

	// As an earlier build would have stored it: no CA file, not insecure.
	path := filepath.Join(dir, "state.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(raw), `"insecure": true`, `"insecure": false`, -1)
	var doc map[string]any
	if err := json.Unmarshal([]byte(edited), &doc); err != nil {
		t.Fatal(err)
	}
	for _, tg := range doc["targets"].([]any) {
		if tg := tg.(map[string]any); tg["name"] == "secure" {
			tg["tls"] = map[string]any{"server_name": "s3.example.com"}
		}
	}
	for _, c := range doc["clusters"].([]any) {
		c.(map[string]any)["insecure"] = true // the mock VAST's own certificate
	}
	out, _ := json.Marshal(doc)
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}

	s2, _ := run(t, dir)
	s2.poller.pollOnce(context.Background())
	if tj := target(t, s2, "plain"); len(tj.Endpoints) != 6 || tj.Status.Problem != "" {
		t.Errorf("the plain target: %d endpoints, status %+v", len(tj.Endpoints), tj.Status)
	}
	tj := target(t, s2, "secure")
	if len(tj.Endpoints) != 0 || tj.Status == nil || !strings.Contains(tj.Status.Problem, "not served") || !strings.Contains(tj.Status.Problem, "CA bundle") {
		t.Errorf("the TLS target stored without a CA file: %d endpoints, status %+v", len(tj.Endpoints), tj.Status)
	}
	for _, spec := range s2.XDS().Targets() {
		if spec.Name == "secure" {
			t.Error("the TLS target with nothing to verify against is served to Envoys")
		}
	}
}

// A restarted daemon serves its targets at once, with the endpoints it had,
// even if VAST cannot be reached: Envoys that reconnect must not be handed
// a config with their targets missing.
func TestRestartServesTheLastEndpoints(t *testing.T) {
	m := mockVAST(t)
	dir := t.TempDir()
	s1, stop := run(t, dir)
	addCluster(t, s1, m)
	want := addTarget(t, s1, "s3", "main", 9000)
	stop()
	m.Close() // VAST is gone

	// New builds the first snapshot before any listener exists.
	s2, err := New(Config{GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0", StateDir: dir, VASTPollInterval: time.Hour,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.cancel()
	served := s2.XDS().Targets()
	if len(served) != 1 || served[0].Name != "s3" || served[0].ListenPort != 9000 || len(served[0].Endpoints) != 6 {
		t.Fatalf("served right after restart: %+v", served)
	}
	for i, e := range served[0].Endpoints {
		if e.IP != want.Endpoints[i].IP || e.Port != 80 || e.CNode != want.Endpoints[i].CNode {
			t.Errorf("endpoint %d after restart: %+v, before: %+v", i, e, want.Endpoints[i])
		}
	}
	// The first poll fails; the endpoints stay and the status explains.
	s2.poller.pollOnce(context.Background())
	if got := s2.XDS().Targets(); len(got) != 1 || len(got[0].Endpoints) != 6 {
		t.Fatalf("served after a failed first poll: %+v", got)
	}
	st, _ := s2.poller.targetStatus("s3")
	if !st.Held || !strings.Contains(st.Problem, "waiting for the first successful poll") {
		t.Errorf("status: %+v", st)
	}
}

// A target added while its cluster has never been read is not served, not
// served empty: there is nothing to say about it yet.
func TestTargetWaitsForFirstPoll(t *testing.T) {
	m := mockVAST(t)
	dir := t.TempDir()
	s1, stop := run(t, dir)
	addCluster(t, s1, m)
	addTarget(t, s1, "s3", "main", 9000)
	stop()
	if err := os.Remove(filepath.Join(dir, "served.json")); err != nil {
		t.Fatal(err)
	}
	m.FailNext(100)

	s2, _ := run(t, dir)
	s2.poller.pollOnce(context.Background())
	if got := s2.XDS().Targets(); len(got) != 0 {
		t.Fatalf("served with no cache and no successful poll: %+v", got)
	}
	tj := target(t, s2, "s3")
	if len(tj.Endpoints) != 0 || tj.Status == nil || !strings.Contains(tj.Status.Problem, "waiting for the first successful poll") {
		t.Errorf("target: %+v status %+v", tj, tj.Status)
	}
	m.FailNext(0)
	s2.poller.pollOnce(context.Background())
	if got := s2.XDS().Targets(); len(got) != 1 || len(got[0].Endpoints) != 6 {
		t.Fatalf("after the first successful poll: %+v", got)
	}
}

func TestCNodeRollups(t *testing.T) {
	m := mockVAST(t)
	s, _ := run(t, t.TempDir())
	addCluster(t, s, m)
	tj := addTarget(t, s, "s3", "main", 9000)

	// 10 requests to each VIP, slow on cnode-3-12's; one in flight per VIP.
	var load []metrics.LoadReport
	for _, e := range tj.Endpoints {
		d := 10 * time.Millisecond
		if e.CNode == "cnode-3-12" {
			d = 800 * time.Millisecond
		}
		ep := fmt.Sprintf("%s:%d", e.IP, e.Port)
		batch := make([]metrics.Sample, 10)
		for i := range batch {
			batch[i] = metrics.Sample{Target: "s3", Endpoint: ep, Status: 200, Duration: d, BytesIn: 100}
		}
		s.agg.Record("host-a", batch)
		load = append(load, metrics.LoadReport{Key: metrics.EndpointKey{Target: "s3", Endpoint: ep}, InProgress: 1})
	}
	s.agg.Load("host-a", 1, load)

	resp := getJSON[cnodesResponse](t, s, "/api/v1/cnodes?window=5m")
	if len(resp.CNodes) != 3 || resp.Window != "5m" {
		t.Fatalf("cnodes: %+v", resp)
	}
	for _, c := range resp.CNodes {
		if c.Cluster != "lab" || c.State != "ACTIVE" || !c.Healthy || !c.Enabled || c.Hostname == "" {
			t.Errorf("%s: %+v", c.Name, c)
		}
		// Two VIPs each: 20 requests, 2 in flight, two VIP rows of 10.
		if c.VIPs != 2 || c.Stats.Requests != 20 || c.Stats.BytesIn != 2000 || c.InFlight != 2 || len(c.Endpoints) != 2 {
			t.Errorf("%s rollup: vips %d, requests %d, in flight %d, %d rows", c.Name, c.VIPs, c.Stats.Requests, c.InFlight, len(c.Endpoints))
		}
		for _, row := range c.Endpoints {
			if row.Target != "s3" || row.CNode != c.Name || row.Stats.Requests != 10 || row.InFlight != 1 {
				t.Errorf("%s VIP row: %+v", c.Name, row)
			}
		}
		slow := c.Name == "cnode-3-12"
		if p50 := c.Stats.LatencyMS.P50; (slow && p50 < 600) || (!slow && p50 > 20) {
			t.Errorf("%s p50 = %.0f ms", c.Name, p50)
		}
	}
	// One CNode is the leader and one hosts management, as VAST reports.
	leaders := 0
	for _, c := range resp.CNodes {
		if c.IsLeader {
			leaders++
		}
	}
	if leaders != 1 {
		t.Errorf("%d leaders", leaders)
	}

	// A CNode out of service is still listed, flagged, with its VIPs
	// counted but not served.
	_ = m.SetCNode("cnode-3-10", "INACTIVE", true)
	s.poller.pollOnce(context.Background())
	for _, c := range getJSON[cnodesResponse](t, s, "/api/v1/cnodes").CNodes {
		if c.Name == "cnode-3-10" && (c.Healthy || c.State != "INACTIVE" || c.VIPs != 2) {
			t.Errorf("cnode-3-10 out of service: %+v", c)
		}
	}
	// /metrics carries the CNode on every endpoint series.
	_, prom := get(t, "http://"+s.HTTPAddr()+"/metrics", "")
	if !strings.Contains(prom, `cnode="cnode-3-11"`) || !strings.Contains(prom, `deltav_target_endpoints{target="s3"} 4`) {
		t.Error("/metrics lacks the CNode label or the served endpoint count")
	}
}
