package ratelimit

import (
	"math"
	"sort"

	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// idleHeld is the part of a rule's rate that may be held, in total, by
// Envoys that have a count open and are sending nothing.
const idleHeld = 0.10

// Divide splits a rate among the Envoys that are using one count of a rule,
// by what each is asking for. demand[i] is Envoy i's recent requests per
// second, allowed and refused together. The shares it returns add up to
// rate.
//
// While the Envoys together ask for less than the rate, each gets what it
// asks for and an equal part of what is left over, so one that speeds up
// has room at once. When they ask for more, the rate is shared as evenly as
// their demands allow: nobody gets more than it asks for, and those who ask
// for the most get equal shares (max-min fairness).
//
// An Envoy that asks for nothing is counted as asking for a little: it has
// the count open and may send at any moment, and with no share at all its
// first requests would be refused until its next report. The little is a
// tenth of the rate divided by the number of Envoys, so that however many
// sit idle, those that are busy keep nine tenths between them.
func Divide(rate float64, demand []float64) []float64 {
	n := len(demand)
	if n == 0 || rate <= 0 {
		return make([]float64, n)
	}
	floor := idleHeld * rate / float64(n)
	want := make([]float64, n)
	total := 0.0
	for i, d := range demand {
		want[i] = math.Max(d, floor)
		total += want[i]
	}
	shares := make([]float64, n)
	if total <= rate {
		spare := (rate - total) / float64(n)
		for i := range shares {
			shares[i] = want[i] + spare
		}
		return shares
	}
	// More is wanted than there is. Serve the smallest demands in full
	// while an equal split of what remains covers them; the rest share
	// what is left equally.
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return want[order[a]] < want[order[b]] })
	remaining := rate
	for k, i := range order {
		equal := remaining / float64(n-k)
		if want[i] > equal {
			for _, j := range order[k:] {
				shares[j] = equal
			}
			break
		}
		shares[i] = want[i]
		remaining -= want[i]
	}
	return shares
}

// DivideBurst splits a rule's burst among Envoys in proportion to their
// shares of its rate. Every Envoy gets at least one: an Envoy with a burst
// of none could never let a request through. So the parts add up to burst
// exactly while there are no more Envoys than burst, and to the number of
// Envoys once there are more, which is more than the rule says: see
// Status.MostEnvoys.
func DivideBurst(burst uint32, shares []float64) []uint32 {
	n := len(shares)
	out := make([]uint32, n)
	if n == 0 {
		return out
	}
	total := 0.0
	for _, s := range shares {
		total += s
	}
	if uint64(n) >= uint64(burst) || total <= 0 {
		for i := range out {
			out[i] = 1
		}
		return out
	}
	// In proportion, rounded by largest remainder so the parts add up.
	type frac struct {
		i int
		f float64
	}
	fracs := make([]frac, n)
	var given uint32
	for i, s := range shares {
		exact := float64(burst) * s / total
		whole := math.Floor(exact)
		out[i] = uint32(whole)
		given += out[i]
		fracs[i] = frac{i, exact - whole}
	}
	sort.SliceStable(fracs, func(a, b int) bool { return fracs[a].f > fracs[b].f })
	for k := 0; given < burst && k < n; k++ {
		out[fracs[k].i]++
		given++
	}
	// Then one for anybody left with none, from whoever has the most.
	for i := range out {
		if out[i] > 0 {
			continue
		}
		most := 0
		for j := range out {
			if out[j] > out[most] {
				most = j
			}
		}
		if out[most] > 1 {
			out[most]--
		}
		out[i] = 1
	}
	return out
}

// maxFillSeconds is the longest interval a token bucket is described over:
// a hundred years, so a share as small as one request a century can be
// said. An idle Envoy's share of a slow rule on a large fleet is very
// small, and rounding each up to something sayable in an hour would add up
// to several times the rule.
const maxFillSeconds = 100 * 365 * 86400

// tokenBucket describes a rate and a burst to Envoy, which refills a bucket
// continuously at tokens_per_fill / fill_interval.
//
// The interval is a whole number of seconds because Envoy 1.40 drops the
// fraction when it reads it (global_client_impl.cc, ToInt64Seconds): half a
// second would become zero, and a division by zero an unlimited rate. A
// rate that is not a whole number per second is therefore written as more
// tokens over a longer interval, chosen so that rounding to whole tokens
// and whole seconds costs under one percent.
func tokenBucket(rate float64, burst uint32) *typev3.TokenBucket {
	tokens, seconds := 0.0, 0.0
	for _, n := range []float64{1, 10, 100, 1000} {
		if rate*n >= 100 {
			tokens, seconds = math.Round(rate*n), n
			break
		}
	}
	if seconds == 0 {
		// Under one request in ten seconds: a few tokens over however many
		// seconds that takes, at least a hundred.
		tokens = math.Max(1, math.Ceil(rate*100))
		seconds = maxFillSeconds
		if rate > 0 {
			seconds = math.Min(math.Max(1, math.Round(tokens/rate)), maxFillSeconds)
		}
	}
	return &typev3.TokenBucket{
		MaxTokens:     max(burst, 1),
		TokensPerFill: wrapperspb.UInt32(uint32(math.Min(tokens, math.MaxUint32))),
		FillInterval:  &durationpb.Duration{Seconds: int64(seconds)},
	}
}

// AllowAll is the strategy of a count that has no share from the daemon:
// requests pass. It cannot be a rate. Envoy 1.40 has no token bucket for a
// count until the daemon assigns one, and uses it regardless (filter.cc,
// shouldAllowRequest): a rate here would stop Envoy.
func AllowAll() *typev3.RateLimitStrategy {
	return &typev3.RateLimitStrategy{Strategy: &typev3.RateLimitStrategy_BlanketRule_{BlanketRule: typev3.RateLimitStrategy_ALLOW_ALL}}
}
