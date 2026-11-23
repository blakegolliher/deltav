package server

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"net"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"deltav/internal/limit"
	"deltav/internal/ratelimit"
	"deltav/internal/state"
	"deltav/internal/vast"
	"deltav/internal/xds"
)

const (
	// DefaultVASTPollInterval is how often each VAST cluster is read: four
	// GETs per poll.
	DefaultVASTPollInterval = 10 * time.Second
	// emptyHold is how many polls in a row must find a target without
	// endpoints before that is believed. A pool does not usually lose
	// every VIP at once; one odd answer should not take a target down,
	// and Envoy's own health checks cover endpoints that are really gone.
	emptyHold = 3
	// pollTimeout bounds one poll of one cluster.
	pollTimeout = 45 * time.Second
	// pollParallel is how many clusters are read at once.
	pollParallel = 4
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
	// DNS is the cluster's DNS settings, and DNSErr why the latest poll
	// could not read them ("" if it could). They are read apart from the
	// rest and their failure fails nothing: DNS then stays as last read.
	// DNSRead is whether they have been read at all in this process; until
	// they have, the suffixes restored from the cache stay.
	DNS     []vast.DNS
	DNSErr  string
	DNSRead bool
}

// hosts is where VAST would find a bucket in a host name: under the
// cluster's DNS suffix, with or without a pool's own label before it.
func (v *clusterView) hosts() ratelimit.Hosts {
	var h ratelimit.Hosts
	for _, d := range v.DNS {
		if d.DomainSuffix != "" {
			h.Suffixes = append(h.Suffixes, d.DomainSuffix)
		}
	}
	for _, p := range v.Pools {
		if p.DomainName != "" {
			h.Pools = append(h.Pools, p.DomainName)
		}
	}
	return h
}

// targetStatus says how a VAST target resolved at the last poll.
type targetStatus struct {
	VIPs    int `json:"vips"`    // VIPs in the pool
	Healthy int `json:"healthy"` // of those, on a healthy CNode
	// Drained counts VIPs on a healthy CNode that are withheld because an
	// operator drained that CNode.
	Drained int `json:"drained,omitempty"`
	// Unusable counts VIPs left out because VAST gave an address for them
	// that is not an IP address.
	Unusable int    `json:"unusable,omitempty"`
	Problem  string `json:"problem,omitempty"` // why it could not be resolved
	// BucketHosts are the host names under which a request's bucket is
	// read from its host name and not its path, as "*.<suffix>": rate
	// rules read requests as VAST does. BucketHostsProblem says why they
	// are not known, if the cluster's DNS settings could not be read; rules
	// about buckets then miss requests that name the bucket in the host.
	BucketHosts        []string `json:"bucket_hosts,omitempty"`
	BucketHostsProblem string   `json:"bucket_hosts_problem,omitempty"`
	// Held is true while the endpoints served are the last known ones and
	// not what the latest poll said (or there has been no poll yet).
	Held  bool         `json:"held,omitempty"`
	Limit *limitStatus `json:"limit,omitempty"`
}

// limitStatus is a target's per-CNode connection limit and, in enforce
// mode, how it is being divided among Envoys.
type limitStatus struct {
	PerCNode uint32 `json:"per_cnode"`
	Mode     string `json:"mode"`
	// Share is the connections each Envoy may open to each VIP, as now
	// served; 0 in alert mode, where nothing is enforced.
	Share uint32 `json:"share"`
	// Computed is the share worked out at the last poll. It differs from
	// Share while a change is held back so that Envoys are not made to
	// reconnect their pools for every Envoy that joins or leaves.
	Computed        uint32 `json:"computed"`
	Envoys          int    `json:"envoys"`             // connected Envoys the limit is divided among
	MaxVIPsPerCNode int    `json:"max_vips_per_cnode"` // most VIPs of this target on one CNode
	// Workers is the most worker threads any connected Envoy declares (at
	// least 1). An Envoy can hold Workers - 1 connections to a VIP beyond
	// its share, so the share is reduced by that much.
	Workers int `json:"workers"`
	// Undeclared counts connected Envoys that do not say how many workers
	// they run. Each of those can exceed its share by its own, unknown,
	// number of workers less one.
	Undeclared int `json:"undeclared_workers"`
	// Holds is false when no share can keep this fleet within the limit:
	// Envoys × VIPs on one CNode × workers is more than the limit, and no
	// Envoy can be given less than 1. Enforce mode only.
	Holds bool `json:"holds"`
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

	pollMu  sync.Mutex // one poll at a time
	applyMu sync.Mutex // one apply at a time: compute and publish together

	// The cache last saved, and whether the latest save failed. Under
	// applyMu. A save follows this and not the config: a config already
	// published is not published again, but a failed save must be retried.
	saved       state.Cache
	saveFailing bool

	mu       sync.Mutex
	apis     map[string]apiEntry
	views    map[string]*clusterView
	gen      map[string]uint64          // per cluster: moved on by attach and detach; see commit
	served   map[string][]xds.Endpoint  // per VAST target: endpoints now served
	hosts    map[string]ratelimit.Hosts // per VAST target: where a bucket is in a host name
	empty    map[string]int             // per VAST target: polls in a row without endpoints
	emptyAt  map[string]time.Time       // per VAST target: the poll last counted in empty
	status   map[string]targetStatus
	debounce map[string]*limit.Debouncer // per VAST target: the enforced share
	now      func() time.Time

	// How a change of share is held back; see limit.Debouncer.
	minChange   float64
	minInterval time.Duration
}

func newPoller(s *Server, interval time.Duration) *poller {
	if interval <= 0 {
		interval = DefaultVASTPollInterval
	}
	p := &poller{
		s: s, interval: interval,
		apis: map[string]apiEntry{}, views: map[string]*clusterView{}, gen: map[string]uint64{},
		served: map[string][]xds.Endpoint{}, hosts: map[string]ratelimit.Hosts{}, empty: map[string]int{}, emptyAt: map[string]time.Time{}, status: map[string]targetStatus{},
		debounce: map[string]*limit.Debouncer{}, now: time.Now,
		minChange: s.cfg.LimitMinChange, minInterval: s.cfg.LimitMinInterval,
	}
	if p.minChange <= 0 {
		p.minChange = limit.DefaultMinChange
	}
	if p.minInterval <= 0 {
		p.minInterval = limit.DefaultMinInterval
	}
	p.newAPI = func(c state.Cluster) (vast.API, error) {
		return vast.New(vast.Options{
			Host: c.Host, User: c.User, Password: c.Password, Tenant: c.Tenant, Insecure: c.Insecure,
			Timeout: 15 * time.Second,
		})
	}
	// What was served before the last shutdown, so that a restarted daemon
	// offers Envoys the same config at once: the endpoints, and any
	// enforced share. Without the share the first snapshot would be worked
	// out for a fleet of none, and loosen every limit until the fleet had
	// reconnected and the debounce interval had passed.
	cache, err := s.store.LoadCache()
	if err != nil {
		s.log.Warn("cannot read the cache of what was served before the restart; targets wait for their first poll", "error", err)
	} else {
		p.saved = cache // what is on disk; an unreadable one is replaced at the first apply
	}
	for name, eps := range cache.Endpoints {
		// [] is a target served with no endpoints: it is served again, so
		// that its listener stays. A target not in the cache is not served.
		served := make([]xds.Endpoint, 0, len(eps))
		for _, e := range eps {
			served = append(served, xds.Endpoint{IP: e.IP, Port: e.Port, CNode: e.CNode})
		}
		p.served[name] = served
	}
	for name, h := range cache.Hosts {
		p.hosts[name] = h
	}
	for name, share := range cache.Shares {
		d := &limit.Debouncer{MinChange: p.minChange, MinInterval: p.minInterval}
		d.Restore(share, p.now())
		p.debounce[name] = d
	}
	return p
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
		}
	}
}

// api returns the client for a cluster, rebuilt if its settings changed.
// It is kept only if the cluster is still the one of generation gen.
func (p *poller) api(c state.Cluster, gen uint64) (vast.API, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.apis[c.Name]; ok && e.cfg == c {
		return e.api, nil
	}
	api, err := p.newAPI(c)
	if err != nil {
		return nil, err
	}
	if p.gen[c.Name] == gen {
		p.apis[c.Name] = apiEntry{cfg: c, api: api}
	}
	return api, nil
}

// fetch reads one cluster: its CNodes, pools and VIPs, and its DNS settings.
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
	view := &clusterView{CNodes: cnodes, Pools: pools, VIPs: vips}
	// Last, and not fatal: endpoints do not depend on it, and a user who
	// may read pools may not be allowed to read this.
	if view.DNS, err = api.ListDNS(ctx); err != nil {
		view.DNS, view.DNSErr = nil, err.Error()
	} else {
		view.DNSRead = true
	}
	return view, nil
}

// pollOnce reads every cluster and applies the result. It returns once
// every cluster has answered or failed.
func (p *poller) pollOnce(ctx context.Context) {
	p.pollMu.Lock()
	defer p.pollMu.Unlock()
	// The generations before the clusters: a cluster attached or detached
	// from here on has a new one, and what this poll read for it is dropped.
	p.mu.Lock()
	gen := maps.Clone(p.gen)
	p.mu.Unlock()
	// Clusters are read side by side, and each answer is applied as it
	// comes: one cluster slow to answer must not hold back what another
	// said. apply is serialized and works from all views as they are.
	sem := make(chan struct{}, pollParallel)
	var wg sync.WaitGroup
	for _, c := range p.s.store.Clusters() {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			var view *clusterView
			api, err := p.api(c, gen[c.Name])
			if err == nil {
				view, err = fetch(ctx, api)
			}
			if ctx.Err() != nil {
				return // shutting down
			}
			if p.commit(c.Name, gen[c.Name], view, err) {
				p.apply(ctx)
			}
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return
	}
	// Against the clusters there are now, not those this poll read: one
	// attached meanwhile has the view its attach stored.
	p.mu.Lock()
	known := map[string]bool{}
	for _, c := range p.s.store.Clusters() {
		known[c.Name] = true
	}
	for name := range p.views {
		if !known[name] {
			delete(p.views, name)
		}
	}
	for name := range p.apis {
		if !known[name] {
			delete(p.apis, name)
		}
	}
	p.mu.Unlock()
	p.apply(ctx)
}

// commit stores what a poll read of a cluster, unless the cluster was
// attached or detached since the read began (its generation moved on): a
// late answer from a detached cluster must not land on a new one of the
// same name, nor bring a detached one back. Generations are never deleted,
// so a re-attach cannot reuse one. It says whether it stored the result.
func (p *poller) commit(cluster string, gen uint64, view *clusterView, err error) bool {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.gen[cluster] != gen {
		return false
	}
	old := p.views[cluster]
	switch {
	case err == nil:
		view.At = now
		if view.DNSErr != "" {
			if old != nil {
				view.DNS, view.DNSRead = old.DNS, old.DNSRead
			}
			if old == nil || old.DNSErr == "" {
				p.s.log.Warn("cannot read the cluster's DNS settings; rate rules will not see a bucket named in a host name unless they were read before",
					"cluster", cluster, "error", view.DNSErr)
			}
		}
		p.views[cluster] = view
		if old != nil && old.Err != "" {
			p.s.log.Info("VAST cluster reachable again", "cluster", cluster)
		}
	case old != nil:
		if old.Err == "" {
			p.s.log.Warn("VAST poll failed; keeping the last known endpoints", "cluster", cluster, "error", err)
		}
		old.Err, old.ErrAt = err.Error(), now
	default:
		p.s.log.Warn("VAST poll failed", "cluster", cluster, "error", err)
		p.views[cluster] = &clusterView{Err: err.Error(), ErrAt: now}
	}
	return true
}

// resolve finds the endpoints of a target in a cluster view: the VIPs of
// its pool that sit on a healthy CNode that is not drained, sorted by
// address. drained holds the names of the cluster's drained CNodes.
func resolve(t state.Target, v *clusterView, drained map[string]bool) ([]xds.Endpoint, targetStatus) {
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
		// An address Envoy could not be given is left out, and the rest
		// of the pool served. Passed on, it would fail the whole snapshot:
		// no target could change, nor any rule, until VAST stopped saying it.
		if net.ParseIP(vip.IP) == nil {
			st.Unusable++
			continue
		}
		// A VIP on a CNode that is not in service, or on one the cluster
		// did not list, is left out.
		if c, ok := cnodes[vip.CNode]; ok && c.Healthy() {
			st.Healthy++
			if drained[vip.CNode] {
				st.Drained++
				continue
			}
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
	drained := map[string]map[string]bool{} // cluster -> CNode names
	for _, d := range p.s.store.Drained() {
		if drained[d.Cluster] == nil {
			drained[d.Cluster] = map[string]bool{}
		}
		drained[d.Cluster][d.CNode] = true
	}
	envoys := p.s.fleet.Connected()
	workers, undeclared := p.s.fleet.Workers()
	now := p.now()
	known := map[string]bool{}
	for _, t := range targets {
		known[t.Name] = true
		if t.TLS != nil {
			// A target stored by an earlier build may ask for what Envoy
			// cannot do. Served, it would be rejected by every Envoy; it is
			// left out, with the reason, and the other targets go on.
			if err := (&xds.TLSSpec{ServerName: t.TLS.ServerName, CAFile: t.TLS.CAFile, Insecure: t.TLS.Insecure}).Validate(); err != nil {
				p.status[t.Name] = targetStatus{Problem: "not served: " + err.Error() + "; remove the target and add it again"}
				delete(p.served, t.Name)
				continue
			}
		}
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
			hosts := view.hosts()
			if !view.DNSRead {
				// No DNS read yet: keep the suffixes the cache restored
				// rather than none. Pools were read; they are taken.
				hosts.Suffixes = p.hosts[t.Name].Suffixes
			}
			p.hosts[t.Name] = hosts
			eps, st = resolve(t, view, drained[t.Cluster])
			if view.DNSErr != "" {
				st.BucketHostsProblem = "the cluster's DNS settings could not be read: " + view.DNSErr
			}
			switch {
			case len(eps) > 0:
				p.empty[t.Name] = 0
			case st.Problem == "" && st.Drained > 0 && st.Drained == st.Healthy:
				// Emptied by draining: that is an operator's decision,
				// not an odd answer from VAST. It takes effect now.
				p.empty[t.Name] = 0
			default:
				// Count polls, not calls: an operator's command also brings
				// us here, any number of times between two polls, and each
				// sees the same answer from VAST.
				if !view.At.Equal(p.emptyAt[t.Name]) {
					p.empty[t.Name]++
					p.emptyAt[t.Name] = view.At
				}
				if had && len(prev) > 0 && p.empty[t.Name] < emptyHold {
					eps, st.Held = prev, true
				}
			}
		}
		hosts := p.hosts[t.Name]
		for _, suffix := range hosts.Suffixes {
			st.BucketHosts = append(st.BucketHosts, "*."+strings.ToLower(strings.Trim(suffix, ".")))
		}
		spec := xds.TargetSpec{Name: t.Name, ListenPort: t.ListenPort, Endpoints: eps, AccessLog: true, S3Hosts: hosts}
		if t.Limit != nil {
			ls := &limitStatus{PerCNode: t.Limit.PerCNode, Mode: t.Limit.Mode, Envoys: envoys, Workers: max(workers, 1), Undeclared: undeclared, Holds: true}
			perCNode := map[string]int{}
			for _, e := range eps {
				perCNode[e.CNode]++
				ls.MaxVIPsPerCNode = max(ls.MaxVIPsPerCNode, perCNode[e.CNode])
			}
			want := uint32(0)
			if t.Limit.Mode == state.ModeEnforce {
				ls.Computed = limit.Share(t.Limit.PerCNode, envoys, ls.MaxVIPsPerCNode, workers)
				ls.Holds = limit.Holds(t.Limit.PerCNode, envoys, ls.MaxVIPsPerCNode, workers)
				want = ls.Computed
			}
			d := p.debounce[t.Name]
			if d == nil {
				d = &limit.Debouncer{MinChange: p.minChange, MinInterval: p.minInterval}
				p.debounce[t.Name] = d
			}
			ls.Share, _ = d.Next(now, want)
			spec.PerHostLimit = ls.Share
			st.Limit = ls
		} else {
			delete(p.debounce, t.Name) // also drops a share restored for a limit since removed
		}
		p.served[t.Name], p.status[t.Name] = eps, st
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
	for name := range p.hosts {
		if !known[name] {
			delete(p.hosts, name)
		}
	}
	for name := range p.status {
		if !known[name] {
			delete(p.status, name)
			delete(p.empty, name)
			delete(p.emptyAt, name)
			delete(p.debounce, name)
		}
	}
	cache := state.Cache{Endpoints: make(map[string][]state.Endpoint, len(p.served)), Shares: map[string]uint32{}, Hosts: map[string]ratelimit.Hosts{}}
	for name, h := range p.hosts {
		if len(h.Suffixes) > 0 {
			cache.Hosts[name] = h
		}
	}
	for name, eps := range p.served {
		// Every target served, even with no endpoints (see newPoller).
		cached := make([]state.Endpoint, 0, len(eps))
		for _, e := range eps {
			cached = append(cached, state.Endpoint{IP: e.IP, Port: e.Port, CNode: e.CNode})
		}
		cache.Endpoints[name] = cached
	}
	for name, d := range p.debounce {
		if d.Current() > 0 {
			cache.Shares[name] = d.Current()
		}
	}
	p.mu.Unlock()

	// Request-rate rules apply to every target alike.
	rules := p.s.store.Rules()
	for i := range specs {
		specs[i].RateLimits = rules
	}
	version, changed, err = p.s.xds.Update(ctx, specs)
	if err != nil {
		p.s.log.Error("cannot serve the targets", "error", err)
		return "", false, err
	}
	if changed {
		p.s.log.Info("targets updated", "targets", len(specs), "config_version", version)
	}
	// Saved when it differs from what was last saved, not only when the
	// config changed: a failed save is retried at every apply until one
	// works, and hosts with no rule to render them change no config.
	if changed || !reflect.DeepEqual(cache, p.saved) {
		if err := p.s.store.SaveCache(cache); err != nil {
			// Once per config, not at every retry.
			if !p.saveFailing || changed {
				p.s.log.Warn("cannot save the cache of what is served; a restart while VAST cannot be read would not serve it", "error", err)
			}
			p.saveFailing = true
		} else {
			if p.saveFailing {
				p.s.log.Info("the cache of what is served is saved again")
			}
			p.saved, p.saveFailing = cache, false
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

// limitChanged makes the next apply push a target's share at once: an
// operator changing a limit should not wait out the debounce.
func (p *poller) limitChanged(target string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d := p.debounce[target]; d != nil {
		d.Reset()
	}
}

func (p *poller) targetStatus(name string) (targetStatus, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.status[name]
	return st, ok
}
