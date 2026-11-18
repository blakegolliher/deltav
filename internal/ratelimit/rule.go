// Package ratelimit is request-rate limiting for S3 traffic through the
// fleet: rules in tollbooth's vocabulary (which operations, for which access
// keys, buckets and prefixes, how many per second, counted per what), the
// script that recognizes a request inside Envoy, and the quota service that
// divides each rule's rate among the Envoys that need it.
//
// A rule's rate belongs to the whole fleet. Envoy's rate limit quota filter
// reports to the daemon, every few seconds, how many requests each Envoy
// allowed and refused for each bucket of each rule, and the daemon answers
// with that Envoy's share (server.go). No request waits for the daemon.
package ratelimit

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// What a rule's limit can be counted per.
const (
	PerTotal     = "total"      // one count for everything the rule matches
	PerAccessKey = "access_key" // a count for each access key
	PerBucket    = "bucket"     // a count for each bucket
	PerPrefix    = "prefix"     // a count for each of the rule's prefixes
	PerClient    = "client"     // a count for each client host (each Envoy)
)

// Rule limits the rate of the requests it matches. The filters (Operation,
// AccessKey, Bucket, Prefix) are optional; a request must pass all that are
// set, by matching any one of a filter's values. A request that matches
// several rules must be allowed by every one of them.
type Rule struct {
	Name string `json:"name"`
	// Operation holds S3 operation names (PutObject), or families (List*).
	Operation []string `json:"operation,omitempty"`
	// AccessKey holds access key IDs; "-" is a request that carries none.
	AccessKey []string `json:"access_key,omitempty"`
	Bucket    []string `json:"bucket,omitempty"`
	// Prefix holds key prefixes. Each is compared with the object key, or
	// for a listing with its prefix parameter; a request for anything else
	// does not match a rule that has prefixes.
	Prefix []string `json:"prefix,omitempty"`

	// Exactly one of the three limits, or Blocked.
	LimitPerSecond float64 `json:"limit_per_second,omitempty"`
	LimitPerMinute float64 `json:"limit_per_minute,omitempty"`
	LimitPerHour   float64 `json:"limit_per_hour,omitempty"`
	// Blocked refuses every matching request.
	Blocked bool `json:"blocked,omitempty"`
	// Burst is how many requests may pass at once after a quiet spell. The
	// default is the limit's own number, so "20 per hour" lets 20 through
	// together.
	Burst uint32 `json:"burst,omitempty"`
	// Per says what gets a count of its own: PerTotal alone, or any of the
	// others together. Required unless Blocked: a rate with no "for whom"
	// throttles everyone at once.
	Per []string `json:"per,omitempty"`
	// Respond is the status a refused request gets: 503 (SlowDown, what S3
	// itself answers, and the default) or 429.
	Respond int    `json:"respond,omitempty"`
	Note    string `json:"note,omitempty"`
}

// Bounds on a rule's rate, in requests per second.
const (
	minRate = 1.0 / 3600 // one an hour
	maxRate = 1e6
)

var (
	nameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	// ErrInvalid marks a rule that cannot be used as written.
	ErrInvalid = errors.New("invalid rule")
)

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Normalize checks the rule and puts it in its canonical form: operations
// in S3's spelling, lists sorted with no repeats, defaults filled in.
func (r *Rule) Normalize() error {
	if !nameRE.MatchString(r.Name) {
		return invalid("name %q: use 1 to 64 letters, digits, dots, dashes and underscores", r.Name)
	}
	for i, op := range r.Operation {
		if op == "*" {
			return invalid("operation \"*\": leave the operation out to mean every operation")
		}
		got := expandOperation(op)
		if len(got) == 0 {
			return invalid("operation %q: not an S3 operation this knows, nor the start of one followed by * (see \"deltav rule operations\")", op)
		}
		if strings.HasSuffix(op, "*") {
			// Keep a family as written but in the spelling of its members.
			r.Operation[i] = got[0][:len(op)-1] + "*"
		} else {
			r.Operation[i] = got[0]
		}
	}
	for what, list := range map[string]*[]string{"operation": &r.Operation, "access_key": &r.AccessKey, "bucket": &r.Bucket, "prefix": &r.Prefix} {
		seen := map[string]bool{}
		out := (*list)[:0]
		for _, v := range *list {
			switch {
			case v == "":
				return invalid("%s: an empty value", what)
			case len(v) > 1024:
				return invalid("%s: a value longer than 1024 bytes", what)
			case strings.ContainsAny(v, "\r\n\x00"):
				return invalid("%s %q: line breaks are not allowed", what, v)
			}
			if !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
		sort.Strings(out)
		*list = out
		if len(out) > 1000 {
			return invalid("%s: more than 1000 values", what)
		}
	}

	limits := 0
	for _, l := range []float64{r.LimitPerSecond, r.LimitPerMinute, r.LimitPerHour} {
		if l < 0 || math.IsNaN(l) || math.IsInf(l, 0) {
			return invalid("a limit must be a positive number")
		}
		if l > 0 {
			limits++
		}
	}
	switch {
	case r.Blocked && (limits > 0 || r.Burst > 0):
		return invalid("a blocked rule refuses everything: it cannot also have a limit or a burst")
	case !r.Blocked && limits != 1:
		return invalid("give exactly one of limit_per_second, limit_per_minute, limit_per_hour, or blocked")
	}
	if !r.Blocked {
		if rate := r.Rate(); rate < minRate || rate > maxRate {
			return invalid("the limit must be between one per hour and %g per second", maxRate)
		}
		if r.Burst == 0 {
			r.Burst = r.defaultBurst()
		}
		if float64(r.Burst)/r.Rate() > 24*3600 {
			return invalid("burst %d would take more than a day to refill at this rate", r.Burst)
		}
	}

	seen := map[string]bool{}
	for _, p := range r.Per {
		switch p {
		case PerTotal, PerAccessKey, PerBucket, PerPrefix, PerClient:
		default:
			return invalid("per %q: use total, or any of access_key, bucket, prefix, client", p)
		}
		if seen[p] {
			return invalid("per: %s is given twice", p)
		}
		seen[p] = true
	}
	sort.Strings(r.Per)
	switch {
	case r.Blocked && len(r.Per) > 0:
		return invalid("a blocked rule counts nothing: leave per out")
	case !r.Blocked && len(r.Per) == 0:
		return invalid("say what the limit is counted per: total, or any of access_key, bucket, prefix, client")
	case seen[PerTotal] && len(r.Per) > 1:
		return invalid("per: total means one count for everything and cannot be combined")
	case seen[PerPrefix] && len(r.Prefix) == 0:
		return invalid("per: prefix needs the rule to list prefixes")
	}

	switch r.Respond {
	case 0:
		r.Respond = 503
	case 503, 429:
	default:
		return invalid("respond %d: use 503 (SlowDown, what S3 answers) or 429", r.Respond)
	}
	if len(r.Note) > 200 || strings.ContainsAny(r.Note, "\r\n") {
		return invalid("note: at most 200 characters on one line")
	}
	return nil
}

// Rate is the rule's limit in requests per second; 0 for a blocked rule.
func (r *Rule) Rate() float64 {
	switch {
	case r.LimitPerSecond > 0:
		return r.LimitPerSecond
	case r.LimitPerMinute > 0:
		return r.LimitPerMinute / 60
	case r.LimitPerHour > 0:
		return r.LimitPerHour / 3600
	}
	return 0
}

func (r *Rule) defaultBurst() uint32 {
	n := math.Max(r.LimitPerSecond, math.Max(r.LimitPerMinute, r.LimitPerHour))
	return uint32(math.Max(1, math.Min(math.Ceil(n), math.MaxUint32)))
}

// Counts reports whether the rule keeps a separate count per the given
// dimension.
func (r *Rule) Counts(per string) bool {
	for _, p := range r.Per {
		if p == per {
			return true
		}
	}
	return false
}

// Ops returns the operations the rule matches, with families expanded, or
// nil if it matches every operation.
func (r *Rule) Ops() []string {
	seen := map[string]bool{}
	var out []string
	for _, o := range r.Operation {
		for _, op := range expandOperation(o) {
			if !seen[op] {
				seen[op] = true
				out = append(out, op)
			}
		}
	}
	sort.Strings(out)
	return out
}

// amount writes a limit's number: to three decimals, or for a number under
// one to three figures, so that the smallest limit there can be (one an
// hour, said per second) does not read as zero.
func amount(v float64) string {
	if v > 0 && v < 1 {
		return strconv.FormatFloat(v, 'g', 3, 64)
	}
	return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.3f", v), "0"), ".")
}

// limitParts returns the rule's limit as a number and the unit of time it
// is per.
func (r *Rule) limitParts() (n, unit string) {
	switch {
	case r.LimitPerSecond > 0:
		return amount(r.LimitPerSecond), "second"
	case r.LimitPerMinute > 0:
		return amount(r.LimitPerMinute), "minute"
	}
	return amount(r.LimitPerHour), "hour"
}

// Limit says the rule's limit in words: "600 per minute", "blocked".
func (r *Rule) Limit() string {
	if r.Blocked {
		return "blocked"
	}
	n, unit := r.limitParts()
	return n + " per " + unit
}

func oneOf(noun, plural string, values []string) string {
	if len(values) == 1 {
		return noun + " " + values[0]
	}
	if len(values) <= 3 {
		return plural + " " + strings.Join(values[:len(values)-1], ", ") + " and " + values[len(values)-1]
	}
	return fmt.Sprintf("%d %s", len(values), plural)
}

// Sentence says what the rule does in one or two sentences, for a person
// reading a list of rules.
func (r *Rule) Sentence() string {
	what := "requests"
	if len(r.Operation) > 0 {
		if len(r.Operation) <= 3 {
			what = strings.Join(r.Operation, ", ") + " calls"
		} else {
			what = fmt.Sprintf("calls to %d operations", len(r.Operation))
		}
	}
	var only []string
	if len(r.AccessKey) > 0 {
		only = append(only, "from "+oneOf("access key", "access keys", r.AccessKey))
	}
	if len(r.Bucket) > 0 {
		only = append(only, "to "+oneOf("bucket", "buckets", r.Bucket))
	}
	if len(r.Prefix) > 0 {
		only = append(only, "under "+oneOf("prefix", "prefixes", r.Prefix))
	}
	scope := ""
	if len(only) > 0 {
		scope = " " + strings.Join(only, ", ")
	}
	answer := "503 SlowDown"
	if r.Respond == 429 {
		answer = "429 TooManyRequests"
	}
	if r.Blocked {
		return fmt.Sprintf("All %s%s are refused with %s.", what, scope, answer)
	}
	var each []string
	for _, p := range r.Per {
		switch p {
		case PerAccessKey:
			each = append(each, "each access key")
		case PerBucket:
			each = append(each, "each bucket")
		case PerPrefix:
			each = append(each, "each listed prefix")
		case PerClient:
			each = append(each, "each client host")
		}
	}
	whom := "in total across the fleet"
	if len(each) > 0 {
		whom = "for " + strings.Join(each, " and ")
		if !r.Counts(PerClient) {
			whom += ", across the fleet"
		}
	}
	n, unit := r.limitParts()
	s := fmt.Sprintf("At most %s %s per %s%s, %s.", n, what, unit, scope, whom)
	if r.Burst != r.defaultBurst() {
		s += fmt.Sprintf(" Up to %d may pass at once.", r.Burst)
	}
	return s + fmt.Sprintf(" Over the limit, the client gets %s.", answer)
}

// Validate checks a whole list: each rule, and that names are not reused.
func Validate(rules []Rule) error {
	seen := map[string]bool{}
	for i := range rules {
		if err := rules[i].Normalize(); err != nil {
			return fmt.Errorf("rule %q: %w", rules[i].Name, err)
		}
		if seen[rules[i].Name] {
			return invalid("two rules are named %q", rules[i].Name)
		}
		seen[rules[i].Name] = true
	}
	return nil
}
