package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"

	"deltav/internal/metrics"
	"deltav/internal/state"
)

// VAST clusters, the targets built on them, and the CNode view.
//
//	GET    /api/v1/vast               attached clusters and the state of their polling
//	POST   /api/v1/vast               attach a cluster (it is read once first, to prove it works)
//	DELETE /api/v1/vast/{name}        detach a cluster no target uses
//	GET    /api/v1/vast/{name}/pools  its VIP pools and how their VIPs spread over CNodes
//	POST   /api/v1/targets            add a target
//	DELETE /api/v1/targets/{name}     remove a target
//	GET    /api/v1/cnodes             per-CNode rollups, with per-VIP rows beneath
//
// A cluster's password goes in with POST and never comes out again.

// storeError maps a state error to an HTTP status.
func storeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, state.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, state.ErrConflict):
		status = http.StatusConflict
	case errors.Is(err, state.ErrInvalid):
		status = http.StatusBadRequest
	}
	writeError(w, status, "%v", err)
}

// readJSON decodes a request body, rejecting fields it does not know so a
// misspelled one is an error and not silently ignored.
func readJSON(w http.ResponseWriter, r *http.Request, into any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		writeError(w, http.StatusBadRequest, "request body: %v", err)
		return false
	}
	return true
}

type clusterJSON struct {
	Name     string `json:"name"`
	Host     string `json:"host"`
	User     string `json:"user"`
	Tenant   string `json:"tenant,omitempty"`
	Insecure bool   `json:"insecure"`
	// LastPoll is the last successful read of the cluster. LastError is
	// set while the latest read failed; the data shown is then LastPoll's.
	LastPoll    *time.Time `json:"last_poll"`
	LastError   string     `json:"last_error,omitempty"`
	LastErrorAt *time.Time `json:"last_error_at,omitempty"`
	CNodes      int        `json:"cnodes"`
	Pools       int        `json:"pools"`
	VIPs        int        `json:"vips"`
}

func (s *Server) clusterJSON(c state.Cluster) clusterJSON {
	out := clusterJSON{Name: c.Name, Host: c.Host, User: c.User, Tenant: c.Tenant, Insecure: c.Insecure}
	if v := s.poller.view(c.Name); v != nil {
		if !v.At.IsZero() {
			out.LastPoll = &v.At
		}
		if v.Err != "" {
			out.LastError, out.LastErrorAt = v.Err, &v.ErrAt
		}
		out.CNodes, out.Pools, out.VIPs = len(v.CNodes), len(v.Pools), len(v.VIPs)
	}
	return out
}

func (s *Server) handleClusters(w http.ResponseWriter, _ *http.Request) {
	out := struct {
		Clusters []clusterJSON `json:"clusters"`
	}{Clusters: []clusterJSON{}}
	for _, c := range s.store.Clusters() {
		out.Clusters = append(out.Clusters, s.clusterJSON(c))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleClusterAdd(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name     string `json:"name"`
		Host     string `json:"host"`
		User     string `json:"user"`
		Password string `json:"password"`
		Tenant   string `json:"tenant"`
		Insecure bool   `json:"insecure"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	c := state.Cluster{Name: in.Name, Host: in.Host, User: in.User, Password: in.Password, Tenant: in.Tenant, Insecure: in.Insecure}
	if _, exists := s.store.Cluster(c.Name); exists {
		writeError(w, http.StatusConflict, "cluster %q already exists", c.Name)
		return
	}
	// Read the cluster once before storing it: wrong credentials or a bad
	// certificate should fail here, not quietly in the poller.
	api, err := s.poller.newAPI(c)
	if err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	view, err := fetch(r.Context(), api)
	if err != nil {
		writeError(w, http.StatusBadGateway, "cannot read cluster %q: %v", c.Name, err)
		return
	}
	if err := s.store.AddCluster(c); err != nil {
		storeError(w, err)
		return
	}
	view.At = time.Now()
	s.poller.mu.Lock()
	s.poller.views[c.Name] = view
	s.poller.apis[c.Name] = apiEntry{cfg: c, api: api}
	s.poller.gen[c.Name]++
	s.poller.mu.Unlock()
	s.log.Info("VAST cluster attached", "cluster", c.Name, "host", c.Host, "cnodes", len(view.CNodes), "pools", len(view.Pools), "vips", len(view.VIPs))
	writeJSON(w, http.StatusCreated, s.clusterJSON(c))
}

func (s *Server) handleClusterRemove(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.store.RemoveCluster(name); err != nil {
		storeError(w, err)
		return
	}
	s.poller.mu.Lock()
	delete(s.poller.views, name)
	delete(s.poller.apis, name)
	s.poller.gen[name]++
	s.poller.mu.Unlock()
	s.log.Info("VAST cluster detached", "cluster", name)
	w.WriteHeader(http.StatusNoContent)
}

type poolJSON struct {
	Name    string `json:"name"`
	Tenant  string `json:"tenant,omitempty"`
	Role    string `json:"role"`
	Enabled bool   `json:"enabled"`
	VIPs    int    `json:"vips"`
	// PerCNode is how many of the pool's VIPs each CNode hosts now. An
	// uneven spread matters for the per-CNode connection limit.
	PerCNode map[string]int `json:"per_cnode"`
}

func (s *Server) handleClusterPools(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := s.store.Cluster(name); !ok {
		writeError(w, http.StatusNotFound, "no cluster %q", name)
		return
	}
	v := s.poller.view(name)
	if v == nil || v.At.IsZero() {
		writeError(w, http.StatusServiceUnavailable, "cluster %q has not been read yet", name)
		return
	}
	type key struct{ pool, tenant string }
	index := map[key]*poolJSON{}
	out := struct {
		Pools []*poolJSON `json:"pools"`
	}{Pools: []*poolJSON{}}
	for _, p := range v.Pools {
		pj := &poolJSON{Name: p.Name, Tenant: p.TenantName, Role: p.Role, Enabled: p.Enabled, PerCNode: map[string]int{}}
		index[key{p.Name, p.TenantName}] = pj
		out.Pools = append(out.Pools, pj)
	}
	for _, vip := range v.VIPs {
		if pj := index[key{vip.VIPPool, vip.TenantName}]; pj != nil {
			pj.VIPs++
			pj.PerCNode[vip.CNode]++
		}
	}
	sort.Slice(out.Pools, func(i, j int) bool {
		if out.Pools[i].Name != out.Pools[j].Name {
			return out.Pools[i].Name < out.Pools[j].Name
		}
		return out.Pools[i].Tenant < out.Pools[j].Tenant
	})
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleTargetAdd(w http.ResponseWriter, r *http.Request) {
	var in state.Target
	if !readJSON(w, r, &in) {
		return
	}
	if _, ok := s.store.Cluster(in.Cluster); !ok {
		writeError(w, http.StatusNotFound, "no cluster %q", in.Cluster)
		return
	}
	// The pool must exist now. A target whose pool cannot be found would
	// open a listener on every client that only answers 503.
	view := s.poller.view(in.Cluster)
	if view == nil || view.At.IsZero() {
		writeError(w, http.StatusServiceUnavailable, "cluster %q has not been read yet; try again shortly", in.Cluster)
		return
	}
	if _, st := resolve(in, view, nil); st.Problem != "" {
		writeError(w, http.StatusBadRequest, "%s", st.Problem)
		return
	}
	reserved := map[uint32]string{}
	for _, t := range s.staticTargets() {
		reserved[t.ListenPort] = t.Name
		if t.Name == in.Name {
			writeError(w, http.StatusConflict, "target %q already exists (static)", in.Name)
			return
		}
	}
	if err := s.store.AddTarget(in, reserved); err != nil {
		storeError(w, err)
		return
	}
	// The daemon's context, not the request's: a client that hangs up must
	// not leave the fleet with half a push.
	if _, _, err := s.poller.apply(s.ctx); err != nil {
		_ = s.store.RemoveTarget(in.Name)
		writeError(w, http.StatusConflict, "target %q cannot be served: %v", in.Name, err)
		return
	}
	s.log.Info("target added", "target", in.Name, "cluster", in.Cluster, "pool", in.Pool)
	for _, t := range s.targetList() {
		if t.Name == in.Name {
			writeJSON(w, http.StatusCreated, t)
			return
		}
	}
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) handleTargetRemove(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.store.RemoveTarget(name); err != nil {
		storeError(w, err)
		return
	}
	_, _, _ = s.poller.apply(s.ctx)
	s.log.Info("target removed", "target", name)
	w.WriteHeader(http.StatusNoContent)
}

type cnodeJSON struct {
	Cluster      string `json:"cluster"`
	Name         string `json:"name"`
	Hostname     string `json:"hostname"`
	State        string `json:"state"`
	DisplayState string `json:"display_state"`
	Enabled      bool   `json:"enabled"`
	// Healthy is Delta-V's verdict: enabled and ACTIVE. The VIPs of a CNode
	// that is not healthy are not served to Envoys.
	Healthy bool `json:"healthy"`
	// Drained is true when an operator has withheld this CNode's VIPs from
	// Envoys. It says nothing about the CNode's health.
	Drained  bool `json:"drained"`
	IsLeader bool `json:"is_leader"`
	IsMgmt   bool `json:"is_mgmt"`
	// VIPs counts the VIPs this CNode hosts in the pools of configured
	// targets; a CNode hosts many more in pools Delta-V does not serve.
	VIPs     int             `json:"vips"`
	InFlight uint64          `json:"in_flight"`
	Stats    metrics.Summary `json:"stats"`
	// RateHistory is the request rate per second in each of the last
	// complete 10-second slots, oldest first, for a sparkline.
	RateHistory []float64       `json:"rate_history"`
	Endpoints   []endpointStats `json:"endpoints"` // one row per VIP and target
	// Limits holds, for each target with a limit that has VIPs here, this
	// CNode's requests in flight against that limit.
	Limits []cnodeUsage `json:"limits"`
}

type cnodesResponse struct {
	Window string      `json:"window"`
	CNodes []cnodeJSON `json:"cnodes"`
}

// handleCNodes joins telemetry, which is per VIP, to CNodes using the
// VIP-to-CNode map from VAST. A VIP's history follows it: after a VIP
// moves, its whole window counts toward the CNode that hosts it now.
func (s *Server) handleCNodes(w http.ResponseWriter, r *http.Request) {
	win, ok := window(w, r)
	if !ok {
		return
	}
	type cnodeKey struct{ cluster, name string }
	// Which cluster a target belongs to, and so which CNode an endpoint is
	// on: endpoint -> CNode name comes from what is served.
	clusterOf := map[string]string{}
	for _, t := range s.store.Targets() {
		clusterOf[t.Name] = t.Cluster
	}
	served := s.xds.Targets()
	cnodeOf := configuredEndpoints(served)
	group := func(k metrics.EndpointKey) (cnodeKey, bool) {
		cluster, vastTarget := clusterOf[k.Target]
		name := cnodeOf[k]
		return cnodeKey{cluster, name}, vastTarget && name != ""
	}
	byCNode := func(k metrics.EndpointKey) (string, bool) {
		ck, ok := group(k)
		return ck.cluster + "\x00" + ck.name, ok
	}
	rollup := s.agg.Rollup(win, byCNode)
	history := s.agg.RateHistory(byCNode)
	keys := make([]metrics.EndpointKey, 0, len(cnodeOf))
	for k := range cnodeOf {
		keys = append(keys, k)
	}
	rows := map[cnodeKey][]endpointStats{}
	for _, e := range s.agg.Endpoints(win, keys...) {
		if ck, ok := group(e.Key); ok {
			rows[ck] = append(rows[ck], endpointStats{
				Target: e.Key.Target, Endpoint: e.Key.Endpoint, CNode: ck.name, Configured: true,
				InFlight: e.InFlight, Stats: e.Summary, Load: e.Load,
			})
		}
	}

	resp := cnodesResponse{Window: win.String(), CNodes: []cnodeJSON{}}
	idle := s.agg.Idle(win)
	drained := map[cnodeKey]bool{}
	for _, d := range s.store.Drained() {
		drained[cnodeKey{d.Cluster, d.CNode}] = true
	}
	limits := map[cnodeKey][]cnodeUsage{}
	for _, u := range s.limitUsage() {
		ck := cnodeKey{u.Cluster, u.CNode}
		limits[ck] = append(limits[ck], u)
	}
	for _, c := range s.store.Clusters() {
		v := s.poller.view(c.Name)
		if v == nil {
			continue
		}
		// VIPs per CNode within the pools that targets use.
		pools := map[[2]string]bool{}
		for _, t := range s.store.Targets() {
			if t.Cluster != c.Name {
				continue
			}
			for _, p := range v.Pools {
				if p.Name == t.Pool && (t.Tenant == "" || p.TenantName == t.Tenant) {
					pools[[2]string{p.Name, p.TenantName}] = true
				}
			}
		}
		hosted := map[string]int{}
		for _, vip := range v.VIPs {
			if pools[[2]string{vip.VIPPool, vip.TenantName}] {
				hosted[vip.CNode]++
			}
		}
		for _, n := range v.CNodes {
			ck := cnodeKey{c.Name, n.Name}
			cj := cnodeJSON{
				Cluster: c.Name, Name: n.Name, Hostname: n.Hostname, State: n.State, DisplayState: n.DisplayState,
				Enabled: n.Enabled, Healthy: n.Healthy(), Drained: drained[ck], IsLeader: n.IsLeader, IsMgmt: n.IsMgmt,
				VIPs: hosted[n.Name], Stats: idle, Endpoints: rows[ck], Limits: limits[ck],
			}
			if cj.Limits == nil {
				cj.Limits = []cnodeUsage{}
			}
			if cj.Endpoints == nil {
				cj.Endpoints = []endpointStats{}
			}
			if cj.RateHistory = history[c.Name+"\x00"+n.Name]; cj.RateHistory == nil {
				cj.RateHistory = make([]float64, metrics.HistoryPoints)
			}
			if ts, ok := rollup[c.Name+"\x00"+n.Name]; ok {
				cj.InFlight = ts.InFlight
				if ts.Summary.WindowSeconds > 0 {
					cj.Stats = ts.Summary
				}
			}
			resp.CNodes = append(resp.CNodes, cj)
		}
	}
	sort.SliceStable(resp.CNodes, func(i, j int) bool {
		if resp.CNodes[i].Cluster != resp.CNodes[j].Cluster {
			return resp.CNodes[i].Cluster < resp.CNodes[j].Cluster
		}
		return resp.CNodes[i].Name < resp.CNodes[j].Name
	})
	writeJSON(w, http.StatusOK, resp)
}
