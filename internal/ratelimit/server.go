package ratelimit

import (
	"fmt"
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
	// AssignmentTTL is how long an Envoy may use a share without hearing
	// again. It is long on purpose. When the daemon is away, every Envoy
	// goes on with the share it has, and the shares still add up to the
	// rule; when the time runs out the count is no longer limited at all
	// (xds/ratelimit.go). Whole seconds: Envoy 1.40 reads no more of it.
	AssignmentTTL = 10 * time.Minute
	// A share is sent again once it is this old, changed or not.
	renewAfter = AssignmentTTL / 4
	// A count's part that has not reported for this long is no longer
	// counted when the rate is divided: its Envoy has let it go.
	staleAfter = 3 * ReportInterval
	// idleFor is the least time a count must show no requests before the
	// daemon tells the Envoy to forget it. See keepFor for the most.
	idleFor = 6 * ReportInterval
	// maxKeysPerRule bounds what one rule can make the daemon remember. A
	// rule that counts per access key or per bucket gets a count for every
	// value clients send, and a client can send anything. Past the bound,
	// new values are not limited: failing open, as everywhere here.
	maxKeysPerRule = 100000
	// A share that has been worked out again is pushed to an Envoy that
	// did not just report only if it differs from the one it has by more
	// than resendChange; in answer to its own report, by answerChange. So
	// what the Envoys hold can add up to a tenth over the rule for the few
	// seconds until each next reports, and to a hundredth for longer.
	resendChange = 0.1
	answerChange = 0.01
	// Shares of one count are worked out again at most this often, however
	// many Envoys report it; and at once when an Envoy joins or leaves a
	// count that few have open.
	recomputeEvery = time.Second
	joinEvery      = 100 * time.Millisecond
	fewEnvoys      = 32
	// A rate is worked out from a report only over at least this long.
	measureOver = time.Second
	sweepEvery  = 30 * time.Second
	// Status is worked out at most this often, whoever asks.
	statusFor = time.Second
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
	id *rlqs.BucketId // as the Envoy sent it; answers must carry the same

	// Use, as requests per second over its last report or two.
	allowedRate, deniedRate float64
	// Requests reported since `since` and not yet turned into a rate.
	allowed, denied uint64
	since           time.Time
	last            time.Time // its last report
	active          time.Time // its last report that showed a request

	share float64 // as last worked out, requests per second
	burst uint32
	// What it was last sent, if anything.
	assigned  bool
	sent      float64
	sentBurst uint32
	sentAt    time.Time
}

type bucket struct {
	parts    map[int64]*part // by stream
	computed time.Time       // when the shares were last worked out
	dirty    bool            // an Envoy has joined or left since
}

type stream struct {
	id   int64
	peer string
	send chan *rlqs.RateLimitQuotaResponse
}

// meter is a count of events with their rate over the last few seconds.
type meter struct {
	total uint64
	n     [8]uint64
	sec   [8]int64
}

func (m *meter) add(now time.Time, n uint64) {
	sec := now.Unix()
	i := int(sec & 7)
	if m.sec[i] != sec {
		m.sec[i], m.n[i] = sec, 0
	}
	m.n[i] += n
	m.total += n
}

// rate is events per second over the five whole seconds before this one.
func (m *meter) rate(now time.Time) float64 {
	sec := now.Unix()
	var sum uint64
	for back := int64(1); back <= 5; back++ {
		if i := int((sec - back) & 7); m.sec[i] == sec-back {
			sum += m.n[i]
		}
	}
	return float64(sum) / 5
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
	blocked  map[string]*meter // refusals by blocked rules, which Envoys do not report here
	overflow uint64
	swept    time.Time
	divided  uint64 // times a count's shares were worked out
	recent   []Refusal
	oldest   int // index in recent of the oldest, once it is full

	statusAt time.Time
	status   map[string]Status
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
		blocked: map[string]*meter{},
	}
}

// Register adds the service to a gRPC server.
func (s *Server) Register(g *grpc.Server) { rlqs.RegisterRateLimitQuotaServiceServer(g, s) }

// sameShares reports whether two versions of a rule divide the same way.
func sameShares(a, b Rule) bool {
	return a.Blocked == b.Blocked && a.Rate() == b.Rate() && a.Burst == b.Burst && a.Counts(PerClient) == b.Counts(PerClient)
}

// SetRules replaces the rules. Counts of rules that are gone, or are now
// blocked, are given up; every Envoy using a count of a rule whose rate or
// burst has changed is sent its share afresh.
func (s *Server) SetRules(rules []Rule) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.rules
	s.rules = map[string]Rule{}
	for _, r := range rules {
		s.rules[r.Name] = r
	}
	for name := range s.blocked {
		if r, ok := s.rules[name]; !ok || !r.Blocked {
			delete(s.blocked, name)
		}
	}
	s.statusAt = time.Time{}
	out := map[int64][]*rlqs.RateLimitQuotaResponse_BucketAction{}
	for k, b := range s.buckets {
		rule, ok := s.rules[k.rule]
		if !ok || rule.Blocked {
			for id, p := range b.parts {
				out[id] = append(out[id], abandon(p.id))
			}
			s.drop(k)
			continue
		}
		if sameShares(old[k.rule], rule) {
			continue
		}
		for _, p := range b.parts {
			p.assigned = false
		}
		b.dirty = true
		s.assign(k, 0, out)
	}
	s.flush(out)
}

func (s *Server) drop(k bucketKey) {
	if _, ok := s.buckets[k]; ok {
		delete(s.buckets, k)
		s.keys[k.rule]--
	}
}

func (s *Server) open(from string) *stream {
	me := &stream{peer: from, send: make(chan *rlqs.RateLimitQuotaResponse, 256)}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	me.id = s.nextID
	s.streams[me.id] = me
	return me
}

// StreamRateLimitQuotas is one Envoy's stream of usage reports, answered
// with its shares.
func (s *Server) StreamRateLimitQuotas(st rlqs.RateLimitQuotaService_StreamRateLimitQuotasServer) error {
	ctx := st.Context()
	from := ""
	if p, ok := peer.FromContext(ctx); ok {
		from = p.Addr.String()
	}
	me := s.open(from)
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

// keepFor is how long one Envoy's part of a count must show no requests
// before the Envoy is told to forget it: until the tokens it holds would be
// full again. An Envoy that opens a count afresh starts with them full, so
// letting go any sooner would hand a client that pauses a second burst it
// has not waited for; a rule of a few requests an hour would mean nothing.
func keepFor(p *part) time.Duration {
	if !p.assigned || p.sent <= 0 {
		return idleFor
	}
	refill := 1.1*float64(p.sentBurst)/p.sent + ReportInterval.Seconds()
	if refill >= math.MaxInt64/float64(time.Second) {
		return math.MaxInt64
	}
	return max(idleFor, time.Duration(refill*float64(time.Second)))
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
		rule, known := s.rules[k.rule]
		if !known || rule.Blocked {
			// A rule that has just been removed, or blocked, reported by
			// an Envoy that still has a count of it. Forgetting the count
			// costs nothing: its script no longer counts anything there.
			if b := s.buckets[k]; b != nil {
				delete(b.parts, me.id)
				b.dirty = true
				touched[k] = true
			}
			out[me.id] = append(out[me.id], abandon(u.GetBucketId()))
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
				// Not remembered, so not limited; and not left with the
				// Envoy either, which would report it for ever.
				s.overflow++
				out[me.id] = append(out[me.id], abandon(u.GetBucketId()))
				continue
			}
			b = &bucket{parts: map[int64]*part{}}
			s.buckets[k] = b
			s.keys[k.rule]++
		}
		p := b.parts[me.id]
		if p == nil {
			// A count's first report comes at once: it says the count is
			// in use, not yet how much.
			p = &part{id: u.GetBucketId(), since: now, active: now}
			b.parts[me.id] = p
			b.dirty = true
		}
		p.last = now
		p.allowed += u.GetNumRequestsAllowed()
		p.denied += u.GetNumRequestsDenied()
		if u.GetNumRequestsAllowed()+u.GetNumRequestsDenied() > 0 {
			p.active = now
		}
		// The daemon's own clock, not the time Envoy says has passed, which
		// it gives in whole seconds: 4 or 5 for the same five.
		if span := now.Sub(p.since); span >= measureOver {
			p.allowedRate = float64(p.allowed) / span.Seconds()
			p.deniedRate = float64(p.denied) / span.Seconds()
			p.allowed, p.denied, p.since = 0, 0, now
		}
		if now.Sub(p.active) >= keepFor(p) {
			out[me.id] = append(out[me.id], abandon(p.id))
			delete(b.parts, me.id)
			b.dirty = true
		}
		touched[k] = true
	}
	for k := range touched {
		s.assign(k, me.id, out)
	}
	if now.Sub(s.swept) >= sweepEvery {
		s.sweep(now)
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
			b.dirty = true
			s.assign(k, 0, out)
		}
	}
	s.flush(out)
}

// prune drops the parts of a count whose Envoy has gone or stopped
// reporting, and reports whether any are left.
func (s *Server) prune(b *bucket, now time.Time) bool {
	for id, p := range b.parts {
		if _, alive := s.streams[id]; !alive || now.Sub(p.last) > staleAfter {
			delete(b.parts, id)
			b.dirty = true
		}
	}
	return len(b.parts) > 0
}

// sweep drops what no report will come for again: a count is otherwise
// looked at only when one of its Envoys reports.
func (s *Server) sweep(now time.Time) {
	s.swept = now
	for k, b := range s.buckets {
		if !s.prune(b, now) {
			s.drop(k)
		}
	}
}

// divide works out every Envoy's share of one count.
func (s *Server) divide(k bucketKey, b *bucket, now time.Time) {
	ids := make([]int64, 0, len(b.parts))
	for id := range b.parts {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	rule := s.rules[k.rule]
	shares := make([]float64, len(ids))
	var bursts []uint32
	if rule.Counts(PerClient) {
		// Each client host has the whole rate, and burst, to itself.
		bursts = make([]uint32, len(ids))
		for i := range shares {
			shares[i], bursts[i] = rule.Rate(), rule.Burst
		}
	} else {
		demand := make([]float64, len(ids))
		for i, id := range ids {
			demand[i] = b.parts[id].allowedRate + b.parts[id].deniedRate
		}
		shares = Divide(rule.Rate(), demand)
		bursts = DivideBurst(rule.Burst, shares)
	}
	for i, id := range ids {
		b.parts[id].share, b.parts[id].burst = shares[i], bursts[i]
	}
	b.computed, b.dirty = now, false
	s.divided++
}

func (p *part) differs(by float64) bool {
	return !p.assigned || p.burst != p.sentBurst || math.Abs(p.share-p.sent) > by*p.sent
}

// assign queues the shares of one count that are due: to the Envoy that has
// just reported (reporter; 0 for none), if what it holds is not what it
// should have or is getting old; and, when the shares have just been worked
// out again, to any other whose share has changed enough to be worth a
// message.
func (s *Server) assign(k bucketKey, reporter int64, out map[int64][]*rlqs.RateLimitQuotaResponse_BucketAction) {
	b := s.buckets[k]
	if b == nil {
		return
	}
	now := s.now()
	if !s.prune(b, now) {
		s.drop(k)
		return
	}
	since := now.Sub(b.computed)
	if since >= recomputeEvery || (b.dirty && (len(b.parts) <= fewEnvoys || since >= joinEvery)) {
		s.divide(k, b, now)
		for id, p := range b.parts {
			if id != reporter && p.differs(resendChange) {
				s.send(p, id, now, out)
			}
		}
	}
	p := b.parts[reporter]
	if p == nil {
		return
	}
	if p.burst == 0 {
		// It joined a count that many have open, between two divisions.
		// What an idle Envoy would get does until the next, which the
		// count's next report from anyone brings.
		rule := s.rules[k.rule]
		p.share, p.burst = idleHeld*rule.Rate()/float64(len(b.parts)), 1
		if rule.Counts(PerClient) {
			p.share, p.burst = rule.Rate(), rule.Burst
		}
	}
	if p.differs(answerChange) || now.Sub(p.sentAt) >= renewAfter {
		s.send(p, reporter, now, out)
	}
}

func (s *Server) send(p *part, id int64, now time.Time, out map[int64][]*rlqs.RateLimitQuotaResponse_BucketAction) {
	strategy := &typev3.RateLimitStrategy{Strategy: &typev3.RateLimitStrategy_TokenBucket{TokenBucket: tokenBucket(p.share, p.burst)}}
	out[id] = append(out[id], assignment(p.id, strategy, AssignmentTTL))
	p.assigned, p.sent, p.sentBurst, p.sentAt = true, p.share, p.burst, now
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
			// The Envoy is not reading. Shares it was to be sent count as
			// not sent, so its next report is answered with them.
			s.log.Warn("rate limit shares not sent: the Envoy's stream is backed up", "peer", st.peer)
			for _, a := range actions {
				if a.GetQuotaAssignmentAction() == nil {
					continue
				}
				id := a.GetBucketId().GetBucket()
				if b := s.buckets[bucketKey{id[IDRule], id[IDKey]}]; b != nil {
					if p := b.parts[st.id]; p != nil {
						p.assigned = false
					}
				}
			}
		}
	}
}

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
	// MostEnvoys is the most Envoys that have any one count of the rule
	// open. Each must be given a burst of at least one request, so when
	// there are more of them than the rule's burst, that many requests can
	// pass at once and not the rule's burst. The rate is held regardless.
	MostEnvoys int `json:"most_envoys"`
	// Top holds the counts with the most refused requests, then the most
	// requests, at most ten.
	Top []KeyStatus `json:"top"`
}

// Holds reports whether the rule's burst is being kept, given how many
// Envoys share its busiest count (Status.MostEnvoys).
func (r *Rule) Holds(st Status) bool {
	return r.Blocked || r.Counts(PerClient) || uint64(st.MostEnvoys) <= uint64(r.Burst)
}

// HoldNote says, for a rule whose burst is not being kept, why not; it is
// empty for a rule that holds.
func (r *Rule) HoldNote(st Status) string {
	if r.Holds(st) {
		return ""
	}
	return fmt.Sprintf("Its burst of %d is not being kept: one count is open on %d Envoys, and each must be allowed at least one request at a time, so %d can pass at once. Its rate is kept. A larger burst, or counting per client, would hold.",
		r.Burst, st.MostEnvoys, st.MostEnvoys)
}

func busier(a, b KeyStatus) bool {
	if a.DeniedRate != b.DeniedRate {
		return a.DeniedRate > b.DeniedRate
	}
	if a.AllowedRate != b.AllowedRate {
		return a.AllowedRate > b.AllowedRate
	}
	return a.Key < b.Key
}

// Status returns each rule's use by name. Overflow is how many times a new
// count was left unlimited because its rule already had maxKeysPerRule. The
// answer is worked out at most once a second and shared by everyone who
// asks in that second: it must not be changed.
func (s *Server) Status() (rules map[string]Status, overflow uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if s.status != nil && now.Sub(s.statusAt) < statusFor && !now.Before(s.statusAt) {
		return s.status, s.overflow
	}
	rules = map[string]Status{}
	envoys := map[string]map[int64]bool{}
	for name, r := range s.rules {
		st := Status{Top: []KeyStatus{}}
		if t := s.totals[name]; t != nil {
			st.Totals = *t
		}
		if m := s.blocked[name]; m != nil && r.Blocked {
			st.DeniedRate = m.rate(now)
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
		st.MostEnvoys = max(st.MostEnvoys, ks.Envoys)
		// The ten busiest, kept in order: no sort of every count.
		at := sort.Search(len(st.Top), func(i int) bool { return busier(ks, st.Top[i]) })
		if at < 10 {
			if len(st.Top) < 10 {
				st.Top = append(st.Top, KeyStatus{})
			}
			copy(st.Top[at+1:], st.Top[at:])
			st.Top[at] = ks
		}
		rules[k.rule] = st
	}
	for name, st := range rules {
		st.Envoys = len(envoys[name])
		rules[name] = st
	}
	s.status, s.statusAt = rules, now
	return rules, s.overflow
}
