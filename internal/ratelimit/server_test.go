package ratelimit

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"testing"
	"time"

	rlqs "github.com/envoyproxy/go-control-plane/envoy/service/rate_limit_quota/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
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
		{"an idle Envoy counts as asking for a little", 100, []float64{80, 0}, []float64{87.5, 12.5}},
		{"over the rate: equal shares for equal demands", 100, []float64{300, 300}, []float64{50, 50}},
		{"over the rate: the small demand is met, the rest goes to the large", 100, []float64{20, 500}, []float64{20, 80}},
		{"over the rate: a demand under the floor is counted as the floor", 100, []float64{2, 500}, []float64{5, 95}},
		{"over the rate, three ways", 90, []float64{10, 50, 200}, []float64{10, 40, 40}},
		{"a key used on one host of many gets nine tenths", 1000, append([]float64{5000}, make([]float64, 9)...), append([]float64{910}, 10, 10, 10, 10, 10, 10, 10, 10, 10)},
		{"however many sit idle", 1000, append([]float64{5000}, make([]float64, 99)...), nil},
	} {
		got := Divide(c.rate, c.demand)
		sum, same := 0.0, c.want == nil || len(got) == len(c.want)
		for i := range got {
			sum += got[i]
			same = same && (c.want == nil || near(got[i], c.want[i]))
		}
		if !same {
			t.Errorf("%s: Divide(%v, %v) = %v, want %v", c.what, c.rate, c.demand, got, c.want)
		}
		if !near(sum, c.rate) {
			t.Errorf("%s: shares add up to %v, not the rate %v", c.what, sum, c.rate)
		}
		if c.demand[0] >= c.rate && len(got) > 1 && got[0] < 0.9*c.rate-0.01 && c.demand[1] == 0 {
			t.Errorf("%s: the one busy Envoy has %v of %v", c.what, got[0], c.rate)
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

func TestDivideBurst(t *testing.T) {
	for _, c := range []struct {
		what   string
		burst  uint32
		shares []float64
		want   []uint32
	}{
		{"one Envoy has it all", 200, []float64{100}, []uint32{200}},
		{"in proportion to the shares", 100, []float64{75, 25}, []uint32{75, 25}},
		{"in proportion, to the nearest request, adding up exactly", 200, []float64{83.7, 16.3}, []uint32{167, 33}},
		{"nobody is left with none", 4, []float64{0.5, 0.3, 0.2}, []uint32{2, 1, 1}},
		{"as many Envoys as the burst: one each", 3, []float64{0.5, 0.3, 0.2}, []uint32{1, 1, 1}},
		{"more Envoys than the burst: still one each, which is more than the burst", 3, []float64{1, 1, 1, 1, 1}, []uint32{1, 1, 1, 1, 1}},
	} {
		got := DivideBurst(c.burst, c.shares)
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%s: DivideBurst(%d, %v) = %v, want %v", c.what, c.burst, c.shares, got, c.want)
		}
	}
	// While there are no more Envoys than the burst, the parts add up to it.
	for n := 1; n <= 40; n++ {
		shares := Divide(1000, append([]float64{700, 200, 55}, make([]float64, n-1)...)[:n])
		var sum uint32
		for _, b := range DivideBurst(40, shares) {
			if b == 0 {
				t.Fatalf("%d Envoys: one has no burst", n)
			}
			sum += b
		}
		if sum != 40 {
			t.Errorf("%d Envoys: bursts add up to %d, not 40", n, sum)
		}
	}
}

func TestTokenBucket(t *testing.T) {
	for _, c := range []struct {
		rate  float64
		burst uint32
	}{{0.5, 1}, {20.0 / 3600, 20}, {1.0 / 3600, 1}, {10, 10}, {16.25, 33}, {83.75, 168}, {999.5, 2000}, {1000, 1000}, {123456.4, 5}, {1e6, 1},
		// Small shares of slow rules: an idle Envoy's part on a large fleet.
		{0.095, 1}, {1.4 / 3600, 1}, {0.04 / 3600, 1}, {1e-7, 1}, {3e-9, 1}} {
		tb := tokenBucket(c.rate, c.burst)
		// Envoy 1.40 reads the interval in whole seconds: a fraction of a
		// second would be read as none, and the rate as unlimited.
		if tb.GetFillInterval().GetSeconds() < 1 || tb.GetFillInterval().GetNanos() != 0 {
			t.Errorf("tokenBucket(%v, %d): fill interval %v is not a whole number of seconds", c.rate, c.burst, tb.GetFillInterval())
		}
		got := float64(tb.GetTokensPerFill().GetValue()) / float64(tb.GetFillInterval().GetSeconds())
		if !near(got/c.rate, 1) || tb.GetMaxTokens() != c.burst {
			t.Errorf("tokenBucket(%v, %d) = %v tokens per %d s, max %d: %v per second", c.rate, c.burst,
				tb.GetTokensPerFill().GetValue(), tb.GetFillInterval().GetSeconds(), tb.GetMaxTokens(), got)
		}
	}
	// Nothing Envoy could divide by zero, whatever it is asked to say.
	for _, rate := range []float64{0, 1e-30} {
		if tb := tokenBucket(rate, 0); tb.GetFillInterval().GetSeconds() < 1 || tb.GetTokensPerFill().GetValue() < 1 || tb.GetMaxTokens() < 1 {
			t.Errorf("tokenBucket(%v, 0) = %v", rate, tb)
		}
	}
}

// clock is the daemon's clock in these tests: it moves when told to.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

// envoy plays one Envoy's side of the quota service, without the network:
// reports go straight in, and answers are read from where the stream's
// sender would take them.
type envoy struct {
	t  *testing.T
	s  *Server
	st *stream
}

func quotaServer(t *testing.T, rules ...Rule) (*Server, *clock, func() *envoy) {
	t.Helper()
	if err := Validate(rules); err != nil {
		t.Fatal(err)
	}
	s := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)))
	clk := &clock{t: time.Unix(1_800_000_000, 0)}
	s.now = clk.now
	s.SetRules(rules)
	return s, clk, func() *envoy { return &envoy{t: t, s: s, st: s.open("test")} }
}

type use struct {
	rule, key       string
	allowed, denied uint64
}

func usage(uses ...use) *rlqs.RateLimitQuotaUsageReports {
	req := &rlqs.RateLimitQuotaUsageReports{Domain: Domain}
	for _, u := range uses {
		req.BucketQuotaUsages = append(req.BucketQuotaUsages, &rlqs.RateLimitQuotaUsageReports_BucketQuotaUsage{
			BucketId:           &rlqs.BucketId{Bucket: map[string]string{IDRule: u.rule, IDKey: u.key}},
			NumRequestsAllowed: u.allowed, NumRequestsDenied: u.denied,
		})
	}
	return req
}

func (e *envoy) report(uses ...use) { e.s.report(e.st, usage(uses...)) }
func (e *envoy) leave()             { e.s.closed(e.st) }

// answer is what the daemon told an Envoy to do with one count.
type answer struct {
	rate    float64 // requests per second
	burst   uint32
	abandon bool
	ttl     time.Duration
}

func answers(m *rlqs.RateLimitQuotaResponse) map[string]answer {
	out := map[string]answer{}
	for _, a := range m.GetBucketAction() {
		id := a.GetBucketId().GetBucket()
		var ans answer
		if a.GetAbandonAction() != nil {
			ans.abandon = true
		} else {
			q := a.GetQuotaAssignmentAction()
			ans.ttl = q.GetAssignmentTimeToLive().AsDuration()
			tb := q.GetRateLimitStrategy().GetTokenBucket()
			ans.rate = float64(tb.GetTokensPerFill().GetValue()) / float64(tb.GetFillInterval().GetSeconds())
			ans.burst = tb.GetMaxTokens()
		}
		out[id[IDRule]+"/"+id[IDKey]] = ans
	}
	return out
}

// next returns the daemon's next message as answers by "rule/key".
func (e *envoy) next() map[string]answer {
	e.t.Helper()
	select {
	case m := <-e.st.send:
		return answers(m)
	default:
		e.t.Fatal("no message from the daemon")
		return nil
	}
}

// drain returns everything the daemon has sent and not yet been read.
func (e *envoy) drain() []map[string]answer {
	var out []map[string]answer
	for {
		select {
		case m := <-e.st.send:
			out = append(out, answers(m))
		default:
			return out
		}
	}
}

func (e *envoy) quiet(what string) {
	e.t.Helper()
	if got := e.drain(); len(got) != 0 {
		e.t.Errorf("%s: unexpected messages: %v", what, got)
	}
}

const five = 5 * time.Second

func TestSharesFollowDemand(t *testing.T) {
	s, clk, connect := quotaServer(t,
		Rule{Name: "puts", Operation: []string{"PutObject"}, LimitPerSecond: 100, Burst: 200, Per: []string{PerAccessKey}})
	a, b := connect(), connect()

	// The first Envoy to use a key has the whole rate, at once.
	a.report(use{rule: "puts", key: "AK1", allowed: 1})
	if got := a.next()["puts/AK1"]; !near(got.rate, 100) || got.burst != 200 || got.ttl != AssignmentTTL {
		t.Fatalf("first Envoy: %+v", got)
	}
	// A report that changes nothing is not answered: the share it has is
	// good for minutes.
	clk.advance(five)
	a.report(use{rule: "puts", key: "AK1", allowed: 399}) // with the first, 80 a second
	a.quiet("still alone")

	// A second Envoy starts using the same key. It gets a share before it
	// has said how much it wants, and the first is told its new, smaller
	// share without having asked.
	b.report(use{rule: "puts", key: "AK1", allowed: 1})
	if got := b.next()["puts/AK1"]; !near(got.rate, 12.5) || got.burst != 25 {
		t.Fatalf("second Envoy on arrival: %+v", got)
	}
	if got := a.next()["puts/AK1"]; !near(got.rate, 87.5) || got.burst != 175 {
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
	clk.advance(five)
	b.report(use{rule: "puts", key: "AK1", allowed: 79, denied: 1500}, use{rule: "puts", key: "AK2"})
	got := b.next()
	if !near(got["puts/AK1"].rate, 50) || got["puts/AK1"].burst != 100 {
		t.Fatalf("second Envoy, wanting more: %+v", got)
	}
	if _, again := got["puts/AK2"]; again {
		t.Errorf("a share that has not changed was sent again: %+v", got)
	}
	if got := a.next()["puts/AK1"]; !near(got.rate, 50) || got.burst != 100 {
		t.Fatalf("first Envoy, now sharing equally: %+v", got)
	}
	// A report that changes nothing is answered to nobody.
	clk.advance(time.Second)
	a.report(use{rule: "puts", key: "AK1", allowed: 250, denied: 230}) // six seconds' worth: still 80 a second
	a.quiet("no change")
	b.quiet("no change")

	rules, _ := s.Status()
	st := rules["puts"]
	if !near(st.AllowedRate, 250.0/6+16+0.2) || !near(st.DeniedRate, 230.0/6+300) || st.Keys != 2 || st.Envoys != 2 || st.MostEnvoys != 2 ||
		st.Totals.Allowed != 1+399+1+1+79+250 || st.Totals.Denied != 1730 || len(st.Top) != 2 || st.Top[0].Key != "AK1" || st.Top[0].Envoys != 2 {
		t.Errorf("status: %+v", st)
	}

	// The second Envoy goes away: the first is given the whole rate again.
	b.leave()
	if got := a.next()["puts/AK1"]; !near(got.rate, 100) || got.burst != 200 {
		t.Fatalf("first Envoy, alone again: %+v", got)
	}

	// The operator halves the rate: told at once. A change to some other
	// rule is no news. Then the rule is removed: the Envoy is told to
	// forget the count.
	halved := Rule{Name: "puts", Operation: []string{"PutObject"}, LimitPerSecond: 50, Burst: 50, Per: []string{PerAccessKey}, Respond: 503}
	s.SetRules([]Rule{halved})
	if got := a.next()["puts/AK1"]; !near(got.rate, 50) || got.burst != 50 {
		t.Fatalf("after the rate was halved: %+v", got)
	}
	s.SetRules([]Rule{halved, {Name: "other", LimitPerSecond: 1, Burst: 1, Per: []string{PerTotal}, Respond: 503}})
	a.quiet("another rule was added")
	s.SetRules(nil)
	if got := a.next()["puts/AK1"]; !got.abandon {
		t.Fatalf("after the rule was removed: %+v", got)
	}
	// An Envoy that reports a rule that no longer exists is told to forget
	// the count, not left to report it for ever.
	a.report(use{rule: "puts", key: "AK1", allowed: 5})
	if got := a.next()["puts/AK1"]; !got.abandon {
		t.Fatalf("a rule that is gone: %+v", got)
	}
	if rules, _ := s.Status(); len(rules) != 0 {
		t.Errorf("status with no rules: %+v", rules)
	}
}

// A share lasts ten minutes and is renewed well before that, so an Envoy
// that cannot reach the daemon for a while goes on with what it has.
func TestSharesAreRenewed(t *testing.T) {
	_, clk, connect := quotaServer(t, Rule{Name: "r", LimitPerSecond: 10, Per: []string{PerTotal}})
	a := connect()
	a.report(use{rule: "r", key: "*", allowed: 1})
	if got := a.next()["r/*"]; got.ttl != AssignmentTTL || AssignmentTTL < 4*renewAfter {
		t.Fatalf("first share: %+v", got)
	}
	sent := 0
	for at := five; at <= AssignmentTTL; at += five {
		clk.advance(five)
		a.report(use{rule: "r", key: "*", allowed: 40})
		for _, m := range a.drain() {
			if got := m["r/*"]; !near(got.rate, 10) || got.ttl != AssignmentTTL {
				t.Fatalf("after %v: %+v", at, got)
			}
			sent++
		}
	}
	if sent != 4 {
		t.Errorf("a steady count's share was sent %d times in %v, want 4", sent, AssignmentTTL)
	}
}

func TestOtherKindsOfRule(t *testing.T) {
	s, clk, connect := quotaServer(t,
		Rule{Name: "per-host", LimitPerSecond: 40, Per: []string{PerAccessKey, PerClient}},
		Rule{Name: "blocked", Operation: []string{"DeleteObject"}, Blocked: true},
		Rule{Name: "hourly", Operation: []string{"List*"}, LimitPerHour: 20, Per: []string{PerTotal}})
	a, b := connect(), connect()

	// Counted per client host: every Envoy has the whole rate, however
	// many use the key.
	a.report(use{rule: "per-host", key: "AK1", allowed: 1})
	b.report(use{rule: "per-host", key: "AK1", allowed: 1})
	if ga, gb := a.next()["per-host/AK1"], b.next()["per-host/AK1"]; !near(ga.rate, 40) || !near(gb.rate, 40) || ga.burst != 40 || gb.burst != 40 {
		t.Errorf("per client host: %+v and %+v", ga, gb)
	}
	a.quiet("per client host")

	// Blocked rules are refused by the script in each Envoy, which reports
	// no count of them. One that does, left from before the rule was
	// blocked, is told to forget it. What the daemon knows of the refusals
	// it is told one at a time.
	a.report(use{rule: "blocked", key: "*", denied: 1})
	if got := a.next()["blocked/*"]; !got.abandon {
		t.Errorf("a count of a blocked rule: %+v", got)
	}
	var batch []Refusal
	for i := 0; i < 10; i++ {
		batch = append(batch, Refusal{Client: "host-1", Target: "s3", Blocked: true, Rules: []string{"blocked"}, Operation: "DeleteObject", AccessKey: "AK1", Bucket: "archive", Status: 503})
	}
	batch = append(batch,
		// Over a limit: its count comes in the Envoy's reports, not here.
		Refusal{Client: "host-2", Target: "s3", Rules: []string{"per-host"}, Operation: "GetObject", AccessKey: "AK1", Bucket: "data", Status: 503},
		Refusal{Client: "host-2", Target: "s3", Blocked: true, Rules: []string{"no-such-rule"}, Status: 503})
	s.Refusals(batch)
	if got := s.Recent(2); len(got) != 2 || got[0].Rules[0] != "no-such-rule" || got[1].Rules[0] != "per-host" || got[1].At.IsZero() || len(s.Recent(0)) != 12 {
		t.Errorf("the latest refusals, newest first: %+v", got)
	}
	clk.advance(time.Second)
	rules, _ := s.Status()
	if st := rules["blocked"]; st.Totals.Denied != 10 || !near(st.DeniedRate, 2) || st.Keys != 0 {
		t.Errorf("blocked rule's status: %+v", st)
	}
	if st := rules["per-host"]; st.Totals.Denied != 0 || st.Keys != 1 || st.Envoys != 2 || st.MostEnvoys != 2 {
		t.Errorf("per-host rule's status: %+v", st)
	}
	if r := s.rules["per-host"]; !r.Holds(rules["per-host"]) {
		t.Error("a rule counted per client host holds however many hosts there are")
	}

	// A count nobody uses is given up, so that neither side remembers
	// every key ever seen: here after six silent reports.
	for i := 1; i <= 6; i++ {
		clk.advance(five)
		a.report(use{rule: "per-host", key: "AK1"})
		b.report(use{rule: "per-host", key: "AK1", allowed: 10})
		got := a.drain()
		if gone := len(got) == 1 && got[0]["per-host/AK1"].abandon; gone != (i == 6) {
			t.Fatalf("after %d silent reports: %v", i, got)
		}
	}
	b.quiet("the other Envoy still uses it")
	clk.advance(time.Second)
	if rules, _ := s.Status(); rules["per-host"].Keys != 1 || rules["per-host"].Envoys != 1 {
		t.Errorf("status after one Envoy gave up a count: %+v", rules["per-host"])
	}
}

// A count is not given up while the tokens its Envoy holds are still
// refilling. If it were, a client could spend a slow rule's burst, pause
// for half a minute, and find it whole again.
func TestSlowRuleIsNotBeatenByPausing(t *testing.T) {
	_, clk, connect := quotaServer(t, Rule{Name: "hourly", LimitPerHour: 6, Burst: 3, Per: []string{PerTotal}})
	a := connect()
	a.report(use{rule: "hourly", key: "*", allowed: 1})
	if got := a.next()["hourly/*"]; !near(got.rate*3600, 6) || got.burst != 3 {
		t.Fatalf("6 per hour: %+v (%v an hour)", got, got.rate*3600)
	}
	clk.advance(five)
	a.report(use{rule: "hourly", key: "*", allowed: 2, denied: 5}) // the burst is spent
	// Three tokens at six an hour take half an hour to come back.
	const refill = 30 * time.Minute
	var idle time.Duration
	for idle = five; ; idle += five {
		clk.advance(five)
		a.report(use{rule: "hourly", key: "*"})
		gone := false
		for _, m := range a.drain() {
			gone = gone || m["hourly/*"].abandon
		}
		if gone {
			break
		}
		if idle > 2*refill {
			t.Fatal("the count was never given up")
		}
	}
	if idle < refill {
		t.Errorf("the count was given up after %v of silence; its burst takes %v to refill", idle, refill)
	}
	if idle > refill+refill/5 {
		t.Errorf("the count was kept for %v of silence; its burst takes %v to refill", idle, refill)
	}
}

// The rate is held however many Envoys share a count. The burst is not,
// once there are more Envoys than the burst: each needs one request's worth
// to be able to pass anything. The rule's status says so.
func TestManyEnvoysOnASlowRule(t *testing.T) {
	rule := Rule{Name: "hourly", LimitPerHour: 20, Per: []string{PerTotal}}
	for _, n := range []int{10, 50, 400} {
		s, clk, connect := quotaServer(t, rule)
		envoys := make([]*envoy, n)
		for i := range envoys {
			envoys[i] = connect()
			envoys[i].report(use{rule: "hourly", key: "*", allowed: 1})
		}
		clk.advance(five)
		for _, e := range envoys {
			e.report(use{rule: "hourly", key: "*"})
		}
		rate, burst := 0.0, uint32(0)
		for _, e := range envoys {
			var last answer
			for _, m := range e.drain() {
				last = m["hourly/*"]
			}
			if last.burst == 0 {
				t.Fatalf("%d Envoys: one was sent no share", n)
			}
			rate, burst = rate+last.rate, burst+last.burst
		}
		if !near(rate*3600/20, 1) {
			t.Errorf("%d Envoys: the shares add up to %.2f an hour, not 20", n, rate*3600)
		}
		wantBurst := uint32(max(n, 20))
		if burst != wantBurst {
			t.Errorf("%d Envoys: the bursts add up to %d, want %d", n, burst, wantBurst)
		}
		clk.advance(time.Second)
		rules, _ := s.Status()
		r := s.rules["hourly"]
		if st := rules["hourly"]; st.MostEnvoys != n || r.Holds(st) != (n <= 20) {
			t.Errorf("%d Envoys: status %+v, holds %v", n, st, r.Holds(st))
		}
	}
}

// A count that every Envoy has open costs the daemon about the same work
// for each round of reports, not work that grows with the square of the
// fleet; and a report that changes nothing sends nothing to anyone.
func TestWorkDoesNotGrowWithTheSquareOfTheFleet(t *testing.T) {
	const n = 500
	s, clk, connect := quotaServer(t, Rule{Name: "r", LimitPerSecond: 1000, Per: []string{PerTotal}})
	envoys := make([]*envoy, n)
	for i := range envoys {
		envoys[i] = connect()
		envoys[i].report(use{rule: "r", key: "*", allowed: 1})
	}
	for round := 0; round < 4; round++ {
		clk.advance(five)
		for _, e := range envoys {
			e.report(use{rule: "r", key: "*", allowed: 5})
		}
	}
	t.Logf("%d Envoys, five rounds of reports: the count's shares were worked out %d times", n, s.divided)
	if s.divided > fewEnvoys+8 {
		t.Errorf("the count's shares were worked out %d times for %d Envoys over five rounds of reports", s.divided, n)
	}
	rate, sent := 0.0, 0
	for _, e := range envoys {
		var last answer
		for _, m := range e.drain() {
			last = m["r/*"]
			sent++
		}
		rate += last.rate
	}
	if !near(rate/1000, 1) {
		t.Errorf("the shares the Envoys hold add up to %.1f a second, not 1000", rate)
	}
	// Each was told on arrival, and again as the count filled up; steady
	// rounds after that are silent.
	before := sent
	clk.advance(five)
	for _, e := range envoys {
		e.report(use{rule: "r", key: "*", allowed: 5})
	}
	for _, e := range envoys {
		sent += len(e.drain())
	}
	if sent != before {
		t.Errorf("a round of reports that changed nothing sent %d messages", sent-before)
	}
}

// A rule that counts per access key is handed a new count for every value a
// client cares to send. Past a bound the daemon stops remembering them, and
// does not limit them; nor does it leave the Envoy remembering them.
func TestKeysAreBounded(t *testing.T) {
	s, _, connect := quotaServer(t, Rule{Name: "r", LimitPerSecond: 1, Per: []string{PerAccessKey}})
	s.keys["r"] = maxKeysPerRule
	a := connect()
	a.report(use{rule: "r", key: "one-too-many", allowed: 1})
	if got := a.next()["r/one-too-many"]; !got.abandon {
		t.Errorf("past the bound: %+v", got)
	}
	if _, overflow := s.Status(); overflow != 1 {
		t.Errorf("overflow = %d", overflow)
	}
}

// What no report will come for again is dropped all the same.
func TestCountsOfSilentEnvoysAreDropped(t *testing.T) {
	s, clk, connect := quotaServer(t, Rule{Name: "r", LimitPerSecond: 10, Per: []string{PerAccessKey}})
	a, b := connect(), connect()
	a.report(use{rule: "r", key: "AK1", allowed: 1})
	b.report(use{rule: "r", key: "AK2", allowed: 1})
	// a stops reporting AK1 without closing its stream; b carries on.
	for i := 0; i < 8; i++ {
		clk.advance(five)
		b.report(use{rule: "r", key: "AK2", allowed: 1})
	}
	if _, ok := s.buckets[bucketKey{"r", "AK1"}]; ok || s.keys["r"] != 1 {
		t.Errorf("a count nobody reports is still remembered: %d keys", s.keys["r"])
	}
}

// The same service over the wire, as Envoy reaches it.
func TestQuotaStream(t *testing.T) {
	rule := Rule{Name: "r", LimitPerSecond: 10, Per: []string{PerTotal}}
	if err := rule.Normalize(); err != nil {
		t.Fatal(err)
	}
	s := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.SetRules([]Rule{rule})
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	st, err := rlqs.NewRateLimitQuotaServiceClient(conn).StreamRateLimitQuotas(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Send(usage(use{rule: "r", key: "*", allowed: 1})); err != nil {
		t.Fatal(err)
	}
	m, err := st.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if got := answers(m)["r/*"]; !near(got.rate, 10) || got.burst != 10 {
		t.Errorf("over the wire: %+v", got)
	}

	// A stream for some other domain is refused.
	other, err := rlqs.NewRateLimitQuotaServiceClient(conn).StreamRateLimitQuotas(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Send(&rlqs.RateLimitQuotaUsageReports{Domain: "someone-else"}); err != nil {
		t.Fatal(err)
	}
	if m, err := other.Recv(); err == nil {
		t.Errorf("a report for another domain was answered: %v", m)
	}
}

// The daemon remembers the latest refusals one by one, and no more of them
// than it says.
func TestRecentRefusals(t *testing.T) {
	s, _, _ := quotaServer(t, Rule{Name: "stop", Blocked: true})
	if got := s.Recent(10); len(got) != 0 {
		t.Errorf("before any: %v", got)
	}
	for i := 0; i < recentRefusals+250; i++ {
		s.Refusals([]Refusal{{Client: "host", Blocked: true, Rules: []string{"stop"}, Status: 500 + i%100}, {Client: "host", Rules: []string{"other"}, Status: i}})
	}
	got := s.Recent(0)
	if len(got) != recentRefusals {
		t.Fatalf("%d remembered, want %d", len(got), recentRefusals)
	}
	last := recentRefusals + 250 - 1
	for i, r := range got {
		// Newest first: pairs of (other, stop) counting down.
		want := last - i/2
		if (i%2 == 0) != (r.Rules[0] == "other") || (r.Rules[0] == "other" && r.Status != want) {
			t.Fatalf("refusal %d from the newest is %+v", i, r)
		}
	}
	rules, _ := s.Status()
	if rules["stop"].Totals.Denied != recentRefusals+250 {
		t.Errorf("every refusal by a blocked rule is counted, remembered or not: %+v", rules["stop"])
	}
}
