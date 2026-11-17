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

	"github.com/spf13/cobra"

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
	if row := strings.Join(strings.Fields(lines[1]), " "); row != "s3 127.0.0.1:9000 vast lab main no 6 - 6 of 6 VIPs on healthy CNodes" {
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

func TestFleetAndNodeList(t *testing.T) {
	api, mock := daemon(t)
	t.Setenv("DELTAV_PASSWORD", vasttest.Password)
	mustRun(t, "vast", "add", "lab", "--api", api, "--host", mock.Host(), "--user", vasttest.User, "--insecure")
	mustRun(t, "target", "add", "s3", "--api", api, "--cluster", "lab", "--pool", "main", "--port", "80", "--listen-port", "9000")

	// CNodes: three, ACTIVE, two VIPs of the target's pool each.
	got := mustRun(t, "node", "list", "--api", api)
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "CLUSTER") {
		t.Fatalf("node list:\n%s", got)
	}
	for i, name := range []string{"cnode-3-10", "cnode-3-11", "cnode-3-12"} {
		if row := strings.Join(strings.Fields(lines[i+1]), " "); row != "lab "+name+" ACTIVE 2 0 0.0 0.00 0.00 - - 0.0 0.0 -" {
			t.Errorf("node list row %d: %q", i, row)
		}
	}
	got = mustRun(t, "node", "list", "--api", api, "--vips", "--window", "1h")
	if n := strings.Count(got, ":80"); n != 6 || strings.Count(got, "target s3") != 6 {
		t.Errorf("node list --vips shows %d VIP rows, want 6:\n%s", n, got)
	}
	if err := json.Unmarshal([]byte(mustRun(t, "node", "list", "--api", api, "--json")), &struct{ CNodes []any }{}); err != nil {
		t.Errorf("node list --json: %v", err)
	}
	if _, err := run(t, "dv", "node", "list", "--api", api, "--window", "2d"); err == nil || !strings.Contains(err.Error(), "1m, 5m or 1h") {
		t.Errorf("bad window: %v", err)
	}

	// No Envoy has connected: an empty fleet, said plainly.
	got = mustRun(t, "fleet", "list", "--api", api)
	if !strings.HasPrefix(got, "NODE") || !strings.Contains(got, "Rows 0-0 of 0 matching; 0 Envoys, 0 connected") {
		t.Errorf("fleet list with no Envoys:\n%s", got)
	}
	if _, err := run(t, "dv", "fleet", "list", "--api", api, "--sort", "bogus"); err == nil || !strings.Contains(err.Error(), "want one of") {
		t.Errorf("bad sort key: %v", err)
	}
	var fl struct {
		Total   int
		Members []any
	}
	if err := json.Unmarshal([]byte(mustRun(t, "fleet", "list", "--api", api, "--json", "--state", "nacked", "--connected", "true", "--desc", "--filter", "x")), &fl); err != nil || fl.Members == nil {
		t.Errorf("fleet list --json: %v %+v", err, fl)
	}
}

// The TUI refuses to start without a terminal and says what to use instead.
func TestTUINeedsATerminal(t *testing.T) {
	_, err := run(t, "dv", "tui", "--api", "http://127.0.0.1:1")
	if err == nil || !strings.Contains(err.Error(), "needs a terminal") {
		t.Fatalf("got %v", err)
	}
}

func TestLimitAndDrainCommands(t *testing.T) {
	api, mock := daemon(t)
	t.Setenv("DELTAV_PASSWORD", vasttest.Password)
	mustRun(t, "vast", "add", "lab", "--api", api, "--host", mock.Host(), "--user", vasttest.User, "--insecure")
	mustRun(t, "target", "add", "s3", "--api", api, "--cluster", "lab", "--pool", "main", "--port", "80", "--listen-port", "9000")

	// Alert is the default and says that nothing is enforced.
	got := mustRun(t, "limit", "set", "s3", "--api", api, "--per-cnode", "100")
	if got != "s3: 100 connections per CNode, alert only: CNodes over it are reported, nothing is enforced\n" {
		t.Errorf("limit set (alert): %q", got)
	}
	// Enforce says what each Envoy gets. No Envoy is connected, which
	// counts as one: 100 / (1 x 2 VIPs per CNode) = 50.
	got = mustRun(t, "limit", "set", "s3", "--api", api, "--per-cnode", "100", "--mode", "enforce")
	if got != "s3: 100 connections per CNode, enforced: each of 1 Envoys may open 50 to each VIP (a CNode hosts up to 2 of this target's VIPs)\n" {
		t.Errorf("limit set (enforce): %q", got)
	}
	// A limit too small to divide: the share stops at 1 and the command says
	// what that allows.
	got = mustRun(t, "limit", "set", "s3", "--api", api, "--per-cnode", "1", "--mode", "enforce")
	if !strings.Contains(got, "may open 1 to each VIP") || !strings.Contains(got, "the share cannot go below 1, so the fleet can reach 2") {
		t.Errorf("limit set below the floor: %q", got)
	}
	got = mustRun(t, "target", "list", "--api", api)
	if row := strings.Join(strings.Fields(strings.Split(got, "\n")[1]), " "); !strings.HasPrefix(row, "s3 127.0.0.1:9000 vast lab main no 6 1 enforce (1 per Envoy per VIP) 6 of 6 VIPs") {
		t.Errorf("target list with a limit: %q", row)
	}
	if _, err := run(t, "dv", "limit", "set", "s3", "--api", api, "--per-cnode", "5", "--mode", "strict"); err == nil || !strings.Contains(err.Error(), "alert or enforce") {
		t.Errorf("bad mode: %v", err)
	}
	if _, err := run(t, "dv", "limit", "set", "s3", "--api", api); err == nil || !strings.Contains(err.Error(), "--per-cnode") {
		t.Errorf("no --per-cnode: %v", err)
	}
	if _, err := run(t, "dv", "limit", "set", "nope", "--api", api, "--per-cnode", "5"); err == nil || !strings.Contains(err.Error(), "no target") {
		t.Errorf("unknown target: %v", err)
	}
	if got = mustRun(t, "limit", "set", "s3", "--api", api, "--per-cnode", "0"); got != "s3: no limit\n" {
		t.Errorf("removing the limit: %q", got)
	}

	// Drain: with one cluster attached there is no need to name it.
	if got = mustRun(t, "node", "drain", "cnode-3-11", "--api", api); got != "drained cnode-3-11 on lab\n" {
		t.Errorf("node drain: %q", got)
	}
	got = mustRun(t, "node", "list", "--api", api)
	if !strings.Contains(got, "ACTIVE (drained)") || strings.Count(got, "(drained)") != 1 {
		t.Errorf("node list after a drain:\n%s", got)
	}
	got = mustRun(t, "target", "list", "--api", api)
	if !strings.Contains(got, "6 of 6 VIPs on healthy CNodes, 2 withheld by drain") || !strings.Contains(got, " 4 ") {
		t.Errorf("target list after a drain:\n%s", got)
	}
	if _, err := run(t, "dv", "node", "drain", "cnode-9-99", "--api", api); err == nil || !strings.Contains(err.Error(), "has no CNode") {
		t.Errorf("draining an unknown CNode: %v", err)
	}
	if got = mustRun(t, "node", "undrain", "cnode-3-11", "--api", api, "--cluster", "lab"); got != "undrained cnode-3-11 on lab\n" {
		t.Errorf("node undrain: %q", got)
	}
	if _, err := run(t, "dv", "node", "undrain", "cnode-3-11", "--api", api); err == nil || !strings.Contains(err.Error(), "is not drained") {
		t.Errorf("undraining twice: %v", err)
	}
	if mock.Mutating() != 0 {
		t.Errorf("%d requests other than GET and the token call reached the cluster", mock.Mutating())
	}
}

// Every command in the tree does something: none is a placeholder.
func TestNoPlaceholderCommands(t *testing.T) {
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if !c.HasSubCommands() && c.RunE == nil && c.Run == nil {
			t.Errorf("%q has no implementation", c.CommandPath())
		}
		if c.HasSubCommands() && c.Short == "" {
			t.Errorf("%q has no description", c.CommandPath())
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(NewRoot("deltav"))
}
