package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

// Review finding C7: target creation accepts limits the limit endpoint
// would reject. The same limit is given to PUT /targets/{name}/limit and,
// nested, to POST /targets; the two must agree.

func reviewC7Daemon(t *testing.T) *Server {
	t.Helper()
	m := mockVAST(t)
	s, _ := run(t, t.TempDir())
	addCluster(t, s, m)
	addTarget(t, s, "base", "main", 9000)
	connect(s, 0, 10) // 10 Envoys, 2 VIPs per CNode: limit 100 -> share 5
	return s
}

func addTargetWithLimit(t *testing.T, s *Server, name string, port int, limit map[string]any) (int, string) {
	t.Helper()
	return call(t, s, "POST", "/api/v1/targets", map[string]any{
		"name": name, "cluster": "lab", "pool": "main", "port": 80, "listen_port": port, "limit": limit,
	})
}

func TestReviewC7_MisspelledModeAtCreation(t *testing.T) {
	s := reviewC7Daemon(t)
	bad := map[string]any{"per_cnode": 100, "mode": "enforced"}
	putCode, putBody := call(t, s, "PUT", "/api/v1/targets/base/limit", bad)
	if putCode != http.StatusBadRequest {
		t.Fatalf("control: PUT limit with mode %q: %d %s", bad["mode"], putCode, putBody)
	}
	code, body := addTargetWithLimit(t, s, "typo", 9001, bad)
	if code != http.StatusBadRequest {
		tj := target(t, s, "typo")
		b, _ := json.Marshal(tj.Status.Limit)
		t.Errorf("POST target with limit mode %q: HTTP %d (PUT limit gave %d); limit status %s; per-host limit served to Envoys %d",
			bad["mode"], code, putCode, b, perHost(t, s, "typo"))
	}
	_ = body
}

func TestReviewC7_ZeroLimitAtCreation(t *testing.T) {
	s := reviewC7Daemon(t)
	zero := map[string]any{"per_cnode": 0, "mode": "enforce"}
	// Control: through the limit endpoint, zero means "no limit".
	putCode, putBody := call(t, s, "PUT", "/api/v1/targets/base/limit", zero)
	if putCode != http.StatusOK || target(t, s, "base").Status.Limit != nil {
		t.Fatalf("control: PUT limit 0: %d %s", putCode, putBody)
	}
	code, body := addTargetWithLimit(t, s, "zero", 9001, zero)
	if code != http.StatusCreated {
		if code != http.StatusBadRequest {
			t.Errorf("POST target with limit 0: %d %s", code, body)
		}
		return // refused: acceptable
	}
	tj := target(t, s, "zero")
	if tj.Status.Limit != nil {
		inFlight(s, tj, map[string]uint64{"cnode-3-10": 1})
		tj = target(t, s, "zero")
		b, _ := json.Marshal(tj.Status.Limit)
		t.Errorf("POST target with limit 0 stored a limit (PUT limit 0 removes it): status %s; per-host limit served %d; with 1 request in flight, CNodes reported over the limit: %d",
			b, perHost(t, s, "zero"), len(tj.Over))
	}
}

func TestReviewC7_EmptyModeAtCreation(t *testing.T) {
	s := reviewC7Daemon(t)
	noMode := map[string]any{"per_cnode": 100}
	if code, body := call(t, s, "PUT", "/api/v1/targets/base/limit", noMode); code != http.StatusOK || target(t, s, "base").Status.Limit.Mode != "alert" {
		t.Fatalf("control: PUT limit without a mode: %d %s", code, body)
	}
	code, body := addTargetWithLimit(t, s, "nomode", 9001, noMode)
	if code == http.StatusCreated {
		if l := target(t, s, "nomode").Status.Limit; l == nil || l.Mode != "alert" {
			b, _ := json.Marshal(l)
			t.Errorf("POST target with a limit and no mode: stored mode is not alert as PUT makes it: %s", b)
		}
	} else if code != http.StatusBadRequest {
		t.Errorf("POST target with a limit and no mode: %d %s", code, body)
	}
}
