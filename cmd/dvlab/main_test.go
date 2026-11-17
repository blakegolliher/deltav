package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The probe end to end against a stand-in for the management API. The
// response fields here are invented for the test; they say nothing about
// what a real VAST cluster returns.
func TestVastProbeWritesSanitizedFixtures(t *testing.T) {
	var (
		mu   sync.Mutex
		seen []string
	)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/token/":
			var in struct{ Username, Password string }
			if json.NewDecoder(r.Body).Decode(&in) != nil || in.Username != "probe" || in.Password != "probe-password" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"access":"tok","refresh":"ref"}`))
		case "/api/cnodes/":
			_, _ = w.Write([]byte(`[{"id":1,"name":"cnode-1","mgmt_ip":"10.20.30.41","sn":"ABC123"}]`))
		case "/api/vippools/":
			_, _ = w.Write([]byte(`[{"id":2,"name":"main","tenant_name":"default"},{"id":3,"name":"pat-pool","tenant_name":"research"}]`))
		case "/api/vips/":
			_, _ = w.Write([]byte(`[{"id":7,"ip":"10.20.40.1","vippool":"pat-pool","cnode":"cnode-1"}]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "fixtures")
	t.Setenv("VAST_HOST", strings.TrimPrefix(srv.URL, "https://"))
	t.Setenv("VAST_USER", "probe")
	t.Setenv("VAST_PASSWORD", "probe-password")
	cmd := vastProbeCmd()
	cmd.SetArgs([]string{"--out", out, "--insecure"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	// Only the token call and GETs reached the server.
	want := "POST /api/token/,GET /api/cnodes/,GET /api/vippools/,GET /api/vips/"
	if got := strings.Join(seen, ","); got != want {
		t.Errorf("requests: %s\n    want: %s", got, want)
	}

	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatal(err)
		}
		if !json.Valid(b) {
			t.Fatalf("%s is not valid JSON:\n%s", name, b)
		}
		return string(b)
	}
	all := read("cnodes.json") + read("vippools.json") + read("vips.json")
	for _, leak := range []string{"10.20.", "ABC123", "pat-pool", "research", "probe-password", "tok"} {
		if strings.Contains(all, leak) {
			t.Errorf("fixtures contain %q:\n%s", leak, all)
		}
	}
	for _, keep := range []string{`"name": "cnode-1"`, `"name": "main"`, `"tenant_name": "default"`, `"id": 7`, `"ip": "192.0.2.2"`, `"mgmt_ip": "192.0.2.1"`} {
		if !strings.Contains(all, keep) {
			t.Errorf("fixtures lack %s:\n%s", keep, all)
		}
	}
}

func TestVastProbeNeedsCredentialsFromTheEnvironment(t *testing.T) {
	t.Setenv("VAST_HOST", "")
	t.Setenv("VAST_USER", "")
	t.Setenv("VAST_PASSWORD", "")
	cmd := vastProbeCmd()
	cmd.SetArgs([]string{"--out", t.TempDir(), "--host", "vast.example.com", "--user", "admin"})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "VAST_PASSWORD") {
		t.Fatalf("got %v", err)
	}
	if cmd.Flags().Lookup("password") != nil {
		t.Error("the probe must not take the password as a flag")
	}
}
