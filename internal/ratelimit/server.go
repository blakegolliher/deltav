package ratelimit

import (
	"log/slog"
	"math"
	"sort"
	"sync"
	"time"

	rlqs "github.com/envoyproxy/go-control-plane/envoy/service/rate_limit_quota/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

const (
	// Domain names Delta-V's rules to Envoy's rate limit quota filter.
	Domain = "deltav"
	// ReportInterval is how often an Envoy reports each count's use. Envoy
	// 1.40 reports every 5 seconds whatever interval it is configured with
	// (filter_persistence.cc), so that is how fast shares can follow demand.
	ReportInterval = 5 * time.Second
	// assignmentTTL is how long an Envoy may use a share without hearing
	// again. Every report is answered, so a share is refreshed four times
	// over before it runs out; when the daemon is gone it runs out, and the
	// Envoy falls back to the rule's own rate (Rule.Strategy). Whole
	// seconds: Envoy 1.40 reads no more of it.
	assignmentTTL = 4 * ReportInterval
	// A count's part that has not reported for this long is no longer
	// counted when the rate is divided: its Envoy has let it go.
	staleAfter = 3 * ReportInterval
	// idleReports is how many reports in a row may show no requests before
	// the daemon tells the Envoy to forget the count. It is opened again by
	// the next request that matches.
	idleReports = 6
	// maxKeysPerRule bounds what one rule can make the daemon remember. A
	// rule that counts per access key or per bucket gets a count for every
	// value clients send, and a client can send anything. Past the bound,
	// new values are not limited: failing open, as everywhere here.
	maxKeysPerRule = 100000
	// A recomputed share is sent to an Envoy that did not just report only
	// if it differs from the one it has by more than this fraction.
	resendChange = 0.1
)

// Bucket ID fields. A bucket ID is what Envoy and the daemon both call one
// count of one rule: the rule's name, and the value it is counted under.
const (
	IDRule = "rule"
	IDKey  = "key"
)

type bucketKey struct{ rule, key string }

// part is one Envoy's use of one count.
type part struct {
	id          *rlqs.BucketId // as the Envoy sent it; answers must carry the same
	allowedRate float64        // requests per second in its last report
	deniedRate  float64
	last        time.Time
	idle        int     // reports in a row with no requests
	sent        float64 // the share it was last given, requests per second
	answer      bool    // it has just reported and is owed an answer
}

type bucket struct {
	parts map[int64]*part // by stream
}

type stream struct {
	id   int64
	peer string
	send chan *rlqs.RateLimitQuotaResponse
}

// Server is the rate limit quota service Envoys report to. It holds no
// request's fate: an Envoy decides each request by itself, from the share
// it was last given.
type Server struct {
	rlqs.UnimplementedRateLimitQuotaServiceServer
	log *slog.Logger
	now func() time.Time

	mu       sync.Mutex
	rules    map[string]Rule
	buckets  map[bucketKey]*bucket
	keys     map[string]int // counts per rule
	streams  map[int64]*stream
	nextID   int64
	totals   map[string]*Totals
	overflow uint64
}

// Totals are a rule's requests since the daemon started, as Envoys reported
// them.
type Totals struct {
	Allowed uint64 `json:"allowed"`
	Denied  uint64 `json:"denied"`
}

// NewServer returns a quota service with no rules.
func NewServer(log *slog.Logger) *Server {
	return &Server{
		log: log, now: time.Now, rules: map[string]Rule{}, buckets: map[bucketKey]*bucket{},
		keys: map[string]int{}, streams: map[int64]*stream{}, totals: map[string]*Totals{},
	}
}

// Register adds the service to a gRPC server.
func (s *Server) Register(g *grpc.Server) { rlqs.RegisterRateLimitQuotaServiceServer(g, s) }

// SetRules replaces the rules. Counts of rules that are gone are dropped,
// and every Envoy using a count of a rule that remains is sent its share
// afresh, since the rate may have changed.
func (s *Server) SetRules(rules []Rule) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules = map[string]Rule{}
	for _, r := range rules {
		s.rules[r.Name] = r
	}
	out := map[int64][]*rlqs.RateLimitQuotaResponse_BucketAction{}
	for k, b := range s.buckets {
		if _, ok := s.rules[k.rule]; !ok {
			for id, p := range b.parts {
				out[id] = append(out[id], abandon(p.id))
			}
			s.drop(k)
			continue
		}
		for _, p := range b.parts {
			p.answer = true
		}
		s.assign(k, out)
	}
	s.flush(out)
}

func (s *Server) drop(k bucketKey) {
	if _, ok := s.buckets[k]; ok {
		delete(s.buckets, k)
		s.keys[k.rule]--
	}
}

// StreamRateLimitQuotas is one Envoy's stream of usage reports, answered
// with its shares.
func (s *Server) StreamRateLimitQuotas(st rlqs.RateLimitQuotaService_StreamRateLimitQuotasServer) error {
	ctx := st.Context()
	me := &stream{send: make(chan *rlqs.RateLimitQuotaResponse, 256)}
	if p, ok := peer.FromContext(ctx); ok {
		me.peer = p.Addr.String()
	}
	s.mu.Lock()
	s.nextID++
	me.id = s.nextID
	s.streams[me.id] = me
	s.mu.Unlock()
	defer s.closed(me)

	// One goroutine sends: shares are pushed to an Envoy both in answer to
	// its own reports and when another Envoy's report changes the division.
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case msg := <-me.send:
				if st.Send(msg) != nil {
					return
				}
			case <-done:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	first := true
	for {
		req, err := st.Recv()
		if err != nil {
			return nil // the Envoy went away, or the daemon is stopping
		}
		// The domain comes with the first report only.
		if first && req.GetDomain() != Domain {
			return status.Errorf(codes.InvalidArgument, "rate limit quota domain %q is not served here", req.GetDomain())
		}
		first = false
		s.report(me, req)
	}
}

// report takes one Envoy's usage report and sends out the shares it changes.
func (s *Server) report(me *stream, req *rlqs.RateLimitQuotaUsageReports) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	out := map[int64][]*rlqs.RateLimitQuotaResponse_BucketAction{}
	touched := map[bucketKey]bool{}
	for _, u := range req.GetBucketQuotaUsages() {
		id := u.GetBucketId().GetBucket()
		k := bucketKey{id[IDRule], id[IDKey]}
		if _, known := s.rules[k.rule]; !known {
			// A rule that has just been removed, reported by an Envoy
			// that has not yet taken the config without it.
			out[me.id] = append(out[me.id], assignment(u.GetBucketId(), allowAll(), 2*ReportInterval))
			continue
		}
		t := s.totals[k.rule]
		if t == nil {
			t = &Totals{}
			s.totals[k.rule] = t
		}
		t.Allowed += u.GetNumRequestsAllowed()
		t.Denied += u.GetNumRequestsDenied()

		b := s.buckets[k]
		if b == nil {
			if s.keys[k.rule] >= maxKeysPerRule {
				s.overflow++
				out[me.id] = append(out[me.id], assignment(u.GetBucketId(), allowAll(), assignmentTTL))
				continue
			}
			b = &bucket{parts: map[int64]*part{}}
			s.buckets[k] = b
			s.keys[k.rule]++
		}
		p := b.parts[me.id]
		if p == nil {
			p = &part{id: u.GetBucketId()}
			b.parts[me.id] = p
		}
		p.last, p.answer = now, true
		n := u.GetNumRequestsAllowed() + u.GetNumRequestsDenied()
		// The first report of a count comes at once, with no time elapsed:
		// it says the count is in use, not yet how much.
		if elapsed := u.GetTimeElapsed().AsDuration().Seconds(); elapsed > 0 {
			p.allowedRate = float64(u.GetNumRequestsAllowed()) / elapsed
			p.deniedRate = float64(u.GetNumRequestsDenied()) / elapsed
			if n == 0 {
				p.idle++
			} else {
				p.idle = 0
			}
		}
		if p.idle >= idleReports {
			out[me.id] = append(out[me.id], abandon(p.id))
			delete(b.parts, me.id)
		}
		touched[k] = true
	}
	for k := range touched {
		s.assign(k, out)
	}
	s.flush(out)
}

// closed forgets a stream, and gives what it held to those that remain.
func (s *Server) closed(me *stream) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.streams, me.id)
	out := map[int64][]*rlqs.RateLimitQuotaResponse_BucketAction{}
	for k, b := range s.buckets {
		if _, ok := b.parts[me.id]; ok {
			delete(b.parts, me.id)
			s.assign(k, out)
		}
	}
	s.flush(out)
}

// assign works out every Envoy's share of one count and queues the answers
// that are due: to an Envoy that has just reported, always, which also
// renews its share's time to live; to any other, only if its share has
// changed enough to be worth a message.
func (s *Server) assign(k bucketKey, out map[int64][]*rlqs.RateLimitQuotaResponse_BucketAction) {
	b := s.buckets[k]
	if b == nil {
		return
	}
	now := s.now()
	ids := make([]int64, 0, len(b.parts))
	for id, p := range b.parts {
		if _, alive := s.streams[id]; !alive || now.Sub(p.last) > staleAfter {
			delete(b.parts, id)
			continue
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		s.drop(k)
		return
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	rule := s.rules[k.rule]
	shares := make([]float64, len(ids))
	switch {
	case rule.Blocked:
	case rule.Counts(PerClient):
		// Each client host has the whole rate to itself.
		for i := range shares {
			shares[i] = rule.Rate()
		}
	default:
		demand := make([]float64, len(ids))
		for i, id := range ids {
			demand[i] = b.parts[id].allowedRate + b.parts[id].deniedRate
		}
		shares = Divide(rule.Rate(), demand)
	}
	for i, id := range ids {
		p := b.parts[id]
		changed := p.sent == 0 || math.Abs(shares[i]-p.sent) > resendChange*p.sent
		if !p.answer && !changed {
			continue
		}
		strategy := blanket(typev3.RateLimitStrategy_DENY_ALL)
		if !rule.Blocked {
			strategy = &typev3.RateLimitStrategy{Strategy: &typev3.RateLimitStrategy_TokenBucket{
				TokenBucket: tokenBucket(shares[i], burstShare(rule.Burst, shares[i], rule.Rate()))}}
		}
		out[id] = append(out[id], assignment(p.id, strategy, assignmentTTL))
		p.sent, p.answer = shares[i], false
	}
}

func (s *Server) flush(out map[int64][]*rlqs.RateLimitQuotaResponse_BucketAction) {
	for id, actions := range out {
		st := s.streams[id]
		if st == nil || len(actions) == 0 {
			continue
		}
		select {
		case st.send <- &rlqs.RateLimitQuotaResponse{BucketAction: actions}:
		default:
			// The Envoy is not reading. Its next report is answered anew.
			s.log.Warn("rate limit shares not sent: the Envoy's stream is backed up", "peer", st.peer)
		}
	}
}

func allowAll() *typev3.RateLimitStrategy { return blanket(typev3.RateLimitStrategy_ALLOW_ALL) }

func assignment(id *rlqs.BucketId, strategy *typev3.RateLimitStrategy, ttl time.Duration) *rlqs.RateLimitQuotaResponse_BucketAction {
	return &rlqs.RateLimitQuotaResponse_BucketAction{
		BucketId: id,
		BucketAction: &rlqs.RateLimitQuotaResponse_BucketAction_QuotaAssignmentAction_{
			QuotaAssignmentAction: &rlqs.RateLimitQuotaResponse_BucketAction_QuotaAssignmentAction{
				AssignmentTimeToLive: durationpb.New(ttl), RateLimitStrategy: strategy,
			},
		},
	}
}

func abandon(id *rlqs.BucketId) *rlqs.RateLimitQuotaResponse_BucketAction {
	return &rlqs.RateLimitQuotaResponse_BucketAction{
		BucketId:     id,
		BucketAction: &rlqs.RateLimitQuotaResponse_BucketAction_AbandonAction_{AbandonAction: &rlqs.RateLimitQuotaResponse_BucketAction_AbandonAction{}},
	}
}

// KeyStatus is one count of a rule: what it is counted under, and its use
// in the Envoys' last reports.
type KeyStatus struct {
	Key         string  `json:"key"`
	AllowedRate float64 `json:"allowed_rate"`
	DeniedRate  float64 `json:"denied_rate"`
	Envoys      int     `json:"envoys"` // Envoys with the count open
}

// Status is what the Envoys last reported for one rule.
type Status struct {
	AllowedRate float64 `json:"allowed_rate"` // requests per second let through
	DeniedRate  float64 `json:"denied_rate"`  // requests per second refused
	Totals      Totals  `json:"totals"`
	Keys        int     `json:"keys"`   // counts open: access keys, buckets, ... the rule counts per
	Envoys      int     `json:"envoys"` // Envoys with any count of the rule open
	// Top holds the counts with the most refused requests, then the most
	// requests, at most ten.
	Top []KeyStatus `json:"top"`
}

// Status returns each rule's use by name. Overflow is how many times a new
// count was left unlimited because its rule already had maxKeysPerRule.
func (s *Server) Status() (rules map[string]Status, overflow uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	rules = map[string]Status{}
	envoys := map[string]map[int64]bool{}
	for name := range s.rules {
		st := Status{Top: []KeyStatus{}}
		if t := s.totals[name]; t != nil {
			st.Totals = *t
		}
		rules[name] = st
		envoys[name] = map[int64]bool{}
	}
	for k, b := range s.buckets {
		st, ok := rules[k.rule]
		if !ok {
			continue
		}
		ks := KeyStatus{Key: k.key}
		for id, p := range b.parts {
			if now.Sub(p.last) > staleAfter {
				continue
			}
			ks.AllowedRate += p.allowedRate
			ks.DeniedRate += p.deniedRate
			ks.Envoys++
			envoys[k.rule][id] = true
		}
		st.AllowedRate += ks.AllowedRate
		st.DeniedRate += ks.DeniedRate
		st.Keys++
		st.Top = append(st.Top, ks)
		rules[k.rule] = st
	}
	for name, st := range rules {
		sort.Slice(st.Top, func(i, j int) bool {
			a, b := st.Top[i], st.Top[j]
			if a.DeniedRate != b.DeniedRate {
				return a.DeniedRate > b.DeniedRate
			}
			if a.AllowedRate != b.AllowedRate {
				return a.AllowedRate > b.AllowedRate
			}
			return a.Key < b.Key
		})
		if len(st.Top) > 10 {
			st.Top = st.Top[:10]
		}
		st.Envoys = len(envoys[name])
		rules[name] = st
	}
	return rules, s.overflow
}
