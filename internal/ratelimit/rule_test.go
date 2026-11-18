package ratelimit

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	r := Rule{Name: "tenant-a", Operation: []string{"putobject", "LIST*", "PutObject"}, AccessKey: []string{"B", "A", "B"},
		LimitPerMinute: 600, Per: []string{PerBucket, PerAccessKey}}
	if err := r.Normalize(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.Operation, ","); got != "List*,PutObject" {
		t.Errorf("operations: %s", got)
	}
	if got := strings.Join(r.AccessKey, ","); got != "A,B" {
		t.Errorf("access keys: %s", got)
	}
	if r.Respond != 503 || r.Burst != 600 || r.Rate() != 10 || strings.Join(r.Per, ",") != "access_key,bucket" {
		t.Errorf("defaults: respond %d, burst %d, rate %v, per %v", r.Respond, r.Burst, r.Rate(), r.Per)
	}
	ops := r.Ops()
	if len(ops) < 10 || ops[0] != "ListBucketAnalyticsConfigurations" || ops[len(ops)-1] != "PutObject" {
		t.Errorf("expanded operations: %v", ops)
	}
	for _, op := range ops {
		if op != "PutObject" && !strings.HasPrefix(op, "List") {
			t.Errorf("List* expanded to %s", op)
		}
	}
	// Normalizing again changes nothing.
	again := r
	again.Operation, again.AccessKey, again.Per = append([]string(nil), r.Operation...), append([]string(nil), r.AccessKey...), append([]string(nil), r.Per...)
	if err := again.Normalize(); err != nil || strings.Join(again.Operation, ",") != strings.Join(r.Operation, ",") || again.Burst != r.Burst {
		t.Errorf("not stable: %+v, %v", again, err)
	}

	hour := Rule{Name: "h", LimitPerHour: 20, Per: []string{PerTotal}}
	if err := hour.Normalize(); err != nil || hour.Burst != 20 || hour.Rate() != 20.0/3600 {
		t.Errorf("20 per hour: burst %d, rate %v, %v", hour.Burst, hour.Rate(), err)
	}
	small := Rule{Name: "s", LimitPerSecond: 0.5, Per: []string{PerTotal}}
	if err := small.Normalize(); err != nil || small.Burst != 1 {
		t.Errorf("0.5 per second: burst %d, %v", small.Burst, err)
	}
	blocked := Rule{Name: "b", Operation: []string{"DeleteObject"}, Blocked: true, Respond: 429}
	if err := blocked.Normalize(); err != nil || blocked.Rate() != 0 || blocked.Burst != 0 {
		t.Errorf("blocked: %+v, %v", blocked, err)
	}
}

func TestNormalizeRefuses(t *testing.T) {
	ok := Rule{Name: "ok", LimitPerSecond: 10, Per: []string{PerTotal}}
	for what, change := range map[string]func(*Rule){
		"no name":                         func(r *Rule) { r.Name = "" },
		"a name with a space":             func(r *Rule) { r.Name = "two words" },
		"an operation that is not one":    func(r *Rule) { r.Operation = []string{"WRITE"} },
		"a bare star":                     func(r *Rule) { r.Operation = []string{"*"} },
		"a family that matches nothing":   func(r *Rule) { r.Operation = []string{"Frob*"} },
		"an empty access key":             func(r *Rule) { r.AccessKey = []string{""} },
		"a bucket with a line break":      func(r *Rule) { r.Bucket = []string{"a\nb"} },
		"no limit":                        func(r *Rule) { r.LimitPerSecond = 0 },
		"two limits":                      func(r *Rule) { r.LimitPerHour = 5 },
		"a negative limit":                func(r *Rule) { r.LimitPerSecond = -1 },
		"a limit under one per hour":      func(r *Rule) { r.LimitPerSecond, r.LimitPerHour = 0, 0.5 },
		"a limit over a million":          func(r *Rule) { r.LimitPerSecond = 2e6 },
		"blocked with a limit":            func(r *Rule) { r.Blocked = true },
		"blocked with per":                func(r *Rule) { r.Blocked, r.LimitPerSecond = true, 0 },
		"no per":                          func(r *Rule) { r.Per = nil },
		"an unknown per":                  func(r *Rule) { r.Per = []string{"source_ip"} },
		"total with another":              func(r *Rule) { r.Per = []string{PerTotal, PerBucket} },
		"per twice":                       func(r *Rule) { r.Per = []string{PerBucket, PerBucket} },
		"per prefix without prefixes":     func(r *Rule) { r.Per = []string{PerPrefix} },
		"a status that is not 503 or 429": func(r *Rule) { r.Respond = 500 },
		"a note of two lines":             func(r *Rule) { r.Note = "a\nb" },
		"a burst that takes days":         func(r *Rule) { r.LimitPerSecond, r.LimitPerHour, r.Burst = 0, 1, 100 },
	} {
		r := ok
		r.Per = append([]string(nil), ok.Per...)
		change(&r)
		if err := r.Normalize(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", what, err)
		}
	}
	if err := Validate([]Rule{ok, ok}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "two rules are named") {
		t.Errorf("a repeated name: %v", err)
	}
	if err := Validate([]Rule{ok, {Name: "bad"}}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), `rule "bad"`) {
		t.Errorf("an invalid rule in a list: %v", err)
	}
}

func TestSentence(t *testing.T) {
	for _, c := range []struct {
		r    Rule
		want string
	}{
		{Rule{Name: "a", Operation: []string{"ListObjectsV2"}, LimitPerSecond: 20, Per: []string{PerAccessKey}},
			"At most 20 ListObjectsV2 calls per second, for each access key, across the fleet. Over the limit, the client gets 503 SlowDown."},
		{Rule{Name: "b", Operation: []string{"List*"}, LimitPerMinute: 600, Per: []string{PerTotal}},
			"At most 600 List* calls per minute, in total across the fleet. Over the limit, the client gets 503 SlowDown."},
		{Rule{Name: "c", AccessKey: []string{"AKIA1"}, Bucket: []string{"x", "y"}, LimitPerSecond: 2000, Burst: 4000, Per: []string{PerAccessKey, PerClient}, Respond: 429},
			"At most 2000 requests per second from access key AKIA1, to buckets x and y, for each access key and each client host. Up to 4000 may pass at once. Over the limit, the client gets 429 TooManyRequests."},
		{Rule{Name: "d", Operation: []string{"DeleteObject"}, Bucket: []string{"archive"}, Blocked: true},
			"All DeleteObject calls to bucket archive are refused with 503 SlowDown."},
	} {
		if err := c.r.Normalize(); err != nil {
			t.Fatal(err)
		}
		if got := c.r.Sentence(); got != c.want {
			t.Errorf("%s:\n got  %s\n want %s", c.r.Name, got, c.want)
		}
	}
}
