package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	discoverygrpc "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"

	"deltav/internal/fleet"
	"deltav/internal/xds"
)

// adsClient speaks Envoy's side of the state-of-the-world ADS protocol. It
// is a protocol test double only: the data path is tested with real Envoy
// in test/integration.
type adsClient struct {
	t      *testing.T
	stream discoverygrpc.AggregatedDiscoveryService_StreamAggregatedResourcesClient
	node   *corev3.Node
	sent   bool
}

func semver(major, minor, patch uint32) *typev3.SemanticVersion {
	return &typev3.SemanticVersion{MajorNumber: major, MinorNumber: minor, Patch: patch}
}

func dialADS(t *testing.T, s *Server, nodeID string) *adsClient {
	t.Helper()
	conn, err := grpc.NewClient(s.GRPCAddr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(func() { cancel(); conn.Close() })
	stream, err := discoverygrpc.NewAggregatedDiscoveryServiceClient(conn).StreamAggregatedResources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return &adsClient{t: t, stream: stream, node: &corev3.Node{
		Id:                   nodeID,
		Cluster:              "deltav",
		UserAgentName:        "envoy",
		UserAgentVersionType: &corev3.Node_UserAgentBuildVersion{UserAgentBuildVersion: &corev3.BuildVersion{Version: semver(1, 40, 2)}},
	}}
}

// send issues a request. Like Envoy with set_node_on_first_message_only,
// only the first request on the stream carries the node.
func (c *adsClient) send(req *discoverygrpc.DiscoveryRequest) {
	c.t.Helper()
	if !c.sent {
		req.Node = c.node
		c.sent = true
	}
	if err := c.stream.Send(req); err != nil {
		c.t.Fatalf("ADS send: %v", err)
	}
}

func (c *adsClient) recv() *discoverygrpc.DiscoveryResponse {
	c.t.Helper()
	resp, err := c.stream.Recv()
	if err != nil {
		c.t.Fatalf("ADS recv: %v", err)
	}
	return resp
}

func (c *adsClient) ack(resp *discoverygrpc.DiscoveryResponse, names ...string) {
	c.send(&discoverygrpc.DiscoveryRequest{
		TypeUrl: resp.GetTypeUrl(), VersionInfo: resp.GetVersionInfo(), ResponseNonce: resp.GetNonce(), ResourceNames: names,
	})
}

func (c *adsClient) nack(resp *discoverygrpc.DiscoveryResponse, accepted, msg string, names ...string) {
	c.send(&discoverygrpc.DiscoveryRequest{
		TypeUrl: resp.GetTypeUrl(), VersionInfo: accepted, ResponseNonce: resp.GetNonce(), ResourceNames: names,
		ErrorDetail: &status.Status{Code: 13, Message: msg},
	})
}

func fleetOf(t *testing.T, s *Server) fleetResponse {
	t.Helper()
	code, body := get(t, "http://"+s.HTTPAddr()+"/api/v1/fleet", "")
	var f fleetResponse
	if code != 200 || json.Unmarshal([]byte(body), &f) != nil {
		t.Fatalf("/api/v1/fleet: %d %s", code, body)
	}
	return f
}

// waitMember polls the fleet API until ok accepts the member. Callbacks run
// on the stream's goroutine, so state trails a Send by a moment.
func waitMember(t *testing.T, s *Server, nodeID, what string, ok func(fleet.Member) bool) fleet.Member {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last fleet.Member
	for time.Now().Before(deadline) {
		for _, m := range fleetOf(t, s).Members {
			if m.NodeID == nodeID {
				if last = m.Member; ok(last) {
					return last
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	b, _ := json.MarshalIndent(last, "", "  ")
	t.Fatalf("member %s never reached: %s\nlast seen state:\n%s", nodeID, what, b)
	return last
}

// settle waits until the cache holds n open watches, that is, until every
// ACK sent so far has been processed. A push made before that can reach a
// stream out of order, which is allowed but would make order assertions
// flaky.
func settle(t *testing.T, s *Server, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for s.XDS().OpenWatches() != n {
		if time.Now().After(deadline) {
			t.Fatalf("%d open watches, want %d", s.XDS().OpenWatches(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func unpack[T proto.Message](t *testing.T, resp *discoverygrpc.DiscoveryResponse, into T) T {
	t.Helper()
	if len(resp.GetResources()) != 1 {
		t.Fatalf("%s: %d resources, want 1", resp.GetTypeUrl(), len(resp.GetResources()))
	}
	if err := resp.GetResources()[0].UnmarshalTo(into); err != nil {
		t.Fatal(err)
	}
	return into
}

func TestADSFlowAndFleetRegistry(t *testing.T) {
	target := xds.TargetSpec{Name: "s3", ListenPort: 9000, Endpoints: []xds.Endpoint{
		{IP: "192.0.2.11", Port: 80}, {IP: "192.0.2.12", Port: 80},
	}}
	s := start(t, Config{GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0", StaticTargets: []xds.TargetSpec{target}})
	v1 := s.XDS().Version()

	// The targets API reports what is being served.
	code, body := get(t, "http://"+s.HTTPAddr()+"/api/v1/targets", "")
	var targets targetsResponse
	if code != 200 || json.Unmarshal([]byte(body), &targets) != nil {
		t.Fatalf("/api/v1/targets: %d %s", code, body)
	}
	if targets.ConfigVersion != v1 || len(targets.Targets) != 1 || targets.Targets[0].Name != "s3" ||
		targets.Targets[0].ListenPort != 9000 || len(targets.Targets[0].Endpoints) != 2 {
		t.Fatalf("/api/v1/targets = %s", body)
	}
	if f := fleetOf(t, s); f.Total != 0 || f.ConfigVersion != v1 {
		t.Fatalf("fleet before any Envoy connects: %+v", f)
	}

	// Envoy's startup order: clusters, then their endpoints, then listeners.
	c := dialADS(t, s, "host-a")
	c.send(&discoverygrpc.DiscoveryRequest{TypeUrl: resource.ClusterType})
	cds := c.recv()
	if cds.GetTypeUrl() != resource.ClusterType || cds.GetVersionInfo() != v1 {
		t.Fatalf("CDS response: type %s version %s", cds.GetTypeUrl(), cds.GetVersionInfo())
	}
	if got := unpack(t, cds, &clusterv3.Cluster{}).GetName(); got != "s3" {
		t.Fatalf("cluster name = %q", got)
	}
	waitMember(t, s, "host-a", "registered with CDS pending", func(m fleet.Member) bool {
		return m.Connected && m.ConfigState == fleet.StatePending && m.EnvoyVersion == "1.40.2" && m.Cluster == "deltav"
	})
	c.ack(cds)

	c.send(&discoverygrpc.DiscoveryRequest{TypeUrl: resource.EndpointType, ResourceNames: []string{"s3"}})
	eds := c.recv()
	cla := unpack(t, eds, &endpointv3.ClusterLoadAssignment{})
	if n := len(cla.GetEndpoints()[0].GetLbEndpoints()); cla.GetClusterName() != "s3" || n != 2 {
		t.Fatalf("EDS: cluster %q with %d endpoints", cla.GetClusterName(), n)
	}
	c.ack(eds, "s3")

	c.send(&discoverygrpc.DiscoveryRequest{TypeUrl: resource.ListenerType})
	lds := c.recv()
	if got := unpack(t, lds, &listenerv3.Listener{}).GetAddress().GetSocketAddress().GetPortValue(); got != 9000 {
		t.Fatalf("listener port = %d", got)
	}
	c.ack(lds)

	m := waitMember(t, s, "host-a", "every type acked at v1", func(m fleet.Member) bool {
		return m.ConfigState == fleet.StateAcked && m.ConfigVersion == v1 && len(m.Types) == 3
	})
	if m.Address == "" || m.ConnectedAt.IsZero() || m.LastSeen.Before(m.ConnectedAt) {
		t.Errorf("member bookkeeping: %+v", m)
	}

	// Dropping an endpoint pushes all three types under one new version.
	target.Endpoints = target.Endpoints[:1]
	v2, changed, err := s.XDS().Update(context.Background(), []xds.TargetSpec{target})
	if err != nil || !changed || v2 == v1 {
		t.Fatalf("update: %q -> %q changed=%v err=%v", v1, v2, changed, err)
	}
	pushed := map[string]*discoverygrpc.DiscoveryResponse{}
	for len(pushed) < 3 {
		r := c.recv()
		if r.GetVersionInfo() != v2 {
			t.Fatalf("%s pushed at version %q, want %q", r.GetTypeUrl(), r.GetVersionInfo(), v2)
		}
		pushed[r.GetTypeUrl()] = r
	}
	cla = unpack(t, pushed[resource.EndpointType], &endpointv3.ClusterLoadAssignment{})
	if n := len(cla.GetEndpoints()[0].GetLbEndpoints()); n != 1 {
		t.Fatalf("EDS after update has %d endpoints, want 1", n)
	}
	waitMember(t, s, "host-a", "pending after a push", func(m fleet.Member) bool {
		return m.ConfigState == fleet.StatePending && m.ConfigVersion == ""
	})

	// Envoy accepts clusters and endpoints but rejects the listener.
	c.ack(pushed[resource.ClusterType])
	c.ack(pushed[resource.EndpointType], "s3")
	c.nack(pushed[resource.ListenerType], v1, "cannot bind '127.0.0.1:9000': Address already in use")
	m = waitMember(t, s, "host-a", "nacked", func(m fleet.Member) bool { return m.ConfigState == fleet.StateNacked })
	if m.ConfigError != "lds: cannot bind '127.0.0.1:9000': Address already in use" || m.ConfigVersion != "" {
		t.Errorf("after NACK: error %q, version %q", m.ConfigError, m.ConfigVersion)
	}
	for _, ts := range m.Types {
		wantState, wantAcked := fleet.StateAcked, v2
		if ts.Type == "lds" {
			wantState, wantAcked = fleet.StateNacked, v1
		}
		if ts.State != wantState || ts.AckedVersion != wantAcked || ts.SentVersion != v2 {
			t.Errorf("%s: state %s acked %q sent %q; want %s acked %q sent %q",
				ts.Type, ts.State, ts.AckedVersion, ts.SentVersion, wantState, wantAcked, v2)
		}
	}

	// A rejected config must not be sent again until it changes: the cache
	// would otherwise re-send it at once, and Envoy would reject it again,
	// in a tight loop (see xds.withNACKGuard).
	time.Sleep(300 * time.Millisecond)
	m = waitMember(t, s, "host-a", "still nacked", func(m fleet.Member) bool { return true })
	if m.ConfigState != fleet.StateNacked {
		t.Fatalf("after a NACK the config was pushed again: state %s", m.ConfigState)
	}

	// The next change is pushed as usual, and an ACK clears the error.
	target.Endpoints = append(target.Endpoints, xds.Endpoint{IP: "192.0.2.13", Port: 80})
	v3, _, err := s.XDS().Update(context.Background(), []xds.TargetSpec{target})
	if err != nil || v3 == v2 {
		t.Fatalf("second update: %q -> %q err=%v", v2, v3, err)
	}
	for range 3 {
		r := c.recv()
		if r.GetVersionInfo() != v3 {
			t.Fatalf("%s pushed at version %q, want %q", r.GetTypeUrl(), r.GetVersionInfo(), v3)
		}
		if r.GetTypeUrl() == resource.EndpointType {
			c.ack(r, "s3")
		} else {
			c.ack(r)
		}
	}
	waitMember(t, s, "host-a", "recovered at v3", func(m fleet.Member) bool {
		return m.ConfigState == fleet.StateAcked && m.ConfigVersion == v3 && m.ConfigError == ""
	})
	v2 = v3

	// A second Envoy gets the current snapshot; the first is unaffected.
	c2 := dialADS(t, s, "host-b")
	c2.send(&discoverygrpc.DiscoveryRequest{TypeUrl: resource.ClusterType})
	if r := c2.recv(); r.GetVersionInfo() != v2 {
		t.Fatalf("second Envoy got version %q, want %q", r.GetVersionInfo(), v2)
	}
	waitMember(t, s, "host-b", "registered", func(m fleet.Member) bool { return m.Connected })
	if f := fleetOf(t, s); f.Total != 2 || f.Connected != 2 || f.ConfigVersion != v2 {
		t.Fatalf("fleet with two Envoys: total %d connected %d version %q", f.Total, f.Connected, f.ConfigVersion)
	}

	// Closing the stream leaves the member listed as disconnected.
	if err := c.stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	m = waitMember(t, s, "host-a", "disconnected", func(m fleet.Member) bool { return !m.Connected })
	if m.DisconnectedAt == nil {
		t.Error("disconnected member has no disconnected_at")
	}
	if f := fleetOf(t, s); f.Total != 2 || f.Connected != 1 {
		t.Fatalf("after disconnect: total %d connected %d", f.Total, f.Connected)
	}
}

// An Envoy that connects before any target exists is answered with empty
// config it can ACK, not left waiting.
func TestADSEmptySnapshotIsAnswered(t *testing.T) {
	s := start(t, Config{GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0"})
	c := dialADS(t, s, "host-a")
	for _, typeURL := range []string{resource.ClusterType, resource.ListenerType} {
		c.send(&discoverygrpc.DiscoveryRequest{TypeUrl: typeURL})
		r := c.recv()
		if r.GetTypeUrl() != typeURL || len(r.GetResources()) != 0 || r.GetVersionInfo() != s.XDS().Version() {
			t.Fatalf("%s: %d resources at version %q", typeURL, len(r.GetResources()), r.GetVersionInfo())
		}
		c.ack(r)
	}
	waitMember(t, s, "host-a", "acked empty config", func(m fleet.Member) bool {
		return m.ConfigState == fleet.StateAcked && m.ConfigVersion == s.XDS().Version()
	})
}

// An Envoy that rejects one target's cluster never asks for that cluster's
// endpoints. It must still be sent endpoint updates for the targets it did
// accept, then and after the bad target is withdrawn.
func TestEndpointsFlowDespiteRejectedCluster(t *testing.T) {
	good := xds.TargetSpec{Name: "s3", ListenPort: 9000, Endpoints: []xds.Endpoint{{IP: "192.0.2.11", Port: 80}}}
	bad := xds.TargetSpec{Name: "broken", ListenPort: 9001, Endpoints: []xds.Endpoint{{IP: "192.0.2.99", Port: 443}}}
	s := start(t, Config{GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0", StaticTargets: []xds.TargetSpec{good}})
	c := dialADS(t, s, "host-a")
	c.send(&discoverygrpc.DiscoveryRequest{TypeUrl: resource.ClusterType})
	cds := c.recv()
	c.ack(cds)
	c.send(&discoverygrpc.DiscoveryRequest{TypeUrl: resource.EndpointType, ResourceNames: []string{"s3"}})
	c.ack(c.recv(), "s3")
	c.send(&discoverygrpc.DiscoveryRequest{TypeUrl: resource.ListenerType})
	c.ack(c.recv())

	// handle answers one push the way an Envoy that cannot build "broken"
	// would: it rejects CDS when that cluster is present and never
	// subscribes to its endpoints.
	accepted := cds.GetVersionInfo()
	handle := func(want string, endpoints int) {
		t.Helper()
		for i, typeURL := range []string{resource.ClusterType, resource.EndpointType, resource.ListenerType} {
			r := c.recv()
			if r.GetTypeUrl() != typeURL || r.GetVersionInfo() != want {
				t.Fatalf("push %d: got %s at %q, want %s at %q (clusters, then endpoints, then listeners)",
					i, r.GetTypeUrl(), r.GetVersionInfo(), typeURL, want)
			}
			switch typeURL {
			case resource.ClusterType:
				if len(r.GetResources()) > 1 {
					c.nack(r, accepted, "Error adding/updating cluster(s) broken: Invalid path: /nonexistent/ca.pem")
				} else {
					c.ack(r)
					accepted = r.GetVersionInfo()
				}
			case resource.EndpointType:
				cla := unpack(t, r, &endpointv3.ClusterLoadAssignment{})
				if n := len(cla.GetEndpoints()[0].GetLbEndpoints()); cla.GetClusterName() != "s3" || n != endpoints {
					t.Fatalf("EDS at %q: cluster %q with %d endpoints, want s3 with %d", want, cla.GetClusterName(), n, endpoints)
				}
				c.ack(r, "s3")
			default:
				c.ack(r)
			}
		}
	}
	update := func(targets ...xds.TargetSpec) string {
		t.Helper()
		settle(t, s, 3) // a rejected type keeps its watch too
		v, changed, err := s.XDS().Update(context.Background(), targets)
		if err != nil || !changed {
			t.Fatalf("update: changed=%v err=%v", changed, err)
		}
		return v
	}

	handle(update(good, bad), 1)
	waitMember(t, s, "host-a", "nacked", func(m fleet.Member) bool { return m.ConfigState == fleet.StateNacked })

	// While the bad target is still there, the good one gains an endpoint.
	good.Endpoints = append(good.Endpoints, xds.Endpoint{IP: "192.0.2.12", Port: 80})
	handle(update(good, bad), 2)

	// The bad target is withdrawn; a further endpoint change still arrives.
	v := update(good)
	handle(v, 2)
	waitMember(t, s, "host-a", "in sync once the bad target is gone", func(m fleet.Member) bool {
		return m.ConfigState == fleet.StateAcked && m.ConfigVersion == v
	})
	good.Endpoints = good.Endpoints[:1]
	handle(update(good), 1)
}
