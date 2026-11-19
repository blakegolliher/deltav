package xds

import (
	xdscorev3 "github.com/cncf/xds/go/xds/core/v3"
	xdsmatcherv3 "github.com/cncf/xds/go/xds/type/matcher/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	luav3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/lua/v3"
	rlqfv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/rate_limit_quota/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	envoymatcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"deltav/internal/ratelimit"
)

// Rate limiting is in two places, and which is which matters.
//
// The listener has a Lua filter and then ratelimit.Slots rate limit quota
// filters, ahead of the router. They are the same for every target whatever
// its rules, and they never change.
//
// The route has the rules: the script package ratelimit generates from
// them, as the Lua filter's configuration for that route. The script works
// out what each request is, refuses it if a blocked rule matches, and
// otherwise names, in the request headers of one slot each, the rules that
// count it and what they count it under. Slot j's quota filter counts any
// request that has slot j's headers in the bucket they name. The daemon's
// quota service gives each Envoy its share of each bucket.
//
// The division is there because of how Envoy takes a change. A changed
// listener applies to new connections, and the old ones live on under the
// old one until they close, which for an S3 client that keeps its
// connections open is as long as it likes. A changed route applies to the
// next request on every connection. Rules live in the route so that adding,
// changing or removing one takes effect at once, everywhere.
//
// A quota filter puts a request in one bucket, and a request that matches
// several rules must be allowed by every one of them: hence several
// filters, one rule each. They run in turn; the first to refuse answers.
//
// A target with no rules turns all of these off in its route, and a request
// passes exactly as it would with no such filters.

// LuaFilter is the name of the listener's Lua filter.
const LuaFilter = "envoy.filters.http.lua"

// SlotFilter is the name of slot j's rate limit quota filter, j from 1.
func SlotFilter(j int) string { return "deltav.rate_limit.slot" + string(rune('0'+j)) }

// What an Envoy does when it cannot reach the daemon:
//
//   - A blocked rule goes on refusing: that is the script's doing alone.
//   - A count that has a share keeps it. A share is good for ten minutes
//     (ratelimit.AssignmentTTL) and the daemon renews it well before then,
//     so for ten minutes without the daemon the fleet holds every rule
//     exactly as it stood.
//   - After that, and for counts first opened while the daemon is away, the
//     rule is not enforced. A limiter that takes storage away when its own
//     control plane is down is worse than none.
//
// A count that has just been opened lets requests through until the
// daemon's answer to its first report arrives, a matter of milliseconds.
// That cannot be a rate: Envoy 1.40 has no token bucket for a count until
// the daemon assigns one (filter.cc).

// rateLimitFilters returns the HTTP filters that go ahead of the router in
// every listener.
func rateLimitFilters() ([]*hcmv3.HttpFilter, error) {
	// No script of its own: a route brings one, or turns the filter off.
	// Envoy re-reads the route after a script changes a header unless told
	// not to; nothing a script sets is something a route here depends on.
	lua, err := toAny(&luav3.Lua{ClearRouteCache: wrapperspb.Bool(false)})
	if err != nil {
		return nil, err
	}
	filters := []*hcmv3.HttpFilter{{
		Name:       LuaFilter,
		ConfigType: &hcmv3.HttpFilter_TypedConfig{TypedConfig: lua},
	}}
	for j := 1; j <= ratelimit.Slots; j++ {
		cfg, err := quotaFilter(j)
		if err != nil {
			return nil, err
		}
		filters = append(filters, &hcmv3.HttpFilter{
			Name:       SlotFilter(j),
			ConfigType: &hcmv3.HttpFilter_TypedConfig{TypedConfig: cfg},
		})
	}
	return filters, nil
}

// routeLimits returns what a target's route says about rate limiting: each
// filter's configuration for the route, and the request headers that must
// not reach VAST.
func (t TargetSpec) routeLimits() (map[string]*anypb.Any, []string, error) {
	per := map[string]*anypb.Any{}
	if len(t.RateLimits) == 0 {
		off, err := toAny(&luav3.LuaPerRoute{Override: &luav3.LuaPerRoute_Disabled{Disabled: true}})
		if err != nil {
			return nil, nil, err
		}
		per[LuaFilter] = off
		skip, err := toAny(&routev3.FilterConfig{Disabled: true})
		if err != nil {
			return nil, nil, err
		}
		for j := 1; j <= ratelimit.Slots; j++ {
			per[SlotFilter(j)] = skip
		}
		return per, nil, nil
	}
	script, err := toAny(&luav3.LuaPerRoute{Override: &luav3.LuaPerRoute_SourceCode{SourceCode: &corev3.DataSource{
		Specifier: &corev3.DataSource_InlineString{InlineString: ratelimit.Script(t.RateLimits, t.S3Hosts)},
	}}})
	if err != nil {
		return nil, nil, err
	}
	per[LuaFilter] = script
	return per, ratelimit.SlotHeaders(), nil
}

func headerInput(name string) (*xdscorev3.TypedExtensionConfig, error) {
	in, err := toAny(&envoymatcherv3.HttpRequestHeaderMatchInput{HeaderName: name})
	if err != nil {
		return nil, err
	}
	return &xdscorev3.TypedExtensionConfig{Name: "request_header", TypedConfig: in}, nil
}

// refusal is the response to a request over a rule's limit: S3's own error
// document, so that every S3 client treats it as S3 throttling.
func refusal(status int) *rlqfv3.RateLimitQuotaBucketSettings_DenyResponseSettings {
	code := typev3.StatusCode_ServiceUnavailable
	if status == 429 {
		code = typev3.StatusCode_TooManyRequests
	}
	header := func(k, v string) *corev3.HeaderValueOption {
		return &corev3.HeaderValueOption{
			Header:       &corev3.HeaderValue{Key: k, Value: v},
			AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
		}
	}
	return &rlqfv3.RateLimitQuotaBucketSettings_DenyResponseSettings{
		HttpStatus: &typev3.HttpStatus{Code: code},
		HttpBody:   wrapperspb.Bytes([]byte(ratelimit.RefusalBody(status))),
		ResponseHeadersToAdd: []*corev3.HeaderValueOption{
			header("content-type", "application/xml"),
			// How long to wait is not known here: the share is refilled
			// continuously. One second is the shortest wait that can be said.
			header("retry-after", "1"),
		},
	}
}

// quotaFilter is slot j's filter. It knows nothing of any rule: the bucket
// a request is counted in, and the status a refusal carries, are whatever
// the script wrote in the slot's headers.
func quotaFilter(j int) (*anypb.Any, error) {
	value := func(header string) (*rlqfv3.RateLimitQuotaBucketSettings_BucketIdBuilder_ValueBuilder, error) {
		in, err := toAny(&envoymatcherv3.HttpRequestHeaderMatchInput{HeaderName: header})
		if err != nil {
			return nil, err
		}
		return &rlqfv3.RateLimitQuotaBucketSettings_BucketIdBuilder_ValueBuilder{
			ValueSpecifier: &rlqfv3.RateLimitQuotaBucketSettings_BucketIdBuilder_ValueBuilder_CustomValue{
				CustomValue: &corev3.TypedExtensionConfig{Name: "request_header", TypedConfig: in},
			},
		}, nil
	}
	rule, err := value(ratelimit.SlotRuleHeader(j))
	if err != nil {
		return nil, err
	}
	key, err := value(ratelimit.SlotKeyHeader(j))
	if err != nil {
		return nil, err
	}
	settings := func(status int) (*xdsmatcherv3.Matcher_OnMatch, error) {
		s, err := toAny(&rlqfv3.RateLimitQuotaBucketSettings{
			BucketIdBuilder: &rlqfv3.RateLimitQuotaBucketSettings_BucketIdBuilder{
				BucketIdBuilder: map[string]*rlqfv3.RateLimitQuotaBucketSettings_BucketIdBuilder_ValueBuilder{
					ratelimit.IDRule: rule,
					ratelimit.IDKey:  key,
				},
			},
			// Required, and ignored: Envoy 1.40 reports every 5 seconds.
			ReportingInterval:    durationpb.New(ratelimit.ReportInterval),
			DenyResponseSettings: refusal(status),
			NoAssignmentBehavior: &rlqfv3.RateLimitQuotaBucketSettings_NoAssignmentBehavior{
				NoAssignmentBehavior: &rlqfv3.RateLimitQuotaBucketSettings_NoAssignmentBehavior_FallbackRateLimit{
					FallbackRateLimit: ratelimit.AllowAll(),
				},
			},
			// No expired_assignment_behavior: a share that runs out leaves
			// the count as it was before it had one.
		})
		if err != nil {
			return nil, err
		}
		return &xdsmatcherv3.Matcher_OnMatch{OnMatch: &xdsmatcherv3.Matcher_OnMatch_Action{
			Action: &xdscorev3.TypedExtensionConfig{Name: "bucket", TypedConfig: s},
		}}, nil
	}
	single := func(header string, m *xdsmatcherv3.StringMatcher) (*xdsmatcherv3.Matcher_MatcherList_Predicate, error) {
		in, err := headerInput(header)
		if err != nil {
			return nil, err
		}
		return &xdsmatcherv3.Matcher_MatcherList_Predicate{MatchType: &xdsmatcherv3.Matcher_MatcherList_Predicate_SinglePredicate_{
			SinglePredicate: &xdsmatcherv3.Matcher_MatcherList_Predicate_SinglePredicate{
				Input:   in,
				Matcher: &xdsmatcherv3.Matcher_MatcherList_Predicate_SinglePredicate_ValueMatch{ValueMatch: m},
			},
		}}, nil
	}
	// "The slot is in use": the script only ever sets its key to something.
	used, err := single(ratelimit.SlotKeyHeader(j), &xdsmatcherv3.StringMatcher{MatchPattern: &xdsmatcherv3.StringMatcher_SafeRegex{
		SafeRegex: &xdsmatcherv3.RegexMatcher{
			EngineType: &xdsmatcherv3.RegexMatcher_GoogleRe2{GoogleRe2: &xdsmatcherv3.RegexMatcher_GoogleRE2{}},
			Regex:      ".+",
		},
	}})
	if err != nil {
		return nil, err
	}
	is429, err := single(ratelimit.SlotStatusHeader(j), &xdsmatcherv3.StringMatcher{MatchPattern: &xdsmatcherv3.StringMatcher_Exact{Exact: "429"}})
	if err != nil {
		return nil, err
	}
	on429, err := settings(429)
	if err != nil {
		return nil, err
	}
	on503, err := settings(503)
	if err != nil {
		return nil, err
	}
	return toAny(&rlqfv3.RateLimitQuotaFilterConfig{
		RlqsServer: controlGRPC(),
		Domain:     ratelimit.Domain,
		BucketMatchers: &xdsmatcherv3.Matcher{MatcherType: &xdsmatcherv3.Matcher_MatcherList_{MatcherList: &xdsmatcherv3.Matcher_MatcherList{
			Matchers: []*xdsmatcherv3.Matcher_MatcherList_FieldMatcher{
				{
					Predicate: &xdsmatcherv3.Matcher_MatcherList_Predicate{MatchType: &xdsmatcherv3.Matcher_MatcherList_Predicate_AndMatcher{
						AndMatcher: &xdsmatcherv3.Matcher_MatcherList_Predicate_PredicateList{
							Predicate: []*xdsmatcherv3.Matcher_MatcherList_Predicate{used, is429},
						},
					}},
					OnMatch: on429,
				},
				{Predicate: used, OnMatch: on503},
			},
		}}},
	})
}
