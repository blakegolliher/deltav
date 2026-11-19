package xds

import (
	"strings"
	"testing"

	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	luav3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/lua/v3"
	rlqfv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/rate_limit_quota/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"google.golang.org/protobuf/proto"

	"deltav/internal/ratelimit"
)

func rules(t *testing.T, rules ...ratelimit.Rule) []ratelimit.Rule {
	t.Helper()
	if err := ratelimit.Validate(rules); err != nil {
		t.Fatal(err)
	}
	return rules
}

// The property everything else here rests on: rules are in the route, and
// the listener is the same whatever they are. Envoy applies a new listener
// to new connections only; a rule has to reach the ones already open.
func TestRulesNeverChangeTheListener(t *testing.T) {
	spec := testTarget()
	none, err := spec.Resources()
	if err != nil {
		t.Fatal(err)
	}
	spec.RateLimits = rules(t,
		ratelimit.Rule{Name: "gets", Operation: []string{"GetObject"}, LimitPerSecond: 20, Per: []string{ratelimit.PerAccessKey}},
		ratelimit.Rule{Name: "no-deletes", Operation: []string{"DeleteObject"}, Blocked: true})
	some, err := spec.Resources()
	if err != nil {
		t.Fatal(err)
	}
	spec.RateLimits = rules(t, ratelimit.Rule{Name: "gets", Operation: []string{"GetObject"}, LimitPerSecond: 5, Per: []string{ratelimit.PerTotal}, Respond: 429})
	other, err := spec.Resources()
	if err != nil {
		t.Fatal(err)
	}
	for what, r := range map[string]Rendered{"with rules": some, "with other rules": other} {
		if !proto.Equal(none.Listener, r.Listener) {
			t.Errorf("the listener %s differs from the one with none", what)
		}
		if !proto.Equal(none.Cluster, r.Cluster) || !proto.Equal(none.Endpoints, r.Endpoints) {
			t.Errorf("the cluster or endpoints %s differ from those with none", what)
		}
		if proto.Equal(none.Route, r.Route) {
			t.Errorf("the route %s is the same as the one with none", what)
		}
	}
	if proto.Equal(some.Route, other.Route) {
		t.Error("two different sets of rules made the same route")
	}
}

func perRoute(t *testing.T, r Rendered) (*routev3.Route, map[string]bool, string) {
	t.Helper()
	route := r.Route.GetVirtualHosts()[0].GetRoutes()[0]
	off := map[string]bool{}
	script := ""
	for name, cfg := range route.GetTypedPerFilterConfig() {
		switch name {
		case LuaFilter:
			var lua luav3.LuaPerRoute
			if err := cfg.UnmarshalTo(&lua); err != nil {
				t.Fatal(err)
			}
			off[name] = lua.GetDisabled()
			script = lua.GetSourceCode().GetInlineString()
		default:
			var fc routev3.FilterConfig
			if err := cfg.UnmarshalTo(&fc); err != nil {
				t.Fatal(err)
			}
			off[name] = fc.GetDisabled()
		}
	}
	return route, off, script
}

func TestRouteWithAndWithoutRules(t *testing.T) {
	spec := testTarget()
	r, err := spec.Resources()
	if err != nil {
		t.Fatal(err)
	}
	// No rules: every rate-limit filter is off for the route, and no
	// header is taken out of a request. The request passes as it came.
	route, off, script := perRoute(t, r)
	if len(off) != 1+ratelimit.Slots || !off[LuaFilter] || script != "" || len(route.GetRequestHeadersToRemove()) != 0 {
		t.Errorf("without rules: off %v, script %q, removes %v", off, script, route.GetRequestHeadersToRemove())
	}
	for j := 1; j <= ratelimit.Slots; j++ {
		if !off[SlotFilter(j)] {
			t.Errorf("without rules, quota filter %d is on", j)
		}
	}

	spec.RateLimits = rules(t,
		ratelimit.Rule{Name: "gets", Operation: []string{"GetObject"}, LimitPerSecond: 20, Per: []string{ratelimit.PerAccessKey}},
		ratelimit.Rule{Name: "no-deletes", Operation: []string{"DeleteObject"}, Blocked: true})
	if r, err = spec.Resources(); err != nil {
		t.Fatal(err)
	}
	// With rules: the route carries the script, nothing is turned off, and
	// what the script writes for the quota filters stops at the router.
	route, off, script = perRoute(t, r)
	if len(off) != 1 || off[LuaFilter] || script != ratelimit.Script(spec.RateLimits, spec.S3Hosts) || !strings.Contains(script, `name="gets"`) {
		t.Errorf("with rules: off %v, script of %d bytes", off, len(script))
	}
	if got := strings.Join(route.GetRequestHeadersToRemove(), " "); got != strings.Join(ratelimit.SlotHeaders(), " ") || len(ratelimit.SlotHeaders()) != 3*ratelimit.Slots {
		t.Errorf("with rules the route removes %q", got)
	}
}

// The listener's part: a script filter with no script of its own, and one
// quota filter per slot that takes everything about a request's count from
// the slot's headers.
func TestListenerRateLimitFilters(t *testing.T) {
	r, err := testTarget().Resources()
	if err != nil {
		t.Fatal(err)
	}
	hcm := &hcmv3.HttpConnectionManager{}
	if err := r.Listener.GetFilterChains()[0].GetFilters()[0].GetTypedConfig().UnmarshalTo(hcm); err != nil {
		t.Fatal(err)
	}
	filters := hcm.GetHttpFilters()
	var lua luav3.Lua
	if err := filters[0].GetTypedConfig().UnmarshalTo(&lua); err != nil {
		t.Fatal(err)
	}
	if lua.GetDefaultSourceCode() != nil || lua.GetInlineCode() != "" || len(lua.GetSourceCodes()) != 0 {
		t.Error("the listener's script filter must have no script: scripts come with routes")
	}
	for j := 1; j <= ratelimit.Slots; j++ {
		f := filters[j]
		if f.GetName() != SlotFilter(j) {
			t.Fatalf("filter %d is %q", j, f.GetName())
		}
		var cfg rlqfv3.RateLimitQuotaFilterConfig
		if err := f.GetTypedConfig().UnmarshalTo(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.GetDomain() != ratelimit.Domain || cfg.GetRlqsServer().GetEnvoyGrpc().GetClusterName() != ControlCluster {
			t.Errorf("slot %d: domain %q, server %v", j, cfg.GetDomain(), cfg.GetRlqsServer())
		}
		matchers := cfg.GetBucketMatchers().GetMatcherList().GetMatchers()
		if len(matchers) != 2 {
			t.Fatalf("slot %d: %d matchers, want one for a 429 refusal and one for the rest", j, len(matchers))
		}
		for i, status := range []uint32{429, 503} {
			var s rlqfv3.RateLimitQuotaBucketSettings
			if err := matchers[i].GetOnMatch().GetAction().GetTypedConfig().UnmarshalTo(&s); err != nil {
				t.Fatal(err)
			}
			if got := uint32(s.GetDenyResponseSettings().GetHttpStatus().GetCode()); got != status {
				t.Errorf("slot %d, matcher %d refuses with %d, want %d", j, i, got, status)
			}
			if string(s.GetDenyResponseSettings().GetHttpBody().GetValue()) != ratelimit.RefusalBody(int(status)) {
				t.Errorf("slot %d: a %d refusal's body is not the one the script sends", j, status)
			}
			b := s.GetBucketIdBuilder().GetBucketIdBuilder()
			if len(b) != 2 || b[ratelimit.IDRule].GetCustomValue() == nil || b[ratelimit.IDKey].GetCustomValue() == nil {
				t.Errorf("slot %d: the count's rule and name must both come from the request: %v", j, b)
			}
			// Without a share, requests pass; a share that runs out leaves
			// the count as it was before it had one.
			if s.GetNoAssignmentBehavior().GetFallbackRateLimit().GetBlanketRule().String() != "ALLOW_ALL" || s.GetExpiredAssignmentBehavior() != nil {
				t.Errorf("slot %d: without a share: %v / %v", j, s.GetNoAssignmentBehavior(), s.GetExpiredAssignmentBehavior())
			}
		}
	}
}

// The same target renders to the same bytes every time. If it did not, the
// daemon would push an unchanged fleet as if it had changed, and Envoy
// would replace a listener it should have kept.
func TestRenderingIsStable(t *testing.T) {
	spec := testTarget()
	spec.AccessLog = true
	spec.RateLimits = rules(t, ratelimit.Rule{Name: "gets", Operation: []string{"GetObject"}, LimitPerSecond: 20, Per: []string{ratelimit.PerAccessKey}})
	var first [4][]byte
	for i := 0; i < 50; i++ {
		r, err := spec.Resources()
		if err != nil {
			t.Fatal(err)
		}
		for j, m := range []proto.Message{r.Listener, r.Route, r.Cluster, r.Endpoints} {
			b, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			if i == 0 {
				first[j] = b
			} else if string(b) != string(first[j]) {
				t.Fatalf("render %d of resource %d differs from the first", i+1, j)
			}
		}
	}
}
