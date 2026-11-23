package xds

import (
	"context"
	"sort"
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	streamv3 "github.com/envoyproxy/go-control-plane/pkg/server/stream/v3"
)

// Review finding C1: a partial xDS publish can defeat rollback.
//
// The failure is produced by the real go-control-plane snapshot cache, not a
// fake: a watch is open on exactly one resource type with a response channel
// nobody reads, and Update runs with a cancelled context. SetSnapshot for
// that type stores the snapshot, then tries to answer the watch, sees the
// cancelled context and returns context.Canceled (simple.go:225-248, 501-506
// in go-control-plane v0.14.0). The steps before it have no watch to
// answer, so they succeed. This is the same error a daemon shutdown (s.ctx
// cancelled) produces when a stream's channel cannot take the push at once.

var reviewC1Types = []resource.Type{resource.ClusterType, resource.EndpointType, resource.ListenerType, resource.RouteType}

func reviewC1Targets() (a, b TargetSpec) {
	a = TargetSpec{Name: "a", ListenPort: 9000, Endpoints: []Endpoint{{IP: "192.0.2.11", Port: 80}}}
	b = TargetSpec{Name: "b", ListenPort: 9001, Endpoints: []Endpoint{{IP: "192.0.2.22", Port: 80}}}
	return a, b
}

// cacheView reports, per type, the version and resource names the cache
// would hand to an Envoy.
func cacheView(t *testing.T, s *Server) map[resource.Type]string {
	t.Helper()
	snap, err := s.cache.GetSnapshot(fleetKey)
	if err != nil {
		t.Fatal(err)
	}
	out := map[resource.Type]string{}
	for _, typ := range reviewC1Types {
		var names []string
		for n := range snap.GetResources(typ) {
			names = append(names, n)
		}
		sort.Strings(names)
		out[typ] = snap.GetVersion(typ) + " " + join(names)
	}
	return out
}

func join(s []string) string {
	r := "["
	for i, x := range s {
		if i > 0 {
			r += ","
		}
		r += x
	}
	return r + "]"
}

// stuckWatch opens a watch on typ that will be answered by the next version
// change, on a channel nobody reads. The subscription names a resource that
// does not exist, so CreateWatch holds the watch rather than answering it.
func stuckWatch(t *testing.T, s *Server, typ resource.Type) (cancel func()) {
	t.Helper()
	snap, err := s.cache.GetSnapshot(fleetKey)
	if err != nil {
		t.Fatal(err)
	}
	req := &discoveryv3.DiscoveryRequest{
		Node:          &corev3.Node{Id: "review-c1"},
		TypeUrl:       typ,
		VersionInfo:   snap.GetVersion(typ),
		ResourceNames: []string{"does-not-exist"},
	}
	sub := streamv3.NewSotwSubscription(req.ResourceNames, false)
	cancel, err = s.cache.CreateWatch(req, sub, make(chan cache.Response))
	if err != nil {
		t.Fatal(err)
	}
	return cancel
}

func TestReviewC1_RollbackAfterPartialPublish(t *testing.T) {
	for i, failing := range reviewC1Types {
		t.Run(failing[len("type.googleapis.com/envoy.config."):], func(t *testing.T) {
			ctx := context.Background()
			s := newTestServer(t)
			a, b := reviewC1Targets()

			vA, _, err := s.Update(ctx, []TargetSpec{a})
			if err != nil {
				t.Fatal(err)
			}
			wantA := cacheView(t, s)

			// Publish B with a failure at step i.
			cancelWatch := stuckWatch(t, s, failing)
			dead, cancel := context.WithCancel(ctx)
			cancel()
			_, _, err = s.Update(dead, []TargetSpec{b})
			cancelWatch()
			if err == nil {
				t.Fatalf("injected failure at step %d (%s) did not surface", i+1, failing)
			}
			t.Logf("publish B failed as injected: %v", err)
			t.Logf("cache after failed publish of B: %v", cacheView(t, s))
			t.Logf("Server.Version()=%q Server.Targets()=%v", s.Version(), names(s.Targets()))

			// Roll back to A, as rulesChanged and the VAST API handlers do.
			vR, changed, err := s.Update(ctx, []TargetSpec{a})
			if err != nil {
				t.Fatalf("rollback to A returned error %v", err)
			}
			got := cacheView(t, s)
			t.Logf("rollback returned version=%q changed=%v (A was %q)", vR, changed, vA)
			for _, typ := range reviewC1Types {
				gotNames := got[typ][len(versionOf(got[typ])):]
				wantNames := wantA[typ][len(versionOf(wantA[typ])):]
				if gotNames != wantNames {
					t.Errorf("after rollback, cache %s = %q; want A's resources %q", typ, got[typ], wantA[typ])
				}
				if v := versionOf(got[typ]); v != vR {
					t.Errorf("after rollback, cache %s at version %q but Update/Version() report %q", typ, v, vR)
				}
			}

			// Retrying the NEW config instead must leave the cache wholly on B.
			vB, changed, err := s.Update(ctx, []TargetSpec{b})
			if err != nil || !changed {
				t.Fatalf("retry of B: version %q changed %v err %v", vB, changed, err)
			}
			for typ, v := range cacheView(t, s) {
				if versionOf(v) != vB || v[len(vB):] != " [b]" {
					t.Errorf("after retry of B, cache %s = %q; want version %q with [b]", typ, v, vB)
				}
			}
		})
	}
}

func versionOf(v string) string {
	for i := range v {
		if v[i] == ' ' {
			return v[:i]
		}
	}
	return v
}

func names(ts []TargetSpec) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.Name)
	}
	return out
}
