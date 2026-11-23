package metrics

import (
	"testing"
	"time"
)

func hasTotals(a *Aggregator, k EndpointKey) (EndpointTotals, bool) {
	for _, e := range a.EndpointTotals() {
		if e.Key == k {
			return e, true
		}
	}
	return EndpointTotals{}, false
}

func listed(a *Aggregator, k EndpointKey) bool {
	for _, e := range a.Endpoints(Window1h) {
		if e.Key == k {
			return true
		}
	}
	return false
}

// T3a: an endpoint known only from load reports (no access-log sample ever)
// whose stream has closed is gone after the retention period, like any
// idle series.
func TestReviewT3_LoadOnlyEndpointExpires(t *testing.T) {
	a, c := newAggregator()
	k := EndpointKey{"s3", "10.0.0.9:80"}
	a.Load("host-a", 1, []LoadReport{{Key: k, Issued: 5, Success: 5}})
	a.LoadGone("host-a", 1)
	c.advance(2 * time.Hour)
	a.Prune(time.Hour, 2*time.Minute)
	if e, ok := hasTotals(a, k); ok {
		t.Errorf("load-only endpoint idle 2h survived Prune(1h): EndpointTotals has %+v", e)
	}
	if listed(a, k) {
		t.Errorf("load-only endpoint idle 2h survived Prune(1h): still in Endpoints()")
	}
}

// T3b, the converse: an endpoint whose access-log series has gone idle but
// whose load reports still count requests keeps its load totals.
func TestReviewT3_ActiveLoadCountersOutliveIdleAccessLogSeries(t *testing.T) {
	a, c := newAggregator()
	k := EndpointKey{"s3", "10.0.0.1:80"}
	a.Record("host-a", []Sample{ok("s3", "10.0.0.1:80", time.Millisecond)})
	var issued uint64
	for i := 0; i < 14; i++ { // 70 minutes of load reports, no access log
		c.advance(5 * time.Minute)
		a.Load("host-a", 1, []LoadReport{{Key: k, Issued: 10, Success: 10}})
		issued += 10
	}
	a.Prune(time.Hour, 2*time.Minute)
	e, ok := hasTotals(a, k)
	if !ok || e.Load.Issued != issued {
		t.Errorf("load totals of an endpoint reported 5 minutes ago: present=%v issued=%d, want %d (Prometheus sees a counter reset)", ok, e.Load.Issued, issued)
	}
}
