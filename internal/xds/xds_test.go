package xds

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"

	bootstrapv3 "github.com/envoyproxy/go-control-plane/envoy/config/bootstrap/v3"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	"google.golang.org/protobuf/encoding/protojson"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	s, err := New(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestUpdateVersions(t *testing.T) {
	ctx := context.Background()
	s := newTestServer(t)
	v0 := s.Version()
	if v0 == "" {
		t.Fatal("a new server must already offer an (empty) snapshot")
	}

	a := testTarget()
	b := TargetSpec{Name: "other", ListenPort: 9001, Endpoints: []Endpoint{{IP: "192.0.2.50", Port: 443}}}

	v1, changed, err := s.Update(ctx, []TargetSpec{a, b})
	if err != nil || !changed || v1 == v0 {
		t.Fatalf("first update: version %q -> %q, changed %v, err %v", v0, v1, changed, err)
	}
	// Same targets in another order: nothing to push.
	v2, changed, err := s.Update(ctx, []TargetSpec{b, a})
	if err != nil || changed || v2 != v1 {
		t.Fatalf("identical update: version %q -> %q, changed %v, err %v", v1, v2, changed, err)
	}
	if got := s.Targets(); len(got) != 2 || got[0].Name != "other" || got[1].Name != "s3" {
		t.Fatalf("targets = %+v, want other then s3", got)
	}

	a.Endpoints = a.Endpoints[:1]
	v3, changed, err := s.Update(ctx, []TargetSpec{a, b})
	if err != nil || !changed || v3 == v1 {
		t.Fatalf("endpoint removal: version %q -> %q, changed %v, err %v", v1, v3, changed, err)
	}
	if s.Version() != v3 {
		t.Errorf("Version() = %q, want %q", s.Version(), v3)
	}

	v4, changed, err := s.Update(ctx, nil)
	if err != nil || !changed || v4 == v3 || len(s.Targets()) != 0 {
		t.Fatalf("clearing targets: version %q, changed %v, err %v", v4, changed, err)
	}
}

// Versions must differ between daemon runs: an Envoy that reconnects after
// a restart reports the version it holds, and is only sent config if the
// daemon's differs.
func TestVersionsDifferAcrossServers(t *testing.T) {
	a, b := newTestServer(t), newTestServer(t) // built within the same second
	va, _, _ := a.Update(context.Background(), []TargetSpec{testTarget()})
	vb, _, _ := b.Update(context.Background(), []TargetSpec{testTarget()})
	if va == vb {
		t.Fatalf("both servers produced version %q", va)
	}
}

func TestUpdateRejectsConflicts(t *testing.T) {
	ctx := context.Background()
	s := newTestServer(t)
	good, _, err := s.Update(ctx, []TargetSpec{testTarget()})
	if err != nil {
		t.Fatal(err)
	}
	a := testTarget()
	sameName := TargetSpec{Name: "s3", ListenPort: 9001}
	samePort := TargetSpec{Name: "other", ListenPort: 9000}
	invalid := TargetSpec{Name: "bad", ListenPort: 9002, Endpoints: []Endpoint{{IP: "not-an-ip", Port: 80}}}
	for name, targets := range map[string][]TargetSpec{
		"duplicate name": {a, sameName},
		"duplicate port": {a, samePort},
		"invalid target": {a, invalid},
	} {
		if _, _, err := s.Update(ctx, targets); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if s.Version() != good || len(s.Targets()) != 1 {
		t.Errorf("a rejected update changed the snapshot: version %q, %d targets", s.Version(), len(s.Targets()))
	}
}

func TestBootstrap(t *testing.T) {
	b, err := Bootstrap(BootstrapOptions{XDSAddress: "192.0.2.10:18000", AdminAddress: "127.0.0.1:9901"})
	if err != nil {
		t.Fatal(err)
	}
	if b.GetNode().GetId() != "" || b.GetNode().GetCluster() != DefaultNodeCluster {
		t.Errorf("node = %v: the ID must be left to --service-node", b.GetNode())
	}
	if len(b.GetStaticResources().GetClusters()) != 1 {
		t.Fatalf("want one static cluster")
	}
	c := b.GetStaticResources().GetClusters()[0]
	if c.GetName() != ControlCluster || c.GetType() != clusterv3.Cluster_STATIC {
		t.Errorf("control cluster %q type %v", c.GetName(), c.GetType())
	}
	sa := c.GetLoadAssignment().GetEndpoints()[0].GetLbEndpoints()[0].GetEndpoint().GetAddress().GetSocketAddress()
	if sa.GetAddress() != "192.0.2.10" || sa.GetPortValue() != 18000 {
		t.Errorf("control endpoint = %v", sa)
	}
	ads := b.GetDynamicResources().GetAdsConfig()
	if ads.GetApiType() != corev3.ApiConfigSource_GRPC || ads.GetTransportApiVersion() != corev3.ApiVersion_V3 ||
		!ads.GetSetNodeOnFirstMessageOnly() || ads.GetGrpcServices()[0].GetEnvoyGrpc().GetClusterName() != ControlCluster {
		t.Errorf("ads_config = %v", ads)
	}
	lrs := b.GetClusterManager().GetLoadStatsConfig()
	if lrs.GetApiType() != corev3.ApiConfigSource_GRPC || lrs.GetTransportApiVersion() != corev3.ApiVersion_V3 ||
		lrs.GetGrpcServices()[0].GetEnvoyGrpc().GetClusterName() != ControlCluster {
		t.Errorf("load_stats_config = %v", lrs)
	}
	if b.GetDynamicResources().GetCdsConfig().GetAds() == nil || b.GetDynamicResources().GetLdsConfig().GetAds() == nil {
		t.Error("CDS and LDS must come over ADS")
	}
	if a := b.GetAdmin().GetAddress().GetSocketAddress(); a.GetAddress() != "127.0.0.1" || a.GetPortValue() != 9901 {
		t.Errorf("admin = %v", a)
	}

	// A hostname needs DNS; no admin address means no admin interface.
	b, err = Bootstrap(BootstrapOptions{XDSAddress: "deltav.example.com:18000"})
	if err != nil {
		t.Fatal(err)
	}
	if got := b.GetStaticResources().GetClusters()[0].GetType(); got != clusterv3.Cluster_STRICT_DNS {
		t.Errorf("hostname control cluster type = %v", got)
	}
	if b.GetAdmin() != nil {
		t.Error("admin configured without an address")
	}
}

func TestBootstrapJSONRoundTrips(t *testing.T) {
	out, err := BootstrapJSON(BootstrapOptions{XDSAddress: "[2001:db8::1]:18000", NodeCluster: "lab"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(out), "}\n") {
		t.Errorf("output does not end in a newline")
	}
	var b bootstrapv3.Bootstrap
	if err := protojson.Unmarshal(out, &b); err != nil {
		t.Fatalf("output is not a Bootstrap: %v\n%s", err, out)
	}
	if b.GetNode().GetCluster() != "lab" {
		t.Errorf("node cluster = %q", b.GetNode().GetCluster())
	}
	sa := b.GetStaticResources().GetClusters()[0].GetLoadAssignment().GetEndpoints()[0].GetLbEndpoints()[0].GetEndpoint().GetAddress().GetSocketAddress()
	if net.ParseIP(sa.GetAddress()) == nil || sa.GetPortValue() != 18000 {
		t.Errorf("control endpoint = %v", sa)
	}
}

func TestBootstrapRejectsBadAddresses(t *testing.T) {
	for _, o := range []BootstrapOptions{
		{},
		{XDSAddress: "no-port"},
		{XDSAddress: "host:0"},
		{XDSAddress: "0.0.0.0:18000"},
		{XDSAddress: "[::]:18000"},
		{XDSAddress: "192.0.2.10:18000", AdminAddress: "localhost:9901"},
		{XDSAddress: "192.0.2.10:18000", AdminAddress: "9901"},
	} {
		if _, err := Bootstrap(o); err == nil {
			t.Errorf("%+v: want an error", o)
		}
	}
}
