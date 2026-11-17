package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	accesslogdatav3 "github.com/envoyproxy/go-control-plane/envoy/data/accesslog/v3"
	accesslogv3 "github.com/envoyproxy/go-control-plane/envoy/service/accesslog/v3"
	discoverygrpc "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	loadstatsv3 "github.com/envoyproxy/go-control-plane/envoy/service/load_stats/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"deltav/internal/fleet"
	"deltav/internal/metrics"
	"deltav/internal/xds"
)

// samples returns n successful requests to one endpoint of target s3.
func samples(n int) []metrics.Sample {
	out := make([]metrics.Sample, n)
	for i := range out {
		out[i] = metrics.Sample{Target: "s3", Endpoint: "192.0.2.11:80", Status: 200, Duration: 5 * time.Millisecond}
	}
	return out
}

func dial(t *testing.T, s *Server) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(s.GRPCAddr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func addr(ip string, port uint32) *corev3.Address {
	return &corev3.Address{Address: &corev3.Address_SocketAddress{SocketAddress: &corev3.SocketAddress{
		Address: ip, PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: port},
	}}}
}

// entry builds an access log entry as Envoy's HTTP gRPC access logger does.
func entry(ip string, status uint32, d time.Duration, in, out uint64) *accesslogdatav3.HTTPAccessLogEntry {
	e := &accesslogdatav3.HTTPAccessLogEntry{
		CommonProperties: &accesslogdatav3.AccessLogCommon{UpstreamCluster: "s3", Duration: durationpb.New(d)},
		Request:          &accesslogdatav3.HTTPRequestProperties{RequestBodyBytes: in},
		Response:         &accesslogdatav3.HTTPResponseProperties{ResponseBodyBytes: out},
	}
	if ip != "" {
		e.CommonProperties.UpstreamRemoteAddress = addr(ip, 80)
	}
	if status != 0 {
		e.Response.ResponseCode = wrapperspb.UInt32(status)
	}
	return e
}

func logs(node string, entries ...*accesslogdatav3.HTTPAccessLogEntry) *accesslogv3.StreamAccessLogsMessage {
	m := &accesslogv3.StreamAccessLogsMessage{LogEntries: &accesslogv3.StreamAccessLogsMessage_HttpLogs{
		HttpLogs: &accesslogv3.StreamAccessLogsMessage_HTTPAccessLogEntries{LogEntry: entries},
	}}
	if node != "" {
		m.Identifier = &accesslogv3.StreamAccessLogsMessage_Identifier{Node: &corev3.Node{Id: node}, LogName: "s3"}
	}
	return m
}

func getJSON[T any](t *testing.T, s *Server, path string) T {
	t.Helper()
	code, body := get(t, "http://"+s.HTTPAddr()+path, "")
	var out T
	if code != 200 {
		t.Fatalf("GET %s: %d %s", path, code, body)
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("GET %s: %v\n%s", path, err, body)
	}
	return out
}

// eventually polls until ok returns "" (success) or times out with the last
// reason. Telemetry is applied on the stream's goroutine, after the send.
func eventually(t *testing.T, ok func() string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		why := ok()
		if why == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(why)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func findEndpoint(r endpointsResponse, ep string) *endpointStats {
	for i := range r.Endpoints {
		if r.Endpoints[i].Endpoint == ep {
			return &r.Endpoints[i]
		}
	}
	return nil
}

// register makes an Envoy known to the fleet by opening an ADS stream, and
// returns once the daemon has seen it accept the config.
func register(t *testing.T, s *Server, nodeID string) *adsClient {
	t.Helper()
	c := dialADS(t, s, nodeID)
	c.send(&discoverygrpc.DiscoveryRequest{TypeUrl: resource.ClusterType})
	c.ack(c.recv())
	waitMember(t, s, nodeID, "acked", func(m fleet.Member) bool { return m.ConfigState == fleet.StateAcked })
	return c
}

func telemetryServer(t *testing.T) *Server {
	return start(t, Config{
		GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0", LoadReportInterval: time.Second,
		StaticTargets: []xds.TargetSpec{{Name: "s3", ListenPort: 9000, Endpoints: []xds.Endpoint{
			{IP: "192.0.2.11", Port: 80, CNode: "cnode-1"}, {IP: "192.0.2.12", Port: 80, CNode: "cnode-2"}, {IP: "192.0.2.13", Port: 80},
		}}},
	})
}

func TestAccessLogIngestAndAPI(t *testing.T) {
	s := telemetryServer(t)
	register(t, s, "host-a")
	register(t, s, "host-b")
	conn := dial(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Static targets are told to stream access logs.
	if ts := s.XDS().Targets(); len(ts) != 1 || !ts[0].AccessLog {
		t.Fatalf("static target does not have access logging on: %+v", ts)
	}

	a, err := accesslogv3.NewAccessLogServiceClient(conn).StreamAccessLogs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// host-a: 60 fast 200s and 20 slow 200s to .11; 10 403s and 10 503s to
	// .12; one request Envoy refused itself, which reached no endpoint.
	var batch []*accesslogdatav3.HTTPAccessLogEntry
	for i := 0; i < 60; i++ {
		batch = append(batch, entry("192.0.2.11", 200, 10*time.Millisecond, 1000, 0))
	}
	for i := 0; i < 20; i++ {
		batch = append(batch, entry("192.0.2.11", 200, 400*time.Millisecond, 0, 5000))
	}
	if err := a.Send(logs("host-a", batch...)); err != nil {
		t.Fatal(err)
	}
	// Later messages on a stream carry no identifier.
	batch = nil
	for i := 0; i < 10; i++ {
		batch = append(batch, entry("192.0.2.12", 403, time.Millisecond, 0, 200), entry("192.0.2.12", 503, time.Millisecond, 0, 200))
	}
	overflow := entry("", 503, 0, 0, 80)
	overflow.CommonProperties.ResponseFlags = &accesslogdatav3.ResponseFlags{UpstreamOverflow: true}
	// A periodic record of a request still in progress must not be counted.
	running := entry("192.0.2.11", 0, time.Second, 0, 0)
	running.CommonProperties.IntermediateLogEntry = true
	if err := a.Send(logs("", append(batch, overflow, running)...)); err != nil {
		t.Fatal(err)
	}
	// host-b on its own stream: 5 requests to .11.
	b, err := accesslogv3.NewAccessLogServiceClient(conn).StreamAccessLogs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Send(logs("host-b", entry("192.0.2.11", 200, 10*time.Millisecond, 10, 10), entry("192.0.2.11", 200, 10*time.Millisecond, 10, 10),
		entry("192.0.2.11", 200, 10*time.Millisecond, 10, 10), entry("192.0.2.11", 200, 10*time.Millisecond, 10, 10), entry("192.0.2.11", 200, 10*time.Millisecond, 10, 10))); err != nil {
		t.Fatal(err)
	}
	if _, err := b.CloseAndRecv(); err != nil {
		t.Fatalf("closing an access log stream: %v", err)
	}

	var eps endpointsResponse
	eventually(t, func() string {
		eps = getJSON[endpointsResponse](t, s, "/api/v1/endpoints")
		var n uint64
		for _, e := range eps.Endpoints {
			n += e.Stats.Requests
		}
		if n != 106 {
			return fmt.Sprintf("endpoints API shows %d requests, want 106", n)
		}
		return ""
	})
	if len(eps.Endpoints) != 4 || eps.Window != "1m" {
		t.Fatalf("want 3 configured endpoints and the no-endpoint row, got %+v", eps)
	}

	e11 := findEndpoint(eps, "192.0.2.11:80")
	if e11 == nil || !e11.Configured || e11.CNode != "cnode-1" || e11.Target != "s3" {
		t.Fatalf("endpoint .11: %+v", e11)
	}
	st := e11.Stats
	if st.Requests != 85 || st.Status.S2xx != 85 || st.BytesIn != 60050 || st.BytesOut != 100050 {
		t.Errorf(".11 counts: %+v", st)
	}
	if st.RequestRate <= 0 || st.BytesInRate <= 0 || st.WindowSeconds <= 0 || st.WindowSeconds > 60 {
		t.Errorf(".11 rates: %+v", st)
	}
	// 65 of 85 at 10 ms and 20 at 400 ms: p50 is fast, p90 and p99 slow.
	if st.LatencyMS.P50 < 7 || st.LatencyMS.P50 > 13 || st.LatencyMS.P90 < 300 || st.LatencyMS.P99 > 520 {
		t.Errorf(".11 latency: %+v", st.LatencyMS)
	}

	e12 := findEndpoint(eps, "192.0.2.12:80")
	if e12 == nil || e12.Stats.Requests != 20 || e12.Stats.Status.S403 != 10 || e12.Stats.Status.S503 != 10 ||
		e12.Stats.Status.S4xx != 10 || e12.Stats.Status.S5xx != 10 || e12.Stats.Rate403 <= 0 || e12.Stats.Rate503 <= 0 {
		t.Errorf("endpoint .12: %+v", e12)
	}
	if e13 := findEndpoint(eps, "192.0.2.13:80"); e13 == nil || !e13.Configured || e13.Stats.Requests != 0 || e13.Stats.WindowSeconds <= 0 {
		t.Errorf("idle configured endpoint .13: %+v", e13)
	}
	if none := findEndpoint(eps, ""); none == nil || none.Configured || none.Stats.Requests != 1 || none.Stats.Overflow != 1 {
		t.Errorf("no-endpoint row: %+v", none)
	}
	if got := getJSON[endpointsResponse](t, s, "/api/v1/endpoints?target=other&window=1h"); len(got.Endpoints) != 0 || got.Window != "1h" {
		t.Errorf("?target=other: %+v", got)
	}

	ov := getJSON[overviewResponse](t, s, "/api/v1/overview?window=5m")
	if len(ov.Targets) != 1 || ov.Window != "5m" || ov.ConfigVersion != s.XDS().Version() {
		t.Fatalf("overview: %+v", ov)
	}
	if tg := ov.Targets[0]; tg.Name != "s3" || tg.Endpoints != 3 || tg.ListenPort != 9000 || tg.Stats.Requests != 106 ||
		tg.Stats.Status.S503 != 11 || tg.Stats.Status.S403 != 10 || tg.Stats.Overflow != 1 {
		t.Errorf("overview target: %+v", tg)
	}
	if ov.Fleet != (fleetCounts{Total: 2, Connected: 2, Acked: 2}) {
		t.Errorf("overview fleet: %+v", ov.Fleet)
	}

	// Per-member stats, and the detail view with every window.
	fl := getJSON[fleetResponse](t, s, "/api/v1/fleet?sort=request_rate&order=desc")
	if len(fl.Members) != 2 || fl.Members[0].NodeID != "host-a" || fl.Members[0].Stats.Requests != 101 || fl.Members[1].Stats.Requests != 5 {
		t.Errorf("fleet by request rate: %+v", fl.Members)
	}
	detail := getJSON[memberDetail](t, s, "/api/v1/fleet/host-b")
	if detail.NodeID != "host-b" || len(detail.Windows) != 3 || detail.Windows["1m"].Requests != 5 || detail.Windows["1h"].Requests != 5 {
		t.Errorf("fleet detail: %+v", detail)
	}
	if code, body := get(t, "http://"+s.HTTPAddr()+"/api/v1/fleet/nobody", ""); code != 404 || !strings.Contains(body, `"error"`) {
		t.Errorf("unknown member: %d %s", code, body)
	}
	if code, body := get(t, "http://"+s.HTTPAddr()+"/api/v1/overview?window=2m", ""); code != 400 || !strings.Contains(body, "1m, 5m or 1h") {
		t.Errorf("bad window: %d %s", code, body)
	}

	// Prometheus: cumulative counters and a histogram, per endpoint.
	_, prom := get(t, "http://"+s.HTTPAddr()+"/metrics", "")
	for _, want := range []string{
		`deltav_endpoint_requests_total{class="2xx",cnode="cnode-1",endpoint="192.0.2.11:80",target="s3"} 85`,
		`deltav_endpoint_requests_total{class="4xx",cnode="cnode-2",endpoint="192.0.2.12:80",target="s3"} 10`,
		`deltav_endpoint_responses_total{cnode="cnode-2",code="403",endpoint="192.0.2.12:80",target="s3"} 10`,
		`deltav_endpoint_responses_total{cnode="cnode-2",code="503",endpoint="192.0.2.12:80",target="s3"} 10`,
		`deltav_endpoint_overflow_total{cnode="",endpoint="",target="s3"} 1`,
		`deltav_endpoint_bytes_total{cnode="cnode-1",direction="in",endpoint="192.0.2.11:80",target="s3"} 60050`,
		`deltav_endpoint_request_duration_seconds_bucket{cnode="cnode-1",endpoint="192.0.2.11:80",target="s3",le="0.0128"} 65`,
		`deltav_endpoint_request_duration_seconds_bucket{cnode="cnode-1",endpoint="192.0.2.11:80",target="s3",le="+Inf"} 85`,
		`deltav_endpoint_request_duration_seconds_count{cnode="cnode-1",endpoint="192.0.2.11:80",target="s3"} 85`,
		`deltav_fleet_members{state="connected"} 2`,
		`deltav_fleet_config{state="acked"} 2`,
		`deltav_target_endpoints{target="s3"} 3`,
		`deltav_telemetry_access_log_entries_total 106`,
	} {
		if !strings.Contains(prom, want+"\n") {
			t.Errorf("/metrics lacks: %s", want)
		}
	}
	if strings.Contains(prom, "deltav_member_requests_total") {
		t.Error("/metrics has per-member series although they are off by default")
	}
}

func TestAccessLogStreamMustIdentifyNode(t *testing.T) {
	s := telemetryServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a, err := accesslogv3.NewAccessLogServiceClient(dial(t, s)).StreamAccessLogs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Send(logs("", entry("192.0.2.11", 200, time.Millisecond, 0, 0))); err != nil {
		t.Fatal(err)
	}
	if _, err := a.CloseAndRecv(); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

func loadStats(node string, clusters ...*endpointv3.ClusterStats) *loadstatsv3.LoadStatsRequest {
	req := &loadstatsv3.LoadStatsRequest{ClusterStats: clusters}
	if node != "" {
		req.Node = &corev3.Node{Id: node}
	}
	return req
}

func clusterStats(cluster string, eps ...*endpointv3.UpstreamEndpointStats) *endpointv3.ClusterStats {
	return &endpointv3.ClusterStats{ClusterName: cluster, UpstreamLocalityStats: []*endpointv3.UpstreamLocalityStats{{UpstreamEndpointStats: eps}}}
}

func epStats(ip string, inProgress, issued, success, errs uint64) *endpointv3.UpstreamEndpointStats {
	return &endpointv3.UpstreamEndpointStats{
		Address: addr(ip, 80), TotalRequestsInProgress: inProgress,
		TotalIssuedRequests: issued, TotalSuccessfulRequests: success, TotalErrorRequests: errs,
	}
}

func TestLoadReportIngestAndAPI(t *testing.T) {
	s := telemetryServer(t)
	register(t, s, "host-a")
	conn := dial(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	open := func(node string) loadstatsv3.LoadReportingService_StreamLoadStatsClient {
		t.Helper()
		st, err := loadstatsv3.NewLoadReportingServiceClient(conn).StreamLoadStats(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Send(loadStats(node)); err != nil {
			t.Fatal(err)
		}
		resp, err := st.Recv()
		if err != nil {
			t.Fatal(err)
		}
		// Per-endpoint granularity is the whole point; without it Envoy
		// reports only per-locality totals.
		if !resp.GetReportEndpointGranularity() || !resp.GetSendAllClusters() || resp.GetLoadReportingInterval().AsDuration() != time.Second {
			t.Fatalf("load stats response: %v", resp)
		}
		return st
	}
	inFlight := func(want11, want12, wantTarget uint64) {
		t.Helper()
		eventually(t, func() string {
			eps := getJSON[endpointsResponse](t, s, "/api/v1/endpoints")
			ov := getJSON[overviewResponse](t, s, "/api/v1/overview")
			got11, got12 := findEndpoint(eps, "192.0.2.11:80").InFlight, findEndpoint(eps, "192.0.2.12:80").InFlight
			if got11 != want11 || got12 != want12 || ov.Targets[0].InFlight != wantTarget {
				return fmt.Sprintf("in flight: .11=%d .12=%d target=%d, want %d %d %d", got11, got12, ov.Targets[0].InFlight, want11, want12, wantTarget)
			}
			return ""
		})
	}

	a, b := open("host-a"), open("host-b")
	// The control cluster carries the daemon's own streams, not S3 traffic.
	if err := a.Send(loadStats("", clusterStats("s3", epStats("192.0.2.11", 3, 10, 6, 1), epStats("192.0.2.12", 1, 2, 1, 0)),
		clusterStats(xds.ControlCluster, epStats("192.0.2.200", 2, 2, 0, 0)))); err != nil {
		t.Fatal(err)
	}
	if err := b.Send(loadStats("", clusterStats("s3", epStats("192.0.2.11", 4, 5, 1, 0)))); err != nil {
		t.Fatal(err)
	}
	inFlight(7, 1, 8)

	eps := getJSON[endpointsResponse](t, s, "/api/v1/endpoints")
	if len(eps.Endpoints) != 3 {
		t.Errorf("the control cluster leaked into the endpoints: %+v", eps.Endpoints)
	}
	if got := findEndpoint(eps, "192.0.2.11:80").Load; got.Issued != 15 || got.Success != 7 || got.Errors != 1 {
		t.Errorf(".11 load totals: %+v", got)
	}
	if m := getJSON[memberDetail](t, s, "/api/v1/fleet/host-a"); m.InFlight != 4 {
		t.Errorf("host-a in flight = %d, want 4", m.InFlight)
	}
	_, prom := get(t, "http://"+s.HTTPAddr()+"/metrics", "")
	for _, want := range []string{
		`deltav_endpoint_in_flight_requests{cnode="cnode-1",endpoint="192.0.2.11:80",target="s3"} 7`,
		`deltav_endpoint_load_issued_total{cnode="cnode-1",endpoint="192.0.2.11:80",target="s3"} 15`,
		`deltav_endpoint_load_completed_total{cnode="cnode-1",endpoint="192.0.2.11:80",result="error",target="s3"} 1`,
		`deltav_telemetry_load_reports_total 2`,
	} {
		if !strings.Contains(prom, want+"\n") {
			t.Errorf("/metrics lacks: %s", want)
		}
	}

	// The next report replaces the last: host-a now has nothing in flight
	// to .11 (Envoy leaves idle endpoints out) and two to .12.
	if err := a.Send(loadStats("", clusterStats("s3", epStats("192.0.2.12", 2, 3, 2, 0)))); err != nil {
		t.Fatal(err)
	}
	inFlight(4, 2, 6)
	// An empty report means nothing in flight at all.
	if err := a.Send(loadStats("")); err != nil {
		t.Fatal(err)
	}
	inFlight(4, 0, 4)
	// A stream that ends takes its counts with it.
	if err := b.CloseSend(); err != nil {
		t.Fatal(err)
	}
	inFlight(0, 0, 0)
}

func TestFleetPagingSortAndFilter(t *testing.T) {
	s := start(t, Config{GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0", MetricsPerMember: true})
	// 250 Envoys, fed straight into the registry: this test is about the
	// list API, and the stream path is covered elsewhere.
	for i := 0; i < 250; i++ {
		id := int64(1000 + i)
		s.fleet.StreamOpen(id, fmt.Sprintf("10.1.%d.%d:40000", i/200, i%200))
		s.fleet.Request(id, &discoverygrpc.DiscoveryRequest{TypeUrl: resource.ClusterType, Node: &corev3.Node{Id: fmt.Sprintf("host-%03d", i)}})
		switch {
		case i%50 == 0: // 5 rejected the config
			s.fleet.Response(id, &discoverygrpc.DiscoveryResponse{TypeUrl: resource.ClusterType, VersionInfo: "v1", Nonce: "1"})
			s.fleet.Request(id, &discoverygrpc.DiscoveryRequest{TypeUrl: resource.ClusterType, ResponseNonce: "1", ErrorDetail: status.New(codes.Internal, "boom").Proto()})
		case i%10 == 0: // 20 more have disconnected
			s.fleet.StreamClosed(id)
		}
	}
	// host-007 is the busiest, then host-003.
	s.agg.Record("host-007", samples(30))
	s.agg.Record("host-003", samples(20))
	s.agg.Record("host-200", samples(10))

	ids := func(path string) (fleetResponse, []string) {
		t.Helper()
		r := getJSON[fleetResponse](t, s, path)
		out := make([]string, len(r.Members))
		for i, m := range r.Members {
			out[i] = m.NodeID
		}
		return r, out
	}

	r, got := ids("/api/v1/fleet")
	if r.Total != 250 || r.Connected != 230 || r.Matched != 250 || r.Limit != 100 || r.Offset != 0 || len(got) != 100 || got[0] != "host-000" || got[99] != "host-099" {
		t.Fatalf("default page: total %d connected %d matched %d limit %d, %d members %v…", r.Total, r.Connected, r.Matched, r.Limit, len(got), got[:2])
	}
	if r, got = ids("/api/v1/fleet?limit=20&offset=240"); len(got) != 10 || got[0] != "host-240" || r.Matched != 250 {
		t.Errorf("last page: %v", got)
	}
	if _, got = ids("/api/v1/fleet?offset=900"); len(got) != 0 {
		t.Errorf("page past the end: %v", got)
	}
	if r, _ = ids("/api/v1/fleet?limit=5000"); r.Limit != 1000 || len(r.Members) != 250 {
		t.Errorf("limit above the maximum: limit %d, %d members", r.Limit, len(r.Members))
	}
	if _, got = ids("/api/v1/fleet?order=desc&limit=2"); got[0] != "host-249" || got[1] != "host-248" {
		t.Errorf("node_id descending: %v", got)
	}
	if _, got = ids("/api/v1/fleet?sort=request_rate&order=desc&limit=4"); got[0] != "host-007" || got[1] != "host-003" || got[2] != "host-200" || got[3] != "host-000" {
		t.Errorf("by request rate: %v", got)
	}
	if r, got = ids("/api/v1/fleet?state=nacked"); r.Matched != 5 || got[0] != "host-000" || got[4] != "host-200" || r.Members[0].ConfigError != "cds: boom" {
		t.Errorf("state=nacked: matched %d %v", r.Matched, got)
	}
	if r, _ = ids("/api/v1/fleet?connected=false"); r.Matched != 20 || r.Total != 250 {
		t.Errorf("connected=false: matched %d", r.Matched)
	}
	if r, got = ids("/api/v1/fleet?q=HOST-01&limit=3"); r.Matched != 10 || got[0] != "host-010" {
		t.Errorf("q=HOST-01: matched %d %v", r.Matched, got)
	}
	if r, _ = ids("/api/v1/fleet?q=10.1.1."); r.Matched != 50 {
		t.Errorf("q on address: matched %d, want 50", r.Matched)
	}
	if r, got = ids("/api/v1/fleet?q=host-2&state=nacked&connected=true"); r.Matched != 1 || got[0] != "host-200" {
		t.Errorf("combined filters: matched %d %v", r.Matched, got)
	}
	for _, bad := range []string{"limit=-1", "offset=x", "sort=bogus", "order=up", "state=happy", "connected=maybe", "window=7d"} {
		if code, body := get(t, "http://"+s.HTTPAddr()+"/api/v1/fleet?"+bad, ""); code != 400 || !strings.Contains(body, `"error"`) {
			t.Errorf("?%s: %d %s", bad, code, body)
		}
	}

	_, prom := get(t, "http://"+s.HTTPAddr()+"/metrics", "")
	for _, want := range []string{
		`deltav_member_requests_total{class="2xx",node="host-007"} 30`,
		`deltav_fleet_members{state="disconnected"} 20`,
		`deltav_fleet_config{state="nacked"} 5`,
	} {
		if !strings.Contains(prom, want+"\n") {
			t.Errorf("/metrics lacks: %s", want)
		}
	}
}

func TestEventStream(t *testing.T) {
	s := telemetryServer(t)
	s.agg.Record("host-a", samples(7))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+s.HTTPAddr()+"/api/v1/events?interval=100ms", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("events: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	// Two events prove it repeats; each must be a complete overview.
	sc := bufio.NewScanner(resp.Body)
	events := 0
	for events < 2 && sc.Scan() {
		line := sc.Text()
		if line == "event: overview" {
			continue
		}
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			if line != "" {
				t.Fatalf("unexpected line %q", line)
			}
			continue
		}
		var ov overviewResponse
		if err := json.Unmarshal([]byte(data), &ov); err != nil || len(ov.Targets) != 1 || ov.Targets[0].Stats.Requests != 7 {
			t.Fatalf("event %d: %v %s", events, err, data)
		}
		events++
	}
	if events != 2 {
		t.Fatalf("got %d events: %v", events, sc.Err())
	}
	if code, _ := get(t, "http://"+s.HTTPAddr()+"/api/v1/events?interval=1ns", ""); code != 400 {
		t.Errorf("interval=1ns: %d", code)
	}
	// The test's cleanup then stops the daemon with this stream still open:
	// shutdown must not wait for it.
}
