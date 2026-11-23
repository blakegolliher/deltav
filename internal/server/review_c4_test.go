package server

// Review C4: a failed write of the restart cache (served.json) must be
// retried by later, healthy polls; and what the cache records must follow
// what is served even when the rendered xDS does not change.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"deltav/internal/vast"
)

func TestReviewC4_FailedCacheWriteIsRetried(t *testing.T) {
	dir := t.TempDir()
	lab := reviewTopology("main", "10.0.0.1", "10.0.0.2", "10.0.0.3")
	fakes := map[string]*reviewFake{"lab-host": lab}
	s1 := reviewServer(t, dir, fakes)
	ctx := context.Background()

	// Obstruct the cache: a directory where served.json goes makes the
	// rename in writeAtomic fail.
	cachePath := filepath.Join(dir, "served.json")
	if err := os.Mkdir(cachePath, 0o700); err != nil {
		t.Fatal(err)
	}
	reviewAddCluster(t, s1, "lab", "lab-host")
	reviewAddTarget(t, s1, "s3", "lab", "main", 9000)
	s1.poller.pollOnce(ctx) // publishes s3; the cache write fails
	if spec, ok := reviewSpec(s1, "s3"); !ok || len(spec.Endpoints) != 3 {
		t.Fatalf("s3 not served: %v %+v", ok, spec)
	}
	if err := os.Remove(cachePath); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		s1.poller.pollOnce(ctx) // healthy, nothing changed
	}
	if _, err := os.Stat(cachePath); err != nil {
		t.Errorf("after the obstruction was removed and 3 healthy polls: %v", err)
	} else if eps := reviewCache(t, s1).Endpoints["s3"]; len(eps) != 3 {
		t.Errorf("the cache has %d endpoints for s3, want 3", len(eps))
	}
	s1.cancel()

	// What it costs: a restart while VAST is down loses the healthy listener.
	lab.set(func(f *reviewFake) { f.err = errors.New("connection refused") })
	s2 := reviewServer(t, dir, fakes)
	if _, ok := reviewSpec(s2, "s3"); !ok {
		t.Errorf("restarted with VAST down: s3, healthy and served before the restart, is not served")
	}
}

// With no rate rules the DNS suffix is not in the rendered xDS, so a suffix
// change leaves the digest alone and the cache keeps the old suffix.
func TestReviewC4_HostsChangeWithoutRulesReachesCache(t *testing.T) {
	dir := t.TempDir()
	lab := reviewTopology("main", "10.0.0.1", "10.0.0.2", "10.0.0.3")
	lab.set(func(f *reviewFake) { f.dns = []vast.DNS{{DomainSuffix: "a.example.com", Enabled: true}} })
	s := reviewServer(t, dir, map[string]*reviewFake{"lab-host": lab})
	ctx := context.Background()
	reviewAddCluster(t, s, "lab", "lab-host")
	reviewAddTarget(t, s, "s3", "lab", "main", 9000)
	s.poller.pollOnce(ctx)
	if h := reviewCache(t, s).Hosts["s3"]; len(h.Suffixes) != 1 || h.Suffixes[0] != "a.example.com" {
		t.Fatalf("cached hosts after the first poll: %+v", h)
	}
	lab.set(func(f *reviewFake) { f.dns = []vast.DNS{{DomainSuffix: "b.example.com", Enabled: true}} })
	s.poller.pollOnce(ctx)
	// What apply now holds for the target (the xDS snapshot itself did not
	// change, so XDS().Targets() still carries the spec of the last publish).
	s.poller.mu.Lock()
	got := s.poller.hosts["s3"].Suffixes
	s.poller.mu.Unlock()
	if len(got) != 1 || got[0] != "b.example.com" {
		t.Fatalf("poller hosts: %v", got)
	}
	if h := reviewCache(t, s).Hosts["s3"]; len(h.Suffixes) != 1 || h.Suffixes[0] != "b.example.com" {
		t.Errorf("served suffix is b.example.com, cached hosts on disk: %+v", h)
	}
}
