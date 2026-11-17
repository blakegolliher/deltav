package metrics

import (
	"math"
	"sort"
	"time"
)

// Latency histograms use fixed buckets so slots can be added together. The
// upper bounds are 100 µs × 2^(i/3): three buckets per doubling, from 100 µs
// to about 8.8 minutes, then one bucket for everything longer.
const (
	histBase      = 100 * time.Microsecond
	histPerOctave = 3
	histBuckets   = 69
)

// histBounds[i] is the inclusive upper bound of bucket i. The last bucket
// has no upper bound.
var histBounds = func() [histBuckets - 1]time.Duration {
	var b [histBuckets - 1]time.Duration
	for i := range b {
		if i%histPerOctave == 0 {
			b[i] = histBase << (i / histPerOctave) // exact powers of two
		} else {
			b[i] = time.Duration(math.Round(float64(histBase) * math.Pow(2, float64(i)/histPerOctave)))
		}
	}
	return b
}()

func bucketOf(d time.Duration) int {
	return sort.Search(len(histBounds), func(i int) bool { return d <= histBounds[i] })
}

// quantileMS estimates the q-quantile in milliseconds, interpolating within
// the bucket that holds it. It returns 0 for an empty histogram.
func quantileMS[T uint32 | uint64](hist []T, q float64) float64 {
	var n uint64
	for _, c := range hist {
		n += uint64(c)
	}
	if n == 0 {
		return 0
	}
	rank := uint64(math.Ceil(q * float64(n)))
	if rank < 1 {
		rank = 1
	}
	var seen uint64
	for i, c := range hist {
		if c == 0 {
			continue
		}
		if seen+uint64(c) < rank {
			seen += uint64(c)
			continue
		}
		var lo time.Duration
		if i > 0 {
			lo = histBounds[i-1]
		}
		if i == len(histBounds) {
			return ms(lo) // the open-ended bucket: all that is known is "at least"
		}
		frac := float64(rank-seen) / float64(c)
		return ms(lo) + frac*(ms(histBounds[i])-ms(lo))
	}
	return 0
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// PrometheusBuckets converts a cumulative histogram to Prometheus form: a
// map from upper bound in seconds to the count at or below it. It keeps
// every third bound (100 µs, 200 µs, 400 µs, ...), which keeps the series
// count down and loses nothing Prometheus could use.
func PrometheusBuckets(hist *[histBuckets]uint64) (buckets map[float64]uint64, count uint64) {
	buckets = make(map[float64]uint64, len(histBounds)/histPerOctave+1)
	for i, c := range hist {
		count += c
		if i < len(histBounds) && i%histPerOctave == 0 {
			buckets[histBounds[i].Seconds()] = count
		}
	}
	return buckets, count
}
