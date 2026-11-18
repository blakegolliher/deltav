// Package fleet is the registry of Envoys connected to the daemon. It is fed
// only by ADS stream callbacks: nothing here dials a client host.
package fleet

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"google.golang.org/grpc/peer"
)

// ConfigState says where an Envoy stands with the config last pushed to it.
type ConfigState string

const (
	// StatePending: a push (or the first subscription) has not been answered.
	StatePending ConfigState = "pending"
	// StateAcked: Envoy accepted the last push.
	StateAcked ConfigState = "acked"
	// StateNacked: Envoy rejected the last push and kept its previous config.
	StateNacked ConfigState = "nacked"
)

// TypeStatus is the ACK/NACK state of one xDS resource type on one Envoy.
type TypeStatus struct {
	Type         string      `json:"type"` // cds, eds, lds, ... or the type URL
	State        ConfigState `json:"state"`
	SentVersion  string      `json:"sent_version,omitempty"`  // last version pushed
	AckedVersion string      `json:"acked_version,omitempty"` // last version Envoy accepted
	Error        string      `json:"error,omitempty"`         // Envoy's text for a NACK
	UpdatedAt    time.Time   `json:"updated_at"`
}

// Member is one Envoy, keyed by its xDS node ID.
type Member struct {
	NodeID         string     `json:"node_id"`
	Cluster        string     `json:"cluster,omitempty"`
	EnvoyVersion   string     `json:"envoy_version,omitempty"`
	Address        string     `json:"address,omitempty"` // peer address of the ADS stream
	Connected      bool       `json:"connected"`
	ConnectedAt    time.Time  `json:"connected_at"`
	DisconnectedAt *time.Time `json:"disconnected_at,omitempty"`
	LastSeen       time.Time  `json:"last_seen"`
	// Workers is how many worker threads the Envoy says it runs, in its
	// node metadata (WorkersMetadataKey); 0 if it does not say. The client
	// unit sets it from the same variable as Envoy's --concurrency. The
	// enforce-mode share allows for it (DESIGN.md, amendments 34 and 40).
	Workers int `json:"workers,omitempty"`

	// ConfigVersion is the version every subscribed type has ACKed, or ""
	// while types disagree or none has answered. ConfigState is the worst
	// state across types (nacked, then pending, then acked) and ConfigError
	// the first NACK text.
	ConfigVersion string       `json:"config_version,omitempty"`
	ConfigState   ConfigState  `json:"config_state"`
	ConfigError   string       `json:"config_error,omitempty"`
	Types         []TypeStatus `json:"types"`
}

type stream struct {
	nodeID string
	peer   string
	opened time.Time
	nonce  map[string]string // type URL -> nonce of the last response sent
}

type member struct {
	Member
	streamID int64
	types    map[string]*TypeStatus // by type URL
}

// Registry tracks every Envoy that has opened an ADS stream.
type Registry struct {
	mu      sync.Mutex
	now     func() time.Time
	streams map[int64]*stream
	members map[string]*member
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{now: time.Now, streams: map[int64]*stream{}, members: map[string]*member{}}
}

// Callbacks adapts the registry to go-control-plane's stream callbacks.
func (r *Registry) Callbacks() serverv3.Callbacks {
	return serverv3.CallbackFuncs{
		StreamOpenFunc: func(ctx context.Context, id int64, _ string) error {
			addr := ""
			if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
				addr = p.Addr.String()
			}
			r.StreamOpen(id, addr)
			return nil
		},
		StreamClosedFunc: func(id int64, _ *corev3.Node) { r.StreamClosed(id) },
		StreamRequestFunc: func(id int64, req *discoveryv3.DiscoveryRequest) error {
			r.Request(id, req)
			return nil
		},
		StreamResponseFunc: func(_ context.Context, id int64, _ *discoveryv3.DiscoveryRequest, resp *discoveryv3.DiscoveryResponse) {
			r.Response(id, resp)
		},
	}
}

// StreamOpen records a new ADS stream. The Envoy behind it is not known
// until its first request, which is the only one sure to carry the node.
func (r *Registry) StreamOpen(id int64, peerAddr string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.streams[id] = &stream{peer: peerAddr, opened: r.now(), nonce: map[string]string{}}
}

// WorkersMetadataKey is the node metadata field in which an Envoy declares
// how many worker threads it runs. Envoy does not report that by itself.
const WorkersMetadataKey = "deltav_workers"

// declaredWorkers reads WorkersMetadataKey; 0 means not declared, or not a
// believable number.
func declaredWorkers(node *corev3.Node) int {
	v, ok := node.GetMetadata().GetFields()[WorkersMetadataKey]
	if !ok {
		return 0
	}
	n := v.GetNumberValue()
	if n < 1 || n > 4096 || n != float64(int(n)) {
		return 0
	}
	return int(n)
}

// Workers summarizes the worker threads of the connected Envoys: the most
// any one declares (0 if none does), and how many do not declare any.
func (r *Registry) Workers() (most, undeclared int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.members {
		if !m.Connected {
			continue
		}
		if m.Workers == 0 {
			undeclared++
		}
		most = max(most, m.Workers)
	}
	return most, undeclared
}

// Request records a discovery request: the first one on a stream registers
// the member, and one that carries a response nonce is an ACK or a NACK.
func (r *Registry) Request(id int64, req *discoveryv3.DiscoveryRequest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	st := r.streams[id]
	if st == nil {
		st = &stream{opened: now, nonce: map[string]string{}}
		r.streams[id] = st
	}
	if st.nodeID == "" {
		node := req.GetNode()
		if node.GetId() == "" {
			return // nothing to key the member on
		}
		st.nodeID = node.GetId()
		m := r.members[st.nodeID]
		if m == nil {
			m = &member{}
			r.members[st.nodeID] = m
		}
		// A new stream supersedes any earlier one from the same node (an
		// Envoy restart can open it before the old stream is seen closing).
		m.Member = Member{
			NodeID:       st.nodeID,
			Cluster:      node.GetCluster(),
			EnvoyVersion: envoyVersion(node),
			Address:      st.peer,
			Connected:    true,
			ConnectedAt:  st.opened,
			Workers:      declaredWorkers(node),
		}
		m.streamID = id
		m.types = map[string]*TypeStatus{}
	}
	m := r.members[st.nodeID]
	if m == nil || m.streamID != id {
		return // a superseded stream still draining
	}
	m.LastSeen = now

	typeURL := req.GetTypeUrl()
	ts := m.types[typeURL]
	if ts == nil {
		ts = &TypeStatus{Type: shortType(typeURL), State: StatePending, UpdatedAt: now}
		m.types[typeURL] = ts
	}
	nonce := req.GetResponseNonce()
	if nonce == "" || nonce != st.nonce[typeURL] {
		return // a first subscription, or an answer to a response since replaced
	}
	if detail := req.GetErrorDetail(); detail != nil {
		ts.State = StateNacked
		ts.Error = detail.GetMessage()
		if ts.Error == "" {
			ts.Error = fmt.Sprintf("rejected (code %d, no message)", detail.GetCode())
		}
	} else {
		ts.State = StateAcked
		ts.AckedVersion = req.GetVersionInfo()
		ts.Error = ""
	}
	ts.UpdatedAt = now
}

// Response records a push, which stays pending until Envoy answers it.
func (r *Registry) Response(id int64, resp *discoveryv3.DiscoveryResponse) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.streams[id]
	if st == nil {
		return
	}
	typeURL := resp.GetTypeUrl()
	st.nonce[typeURL] = resp.GetNonce()
	m := r.members[st.nodeID]
	if m == nil || m.streamID != id {
		return
	}
	now := r.now()
	ts := m.types[typeURL]
	if ts == nil {
		ts = &TypeStatus{Type: shortType(typeURL)}
		m.types[typeURL] = ts
	}
	ts.State = StatePending
	ts.SentVersion = resp.GetVersionInfo()
	ts.Error = "" // an earlier NACK was about config this push replaces
	ts.UpdatedAt = now
}

// StreamClosed marks the member disconnected unless a newer stream from the
// same node has already replaced this one.
func (r *Registry) StreamClosed(id int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.streams[id]
	delete(r.streams, id)
	if st == nil {
		return
	}
	if m := r.members[st.nodeID]; m != nil && m.streamID == id {
		now := r.now()
		m.Connected = false
		m.DisconnectedAt = &now
	}
}

// Prune forgets members that disconnected more than retention ago and
// returns how many it removed.
func (r *Registry) Prune(retention time.Duration) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := r.now().Add(-retention)
	n := 0
	for id, m := range r.members {
		if !m.Connected && m.DisconnectedAt != nil && m.DisconnectedAt.Before(cutoff) {
			delete(r.members, id)
			n++
		}
	}
	return n
}

// Connected is the number of Envoys with an open ADS stream.
func (r *Registry) Connected() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, m := range r.members {
		if m.Connected {
			n++
		}
	}
	return n
}

// List returns a copy of every member, sorted by node ID.
func (r *Registry) List() []Member {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Member, 0, len(r.members))
	for _, m := range r.members {
		out = append(out, m.snapshot())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}

// Get returns one member by node ID.
func (r *Registry) Get(nodeID string) (Member, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.members[nodeID]
	if m == nil {
		return Member{}, false
	}
	return m.snapshot(), true
}

// snapshot copies the member and derives its summary fields.
func (m *member) snapshot() Member {
	out := m.Member
	out.Types = make([]TypeStatus, 0, len(m.types))
	for _, ts := range m.types {
		out.Types = append(out.Types, *ts)
	}
	sort.Slice(out.Types, func(i, j int) bool { return out.Types[i].Type < out.Types[j].Type })

	out.ConfigState = StatePending
	out.ConfigVersion, out.ConfigError = "", ""
	if len(out.Types) == 0 {
		return out
	}
	out.ConfigState = StateAcked
	same := true
	for _, ts := range out.Types {
		switch {
		case ts.State == StateNacked:
			if out.ConfigState != StateNacked {
				out.ConfigError = ts.Type + ": " + ts.Error
			}
			out.ConfigState = StateNacked
		case ts.State == StatePending && out.ConfigState == StateAcked:
			out.ConfigState = StatePending
		}
		if ts.AckedVersion != out.Types[0].AckedVersion {
			same = false
		}
	}
	if same && out.ConfigState == StateAcked {
		out.ConfigVersion = out.Types[0].AckedVersion
	}
	return out
}

// envoyVersion returns "1.40.2" from the node's build version, falling back
// to the free-form user agent version string.
func envoyVersion(n *corev3.Node) string {
	if v := n.GetUserAgentBuildVersion().GetVersion(); v != nil {
		return fmt.Sprintf("%d.%d.%d", v.GetMajorNumber(), v.GetMinorNumber(), v.GetPatch())
	}
	return n.GetUserAgentVersion()
}

func shortType(typeURL string) string {
	switch typeURL {
	case resource.ClusterType:
		return "cds"
	case resource.EndpointType:
		return "eds"
	case resource.ListenerType:
		return "lds"
	case resource.RouteType:
		return "rds"
	case resource.SecretType:
		return "sds"
	case resource.RuntimeType:
		return "rtds"
	}
	return typeURL
}
