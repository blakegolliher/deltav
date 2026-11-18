package ratelimit

import "time"

// Refusal is one request a rule refused, as its Envoy logged it.
type Refusal struct {
	At     time.Time `json:"at"`
	Client string    `json:"client"` // the Envoy that refused it: its client host
	Target string    `json:"target"`
	// Blocked is true if a blocked rule refused the request, and Rules
	// then names that rule. Otherwise the request was over a limit, and
	// Rules names the rules that counted it: Envoy does not say which of
	// them refused it, though with one rule there is no doubt.
	Blocked bool     `json:"blocked"`
	Rules   []string `json:"rules"`
	// What the script took the request to be. The access key and bucket
	// are as in a count's name: percent-encoded, cut at 200 bytes, "-" for
	// an access key the request did not carry.
	Operation string `json:"operation"`
	AccessKey string `json:"access_key"`
	Bucket    string `json:"bucket"`
	Status    int    `json:"status"` // what the client was answered
}

// recentRefusals is how many refusals the daemon remembers one by one. It
// keeps counts of all of them (Status); this is for seeing who is being
// refused, and is lost when the daemon stops.
const recentRefusals = 1000

// Refusals records requests that rules refused, as Envoys' access logs
// tell of them. Refusals by blocked rules are also counted here: an Envoy
// answers those by itself and reports nothing of them to the quota service.
func (s *Server) Refusals(batch []Refusal) {
	if len(batch) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for i := range batch {
		r := &batch[i]
		if r.At.IsZero() {
			r.At = now
		}
		if len(s.recent) < recentRefusals {
			s.recent = append(s.recent, *r)
		} else {
			s.recent[s.oldest] = *r
			s.oldest = (s.oldest + 1) % recentRefusals
		}
		if !r.Blocked || len(r.Rules) != 1 {
			continue
		}
		name := r.Rules[0]
		if rule, ok := s.rules[name]; !ok || !rule.Blocked {
			continue
		}
		m := s.blocked[name]
		if m == nil {
			m = &meter{}
			s.blocked[name] = m
		}
		m.add(now, 1)
		t := s.totals[name]
		if t == nil {
			t = &Totals{}
			s.totals[name] = t
		}
		t.Denied++
	}
}

// Recent returns the latest refusals, newest first, at most limit of them
// (all that are remembered if limit is not positive).
func (s *Server) Recent(limit int) []Refusal {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.recent)
	if limit <= 0 || limit > n {
		limit = n
	}
	out := make([]Refusal, 0, limit)
	for i := 0; i < limit; i++ {
		// The newest is just before the oldest, going round.
		out = append(out, s.recent[((s.oldest-1-i)%n+n)%n])
	}
	return out
}
