package metrics

import (
	"math"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

// newAggregator starts a daemon that has been up for two hours, at a time
// aligned to both slot lengths, so window arithmetic in tests is exact.
func newAggregator() (*Aggregator, *clock) {
	c := &clock{t: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
	a := New()
	a.now = c.now
	a.started = c.t.Add(-2 * time.Hour)
	return a, c
}

func ok(target, endpoint string, d time.Duration) Sample {
	return Sample{Target: target, Endpoint: endpoint, Status: 200, Duration: d, BytesIn: 100, BytesOut: 1000}
}

func endpoint(t *testing.T, a *Aggregator, w Window, target, ep string) EndpointStats {
	t.Helper()
	for _, e := range a.Endpoints(w) {
		if e.Key == (EndpointKey{target, ep}) {
			return e
		}
	}
	t.Fatalf("no endpoint %s/%s in %v", target, ep, a.Endpoints(w))
	return EndpointStats{}
}

func near(got, want, tolerance float64) bool { return math.Abs(got-want) <= tolerance*math.Abs(want) }

func TestBuckets(t *testing.T) {
	if histBounds[0] != 100*time.Microsecond || histBounds[3] != 200*time.Microsecond || histBounds[30] != 102400*time.Microsecond {
		t.Fatalf("bounds: %v %v %v", histBounds[0], histBounds[3], histBounds[30])
	}
	for i := 1; i < len(histBounds); i++ {
		ratio := float64(histBounds[i]) / float64(histBounds[i-1])
		if ratio < 1.25 || ratio > 1.27 {
			t.Errorf("bound %d / bound %d = %.4f, want 2^(1/3)", i, i-1, ratio)
		}
	}
	// Each bound belongs to its own bucket; one nanosecond more is the next.
	for i, b := range histBounds {
		if bucketOf(b) != i || bucketOf(b+1) != i+1 {
			t.Errorf("bucketOf(%v) = %d, bucketOf(+1ns) = %d; want %d and %d", b, bucketOf(b), bucketOf(b+1), i, i+1)
		}
	}
	if bucketOf(0) != 0 || bucketOf(-time.Second) != 0 || bucketOf(24*time.Hour) != histBuckets-1 {
		t.Errorf("edge buckets: %d %d %d", bucketOf(0), bucketOf(-time.Second), bucketOf(24*time.Hour))
	}
}

func TestQuantiles(t *testing.T) {
	var h [histBuckets]uint32
	if quantileMS(h[:], 0.5) != 0 {
		t.Error("empty histogram must report 0")
	}
	// 1..1000 ms, uniform: percentiles must land within the bucket error.
	for i := 1; i <= 1000; i++ {
		h[bucketOf(time.Duration(i)*time.Millisecond)]++
	}
	for q, want := range map[float64]float64{0.50: 500, 0.90: 900, 0.99: 990} {
		if got := quantileMS(h[:], q); !near(got, want, 0.13) {
			t.Errorf("p%.0f = %.1f ms, want %.0f ±13%%", q*100, got, want)
		}
	}
	// One sample: every percentile is inside that sample's bucket.
	var one [histBuckets]uint32
	one[bucketOf(42*time.Millisecond)]++
	for _, q := range []float64{0.5, 0.99} {
		if got := quantileMS(one[:], q); got < 42/1.26 || got > 42*1.26 {
			t.Errorf("single 42 ms sample: p%.0f = %.1f", q*100, got)
		}
	}
	// Beyond the last bound all that is known is "at least".
	var huge [histBuckets]uint32
	huge[histBuckets-1] = 5
	if got := quantileMS(huge[:], 0.5); got != ms(histBounds[len(histBounds)-1]) {
		t.Errorf("open-ended bucket: %.0f ms", got)
	}
}

func TestSummaryCounts(t *testing.T) {
	a, _ := newAggregator()
	a.Record("host-a", []Sample{
		ok("s3", "10.0.0.1:80", 10*time.Millisecond),
		{Target: "s3", Endpoint: "10.0.0.1:80", Status: 403, Duration: time.Millisecond},
		{Target: "s3", Endpoint: "10.0.0.1:80", Status: 404, Duration: time.Millisecond},
		{Target: "s3", Endpoint: "10.0.0.1:80", Status: 503, Duration: time.Millisecond},
		{Target: "s3", Endpoint: "10.0.0.1:80", Status: 500, Duration: time.Millisecond},
		{Target: "s3", Endpoint: "10.0.0.1:80", Status: 304, Duration: time.Millisecond},
		{Target: "s3", Endpoint: "", Status: 503, Overflow: true},
		{Target: "s3", Endpoint: "10.0.0.1:80", Status: 0, Duration: time.Second},
	})
	s := endpoint(t, a, Window1m, "s3", "10.0.0.1:80").Summary
	want := StatusCounts{S2xx: 1, S3xx: 1, S4xx: 2, S5xx: 2, S403: 1, S503: 1, None: 1}
	if s.Requests != 7 || s.Status != want || s.Overflow != 0 || s.BytesIn != 100 || s.BytesOut != 1000 {
		t.Errorf("endpoint summary: %+v", s)
	}
	// A request that reached no endpoint is kept apart under "".
	none := endpoint(t, a, Window1m, "s3", "").Summary
	if none.Requests != 1 || none.Overflow != 1 || none.Status.S503 != 1 {
		t.Errorf("no-endpoint summary: %+v", none)
	}
	// The target and the member each see all eight.
	if got := a.Targets(Window1m)["s3"].Summary; got.Requests != 8 || got.Status.S503 != 2 || got.Overflow != 1 {
		t.Errorf("target summary: %+v", got)
	}
	if got := a.Member("host-a", Window1m).Summary; got.Requests != 8 {
		t.Errorf("member summary: %+v", got)
	}
	if got := a.Member("host-z", Window1m).Summary; got.Requests != 0 || got.LatencyMS.P99 != 0 {
		t.Errorf("unknown member: %+v", got)
	}
}

func TestWindowsAndRates(t *testing.T) {
	a, c := newAggregator()
	// 10 requests per second for 30 minutes, to one endpoint from one host.
	batch := make([]Sample, 10)
	for i := range batch {
		batch[i] = ok("s3", "10.0.0.1:80", 20*time.Millisecond)
	}
	for i := 0; i < 30*60; i++ {
		a.Record("host-a", batch)
		c.advance(time.Second)
	}
	// The clock now sits on a slot boundary: each window is its whole slots
	// less the empty current one.
	for w, want := range map[Window]struct {
		requests uint64
		seconds  float64
	}{
		Window1m: {500, 50},
		Window5m: {2900, 290},
		Window1h: {18000, 3540},
	} {
		s := endpoint(t, a, w, "s3", "10.0.0.1:80").Summary
		if s.Requests != want.requests || s.WindowSeconds != want.seconds {
			t.Errorf("%v: %d requests over %.0fs, want %d over %.0fs", w, s.Requests, s.WindowSeconds, want.requests, want.seconds)
		}
	}
	s := endpoint(t, a, Window1m, "s3", "10.0.0.1:80").Summary
	if s.RequestRate != 10 || s.BytesInRate != 1000 || s.BytesOutRate != 10000 {
		t.Errorf("1m rates: %.2f req/s, %.0f B/s in, %.0f B/s out", s.RequestRate, s.BytesInRate, s.BytesOutRate)
	}
	if !near(s.LatencyMS.P50, 20, 0.13) || !near(s.LatencyMS.P99, 20, 0.26) {
		t.Errorf("latency: %+v", s.LatencyMS)
	}

	// Part-way into a slot, the window includes the elapsed part.
	c.advance(4 * time.Second)
	if s := endpoint(t, a, Window1m, "s3", "10.0.0.1:80").Summary; s.WindowSeconds != 54 || s.Requests != 500 {
		t.Errorf("4s into a slot: %d requests over %.0fs", s.Requests, s.WindowSeconds)
	}

	// Traffic stops: it leaves the 1m window after a minute, the 5m after
	// five, and the hour window last. Old slots must not be counted again
	// when the ring comes back round.
	c.advance(56 * time.Second)
	if s := endpoint(t, a, Window1m, "s3", "10.0.0.1:80").Summary; s.Requests != 0 || s.RequestRate != 0 {
		t.Errorf("a minute after traffic stopped: %+v", s)
	}
	c.advance(5 * time.Minute)
	if s := endpoint(t, a, Window5m, "s3", "10.0.0.1:80").Summary; s.Requests != 0 {
		t.Errorf("five minutes after traffic stopped: %d requests in 5m", s.Requests)
	}
	if s := endpoint(t, a, Window1h, "s3", "10.0.0.1:80").Summary; s.Requests != 18000 {
		t.Errorf("hour window lost requests early: %d", s.Requests)
	}
	c.advance(55 * time.Minute)
	if s := endpoint(t, a, Window1h, "s3", "10.0.0.1:80").Summary; s.Requests != 0 {
		t.Errorf("an hour after traffic stopped: %d requests in 1h", s.Requests)
	}
	a.Record("host-a", batch[:1])
	for _, w := range Windows {
		if s := endpoint(t, a, w, "s3", "10.0.0.1:80").Summary; s.Requests != 1 {
			t.Errorf("%v after reusing old slots: %d requests, want 1", w, s.Requests)
		}
	}
	// Cumulative totals never go down.
	tot := a.EndpointTotals()
	if len(tot) != 1 || tot[0].Totals.Requests != 18001 || tot[0].Totals.Class[2] != 18001 {
		t.Errorf("totals: %+v", tot)
	}
}

// A daemon that has run for 20 seconds must not average over a full minute.
func TestRatesOnAYoungDaemon(t *testing.T) {
	a, c := newAggregator()
	a.started = c.t
	for i := 0; i < 20; i++ {
		a.Record("host-a", []Sample{ok("s3", "10.0.0.1:80", time.Millisecond)})
		c.advance(time.Second)
	}
	for _, w := range Windows {
		s := endpoint(t, a, w, "s3", "10.0.0.1:80").Summary
		if s.WindowSeconds != 20 || s.RequestRate != 1 {
			t.Errorf("%v: %.2f req/s over %.0fs, want 1 over 20", w, s.RequestRate, s.WindowSeconds)
		}
	}
}

func TestLoadReports(t *testing.T) {
	a, c := newAggregator()
	e1, e2 := EndpointKey{"s3", "10.0.0.1:80"}, EndpointKey{"s3", "10.0.0.2:80"}
	a.Load("host-a", 1, []LoadReport{{Key: e1, InProgress: 3, Issued: 10, Success: 6, Errors: 1}, {Key: e2, InProgress: 1, Issued: 2, Success: 1}})
	a.Load("host-b", 2, []LoadReport{{Key: e1, InProgress: 4, Issued: 5, Success: 1}})

	if got := endpoint(t, a, Window1m, "s3", "10.0.0.1:80"); got.InFlight != 7 || got.Load != (LoadTotals{Issued: 15, Success: 7, Errors: 1}) {
		t.Fatalf("endpoint 1: %+v", got)
	}
	if got := a.Targets(Window1m)["s3"].InFlight; got != 8 {
		t.Fatalf("target in flight = %d, want 8", got)
	}
	if got := a.Member("host-a", Window1m).InFlight; got != 4 {
		t.Fatalf("host-a in flight = %d, want 4", got)
	}

	// A new report replaces the member's picture: endpoints it leaves out
	// have nothing in flight. Request counts keep adding up.
	a.Load("host-a", 1, []LoadReport{{Key: e2, InProgress: 2, Issued: 3, Success: 3}})
	if got := endpoint(t, a, Window1m, "s3", "10.0.0.1:80").InFlight; got != 4 {
		t.Errorf("endpoint 1 in flight = %d, want host-b's 4", got)
	}
	if got := endpoint(t, a, Window1m, "s3", "10.0.0.2:80"); got.InFlight != 2 || got.Load.Issued != 5 {
		t.Errorf("endpoint 2: %+v", got)
	}

	// A stream that ends takes its in-flight counts with it, unless the
	// member has already reported on a newer stream.
	a.Load("host-b", 3, []LoadReport{{Key: e1, InProgress: 9}})
	a.LoadGone("host-b", 2)
	if got := endpoint(t, a, Window1m, "s3", "10.0.0.1:80").InFlight; got != 9 {
		t.Errorf("old stream's close removed the new stream's report: in flight = %d", got)
	}
	a.LoadGone("host-b", 3)
	a.LoadGone("host-a", 1)
	if got := a.Targets(Window1m)["s3"].InFlight; got != 0 {
		t.Errorf("in flight after every stream closed = %d", got)
	}

	// A stream that died without closing is dropped by Prune.
	a.Load("host-c", 4, []LoadReport{{Key: e1, InProgress: 5}})
	c.advance(3 * time.Minute)
	a.Prune(time.Hour, 2*time.Minute)
	if got := a.Targets(Window1m)["s3"].InFlight; got != 0 {
		t.Errorf("stale load report survived Prune: in flight = %d", got)
	}
	if got := endpoint(t, a, Window1m, "s3", "10.0.0.1:80").Load.Issued; got != 15 {
		t.Errorf("load totals changed: issued = %d", got)
	}
}

func TestPrune(t *testing.T) {
	a, c := newAggregator()
	a.Record("old-host", []Sample{ok("s3", "10.0.0.1:80", time.Millisecond)})
	c.advance(50 * time.Minute)
	a.Record("new-host", []Sample{ok("s3", "10.0.0.2:80", time.Millisecond)})
	c.advance(20 * time.Minute)
	a.Prune(time.Hour, time.Minute)
	eps := a.Endpoints(Window1h)
	if len(eps) != 1 || eps[0].Key.Endpoint != "10.0.0.2:80" {
		t.Errorf("endpoints after prune: %+v", eps)
	}
	if got := a.MemberRequests(Window1h); len(got) != 1 || got["new-host"] != 1 {
		t.Errorf("members after prune: %v", got)
	}
}

func TestPrometheusBuckets(t *testing.T) {
	a, _ := newAggregator()
	for _, d := range []time.Duration{50 * time.Microsecond, 150 * time.Microsecond, 3 * time.Millisecond, time.Hour} {
		a.Record("host-a", []Sample{ok("s3", "10.0.0.1:80", d)})
	}
	tot := a.EndpointTotals()[0].Totals
	buckets, count := PrometheusBuckets(&tot.Hist)
	if count != 4 {
		t.Fatalf("count = %d", count)
	}
	for le, want := range map[float64]uint64{0.0001: 1, 0.0002: 2, 0.0032: 3, 0.0064: 3, 419.4304: 3} {
		if got, ok := buckets[le]; !ok || got != want {
			t.Errorf("le=%g: %d (present %v), want %d", le, got, ok, want)
		}
	}
	if len(buckets) != 23 {
		t.Errorf("%d buckets, want 23", len(buckets))
	}
}

func TestParseWindow(t *testing.T) {
	for in, want := range map[string]Window{"": Window1m, "1m": Window1m, "5m": Window5m, "1h": Window1h} {
		if got, err := ParseWindow(in); err != nil || got != want || (in != "" && got.String() != in) {
			t.Errorf("ParseWindow(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := ParseWindow("2m"); err == nil {
		t.Error("ParseWindow accepted 2m")
	}
}

// BenchmarkRecord measures ingest cost per access log entry, in batches of
// the size Envoy typically flushes.
func BenchmarkRecord(b *testing.B) {
	a := New()
	batch := make([]Sample, 100)
	for i := range batch {
		batch[i] = Sample{Target: "s3", Endpoint: "10.0.0.1:80", Status: 200, Duration: time.Duration(i+1) * time.Millisecond, BytesIn: 1 << 20}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i += len(batch) {
		a.Record("host-a", batch)
	}
}
