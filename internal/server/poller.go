package server

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"deltav/internal/state"
	"deltav/internal/vast"
	"deltav/internal/xds"
)

const (
	// DefaultVASTPollInterval is how often each VAST cluster is read: three
	// GETs per poll.
	DefaultVASTPollInterval = 10 * time.Second
	// emptyHold is how many polls in a row must find a target without
	// endpoints before that is believed. A pool does not usually lose
	// every VIP at once; one odd answer should not take a target down,
	// and Envoy's own health checks cover endpoints that are really gone.
	emptyHold = 3
	// pollTimeout bounds one poll of one cluster.
	pollTimeout = 45 * time.Second
)

// clusterView is what the last successful poll of a cluster returned, plus
// the outcome of the latest poll. A failed poll leaves the data in place:
// an unreachable VAST must not change what Envoys are served.
type clusterView struct {
	At     time.Time // last successful poll; zero if there has been none
	Err    string    // the latest poll's error, "" if it succeeded
	ErrAt  time.Time
	CNodes []vast.CNode
	Pools  []vast.VIPPool
	VIPs   []vast.VIP
}

// targetStatus says how a VAST target resolved at the last poll.
type targetStatus struct {
	VIPs    int    `json:"vips"`              // VIPs in the pool
	Healthy int    `json:"healthy"`           // of those, on a healthy CNode
	Problem string `json:"problem,omitempty"` // why it could not be resolved
	// Held is true while the endpoints served are the last known ones and
	// not what the latest poll said (or there has been no poll yet).
	Held bool `json:"held,omitempty"`
}

type apiEntry struct {
	cfg state.Cluster
	api vast.API
}

// poller reads every attached VAST cluster and turns targets into the
// endpoint lists served to Envoys.
type poller struct {
	s        *Server
	interval time.Duration
	newAPI   func(state.Cluster) (vast.API, error)
	kick     chan struct{}

	pollMu  sync.Mutex // one poll at a time
	applyMu sync.Mutex // one apply at a time: compute and publish together

	mu     sync.Mutex
	apis   map[string]apiEntry
	views  map[string]*clusterView
	served map[string][]xds.Endpoint // per VAST target: endpoints now served
	empty  map[string]int            // per VAST target: polls in a row without endpoints
	status map[string]targetStatus
}

func newPoller(s *Server, interval time.Duration) *poller {
	if interval <= 0 {
		interval = DefaultVASTPollInterval
	}
	p := &poller{
		s: s, interval: interval, kick: make(chan struct{}, 1),
		apis: map[string]apiEntry{}, views: map[string]*clusterView{},
		served: map[string][]xds.Endpoint{}, empty: map[string]int{}, status: map[string]targetStatus{},
	}
	p.newAPI = func(c state.Cluster) (vast.API, error) {
		return vast.New(vast.Options{
			Host: c.Host, User: c.User, Password: c.Password, Tenant: c.Tenant, Insecure: c.Insecure,
			Timeout: 15 * time.Second,
		})
	}
	// Endpoints served before the last shutdown, so that a restarted
	// daemon offers Envoys the same config at once.
	for name, eps := range s.store.LoadEndpoints() {
		for _, e := range eps {
			p.served[name] = append(p.served[name], xds.Endpoint{IP: e.IP, Port: e.Port, CNode: e.CNode})
		}
	}
	return p
}

// poke asks for a poll now instead of at the next tick.
func (p *poller) poke() {
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

func (p *poller) run(ctx context.Context) {
	tick := time.NewTicker(p.interval)
	defer tick.Stop()
	for {
		p.pollOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-p.kick:
		}
	}
}

// api returns the client for a cluster, rebuilt if its settings changed.
func (p *poller) api(c state.Cluster) (vast.API, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.apis[c.Name]; ok && e.cfg == c {
		return e.api, nil
	}
	api, err := p.newAPI(c)
	if err != nil {
		return nil, err
	}
	p.apis[c.Name] = apiEntry{cfg: c, api: api}
	return api, nil
}

// fetch reads one cluster: its CNodes, pools and VIPs.
func fetch(ctx context.Context, api vast.API) (*clusterView, error) {
	ctx, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()
	cnodes, err := api.ListCNodes(ctx)
	if err != nil {
		return nil, err
	}
	pools, err := api.ListVIPPools(ctx)
	if err != nil {
		return nil, err
	}
	vips, err := api.ListVIPs(ctx)
	if err != nil {
		return nil, err
	}
	return &clusterView{CNodes: cnodes, Pools: pools, VIPs: vips}, nil
}

// pollOnce reads every cluster and applies the result.
func (p *poller) pollOnce(ctx context.Context) {
	p.pollMu.Lock()
	defer p.pollMu.Unlock()
	clusters := p.s.store.Clusters()
	known := map[string]bool{}
	for _, c := range clusters {
		known[c.Name] = true
		var view *clusterView
		api, err := p.api(c)
		if err == nil {
			view, err = fetch(ctx, api)
		}
		if ctx.Err() != nil {
			return // shutting down
		}
		now := time.Now()
		p.mu.Lock()
		old := p.views[c.Name]
		switch {
		case err == nil:
			view.At = now
			p.views[c.Name] = view
			if old != nil && old.Err != "" {
				p.s.log.Info("VAST cluster reachable again", "cluster", c.Name)
			}
		case old != nil:
			if old.Err == "" {
				p.s.log.Warn("VAST poll failed; keeping the last known endpoints", "cluster", c.Name, "error", err)
			}
			old.Err, old.ErrAt = err.Error(), now
		default:
			p.s.log.Warn("VAST poll failed", "cluster", c.Name, "error", err)
			p.views[c.Name] = &clusterView{Err: err.Error(), ErrAt: now}
		}
		p.mu.Unlock()
	}
	p.mu.Lock()
	for name := range p.views {
		if !known[name] {
			delete(p.views, name)
			delete(p.apis, name)
		}
	}
	p.mu.Unlock()
	p.apply(ctx)
}

// resolve finds the endpoints of a target in a cluster view: the VIPs of
// its pool that sit on a healthy CNode, sorted by address.
func resolve(t state.Target, v *clusterView) ([]xds.Endpoint, targetStatus) {
	var st targetStatus
	var pools []vast.VIPPool
	for _, p := range v.Pools {
		if p.Name == t.Pool && (t.Tenant == "" || p.TenantName == t.Tenant) {
			pools = append(pools, p)
		}
	}
	switch {
	case len(pools) == 0 && t.Tenant != "":
		st.Problem = fmt.Sprintf("cluster %q has no VIP pool %q in tenant %q", t.Cluster, t.Pool, t.Tenant)
		return nil, st
	case len(pools) == 0:
		st.Problem = fmt.Sprintf("cluster %q has no VIP pool %q", t.Cluster, t.Pool)
		return nil, st
	case len(pools) > 1:
		tenants := make([]string, len(pools))
		for i, p := range pools {
			tenants[i] = p.TenantName
		}
		st.Problem = fmt.Sprintf("cluster %q has %d VIP pools named %q (tenants %s): set the target's tenant",
			t.Cluster, len(pools), t.Pool, strings.Join(tenants, ", "))
		return nil, st
	}
	pool := pools[0]
	cnodes := make(map[string]vast.CNode, len(v.CNodes))
	for _, c := range v.CNodes {
		cnodes[c.Name] = c
	}
	var eps []xds.Endpoint
	for _, vip := range v.VIPs {
		if vip.VIPPool != pool.Name || vip.TenantName != pool.TenantName {
			continue
		}
		st.VIPs++
		// A VIP on a CNode that is not in service, or on one the cluster
		// did not list, is left out.
		if c, ok := cnodes[vip.CNode]; ok && c.Healthy() {
			st.Healthy++
			eps = append(eps, xds.Endpoint{IP: vip.IP, Port: t.Port, CNode: vip.CNode})
		}
	}
	if !pool.Enabled {
		st.Problem = fmt.Sprintf("VIP pool %q is disabled", t.Pool)
		return nil, st
	}
	sort.Slice(eps, func(i, j int) bool {
		return bytes.Compare(net.ParseIP(eps[i].IP).To16(), net.ParseIP(eps[j].IP).To16()) < 0
	})
	return eps, st
}

// apply turns the static targets, the stored targets and the cluster views
// into the snapshot served to Envoys, and remembers what it served. It is
// the only caller of xds.Server.Update once the daemon is built.
func (p *poller) apply(ctx context.Context) (version string, changed bool, err error) {
	p.applyMu.Lock()
	defer p.applyMu.Unlock()
	p.mu.Lock()
	specs := p.s.staticSpecs()
	targets := p.s.store.Targets()
	known := map[string]bool{}
	for _, t := range targets {
		known[t.Name] = true
		prev, had := p.served[t.Name]
		view := p.views[t.Cluster]
		var (
			eps []xds.Endpoint
			st  targetStatus
		)
		switch {
		case view == nil || view.At.IsZero():
			// Not read yet (or never successfully). Serve what was served
			// before the restart, if anything; otherwise wait.
			st.Problem = fmt.Sprintf("waiting for the first successful poll of cluster %q", t.Cluster)
			if view != nil && view.Err != "" {
				st.Problem += ": " + view.Err
			}
			if !had {
				p.status[t.Name] = st
				continue
			}
			eps, st.Held = prev, true
		default:
			eps, st = resolve(t, view)
			if len(eps) > 0 {
				p.empty[t.Name] = 0
			} else if p.empty[t.Name]++; had && len(prev) > 0 && p.empty[t.Name] < emptyHold {
				eps, st.Held = prev, true
			}
		}
		p.served[t.Name], p.status[t.Name] = eps, st
		spec := xds.TargetSpec{Name: t.Name, ListenPort: t.ListenPort, Endpoints: eps, AccessLog: true}
		if t.TLS != nil {
			spec.TLS = &xds.TLSSpec{ServerName: t.TLS.ServerName, CAFile: t.TLS.CAFile, Insecure: t.TLS.Insecure}
		}
		specs = append(specs, spec)
	}
	for name := range p.served {
		if !known[name] {
			delete(p.served, name)
		}
	}
	for name := range p.status {
		if !known[name] {
			delete(p.status, name)
			delete(p.empty, name)
		}
	}
	cache := make(map[string][]state.Endpoint, len(p.served))
	for name, eps := range p.served {
		for _, e := range eps {
			cache[name] = append(cache[name], state.Endpoint{IP: e.IP, Port: e.Port, CNode: e.CNode})
		}
	}
	p.mu.Unlock()

	version, changed, err = p.s.xds.Update(ctx, specs)
	if err != nil {
		p.s.log.Error("cannot serve the targets", "error", err)
		return "", false, err
	}
	if changed {
		p.s.log.Info("targets updated", "targets", len(specs), "config_version", version)
		if err := p.s.store.SaveEndpoints(cache); err != nil {
			p.s.log.Warn("cannot save the endpoint cache", "error", err)
		}
	}
	return version, changed, nil
}

// view returns a copy of a cluster's view, or nil.
func (p *poller) view(cluster string) *clusterView {
	p.mu.Lock()
	defer p.mu.Unlock()
	if v := p.views[cluster]; v != nil {
		c := *v
		return &c
	}
	return nil
}

func (p *poller) targetStatus(name string) (targetStatus, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.status[name]
	return st, ok
}
