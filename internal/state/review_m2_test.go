package state

// Review M2: a state file written by a newer build (higher version, fields
// this build does not know) must be refused, not opened writable: the
// next save would drop what this build cannot represent.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const reviewFutureState = `{
  "version": 2,
  "clusters": [
    {"name": "lab", "host": "vast.example.com", "user": "admin", "password": "pw", "mgmt_vips": ["192.0.2.10"]}
  ],
  "targets": [
    {"name": "main", "cluster": "lab", "pool": "main", "port": 80, "listen_port": 9000, "weights": {"cnode-1": 2}}
  ],
  "quotas": [{"name": "q1", "bytes_per_second": 1000}]
}
`

func TestReviewM2_FutureVersionIsRefusedAndUntouched(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte(reviewFutureState), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err == nil {
		t.Errorf("Open accepted a version-2 state file with unknown fields")
		// What follows from accepting it: one ordinary change rewrites the
		// file from the fields this build knows.
		if err := s.SetDrained("lab", "cnode-1", true); err != nil {
			t.Fatal(err)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, []byte(reviewFutureState)) {
		var lost []string
		for _, field := range []string{`"mgmt_vips"`, `"weights"`, `"quotas"`} {
			if !strings.Contains(string(after), field) {
				lost = append(lost, field)
			}
		}
		t.Errorf("the state file changed; fields lost: %v; version line now: %q", lost, firstLineWith(string(after), `"version"`))
	}
}

func firstLineWith(s, sub string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, sub) {
			return strings.TrimSpace(line)
		}
	}
	return ""
}
