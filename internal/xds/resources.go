package xds

import (
	"fmt"
	"net"
	"time"

	accesslogv3 "github.com/envoyproxy/go-control-plane/envoy/config/accesslog/v3"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	grpcalsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/access_loggers/grpc/v3"
	routerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	upstreamhttpv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/upstreams/http/v3"
	matcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// ControlCluster is the static bootstrap cluster that points back at the
// daemon. Envoy uses it for ADS, access logs and load reports.
const ControlCluster = "deltav_control"

// ListenAddress is where every target's listener binds on a client host.
const ListenAddress = "127.0.0.1"

// TargetSpec is everything needed to render one target's Envoy resources.
// It is computed from daemon state; xds itself holds no policy.
type TargetSpec struct {
	Name         string
	ListenPort   uint32
	Endpoints    []Endpoint
	TLS          *TLSSpec // nil: plaintext upstream
	HealthCheck  HealthCheckSpec
	AccessLog    bool   // stream per-request entries to the daemon (ALS)
	PerHostLimit uint32 // enforce mode: per_host_thresholds.max_connections; 0 = none
}

// Endpoint is one VIP. CNode is carried as metadata for debugging only;
// the daemon joins telemetry to CNodes on its side.
type Endpoint struct {
	IP    string
	Port  uint32
	CNode string
}

// TLSSpec configures upstream TLS. Verification is on unless Insecure.
type TLSSpec struct {
	ServerName string // SNI and the name the certificate must match
	CAFile     string // CA bundle path on the client host; "" uses the host's trust store
	Insecure   bool   // lab opt-out: no certificate verification
}

// HealthCheckSpec is the active health check; zero values take defaults.
type HealthCheckSpec struct {
	Disabled bool
	Interval time.Duration // default 15s (see DESIGN.md, amendment 5)
}

// Defaults that DESIGN.md explains; change them there first.
const (
	circuitBreakerMax   = 65536 // Envoy's 1024 default would cap busy clients (amendment 2)
	maxEjectionPercent  = 34    // at most a third of the pool (amendment 3)
	upstreamIdleTimeout = 5 * time.Minute
	downstreamIdle      = time.Hour
	streamIdleTimeout   = time.Hour
	connectTimeout      = 2 * time.Second
	defaultHCInterval   = 15 * time.Second
)

// Resources renders the listener, cluster and endpoint assignment for t.
func (t TargetSpec) Resources() (*listenerv3.Listener, *clusterv3.Cluster, *endpointv3.ClusterLoadAssignment, error) {
	if t.Name == "" || t.ListenPort == 0 || t.ListenPort > 65535 {
		return nil, nil, nil, fmt.Errorf("target %q: name and a listen port 1-65535 are required", t.Name)
	}
	l, err := t.listener()
	if err != nil {
		return nil, nil, nil, err
	}
	c, err := t.cluster()
	if err != nil {
		return nil, nil, nil, err
	}
	e, err := t.loadAssignment()
	if err != nil {
		return nil, nil, nil, err
	}
	for _, m := range []interface{ ValidateAll() error }{l, c, e} {
		if err := m.ValidateAll(); err != nil {
			return nil, nil, nil, fmt.Errorf("target %q: %w", t.Name, err)
		}
	}
	return l, c, e, nil
}

func (t TargetSpec) listener() (*listenerv3.Listener, error) {
	route := &routev3.RouteConfiguration{
		Name: t.Name,
		// An inline route table is validated against known clusters by
		// default, so a listener that reaches Envoy ahead of its cluster
		// would be rejected. Pushes are ordered clusters-first, but an ACK
		// in flight during a push can reorder them on one stream; accept the
		// listener and let it answer 503 for the moment the cluster is late.
		ValidateClusters: wrapperspb.Bool(false),
		VirtualHosts: []*routev3.VirtualHost{{
			Name:    t.Name,
			Domains: []string{"*"},
			Routes: []*routev3.Route{{
				Match: &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}},
				Action: &routev3.Route_Route{Route: &routev3.RouteAction{
					ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: t.Name},
					// 0 disables Envoy's 15 s default so multi-GB transfers
					// are not cut off. No host rewrite: SigV4 signs Host.
					// No retry policy: SDKs retry; stacked retries amplify.
					Timeout: durationpb.New(0),
				}},
			}},
		}},
	}
	router, err := anypb.New(&routerv3.Router{SuppressEnvoyHeaders: true})
	if err != nil {
		return nil, err
	}
	hcm := &hcmv3.HttpConnectionManager{
		StatPrefix:     t.Name,
		CodecType:      hcmv3.HttpConnectionManager_AUTO,
		RouteSpecifier: &hcmv3.HttpConnectionManager_RouteConfig{RouteConfig: route},
		HttpFilters: []*hcmv3.HttpFilter{{
			Name:       "envoy.filters.http.router",
			ConfigType: &hcmv3.HttpFilter_TypedConfig{TypedConfig: router},
		}},
		// S3 SDKs send "Expect: 100-continue"; without this Envoy strips
		// the header (breaking a signature that covers it) and answers
		// 100 itself, so the client streams a body the server may reject.
		Proxy_100Continue: true,
		// Object keys may contain "//", "%2F", "/./": pass paths through
		// byte for byte. Set explicitly because defaults are runtime-overridable.
		NormalizePath:                wrapperspb.Bool(false),
		MergeSlashes:                 false,
		PathWithEscapedSlashesAction: hcmv3.HttpConnectionManager_KEEP_UNCHANGED,
		// Never mint or replace x-request-id: a client may have signed it.
		GenerateRequestId:          wrapperspb.Bool(false),
		PreserveExternalRequestId:  true,
		ServerHeaderTransformation: hcmv3.HttpConnectionManager_PASS_THROUGH,
		StreamIdleTimeout:          durationpb.New(streamIdleTimeout),
		CommonHttpProtocolOptions:  &corev3.HttpProtocolOptions{IdleTimeout: durationpb.New(downstreamIdle)},
	}
	if t.AccessLog {
		al, err := accessLog(t.Name)
		if err != nil {
			return nil, err
		}
		hcm.AccessLog = []*accesslogv3.AccessLog{al}
	}
	hcmAny, err := anypb.New(hcm)
	if err != nil {
		return nil, err
	}
	return &listenerv3.Listener{
		Name:    t.Name,
		Address: socketAddress(ListenAddress, t.ListenPort),
		FilterChains: []*listenerv3.FilterChain{{
			Filters: []*listenerv3.Filter{{
				Name:       "envoy.filters.network.http_connection_manager",
				ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: hcmAny},
			}},
		}},
	}, nil
}

func accessLog(logName string) (*accesslogv3.AccessLog, error) {
	cfg, err := anypb.New(&grpcalsv3.HttpGrpcAccessLogConfig{
		CommonConfig: &grpcalsv3.CommonGrpcAccessLogConfig{
			LogName:             logName,
			GrpcService:         controlGRPC(),
			TransportApiVersion: corev3.ApiVersion_V3,
			BufferFlushInterval: durationpb.New(time.Second),
			BufferSizeBytes:     wrapperspb.UInt32(16 * 1024),
		},
	})
	if err != nil {
		return nil, err
	}
	return &accesslogv3.AccessLog{
		Name:       "envoy.access_loggers.http_grpc",
		ConfigType: &accesslogv3.AccessLog_TypedConfig{TypedConfig: cfg},
	}, nil
}

func controlGRPC() *corev3.GrpcService {
	return &corev3.GrpcService{TargetSpecifier: &corev3.GrpcService_EnvoyGrpc_{
		EnvoyGrpc: &corev3.GrpcService_EnvoyGrpc{ClusterName: ControlCluster},
	}}
}

func (t TargetSpec) cluster() (*clusterv3.Cluster, error) {
	thresholds := &clusterv3.CircuitBreakers{
		Thresholds: []*clusterv3.CircuitBreakers_Thresholds{{
			Priority:           corev3.RoutingPriority_DEFAULT,
			MaxConnections:     wrapperspb.UInt32(circuitBreakerMax),
			MaxPendingRequests: wrapperspb.UInt32(circuitBreakerMax),
			MaxRequests:        wrapperspb.UInt32(circuitBreakerMax),
		}},
	}
	if t.PerHostLimit > 0 {
		thresholds.PerHostThresholds = []*clusterv3.CircuitBreakers_Thresholds{{
			Priority:       corev3.RoutingPriority_DEFAULT,
			MaxConnections: wrapperspb.UInt32(t.PerHostLimit),
		}}
	}
	proto1, err := anypb.New(&upstreamhttpv3.HttpProtocolOptions{
		CommonHttpProtocolOptions: &corev3.HttpProtocolOptions{IdleTimeout: durationpb.New(upstreamIdleTimeout)},
		UpstreamProtocolOptions: &upstreamhttpv3.HttpProtocolOptions_ExplicitHttpConfig_{
			ExplicitHttpConfig: &upstreamhttpv3.HttpProtocolOptions_ExplicitHttpConfig{
				ProtocolConfig: &upstreamhttpv3.HttpProtocolOptions_ExplicitHttpConfig_HttpProtocolOptions{
					HttpProtocolOptions: &corev3.Http1ProtocolOptions{},
				},
			},
		},
	})
	if err != nil {
		return nil, err
	}
	c := &clusterv3.Cluster{
		Name:                 t.Name,
		ClusterDiscoveryType: &clusterv3.Cluster_Type{Type: clusterv3.Cluster_EDS},
		EdsClusterConfig: &clusterv3.Cluster_EdsClusterConfig{EdsConfig: &corev3.ConfigSource{
			ResourceApiVersion:    corev3.ApiVersion_V3,
			ConfigSourceSpecifier: &corev3.ConfigSource_Ads{Ads: &corev3.AggregatedConfigSource{}},
		}},
		ConnectTimeout:  durationpb.New(connectTimeout),
		LbPolicy:        clusterv3.Cluster_LEAST_REQUEST,
		CircuitBreakers: thresholds,
		OutlierDetection: &clusterv3.OutlierDetection{
			Consecutive_5Xx:                        wrapperspb.UInt32(5),
			ConsecutiveGatewayFailure:              wrapperspb.UInt32(5),
			EnforcingConsecutiveGatewayFailure:     wrapperspb.UInt32(100),
			SplitExternalLocalOriginErrors:         true,
			ConsecutiveLocalOriginFailure:          wrapperspb.UInt32(3),
			EnforcingConsecutiveLocalOriginFailure: wrapperspb.UInt32(100),
			// Success-rate ejection is on by default; with S3 503 SlowDown as
			// backpressure it would eject healthy-but-busy VIPs. Off.
			EnforcingSuccessRate: wrapperspb.UInt32(0),
			Interval:             durationpb.New(10 * time.Second),
			BaseEjectionTime:     durationpb.New(15 * time.Second),
			MaxEjectionTime:      durationpb.New(2 * time.Minute),
			MaxEjectionPercent:   wrapperspb.UInt32(maxEjectionPercent),
			AlwaysEjectOneHost:   wrapperspb.Bool(true),
		},
		// Without this, an endpoint removed from EDS (a drained CNode's VIP)
		// keeps taking traffic until it fails an active health check.
		IgnoreHealthOnHostRemoval: true,
		TypedExtensionProtocolOptions: map[string]*anypb.Any{
			"envoy.extensions.upstreams.http.v3.HttpProtocolOptions": proto1,
		},
	}
	if !t.HealthCheck.Disabled {
		interval := t.HealthCheck.Interval
		if interval <= 0 {
			interval = defaultHCInterval
		}
		c.HealthChecks = []*corev3.HealthCheck{{
			Timeout:            durationpb.New(2 * time.Second),
			Interval:           durationpb.New(interval),
			IntervalJitter:     durationpb.New(interval / 5),
			UnhealthyThreshold: wrapperspb.UInt32(3),
			HealthyThreshold:   wrapperspb.UInt32(1),
			NoTrafficInterval:  durationpb.New(60 * time.Second),
			// Empty send/receive: a TCP connect check. No S3 health URL is assumed.
			HealthChecker: &corev3.HealthCheck_TcpHealthCheck_{TcpHealthCheck: &corev3.HealthCheck_TcpHealthCheck{}},
		}}
	}
	if t.TLS != nil {
		ts, err := t.TLS.transportSocket()
		if err != nil {
			return nil, err
		}
		c.TransportSocket = ts
	}
	return c, nil
}

func (s *TLSSpec) transportSocket() (*corev3.TransportSocket, error) {
	if s.ServerName == "" {
		return nil, fmt.Errorf("upstream TLS needs a server name (SNI); auto SNI would send 127.0.0.1")
	}
	common := &tlsv3.CommonTlsContext{}
	if !s.Insecure {
		vc := &tlsv3.CertificateValidationContext{
			MatchTypedSubjectAltNames: []*tlsv3.SubjectAltNameMatcher{{
				SanType: tlsv3.SubjectAltNameMatcher_DNS,
				Matcher: &matcherv3.StringMatcher{MatchPattern: &matcherv3.StringMatcher_Exact{Exact: s.ServerName}},
			}},
		}
		if s.CAFile != "" {
			vc.TrustedCa = &corev3.DataSource{Specifier: &corev3.DataSource_Filename{Filename: s.CAFile}}
		} else {
			// No one bundle path is right for every distribution (Debian's
			// ca-certificates.crt does not exist on RHEL), so let Envoy use
			// the host's default trust store.
			vc.SystemRootCerts = &tlsv3.CertificateValidationContext_SystemRootCerts{}
		}
		common.ValidationContextType = &tlsv3.CommonTlsContext_ValidationContext{ValidationContext: vc}
	}
	cfg, err := anypb.New(&tlsv3.UpstreamTlsContext{Sni: s.ServerName, CommonTlsContext: common})
	if err != nil {
		return nil, err
	}
	return &corev3.TransportSocket{
		Name:       "envoy.transport_sockets.tls",
		ConfigType: &corev3.TransportSocket_TypedConfig{TypedConfig: cfg},
	}, nil
}

func (t TargetSpec) loadAssignment() (*endpointv3.ClusterLoadAssignment, error) {
	lbs := make([]*endpointv3.LbEndpoint, 0, len(t.Endpoints))
	for _, ep := range t.Endpoints {
		if net.ParseIP(ep.IP) == nil {
			return nil, fmt.Errorf("target %q: endpoint %q is not an IP address", t.Name, ep.IP)
		}
		lb := &endpointv3.LbEndpoint{HostIdentifier: &endpointv3.LbEndpoint_Endpoint{
			Endpoint: &endpointv3.Endpoint{Address: socketAddress(ep.IP, ep.Port)},
		}}
		if ep.CNode != "" {
			md, err := structpb.NewStruct(map[string]any{"cnode": ep.CNode})
			if err != nil {
				return nil, err
			}
			lb.Metadata = &corev3.Metadata{FilterMetadata: map[string]*structpb.Struct{"deltav": md}}
		}
		lbs = append(lbs, lb)
	}
	return &endpointv3.ClusterLoadAssignment{
		ClusterName: t.Name,
		Endpoints:   []*endpointv3.LocalityLbEndpoints{{LbEndpoints: lbs}},
	}, nil
}

func socketAddress(ip string, port uint32) *corev3.Address {
	return &corev3.Address{Address: &corev3.Address_SocketAddress{SocketAddress: &corev3.SocketAddress{
		Protocol:      corev3.SocketAddress_TCP,
		Address:       ip,
		PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: port},
	}}}
}
