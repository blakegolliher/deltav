package server

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"deltav/internal/fleet"
	"deltav/internal/metrics"
	"deltav/internal/xds"
)

// The HTTP API. Every handler reads daemon state through the same few
// accessors; the CLI, the TUI and the web UI are all clients of these
// endpoints and have no other way in.
//
//	GET /api/v1/overview    per-target rollups and fleet counts
//	GET /api/v1/targets     what is being served to Envoys
//	GET /api/v1/endpoints   per-endpoint stats
//	GET /api/v1/fleet       Envoys: paged, sorted, filtered
//	GET /api/v1/fleet/{id}  one Envoy, every window
//	GET /api/v1/events      server-sent events: the overview, repeated
//	GET /api/v1/settings    the daemon settings a UI needs to explain itself
//
// Stats take ?window=1m|5m|1h (default 1m).

func writeError(w http.ResponseWriter, status int, format string, args ...any) {
	writeJSON(w, status, map[string]string{"error": fmt.Sprintf(format, args...)})
}

func window(w http.ResponseWriter, r *http.Request) (metrics.Window, bool) {
	win, err := metrics.ParseWindow(r.URL.Query().Get("window"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return 0, false
	}
	return win, true
}

// settingsResponse is GET /api/v1/settings: how the daemon was started, as
// far as a UI needs it to say what a control will do.
type settingsResponse struct {
	// A recomputed enforce-mode share is pushed only when it differs from
	// the one in force by more than LimitMinChange (a fraction of it) and
	// at least LimitMinIntervalSeconds after the last push.
	LimitMinChange          float64 `json:"limit_min_change"`
	LimitMinIntervalSeconds float64 `json:"limit_min_interval_seconds"`
	VASTPollIntervalSeconds float64 `json:"vast_poll_interval_seconds"`
	// TokenRequired is true when the API asks for a bearer token.
	TokenRequired bool `json:"token_required"`
}

func (s *Server) handleSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, settingsResponse{
		LimitMinChange:          s.poller.minChange,
		LimitMinIntervalSeconds: s.poller.minInterval.Seconds(),
		VASTPollIntervalSeconds: s.poller.interval.Seconds(),
		TokenRequired:           s.cfg.APIToken != "",
	})
}

type endpointJSON struct {
	IP    string `json:"ip"`
	Port  uint32 `json:"port"`
	CNode string `json:"cnode,omitempty"`
}

type targetJSON struct {
	Name       string `json:"name"`
	ListenPort uint32 `json:"listen_port"`
	TLS        bool   `json:"tls"`
	// Source is "static" for a --static-target and "vast" for a target
	// resolved from a VAST cluster's VIP pool.
	Source  string `json:"source"`
	Cluster string `json:"cluster,omitempty"`
	Pool    string `json:"pool,omitempty"`
	Tenant  string `json:"tenant,omitempty"`
	Port    uint32 `json:"port,omitempty"` // the S3 port on the VIPs
	// Status says how a VAST target resolved at the last poll, and how its
	// limit, if it has one, is being applied.
	Status *targetStatus `json:"status,omitempty"`
	// Over lists the CNodes whose requests in flight for this target
	// exceed its limit now.
	Over      []cnodeUsage   `json:"over,omitempty"`
	Endpoints []endpointJSON `json:"endpoints"` // what Envoys are served
}

type targetsResponse struct {
	ConfigVersion string       `json:"config_version"`
	Targets       []targetJSON `json:"targets"`
}

// targetList returns every target: the static ones and the VAST ones, the
// latter also while they wait for their cluster's first poll and are not
// served yet. It is sorted by name.
func (s *Server) targetList() []targetJSON {
	served := map[string][]endpointJSON{}
	for _, t := range s.xds.Targets() {
		eps := []endpointJSON{}
		for _, e := range t.Endpoints {
			eps = append(eps, endpointJSON{IP: e.IP, Port: e.Port, CNode: e.CNode})
		}
		served[t.Name] = eps
	}
	usage := s.limitUsage()
	out := []targetJSON{}
	for _, t := range s.staticTargets() {
		out = append(out, targetJSON{Name: t.Name, ListenPort: t.ListenPort, TLS: t.TLS != nil, Source: "static", Endpoints: served[t.Name]})
	}
	for _, t := range s.store.Targets() {
		tj := targetJSON{
			Name: t.Name, ListenPort: t.ListenPort, TLS: t.TLS != nil, Source: "vast",
			Cluster: t.Cluster, Pool: t.Pool, Tenant: t.Tenant, Port: t.Port, Endpoints: served[t.Name],
		}
		if st, ok := s.poller.targetStatus(t.Name); ok {
			tj.Status = &st
		}
		if t.Limit != nil {
			tj.Over = overLimit(usage, t.Name)
		}
		if tj.Endpoints == nil {
			tj.Endpoints = []endpointJSON{}
		}
		out = append(out, tj)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Server) handleTargets(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, targetsResponse{ConfigVersion: s.xds.Version(), Targets: s.targetList()})
}

// fleetCounts summarizes the fleet. The config states count connected
// Envoys only: a disconnected one has no current state.
type fleetCounts struct {
	Total     int `json:"total"`
	Connected int `json:"connected"`
	Acked     int `json:"acked"`
	Nacked    int `json:"nacked"`
	Pending   int `json:"pending"`
}

func countFleet(members []fleet.Member) fleetCounts {
	c := fleetCounts{Total: len(members)}
	for _, m := range members {
		if !m.Connected {
			continue
		}
		c.Connected++
		switch m.ConfigState {
		case fleet.StateAcked:
			c.Acked++
		case fleet.StateNacked:
			c.Nacked++
		default:
			c.Pending++
		}
	}
	return c
}

type targetOverview struct {
	Name       string `json:"name"`
	ListenPort uint32 `json:"listen_port"`
	TLS        bool   `json:"tls"`
	Endpoints  int    `json:"endpoints"` // endpoints currently served to Envoys
	// InFlight is the fleet's requests in flight to this target, from load
	// reports. Idle keep-alive connections are not counted.
	InFlight uint64          `json:"in_flight"`
	Stats    metrics.Summary `json:"stats"`
	// RateHistory is the request rate per second in each of the last
	// complete 10-second slots, oldest first, for a sparkline.
	RateHistory []float64 `json:"rate_history"`
	// Limit is the target's per-CNode connection limit, if it has one, and
	// Over the CNodes that exceed it now.
	Limit *limitStatus `json:"limit,omitempty"`
	Over  []cnodeUsage `json:"over,omitempty"`
}

type overviewResponse struct {
	ConfigVersion string           `json:"config_version"`
	Window        string           `json:"window"`
	Fleet         fleetCounts      `json:"fleet"`
	Targets       []targetOverview `json:"targets"`
}

func (s *Server) overview(win metrics.Window) overviewResponse {
	resp := overviewResponse{
		ConfigVersion: s.xds.Version(),
		Window:        win.String(),
		Fleet:         countFleet(s.fleet.List()),
		Targets:       []targetOverview{},
	}
	stats := s.agg.Targets(win)
	history := s.agg.RateHistory(func(k metrics.EndpointKey) (string, bool) { return k.Target, true })
	idle := s.agg.Idle(win)
	usage := s.limitUsage()
	for _, t := range s.xds.Targets() {
		o := targetOverview{Name: t.Name, ListenPort: t.ListenPort, TLS: t.TLS != nil, Endpoints: len(t.Endpoints), Stats: idle}
		if st, ok := s.poller.targetStatus(t.Name); ok && st.Limit != nil {
			o.Limit, o.Over = st.Limit, overLimit(usage, t.Name)
		}
		if o.RateHistory = history[t.Name]; o.RateHistory == nil {
			o.RateHistory = make([]float64, metrics.HistoryPoints)
		}
		if ts, ok := stats[t.Name]; ok {
			o.InFlight, o.Stats = ts.InFlight, ts.Summary
			if ts.Summary.WindowSeconds == 0 {
				o.Stats = idle // in flight, but nothing finished yet
			}
		}
		resp.Targets = append(resp.Targets, o)
	}
	return resp
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	if win, ok := window(w, r); ok {
		writeJSON(w, http.StatusOK, s.overview(win))
	}
}

type endpointStats struct {
	Target string `json:"target"`
	// Endpoint is "ip:port". It is empty for requests Envoy answered
	// itself without reaching an endpoint (no healthy host, overflow).
	Endpoint string `json:"endpoint"`
	CNode    string `json:"cnode,omitempty"`
	// Configured is false for an endpoint that is no longer served to
	// Envoys but still has traffic in the window.
	Configured bool               `json:"configured"`
	InFlight   uint64             `json:"in_flight"`
	Stats      metrics.Summary    `json:"stats"`
	Load       metrics.LoadTotals `json:"load"` // from load reports since daemon start; see metrics.LoadTotals
}

type endpointsResponse struct {
	ConfigVersion string          `json:"config_version"`
	Window        string          `json:"window"`
	Endpoints     []endpointStats `json:"endpoints"`
}

// configuredEndpoints maps every endpoint now served to its CNode name.
func configuredEndpoints(targets []xds.TargetSpec) map[metrics.EndpointKey]string {
	out := map[metrics.EndpointKey]string{}
	for _, t := range targets {
		for _, e := range t.Endpoints {
			key := metrics.EndpointKey{Target: t.Name, Endpoint: net.JoinHostPort(e.IP, strconv.FormatUint(uint64(e.Port), 10))}
			out[key] = e.CNode
		}
	}
	return out
}

func (s *Server) handleEndpoints(w http.ResponseWriter, r *http.Request) {
	win, ok := window(w, r)
	if !ok {
		return
	}
	only := r.URL.Query().Get("target")
	configured := configuredEndpoints(s.xds.Targets())
	keys := make([]metrics.EndpointKey, 0, len(configured))
	for k := range configured {
		keys = append(keys, k)
	}
	resp := endpointsResponse{ConfigVersion: s.xds.Version(), Window: win.String(), Endpoints: []endpointStats{}}
	for _, e := range s.agg.Endpoints(win, keys...) {
		if only != "" && e.Key.Target != only {
			continue
		}
		cnode, isConfigured := configured[e.Key]
		resp.Endpoints = append(resp.Endpoints, endpointStats{
			Target: e.Key.Target, Endpoint: e.Key.Endpoint, CNode: cnode, Configured: isConfigured,
			InFlight: e.InFlight, Stats: e.Summary, Load: e.Load,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

type memberJSON struct {
	fleet.Member
	InFlight uint64          `json:"in_flight"` // from its latest load report
	Stats    metrics.Summary `json:"stats"`
}

// fleetResponse is GET /api/v1/fleet. ConfigVersion is the snapshot the
// daemon currently offers; a member is in sync when its own config_version
// equals it. Total and Connected describe the whole fleet, Matched the part
// that passed the filters, and Members one page of that part.
type fleetResponse struct {
	ConfigVersion string       `json:"config_version"`
	Window        string       `json:"window"`
	Total         int          `json:"total"`
	Connected     int          `json:"connected"`
	Matched       int          `json:"matched"`
	Offset        int          `json:"offset"`
	Limit         int          `json:"limit"`
	Members       []memberJSON `json:"members"`
}

const (
	defaultFleetPage = 100
	maxFleetPage     = 1000
)

// fleetSorts are the ?sort= keys. Each orders ascending; ?order=desc
// reverses. Ties fall back to node ID, so paging is stable.
var fleetSorts = map[string]func(a, b *fleet.Member, req map[string]uint64) int{
	"node_id": func(a, b *fleet.Member, _ map[string]uint64) int { return strings.Compare(a.NodeID, b.NodeID) },
	"address": func(a, b *fleet.Member, _ map[string]uint64) int { return strings.Compare(a.Address, b.Address) },
	"envoy_version": func(a, b *fleet.Member, _ map[string]uint64) int {
		return strings.Compare(a.EnvoyVersion, b.EnvoyVersion)
	},
	"config_state": func(a, b *fleet.Member, _ map[string]uint64) int {
		return strings.Compare(string(a.ConfigState), string(b.ConfigState))
	},
	"last_seen":    func(a, b *fleet.Member, _ map[string]uint64) int { return a.LastSeen.Compare(b.LastSeen) },
	"connected_at": func(a, b *fleet.Member, _ map[string]uint64) int { return a.ConnectedAt.Compare(b.ConnectedAt) },
	"request_rate": func(a, b *fleet.Member, req map[string]uint64) int {
		switch ra, rb := req[a.NodeID], req[b.NodeID]; {
		case ra < rb:
			return -1
		case ra > rb:
			return 1
		}
		return 0
	},
}

func (s *Server) handleFleet(w http.ResponseWriter, r *http.Request) {
	win, ok := window(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	limit, offset := defaultFleetPage, 0
	for name, dst := range map[string]*int{"limit": &limit, "offset": &offset} {
		if v := q.Get(name); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				writeError(w, http.StatusBadRequest, "%s %q: want a non-negative integer", name, v)
				return
			}
			*dst = n
		}
	}
	if limit == 0 || limit > maxFleetPage {
		limit = maxFleetPage
	}
	sortKey := q.Get("sort")
	if sortKey == "" {
		sortKey = "node_id"
	}
	cmp, known := fleetSorts[sortKey]
	if !known {
		keys := make([]string, 0, len(fleetSorts))
		for k := range fleetSorts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		writeError(w, http.StatusBadRequest, "sort %q: want one of %s", sortKey, strings.Join(keys, ", "))
		return
	}
	desc := false
	switch q.Get("order") {
	case "", "asc":
	case "desc":
		desc = true
	default:
		writeError(w, http.StatusBadRequest, "order %q: want asc or desc", q.Get("order"))
		return
	}
	state := fleet.ConfigState(q.Get("state"))
	switch state {
	case "", fleet.StateAcked, fleet.StateNacked, fleet.StatePending:
	default:
		writeError(w, http.StatusBadRequest, "state %q: want acked, nacked or pending", state)
		return
	}
	var connected *bool
	if v := q.Get("connected"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "connected %q: want true or false", v)
			return
		}
		connected = &b
	}
	needle := strings.ToLower(q.Get("q"))

	all := s.fleet.List()
	counts := countFleet(all)
	matched := all[:0:0]
	for _, m := range all {
		if state != "" && m.ConfigState != state {
			continue
		}
		if connected != nil && m.Connected != *connected {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(m.NodeID), needle) &&
			!strings.Contains(strings.ToLower(m.Address), needle) && !strings.Contains(strings.ToLower(m.EnvoyVersion), needle) {
			continue
		}
		matched = append(matched, m)
	}
	var requests map[string]uint64
	if sortKey == "request_rate" {
		requests = s.agg.MemberRequests(win)
	}
	sort.SliceStable(matched, func(i, j int) bool {
		c := cmp(&matched[i], &matched[j], requests)
		if desc {
			c = -c
		}
		if c != 0 {
			return c < 0
		}
		return matched[i].NodeID < matched[j].NodeID
	})

	resp := fleetResponse{
		ConfigVersion: s.xds.Version(), Window: win.String(),
		Total: counts.Total, Connected: counts.Connected, Matched: len(matched),
		Offset: offset, Limit: limit, Members: []memberJSON{},
	}
	if offset < len(matched) {
		page := matched[offset:]
		if len(page) > limit {
			page = page[:limit]
		}
		// Percentiles are worked out for the page only, not the fleet.
		for _, m := range page {
			st := s.agg.Member(m.NodeID, win)
			resp.Members = append(resp.Members, memberJSON{Member: m, InFlight: st.InFlight, Stats: st.Summary})
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

type memberDetail struct {
	fleet.Member
	InFlight uint64                     `json:"in_flight"`
	Windows  map[string]metrics.Summary `json:"windows"`
}

func (s *Server) handleFleetMember(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("node")
	m, ok := s.fleet.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "no Envoy with node ID %q", id)
		return
	}
	out := memberDetail{Member: m, Windows: map[string]metrics.Summary{}}
	for _, win := range metrics.Windows {
		st := s.agg.Member(id, win)
		out.InFlight = st.InFlight
		out.Windows[win.String()] = st.Summary
	}
	writeJSON(w, http.StatusOK, out)
}

// handleEvents is the live feed for the UIs: the one-minute overview as a
// server-sent event, at once and then every ?interval= (default 2s).
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	interval := 2 * time.Second
	if v := r.URL.Query().Get("interval"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 100*time.Millisecond || d > time.Minute {
			writeError(w, http.StatusBadRequest, "interval %q: want a duration from 100ms to 1m", v)
			return
		}
		interval = d
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming is not supported on this connection")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // ask reverse proxies not to hold events back
	w.WriteHeader(http.StatusOK)

	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		data, err := json.Marshal(s.overview(metrics.Window1m))
		if err != nil {
			return
		}
		if _, err := fmt.Fprintf(w, "event: overview\ndata: %s\n\n", data); err != nil {
			return
		}
		flusher.Flush()
		select {
		case <-tick.C:
		case <-r.Context().Done():
			return
		case <-s.ctx.Done(): // daemon shutting down: let Shutdown finish
			return
		}
	}
}
