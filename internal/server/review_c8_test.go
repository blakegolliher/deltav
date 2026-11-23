package server

// Review C8: a slow cluster must not hold back a healthy cluster's endpoint
// change. pollTimeout (45 s) is a constant with no test knob, so the test
// holds the slow cluster on a channel and gives the healthy change a short
// freshness budget instead.

import (
	"context"
	"testing"
	"time"
)

func TestReviewC8_SlowClusterDoesNotDelayHealthyCluster(t *testing.T) {
	const budget = time.Second
	fast := reviewTopology("main", "10.0.0.1", "10.0.0.2", "10.0.0.3")
	// Named to sort after the fast one: the fast cluster is read first in
	// the poll, so the test also shows a change already read being withheld.
	slow := reviewTopology("main", "10.9.0.1")
	s := reviewServer(t, t.TempDir(), map[string]*reviewFake{"fast-host": fast, "slow-host": slow})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reviewAddCluster(t, s, "a-fast", "fast-host")
	reviewAddCluster(t, s, "z-slow", "slow-host")
	reviewAddTarget(t, s, "s3", "a-fast", "main", 9000)
	s.poller.pollOnce(ctx)
	if spec, _ := reviewSpec(s, "s3"); reviewIPs(spec.Endpoints) != "10.0.0.1,10.0.0.2,10.0.0.3" {
		t.Fatalf("before: %s", reviewIPs(spec.Endpoints))
	}

	// The healthy cluster's topology changes; the other cluster stalls.
	fast.setVIPs("main", "10.0.0.1", "10.0.0.2", "10.0.0.4")
	fastRead := make(chan struct{}, 1)
	fast.set(func(f *reviewFake) { f.dnsRead = fastRead })
	entered, release := slow.hold()
	start := time.Now()
	done := make(chan struct{})
	go func() { s.poller.pollOnce(ctx); close(done) }()
	reviewWait(t, fastRead, "the healthy cluster to be read")
	reviewWait(t, entered, "the poll to reach the stalled cluster")
	readAt := time.Since(start)

	want := "10.0.0.1,10.0.0.2,10.0.0.4"
	deadline := time.NewTimer(budget)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	published := false
wait:
	for {
		if spec, _ := reviewSpec(s, "s3"); reviewIPs(spec.Endpoints) == want {
			published = true
			break
		}
		select {
		case <-deadline.C:
			break wait
		case <-tick.C:
		}
	}
	if !published {
		spec, _ := reviewSpec(s, "s3")
		t.Errorf("healthy cluster read %v into the poll; %v later, with another cluster's read in flight, s3 still serves %s (want %s)",
			readAt.Round(time.Millisecond), budget, reviewIPs(spec.Endpoints), want)
	}
	close(release)
	reviewWait(t, done, "the poll to finish")
	if spec, _ := reviewSpec(s, "s3"); reviewIPs(spec.Endpoints) != want {
		t.Errorf("after the stalled read returned: %s", reviewIPs(spec.Endpoints))
	} else {
		t.Logf("the change was published %v after the poll started, only once the stalled cluster returned", time.Since(start).Round(time.Millisecond))
	}
}
