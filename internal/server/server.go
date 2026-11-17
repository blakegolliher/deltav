// Package server wires the deltav daemon: one gRPC listener for everything
// Envoy opens (ADS now; access log and load reporting from milestone 3) and
// one HTTP listener for the API, web UI and /metrics.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	"deltav/internal/fleet"
	"deltav/internal/metrics"
	"deltav/internal/telemetry"
	"deltav/internal/version"
	"deltav/internal/xds"
)

const (
	// fleetRetention is how long a disconnected Envoy stays in the fleet list.
	fleetRetention = time.Hour
	// seriesRetention is how long an endpoint or Envoy with no requests
	// keeps its series: a little past the longest window.
	seriesRetention = time.Hour + 5*time.Minute
	// staleLoadReport drops in-flight counts from an Envoy whose load
	// reports stopped without its stream closing.
	staleLoadReport = 2 * time.Minute
)

// Config is the daemon's listen configuration.
type Config struct {
	GRPCAddr string
	HTTPAddr string
	APIToken string
	Logger   *slog.Logger

	// StaticTargets are served as given, with no VAST cluster behind them
	// (serve --static-target). They are for testing.
	StaticTargets []xds.TargetSpec

	// LoadReportInterval is how often each Envoy is asked to report load;
	// zero means telemetry.DefaultLoadReportInterval.
	LoadReportInterval time.Duration
	// MetricsPerMember adds per-Envoy series to /metrics. Off by default:
	// thousands of hosts make too many series.
	MetricsPerMember bool
}

// Server is a configured, not yet listening, daemon.
type Server struct {
	cfg      Config
	log      *slog.Logger
	registry *prometheus.Registry

	grpc      *grpc.Server
	http      *http.Server
	xds       *xds.Server
	fleet     *fleet.Registry
	agg       *metrics.Aggregator
	telemetry *telemetry.Server

	ctx    context.Context
	cancel context.CancelFunc

	grpcLn, httpLn net.Listener
}

// New validates cfg and builds the daemon without binding any port.
func New(cfg Config) (*Server, error) {
	if cfg.APIToken == "" && !isLoopback(cfg.HTTPAddr) {
		return nil, fmt.Errorf("--http-addr %s is not loopback: set an API token (DELTAV_API_TOKEN)", cfg.HTTPAddr)
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{cfg: cfg, log: log, registry: prometheus.NewRegistry(), ctx: ctx, cancel: cancel}

	s.registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{Namespace: "deltav"}),
	)
	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "deltav_build_info",
		Help: "Build information; always 1.",
	}, []string{"version", "envoy_version", "go_version"})
	buildInfo.WithLabelValues(version.Version, version.EnvoyPinned, runtime.Version()).Set(1)
	s.registry.MustRegister(buildInfo)

	// Envoys hold one long-lived ADS stream each; keepalives let both sides
	// notice dead peers without waiting for TCP timeouts.
	s.grpc = grpc.NewServer(
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: 30 * time.Second, Timeout: 5 * time.Second}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 15 * time.Second, PermitWithoutStream: true}),
	)
	s.fleet = fleet.New()
	x, err := xds.New(ctx, log, s.fleet.Callbacks())
	if err != nil {
		cancel()
		return nil, err
	}
	s.xds = x
	s.xds.Register(s.grpc)

	s.agg = metrics.New()
	s.telemetry = telemetry.New(s.agg, log, cfg.LoadReportInterval, xds.ControlCluster)
	s.telemetry.Register(s.grpc)
	s.registry.MustRegister(collector{s: s, perMember: cfg.MetricsPerMember})

	if len(cfg.StaticTargets) > 0 {
		// Every target streams access logs to the daemon. (The per-target
		// sampling knob of DESIGN.md amendment 8 arrives with target
		// management.)
		static := append([]xds.TargetSpec(nil), cfg.StaticTargets...)
		for i := range static {
			static[i].AccessLog = true
		}
		v, _, err := s.xds.Update(ctx, static)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("static targets: %w", err)
		}
		log.Info("serving static targets", "count", len(cfg.StaticTargets), "config_version", v)
	}

	var handler http.Handler = s.routes()
	if cfg.APIToken != "" {
		handler = requireToken(cfg.APIToken, map[string]bool{"/healthz": true}, handler)
	}
	s.http = &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	return s, nil
}

func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /api/v1/version", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{
			"name":         "deltav",
			"version":      version.Version,
			"envoy_pinned": version.EnvoyPinned,
			"go_version":   runtime.Version(),
		})
	})
	mux.HandleFunc("GET /api/v1/overview", s.handleOverview)
	mux.HandleFunc("GET /api/v1/targets", s.handleTargets)
	mux.HandleFunc("GET /api/v1/endpoints", s.handleEndpoints)
	mux.HandleFunc("GET /api/v1/fleet", s.handleFleet)
	mux.HandleFunc("GET /api/v1/fleet/{node}", s.handleFleetMember)
	mux.HandleFunc("GET /api/v1/events", s.handleEvents)
	mux.Handle("GET /metrics", promhttp.HandlerFor(s.registry, promhttp.HandlerOpts{Registry: s.registry}))
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// XDS exposes the xDS server so in-process callers (tests, and the VAST
// poller from milestone 4) can push targets.
func (s *Server) XDS() *xds.Server { return s.xds }

// Listen binds both listeners. Run calls it if needed; tests call it first
// to learn the ephemeral addresses.
func (s *Server) Listen() error {
	if s.grpcLn != nil {
		return nil
	}
	var lc net.ListenConfig
	g, err := lc.Listen(s.ctx, "tcp", s.cfg.GRPCAddr)
	if err != nil {
		return fmt.Errorf("grpc listen: %w", err)
	}
	h, err := lc.Listen(s.ctx, "tcp", s.cfg.HTTPAddr)
	if err != nil {
		_ = g.Close()
		return fmt.Errorf("http listen: %w", err)
	}
	s.grpcLn, s.httpLn = g, h
	return nil
}

// GRPCAddr and HTTPAddr return the bound addresses after Listen.
func (s *Server) GRPCAddr() string { return s.grpcLn.Addr().String() }
func (s *Server) HTTPAddr() string { return s.httpLn.Addr().String() }

// Run serves until ctx is cancelled or a listener fails, then shuts down.
func (s *Server) Run(ctx context.Context) error {
	if err := s.Listen(); err != nil {
		return err
	}
	s.log.Info("deltav serving", "version", version.Version, "grpc", s.GRPCAddr(), "http", s.HTTPAddr())

	errc := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(time.Minute)
		defer tick.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-tick.C:
				s.fleet.Prune(fleetRetention)
				s.agg.Prune(seriesRetention, staleLoadReport)
			}
		}
	}()
	go func() { defer wg.Done(); errc <- s.grpc.Serve(s.grpcLn) }()
	go func() {
		defer wg.Done()
		if err := s.http.Serve(s.httpLn); !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errc:
	}

	s.cancel() // ends ADS streams so GracefulStop can finish
	shutCtx, done := context.WithTimeout(context.Background(), 10*time.Second)
	defer done()
	_ = s.http.Shutdown(shutCtx)
	stopped := make(chan struct{})
	go func() { s.grpc.GracefulStop(); close(stopped) }()
	select {
	case <-stopped:
	case <-shutCtx.Done():
		s.grpc.Stop()
	}
	wg.Wait()
	s.log.Info("deltav stopped")
	return runErr
}
