package xds

import (
	"fmt"

	xdscorev3 "github.com/cncf/xds/go/xds/core/v3"
	xdsmatcherv3 "github.com/cncf/xds/go/xds/type/matcher/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
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

// Rate limiting in a listener is a Lua filter and then one rate limit quota
// filter for each rule, in the order of the rules, ahead of the router.
//
// The Lua filter runs the script package ratelimit generates: it works out
// what each request is and, for every rule the request matches, sets that
// rule's header to what the rule counts the request under. A rule's quota
// filter matches any request that has its header and counts it in the
// bucket {rule: its name, key: the header's value}. The daemon's quota
// service gives each Envoy its share of each bucket.
//
// One filter per rule because a quota filter puts a request in one bucket
// only, and a request that matches several rules must be allowed by every
// one of them. The filters run in turn; the first to refuse answers.

// What an Envoy does when it has no share from the daemon:
//
//   - A count it has just opened lets requests through until the daemon's
//     answer arrives, a matter of milliseconds (Rule.First).
//   - A share is good for 20 seconds and is renewed every 5. If the daemon
//     goes away it runs out, and for expiredFor after that the Envoy
//     applies the rule's whole rate by itself (Rule.Strategy).
//   - After that, and for counts opened while the daemon is away, the rule
//     is not enforced. A limiter that takes storage away when its own
//     control plane is down is worse than none.
//
// Blocked rules refuse in every one of these states.
const expiredFor = 600 // seconds

// rateLimitFilters returns the HTTP filters that enforce the target's rules
// and the request headers the script sets, which must not reach VAST.
func (t TargetSpec) rateLimitFilters() ([]*hcmv3.HttpFilter, []string, error) {
	if len(t.RateLimits) == 0 {
		return nil, nil, nil
	}
	lua, err := anypb.New(&luav3.Lua{DefaultSourceCode: &corev3.DataSource{
		Specifier: &corev3.DataSource_InlineString{InlineString: ratelimit.Script(t.RateLimits)},
	}})
	if err != nil {
		return nil, nil, err
	}
	filters := []*hcmv3.HttpFilter{{
		Name:       "envoy.filters.http.lua",
		ConfigType: &hcmv3.HttpFilter_TypedConfig{TypedConfig: lua},
	}}
	var headers []string
	for i := range t.RateLimits {
		rule := &t.RateLimits[i]
		header := ratelimit.RuleHeader(i + 1)
		headers = append(headers, header)
		cfg, err := quotaFilter(rule, header)
		if err != nil {
			return nil, nil, fmt.Errorf("rate limit rule %q: %w", rule.Name, err)
		}
		filters = append(filters, &hcmv3.HttpFilter{
			Name:       "deltav.rate_limit." + rule.Name,
			ConfigType: &hcmv3.HttpFilter_TypedConfig{TypedConfig: cfg},
		})
	}
	return filters, headers, nil
}

func headerInput(name string) (*xdscorev3.TypedExtensionConfig, error) {
	in, err := anypb.New(&envoymatcherv3.HttpRequestHeaderMatchInput{HeaderName: name})
	if err != nil {
		return nil, err
	}
	return &xdscorev3.TypedExtensionConfig{Name: "request_header", TypedConfig: in}, nil
}

// refusal is the response to a request over a rule's limit: S3's own error
// document, so that every S3 client treats it as S3 throttling.
func refusal(status int) *rlqfv3.RateLimitQuotaBucketSettings_DenyResponseSettings {
	code, name := typev3.StatusCode_ServiceUnavailable, "SlowDown"
	if status == 429 {
		code, name = typev3.StatusCode_TooManyRequests, "TooManyRequests"
	}
	body := `<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<Error><Code>` + name + `</Code><Message>Please reduce your request rate.</Message></Error>`
	header := func(k, v string) *corev3.HeaderValueOption {
		return &corev3.HeaderValueOption{
			Header:       &corev3.HeaderValue{Key: k, Value: v},
			AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
		}
	}
	return &rlqfv3.RateLimitQuotaBucketSettings_DenyResponseSettings{
		HttpStatus: &typev3.HttpStatus{Code: code},
		HttpBody:   wrapperspb.Bytes([]byte(body)),
		ResponseHeadersToAdd: []*corev3.HeaderValueOption{
			header("content-type", "application/xml"),
			// How long to wait is not known here: the share is refilled
			// continuously. One second is the shortest wait that can be said.
			header("retry-after", "1"),
		},
	}
}

func quotaFilter(rule *ratelimit.Rule, header string) (*anypb.Any, error) {
	match, err := headerInput(header)
	if err != nil {
		return nil, err
	}
	keyInput, err := anypb.New(&envoymatcherv3.HttpRequestHeaderMatchInput{HeaderName: header})
	if err != nil {
		return nil, err
	}
	key := &corev3.TypedExtensionConfig{Name: "request_header", TypedConfig: keyInput}
	settings, err := anypb.New(&rlqfv3.RateLimitQuotaBucketSettings{
		BucketIdBuilder: &rlqfv3.RateLimitQuotaBucketSettings_BucketIdBuilder{
			BucketIdBuilder: map[string]*rlqfv3.RateLimitQuotaBucketSettings_BucketIdBuilder_ValueBuilder{
				ratelimit.IDRule: {ValueSpecifier: &rlqfv3.RateLimitQuotaBucketSettings_BucketIdBuilder_ValueBuilder_StringValue{StringValue: rule.Name}},
				ratelimit.IDKey:  {ValueSpecifier: &rlqfv3.RateLimitQuotaBucketSettings_BucketIdBuilder_ValueBuilder_CustomValue{CustomValue: key}},
			},
		},
		// Required, and ignored: Envoy 1.40 reports every 5 seconds.
		ReportingInterval:    durationpb.New(ratelimit.ReportInterval),
		DenyResponseSettings: refusal(rule.Respond),
		NoAssignmentBehavior: &rlqfv3.RateLimitQuotaBucketSettings_NoAssignmentBehavior{
			NoAssignmentBehavior: &rlqfv3.RateLimitQuotaBucketSettings_NoAssignmentBehavior_FallbackRateLimit{FallbackRateLimit: rule.First()},
		},
		ExpiredAssignmentBehavior: &rlqfv3.RateLimitQuotaBucketSettings_ExpiredAssignmentBehavior{
			ExpiredAssignmentBehaviorTimeout: &durationpb.Duration{Seconds: expiredFor},
			ExpiredAssignmentBehavior: &rlqfv3.RateLimitQuotaBucketSettings_ExpiredAssignmentBehavior_FallbackRateLimit{
				FallbackRateLimit: rule.Strategy(),
			},
		},
	})
	if err != nil {
		return nil, err
	}
	// "Has the header": the script only ever sets it to something.
	present := &xdsmatcherv3.StringMatcher{MatchPattern: &xdsmatcherv3.StringMatcher_SafeRegex{SafeRegex: &xdsmatcherv3.RegexMatcher{
		EngineType: &xdsmatcherv3.RegexMatcher_GoogleRe2{GoogleRe2: &xdsmatcherv3.RegexMatcher_GoogleRE2{}},
		Regex:      ".+",
	}}}
	return anypb.New(&rlqfv3.RateLimitQuotaFilterConfig{
		RlqsServer: controlGRPC(),
		Domain:     ratelimit.Domain,
		BucketMatchers: &xdsmatcherv3.Matcher{MatcherType: &xdsmatcherv3.Matcher_MatcherList_{MatcherList: &xdsmatcherv3.Matcher_MatcherList{
			Matchers: []*xdsmatcherv3.Matcher_MatcherList_FieldMatcher{{
				Predicate: &xdsmatcherv3.Matcher_MatcherList_Predicate{MatchType: &xdsmatcherv3.Matcher_MatcherList_Predicate_SinglePredicate_{
					SinglePredicate: &xdsmatcherv3.Matcher_MatcherList_Predicate_SinglePredicate{
						Input:   match,
						Matcher: &xdsmatcherv3.Matcher_MatcherList_Predicate_SinglePredicate_ValueMatch{ValueMatch: present},
					},
				}},
				OnMatch: &xdsmatcherv3.Matcher_OnMatch{OnMatch: &xdsmatcherv3.Matcher_OnMatch_Action{
					Action: &xdscorev3.TypedExtensionConfig{Name: "bucket", TypedConfig: settings},
				}},
			}},
		}}},
	})
}
