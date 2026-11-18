package cli

import (
	"strings"
	"testing"
)

func TestRuleCommands(t *testing.T) {
	api, _ := daemon(t)

	if got := mustRun(t, "rule", "list", "--api", api); !strings.Contains(got, "No rate rules") {
		t.Errorf("no rules yet: %q", got)
	}
	got := mustRun(t, "rule", "set", "uploads", "--api", api, "--operation", "putobject,UploadPart", "--access-key", "AKIAEXAMPLE1",
		"--per-second", "2000", "--burst", "4000", "--per", "access_key,bucket", "--note", "agreed with the tenant")
	want := "uploads: At most 2000 PutObject, UploadPart calls per second from access key AKIAEXAMPLE1, for each access key and each bucket, across the fleet. " +
		"Up to 4000 may pass at once. Over the limit, the client gets 503 SlowDown.\n"
	if got != want {
		t.Errorf("rule set:\n got  %q\n want %q", got, want)
	}
	// "add" is the same command.
	mustRun(t, "rule", "add", "listings", "--api", api, "--operation", "List*", "--per-minute", "600", "--per", "total")
	mustRun(t, "rule", "set", "no-deletes", "--api", api, "--operation", "DeleteObject", "--bucket", "archive", "--prefix", "a,b/", "--prefix", "c/", "--blocked", "--respond", "429")

	got = mustRun(t, "rule", "list", "--api", api)
	lines := strings.Split(got, "\n")
	fields := func(i int) string { return strings.Join(strings.Fields(lines[i]), " ") }
	if fields(0) != "NAME LIMIT PER OPERATION ONLY FOR RESPOND KEYS ALLOWED/S REFUSED/S" ||
		fields(1) != "uploads 2000 per second access_key, bucket PutObject, UploadPart key AKIAEXAMPLE1 503 0 0 0" ||
		fields(2) != "listings 600 per minute total List* - 503 0 0 0" ||
		// A prefix may hold a comma: each --prefix is one prefix.
		fields(3) != "no-deletes blocked - DeleteObject bucket archive; prefix a,b/, c/ 429 0 0 0" {
		t.Errorf("rule list:\n%s", got)
	}
	if !strings.Contains(got, "\nlistings: At most 600 List* calls per minute, in total across the fleet.") ||
		!strings.Contains(got, "\nno-deletes: All DeleteObject calls to bucket archive, under prefixes a,b/ and c/ are refused with 429 TooManyRequests.") {
		t.Errorf("rule list should say each rule in words:\n%s", got)
	}
	if got := mustRun(t, "rule", "list", "--api", api, "--json"); !strings.Contains(got, `"sentence"`) || !strings.Contains(got, `"limit_per_minute": 600`) {
		t.Errorf("rule list --json: %s", got)
	}

	got = mustRun(t, "rule", "show", "uploads", "--api", api)
	for _, want := range []string{"uploads: At most 2000", "Note: agreed with the tenant", "0 allowed and 0 refused per second", "Nothing has matched it"} {
		if !strings.Contains(got, want) {
			t.Errorf("rule show lacks %q:\n%s", want, got)
		}
	}
	if _, err := run(t, "dv", "rule", "show", "nope", "--api", api); err == nil || !strings.Contains(err.Error(), `no rule "nope"`) {
		t.Errorf("show of a rule that is not there: %v", err)
	}

	// The daemon's own words for what is wrong with a rule.
	for what, args := range map[string][]string{
		"an operation that is not one": {"--operation", "WRITE", "--per-second", "5", "--per", "total"},
		"no per":                       {"--per-second", "5"},
		"two limits":                   {"--per-second", "5", "--per-hour", "5", "--per", "total"},
	} {
		if _, err := run(t, "dv", append([]string{"rule", "set", "bad", "--api", api}, args...)...); err == nil || !strings.Contains(err.Error(), "invalid rule") {
			t.Errorf("%s: %v", what, err)
		}
	}
	if _, err := run(t, "dv", "rule", "set", "bad", "--api", api, "--per", "total"); err == nil || !strings.Contains(err.Error(), "give a limit") {
		t.Errorf("no limit: %v", err)
	}

	if got := mustRun(t, "rule", "remove", "listings", "--api", api); got != "removed rule listings\n" {
		t.Errorf("rule remove: %q", got)
	}
	if _, err := run(t, "dv", "rule", "remove", "listings", "--api", api); err == nil || !strings.Contains(err.Error(), "no rule") {
		t.Errorf("removing it again: %v", err)
	}
	if got := mustRun(t, "rule", "list", "--api", api); strings.Contains(got, "listings") || !strings.Contains(got, "uploads") {
		t.Errorf("after removing: %s", got)
	}
	// The record of what rules refused. A daemon no Envoy has spoken to
	// has none; real ones are in the tests against Envoy.
	if got := mustRun(t, "rule", "refusals", "--api", api); !strings.HasSuffix(got, "\nNo rule has refused a request since the daemon started.\n") || strings.Count(got, "\n") != 2 {
		t.Errorf("rule refusals with none: %q", got)
	}
	if got := mustRun(t, "rule", "refusals", "--api", api, "--json"); !strings.Contains(got, `"refusals": []`) {
		t.Errorf("rule refusals --json: %s", got)
	}
	if _, err := run(t, "dv", "rule", "refusals", "--api", api, "--limit", "0"); err == nil || !strings.Contains(err.Error(), "--limit") {
		t.Errorf("rule refusals --limit 0: %v", err)
	}
	// One request can be limited by only so many rules; with more than
	// that, the list says which ones count.
	note := "One request can be limited by at most 4 rules."
	if got := mustRun(t, "rule", "list", "--api", api); strings.Contains(got, note) {
		t.Errorf("with two rules, one of them blocked:\n%s", got)
	}
	for _, name := range []string{"a", "b", "c", "d"} {
		mustRun(t, "rule", "set", name, "--api", api, "--per-second", "5", "--per", "total")
	}
	if got := mustRun(t, "rule", "list", "--api", api); !strings.Contains(got, "\n\n"+note+" One that matches more is limited by the first 4 in this order; blocked rules always apply.\n") {
		t.Errorf("with five limits:\n%s", got)
	}
	if got := mustRun(t, "rule", "set", "--help"); !strings.Contains(got, "at most 4 rules") || !strings.Contains(got, "before VAST checks its") || !strings.Contains(got, "blocked rules go on refusing") {
		t.Errorf("rule set --help does not say how rules behave:\n%s", got)
	}
	if got := mustRun(t, "rule", "operations"); !strings.Contains(got, "\nPutObject\n") || !strings.Contains(got, "\nListObjectsV2\n") || strings.Count(got, "\n") < 100 {
		t.Errorf("rule operations: %d lines", strings.Count(got, "\n"))
	}
}
