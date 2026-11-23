package server

// Review C5: an in-flight poll can erase a newly attached cluster's view,
// and a late result for a detached cluster can overwrite the view of a
// re-attached cluster of the same name.
//
// This file also holds the helpers shared by the other review_*_test.go
// files in this package: a channel-gated fake VAST API, an in-process
// server built with New (no sockets), and an in-process API caller.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"deltav/internal/state"
	"deltav/internal/vast"
	"deltav/internal/xds"
)

// reviewFake is a vast.API whose answers a test sets, and whose next
// ListCNodes can be held until the test lets it go.
type reviewFake struct {
	mu      sync.Mutex
	cnodes  []vast.CNode
	pools   []vast.VIPPool
	vips    []vast.VIP
	dns     []vast.DNS
	err     error // every read fails with this
	dnsErr  error // ListDNS fails with this
	gate    chan struct{}
	entered chan struct{}
	dnsRead chan struct{} // if set, signalled (non-blocking) after each ListDNS
}

// reviewTopology is three healthy CNodes, an enabled pool, and one VIP per
// address spread over the CNodes.
func reviewTopology(pool string, ips ...string) *reviewFake {
	f := &reviewFake{}
	for i := 1; i <= 3; i++ {
		f.cnodes = append(f.cnodes, vast.CNode{Name: fmt.Sprintf("cn-%d", i), State: vast.StateActive, Enabled: true})
	}
	f.pools = []vast.VIPPool{{Name: pool, Enabled: true}}
	f.setVIPs(pool, ips...)
	return f
}

func (f *reviewFake) setVIPs(pool string, ips ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vips = nil
	for i, ip := range ips {
		f.vips = append(f.vips, vast.VIP{IP: ip, VIPPool: pool, CNode: fmt.Sprintf("cn-%d", i%3+1)})
	}
}

func (f *reviewFake) set(fn func(f *reviewFake)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// hold makes the next ListCNodes report on entered and then wait for
// release (or its context).
func (f *reviewFake) hold() (entered <-chan struct{}, release chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gate, f.entered = make(chan struct{}), make(chan struct{}, 1)
	return f.entered, f.gate
}

func (f *reviewFake) ListCNodes(ctx context.Context) ([]vast.CNode, error) {
	f.mu.Lock()
	gate, entered := f.gate, f.entered
	f.gate, f.entered = nil, nil
	f.mu.Unlock()
	if gate != nil {
		entered <- struct{}{}
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return append([]vast.CNode(nil), f.cnodes...), nil
}

func (f *reviewFake) ListVIPPools(context.Context) ([]vast.VIPPool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return append([]vast.VIPPool(nil), f.pools...), nil
}

func (f *reviewFake) ListVIPs(context.Context) ([]vast.VIP, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return append([]vast.VIP(nil), f.vips...), nil
}

func (f *reviewFake) ListDNS(context.Context) ([]vast.DNS, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dnsRead != nil {
		defer func() {
			select {
			case f.dnsRead <- struct{}{}:
			default:
			}
		}()
	}
	if f.err != nil {
		return nil, f.err
	}
	if f.dnsErr != nil {
		return nil, f.dnsErr
	}
	return append([]vast.DNS(nil), f.dns...), nil
}

// reviewServer builds a daemon with New, without binding any port, whose
// clusters are the fakes keyed by their host.
func reviewServer(t *testing.T, dir string, fakes map[string]*reviewFake) *Server {
	t.Helper()
	s, err := New(Config{
		GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0", StateDir: dir, VASTPollInterval: time.Hour,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.cancel)
	s.poller.newAPI = func(c state.Cluster) (vast.API, error) {
		if f := fakes[c.Host]; f != nil {
			return f, nil
		}
		return nil, fmt.Errorf("no fake VAST for host %q", c.Host)
	}
	return s
}

func reviewAddCluster(t *testing.T, s *Server, name, host string) {
	t.Helper()
	if err := s.store.AddCluster(state.Cluster{Name: name, Host: host, User: "u", Password: "p"}); err != nil {
		t.Fatal(err)
	}
}

func reviewAddTarget(t *testing.T, s *Server, name, cluster, pool string, listen uint32) {
	t.Helper()
	if err := s.store.AddTarget(state.Target{Name: name, Cluster: cluster, Pool: pool, Port: 80, ListenPort: listen}, nil); err != nil {
		t.Fatal(err)
	}
}

// reviewDo calls the API in process.
func reviewDo(t *testing.T, s *Server, method, path string, body any) (int, string) {
	t.Helper()
	var rd io.Reader = http.NoBody
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	rec := httptest.NewRecorder()
	s.apiRoutes().ServeHTTP(rec, httptest.NewRequest(method, path, rd))
	return rec.Code, rec.Body.String()
}

func reviewSpec(s *Server, name string) (xds.TargetSpec, bool) {
	for _, spec := range s.XDS().Targets() {
		if spec.Name == name {
			return spec, true
		}
	}
	return xds.TargetSpec{}, false
}

// reviewCache is the restart cache on disk.
func reviewCache(t *testing.T, s *Server) state.Cache {
	t.Helper()
	c, err := s.store.LoadCache()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func reviewIPs(eps []xds.Endpoint) string {
	var ips []string
	for _, e := range eps {
		ips = append(ips, e.IP)
	}
	return strings.Join(ips, ",")
}

func reviewWait(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// A cluster attached while a poll is in flight keeps the view the attach
// stored: the poll's end-of-poll cleanup must not delete it.
func TestReviewC5_AttachDuringPollKeepsView(t *testing.T) {
	a := reviewTopology("main", "10.0.0.1", "10.0.0.2", "10.0.0.3")
	b := reviewTopology("main", "10.1.0.1", "10.1.0.2", "10.1.0.3")
	s := reviewServer(t, t.TempDir(), map[string]*reviewFake{"a-host": a, "b-host": b})
	ctx := context.Background()
	reviewAddCluster(t, s, "a", "a-host")
	s.poller.pollOnce(ctx)

	entered, release := a.hold()
	done := make(chan struct{})
	go func() { s.poller.pollOnce(ctx); close(done) }()
	reviewWait(t, entered, "the poll to reach cluster a")

	if code, body := reviewDo(t, s, "POST", "/api/v1/vast", map[string]any{"name": "b", "host": "b-host", "user": "u", "password": "p"}); code != http.StatusCreated {
		t.Fatalf("attaching b during the poll: %d %s", code, body)
	}
	if v := s.poller.view("b"); v == nil || v.At.IsZero() {
		t.Fatalf("right after attach, b has no view: %+v", v)
	}
	close(release)
	reviewWait(t, done, "the poll to finish")

	if v := s.poller.view("b"); v == nil || v.At.IsZero() {
		t.Errorf("after the in-flight poll finished, attached cluster b lost its view: %+v", v)
	}
	if code, body := reviewDo(t, s, "POST", "/api/v1/targets", map[string]any{"name": "s3b", "cluster": "b", "pool": "main", "port": 80, "listen_port": 9100}); code != http.StatusCreated {
		t.Errorf("adding a target on just-attached cluster b: %d %s", code, strings.TrimSpace(body))
	}
	// How long it lasts: the next poll reads b again.
	s.poller.pollOnce(ctx)
	if v := s.poller.view("b"); v == nil || v.At.IsZero() {
		t.Errorf("b still has no view after the next poll")
	}
}

// A poll result for a detached cluster must not land on the view of a
// cluster attached afterwards under the same name, nor change what that
// cluster's targets are served.
func TestReviewC5_LateResultForDetachedClusterDoesNotOverwriteReattached(t *testing.T) {
	old := reviewTopology("main", "10.0.0.1", "10.0.0.2", "10.0.0.3")
	nu := reviewTopology("main", "10.1.0.1", "10.1.0.2", "10.1.0.3")
	s := reviewServer(t, t.TempDir(), map[string]*reviewFake{"old-host": old, "new-host": nu})
	ctx := context.Background()
	reviewAddCluster(t, s, "lab", "old-host")
	s.poller.pollOnce(ctx)

	entered, release := old.hold()
	done := make(chan struct{})
	go func() { s.poller.pollOnce(ctx); close(done) }()
	reviewWait(t, entered, "the poll to reach the old cluster")

	if code, body := reviewDo(t, s, "DELETE", "/api/v1/vast/lab", nil); code != http.StatusNoContent {
		t.Fatalf("detach: %d %s", code, body)
	}
	if code, body := reviewDo(t, s, "POST", "/api/v1/vast", map[string]any{"name": "lab", "host": "new-host", "user": "u", "password": "p"}); code != http.StatusCreated {
		t.Fatalf("re-attach: %d %s", code, body)
	}
	if code, body := reviewDo(t, s, "POST", "/api/v1/targets", map[string]any{"name": "s3", "cluster": "lab", "pool": "main", "port": 80, "listen_port": 9000}); code != http.StatusCreated {
		t.Fatalf("add target: %d %s", code, body)
	}
	if spec, _ := reviewSpec(s, "s3"); !strings.HasPrefix(reviewIPs(spec.Endpoints), "10.1.") {
		t.Fatalf("before the late result, s3 serves %s", reviewIPs(spec.Endpoints))
	}
	close(release)
	reviewWait(t, done, "the poll to finish")

	v := s.poller.view("lab")
	if v == nil || len(v.VIPs) == 0 || !strings.HasPrefix(v.VIPs[0].IP, "10.1.") {
		var ips []string
		if v != nil {
			for _, vip := range v.VIPs {
				ips = append(ips, vip.IP)
			}
		}
		t.Errorf("re-attached cluster lab's view holds the detached cluster's VIPs: %v", ips)
	}
	if spec, _ := reviewSpec(s, "s3"); !strings.HasPrefix(reviewIPs(spec.Endpoints), "10.1.") {
		t.Errorf("target s3 on the re-attached cluster is served the detached cluster's VIPs: %s", reviewIPs(spec.Endpoints))
	}
	// How long it lasts: the next poll reads the new cluster.
	s.poller.pollOnce(ctx)
	if spec, _ := reviewSpec(s, "s3"); !strings.HasPrefix(reviewIPs(spec.Endpoints), "10.1.") {
		t.Errorf("after the next poll, s3 still serves %s", reviewIPs(spec.Endpoints))
	}
}

// A cluster detached while a poll is reading it stays detached: the late
// result must not bring its view back.
func TestReviewC5_DetachDuringPollDoesNotResurrectView(t *testing.T) {
	lab := reviewTopology("main", "10.0.0.1", "10.0.0.2", "10.0.0.3")
	s := reviewServer(t, t.TempDir(), map[string]*reviewFake{"lab-host": lab})
	ctx := context.Background()
	reviewAddCluster(t, s, "lab", "lab-host")
	s.poller.pollOnce(ctx)

	entered, release := lab.hold()
	done := make(chan struct{})
	go func() { s.poller.pollOnce(ctx); close(done) }()
	reviewWait(t, entered, "the poll to reach cluster lab")
	if code, body := reviewDo(t, s, "DELETE", "/api/v1/vast/lab", nil); code != http.StatusNoContent {
		t.Fatalf("detach: %d %s", code, body)
	}
	if v := s.poller.view("lab"); v != nil {
		t.Fatalf("detach left a view: %+v", v)
	}
	close(release)
	reviewWait(t, done, "the poll to finish")
	if v := s.poller.view("lab"); v != nil {
		t.Errorf("detached cluster lab has a view again after the in-flight poll: %+v", v)
	}
}
