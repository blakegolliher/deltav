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

	"deltav/internal/version"
	"deltav/internal/xds"
)

// Config is the daemon's listen configuration.
type Config struct {
	GRPCAddr string
	HTTPAddr string
	APIToken string
	Logger   *slog.Logger
}

// Server is a configured, not yet listening, daemon.
type Server struct {
	cfg      Config
	log      *slog.Logger
	registry *prometheus.Registry

	grpc *grpc.Server
	http *http.Server
	xds  *xds.Server

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
	s.xds = xds.New(ctx, log)
	s.xds.Register(s.grpc)

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
	wg.Add(2)
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
