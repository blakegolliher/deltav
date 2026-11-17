package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"deltav/internal/server"
	"deltav/internal/vast/vasttest"
)

// daemon runs a real daemon for the client commands to talk to, and a mock
// VAST (the real cluster's sanitized responses) for it to read.
func daemon(t *testing.T) (api string, mock *vasttest.Server) {
	t.Helper()
	mock, err := vasttest.New("../../testdata/vast/v5.5.3")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	s, err := server.New(server.Config{
		GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0", StateDir: t.TempDir(), VASTPollInterval: time.Hour,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("the daemon did not stop")
		}
	})
	return "http://" + s.HTTPAddr(), mock
}

func mustRun(t *testing.T, args ...string) string {
	t.Helper()
	got, err := run(t, "dv", args...)
	if err != nil {
		t.Fatalf("dv %s: %v\n%s", strings.Join(args, " "), err, got)
	}
	return got
}

func TestVastAndTargetCommands(t *testing.T) {
	api, mock := daemon(t)
	t.Setenv("DELTAV_PASSWORD", vasttest.Password)

	got := mustRun(t, "vast", "add", "lab", "--api", api, "--host", mock.Host(), "--user", vasttest.User, "--insecure")
	if got != "attached lab: 3 CNodes, 48 VIP pools, 187 VIPs\n" {
		t.Errorf("vast add: %q", got)
	}
	got = mustRun(t, "vast", "list", "--api", api)
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "NAME") || !strings.HasPrefix(lines[1], "lab ") ||
		!strings.Contains(lines[1], mock.Host()) || !strings.Contains(lines[1], " 187 ") || !strings.HasSuffix(lines[1], "ok") {
		t.Errorf("vast list:\n%s", got)
	}
	if strings.Contains(got, vasttest.Password) {
		t.Error("vast list prints the password")
	}

	got = mustRun(t, "vast", "pools", "lab", "--api", api)
	var mainLine string
	for _, l := range strings.Split(got, "\n") {
		if strings.HasPrefix(l, "main ") {
			mainLine = strings.Join(strings.Fields(l), " ")
		}
	}
	if mainLine != "main default PROTOCOLS 6 cnode-3-10:2 cnode-3-11:2 cnode-3-12:2" {
		t.Errorf("vast pools, line for main: %q", mainLine)
	}
	if n := strings.Count(got, "\n"); n != 49 {
		t.Errorf("vast pools printed %d lines, want a header and 48 pools", n)
	}

	// A pool that does not exist is refused, with the daemon's reason.
	if _, err := run(t, "dv", "target", "add", "x", "--api", api, "--cluster", "lab", "--pool", "nope", "--port", "80", "--listen-port", "9001"); err == nil ||
		!strings.Contains(err.Error(), `has no VIP pool "nope"`) {
		t.Errorf("target add with an unknown pool: %v", err)
	}
	got = mustRun(t, "target", "add", "s3", "--api", api, "--cluster", "lab", "--pool", "main", "--port", "80", "--listen-port", "9000")
	if got != "added s3: 127.0.0.1:9000 on every client, 6 endpoints (6 of 6 VIPs on healthy CNodes)\n" {
		t.Errorf("target add: %q", got)
	}
	got = mustRun(t, "target", "add", "s3tls", "--api", api, "--cluster", "lab", "--pool", "main", "--port", "443", "--listen-port", "9443",
		"--tls", "--tls-server-name", "s3.example.com")
	if !strings.Contains(got, "added s3tls: 127.0.0.1:9443") {
		t.Errorf("target add --tls: %q", got)
	}
	got = mustRun(t, "target", "list", "--api", api)
	lines = strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) != 3 {
		t.Fatalf("target list:\n%s", got)
	}
	if row := strings.Join(strings.Fields(lines[1]), " "); row != "s3 127.0.0.1:9000 vast lab main no 6 6 of 6 VIPs on healthy CNodes" {
		t.Errorf("target list row: %q", row)
	}
	if row := strings.Join(strings.Fields(lines[2]), " "); !strings.HasPrefix(row, "s3tls 127.0.0.1:9443 vast lab main yes 6") {
		t.Errorf("target list TLS row: %q", row)
	}
	var js struct {
		Targets []struct {
			Name      string
			Endpoints []struct{ IP, CNode string }
		}
	}
	if err := json.Unmarshal([]byte(mustRun(t, "target", "list", "--api", api, "--json")), &js); err != nil ||
		len(js.Targets) != 2 || len(js.Targets[0].Endpoints) != 6 || js.Targets[0].Endpoints[0].CNode == "" {
		t.Errorf("target list --json: %v %+v", err, js)
	}

	if _, err := run(t, "dv", "vast", "remove", "lab", "--api", api); err == nil || !strings.Contains(err.Error(), "used by target") {
		t.Errorf("removing a cluster in use: %v", err)
	}
	for _, name := range []string{"s3", "s3tls"} {
		if got := mustRun(t, "target", "remove", name, "--api", api); got != "removed "+name+"\n" {
			t.Errorf("target remove: %q", got)
		}
	}
	if got := mustRun(t, "vast", "remove", "lab", "--api", api); got != "detached lab\n" {
		t.Errorf("vast remove: %q", got)
	}
	if got := mustRun(t, "vast", "list", "--api", api); strings.Count(got, "\n") != 1 {
		t.Errorf("vast list after removal:\n%s", got)
	}
	if mock.Mutating() != 0 {
		t.Errorf("%d requests other than GET and the token call reached the cluster", mock.Mutating())
	}
}

func TestVastAddPasswordSources(t *testing.T) {
	api, mock := daemon(t)
	base := []string{"vast", "add", "lab", "--api", api, "--host", mock.Host(), "--user", vasttest.User, "--insecure"}

	t.Setenv("DELTAV_PASSWORD", "")
	if _, err := run(t, "dv", base...); err == nil || !strings.Contains(err.Error(), "DELTAV_PASSWORD") {
		t.Errorf("no password: %v", err)
	}
	// From standard input, with the newline a shell adds.
	root := NewRoot("dv")
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetIn(strings.NewReader(vasttest.Password + "\n"))
	root.SetArgs(append(base, "--password-stdin"))
	if err := root.Execute(); err != nil || !strings.Contains(buf.String(), "attached lab") {
		t.Fatalf("--password-stdin: %v %q", err, buf.String())
	}
	// A wrong password is the daemon's answer, not a stored cluster.
	t.Setenv("DELTAV_PASSWORD", "wrong")
	if _, err := run(t, "dv", "vast", "add", "other", "--api", api, "--host", mock.Host(), "--user", vasttest.User, "--insecure"); err == nil ||
		!strings.Contains(err.Error(), "login") || strings.Contains(err.Error(), "wrong") {
		t.Errorf("wrong password: %v", err)
	}
	if _, err := run(t, "dv", "vast", "add", "x", "--api", api, "--user", "u"); err == nil || !strings.Contains(err.Error(), "--host") {
		t.Errorf("no host: %v", err)
	}
}

func TestClientCommandsExplainAnUnreachableDaemon(t *testing.T) {
	_, err := run(t, "dv", "vast", "list", "--api", "http://127.0.0.1:1")
	if err == nil || !strings.Contains(err.Error(), "cannot reach the daemon") || !strings.Contains(err.Error(), `"dv serve"`) {
		t.Fatalf("got %v", err)
	}
	// The API address and token can come from the environment.
	t.Setenv("DELTAV_API", "http://127.0.0.1:1")
	if _, err := run(t, "dv", "target", "list"); err == nil || !strings.Contains(err.Error(), "http://127.0.0.1:1") {
		t.Fatalf("DELTAV_API was not used: %v", err)
	}
}
