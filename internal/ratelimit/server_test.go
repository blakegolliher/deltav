package ratelimit

import (
	"context"
	"io"
	"log/slog"
	"math"
	"net"
	"testing"
	"time"

	rlqs "github.com/envoyproxy/go-control-plane/envoy/service/rate_limit_quota/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/durationpb"
)

func near(got, want float64) bool { return math.Abs(got-want) <= 0.01*math.Max(1, math.Abs(want)) }

func TestDivide(t *testing.T) {
	for _, c := range []struct {
		what   string
		rate   float64
		demand []float64
		want   []float64
	}{
		{"one Envoy has it all, whatever it asks", 100, []float64{0}, []float64{100}},
		{"and when it asks for more than there is", 100, []float64{900}, []float64{100}},
		{"under the rate: what each asks, and an equal part of the rest", 100, []float64{60, 20}, []float64{70, 30}},
		{"an idle Envoy counts as asking for a quarter of an equal share", 100, []float64{80, 0}, []float64{83.75, 16.25}},
		{"over the rate: equal shares for equal demands", 100, []float64{300, 300}, []float64{50, 50}},
		{"over the rate: the small demand is met, the rest goes to the large", 100, []float64{20, 500}, []float64{20, 80}},
		{"over the rate: a demand under the floor is counted as the floor", 100, []float64{10, 500}, []float64{12.5, 87.5}},
		{"over the rate, three ways", 90, []float64{10, 50, 200}, []float64{10, 40, 40}},
		{"a key used on one host of many still gets nearly all of it", 1000, append([]float64{5000}, make([]float64, 9)...), append([]float64{775}, 25, 25, 25, 25, 25, 25, 25, 25, 25)},
	} {
		got := Divide(c.rate, c.demand)
		sum, same := 0.0, len(got) == len(c.want)
		for i := range got {
			sum += got[i]
			same = same && near(got[i], c.want[i])
		}
		if !same {
			t.Errorf("%s: Divide(%v, %v) = %v, want %v", c.what, c.rate, c.demand, got, c.want)
		}
		if !near(sum, c.rate) {
			t.Errorf("%s: shares add up to %v, not the rate %v", c.what, sum, c.rate)
		}
	}
	// Whatever the demands: the shares add up to the rate, nobody gets
	// nothing, and nobody who is being held back gets less than another.
	demands := [][]float64{{1, 2, 3}, {0, 0, 0, 0}, {1e6, 1, 0}, {33.3, 33.3, 33.4}, {250, 250, 250, 250, 0.001}}
	for _, d := range demands {
		for _, rate := range []float64{0.01, 1, 100, 1e5} {
			got := Divide(rate, d)
			sum, top := 0.0, 0.0
			for _, s := range got {
				sum += s
				top = math.Max(top, s)
			}
			if !near(sum/rate, 1) {
				t.Errorf("Divide(%v, %v) adds up to %v", rate, d, sum)
			}
			for i, s := range got {
				if s <= 0 {
					t.Errorf("Divide(%v, %v): share %d is %v", rate, d, i, s)
				}
				if s < d[i] && !near(s/top, 1) && s < top {
					t.Errorf("Divide(%v, %v): Envoy %d is held to %v while another has %v", rate, d, i, s, top)
				}
			}
		}
	}
	if got := Divide(100, nil); len(got) != 0 {
		t.Errorf("nobody to divide among: %v", got)
	}
}

func TestTokenBucket(t *testing.T) {
	for _, c := range []struct {
		rate  float64
		burst uint32
	}{{0.5, 1}, {20.0 / 3600, 20}, {1.0 / 3600, 1}, {10, 10}, {16.25, 33}, {83.75, 168}, {999.5, 2000}, {1000, 1000}, {123456.4, 5}, {1e6, 1}} {
		tb := tokenBucket(c.rate, c.burst)
		got := float64(tb.GetTokensPerFill().GetValue()) / tb.GetFillInterval().AsDuration().Seconds()
		// Envoy 1.40 reads the interval in whole seconds: a fraction of a
		// second would be read as none, and the rate as unlimited.
		if d := tb.GetFillInterval().AsDuration(); d < time.Second || d != d.Truncate(time.Second) {
			t.Errorf("tokenBucket(%v, %d): fill interval %v is not a whole number of seconds", c.rate, c.burst, d)
		}
		if !near(got/c.rate, 1) || tb.GetMaxTokens() != c.burst {
			t.Errorf("tokenBucket(%v, %d) = %v tokens per %v, max %d: %v per second", c.rate, c.burst,
				tb.GetTokensPerFill().GetValue(), tb.GetFillInterval().AsDuration(), tb.GetMaxTokens(), got)
		}
	}
	if b := burstShare(100, 25, 100); b != 25 {
		t.Errorf("a quarter of the rate should carry a quarter of the burst: %d", b)
	}
	if b := burstShare(4, 0.01, 100); b != 1 {
		t.Errorf("a share's burst is never under one request: %d", b)
	}
}

// envoy plays one Envoy's side of the quota stream.
type envoy struct {
	t      *testing.T
	stream rlqs.RateLimitQuotaService_StreamRateLimitQuotasClient
	cancel context.CancelFunc
	got    chan *rlqs.RateLimitQuotaResponse
	first  bool
}

func quotaServer(t *testing.T, rules ...Rule) (*Server, func() *envoy) {
	t.Helper()
	if err := Validate(rules); err != nil {
		t.Fatal(err)
	}
	s := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.SetRules(rules)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := grpc.NewServer()
	s.Register(g)
	go func() { _ = g.Serve(ln) }()
	t.Cleanup(g.Stop)
	conn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return s, func() *envoy {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		st, err := rlqs.NewRateLimitQuotaServiceClient(conn).StreamRateLimitQuotas(ctx)
		if err != nil {
			t.Fatal(err)
		}
		e := &envoy{t: t, stream: st, cancel: cancel, got: make(chan *rlqs.RateLimitQuotaResponse, 64), first: true}
		go func() {
			for {
				m, err := st.Recv()
				if err != nil {
					close(e.got)
					return
				}
				e.got <- m
			}
		}()
		return e
	}
}

type use struct {
	rule, key       string
	allowed, denied uint64
	elapsed         time.Duration
}

func (e *envoy) report(uses ...use) {
	e.t.Helper()
	req := &rlqs.RateLimitQuotaUsageReports{}
	if e.first {
		req.Domain, e.first = Domain, false
	}
	for _, u := range uses {
		req.BucketQuotaUsages = append(req.BucketQuotaUsages, &rlqs.RateLimitQuotaUsageReports_BucketQuotaUsage{
			BucketId:    &rlqs.BucketId{Bucket: map[string]string{IDRule: u.rule, IDKey: u.key}},
			TimeElapsed: durationpb.New(u.elapsed), NumRequestsAllowed: u.allowed, NumRequestsDenied: u.denied,
		})
	}
	if err := e.stream.Send(req); err != nil {
		e.t.Fatal(err)
	}
}

// answer is what the daemon told an Envoy to do with one count.
type answer struct {
	rate    float64 // requests per second; 0 unless a token bucket
	burst   uint32
	deny    bool
	allow   bool
	abandon bool
	ttl     time.Duration
}

// next returns the daemon's next message as answers by "rule/key".
func (e *envoy) next() map[string]answer {
	e.t.Helper()
	select {
	case m, ok := <-e.got:
		if !ok {
			e.t.Fatal("the stream ended")
		}
		out := map[string]answer{}
		for _, a := range m.GetBucketAction() {
			id := a.GetBucketId().GetBucket()
			var ans answer
			switch {
			case a.GetAbandonAction() != nil:
				ans.abandon = true
			default:
				q := a.GetQuotaAssignmentAction()
				ans.ttl = q.GetAssignmentTimeToLive().AsDuration()
				if tb := q.GetRateLimitStrategy().GetTokenBucket(); tb != nil {
					ans.rate = float64(tb.GetTokensPerFill().GetValue()) / tb.GetFillInterval().AsDuration().Seconds()
					ans.burst = tb.GetMaxTokens()
				} else {
					ans.deny = q.GetRateLimitStrategy().GetBlanketRule() == typev3.RateLimitStrategy_DENY_ALL
					ans.allow = !ans.deny
				}
			}
			out[id[IDRule]+"/"+id[IDKey]] = ans
		}
		return out
	case <-time.After(5 * time.Second):
		e.t.Fatal("no message from the daemon")
		return nil
	}
}

func (e *envoy) quiet(what string) {
	e.t.Helper()
	select {
	case m := <-e.got:
		e.t.Errorf("%s: an unexpected message: %v", what, m)
	case <-time.After(150 * time.Millisecond):
	}
}

const five = 5 * time.Second

func TestSharesFollowDemand(t *testing.T) {
	s, connect := quotaServer(t,
		Rule{Name: "puts", Operation: []string{"PutObject"}, LimitPerSecond: 100, Burst: 200, Per: []string{PerAccessKey}})
	a, b := connect(), connect()

	// The first Envoy to use a key has the whole rate, at once.
	a.report(use{rule: "puts", key: "AK1", allowed: 1})
	if got := a.next()["puts/AK1"]; !near(got.rate, 100) || got.burst != 200 || got.ttl != assignmentTTL {
		t.Fatalf("first Envoy: %+v", got)
	}
	a.report(use{rule: "puts", key: "AK1", allowed: 400, elapsed: five}) // 80 a second
	if got := a.next()["puts/AK1"]; !near(got.rate, 100) {
		t.Fatalf("still alone: %+v", got)
	}

	// A second Envoy starts using the same key. It gets a share before it
	// has said how much it wants, and the first is told its new, smaller
	// share without having asked.
	b.report(use{rule: "puts", key: "AK1", allowed: 1})
	if got := b.next()["puts/AK1"]; !near(got.rate, 16.25) || got.burst != 33 {
		t.Fatalf("second Envoy on arrival: %+v", got)
	}
	if got := a.next()["puts/AK1"]; !near(got.rate, 83.75) {
		t.Fatalf("first Envoy after the second arrived: %+v", got)
	}
	// Another key is another count: the second Envoy has all of it.
	b.report(use{rule: "puts", key: "AK2", allowed: 1})
	if got := b.next()["puts/AK2"]; !near(got.rate, 100) {
		t.Fatalf("another key: %+v", got)
	}
	a.quiet("another key")

	// The second wants far more than there is: 16 a second allowed, 300
	// refused. The rate is now shared equally, and both are told.
	b.report(use{rule: "puts", key: "AK1", allowed: 81, denied: 1500, elapsed: five}, use{rule: "puts", key: "AK2", elapsed: five})
	if got := b.next(); !near(got["puts/AK1"].rate, 50) || got["puts/AK1"].burst != 100 || !near(got["puts/AK2"].rate, 100) {
		t.Fatalf("second Envoy, wanting more: %+v", got)
	}
	if got := a.next()["puts/AK1"]; !near(got.rate, 50) {
		t.Fatalf("first Envoy, now sharing equally: %+v", got)
	}
	// A report that changes nothing is answered to its sender, which
	// renews its share's time to live, and to nobody else.
	a.report(use{rule: "puts", key: "AK1", allowed: 250, denied: 150, elapsed: five})
	if got := a.next()["puts/AK1"]; !near(got.rate, 50) {
		t.Fatalf("first Envoy, no change: %+v", got)
	}
	b.quiet("no change")

	rules, _ := s.Status()
	st := rules["puts"]
	if !near(st.AllowedRate, 50+16.2) || !near(st.DeniedRate, 30+300) || st.Keys != 2 || st.Envoys != 2 ||
		st.Totals.Allowed != 1+400+1+1+81+250 || st.Totals.Denied != 1650 || len(st.Top) != 2 || st.Top[0].Key != "AK1" || st.Top[0].Envoys != 2 {
		t.Errorf("status: %+v", st)
	}

	// The second Envoy goes away: the first is given the whole rate again.
	b.cancel()
	if got := a.next()["puts/AK1"]; !near(got.rate, 100) || got.burst != 200 {
		t.Fatalf("first Envoy, alone again: %+v", got)
	}

	// The operator halves the rate: told at once. Then removes the rule:
	// the Envoy is told to forget the count.
	s.SetRules([]Rule{{Name: "puts", Operation: []string{"PutObject"}, LimitPerSecond: 50, Burst: 50, Per: []string{PerAccessKey}, Respond: 503}})
	if got := a.next()["puts/AK1"]; !near(got.rate, 50) || got.burst != 50 {
		t.Fatalf("after the rate was halved: %+v", got)
	}
	s.SetRules(nil)
	if got := a.next()["puts/AK1"]; !got.abandon {
		t.Fatalf("after the rule was removed: %+v", got)
	}
	// An Envoy that still has the old config and reports the rule is not
	// held back by a rule that no longer exists.
	a.report(use{rule: "puts", key: "AK1", allowed: 5, elapsed: five})
	if got := a.next()["puts/AK1"]; !got.allow {
		t.Fatalf("a rule that is gone: %+v", got)
	}
}

func TestOtherKindsOfRule(t *testing.T) {
	s, connect := quotaServer(t,
		Rule{Name: "per-host", LimitPerSecond: 40, Per: []string{PerAccessKey, PerClient}},
		Rule{Name: "blocked", Operation: []string{"DeleteObject"}, Blocked: true},
		Rule{Name: "hourly", Operation: []string{"List*"}, LimitPerHour: 20, Per: []string{PerTotal}})
	a, b := connect(), connect()

	// Counted per client host: every Envoy has the whole rate, however
	// many use the key.
	a.report(use{rule: "per-host", key: "AK1", allowed: 1})
	b.report(use{rule: "per-host", key: "AK1", allowed: 1})
	if ga, gb := a.next()["per-host/AK1"], b.next()["per-host/AK1"]; !near(ga.rate, 40) || !near(gb.rate, 40) || ga.burst != 40 {
		t.Errorf("per client host: %+v and %+v", ga, gb)
	}
	a.quiet("per client host")

	// Blocked: refuse everything.
	a.report(use{rule: "blocked", key: "*", denied: 1})
	if got := a.next()["blocked/*"]; !got.deny {
		t.Errorf("blocked: %+v", got)
	}

	// 20 an hour: a slow refill, with all 20 available at once.
	a.report(use{rule: "hourly", key: "*", allowed: 1})
	if got := a.next()["hourly/*"]; !near(got.rate*3600, 20) || got.burst != 20 {
		t.Errorf("20 per hour: %+v (%v an hour)", got, got.rate*3600)
	}

	// A count nobody uses is given up after six silent reports, so that
	// neither side remembers every key ever seen.
	for i := 0; i < idleReports-1; i++ {
		a.report(use{rule: "hourly", key: "*", elapsed: five})
		if got := a.next()["hourly/*"]; got.abandon {
			t.Fatalf("given up after %d silent reports", i+1)
		}
	}
	a.report(use{rule: "hourly", key: "*", elapsed: five})
	if got := a.next()["hourly/*"]; !got.abandon {
		t.Errorf("after %d silent reports: %+v", idleReports, got)
	}
	if rules, _ := s.Status(); rules["hourly"].Keys != 0 || rules["per-host"].Keys != 1 || rules["per-host"].Envoys != 2 {
		t.Errorf("status after giving up a count: %+v", rules)
	}

	// A stream for some other domain is refused.
	c := connect()
	c.first = false
	if err := c.stream.Send(&rlqs.RateLimitQuotaUsageReports{Domain: "someone-else"}); err != nil {
		t.Fatal(err)
	}
	select {
	case _, ok := <-c.got:
		if ok {
			t.Error("a report for another domain was answered")
		}
	case <-time.After(5 * time.Second):
		t.Error("a stream for another domain was left open")
	}
}

// A rule that counts per access key is handed a new count for every value a
// client cares to send. Past a bound the daemon stops remembering them, and
// does not limit them.
func TestKeysAreBounded(t *testing.T) {
	s, connect := quotaServer(t, Rule{Name: "r", LimitPerSecond: 1, Per: []string{PerAccessKey}})
	s.mu.Lock()
	s.keys["r"] = maxKeysPerRule
	s.mu.Unlock()
	a := connect()
	a.report(use{rule: "r", key: "one-too-many", allowed: 1})
	if got := a.next()["r/one-too-many"]; !got.allow {
		t.Errorf("past the bound: %+v", got)
	}
	if _, overflow := s.Status(); overflow != 1 {
		t.Errorf("overflow = %d", overflow)
	}
}
