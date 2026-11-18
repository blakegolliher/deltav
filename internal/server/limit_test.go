package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discoverygrpc "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"google.golang.org/protobuf/types/known/structpb"

	"deltav/internal/fleet"
	"deltav/internal/metrics"
)

// connect registers Envoys from..to-1 with the fleet, as their ADS streams
// would. The stream path itself is tested in ads_test.go.
func connect(s *Server, from, to int) {
	for i := from; i < to; i++ {
		id := int64(5000 + i)
		s.fleet.StreamOpen(id, "")
		s.fleet.Request(id, &discoverygrpc.DiscoveryRequest{TypeUrl: resource.ClusterType, Node: &corev3.Node{Id: fmt.Sprintf("envoy-%03d", i)}})
	}
}

// connectWorkers registers one Envoy that declares its worker threads in its
// node metadata, as the client unit makes it do.
func connectWorkers(s *Server, i, workers int) {
	id := int64(5000 + i)
	md, _ := structpb.NewStruct(map[string]any{fleet.WorkersMetadataKey: workers})
	s.fleet.StreamOpen(id, "")
	s.fleet.Request(id, &discoverygrpc.DiscoveryRequest{TypeUrl: resource.ClusterType, Node: &corev3.Node{Id: fmt.Sprintf("envoy-%03d", i), Metadata: md}})
}

func setLimit(t *testing.T, s *Server, target string, perCNode int, mode string) targetJSON {
	t.Helper()
	code, body := call(t, s, "PUT", "/api/v1/targets/"+target+"/limit", map[string]any{"per_cnode": perCNode, "mode": mode})
	var tj targetJSON
	if code != 200 || json.Unmarshal([]byte(body), &tj) != nil {
		t.Fatalf("setting the limit: %d %s", code, body)
	}
	return tj
}

// perHost is the threshold Envoys are served for a target.
func perHost(t *testing.T, s *Server, target string) uint32 {
	t.Helper()
	for _, spec := range s.XDS().Targets() {
		if spec.Name == target {
			return spec.PerHostLimit
		}
	}
	t.Fatalf("target %q is not served", target)
	return 0
}

// inFlight reports n requests in flight to each of a target's endpoints on
// the named CNodes, as one Envoy's load report would.
func inFlight(s *Server, tj targetJSON, perVIP map[string]uint64) {
	var reports []metrics.LoadReport
	for _, e := range tj.Endpoints {
		if n := perVIP[e.CNode]; n > 0 {
			key := metrics.EndpointKey{Target: tj.Name, Endpoint: fmt.Sprintf("%s:%d", e.IP, e.Port)}
			reports = append(reports, metrics.LoadReport{Key: key, InProgress: n})
		}
	}
	s.agg.Load("envoy-000", 1, reports)
}

func TestLimitAlert(t *testing.T) {
	m := mockVAST(t)
	s, _ := run(t, t.TempDir())
	addCluster(t, s, m)
	tj := addTarget(t, s, "s3", "main", 9000)
	if tj.Status.Limit != nil {
		t.Fatalf("a new target has a limit: %+v", tj.Status.Limit)
	}

	for what, tc := range map[string]struct {
		path string
		body map[string]any
		code int
	}{
		"unknown target": {"/api/v1/targets/nope/limit", map[string]any{"per_cnode": 5}, 404},
		"unknown mode":   {"/api/v1/targets/s3/limit", map[string]any{"per_cnode": 5, "mode": "strict"}, 400},
		"unknown field":  {"/api/v1/targets/s3/limit", map[string]any{"limit": 5}, 400},
	} {
		if code, body := call(t, s, "PUT", tc.path, tc.body); code != tc.code {
			t.Errorf("%s: %d %s", what, code, body)
		}
	}

	// Alert is the default mode, and enforces nothing.
	tj = setLimit(t, s, "s3", 5, "")
	if l := tj.Status.Limit; l == nil || l.PerCNode != 5 || l.Mode != "alert" || l.Share != 0 || l.MaxVIPsPerCNode != 2 {
		t.Fatalf("limit status: %+v", tj.Status.Limit)
	}
	if got := perHost(t, s, "s3"); got != 0 {
		t.Errorf("alert mode serves a per-host threshold of %d", got)
	}
	if len(tj.Over) != 0 {
		t.Errorf("over the limit with nothing in flight: %+v", tj.Over)
	}

	// Two VIPs per CNode: 3 in flight on each of cnode-3-10's is 6, over a
	// limit of 5; 2 on each of cnode-3-11's is 4, under it.
	inFlight(s, tj, map[string]uint64{"cnode-3-10": 3, "cnode-3-11": 2})
	tj = target(t, s, "s3")
	if len(tj.Over) != 1 || tj.Over[0].CNode != "cnode-3-10" || tj.Over[0].InFlight != 6 || tj.Over[0].PerCNode != 5 || tj.Over[0].Cluster != "lab" {
		t.Errorf("targets: over = %+v", tj.Over)
	}
	ov := getJSON[overviewResponse](t, s, "/api/v1/overview").Targets[0]
	if ov.Limit == nil || ov.Limit.PerCNode != 5 || len(ov.Over) != 1 || ov.Over[0].CNode != "cnode-3-10" {
		t.Errorf("overview: limit %+v over %+v", ov.Limit, ov.Over)
	}
	for _, c := range getJSON[cnodesResponse](t, s, "/api/v1/cnodes").CNodes {
		want := map[string]struct {
			inFlight uint64
			over     bool
		}{"cnode-3-10": {6, true}, "cnode-3-11": {4, false}, "cnode-3-12": {0, false}}[c.Name]
		if len(c.Limits) != 1 || c.Limits[0].Target != "s3" || c.Limits[0].InFlight != want.inFlight || c.Limits[0].Over != want.over {
			t.Errorf("cnodes: %s limits = %+v", c.Name, c.Limits)
		}
	}
	_, prom := get(t, "http://"+s.HTTPAddr()+"/metrics", "")
	for _, want := range []string{
		`deltav_target_cnode_limit{mode="alert",target="s3"} 5`,
		`deltav_cnode_in_flight_requests{cluster="lab",cnode="cnode-3-10",target="s3"} 6`,
		`deltav_cnode_over_limit{cluster="lab",cnode="cnode-3-10",target="s3"} 1`,
		`deltav_cnode_over_limit{cluster="lab",cnode="cnode-3-11",target="s3"} 0`,
	} {
		if !strings.Contains(prom, want+"\n") {
			t.Errorf("/metrics lacks: %s", want)
		}
	}
	if strings.Contains(prom, "deltav_target_limit_share") {
		t.Error("/metrics reports a share in alert mode")
	}

	// Exactly at the limit is not over it.
	setLimit(t, s, "s3", 6, "alert")
	if tj = target(t, s, "s3"); len(tj.Over) != 0 {
		t.Errorf("6 in flight against a limit of 6: over = %+v", tj.Over)
	}

	// Removing the limit removes the reporting.
	if code, body := call(t, s, "DELETE", "/api/v1/targets/s3/limit", nil); code != 200 {
		t.Fatalf("removing the limit: %d %s", code, body)
	}
	if tj = target(t, s, "s3"); tj.Status.Limit != nil || len(tj.Over) != 0 {
		t.Errorf("after removal: limit %+v over %+v", tj.Status.Limit, tj.Over)
	}
	// A limit of 0 is the same as none.
	setLimit(t, s, "s3", 0, "enforce")
	if tj = target(t, s, "s3"); tj.Status.Limit != nil || perHost(t, s, "s3") != 0 {
		t.Errorf("a limit of 0: %+v", tj.Status.Limit)
	}
}

func TestLimitEnforce(t *testing.T) {
	m := mockVAST(t)
	s, _ := run(t, t.TempDir())
	addCluster(t, s, m)
	addTarget(t, s, "s3", "main", 9000)
	ctx := context.Background()
	clock := time.Now()
	s.poller.mu.Lock()
	s.poller.now = func() time.Time { return clock }
	s.poller.mu.Unlock()
	apply := func() string {
		t.Helper()
		if _, _, err := s.poller.apply(ctx); err != nil {
			t.Fatal(err)
		}
		return s.XDS().Version()
	}
	limit := func() *limitStatus { return target(t, s, "s3").Status.Limit }

	// 10 Envoys, 2 VIPs per CNode, limit 100: each Envoy may open
	// 100 / (10 x 2) = 5 connections to each VIP.
	connect(s, 0, 10)
	setLimit(t, s, "s3", 100, "enforce")
	if l := limit(); l.Share != 5 || l.Computed != 5 || l.Envoys != 10 || l.MaxVIPsPerCNode != 2 || l.Mode != "enforce" {
		t.Fatalf("limit status: %+v", l)
	}
	if got := perHost(t, s, "s3"); got != 5 {
		t.Fatalf("per-host threshold served = %d, want 5", got)
	}
	_, prom := get(t, "http://"+s.HTTPAddr()+"/metrics", "")
	if !strings.Contains(prom, `deltav_target_limit_share{target="s3"} 5`+"\n") || !strings.Contains(prom, `deltav_target_cnode_limit{mode="enforce",target="s3"} 100`+"\n") {
		t.Error("/metrics lacks the limit or the share")
	}
	v := s.XDS().Version()

	// One more Envoy: 100 / 22 = 4, a change of exactly a fifth. Not more
	// than a fifth, so Envoys keep their pools.
	connect(s, 10, 11)
	clock = clock.Add(2 * time.Minute)
	if got := apply(); got != v {
		t.Error("a 20% change of share was pushed")
	}
	if l := limit(); l.Share != 5 || l.Computed != 4 || l.Envoys != 11 {
		t.Errorf("after one more Envoy: %+v", l)
	}

	// The fleet nearly doubles: 100 / 40 = 2, a large change, and long
	// enough after the last push. Pushed.
	connect(s, 11, 20)
	clock = clock.Add(10 * time.Second)
	v2 := apply()
	if v2 == v {
		t.Error("a 60% change of share, two minutes after the last push, was not pushed")
	}
	if l := limit(); l.Share != 2 || l.Computed != 2 || l.Envoys != 20 || perHost(t, s, "s3") != 2 {
		t.Errorf("after the fleet grew to 20: %+v, served %d", l, perHost(t, s, "s3"))
	}

	// It doubles again ten seconds later: 100 / 80 = 1. Held, however large
	// the change, because the last push was inside the interval...
	connect(s, 20, 40)
	clock = clock.Add(10 * time.Second)
	if got := apply(); got != v2 {
		t.Error("a share was pushed again inside the interval")
	}
	if l := limit(); l.Share != 2 || l.Computed != 1 || l.Envoys != 40 {
		t.Errorf("inside the interval: %+v", l)
	}
	// ...and pushed once the interval has passed.
	clock = clock.Add(2 * time.Minute)
	if got := apply(); got == v2 {
		t.Error("the new share was not pushed after the interval")
	}
	if l := limit(); l.Share != 1 || perHost(t, s, "s3") != 1 {
		t.Errorf("after the interval: %+v, served %d", l, perHost(t, s, "s3"))
	}

	// An operator's change is not debounced: 400 / (40 x 2) = 5, at once.
	setLimit(t, s, "s3", 400, "enforce")
	if l := limit(); l.Share != 5 || perHost(t, s, "s3") != 5 {
		t.Errorf("after raising the limit: %+v, served %d", l, perHost(t, s, "s3"))
	}

	// A VIP fails over, so one CNode hosts three of the pool's VIPs: the
	// share is worked out for that CNode, 400 / (40 x 3) = 3.
	var moved string
	for _, e := range target(t, s, "s3").Endpoints {
		if e.CNode == "cnode-3-11" {
			moved = e.IP
			break
		}
	}
	if err := m.MoveVIP(moved, "cnode-3-12"); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(2 * time.Minute)
	s.poller.pollOnce(ctx)
	if l := limit(); l.MaxVIPsPerCNode != 3 || l.Computed != 3 || l.Share != 3 || perHost(t, s, "s3") != 3 {
		t.Errorf("with three VIPs on one CNode: %+v, served %d", l, perHost(t, s, "s3"))
	}

	// More Envoys than the limit allows for: the share stops at 1, and the
	// alert still reports when the fleet goes over.
	setLimit(t, s, "s3", 10, "enforce")
	if l := limit(); l.Share != 1 || perHost(t, s, "s3") != 1 {
		t.Errorf("limit 10 for 40 Envoys: %+v", l)
	}
	tj := target(t, s, "s3")
	inFlight(s, tj, map[string]uint64{"cnode-3-12": 4})
	if tj = target(t, s, "s3"); len(tj.Over) != 1 || tj.Over[0].CNode != "cnode-3-12" || tj.Over[0].InFlight != 12 {
		t.Errorf("over the limit in enforce mode: %+v", tj.Over)
	}

	// Back to alert: the threshold goes at once.
	setLimit(t, s, "s3", 10, "alert")
	if l := limit(); l.Share != 0 || l.Mode != "alert" || perHost(t, s, "s3") != 0 {
		t.Errorf("after switching to alert: %+v, served %d", l, perHost(t, s, "s3"))
	}
}

// After a restart the first snapshot must carry the share the fleet already
// has. Worked out afresh, with no Envoy connected yet, it would be the
// whole limit per Envoy.
func TestEnforcedShareSurvivesRestart(t *testing.T) {
	m := mockVAST(t)
	dir := t.TempDir()
	s1, stop := run(t, dir)
	addCluster(t, s1, m)
	addTarget(t, s1, "s3", "main", 9000)
	connect(s1, 0, 10)
	setLimit(t, s1, "s3", 100, "enforce")
	if got := perHost(t, s1, "s3"); got != 5 {
		t.Fatalf("share before the restart = %d", got)
	}
	stop()

	s2, err := New(Config{GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0", StateDir: dir, VASTPollInterval: time.Hour,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.cancel()
	if got := perHost(t, s2, "s3"); got != 5 {
		t.Fatalf("share in the first snapshot after the restart = %d, want the 5 the fleet already has", got)
	}
	// Envoys trickle back; a poll in between must not loosen the limit.
	connect(s2, 0, 2)
	s2.poller.pollOnce(context.Background())
	st, _ := s2.poller.targetStatus("s3")
	if st.Limit == nil || st.Limit.Share != 5 || st.Limit.Computed != 25 || perHost(t, s2, "s3") != 5 {
		t.Errorf("with 2 of 10 Envoys back: %+v, served %d", st.Limit, perHost(t, s2, "s3"))
	}
}

func TestDrain(t *testing.T) {
	m := mockVAST(t)
	dir := t.TempDir()
	s, stop := run(t, dir)
	addCluster(t, s, m)
	addTarget(t, s, "s3", "main", 9000)
	drain := func(method, cnode string, want int) {
		t.Helper()
		if code, body := call(t, s, method, "/api/v1/vast/lab/cnodes/"+cnode+"/drain", nil); code != want {
			t.Errorf("%s drain of %s: %d %s, want %d", method, cnode, code, body, want)
		}
	}
	cnode := func(name string) cnodeJSON {
		t.Helper()
		for _, c := range getJSON[cnodesResponse](t, s, "/api/v1/cnodes").CNodes {
			if c.Name == name {
				return c
			}
		}
		t.Fatalf("no CNode %s", name)
		return cnodeJSON{}
	}

	drain("POST", "cnode-9-99", http.StatusNotFound) // a typo must not be stored
	drain("DELETE", "cnode-3-11", http.StatusNotFound)
	if code, _ := call(t, s, "POST", "/api/v1/vast/nope/cnodes/cnode-3-11/drain", nil); code != http.StatusNotFound {
		t.Errorf("drain on an unknown cluster: %d", code)
	}

	// Drained: its two VIPs go at once, though VAST still calls it ACTIVE.
	v := s.XDS().Version()
	drain("POST", "cnode-3-11", http.StatusNoContent)
	tj := target(t, s, "s3")
	if n := perCNode(tj); len(tj.Endpoints) != 4 || n["cnode-3-11"] != 0 || tj.Status.Drained != 2 || tj.Status.Healthy != 6 || tj.Status.Held {
		t.Errorf("after draining cnode-3-11: %v, status %+v", n, tj.Status)
	}
	if s.XDS().Version() == v {
		t.Error("draining did not change what Envoys are served")
	}
	if c := cnode("cnode-3-11"); !c.Drained || !c.Healthy || c.State != "ACTIVE" || c.VIPs != 2 {
		t.Errorf("drained CNode: %+v", c)
	}
	if c := cnode("cnode-3-10"); c.Drained {
		t.Error("cnode-3-10 is shown as drained")
	}
	_, prom := get(t, "http://"+s.HTTPAddr()+"/metrics", "")
	if !strings.Contains(prom, `deltav_cnode_drained{cluster="lab",cnode="cnode-3-11"} 1`+"\n") {
		t.Error("/metrics lacks the drained CNode")
	}
	drain("POST", "cnode-3-11", http.StatusNoContent) // again: no error, no change
	// A poll does not bring them back.
	s.poller.pollOnce(context.Background())
	if tj = target(t, s, "s3"); len(tj.Endpoints) != 4 {
		t.Errorf("a poll undid the drain: %d endpoints", len(tj.Endpoints))
	}

	// It survives a restart, in the very first snapshot.
	stop()
	s2, err := New(Config{GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0", StateDir: dir, VASTPollInterval: time.Hour,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if served := s2.XDS().Targets(); len(served) != 1 || len(served[0].Endpoints) != 4 {
		t.Errorf("served after restart: %+v", served)
	}
	s2.cancel()

	// Draining every CNode empties the target at once: this is an
	// operator's decision, not an odd answer from VAST to be held back.
	s, _ = run(t, dir)
	s.poller.pollOnce(context.Background())
	drain("POST", "cnode-3-10", http.StatusNoContent)
	drain("POST", "cnode-3-12", http.StatusNoContent)
	if tj = target(t, s, "s3"); len(tj.Endpoints) != 0 || tj.Status.Held || tj.Status.Drained != 6 {
		t.Errorf("with every CNode drained: %d endpoints, status %+v", len(tj.Endpoints), tj.Status)
	}
	// Undrained: back in service at once.
	for _, name := range []string{"cnode-3-10", "cnode-3-11", "cnode-3-12"} {
		drain("DELETE", name, http.StatusNoContent)
	}
	if tj = target(t, s, "s3"); len(tj.Endpoints) != 6 || tj.Status.Drained != 0 {
		t.Errorf("after undraining: %d endpoints, status %+v", len(tj.Endpoints), tj.Status)
	}
	if m.Mutating() != 0 {
		t.Errorf("%d requests other than GET and the token call reached the cluster: drain is Delta-V's own, it changes nothing on VAST", m.Mutating())
	}
}

// An Envoy can hold one connection to a VIP beyond its cap for every worker
// thread after the first, so the cap is lowered by that much; and the status
// says when no cap can hold the fleet to the limit, and how many Envoys do
// not say how many workers they run.
func TestLimitAllowsForWorkerThreads(t *testing.T) {
	m := mockVAST(t)
	s, _ := run(t, t.TempDir())
	addCluster(t, s, m)
	addTarget(t, s, "s3", "main", 9000)
	limit := func() *limitStatus { return target(t, s, "s3").Status.Limit }

	// 10 Envoys with 2 workers each, 2 VIPs per CNode, limit 100:
	// 100 / (10 x 2) = 5 per VIP per Envoy, of which a second worker can
	// take one beyond the cap. So the cap is 4.
	for i := 0; i < 10; i++ {
		connectWorkers(s, i, 2)
	}
	setLimit(t, s, "s3", 100, "enforce")
	if l := limit(); l.Share != 4 || l.Workers != 2 || l.Undeclared != 0 || !l.Holds || l.Envoys != 10 {
		t.Fatalf("ten Envoys with two workers: %+v", l)
	}
	if got := perHost(t, s, "s3"); got != 4 {
		t.Errorf("per-host threshold served = %d, want 4", got)
	}
	for _, mem := range getJSON[fleetResponse](t, s, "/api/v1/fleet").Members {
		if mem.Workers != 2 {
			t.Errorf("%s: workers %d in the API, want 2", mem.NodeID, mem.Workers)
		}
	}

	// The Envoy with the most workers decides: one with 8 joins, and
	// 100 / (11 x 2) = 4 per VIP is less than its 8 workers can hold at any
	// cap. The cap goes to its floor and the status says it cannot hold.
	connectWorkers(s, 10, 8)
	setLimit(t, s, "s3", 100, "enforce") // an operator's change is pushed at once
	if l := limit(); l.Share != 1 || l.Workers != 8 || l.Holds {
		t.Errorf("with an 8-worker Envoy: %+v", l)
	}
	// With room for it, it holds: 1000 / 22 = 45, less 7.
	setLimit(t, s, "s3", 1000, "enforce")
	if l := limit(); l.Share != 38 || !l.Holds {
		t.Errorf("limit 1000 with an 8-worker Envoy: %+v", l)
	}

	// Envoys that do not say are counted, and taken as single-threaded for
	// the arithmetic: there is nothing else to go on.
	connect(s, 11, 14)
	setLimit(t, s, "s3", 1000, "enforce")
	if l := limit(); l.Undeclared != 3 || l.Workers != 8 || l.Envoys != 14 || l.Share != 28 {
		t.Errorf("with three Envoys that do not say: %+v", l)
	}
	// Nonsense in the metadata is not a declaration.
	for i, bad := range []any{0, -3, 2.5, "four", 100000} {
		md, _ := structpb.NewStruct(map[string]any{fleet.WorkersMetadataKey: bad})
		id := int64(7000 + i)
		s.fleet.StreamOpen(id, "")
		s.fleet.Request(id, &discoverygrpc.DiscoveryRequest{TypeUrl: resource.ClusterType, Node: &corev3.Node{Id: fmt.Sprintf("odd-%d", i), Metadata: md}})
	}
	setLimit(t, s, "s3", 1000, "enforce")
	if l := limit(); l.Undeclared != 8 || l.Workers != 8 {
		t.Errorf("with five Envoys declaring nonsense: %+v", l)
	}

	// Alert mode enforces nothing, so there is nothing that cannot hold.
	setLimit(t, s, "s3", 1, "alert")
	if l := limit(); !l.Holds || l.Share != 0 {
		t.Errorf("alert mode: %+v", l)
	}
}
