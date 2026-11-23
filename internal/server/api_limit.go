package server

import (
	"net"
	"net/http"
	"sort"
	"strconv"

	"deltav/internal/metrics"
	"deltav/internal/state"
)

// Per-CNode connection limits and CNode drain.
//
//	PUT    /api/v1/targets/{name}/limit                     set a target's limit and mode
//	DELETE /api/v1/targets/{name}/limit                     remove it
//	POST   /api/v1/vast/{cluster}/cnodes/{cnode}/drain      withhold a CNode's VIPs from Envoys
//	DELETE /api/v1/vast/{cluster}/cnodes/{cnode}/drain      serve them again
//
// "Connections" here are requests in flight, which is what Envoy's load
// reports carry: S3 over HTTP/1.1 holds one request per connection, so it
// is the number of busy connections. Idle keep-alive connections are not
// counted, and an Envoy's first reporting interval for a target is missing
// (DESIGN.md, amendments 7 and 20).

// cnodeUsage is one CNode's standing against one target's limit.
type cnodeUsage struct {
	Target   string `json:"target"`
	Cluster  string `json:"cluster"`
	CNode    string `json:"cnode"`
	InFlight uint64 `json:"in_flight"` // the fleet's requests in flight to this target's VIPs on the CNode
	PerCNode uint32 `json:"per_cnode"`
	Mode     string `json:"mode"`
	Over     bool   `json:"over"` // in flight exceeds the limit
}

// limitUsage compares, for every target with a limit, the fleet's requests
// in flight to each CNode with that limit. It is the alert: in both modes
// it reports, and in alert mode it is all that happens. Rows are sorted by
// target and CNode.
func (s *Server) limitUsage() []cnodeUsage {
	limits := map[string]state.Target{}
	for _, t := range s.store.Targets() {
		if t.Limit != nil {
			limits[t.Name] = t
		}
	}
	if len(limits) == 0 {
		return nil
	}
	inFlight := s.agg.InFlight()
	type key struct{ target, cnode string }
	sums := map[key]uint64{}
	for _, served := range s.xds.Targets() {
		t, ok := limits[served.Name]
		if !ok {
			continue
		}
		for _, e := range served.Endpoints {
			ek := metrics.EndpointKey{Target: t.Name, Endpoint: net.JoinHostPort(e.IP, strconv.FormatUint(uint64(e.Port), 10))}
			sums[key{t.Name, e.CNode}] += inFlight[ek]
		}
	}
	out := make([]cnodeUsage, 0, len(sums))
	for k, n := range sums {
		t := limits[k.target]
		out = append(out, cnodeUsage{
			Target: k.target, Cluster: t.Cluster, CNode: k.cnode, InFlight: n,
			PerCNode: t.Limit.PerCNode, Mode: t.Limit.Mode, Over: n > uint64(t.Limit.PerCNode),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Target != out[j].Target {
			return out[i].Target < out[j].Target
		}
		return out[i].CNode < out[j].CNode
	})
	return out
}

// overLimit returns the rows of usage that are over, for one target ("" for all).
func overLimit(usage []cnodeUsage, target string) []cnodeUsage {
	out := []cnodeUsage{}
	for _, u := range usage {
		if u.Over && (target == "" || u.Target == target) {
			out = append(out, u)
		}
	}
	return out
}

func (s *Server) respondTarget(w http.ResponseWriter, name string) {
	for _, t := range s.targetList() {
		if t.Name == name {
			writeJSON(w, http.StatusOK, t)
			return
		}
	}
	writeError(w, http.StatusNotFound, "no target %q", name)
}

func (s *Server) handleLimitSet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var in struct {
		PerCNode uint32 `json:"per_cnode"`
		Mode     string `json:"mode"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if err := s.store.SetTargetLimit(name, &state.Limit{PerCNode: in.PerCNode, Mode: in.Mode}); err != nil {
		storeError(w, err)
		return
	}
	s.poller.limitChanged(name)
	_, _, _ = s.poller.apply(s.ctx)
	// What was stored: the store reads no mode as alert, and 0 as none.
	for _, t := range s.store.Targets() {
		switch {
		case t.Name != name:
		case t.Limit == nil:
			s.log.Info("limit removed", "target", name)
		default:
			s.log.Info("limit set", "target", name, "per_cnode", t.Limit.PerCNode, "mode", t.Limit.Mode)
		}
	}
	s.respondTarget(w, name)
}

func (s *Server) handleLimitClear(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.store.SetTargetLimit(name, nil); err != nil {
		storeError(w, err)
		return
	}
	_, _, _ = s.poller.apply(s.ctx)
	s.log.Info("limit removed", "target", name)
	s.respondTarget(w, name)
}

func (s *Server) handleDrain(w http.ResponseWriter, r *http.Request) {
	cluster, cnode := r.PathValue("cluster"), r.PathValue("cnode")
	drain := r.Method == http.MethodPost
	if drain {
		// Only a CNode the cluster has: a misspelled name would otherwise
		// sit in the state doing nothing while the operator thinks it did.
		view := s.poller.view(cluster)
		if _, ok := s.store.Cluster(cluster); !ok {
			writeError(w, http.StatusNotFound, "no cluster %q", cluster)
			return
		}
		if view == nil || view.At.IsZero() {
			writeError(w, http.StatusServiceUnavailable, "cluster %q has not been read yet; try again shortly", cluster)
			return
		}
		found := false
		for _, n := range view.CNodes {
			found = found || n.Name == cnode
		}
		if !found {
			writeError(w, http.StatusNotFound, "cluster %q has no CNode %q", cluster, cnode)
			return
		}
	}
	if err := s.store.SetDrained(cluster, cnode, drain); err != nil {
		storeError(w, err)
		return
	}
	_, _, _ = s.poller.apply(s.ctx)
	s.log.Info("CNode drain changed", "cluster", cluster, "cnode", cnode, "drained", drain)
	w.WriteHeader(http.StatusNoContent)
}
