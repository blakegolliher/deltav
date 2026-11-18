// Package telemetry receives what every Envoy streams to the daemon: one
// access log entry per request (ALS) and periodic load reports (LRS). Both
// arrive on streams Envoy opens; nothing here reaches out to a client host.
package telemetry

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	accesslogdatav3 "github.com/envoyproxy/go-control-plane/envoy/data/accesslog/v3"
	accesslogv3 "github.com/envoyproxy/go-control-plane/envoy/service/accesslog/v3"
	loadstatsv3 "github.com/envoyproxy/go-control-plane/envoy/service/load_stats/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"deltav/internal/metrics"
	"deltav/internal/ratelimit"
)

// DefaultLoadReportInterval is how often each Envoy reports load. A report
// is one small message, so 5,000 Envoys at 5 s is 1,000 messages a second.
const DefaultLoadReportInterval = 5 * time.Second

// Server implements Envoy's AccessLogService and LoadReportingService.
type Server struct {
	accesslogv3.UnimplementedAccessLogServiceServer
	loadstatsv3.UnimplementedLoadReportingServiceServer

	agg *metrics.Aggregator
	// refused, if set, is told of every request a rate rule refused.
	refused  func([]ratelimit.Refusal)
	log      *slog.Logger
	interval time.Duration
	ignore   map[string]bool // cluster names that are not targets

	streams atomic.Int64
	entries atomic.Uint64
	reports atomic.Uint64
}

// New builds the telemetry services. Clusters named in ignore (the control
// cluster Envoy uses to reach the daemon) are left out of load reports.
func New(agg *metrics.Aggregator, log *slog.Logger, loadReportInterval time.Duration, ignore ...string) *Server {
	if loadReportInterval <= 0 {
		loadReportInterval = DefaultLoadReportInterval
	}
	s := &Server{agg: agg, log: log.With("component", "telemetry"), interval: loadReportInterval, ignore: map[string]bool{}}
	for _, name := range ignore {
		s.ignore[name] = true
	}
	return s
}

// OnRefusals sets who is told of the requests rate rules refuse, a batch at
// a time as Envoys log them. Call it before the services take streams.
func (s *Server) OnRefusals(f func([]ratelimit.Refusal)) { s.refused = f }

// Register adds both services to a gRPC server.
func (s *Server) Register(g *grpc.Server) {
	accesslogv3.RegisterAccessLogServiceServer(g, s)
	loadstatsv3.RegisterLoadReportingServiceServer(g, s)
}

// Entries and Reports count what has been ingested since start.
func (s *Server) Entries() uint64 { return s.entries.Load() }
func (s *Server) Reports() uint64 { return s.reports.Load() }

// StreamAccessLogs receives access log entries. The first message on a
// stream identifies the Envoy and the log; later ones carry only entries.
func (s *Server) StreamAccessLogs(stream accesslogv3.AccessLogService_StreamAccessLogsServer) error {
	var node, logName string
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return stream.SendAndClose(&accesslogv3.StreamAccessLogsResponse{})
		}
		if err != nil {
			return err
		}
		if id := msg.GetIdentifier(); id != nil {
			node, logName = id.GetNode().GetId(), id.GetLogName()
		}
		if node == "" {
			return status.Error(codes.InvalidArgument, "the first access log message must identify the node")
		}
		entries := msg.GetHttpLogs().GetLogEntry()
		if len(entries) == 0 {
			continue
		}
		batch := make([]metrics.Sample, 0, len(entries))
		var refused []ratelimit.Refusal
		for _, e := range entries {
			sample, ok := sampleFrom(e, logName)
			if !ok {
				continue
			}
			batch = append(batch, sample)
			if sample.Refused && s.refused != nil {
				refused = append(refused, refusalFrom(e, node, sample))
			}
		}
		s.agg.Record(node, batch)
		s.entries.Add(uint64(len(batch)))
		if len(refused) > 0 {
			s.refused(refused)
		}
	}
}

// sampleFrom converts one access log entry. logName is the target name the
// listener logs under; it stands in when Envoy never selected a cluster.
func sampleFrom(e *accesslogdatav3.HTTPAccessLogEntry, logName string) (metrics.Sample, bool) {
	c := e.GetCommonProperties()
	if c.GetIntermediateLogEntry() {
		return metrics.Sample{}, false // a periodic record of a request still running
	}
	s := metrics.Sample{
		Target:   c.GetUpstreamCluster(),
		Endpoint: hostPort(c.GetUpstreamRemoteAddress()),
		BytesIn:  e.GetRequest().GetRequestBodyBytes(),
		BytesOut: e.GetResponse().GetResponseBodyBytes(),
		Overflow: c.GetResponseFlags().GetUpstreamOverflow(),
	}
	if s.Target == "" {
		s.Target = logName
	}
	if code := e.GetResponse().GetResponseCode(); code != nil {
		s.Status = int(code.GetValue())
	}
	// A request a rate rule refused was answered by Envoy and never went to
	// the target. A blocked rule's refusal is the script's, which leaves
	// the rule's name; one over a limit is the quota filter's, which Envoy
	// marks in the response's details.
	s.Refused = note(e, ratelimit.MetadataRefusedBy) != "" || e.GetResponse().GetResponseCodeDetails() == ratelimit.RefusedDetails
	// duration runs to the end of the stream; older Envoys only set the
	// time to the last byte sent downstream.
	if d := c.GetDuration(); d != nil {
		s.Duration = d.AsDuration()
	} else if d := c.GetTimeToLastDownstreamTxByte(); d != nil {
		s.Duration = d.AsDuration()
	}
	return s, true
}

// note returns what the rate-limit script noted about a request under key.
func note(e *accesslogdatav3.HTTPAccessLogEntry, key string) string {
	return e.GetCommonProperties().GetMetadata().GetFilterMetadata()[ratelimit.MetadataNamespace].GetFields()[key].GetStringValue()
}

func refusalFrom(e *accesslogdatav3.HTTPAccessLogEntry, node string, s metrics.Sample) ratelimit.Refusal {
	r := ratelimit.Refusal{
		Client: node, Target: s.Target, Status: s.Status,
		Operation: note(e, ratelimit.MetadataOperation), AccessKey: note(e, ratelimit.MetadataAccessKey), Bucket: note(e, ratelimit.MetadataBucket),
	}
	if t := e.GetCommonProperties().GetStartTime(); t != nil {
		r.At = t.AsTime()
	}
	if by := note(e, ratelimit.MetadataRefusedBy); by != "" {
		r.Blocked, r.Rules = true, []string{by}
	} else if rules := note(e, ratelimit.MetadataRules); rules != "" {
		r.Rules = strings.Split(rules, ",")
	}
	if r.Rules == nil {
		r.Rules = []string{}
	}
	return r
}

func hostPort(a *corev3.Address) string {
	sa := a.GetSocketAddress()
	if sa == nil || sa.GetAddress() == "" {
		return ""
	}
	return net.JoinHostPort(sa.GetAddress(), strconv.FormatUint(uint64(sa.GetPortValue()), 10))
}

// StreamLoadStats asks an Envoy for per-endpoint load reports and consumes
// them. A request's total_requests_in_progress is the number of requests
// that Envoy has in flight to an endpoint at that moment. S3 over HTTP/1.1
// holds one request per connection, so this is its count of busy
// connections; idle keep-alive connections are not included.
func (s *Server) StreamLoadStats(stream loadstatsv3.LoadReportingService_StreamLoadStatsServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	node := first.GetNode().GetId()
	if node == "" {
		return status.Error(codes.InvalidArgument, "the first load stats request must identify the node")
	}
	if err := stream.Send(&loadstatsv3.LoadStatsResponse{
		SendAllClusters:           true,
		LoadReportingInterval:     durationpb.New(s.interval),
		ReportEndpointGranularity: true,
	}); err != nil {
		return err
	}
	id := s.streams.Add(1)
	defer s.agg.LoadGone(node, id)

	for req := first; ; {
		// The opening request normally carries no stats; a report with
		// none means that Envoy has nothing in flight anywhere.
		if req != first || len(req.GetClusterStats()) > 0 {
			s.agg.Load(node, id, s.reportsFrom(req))
			s.reports.Add(1)
		}
		if req, err = stream.Recv(); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func (s *Server) reportsFrom(req *loadstatsv3.LoadStatsRequest) []metrics.LoadReport {
	var out []metrics.LoadReport
	for _, cs := range req.GetClusterStats() {
		if s.ignore[cs.GetClusterName()] {
			continue
		}
		for _, ls := range cs.GetUpstreamLocalityStats() {
			for _, es := range ls.GetUpstreamEndpointStats() {
				out = append(out, metrics.LoadReport{
					Key:        metrics.EndpointKey{Target: cs.GetClusterName(), Endpoint: hostPort(es.GetAddress())},
					InProgress: es.GetTotalRequestsInProgress(),
					Issued:     es.GetTotalIssuedRequests(),
					Success:    es.GetTotalSuccessfulRequests(),
					Errors:     es.GetTotalErrorRequests(),
				})
			}
		}
	}
	return out
}
