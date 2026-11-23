// Package xds renders Envoy configuration for Delta-V targets and serves it
// over ADS using go-control-plane's snapshot cache.
package xds

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discoverygrpc "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	sotwv3 "github.com/envoyproxy/go-control-plane/pkg/server/sotw/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// fleetKey is the one snapshot key: every Envoy receives every target, so
// every node hashes to the same snapshot.
const fleetKey = "fleet"

type fleetHash struct{}

func (fleetHash) ID(*corev3.Node) string { return fleetKey }

// Server owns the snapshot cache and the ADS gRPC service.
type Server struct {
	cache cache.SnapshotCache
	ads   serverv3.Server

	mu       sync.Mutex
	epoch    string // distinguishes this process's versions from an earlier run's
	seq      uint64
	version  string
	digest   [sha256.Size]byte
	targets  []TargetSpec
	snapshot cache.Snapshot // what the cache currently holds
	// dirty is set while the cache may hold something other than version:
	// from the start of a publish until all four types are in. Update then
	// republishes even input identical to the last complete one, so going
	// back to it after a failed publish is not mistaken for a no-op.
	dirty bool
}

// New builds an ADS server whose snapshot holds no targets yet. callbacks
// (may be nil) observe every stream; the fleet registry is fed this way.
func New(ctx context.Context, log *slog.Logger, callbacks serverv3.Callbacks) (*Server, error) {
	// The cache's "ADS" flag is off on purpose. With it on, the cache
	// withholds an EDS response, and drops the watch, whenever the snapshot
	// holds an endpoint resource the Envoy has not asked for. An Envoy that
	// rejects one target's cluster never asks for that cluster's endpoints,
	// so it would stop receiving endpoint updates for every other target
	// until it reconnects. What the flag also provides, ordering across
	// resource types, publish does itself.
	c := cache.NewSnapshotCache(false, fleetHash{}, slogAdapter{log.With("component", "xds-cache")})
	s := &Server{
		cache: c,
		// Ordered ADS: without it the server reads each type's responses
		// from its own channel in random order, undoing publish's ordering.
		ads: serverv3.NewServer(ctx, c, withNACKGuard(callbacks), sotwv3.WithOrderedADS()),
		// Envoy sends the version it already holds when it reconnects, and
		// the cache answers only if its own differs. A bare counter would
		// restart at 1 with the daemon and could collide with a version
		// Envoy kept from the previous run, leaving it on stale config.
		epoch: newEpoch(),
	}
	// An empty snapshot rather than none: Envoys that connect before any
	// target exists get an answer to ACK instead of waiting on a timeout.
	if _, _, err := s.Update(ctx, nil); err != nil {
		return nil, err
	}
	return s, nil
}

// newEpoch names this process's run of versions: its start time, which
// keeps versions readable, and random bits, so two runs started in the same
// second still differ (DESIGN.md, amendments 16 and 74).
func newEpoch() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on supported platforms; nanoseconds are
		// still far less likely to repeat than seconds.
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return strconv.FormatInt(time.Now().Unix(), 36) + "-" + hex.EncodeToString(b[:])
}

// Register adds the ADS service to a gRPC server.
func (s *Server) Register(g *grpc.Server) {
	discoverygrpc.RegisterAggregatedDiscoveryServiceServer(g, s.ads)
}

// Update replaces the fleet's configuration with targets. It returns the
// snapshot version now current and whether this call changed it; identical
// input is not pushed again. Every resource type shares one version, so a
// cluster that re-warms after a CDS change is sent its endpoints at once
// (DESIGN.md, amendment 12).
func (s *Server) Update(ctx context.Context, targets []TargetSpec) (version string, changed bool, err error) {
	targets = append([]TargetSpec(nil), targets...)
	sort.Slice(targets, func(i, j int) bool { return targets[i].Name < targets[j].Name })

	var clusters, endpoints, listeners, routes []types.Resource
	names, ports := map[string]bool{}, map[uint32]string{}
	h := sha256.New()
	for _, t := range targets {
		if names[t.Name] {
			return "", false, fmt.Errorf("target %q is defined twice", t.Name)
		}
		if other, taken := ports[t.ListenPort]; taken {
			return "", false, fmt.Errorf("targets %q and %q both listen on port %d", other, t.Name, t.ListenPort)
		}
		names[t.Name], ports[t.ListenPort] = true, t.Name

		r, err := t.Resources()
		if err != nil {
			return "", false, err
		}
		for _, m := range []proto.Message{r.Cluster, r.Endpoints, r.Listener, r.Route} {
			b, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
			if err != nil {
				return "", false, err
			}
			fmt.Fprintf(h, "%d:", len(b))
			h.Write(b)
		}
		clusters, endpoints = append(clusters, r.Cluster), append(endpoints, r.Endpoints)
		listeners, routes = append(listeners, r.Listener), append(routes, r.Route)
	}
	var digest [sha256.Size]byte
	h.Sum(digest[:0])

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.version != "" && !s.dirty && digest == s.digest {
		return s.version, false, nil
	}
	// The counter advances even if publishing fails part-way, so a version
	// string is never reused for different content.
	s.seq++
	next := fmt.Sprintf("%s.%d", s.epoch, s.seq)
	s.dirty = true
	if err := s.publish(ctx, next, clusters, endpoints, listeners, routes); err != nil {
		return "", false, err
	}
	s.version, s.digest, s.targets, s.dirty = next, digest, targets, false
	return next, true, nil
}

// publish moves the fleet to a new version one resource type at a time:
// clusters, then endpoints, then listeners, then routes. Each step changes
// only that type's version, so the cache pushes only that type, and a
// stream that is waiting on all four carries the pushes in this order.
// Envoy therefore knows a cluster before the listener and routes that lead
// to it, and a cluster that re-warms after a change finds its endpoints
// right behind it. A new listener waits for its routes before it takes
// connections, and asks for them by name as soon as it arrives.
//
// The order is best effort: a stream whose ACK of the previous push is
// still in flight has no watch open for that type, and is answered when the
// ACK arrives, possibly after the later types. Both effects of that are
// harmless: routes do not validate clusters (see route), and a re-warming
// cluster falls back to Envoy's cached endpoints.
//
// Every type gets the new version, changed or not. Envoy compares what it
// is sent with what it has: an identical listener is left alone, which is
// what lets a rule change, carried in the routes, leave every connection
// open.
func (s *Server) publish(ctx context.Context, version string, clusters, endpoints, listeners, routes []types.Resource) error {
	for _, step := range []struct {
		typeURL resource.Type
		items   []types.Resource
	}{
		{resource.ClusterType, clusters},
		{resource.EndpointType, endpoints},
		{resource.ListenerType, listeners},
		{resource.RouteType, routes},
	} {
		s.snapshot.Resources[cache.GetResponseType(step.typeURL)] = cache.NewResources(version, step.items)
		snap := s.snapshot // the cache keeps the pointer; give it its own copy
		if err := s.cache.SetSnapshot(ctx, fleetKey, &snap); err != nil {
			return fmt.Errorf("publishing %s: %w", step.typeURL, err)
		}
	}
	return nil
}

// Version is the snapshot version currently offered to the fleet.
func (s *Server) Version() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.version
}

// OpenWatches is the number of subscriptions waiting for the next change:
// one per resource type per connected Envoy once the fleet has settled.
func (s *Server) OpenWatches() int {
	if info := s.cache.GetStatusInfo(fleetKey); info != nil {
		return info.GetNumWatches()
	}
	return 0
}

// Targets returns the targets in the current snapshot, sorted by name.
func (s *Server) Targets() []TargetSpec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]TargetSpec(nil), s.targets...)
}

// slogAdapter satisfies go-control-plane's printf-style logger.
type slogAdapter struct{ l *slog.Logger }

func (a slogAdapter) Debugf(f string, v ...any) { a.l.Debug(fmt.Sprintf(f, v...)) }
func (a slogAdapter) Infof(f string, v ...any)  { a.l.Info(fmt.Sprintf(f, v...)) }
func (a slogAdapter) Warnf(f string, v ...any)  { a.l.Warn(fmt.Sprintf(f, v...)) }
func (a slogAdapter) Errorf(f string, v ...any) { a.l.Error(fmt.Sprintf(f, v...)) }
