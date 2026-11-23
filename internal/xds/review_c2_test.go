package xds

import (
	"context"
	"net"
	"sort"
	"testing"
	"time"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

// Review finding C2: distinct snapshots can have the same xDS version.

// Two servers built back to back, with no help from the test, publish
// different targets at the same sequence number.
func TestReviewC2_SameSecondServersCollide(t *testing.T) {
	a, b := newTestServer(t), newTestServer(t)
	x, y := reviewC1Targets()
	va, _, err := a.Update(context.Background(), []TargetSpec{x})
	if err != nil {
		t.Fatal(err)
	}
	vb, _, err := b.Update(context.Background(), []TargetSpec{y})
	if err != nil {
		t.Fatal(err)
	}
	if va == vb {
		t.Errorf("servers publishing different targets (%s vs %s) both offer version %q", x.Name, y.Name, va)
	}
}

// reviewADS serves s over ADS on a loopback port and opens one stream to it,
// as an Envoy reconnecting to a restarted daemon would.
func reviewADS(t *testing.T, s *Server) discoveryv3.AggregatedDiscoveryService_StreamAggregatedResourcesClient {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := grpc.NewServer()
	s.Register(g)
	go g.Serve(lis)
	t.Cleanup(g.Stop)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(func() { cancel(); conn.Close() })
	st, err := discoveryv3.NewAggregatedDiscoveryServiceClient(conn).StreamAggregatedResources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// reconnect sends a wildcard request for typ carrying heldVersion, the way
// Envoy's first request on a new stream does, and reports either the
// response or that the cache parked the request as already up to date.
func reconnect(t *testing.T, s *Server, typ resource.Type, heldVersion string) (resp *discoveryv3.DiscoveryResponse, parked bool) {
	t.Helper()
	st := reviewADS(t, s)
	base := s.OpenWatches()
	if err := st.Send(&discoveryv3.DiscoveryRequest{
		Node:        &corev3.Node{Id: "envoy-1", Cluster: "deltav"},
		TypeUrl:     typ,
		VersionInfo: heldVersion,
	}); err != nil {
		t.Fatal(err)
	}
	got := make(chan *discoveryv3.DiscoveryResponse, 1)
	go func() {
		r, err := st.Recv()
		if err == nil {
			got <- r
		}
	}()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(20 * time.Second)
	for {
		select {
		case r := <-got:
			return r, false
		case <-tick.C:
			// The cache holds a watch only when it decided not to answer.
			if s.OpenWatches() > base {
				return nil, true
			}
		case <-deadline:
			t.Fatal("neither a response nor a parked watch")
		}
	}
}

func resourceNames(t *testing.T, r *discoveryv3.DiscoveryResponse) []string {
	t.Helper()
	var out []string
	for _, a := range r.GetResources() {
		var m proto.Message
		switch r.GetTypeUrl() {
		case resource.ClusterType:
			m = &clusterv3.Cluster{}
		case resource.ListenerType:
			m = &listenerv3.Listener{}
		}
		if err := a.UnmarshalTo(m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m.(interface{ GetName() string }).GetName())
	}
	sort.Strings(out)
	return out
}

// A daemon restarted within the same second must not offer the version an
// Envoy kept from the previous run. Had they collided, this case would
// still be answered: the reconnecting Envoy's subscription is new, so
// nothing has been "returned" on it and the cache answers despite the
// equal version (go-control-plane pkg/cache/v3/simple.go:426-455).
func TestReviewC2_ReconnectWithCollidingVersion_NonEmpty(t *testing.T) {
	old, restarted := newTestServer(t), newTestServer(t)
	// Built back to back: the restart lands in the same second.
	x, y := reviewC1Targets()
	held, _, _ := old.Update(context.Background(), []TargetSpec{x})
	now, _, _ := restarted.Update(context.Background(), []TargetSpec{y})
	if held == now {
		t.Fatalf("the restarted daemon offers %q, the version the Envoy already holds", held)
	}
	for _, typ := range []resource.Type{resource.ClusterType, resource.ListenerType} {
		r, parked := reconnect(t, restarted, typ, held)
		if parked {
			t.Errorf("%s: Envoy holding %q (target %s) got nothing from a daemon serving target %s at the same version", typ, held, x.Name, y.Name)
			continue
		}
		t.Logf("%s: answered with version %q, resources %v", typ, r.GetVersionInfo(), resourceNames(t, r))
	}
}

// The case a version collision broke: the restarted daemon has no resource
// of the type (its targets were all removed). A wildcard request carrying
// an equal version and nothing to "return" is parked, and the Envoy would
// keep the old daemon's clusters and listeners.
func TestReviewC2_ReconnectWithCollidingVersion_Empty(t *testing.T) {
	old, restarted := newTestServer(t), newTestServer(t)
	x, y := reviewC1Targets()
	ctx := context.Background()
	old.Update(ctx, []TargetSpec{x})
	held, _, _ := old.Update(ctx, []TargetSpec{x, y}) // seq 3: x and y
	restarted.Update(ctx, []TargetSpec{y})
	now, _, _ := restarted.Update(ctx, nil) // seq 3: nothing
	if held == now {
		t.Fatalf("the restarted daemon offers %q, the version the Envoy already holds", held)
	}
	for _, typ := range []resource.Type{resource.ClusterType, resource.ListenerType} {
		r, parked := reconnect(t, restarted, typ, held)
		if parked {
			t.Errorf("%s: Envoy holding %q with targets [a b] was not told the restarted daemon serves none; it keeps stale config", typ, held)
			continue
		}
		t.Logf("%s: answered with version %q, resources %v", typ, r.GetVersionInfo(), resourceNames(t, r))
	}
}
