package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	accesslogdatav3 "github.com/envoyproxy/go-control-plane/envoy/data/accesslog/v3"
	luav3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/lua/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	accesslogv3 "github.com/envoyproxy/go-control-plane/envoy/service/accesslog/v3"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"deltav/internal/ratelimit"
	"deltav/internal/xds"
)

// served returns what Envoys are given for a target: the HTTP filters of
// its listener, in order; and from its route, the rate-limit script and the
// request headers removed.
func served(t *testing.T, s *Server, target string) (filters []string, script string, removed []string) {
	t.Helper()
	for _, spec := range s.XDS().Targets() {
		if spec.Name != target {
			continue
		}
		res, err := spec.Resources()
		if err != nil {
			t.Fatal(err)
		}
		var hcm hcmv3.HttpConnectionManager
		if err := res.Listener.GetFilterChains()[0].GetFilters()[0].GetTypedConfig().UnmarshalTo(&hcm); err != nil {
			t.Fatal(err)
		}
		for _, f := range hcm.GetHttpFilters() {
			filters = append(filters, f.GetName())
		}
		route := res.Route.GetVirtualHosts()[0].GetRoutes()[0]
		var lua luav3.LuaPerRoute
		if err := route.GetTypedPerFilterConfig()[xds.LuaFilter].UnmarshalTo(&lua); err != nil {
			t.Fatal(err)
		}
		return filters, lua.GetSourceCode().GetInlineString(), route.GetRequestHeadersToRemove()
	}
	t.Fatalf("target %q is not served", target)
	return nil, "", nil
}

func TestRulesAPI(t *testing.T) {
	dir := t.TempDir()
	s, stop := run(t, dir)
	if _, _, err := s.SetStaticTargets(t.Context(), []xds.TargetSpec{{Name: "fixed", ListenPort: 9002, Endpoints: []xds.Endpoint{{IP: "192.0.2.1", Port: 80}}}}); err != nil {
		t.Fatal(err)
	}
	put := func(name string, body any, want int) string {
		t.Helper()
		code, out := call(t, s, "PUT", "/api/v1/rules/"+name, body)
		if code != want {
			t.Errorf("PUT rule %s: %d %s, want %d", name, code, out, want)
		}
		return out
	}
	list := func(s *Server) rulesResponse { t.Helper(); return getJSON[rulesResponse](t, s, "/api/v1/rules") }

	if got := list(s); len(got.Rules) != 0 || got.ReportIntervalSeconds != 5 {
		t.Fatalf("no rules yet: %+v", got)
	}
	// The listener's filters are the same with rules and without; the
	// rules are in the route.
	const fixed = "envoy.filters.http.lua deltav.rate_limit.slot1 deltav.rate_limit.slot2 deltav.rate_limit.slot3 deltav.rate_limit.slot4 envoy.filters.http.router"
	if f, script, removed := served(t, s, "fixed"); strings.Join(f, " ") != fixed || script != "" || len(removed) != 0 {
		t.Errorf("without rules: filters %v, script of %d bytes, removes %v", f, len(script), removed)
	}
	v0 := s.XDS().Version()

	// A rule is normalized, said back in words, and served to Envoys at once.
	out := put("tenant-a", map[string]any{"operation": []string{"putobject", "UploadPart"}, "access_key": []string{"AKIAEXAMPLE1"},
		"limit_per_second": 2000, "per": []string{"bucket", "access_key"}, "note": "agreed with the tenant"}, http.StatusOK)
	var r ruleJSON
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatal(err)
	}
	if r.Name != "tenant-a" || strings.Join(r.Operation, ",") != "PutObject,UploadPart" || r.Burst != 2000 || r.Respond != 503 || r.Limit != "2000 per second" ||
		r.Sentence != "At most 2000 PutObject, UploadPart calls per second from access key AKIAEXAMPLE1, for each access key and each bucket, across the fleet. Over the limit, the client gets 503 SlowDown." {
		t.Errorf("the rule as stored: %+v", r)
	}
	put("listings", map[string]any{"operation": []string{"List*"}, "limit_per_minute": 600, "per": []string{"total"}}, http.StatusOK)
	if s.XDS().Version() == v0 {
		t.Error("adding rules did not change what Envoys are served")
	}
	f, script, removed := served(t, s, "fixed")
	if strings.Join(f, " ") != fixed || !strings.Contains(script, `name="tenant-a"`) || !strings.Contains(script, `name="listings"`) ||
		strings.Join(removed, " ") != strings.Join(ratelimit.SlotHeaders(), " ") {
		t.Errorf("with rules: filters %v, script of %d bytes, removes %v", f, len(script), removed)
	}
	if got := list(s); got.Slots != ratelimit.Slots || !got.Rules[0].Holds || !got.Rules[1].Holds {
		t.Errorf("rules as listed: %+v", got)
	}

	// Replacing a rule keeps its place in the order.
	put("tenant-a", map[string]any{"name": "tenant-a", "blocked": true, "respond": 429}, http.StatusOK)
	if got := list(s); len(got.Rules) != 2 || got.Rules[0].Name != "tenant-a" || !got.Rules[0].Blocked || got.Rules[0].Limit != "blocked" ||
		got.Rules[1].Name != "listings" || got.Rules[1].Status.Top == nil {
		t.Errorf("after replacing the first rule: %+v", got.Rules)
	}

	// What cannot be a rule is refused and changes nothing.
	v1 := s.XDS().Version()
	for what, body := range map[string]any{
		"no limit":          map[string]any{"per": []string{"total"}},
		"no per":            map[string]any{"limit_per_second": 5},
		"unknown operation": map[string]any{"operation": []string{"WRITE"}, "limit_per_second": 5, "per": []string{"total"}},
		"unknown field":     map[string]any{"limit_per_second": 5, "per": []string{"total"}, "rate": 5},
		"another name":      map[string]any{"name": "other", "limit_per_second": 5, "per": []string{"total"}},
	} {
		if out := put("bad", body, http.StatusBadRequest); !strings.Contains(out, "error") {
			t.Errorf("%s: %s", what, out)
		}
	}
	put("bad name", map[string]any{"limit_per_second": 5, "per": []string{"total"}}, http.StatusBadRequest)
	if got := list(s); len(got.Rules) != 2 || s.XDS().Version() != v1 {
		t.Errorf("refused rules changed something: %d rules", len(got.Rules))
	}

	// Rules are in /metrics by name, and survive a restart.
	_, prom := get(t, "http://"+s.HTTPAddr()+"/metrics", "")
	for _, want := range []string{`deltav_rate_limit_requests_total{result="allowed",rule="listings"} 0`, `deltav_rate_limit_keys{rule="tenant-a"} 0`, "deltav_rate_limit_keys_overflow_total 0"} {
		if !strings.Contains(prom, want+"\n") {
			t.Errorf("/metrics lacks %q", want)
		}
	}
	stop()
	s2, _ := run(t, dir)
	if got := list(s2); len(got.Rules) != 2 || got.Rules[0].Name != "tenant-a" || got.Rules[1].LimitPerMinute != 600 {
		t.Errorf("rules after a restart: %+v", got.Rules)
	}

	// Removing.
	if code, _ := call(t, s2, "DELETE", "/api/v1/rules/nope", nil); code != http.StatusNotFound {
		t.Errorf("removing a rule that is not there: %d", code)
	}
	for _, name := range []string{"tenant-a", "listings"} {
		if code, out := call(t, s2, "DELETE", "/api/v1/rules/"+name, nil); code != http.StatusNoContent {
			t.Errorf("removing %s: %d %s", name, code, out)
		}
	}
	if got := list(s2); len(got.Rules) != 0 {
		t.Errorf("rules left: %+v", got.Rules)
	}

	ops := getJSON[map[string][]string](t, s2, "/api/v1/s3-operations")["operations"]
	if len(ops) != len(ratelimit.Operations()) || ops[0] != "AbortMultipartUpload" {
		t.Errorf("operations: %d, first %q", len(ops), ops[0])
	}
}

// refused builds the access log entry of a request a rule refused inside
// Envoy: no upstream, the script's notes in the metadata, and for one over
// a limit the quota filter's mark in the response's details.
func refused(status uint32, details string, notes map[string]any) *accesslogdatav3.HTTPAccessLogEntry {
	e := entry("", status, time.Millisecond, 0, 126)
	e.CommonProperties.UpstreamCluster = ""
	e.CommonProperties.StartTime = timestamppb.New(time.Now())
	e.Response.ResponseCodeDetails = details
	md, err := structpb.NewStruct(notes)
	if err != nil {
		panic(err)
	}
	e.CommonProperties.Metadata = &corev3.Metadata{FilterMetadata: map[string]*structpb.Struct{ratelimit.MetadataNamespace: md}}
	return e
}

// A request a rule refuses never reaches the target. It is counted apart
// from the target's requests and its 503s, attributed to the rule, and kept
// on record one by one.
func TestRefusalsAreCountedApartAndRecorded(t *testing.T) {
	s := telemetryServer(t)
	register(t, s, "host-a")
	if code, body := call(t, s, "PUT", "/api/v1/rules/no-deletes", map[string]any{"operation": []string{"DeleteObject"}, "blocked": true}); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	if code, body := call(t, s, "PUT", "/api/v1/rules/gets", map[string]any{"operation": []string{"GetObject"}, "limit_per_second": 20, "per": []string{"access_key"}}); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	a, err := accesslogv3.NewAccessLogServiceClient(dial(t, s)).StreamAccessLogs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	matched := map[string]any{"rules": "gets", "operation": "GetObject", "access_key": "AKIAEXAMPLE1", "bucket": "data"}
	var entries []*accesslogdatav3.HTTPAccessLogEntry
	for i := 0; i < 5; i++ {
		entries = append(entries, entry("192.0.2.11", 200, 10*time.Millisecond, 0, 1000))
	}
	// One the target itself answered 503, which is the target's.
	entries = append(entries, entry("192.0.2.12", 503, 10*time.Millisecond, 0, 100))
	// One allowed under the rule: counted by it, and an ordinary request.
	allowed := entry("192.0.2.11", 200, 10*time.Millisecond, 0, 1000)
	allowed.CommonProperties.Metadata = refused(0, "", matched).CommonProperties.Metadata
	entries = append(entries, allowed)
	for i := 0; i < 30; i++ {
		entries = append(entries, refused(503, ratelimit.RefusedDetails, matched))
	}
	for i := 0; i < 7; i++ {
		entries = append(entries, refused(503, "lua_response", map[string]any{"refused_by": "no-deletes", "operation": "DeleteObject", "access_key": "-", "bucket": "archive"}))
	}
	if err := a.Send(logs("host-a", entries...)); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() string {
		o := getJSON[overviewResponse](t, s, "/api/v1/overview")
		if st := o.Targets[0].Stats; st.Requests != 7 || st.Refused != 37 || st.Status.S503 != 1 || st.Status.S5xx != 1 || st.Status.S2xx != 6 || st.BytesOut != 6100 {
			return fmt.Sprintf("the target's counts: %+v", st)
		}
		return ""
	})
	// The client host's own numbers leave them out the same way.
	f := getJSON[fleetResponse](t, s, "/api/v1/fleet")
	if len(f.Members) != 1 || f.Members[0].Stats.Requests != 7 || f.Members[0].Stats.Refused != 37 {
		t.Errorf("the member's counts: %+v", f.Members[0].Stats)
	}
	// The blocked rule's refusals are known only from the log; those over a
	// limit are counted by the Envoys' reports, of which this test has none.
	// (A rule's status is worked out at most once a second.)
	eventually(t, func() string {
		rules := getJSON[rulesResponse](t, s, "/api/v1/rules").Rules
		if rules[0].Name != "no-deletes" || rules[0].Status.Totals.Denied != 7 || rules[1].Status.Totals.Denied != 0 {
			return fmt.Sprintf("rules after the refusals: %+v", rules)
		}
		return ""
	})

	rec := getJSON[refusalsResponse](t, s, "/api/v1/refusals").Refusals
	if len(rec) != 37 {
		t.Fatalf("%d refusals on record, want 37", len(rec))
	}
	if r := rec[0]; !r.Blocked || len(r.Rules) != 1 || r.Rules[0] != "no-deletes" || r.Client != "host-a" || r.Target != "s3" ||
		r.Operation != "DeleteObject" || r.AccessKey != "-" || r.Bucket != "archive" || r.Status != 503 || time.Since(r.At) > time.Minute {
		t.Errorf("the latest refusal: %+v", r)
	}
	if r := rec[36]; r.Blocked || len(r.Rules) != 1 || r.Rules[0] != "gets" || r.Operation != "GetObject" || r.AccessKey != "AKIAEXAMPLE1" || r.Bucket != "data" {
		t.Errorf("the earliest refusal: %+v", r)
	}
	if got := getJSON[refusalsResponse](t, s, "/api/v1/refusals?limit=3").Refusals; len(got) != 3 {
		t.Errorf("limit=3 returned %d", len(got))
	}
	for _, bad := range []string{"0", "-1", "many"} {
		if code, _ := call(t, s, "GET", "/api/v1/refusals?limit="+bad, nil); code != http.StatusBadRequest {
			t.Errorf("limit=%s: %d", bad, code)
		}
	}
	// And in /metrics, apart from the target's requests.
	_, prom := get(t, "http://"+s.HTTPAddr()+"/metrics", "")
	for _, want := range []string{`deltav_rate_limit_requests_total{result="denied",rule="no-deletes"} 7`, `deltav_target_refused_total{target="s3"} 37`} {
		if !strings.Contains(prom, want+"\n") {
			t.Errorf("/metrics lacks %q", want)
		}
	}
}
