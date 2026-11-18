package ratelimit

import (
	"math"
	"sort"
	"time"

	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

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
// An Envoy that asks for nothing is counted as asking for a quarter of an
// equal share. It has the count open and may send at any moment; with no
// share at all its first requests would be refused until its next report.
func Divide(rate float64, demand []float64) []float64 {
	n := len(demand)
	if n == 0 || rate <= 0 {
		return make([]float64, n)
	}
	floor := rate / float64(4*n)
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

// burstShare is the part of a rule's burst that goes with a share of its
// rate: in proportion, and at least one request.
func burstShare(burst uint32, share, rate float64) uint32 {
	if rate <= 0 {
		return 1
	}
	return uint32(math.Max(1, math.Round(float64(burst)*share/rate)))
}

// tokenBucket describes a rate and a burst to Envoy, which refills a bucket
// continuously at tokens_per_fill / fill_interval.
//
// The interval is a whole number of seconds because Envoy 1.40 drops the
// fraction when it reads it (global_client_impl.cc, ToInt64Seconds): half a
// second would become zero, and a division by zero an unlimited rate. A
// rate that is not a whole number per second is therefore written as more
// tokens over a longer interval, chosen so that rounding the tokens to a
// whole number costs under one percent.
func tokenBucket(rate float64, burst uint32) *typev3.TokenBucket {
	seconds := 3600.0
	for _, n := range []float64{1, 10, 100, 1000} {
		if rate*n >= 100 {
			seconds = n
			break
		}
	}
	tokens := math.Max(1, math.Min(math.Round(rate*seconds), math.MaxUint32))
	return &typev3.TokenBucket{
		MaxTokens:     max(burst, 1),
		TokensPerFill: wrapperspb.UInt32(uint32(tokens)),
		FillInterval:  durationpb.New(time.Duration(seconds) * time.Second),
	}
}

// Strategy is what Envoy does with a rule's matching requests once its
// share from the daemon has run out, which is to say when the daemon cannot
// be reached. A blocked rule goes on refusing them. Any other rule applies
// its whole rate, on that Envoy alone: the most any one Envoy could ever be
// given, so the daemon being away refuses nothing it would have allowed.
func (r *Rule) Strategy() *typev3.RateLimitStrategy {
	if r.Blocked {
		return blanket(typev3.RateLimitStrategy_DENY_ALL)
	}
	return &typev3.RateLimitStrategy{Strategy: &typev3.RateLimitStrategy_TokenBucket{TokenBucket: tokenBucket(r.Rate(), r.Burst)}}
}

// First is what Envoy does with a count it has just opened, in the moment
// before the daemon's answer to its first report arrives, and with one it
// has opened while the daemon is away: a blocked rule refuses, any other
// lets the request through. It cannot be a rate. Envoy 1.40 has no token
// bucket for a count until the daemon assigns one, and uses it regardless
// (filter.cc, shouldAllowRequest): a rate here would stop Envoy.
func (r *Rule) First() *typev3.RateLimitStrategy {
	if r.Blocked {
		return blanket(typev3.RateLimitStrategy_DENY_ALL)
	}
	return blanket(typev3.RateLimitStrategy_ALLOW_ALL)
}

func blanket(rule typev3.RateLimitStrategy_BlanketRule) *typev3.RateLimitStrategy {
	return &typev3.RateLimitStrategy{Strategy: &typev3.RateLimitStrategy_BlanketRule_{BlanketRule: rule}}
}
