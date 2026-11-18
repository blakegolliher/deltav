package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"

	"deltav/internal/ratelimit"
	"deltav/internal/xds"
)

// filterNames returns the HTTP filters of a served target's listener, in
// order, and the request headers its route removes.
func filterNames(t *testing.T, s *Server, target string) (filters, removed []string) {
	t.Helper()
	for _, spec := range s.XDS().Targets() {
		if spec.Name != target {
			continue
		}
		l, _, _, err := spec.Resources()
		if err != nil {
			t.Fatal(err)
		}
		var hcm hcmv3.HttpConnectionManager
		if err := l.GetFilterChains()[0].GetFilters()[0].GetTypedConfig().UnmarshalTo(&hcm); err != nil {
			t.Fatal(err)
		}
		for _, f := range hcm.GetHttpFilters() {
			filters = append(filters, f.GetName())
		}
		return filters, hcm.GetRouteConfig().GetVirtualHosts()[0].GetRoutes()[0].GetRequestHeadersToRemove()
	}
	t.Fatalf("target %q is not served", target)
	return nil, nil
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
	if f, removed := filterNames(t, s, "fixed"); len(f) != 1 || f[0] != "envoy.filters.http.router" || len(removed) != 0 {
		t.Errorf("without rules the listener has only the router: %v, %v", f, removed)
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
	f, removed := filterNames(t, s, "fixed")
	if strings.Join(f, " ") != "envoy.filters.http.lua deltav.rate_limit.tenant-a deltav.rate_limit.listings envoy.filters.http.router" ||
		strings.Join(removed, " ") != "x-deltav-rl-1 x-deltav-rl-2" {
		t.Errorf("filters %v, headers removed %v", f, removed)
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
