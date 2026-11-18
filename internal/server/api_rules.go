package server

import (
	"net/http"
	"strconv"

	"deltav/internal/ratelimit"
)

// Request-rate rules: how many requests of which kind, for whom, the fleet
// lets through. They are tollbooth's rules, kept here for a fleet of Envoys
// on the client hosts: a rule's rate is the whole fleet's, and the daemon
// divides it among the Envoys that are using it (package ratelimit).
//
//	GET    /api/v1/rules          the rules, in the order they apply, and their use
//	PUT    /api/v1/rules/{name}   add a rule, or replace the one of that name
//	DELETE /api/v1/rules/{name}   remove a rule
//	GET    /api/v1/s3-operations  the operation names a rule can use
//	GET    /api/v1/refusals       the latest requests rules refused, newest first (?limit=N)

type ruleJSON struct {
	ratelimit.Rule
	// Limit and Sentence say the rule in words.
	Limit    string `json:"limit"`
	Sentence string `json:"sentence"`
	// Status is what Envoys last reported: requests let through and
	// refused, and the busiest of the things the rule counts per.
	Status ratelimit.Status `json:"status"`
	// Holds is false while more Envoys share one count of the rule than
	// its burst: each needs a burst of at least one request, so that many
	// can pass at once (status.most_envoys) and not the rule's burst. The
	// rate is held regardless.
	Holds bool `json:"holds"`
}

type rulesResponse struct {
	Rules []ruleJSON `json:"rules"`
	// ReportIntervalSeconds is how often Envoys report, which is how
	// quickly shares follow a change in who is asking.
	ReportIntervalSeconds float64 `json:"report_interval_seconds"`
	// Overflow counts keys that were left unlimited because a rule already
	// had as many as the daemon will remember.
	Overflow uint64 `json:"overflow"`
	// Slots is how many rules can limit one request. A request that
	// matches more is limited by the first that many, in this order; a
	// blocked rule refuses wherever it comes.
	Slots int `json:"slots"`
}

func (s *Server) rules() rulesResponse {
	status, overflow := s.quota.Status()
	out := rulesResponse{Rules: []ruleJSON{}, ReportIntervalSeconds: ratelimit.ReportInterval.Seconds(), Overflow: overflow, Slots: ratelimit.Slots}
	for _, r := range s.store.Rules() {
		st := status[r.Name]
		if st.Top == nil {
			st.Top = []ratelimit.KeyStatus{}
		}
		out.Rules = append(out.Rules, ruleJSON{Rule: r, Limit: r.Limit(), Sentence: r.Sentence(), Status: st, Holds: r.Holds(st)})
	}
	return out
}

func (s *Server) handleRules(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.rules())
}

// refusalsResponse is the record of what rules have refused: each of the
// latest, one by one. Counts of all of them are in the rules' status.
type refusalsResponse struct {
	Refusals []ratelimit.Refusal `json:"refusals"`
}

func (s *Server) handleRefusals(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "limit %q: a number of refusals, 1 or more", v)
			return
		}
		limit = n
	}
	writeJSON(w, http.StatusOK, refusalsResponse{Refusals: s.quota.Recent(limit)})
}

func (s *Server) handleOperations(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string][]string{"operations": ratelimit.Operations()})
}

// rulesChanged serves the rules as they now are, to the quota service and
// to Envoys. If Envoy's config cannot be built from them the old rules go
// back. The caller holds rulesMu, so that what goes back is what was there:
// without it, a change made by someone else in between would go too.
//
// The quota service first. An Envoy that learns of a rule starts counting
// under it at once, and asks for its share within milliseconds; the service
// must know the rule by then, or it would tell the Envoy to forget the
// count.
func (s *Server) rulesChanged(w http.ResponseWriter, before []ratelimit.Rule) bool {
	s.quota.SetRules(s.store.Rules())
	if _, _, err := s.poller.apply(s.ctx); err != nil {
		_ = s.store.ReplaceRules(before)
		s.quota.SetRules(s.store.Rules())
		_, _, _ = s.poller.apply(s.ctx)
		writeError(w, http.StatusConflict, "the rules cannot be served, and were left as they were: %v", err)
		return false
	}
	return true
}

func (s *Server) handleRuleSet(w http.ResponseWriter, r *http.Request) {
	var in ratelimit.Rule
	if !readJSON(w, r, &in) {
		return
	}
	name := r.PathValue("name")
	if in.Name == "" {
		in.Name = name
	}
	if in.Name != name {
		writeError(w, http.StatusBadRequest, "the rule is named %q in the address and %q in the body", name, in.Name)
		return
	}
	s.rulesMu.Lock()
	defer s.rulesMu.Unlock()
	before := s.store.Rules()
	if err := s.store.SetRule(in); err != nil {
		storeError(w, err)
		return
	}
	if !s.rulesChanged(w, before) {
		return
	}
	s.log.Info("rate rule set", "rule", in.Name)
	for _, rule := range s.rules().Rules {
		if rule.Name == name {
			writeJSON(w, http.StatusOK, rule)
			return
		}
	}
}

func (s *Server) handleRuleRemove(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	s.rulesMu.Lock()
	defer s.rulesMu.Unlock()
	before := s.store.Rules()
	if err := s.store.RemoveRule(name); err != nil {
		storeError(w, err)
		return
	}
	if !s.rulesChanged(w, before) {
		return
	}
	s.log.Info("rate rule removed", "rule", name)
	w.WriteHeader(http.StatusNoContent)
}
