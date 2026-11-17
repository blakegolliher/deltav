package xds

import (
	"fmt"
	"net"
	"strconv"
	"time"

	bootstrapv3 "github.com/envoyproxy/go-control-plane/envoy/config/bootstrap/v3"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	upstreamhttpv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/upstreams/http/v3"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
)

// DefaultNodeCluster is the xDS node cluster name every client Envoy reports.
const DefaultNodeCluster = "deltav"

// BootstrapOptions describes the one bootstrap file shared by the whole
// fleet. Nothing in it is host-specific: the node ID comes from Envoy's
// --service-node flag (DESIGN.md, amendment 11).
type BootstrapOptions struct {
	XDSAddress   string // host:port of the daemon's gRPC listener, as client hosts reach it
	NodeCluster  string // default DefaultNodeCluster
	AdminAddress string // ip:port for Envoy's admin interface; "" leaves it off
}

// Bootstrap builds the Envoy bootstrap: one static cluster pointing at the
// daemon, and ADS over it for clusters, endpoints and listeners.
func Bootstrap(o BootstrapOptions) (*bootstrapv3.Bootstrap, error) {
	host, portStr, err := net.SplitHostPort(o.XDSAddress)
	if err != nil {
		return nil, fmt.Errorf("xDS address %q: want host:port: %w", o.XDSAddress, err)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil || port == 0 || host == "" {
		return nil, fmt.Errorf("xDS address %q: want host:port with a port 1-65535", o.XDSAddress)
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		return nil, fmt.Errorf("xDS address %q is a wildcard: give the address client hosts dial", o.XDSAddress)
	}
	nodeCluster := o.NodeCluster
	if nodeCluster == "" {
		nodeCluster = DefaultNodeCluster
	}

	// gRPC needs HTTP/2. Keepalive pings let Envoy notice a dead daemon and
	// reconnect without waiting for TCP to time out.
	h2, err := anypb.New(&upstreamhttpv3.HttpProtocolOptions{
		UpstreamProtocolOptions: &upstreamhttpv3.HttpProtocolOptions_ExplicitHttpConfig_{
			ExplicitHttpConfig: &upstreamhttpv3.HttpProtocolOptions_ExplicitHttpConfig{
				ProtocolConfig: &upstreamhttpv3.HttpProtocolOptions_ExplicitHttpConfig_Http2ProtocolOptions{
					Http2ProtocolOptions: &corev3.Http2ProtocolOptions{
						ConnectionKeepalive: &corev3.KeepaliveSettings{
							Interval: durationpb.New(30 * time.Second),
							Timeout:  durationpb.New(10 * time.Second),
						},
					},
				},
			},
		},
	})
	if err != nil {
		return nil, err
	}
	control := &clusterv3.Cluster{
		Name:           ControlCluster,
		ConnectTimeout: durationpb.New(5 * time.Second),
		LoadAssignment: &endpointv3.ClusterLoadAssignment{
			ClusterName: ControlCluster,
			Endpoints: []*endpointv3.LocalityLbEndpoints{{LbEndpoints: []*endpointv3.LbEndpoint{{
				HostIdentifier: &endpointv3.LbEndpoint_Endpoint{
					Endpoint: &endpointv3.Endpoint{Address: socketAddress(host, uint32(port))},
				},
			}}}},
		},
		TypedExtensionProtocolOptions: map[string]*anypb.Any{
			"envoy.extensions.upstreams.http.v3.HttpProtocolOptions": h2,
		},
	}
	if net.ParseIP(host) != nil {
		control.ClusterDiscoveryType = &clusterv3.Cluster_Type{Type: clusterv3.Cluster_STATIC}
	} else {
		control.ClusterDiscoveryType = &clusterv3.Cluster_Type{Type: clusterv3.Cluster_STRICT_DNS}
		control.DnsLookupFamily = clusterv3.Cluster_V4_PREFERRED
	}

	ads := &corev3.ConfigSource{
		ResourceApiVersion:    corev3.ApiVersion_V3,
		ConfigSourceSpecifier: &corev3.ConfigSource_Ads{Ads: &corev3.AggregatedConfigSource{}},
	}
	b := &bootstrapv3.Bootstrap{
		Node:            &corev3.Node{Cluster: nodeCluster},
		StaticResources: &bootstrapv3.Bootstrap_StaticResources{Clusters: []*clusterv3.Cluster{control}},
		// Load reporting: Envoy opens a stream to the daemon and reports
		// per-endpoint request counts at the interval the daemon asks for.
		ClusterManager: &bootstrapv3.ClusterManager{
			LoadStatsConfig: &corev3.ApiConfigSource{
				ApiType:             corev3.ApiConfigSource_GRPC,
				TransportApiVersion: corev3.ApiVersion_V3,
				GrpcServices:        []*corev3.GrpcService{controlGRPC()},
			},
		},
		DynamicResources: &bootstrapv3.Bootstrap_DynamicResources{
			AdsConfig: &corev3.ApiConfigSource{
				ApiType:             corev3.ApiConfigSource_GRPC,
				TransportApiVersion: corev3.ApiVersion_V3,
				GrpcServices:        []*corev3.GrpcService{controlGRPC()},
				// The node lists every compiled-in extension; repeating it on
				// each ACK is wasted bytes at fleet scale. Once per stream.
				SetNodeOnFirstMessageOnly: true,
			},
			CdsConfig: ads,
			LdsConfig: ads,
		},
	}
	if o.AdminAddress != "" {
		ah, ap, err := net.SplitHostPort(o.AdminAddress)
		if err != nil {
			return nil, fmt.Errorf("admin address %q: want ip:port: %w", o.AdminAddress, err)
		}
		aport, err := strconv.ParseUint(ap, 10, 16)
		if err != nil || net.ParseIP(ah) == nil {
			return nil, fmt.Errorf("admin address %q: want ip:port", o.AdminAddress)
		}
		b.Admin = &bootstrapv3.Admin{Address: socketAddress(ah, uint32(aport))}
	}
	if err := b.ValidateAll(); err != nil {
		return nil, fmt.Errorf("bootstrap: %w", err)
	}
	return b, nil
}

// BootstrapJSON renders the bootstrap as the JSON file Envoy reads with -c.
func BootstrapJSON(o BootstrapOptions) ([]byte, error) {
	b, err := Bootstrap(o)
	if err != nil {
		return nil, err
	}
	out, err := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(b)
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}
