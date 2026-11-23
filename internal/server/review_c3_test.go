package server

// Review C3: a target that was published with zero endpoints must keep its
// listener across a restart while VAST cannot be reached; a target that has
// never been resolved must still not be served.

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"deltav/internal/vast"
)

func TestReviewC3_EmptyPoolTargetKeepsListenerAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	lab := reviewTopology("main", "10.0.0.1", "10.0.0.2", "10.0.0.3")
	lab.set(func(f *reviewFake) { f.pools = append(f.pools, vast.VIPPool{Name: "empty", Enabled: true}) })
	other := reviewTopology("main", "10.9.0.1")
	other.set(func(f *reviewFake) { f.err = errors.New("connection refused") })
	fakes := map[string]*reviewFake{"lab-host": lab, "other-host": other}

	s1 := reviewServer(t, dir, fakes)
	reviewAddCluster(t, s1, "lab", "lab-host")
	reviewAddCluster(t, s1, "other", "other-host")
	reviewAddTarget(t, s1, "empty", "lab", "empty", 9001)  // an enabled pool with no VIPs
	reviewAddTarget(t, s1, "fresh", "other", "main", 9002) // its cluster has never been read
	s1.poller.pollOnce(context.Background())
	if spec, ok := reviewSpec(s1, "empty"); !ok || len(spec.Endpoints) != 0 {
		t.Fatalf("before the restart: empty served=%v with %d endpoints", ok, len(spec.Endpoints))
	}
	if _, ok := reviewSpec(s1, "fresh"); ok {
		t.Fatal("before the restart, the never-resolved target is served")
	}
	s1.cancel()

	// Restart with VAST unavailable.
	lab.set(func(f *reviewFake) { f.err = errors.New("connection refused") })
	s2 := reviewServer(t, dir, fakes)
	if _, ok := reviewSpec(s2, "empty"); !ok {
		t.Errorf("right after the restart, target %q (published before it with 0 endpoints) has no listener; served: %d targets", "empty", len(s2.XDS().Targets()))
	}
	s2.poller.pollOnce(context.Background())
	if _, ok := reviewSpec(s2, "empty"); !ok {
		t.Errorf("after a failed first poll, target %q (published before the restart with 0 endpoints) has no listener", "empty")
	}
	if _, ok := reviewSpec(s2, "fresh"); ok {
		t.Error("after the restart, the never-resolved target is served")
	}
}

func TestReviewC3_FullyDrainedTargetKeepsListenerAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	lab := reviewTopology("main", "10.0.0.1", "10.0.0.2", "10.0.0.3")
	fakes := map[string]*reviewFake{"lab-host": lab}

	s1 := reviewServer(t, dir, fakes)
	reviewAddCluster(t, s1, "lab", "lab-host")
	reviewAddTarget(t, s1, "s3", "lab", "main", 9000)
	s1.poller.pollOnce(context.Background())
	for _, cn := range []string{"cn-1", "cn-2", "cn-3"} {
		if code, body := reviewDo(t, s1, "POST", "/api/v1/vast/lab/cnodes/"+cn+"/drain", nil); code != http.StatusNoContent {
			t.Fatalf("drain %s: %d %s", cn, code, body)
		}
	}
	if spec, ok := reviewSpec(s1, "s3"); !ok || len(spec.Endpoints) != 0 {
		t.Fatalf("with every CNode drained: served=%v with %d endpoints", ok, len(spec.Endpoints))
	}
	s1.cancel()

	lab.set(func(f *reviewFake) { f.err = errors.New("connection refused") })
	s2 := reviewServer(t, dir, fakes)
	s2.poller.pollOnce(context.Background())
	if _, ok := reviewSpec(s2, "s3"); !ok {
		t.Errorf("after a restart with VAST unavailable, the fully drained target s3 has no listener (Envoy clients now get connection refused, not 503)")
	}
}
