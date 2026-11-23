package ratelimit

import (
	"io"
	"log/slog"
	"strconv"
	"testing"
	"time"
)

// T1: one steady round of reports on a count that every Envoy has open
// should cost work linear in the fleet. prune ranges over every part of
// the count under s.mu; it used to run on every report, so a round cost
// the square of the fleet. The parts it looks at are counted (s.pruned).
// Wall time and the longest single report() (which holds s.mu for its
// whole run) are logged too.
func TestReviewT1_PruneWorkPerRoundIsLinear(t *testing.T) {
	for _, n := range []int{100, 500, 1000} {
		s, clk, connect := quotaServer(t, Rule{Name: "r", LimitPerSecond: 1000, Per: []string{PerTotal}})
		envoys := make([]*envoy, n)
		for i := range envoys {
			envoys[i] = connect()
			envoys[i].report(use{rule: "r", key: "*", allowed: 1})
		}
		// Two settling rounds, so the measured one is steady state.
		for round := 0; round < 2; round++ {
			clk.advance(five)
			for _, e := range envoys {
				e.report(use{rule: "r", key: "*", allowed: 5})
				e.drain()
			}
		}
		clk.advance(five)
		before := s.pruned
		var total, longest time.Duration
		for _, e := range envoys {
			start := time.Now()
			e.report(use{rule: "r", key: "*", allowed: 5})
			d := time.Since(start)
			total += d
			longest = max(longest, d)
		}
		visits := int(s.pruned - before)
		for _, e := range envoys {
			e.drain()
		}
		t.Logf("n=%4d: prune visited %8d parts in one round (%.0f per report); round took %v under s.mu, mean %v/report, longest report %v",
			n, visits, float64(visits)/float64(n), total.Round(time.Microsecond), (total / time.Duration(n)).Round(time.Nanosecond), longest.Round(time.Microsecond))
		if visits > 4*n {
			t.Errorf("n=%d: one round of reports made prune visit %d parts; want work linear in the fleet (<= %d)", n, visits, 4*n)
		}
	}
}

// T1, the other way in: every Envoy leaving a shared count at once (the
// daemon's network blipped) must not cost the square of the fleet either.
// They leave over a second, so the shares are worked out again as they go.
func TestReviewT1_MassDisconnectIsLinear(t *testing.T) {
	const n = 1000
	s, clk, connect := quotaServer(t, Rule{Name: "r", LimitPerSecond: 1000, Per: []string{PerTotal}})
	envoys := make([]*envoy, n)
	for i := range envoys {
		envoys[i] = connect()
		envoys[i].report(use{rule: "r", key: "*", allowed: 1})
	}
	clk.advance(five)
	for _, e := range envoys {
		e.report(use{rule: "r", key: "*", allowed: 5})
	}
	before := s.pruned
	for _, e := range envoys {
		clk.advance(time.Millisecond)
		e.leave()
	}
	visits := int(s.pruned - before)
	t.Logf("%d Envoys left: prune visited %d parts", n, visits)
	if visits > 20*n {
		t.Errorf("%d Envoys leaving made prune visit %d parts; want work linear in the fleet (<= %d)", n, visits, 20*n)
	}
	if _, ok := s.buckets[bucketKey{"r", "*"}]; ok || s.keys["r"] != 0 {
		t.Errorf("the count is still remembered after every Envoy left")
	}
}

// The same round as a benchmark: one op is one whole round of n reports.
func BenchmarkReviewT1_SteadyRound(b *testing.B) {
	for _, n := range []int{100, 500, 1000, 2000, 4000} {
		b.Run("n="+strconv.Itoa(n), func(b *testing.B) {
			rules := []Rule{{Name: "r", LimitPerSecond: 1000, Per: []string{PerTotal}}}
			if err := Validate(rules); err != nil {
				b.Fatal(err)
			}
			s := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)))
			clk := &clock{t: time.Unix(1_800_000_000, 0)}
			s.now = clk.now
			s.SetRules(rules)
			sts := make([]*stream, n)
			for i := range sts {
				sts[i] = s.open("bench")
				s.report(sts[i], usage(use{rule: "r", key: "*", allowed: 1}))
			}
			drain := func() {
				for _, st := range sts {
					for len(st.send) > 0 {
						<-st.send
					}
				}
			}
			drain()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				clk.advance(five)
				for _, st := range sts {
					s.report(st, usage(use{rule: "r", key: "*", allowed: 5}))
				}
				b.StopTimer()
				drain()
				b.StartTimer()
			}
		})
	}
}
