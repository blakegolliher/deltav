package server

// Review S3: a partial first poll after a restart (inventory read, DNS not)
// must not erase the cached bucket-in-host-name suffixes; a later
// successful, empty DNS answer must clear them.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"deltav/internal/vast"
)

func reviewSuffixes(s *Server, target string) []string {
	spec, ok := reviewSpec(s, target)
	if !ok {
		return []string{"<target not served>"}
	}
	return spec.S3Hosts.Suffixes
}

// reviewScriptSuffixes is the SUFFIXES line of the rate-rule script the
// target's route carries.
func reviewScriptSuffixes(t *testing.T, s *Server, target string) string {
	t.Helper()
	_, script, _ := served(t, s, target)
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(line, "local SUFFIXES, POOLS = ") {
			return strings.TrimPrefix(line, "local SUFFIXES, POOLS = ")
		}
	}
	return "<no SUFFIXES line>"
}

// reviewS3Setup serves target s3 with a bucket rule from a cluster whose DNS
// suffix is s3.example.com, then stops that daemon.
func reviewS3Setup(t *testing.T, dir string, f *reviewFake) {
	t.Helper()
	f.set(func(f *reviewFake) { f.dns = []vast.DNS{{Name: "dns", DomainSuffix: "s3.example.com", Enabled: true}} })
	s1 := reviewServer(t, dir, map[string]*reviewFake{"lab-host": f})
	reviewAddCluster(t, s1, "lab", "lab-host")
	reviewAddTarget(t, s1, "s3", "lab", "main", 9000)
	if code, body := reviewDo(t, s1, "PUT", "/api/v1/rules/no-archive", map[string]any{"bucket": []string{"archive"}, "blocked": true}); code != http.StatusOK {
		t.Fatalf("setting a rule: %d %s", code, body)
	}
	s1.poller.pollOnce(context.Background())
	if got := reviewSuffixes(s1, "s3"); len(got) != 1 || got[0] != "s3.example.com" {
		t.Fatalf("before the restart, suffixes %v", got)
	}
	if h := reviewCache(t, s1).Hosts["s3"]; len(h.Suffixes) != 1 {
		t.Fatalf("before the restart, cached hosts %+v", h)
	}
	s1.cancel()
}

func TestReviewS3_DNSFailureOnFirstPollAfterRestartKeepsSuffixes(t *testing.T) {
	dir := t.TempDir()
	f := reviewTopology("main", "10.0.0.1", "10.0.0.2", "10.0.0.3")
	reviewS3Setup(t, dir, f)

	// Restart. The inventory reads succeed; the DNS read is refused.
	f.set(func(f *reviewFake) { f.dnsErr = errors.New("403 Forbidden: dns") })
	s2 := reviewServer(t, dir, map[string]*reviewFake{"lab-host": f})
	if got := reviewSuffixes(s2, "s3"); len(got) != 1 || got[0] != "s3.example.com" {
		t.Fatalf("right after the restart (from the cache), suffixes %v", got)
	}
	s2.poller.pollOnce(context.Background())

	if got := reviewSuffixes(s2, "s3"); len(got) != 1 || got[0] != "s3.example.com" {
		t.Errorf("after a first poll whose DNS read failed, served S3Hosts.Suffixes = %q, want [s3.example.com]", got)
	}
	if got := reviewScriptSuffixes(t, s2, "s3"); !strings.HasPrefix(got, `{"s3.example.com"}`) {
		t.Errorf("after a first poll whose DNS read failed, the rule script has SUFFIXES, POOLS = %s", got)
	}
	st, _ := s2.poller.targetStatus("s3")
	if len(st.BucketHosts) != 1 || st.BucketHostsProblem == "" {
		t.Errorf("status: bucket_hosts %v, bucket_hosts_problem %q", st.BucketHosts, st.BucketHostsProblem)
	}
	if h := reviewCache(t, s2).Hosts["s3"]; len(h.Suffixes) != 1 {
		t.Errorf("the restart cache on disk lost the suffixes too: hosts %+v", h)
	}
}

// The other half: a DNS read that succeeds with no suffix is believed, after
// a restart and within one process alike.
func TestReviewS3_SuccessfulEmptyDNSClearsSuffixes(t *testing.T) {
	t.Run("after restart", func(t *testing.T) {
		dir := t.TempDir()
		f := reviewTopology("main", "10.0.0.1", "10.0.0.2", "10.0.0.3")
		reviewS3Setup(t, dir, f)
		f.set(func(f *reviewFake) { f.dns = nil })
		s2 := reviewServer(t, dir, map[string]*reviewFake{"lab-host": f})
		s2.poller.pollOnce(context.Background())
		if got := reviewSuffixes(s2, "s3"); len(got) != 0 {
			t.Errorf("after a successful empty DNS read, suffixes %v", got)
		}
		if h := reviewCache(t, s2).Hosts["s3"]; len(h.Suffixes) != 0 {
			t.Errorf("after a successful empty DNS read, cached hosts %+v", h)
		}
	})
	t.Run("after a failed read in the same process", func(t *testing.T) {
		dir := t.TempDir()
		f := reviewTopology("main", "10.0.0.1", "10.0.0.2", "10.0.0.3")
		reviewS3Setup(t, dir, f)
		s2 := reviewServer(t, dir, map[string]*reviewFake{"lab-host": f})
		ctx := context.Background()
		s2.poller.pollOnce(ctx) // DNS read fine
		f.set(func(f *reviewFake) { f.dnsErr = errors.New("403") })
		s2.poller.pollOnce(ctx) // DNS read fails: kept (existing behaviour)
		if got := reviewSuffixes(s2, "s3"); len(got) != 1 {
			t.Fatalf("with DNS failing after a good read, suffixes %v", got)
		}
		f.set(func(f *reviewFake) { f.dnsErr, f.dns = nil, nil })
		s2.poller.pollOnce(ctx)
		if got := reviewSuffixes(s2, "s3"); len(got) != 0 {
			t.Errorf("after a successful empty DNS read, suffixes %v", got)
		}
	})
}
