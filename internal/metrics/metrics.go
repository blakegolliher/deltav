// Package metrics aggregates Envoy telemetry in memory: one rolling series
// per endpoint and one per fleet member (not their cross-product), plus the
// in-flight request counts Envoys report through load reporting.
//
// Each series keeps 10-second slots for the last five minutes and one-minute
// slots for the last hour, so the 1 m, 5 m and 1 h windows are sums of whole
// slots. A slot is allocated when first written; a series that has been busy
// for the whole hour holds about 35 KB.
package metrics

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Window is one of the rolling windows the API reports.
type Window int

const (
	Window1m Window = iota
	Window5m
	Window1h
)

// Windows lists every window, shortest first.
var Windows = []Window{Window1m, Window5m, Window1h}

func (w Window) String() string {
	return [...]string{"1m", "5m", "1h"}[w]
}

// ParseWindow reads "1m", "5m" or "1h"; "" means one minute.
func ParseWindow(s string) (Window, error) {
	switch s {
	case "", "1m":
		return Window1m, nil
	case "5m":
		return Window5m, nil
	case "1h":
		return Window1h, nil
	}
	return 0, fmt.Errorf("window %q: want 1m, 5m or 1h", s)
}

const (
	fineSlot    = 10 * time.Second
	fineSlots   = 30 // five minutes
	coarseSlot  = time.Minute
	coarseSlots = 60 // one hour
)

// Sample is one finished request, taken from an Envoy access log entry.
type Sample struct {
	Target   string        // the Envoy cluster, which is the target name
	Endpoint string        // upstream "ip:port"; "" if Envoy never chose one
	Status   int           // HTTP status; 0 if no response was sent
	Duration time.Duration // start of request to last byte out
	BytesIn  uint64        // request body bytes
	BytesOut uint64        // response body bytes
	Overflow bool          // refused by a circuit breaker (response flag UO)
}

// EndpointKey names one endpoint of one target.
type EndpointKey struct {
	Target   string
	Endpoint string // "ip:port", or "" for requests that reached no endpoint
}

// Counts are plain totals over some span of time. Class is indexed by the
// status code's hundreds digit, with 0 for requests that got no response;
// S403 and S503 are also counted in their classes.
type Counts struct {
	Requests    uint64
	Class       [6]uint64
	S403, S503  uint64
	Overflow    uint64
	BytesIn     uint64
	BytesOut    uint64
	DurationSum time.Duration
}

func (c *Counts) add(s *Sample) {
	c.Requests++
	if class := s.Status / 100; class >= 1 && class <= 5 {
		c.Class[class]++
	} else {
		c.Class[0]++
	}
	switch s.Status {
	case 403:
		c.S403++
	case 503:
		c.S503++
	}
	if s.Overflow {
		c.Overflow++
	}
	c.BytesIn += s.BytesIn
	c.BytesOut += s.BytesOut
	c.DurationSum += s.Duration
}

func (c *Counts) merge(o *Counts) {
	c.Requests += o.Requests
	for i := range c.Class {
		c.Class[i] += o.Class[i]
	}
	c.S403 += o.S403
	c.S503 += o.S503
	c.Overflow += o.Overflow
	c.BytesIn += o.BytesIn
	c.BytesOut += o.BytesOut
	c.DurationSum += o.DurationSum
}

// slot is one time slice of a series.
type slot struct {
	Counts
	hist [histBuckets]uint32
}

// Totals are a series' counts since the daemon started, for Prometheus.
type Totals struct {
	Counts
	Hist [histBuckets]uint64
}

// ring is a circular buffer of slots, each tagged with the slot number
// (time divided by the slot length) it currently holds.
type ring struct {
	length time.Duration
	epoch  []int64
	slots  []*slot
}

func newRing(length time.Duration, n int) ring {
	return ring{length: length, epoch: make([]int64, n), slots: make([]*slot, n)}
}

// at returns the slot for now, clearing it if it last held an older time.
func (r *ring) at(now time.Time) *slot {
	e := now.UnixNano() / int64(r.length)
	i := int(e % int64(len(r.slots)))
	if r.slots[i] == nil {
		r.slots[i] = &slot{}
	} else if r.epoch[i] != e {
		*r.slots[i] = slot{}
	}
	r.epoch[i] = e
	return r.slots[i]
}

// sum adds the n most recent slots, the current partial one included, into
// out.
func (r *ring) sum(now time.Time, n int, out *slot) {
	cur := now.UnixNano() / int64(r.length)
	for i, s := range r.slots {
		if s != nil && r.epoch[i] > cur-int64(n) && r.epoch[i] <= cur {
			out.Counts.merge(&s.Counts)
			for b, v := range s.hist {
				out.hist[b] += v
			}
		}
	}
}

type series struct {
	fine, coarse ring
	total        Totals
	last         time.Time
}

func newSeries() *series {
	return &series{fine: newRing(fineSlot, fineSlots), coarse: newRing(coarseSlot, coarseSlots)}
}

func (s *series) add(now time.Time, sample *Sample) {
	b := bucketOf(sample.Duration)
	for _, sl := range []*slot{s.fine.at(now), s.coarse.at(now)} {
		sl.Counts.add(sample)
		sl.hist[b]++
	}
	s.total.Counts.add(sample)
	s.total.Hist[b]++
	s.last = now
}

// window adds the slots that make up window w into out.
func (s *series) window(now time.Time, w Window, out *slot) {
	switch w {
	case Window1m:
		s.fine.sum(now, int(time.Minute/fineSlot), out)
	case Window5m:
		s.fine.sum(now, fineSlots, out)
	default:
		s.coarse.sum(now, coarseSlots, out)
	}
}

// StatusCounts breaks a request count down by response status. 4xx includes
// the 403s and 5xx the 503s; None counts requests that got no response.
type StatusCounts struct {
	S2xx uint64 `json:"2xx"`
	S3xx uint64 `json:"3xx"`
	S4xx uint64 `json:"4xx"`
	S5xx uint64 `json:"5xx"`
	S403 uint64 `json:"403"`
	S503 uint64 `json:"503"`
	None uint64 `json:"none"`
}

// Latency holds percentiles in milliseconds. They are estimated from a
// histogram whose buckets grow by 26%, so they are good to about ±13%.
type Latency struct {
	P50 float64 `json:"p50"`
	P90 float64 `json:"p90"`
	P99 float64 `json:"p99"`
}

// Summary is what the API reports for one series over one window. Rates are
// per second, averaged over WindowSeconds.
type Summary struct {
	WindowSeconds float64      `json:"window_seconds"`
	Requests      uint64       `json:"requests"`
	RequestRate   float64      `json:"request_rate"`
	Status        StatusCounts `json:"status"`
	Rate403       float64      `json:"rate_403"`
	Rate503       float64      `json:"rate_503"`
	Overflow      uint64       `json:"overflow"` // refused by a circuit breaker
	BytesIn       uint64       `json:"bytes_in"`
	BytesOut      uint64       `json:"bytes_out"`
	BytesInRate   float64      `json:"bytes_in_rate"`
	BytesOutRate  float64      `json:"bytes_out_rate"`
	LatencyMS     Latency      `json:"latency_ms"`
}

func summarize(sl *slot, span time.Duration) Summary {
	secs := span.Seconds()
	out := Summary{
		WindowSeconds: secs,
		Requests:      sl.Requests,
		Status: StatusCounts{
			S2xx: sl.Class[2], S3xx: sl.Class[3], S4xx: sl.Class[4], S5xx: sl.Class[5],
			S403: sl.S403, S503: sl.S503, None: sl.Class[0] + sl.Class[1],
		},
		Overflow: sl.Overflow,
		BytesIn:  sl.BytesIn,
		BytesOut: sl.BytesOut,
		LatencyMS: Latency{
			P50: quantileMS(sl.hist[:], 0.50),
			P90: quantileMS(sl.hist[:], 0.90),
			P99: quantileMS(sl.hist[:], 0.99),
		},
	}
	if secs > 0 {
		out.RequestRate = float64(sl.Requests) / secs
		out.Rate403 = float64(sl.S403) / secs
		out.Rate503 = float64(sl.S503) / secs
		out.BytesInRate = float64(sl.BytesIn) / secs
		out.BytesOutRate = float64(sl.BytesOut) / secs
	}
	return out
}

// LoadReport is one endpoint's line in an Envoy load report. InProgress is
// a gauge; the other three count requests since that Envoy's last report.
type LoadReport struct {
	Key        EndpointKey
	InProgress uint64
	Issued     uint64
	Success    uint64
	Errors     uint64
}

// LoadTotals are request counts from load reports since the daemon started.
// Unlike access-log counts they are unaffected by log sampling. They miss
// one thing: Envoy does not report a cluster's first reporting interval
// (it resets the counters when it starts tracking the cluster), so requests
// an Envoy sends in the interval after it first learns a target are absent.
type LoadTotals struct {
	Issued  uint64 `json:"issued"`
	Success uint64 `json:"success"`
	Errors  uint64 `json:"errors"`
}

type nodeLoad struct {
	stream   int64
	at       time.Time
	inflight map[EndpointKey]uint64
	total    uint64
}

// Aggregator holds every series. It is safe for concurrent use.
type Aggregator struct {
	mu        sync.Mutex
	now       func() time.Time
	started   time.Time
	endpoints map[EndpointKey]*series
	members   map[string]*series
	load      map[string]*nodeLoad       // by node ID: its latest load report
	inflight  map[EndpointKey]uint64     // fleet total of the latest reports
	lrs       map[EndpointKey]LoadTotals // cumulative
}

// New returns an empty aggregator.
func New() *Aggregator {
	return &Aggregator{
		now: time.Now, started: time.Now(),
		endpoints: map[EndpointKey]*series{}, members: map[string]*series{},
		load: map[string]*nodeLoad{}, inflight: map[EndpointKey]uint64{}, lrs: map[EndpointKey]LoadTotals{},
	}
}

// Record adds a batch of finished requests reported by one fleet member.
// They are counted in the slot current at the time of the call.
func (a *Aggregator) Record(node string, samples []Sample) {
	if len(samples) == 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	member := a.members[node]
	if member == nil {
		member = newSeries()
		a.members[node] = member
	}
	var (
		lastKey EndpointKey
		ep      *series
	)
	for i := range samples {
		s := &samples[i]
		key := EndpointKey{Target: s.Target, Endpoint: s.Endpoint}
		if ep == nil || key != lastKey {
			if ep = a.endpoints[key]; ep == nil {
				ep = newSeries()
				a.endpoints[key] = ep
			}
			lastKey = key
		}
		ep.add(now, s)
		member.add(now, s)
	}
}

// Load replaces what is known of one member's in-flight requests with its
// latest report, and adds the report's request counts to the totals. Envoy
// leaves idle endpoints out of a report, so an endpoint missing from it has
// nothing in flight. stream identifies the reporting stream; see LoadGone.
func (a *Aggregator) Load(node string, stream int64, reports []LoadReport) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dropLoad(node)
	nl := &nodeLoad{stream: stream, at: a.now(), inflight: make(map[EndpointKey]uint64, len(reports))}
	for _, r := range reports {
		if r.InProgress > 0 {
			nl.inflight[r.Key] += r.InProgress
			nl.total += r.InProgress
			a.inflight[r.Key] += r.InProgress
		}
		if r.Issued+r.Success+r.Errors > 0 {
			t := a.lrs[r.Key]
			t.Issued += r.Issued
			t.Success += r.Success
			t.Errors += r.Errors
			a.lrs[r.Key] = t
		}
	}
	a.load[node] = nl
}

// LoadGone forgets a member's in-flight requests when its reporting stream
// ends, unless a newer stream from the same member has reported since.
func (a *Aggregator) LoadGone(node string, stream int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if nl := a.load[node]; nl != nil && nl.stream == stream {
		a.dropLoad(node)
	}
}

func (a *Aggregator) dropLoad(node string) {
	nl := a.load[node]
	if nl == nil {
		return
	}
	for k, v := range nl.inflight {
		if a.inflight[k] -= v; a.inflight[k] == 0 {
			delete(a.inflight, k)
		}
	}
	delete(a.load, node)
}

// EndpointStats is one endpoint's view over a window.
type EndpointStats struct {
	Key      EndpointKey
	Summary  Summary
	InFlight uint64 // fleet-wide requests in flight to it, from load reports
	Load     LoadTotals
}

// span is the time window w covers at now: its whole slots plus the part of
// the current slot that has elapsed. It is capped at the daemon's age, so
// rates are not diluted by time before it was running.
func (a *Aggregator) span(now time.Time, w Window) time.Duration {
	length, n := fineSlot, int(time.Minute/fineSlot)
	switch w {
	case Window5m:
		n = fineSlots
	case Window1h:
		length, n = coarseSlot, coarseSlots
	}
	d := time.Duration(n-1)*length + time.Duration(now.UnixNano()%int64(length))
	if age := now.Sub(a.started); age < d {
		d = age
	}
	if d < time.Second {
		d = time.Second
	}
	return d
}

// Endpoints returns every endpoint that has a series, in-flight requests or
// load-report totals, plus those named in also (so that configured but idle
// endpoints are listed), sorted by target and endpoint.
func (a *Aggregator) Endpoints(w Window, also ...EndpointKey) []EndpointStats {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	keys := make(map[EndpointKey]bool, len(a.endpoints)+len(also))
	for _, k := range also {
		keys[k] = true
	}
	for k := range a.endpoints {
		keys[k] = true
	}
	for k := range a.inflight {
		keys[k] = true
	}
	for k := range a.lrs {
		keys[k] = true
	}
	out := make([]EndpointStats, 0, len(keys))
	for k := range keys {
		es := EndpointStats{Key: k, InFlight: a.inflight[k], Load: a.lrs[k]}
		var sl slot
		if s := a.endpoints[k]; s != nil {
			s.window(now, w, &sl)
		}
		es.Summary = summarize(&sl, a.span(now, w))
		out = append(out, es)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key.Target != out[j].Key.Target {
			return out[i].Key.Target < out[j].Key.Target
		}
		return out[i].Key.Endpoint < out[j].Key.Endpoint
	})
	return out
}

// TargetStats is a target's endpoints added together.
type TargetStats struct {
	Summary  Summary
	InFlight uint64
}

// Targets returns one rollup per target name seen in telemetry.
func (a *Aggregator) Targets(w Window) map[string]TargetStats {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	sums := map[string]*slot{}
	for k, s := range a.endpoints {
		t := sums[k.Target]
		if t == nil {
			t = &slot{}
			sums[k.Target] = t
		}
		s.window(now, w, t)
	}
	out := make(map[string]TargetStats, len(sums))
	for name, t := range sums {
		out[name] = TargetStats{Summary: summarize(t, a.span(now, w))}
	}
	for k, v := range a.inflight {
		ts := out[k.Target]
		ts.InFlight += v
		out[k.Target] = ts
	}
	return out
}

// MemberStats is one fleet member's view over a window.
type MemberStats struct {
	Summary  Summary
	InFlight uint64 // requests it has in flight, from its latest load report
}

// Member returns one member's stats; zero values if it has sent nothing.
func (a *Aggregator) Member(node string, w Window) MemberStats {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	var sl slot
	if s := a.members[node]; s != nil {
		s.window(now, w, &sl)
	}
	out := MemberStats{Summary: summarize(&sl, a.span(now, w))}
	if nl := a.load[node]; nl != nil {
		out.InFlight = nl.total
	}
	return out
}

// Idle is the summary of a series with no requests over window w: zero
// counts over the right span.
func (a *Aggregator) Idle(w Window) Summary {
	a.mu.Lock()
	defer a.mu.Unlock()
	return summarize(&slot{}, a.span(a.now(), w))
}

// MemberRequests returns each member's request count over a window. It is
// the cheap input for sorting the fleet by request rate.
func (a *Aggregator) MemberRequests(w Window) map[string]uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	out := make(map[string]uint64, len(a.members))
	for node, s := range a.members {
		var sl slot
		s.window(now, w, &sl)
		out[node] = sl.Requests
	}
	return out
}

// EndpointTotals is one endpoint's cumulative state, for Prometheus.
type EndpointTotals struct {
	Key      EndpointKey
	Totals   Totals
	InFlight uint64
	Load     LoadTotals
}

// EndpointTotals returns cumulative counts for every endpoint.
func (a *Aggregator) EndpointTotals() []EndpointTotals {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]EndpointTotals, 0, len(a.endpoints))
	seen := map[EndpointKey]bool{}
	for k, s := range a.endpoints {
		seen[k] = true
		out = append(out, EndpointTotals{Key: k, Totals: s.total, InFlight: a.inflight[k], Load: a.lrs[k]})
	}
	for k, t := range a.lrs {
		if !seen[k] {
			out = append(out, EndpointTotals{Key: k, InFlight: a.inflight[k], Load: t})
		}
	}
	return out
}

// MemberTotals returns cumulative counts for every fleet member.
func (a *Aggregator) MemberTotals() map[string]Totals {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]Totals, len(a.members))
	for node, s := range a.members {
		out[node] = s.total
	}
	return out
}

// Prune drops series that have had no request for idle, and load reports
// older than staleLoad (a reporting stream that died without closing).
// Cumulative totals go with their series, which Prometheus sees as a
// counter reset.
func (a *Aggregator) Prune(idle, staleLoad time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	for k, s := range a.endpoints {
		if now.Sub(s.last) > idle {
			delete(a.endpoints, k)
			delete(a.lrs, k)
		}
	}
	for node, s := range a.members {
		if now.Sub(s.last) > idle {
			delete(a.members, node)
		}
	}
	for node, nl := range a.load {
		if now.Sub(nl.at) > staleLoad {
			a.dropLoad(node)
		}
	}
}
