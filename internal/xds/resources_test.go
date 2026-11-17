package xds

import (
	"strings"
	"testing"
	"time"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	routerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	upstreamhttpv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/upstreams/http/v3"
)

func testTarget() TargetSpec {
	return TargetSpec{
		Name:       "s3",
		ListenPort: 9000,
		Endpoints: []Endpoint{
			{IP: "192.0.2.11", Port: 80, CNode: "cnode-1"},
			{IP: "192.0.2.12", Port: 80},
		},
	}
}

// The settings in this test are the S3 correctness rules from the build
// prompt and DESIGN.md amendment 10. Each one breaks signatures or large
// transfers if it regresses.
func TestListenerS3Settings(t *testing.T) {
	l, _, _, err := testTarget().Resources()
	if err != nil {
		t.Fatal(err)
	}
	sa := l.GetAddress().GetSocketAddress()
	if sa.GetAddress() != "127.0.0.1" || sa.GetPortValue() != 9000 {
		t.Errorf("listener binds %s:%d, want 127.0.0.1:9000", sa.GetAddress(), sa.GetPortValue())
	}
	if n := len(l.GetFilterChains()); n != 1 || len(l.GetFilterChains()[0].GetFilters()) != 1 {
		t.Fatalf("want exactly one filter chain with one network filter")
	}
	hcm := &hcmv3.HttpConnectionManager{}
	if err := l.GetFilterChains()[0].GetFilters()[0].GetTypedConfig().UnmarshalTo(hcm); err != nil {
		t.Fatal(err)
	}

	if hcm.GetNormalizePath() == nil || hcm.GetNormalizePath().GetValue() {
		t.Error("normalize_path must be explicitly false")
	}
	if hcm.GetMergeSlashes() {
		t.Error("merge_slashes must be false")
	}
	if hcm.GetPathWithEscapedSlashesAction() != hcmv3.HttpConnectionManager_KEEP_UNCHANGED {
		t.Errorf("path_with_escaped_slashes_action = %v, want KEEP_UNCHANGED", hcm.GetPathWithEscapedSlashesAction())
	}
	if !hcm.GetProxy_100Continue() {
		t.Error("proxy_100_continue must be on")
	}
	if hcm.GetGenerateRequestId() == nil || hcm.GetGenerateRequestId().GetValue() {
		t.Error("generate_request_id must be explicitly false")
	}
	if got := hcm.GetStreamIdleTimeout().AsDuration(); got != time.Hour {
		t.Errorf("stream_idle_timeout = %v, want 1h", got)
	}
	if hcm.GetRequestTimeout() != nil {
		t.Error("request_timeout must stay unset: it would cut off long uploads")
	}
	if len(hcm.GetAccessLog()) != 0 {
		t.Error("access log configured although AccessLog is false")
	}

	// Streaming both ways: the router is the only HTTP filter (no buffer).
	if len(hcm.GetHttpFilters()) != 1 || hcm.GetHttpFilters()[0].GetName() != "envoy.filters.http.router" {
		t.Fatalf("want only the router filter, got %v", hcm.GetHttpFilters())
	}
	router := &routerv3.Router{}
	if err := hcm.GetHttpFilters()[0].GetTypedConfig().UnmarshalTo(router); err != nil {
		t.Fatal(err)
	}
	if !router.GetSuppressEnvoyHeaders() {
		t.Error("router must suppress x-envoy-* headers")
	}

	if vc := hcm.GetRouteConfig().GetValidateClusters(); vc == nil || vc.GetValue() {
		t.Error("validate_clusters must be explicitly false: a listener may arrive before its cluster")
	}
	vhosts := hcm.GetRouteConfig().GetVirtualHosts()
	if len(vhosts) != 1 || len(vhosts[0].GetRoutes()) != 1 {
		t.Fatalf("want one virtual host with one route")
	}
	if len(vhosts[0].GetDomains()) != 1 || vhosts[0].GetDomains()[0] != "*" {
		t.Errorf("virtual host must match any Host, got %v", vhosts[0].GetDomains())
	}
	ra := vhosts[0].GetRoutes()[0].GetRoute()
	if ra.GetCluster() != "s3" {
		t.Errorf("route cluster = %q", ra.GetCluster())
	}
	if ra.GetTimeout() == nil || ra.GetTimeout().AsDuration() != 0 {
		t.Errorf("route timeout = %v, want an explicit 0s (Envoy defaults to 15s)", ra.GetTimeout())
	}
	if ra.GetRetryPolicy() != nil || vhosts[0].GetRetryPolicy() != nil {
		t.Error("no retry policy may be set")
	}
	if ra.GetHostRewriteSpecifier() != nil {
		t.Error("the Host header must not be rewritten: SigV4 signs it")
	}
	if ra.GetPrefixRewrite() != "" || ra.GetRegexRewrite() != nil {
		t.Error("the path must not be rewritten")
	}
}

func TestListenerAccessLog(t *testing.T) {
	spec := testTarget()
	spec.AccessLog = true
	l, _, _, err := spec.Resources()
	if err != nil {
		t.Fatal(err)
	}
	hcm := &hcmv3.HttpConnectionManager{}
	if err := l.GetFilterChains()[0].GetFilters()[0].GetTypedConfig().UnmarshalTo(hcm); err != nil {
		t.Fatal(err)
	}
	if len(hcm.GetAccessLog()) != 1 || hcm.GetAccessLog()[0].GetName() != "envoy.access_loggers.http_grpc" {
		t.Fatalf("want one gRPC access logger, got %v", hcm.GetAccessLog())
	}
}

func TestClusterSettings(t *testing.T) {
	_, c, _, err := testTarget().Resources()
	if err != nil {
		t.Fatal(err)
	}
	if c.GetType() != clusterv3.Cluster_EDS || c.GetEdsClusterConfig().GetEdsConfig().GetAds() == nil {
		t.Error("cluster must be EDS over ADS")
	}
	if c.GetLbPolicy() != clusterv3.Cluster_LEAST_REQUEST {
		t.Errorf("lb_policy = %v", c.GetLbPolicy())
	}
	if !c.GetIgnoreHealthOnHostRemoval() {
		t.Error("ignore_health_on_host_removal must be on, or drained VIPs keep taking traffic")
	}

	th := c.GetCircuitBreakers().GetThresholds()
	if len(th) != 1 || th[0].GetMaxConnections().GetValue() != circuitBreakerMax ||
		th[0].GetMaxRequests().GetValue() != circuitBreakerMax || th[0].GetMaxPendingRequests().GetValue() != circuitBreakerMax {
		t.Errorf("cluster circuit breakers not raised above Envoy's 1024 default: %v", th)
	}
	if len(c.GetCircuitBreakers().GetPerHostThresholds()) != 0 {
		t.Error("per-host thresholds set although PerHostLimit is 0")
	}

	od := c.GetOutlierDetection()
	if od.GetMaxEjectionPercent().GetValue() != 34 || !od.GetAlwaysEjectOneHost().GetValue() {
		t.Errorf("ejection limits: max %d%%, always_eject_one_host %v",
			od.GetMaxEjectionPercent().GetValue(), od.GetAlwaysEjectOneHost().GetValue())
	}
	if od.GetConsecutive_5Xx().GetValue() != 5 || od.GetConsecutiveGatewayFailure().GetValue() != 5 ||
		od.GetEnforcingConsecutiveGatewayFailure().GetValue() != 100 || !od.GetSplitExternalLocalOriginErrors() {
		t.Errorf("outlier detection thresholds: %v", od)
	}
	if od.GetEnforcingSuccessRate() == nil || od.GetEnforcingSuccessRate().GetValue() != 0 {
		t.Error("success-rate ejection must be explicitly off")
	}

	hcs := c.GetHealthChecks()
	if len(hcs) != 1 || hcs[0].GetTcpHealthCheck() == nil {
		t.Fatalf("want one TCP health check, got %v", hcs)
	}
	if hcs[0].GetTcpHealthCheck().GetSend() != nil || len(hcs[0].GetTcpHealthCheck().GetReceive()) != 0 {
		t.Error("TCP health check must be connect-only")
	}
	if got := hcs[0].GetInterval().AsDuration(); got != 15*time.Second {
		t.Errorf("health check interval = %v, want 15s", got)
	}

	opts := &upstreamhttpv3.HttpProtocolOptions{}
	if err := c.GetTypedExtensionProtocolOptions()["envoy.extensions.upstreams.http.v3.HttpProtocolOptions"].UnmarshalTo(opts); err != nil {
		t.Fatal(err)
	}
	if opts.GetExplicitHttpConfig().GetHttpProtocolOptions() == nil {
		t.Error("upstream must be explicit HTTP/1.1")
	}
	if got := opts.GetCommonHttpProtocolOptions().GetIdleTimeout().AsDuration(); got != 5*time.Minute {
		t.Errorf("upstream idle timeout = %v, want 5m", got)
	}
	if c.GetTransportSocket() != nil {
		t.Error("plaintext target has a transport socket")
	}
}

func TestClusterOptions(t *testing.T) {
	spec := testTarget()
	spec.PerHostLimit = 7
	spec.HealthCheck = HealthCheckSpec{Interval: 30 * time.Second}
	_, c, _, err := spec.Resources()
	if err != nil {
		t.Fatal(err)
	}
	ph := c.GetCircuitBreakers().GetPerHostThresholds()
	if len(ph) != 1 || ph[0].GetMaxConnections().GetValue() != 7 {
		t.Errorf("per-host thresholds = %v, want max_connections 7", ph)
	}
	if got := c.GetHealthChecks()[0].GetInterval().AsDuration(); got != 30*time.Second {
		t.Errorf("health check interval = %v, want 30s", got)
	}

	spec.HealthCheck = HealthCheckSpec{Disabled: true}
	_, c, _, err = spec.Resources()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.GetHealthChecks()) != 0 {
		t.Error("health check rendered although disabled")
	}
}

func TestClusterTLS(t *testing.T) {
	tlsContext := func(t *testing.T, s *TLSSpec) *tlsv3.UpstreamTlsContext {
		t.Helper()
		spec := testTarget()
		spec.TLS = s
		_, c, _, err := spec.Resources()
		if err != nil {
			t.Fatal(err)
		}
		if c.GetTransportSocket().GetName() != "envoy.transport_sockets.tls" {
			t.Fatalf("transport socket = %q", c.GetTransportSocket().GetName())
		}
		ctx := &tlsv3.UpstreamTlsContext{}
		if err := c.GetTransportSocket().GetTypedConfig().UnmarshalTo(ctx); err != nil {
			t.Fatal(err)
		}
		return ctx
	}

	t.Run("system trust store", func(t *testing.T) {
		ctx := tlsContext(t, &TLSSpec{ServerName: "s3.example.com"})
		vc := ctx.GetCommonTlsContext().GetValidationContext()
		if ctx.GetSni() != "s3.example.com" {
			t.Errorf("sni = %q", ctx.GetSni())
		}
		if vc.GetSystemRootCerts() == nil || vc.GetTrustedCa() != nil {
			t.Error("with no CA file, the host's trust store must be used")
		}
		sans := vc.GetMatchTypedSubjectAltNames()
		if len(sans) != 1 || sans[0].GetMatcher().GetExact() != "s3.example.com" {
			t.Errorf("certificate name is not pinned to the server name: %v", sans)
		}
	})
	t.Run("CA file", func(t *testing.T) {
		vc := tlsContext(t, &TLSSpec{ServerName: "s3.example.com", CAFile: "/etc/deltav/ca.pem"}).GetCommonTlsContext().GetValidationContext()
		if vc.GetTrustedCa().GetFilename() != "/etc/deltav/ca.pem" || vc.GetSystemRootCerts() != nil {
			t.Errorf("validation context = %v", vc)
		}
	})
	t.Run("insecure", func(t *testing.T) {
		ctx := tlsContext(t, &TLSSpec{ServerName: "s3.example.com", Insecure: true})
		if ctx.GetCommonTlsContext().GetValidationContext() != nil {
			t.Error("insecure target still has a validation context")
		}
	})
	t.Run("server name required", func(t *testing.T) {
		spec := testTarget()
		spec.TLS = &TLSSpec{}
		if _, _, _, err := spec.Resources(); err == nil || !strings.Contains(err.Error(), "server name") {
			t.Errorf("want a server-name error, got %v", err)
		}
	})
}

func TestLoadAssignment(t *testing.T) {
	_, _, e, err := testTarget().Resources()
	if err != nil {
		t.Fatal(err)
	}
	if e.GetClusterName() != "s3" || len(e.GetEndpoints()) != 1 {
		t.Fatalf("assignment = %v", e)
	}
	lbs := e.GetEndpoints()[0].GetLbEndpoints()
	if len(lbs) != 2 {
		t.Fatalf("want 2 endpoints, got %d", len(lbs))
	}
	a := lbs[0].GetEndpoint().GetAddress().GetSocketAddress()
	if a.GetAddress() != "192.0.2.11" || a.GetPortValue() != 80 || a.GetProtocol() != corev3.SocketAddress_TCP {
		t.Errorf("endpoint 0 = %v", a)
	}
	if got := lbs[0].GetMetadata().GetFilterMetadata()["deltav"].GetFields()["cnode"].GetStringValue(); got != "cnode-1" {
		t.Errorf("endpoint 0 cnode metadata = %q", got)
	}
	if lbs[1].GetMetadata() != nil {
		t.Error("endpoint without a CNode carries metadata")
	}
}

func TestResourcesRejectsBadInput(t *testing.T) {
	for name, spec := range map[string]TargetSpec{
		"no name":        {ListenPort: 9000},
		"no listen port": {Name: "s3"},
		"port too large": {Name: "s3", ListenPort: 70000},
		"hostname":       {Name: "s3", ListenPort: 9000, Endpoints: []Endpoint{{IP: "s3.example.com", Port: 80}}},
		"endpoint port":  {Name: "s3", ListenPort: 9000, Endpoints: []Endpoint{{IP: "192.0.2.1", Port: 70000}}},
	} {
		if _, _, _, err := spec.Resources(); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
