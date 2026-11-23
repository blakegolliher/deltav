package metrics

import "testing"

// T2: a node reconnects its load-report stream. The new stream (2) reports
// 7 in flight; then a late report arrives on the superseded stream (1),
// then that stream closes. The node's in-flight count is the new stream's
// until the new stream says otherwise.
func TestReviewT2_SupersededStreamCannotTouchSuccessorGauges(t *testing.T) {
	a, _ := newAggregator()
	k := EndpointKey{"s3", "10.0.0.1:80"}
	check := func(when string) {
		t.Helper()
		m := a.Member("host-a", Window1m).InFlight
		e := a.InFlight()[k]
		tg := a.Targets(Window1m)["s3"].InFlight
		if m != 7 || e != 7 || tg != 7 {
			t.Errorf("%s: member in flight = %d, endpoint = %d, target = %d; want 7 from the current stream", when, m, e, tg)
		}
	}
	a.Load("host-a", 2, []LoadReport{{Key: k, InProgress: 7}})
	check("new stream reported")
	a.Load("host-a", 1, []LoadReport{{Key: k, InProgress: 2, Issued: 3, Success: 3}})
	check("old stream reported late")
	// Its counts are real requests, so they are added all the same.
	if got := a.lrs[k]; got.Issued != 3 || got.Success != 3 {
		t.Errorf("old stream's late report: load totals %+v, want 3 issued and 3 succeeded", got)
	}
	a.LoadGone("host-a", 1)
	check("old stream closed")
}
