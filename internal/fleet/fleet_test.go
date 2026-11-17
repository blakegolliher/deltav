package fleet

import (
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"google.golang.org/genproto/googleapis/rpc/status"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newRegistry() (*Registry, *clock) {
	c := &clock{t: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
	r := New()
	r.now = c.now
	return r, c
}

func subscribe(typeURL, nodeID string) *discoveryv3.DiscoveryRequest {
	req := &discoveryv3.DiscoveryRequest{TypeUrl: typeURL}
	if nodeID != "" {
		req.Node = &corev3.Node{Id: nodeID, Cluster: "deltav", UserAgentVersionType: &corev3.Node_UserAgentVersion{UserAgentVersion: "1.40.2-dev"}}
	}
	return req
}

func push(typeURL, version, nonce string) *discoveryv3.DiscoveryResponse {
	return &discoveryv3.DiscoveryResponse{TypeUrl: typeURL, VersionInfo: version, Nonce: nonce}
}

func ack(typeURL, version, nonce string) *discoveryv3.DiscoveryRequest {
	return &discoveryv3.DiscoveryRequest{TypeUrl: typeURL, VersionInfo: version, ResponseNonce: nonce}
}

func mustGet(t *testing.T, r *Registry, id string) Member {
	t.Helper()
	m, ok := r.Get(id)
	if !ok {
		t.Fatalf("member %q not registered", id)
	}
	return m
}

func TestLifecycle(t *testing.T) {
	r, c := newRegistry()
	r.StreamOpen(1, "10.0.0.5:40000")
	if len(r.List()) != 0 {
		t.Fatal("a stream with no request yet must not be listed: its node is unknown")
	}
	opened := c.t
	c.advance(time.Second)

	r.Request(1, subscribe(resource.ClusterType, "host-a"))
	m := mustGet(t, r, "host-a")
	if !m.Connected || m.Address != "10.0.0.5:40000" || !m.ConnectedAt.Equal(opened) || !m.LastSeen.Equal(c.t) ||
		m.EnvoyVersion != "1.40.2-dev" || m.Cluster != "deltav" || m.ConfigState != StatePending {
		t.Fatalf("after first request: %+v", m)
	}

	r.Response(1, push(resource.ClusterType, "v1", "1"))
	r.Request(1, ack(resource.ClusterType, "v1", "1"))
	r.Request(1, subscribe(resource.ListenerType, ""))
	if m = mustGet(t, r, "host-a"); m.ConfigState != StatePending || m.ConfigVersion != "" {
		t.Fatalf("one type acked, one unanswered: state %s version %q", m.ConfigState, m.ConfigVersion)
	}
	r.Response(1, push(resource.ListenerType, "v1", "2"))
	r.Request(1, ack(resource.ListenerType, "v1", "2"))
	if m = mustGet(t, r, "host-a"); m.ConfigState != StateAcked || m.ConfigVersion != "v1" || len(m.Types) != 2 {
		t.Fatalf("all acked: state %s version %q types %d", m.ConfigState, m.ConfigVersion, len(m.Types))
	}
	if m.Types[0].Type != "cds" || m.Types[1].Type != "lds" {
		t.Errorf("types not sorted by short name: %+v", m.Types)
	}

	// NACK: the request carries the version still in force and an error.
	r.Response(1, push(resource.ListenerType, "v2", "3"))
	nack := ack(resource.ListenerType, "v1", "3")
	nack.ErrorDetail = &status.Status{Code: 13, Message: "cannot bind"}
	r.Request(1, nack)
	m = mustGet(t, r, "host-a")
	if m.ConfigState != StateNacked || m.ConfigError != "lds: cannot bind" || m.ConfigVersion != "" {
		t.Fatalf("after NACK: state %s error %q version %q", m.ConfigState, m.ConfigError, m.ConfigVersion)
	}
	if lds := m.Types[1]; lds.SentVersion != "v2" || lds.AckedVersion != "v1" {
		t.Errorf("lds after NACK: sent %q acked %q", lds.SentVersion, lds.AckedVersion)
	}

	// A new push supersedes the NACK; the ACK clears it.
	r.Response(1, push(resource.ListenerType, "v3", "4"))
	if m = mustGet(t, r, "host-a"); m.ConfigState != StatePending || m.ConfigError != "" {
		t.Fatalf("after re-push: state %s error %q", m.ConfigState, m.ConfigError)
	}
	r.Request(1, ack(resource.ListenerType, "v3", "4"))
	if m = mustGet(t, r, "host-a"); m.ConfigState != StateAcked || m.ConfigVersion != "" {
		t.Fatalf("cds at v1 and lds at v3 must not report one version: state %s version %q", m.ConfigState, m.ConfigVersion)
	}

	c.advance(time.Minute)
	r.StreamClosed(1)
	m = mustGet(t, r, "host-a")
	if m.Connected || m.DisconnectedAt == nil || !m.DisconnectedAt.Equal(c.t) {
		t.Fatalf("after close: %+v", m)
	}
}

func TestStaleNonceIsNotAnACK(t *testing.T) {
	r, _ := newRegistry()
	r.StreamOpen(1, "")
	r.Request(1, subscribe(resource.ClusterType, "host-a"))
	r.Response(1, push(resource.ClusterType, "v1", "1"))
	r.Response(1, push(resource.ClusterType, "v2", "2"))
	r.Request(1, ack(resource.ClusterType, "v1", "1")) // answers a response since replaced
	m := mustGet(t, r, "host-a")
	if m.ConfigState != StatePending || m.Types[0].AckedVersion != "" {
		t.Fatalf("stale ACK was counted: %+v", m.Types[0])
	}
}

// An Envoy that restarts can open its new stream before the old one is seen
// closing. The old stream's late events must not disturb the new state.
func TestNewStreamSupersedesOld(t *testing.T) {
	r, c := newRegistry()
	r.StreamOpen(1, "10.0.0.5:40000")
	r.Request(1, subscribe(resource.ClusterType, "host-a"))
	r.Response(1, push(resource.ClusterType, "v1", "1"))
	r.Request(1, ack(resource.ClusterType, "v1", "1"))

	c.advance(time.Second)
	r.StreamOpen(2, "10.0.0.5:40001")
	r.Request(2, subscribe(resource.ClusterType, "host-a"))
	m := mustGet(t, r, "host-a")
	if m.Address != "10.0.0.5:40001" || m.ConfigState != StatePending || len(m.Types) != 1 || m.Types[0].AckedVersion != "" {
		t.Fatalf("new stream did not reset state: %+v", m)
	}

	r.Response(1, push(resource.ClusterType, "v9", "7"))
	r.Request(1, ack(resource.ClusterType, "v9", "7"))
	r.StreamClosed(1)
	m = mustGet(t, r, "host-a")
	if !m.Connected || m.Types[0].SentVersion != "" || m.Types[0].AckedVersion != "" {
		t.Fatalf("old stream changed the member: %+v", m)
	}
	if len(r.List()) != 1 {
		t.Fatalf("want one member, got %d", len(r.List()))
	}
}

func TestRequestWithoutNodeIDIsIgnored(t *testing.T) {
	r, _ := newRegistry()
	r.StreamOpen(1, "")
	r.Request(1, &discoveryv3.DiscoveryRequest{TypeUrl: resource.ClusterType})
	r.Request(1, &discoveryv3.DiscoveryRequest{TypeUrl: resource.ClusterType, Node: &corev3.Node{}})
	if len(r.List()) != 0 {
		t.Fatalf("registered a member with no node ID: %+v", r.List())
	}
}

func TestPrune(t *testing.T) {
	r, c := newRegistry()
	for id, node := range map[int64]string{1: "gone-long", 2: "gone-recent", 3: "connected"} {
		r.StreamOpen(id, "")
		r.Request(id, subscribe(resource.ClusterType, node))
	}
	r.StreamClosed(1)
	c.advance(50 * time.Minute)
	r.StreamClosed(2)
	c.advance(20 * time.Minute)

	if n := r.Prune(time.Hour); n != 1 {
		t.Fatalf("pruned %d, want 1", n)
	}
	got := r.List()
	if len(got) != 2 || got[0].NodeID != "connected" || got[1].NodeID != "gone-recent" {
		t.Fatalf("after prune: %+v", got)
	}
}

func TestEnvoyVersion(t *testing.T) {
	if got := envoyVersion(&corev3.Node{UserAgentVersionType: &corev3.Node_UserAgentVersion{UserAgentVersion: "abc"}}); got != "abc" {
		t.Errorf("free-form version = %q", got)
	}
	if got := envoyVersion(&corev3.Node{}); got != "" {
		t.Errorf("no version = %q", got)
	}
}
