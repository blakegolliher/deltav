package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"deltav/internal/xds"
)

// Review finding M7: the request decoder accepts trailing data. One JSON
// value followed only by whitespace is a request; anything more is a
// malformed one and must be a 400.

func TestReviewM7_ReadJSONRejectsTrailingData(t *testing.T) {
	const one = `{"per_cnode":5,"mode":"alert"}`
	for _, tc := range []struct {
		name, body string
		ok         bool
	}{
		{"one value", one, true},
		{"trailing whitespace", one + "\n  \t\r\n", true},
		{"second value", one + `{"per_cnode":0}`, false},
		{"trailing garbage", one + "garbage", false},
		{"trailing comma", one + ",", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPut, "/api/v1/targets/x/limit", strings.NewReader(tc.body))
			var in struct {
				PerCNode uint32 `json:"per_cnode"`
				Mode     string `json:"mode"`
			}
			got := readJSON(w, r, &in)
			if got != tc.ok {
				t.Errorf("readJSON(%q) = %v, want %v (decoded %+v)", tc.body, got, tc.ok, in)
			}
			if !tc.ok && w.Code != http.StatusBadRequest {
				t.Errorf("readJSON(%q): response status %d, want 400", tc.body, w.Code)
			}
		})
	}
}

// Through a mutating handler: the first value is acted on today.
func TestReviewM7_RuleWithTrailingValueIsRefused(t *testing.T) {
	s, _ := run(t, t.TempDir())
	if _, _, err := s.SetStaticTargets(t.Context(), []xds.TargetSpec{{Name: "fixed", ListenPort: 9002, Endpoints: []xds.Endpoint{{IP: "192.0.2.1", Port: 80}}}}); err != nil {
		t.Fatal(err)
	}
	body := `{"limit_per_second":5,"per":["total"]}{"blocked":true}`
	req, _ := http.NewRequest(http.MethodPut, "http://"+s.HTTPAddr()+"/api/v1/rules/r1", strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || hasRule(s.store.Rules(), "r1") {
		t.Errorf("PUT rule with a second JSON value after the first: HTTP %d, rule stored=%v; want 400 and nothing stored",
			resp.StatusCode, hasRule(s.store.Rules(), "r1"))
	}
}
