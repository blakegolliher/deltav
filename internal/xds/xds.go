// Package xds serves Envoy configuration over ADS using go-control-plane's
// snapshot cache. Milestone 1 wires an empty cache; milestone 2 fills it.
package xds

import (
	"context"
	"fmt"
	"log/slog"

	discoverygrpc "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"google.golang.org/grpc"
)

// Server owns the snapshot cache and the ADS gRPC service.
type Server struct {
	Cache cache.SnapshotCache
	ads   serverv3.Server
}

// New builds an ADS server over a fresh snapshot cache.
func New(ctx context.Context, log *slog.Logger) *Server {
	c := cache.NewSnapshotCache(true, cache.IDHash{}, slogAdapter{log.With("component", "xds-cache")})
	return &Server{Cache: c, ads: serverv3.NewServer(ctx, c, nil)}
}

// Register adds the ADS service to a gRPC server.
func (s *Server) Register(g *grpc.Server) {
	discoverygrpc.RegisterAggregatedDiscoveryServiceServer(g, s.ads)
}

// slogAdapter satisfies go-control-plane's printf-style logger.
type slogAdapter struct{ l *slog.Logger }

func (a slogAdapter) Debugf(f string, v ...any) { a.l.Debug(fmt.Sprintf(f, v...)) }
func (a slogAdapter) Infof(f string, v ...any)  { a.l.Info(fmt.Sprintf(f, v...)) }
func (a slogAdapter) Warnf(f string, v ...any)  { a.l.Warn(fmt.Sprintf(f, v...)) }
func (a slogAdapter) Errorf(f string, v ...any) { a.l.Error(fmt.Sprintf(f, v...)) }
