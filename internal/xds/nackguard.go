package xds

import (
	"context"
	"sync"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
)

// withNACKGuard stops a rejected config from being re-sent in a loop.
//
// A NACK carries the version Envoy last accepted, not the one it rejected.
// go-control-plane's snapshot cache (v0.14.0) has no notion of a NACK: it
// sees a request whose version differs from the snapshot's and answers at
// once with the same resources, which Envoy rejects again, without pause.
// One bad listener port on some hosts would have each of them spinning
// against the daemon.
//
// The guard remembers the version of the last response sent per stream and
// type. When that response is NACKed it rewrites the request's version to
// the rejected one before the cache sees it, so the cache holds the watch
// open and answers only once the snapshot changes. This relies on the
// server handing the callback the same request it then passes to the cache;
// TestADSFlowAndFleetRegistry in internal/server fails if that changes.
//
// next (may be nil) is called first and sees every request unmodified.
func withNACKGuard(next serverv3.Callbacks) serverv3.Callbacks {
	type sent struct{ nonce, version string }
	var (
		mu      sync.Mutex
		streams = map[int64]map[string]sent{} // stream ID -> type URL -> last response
	)
	return serverv3.CallbackFuncs{
		StreamOpenFunc: func(ctx context.Context, id int64, typeURL string) error {
			if next == nil {
				return nil
			}
			return next.OnStreamOpen(ctx, id, typeURL)
		},
		StreamClosedFunc: func(id int64, node *corev3.Node) {
			mu.Lock()
			delete(streams, id)
			mu.Unlock()
			if next != nil {
				next.OnStreamClosed(id, node)
			}
		},
		StreamRequestFunc: func(id int64, req *discoveryv3.DiscoveryRequest) error {
			if next != nil {
				if err := next.OnStreamRequest(id, req); err != nil {
					return err
				}
			}
			if req.GetErrorDetail() == nil {
				return nil
			}
			mu.Lock()
			last, ok := streams[id][req.GetTypeUrl()]
			mu.Unlock()
			if ok && last.nonce == req.GetResponseNonce() {
				req.VersionInfo = last.version
			}
			return nil
		},
		StreamResponseFunc: func(ctx context.Context, id int64, req *discoveryv3.DiscoveryRequest, resp *discoveryv3.DiscoveryResponse) {
			mu.Lock()
			if streams[id] == nil {
				streams[id] = map[string]sent{}
			}
			streams[id][resp.GetTypeUrl()] = sent{nonce: resp.GetNonce(), version: resp.GetVersionInfo()}
			mu.Unlock()
			if next != nil {
				next.OnStreamResponse(ctx, id, req, resp)
			}
		},
	}
}
