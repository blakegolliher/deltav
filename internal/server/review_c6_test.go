package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"deltav/internal/ratelimit"
	"deltav/internal/xds"
)

// Review finding C6: a successful API response can conceal a failed config
// change. Each test below asserts what the API should do if it promises
// publication (a 2xx only once Envoys are offered the change) and runs it
// against a daemon whose publication has been made to fail.

// breakPublication makes every poller.apply fail before anything is
// published: it adds a static target with no listen port, from which
// xds.TargetSpec.Resources cannot build a config. Nothing in production
// code is changed; the static targets are the package's own field. It
// returns the function that undoes it. The caller must not hold applyMu
// unless check is false.
func breakPublication(t *testing.T, s *Server, check bool) (restore func()) {
	t.Helper()
	s.staticMu.Lock()
	old := s.static
	s.static = append(append([]xds.TargetSpec(nil), old...), xds.TargetSpec{Name: "review-broken"})
	s.staticMu.Unlock()
	if check {
		if _, _, err := s.poller.apply(context.Background()); err == nil {
			t.Fatal("fault injection: apply still succeeds")
		}
	}
	return func() {
		s.staticMu.Lock()
		s.static = old
		s.staticMu.Unlock()
	}
}

// TestReviewC6_RuleRollbackFailureIsReportedAsRestored makes the first
// publication of a rule fail and then makes the rollback fail too (the
// state directory refuses the write that would put the old rules back). The
// caller was told the rules "were left as they were"; that must then be
// true of the store and the quota service. Otherwise the answer must say
// the change is stored (a 500), and then a later poll serving it is what
// the caller was told.
func TestReviewC6_RuleRollbackFailureIsReportedAsRestored(t *testing.T) {
	dir := t.TempDir()
	s, _ := run(t, dir)
	if _, _, err := s.SetStaticTargets(t.Context(), []xds.TargetSpec{{Name: "fixed", ListenPort: 9002, Endpoints: []xds.Endpoint{{IP: "192.0.2.1", Port: 80}}}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	// Hold the poller's apply lock so the handler stops after it has
	// stored the rule and before it publishes.
	s.poller.applyMu.Lock()
	locked := true
	defer func() {
		if locked {
			s.poller.applyMu.Unlock()
		}
	}()
	type answer struct {
		code int
		body string
	}
	done := make(chan answer, 1)
	go func() {
		code, body := call(t, s, "PUT", "/api/v1/rules/r1", map[string]any{"limit_per_second": 5, "per": []string{"total"}})
		done <- answer{code, body}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for !hasRule(s.store.Rules(), "r1") {
		if time.Now().After(deadline) {
			t.Fatal("the handler never stored the rule")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The handler is now at (or about to reach) its apply. Make that apply
	// fail, and make the store's rollback fail.
	restore := breakPublication(t, s, false)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	s.poller.applyMu.Unlock()
	locked = false
	var got answer
	select {
	case got = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("PUT rule did not answer")
	}
	quotaRules, _ := s.quota.Status()
	_, quotaHas := quotaRules["r1"]
	storeHas := hasRule(s.store.Rules(), "r1")
	t.Logf("PUT rule: HTTP %d %s", got.code, strings.TrimSpace(got.body))
	leftAsTheyWere := strings.Contains(got.body, "left as they were")
	if leftAsTheyWere && (storeHas || quotaHas) {
		t.Errorf("caller told the rules were left as they were, but the store still has r1=%v and the quota service knows r1=%v", storeHas, quotaHas)
	}
	if !leftAsTheyWere && (got.code != 500 || !strings.Contains(got.body, "the change is stored but not served") || !storeHas || !quotaHas) {
		t.Errorf("PUT rule: HTTP %d %s; store has r1=%v, quota knows r1=%v; want a 500 saying the change is stored, and the store and the quota service both holding it",
			got.code, got.body, storeHas, quotaHas)
	}

	// The next poll publishes what the store holds.
	restore()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.poller.apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !leftAsTheyWere {
		return
	}
	if _, script, _ := served(t, s, "fixed"); strings.Contains(script, `name="r1"`) {
		t.Errorf("after the next apply Envoys are served rule r1, which the caller was told was not kept")
	}
	if b, err := os.ReadFile(filepath.Join(dir, "state.json")); err == nil && strings.Contains(string(b), `"r1"`) {
		t.Errorf("rule r1 is in the state file on disk and will be served after a restart")
	}
}

// TestReviewC6_RuleRollbackWithOnlyPublishFailing is the control for the
// test above: publication fails (both the change's and the rollback's
// apply) but the store can write. The store and the quota service are then
// as they were, nothing new is served, and the answer says so: a 500, since
// the old rules could not be served again either.
func TestReviewC6_RuleRollbackWithOnlyPublishFailing(t *testing.T) {
	s, _ := run(t, t.TempDir())
	if _, _, err := s.SetStaticTargets(t.Context(), []xds.TargetSpec{{Name: "fixed", ListenPort: 9002, Endpoints: []xds.Endpoint{{IP: "192.0.2.1", Port: 80}}}}); err != nil {
		t.Fatal(err)
	}
	v := s.XDS().Version()
	restore := breakPublication(t, s, true)
	code, body := call(t, s, "PUT", "/api/v1/rules/r1", map[string]any{"limit_per_second": 5, "per": []string{"total"}})
	quotaRules, _ := s.quota.Status()
	_, quotaHas := quotaRules["r1"]
	if code != 500 || !strings.Contains(body, "the old ones are stored again") || hasRule(s.store.Rules(), "r1") || quotaHas || s.XDS().Version() != v {
		t.Errorf("PUT rule: %d %s; store has r1=%v, quota knows r1=%v, version %s -> %s", code, body, hasRule(s.store.Rules(), "r1"), quotaHas, v, s.XDS().Version())
	}
	restore()
	if _, _, err := s.poller.apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, script, _ := served(t, s, "fixed"); strings.Contains(script, `name="r1"`) {
		t.Error("r1 served after publication was restored")
	}
}

func hasRule(rules []ratelimit.Rule, name string) bool {
	for _, r := range rules {
		if r.Name == name {
			return true
		}
	}
	return false
}

// TestReviewC6_TargetAddRollbackFailureIsReported is the same double fault
// for a new target: it cannot be served, and the store refuses to remove it
// again. The answer must say it is stored.
func TestReviewC6_TargetAddRollbackFailureIsReported(t *testing.T) {
	dir := t.TempDir()
	m := mockVAST(t)
	s, _ := run(t, dir)
	addCluster(t, s, m)
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	s.poller.applyMu.Lock()
	locked := true
	defer func() {
		if locked {
			s.poller.applyMu.Unlock()
		}
	}()
	type answer struct {
		code int
		body string
	}
	done := make(chan answer, 1)
	go func() {
		code, body := call(t, s, "POST", "/api/v1/targets", map[string]any{"name": "s3", "cluster": "lab", "pool": "main", "port": 80, "listen_port": 9000})
		done <- answer{code, body}
	}()
	stored := func() bool {
		for _, tg := range s.store.Targets() {
			if tg.Name == "s3" {
				return true
			}
		}
		return false
	}
	deadline := time.Now().Add(10 * time.Second)
	for !stored() {
		if time.Now().After(deadline) {
			t.Fatal("the handler never stored the target")
		}
		time.Sleep(5 * time.Millisecond)
	}
	restore := breakPublication(t, s, false)
	defer restore()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	s.poller.applyMu.Unlock()
	locked = false
	var got answer
	select {
	case got = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("POST target did not answer")
	}
	if got.code != 500 || !strings.Contains(got.body, "it is stored but not served") || !stored() {
		t.Errorf("POST target: HTTP %d %s; stored=%v; want a 500 saying it is stored", got.code, strings.TrimSpace(got.body), stored())
	}
}
