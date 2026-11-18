package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discoverygrpc "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"deltav/internal/apiclient"
	"deltav/internal/server"
	"deltav/internal/vast/vasttest"
)

func TestFormatting(t *testing.T) {
	for in, want := range map[float64]string{0: "0", 0.44: "0.4", 9.96: "10.0", 12.3: "12", 999: "999", 1234: "1.2k", 3.4e6: "3.4M"} {
		if got := rate(in); got != want {
			t.Errorf("rate(%v) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[float64]string{0: "0", 1000: "<0.1", 1.5e6: "1.5", 99.94e6: "99.9", 250e6: "250"} {
		if got := mbps(in); got != want {
			t.Errorf("mbps(%v) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[float64]string{0: "-", 0.84: "0.8ms", 9.96: "10.0ms", 292: "292ms", 1500: "1.5s"} {
		if got := millis(in); got != want {
			t.Errorf("millis(%v) = %q, want %q", in, got, want)
		}
	}
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	for d, want := range map[time.Duration]string{3 * time.Second: "3s", 90 * time.Second: "1m", 5 * time.Hour: "5h", 72 * time.Hour: "3d", -time.Second: "now"} {
		if got := age(now, now.Add(-d)); got != want {
			t.Errorf("age(%v) = %q, want %q", d, got, want)
		}
	}
	if age(now, time.Time{}) != "never" {
		t.Error("age of the zero time")
	}
}

func TestSparkline(t *testing.T) {
	if got := sparkline([]float64{0, 1, 2, 4, 8}, 5); got != "·▁▂▄█" {
		t.Errorf("sparkline = %q", got)
	}
	// No traffic is dots, not a low bar; a short history is right-aligned;
	// a long one keeps its newest points.
	if got := sparkline([]float64{0, 0, 0}, 5); got != "  ···" {
		t.Errorf("idle sparkline = %q", got)
	}
	if got := sparkline([]float64{9, 9, 0, 5, 5}, 2); got != "██" {
		t.Errorf("clipped sparkline = %q", got)
	}
	if sparkline(nil, 0) != "" {
		t.Error("zero-width sparkline")
	}
}

func TestTable(t *testing.T) {
	cols := []column{{"NAME", false}, {"REQ/S", true}, {"STATE", false}}
	rows := [][]string{{"alpha", "5", styleGood.Render("ok")}, {"b", "1234", styleBad.Render("failing")}}
	lines := table(cols, rows, 1, 80)
	plain := make([]string, len(lines))
	for i, l := range lines {
		plain[i] = ansi.Strip(l)
	}
	want := []string{
		"NAME   REQ/S  STATE  ",
		"alpha      5  ok     ",
		"b       1234  failing",
	}
	for i := range want {
		if plain[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, plain[i], want[i])
		}
	}
	if lines[1] == plain[1] || !strings.Contains(lines[1], "ok") {
		t.Error("an unselected row lost its styling")
	}
	// Narrow terminal: every line is cut to the width.
	for _, l := range table(cols, rows, -1, 10) {
		if w := ansi.StringWidth(l); w > 10 {
			t.Errorf("line %q is %d wide", ansi.Strip(l), w)
		}
	}
}

// sample is a model holding data in the API's shapes, without a daemon.
func sample() *Model {
	m := New(context.Background(), &Client{Base: "http://127.0.0.1:1"})
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	m.width, m.height = 160, 40
	m.live, m.lastEvent = true, now

	busy := Summary{Requests: 7200, RequestRate: 120, BytesInRate: 3.2e6, BytesOutRate: 10.1e6, Rate503: 0.4}
	busy.Status.S503, busy.LatencyMS.P50, busy.LatencyMS.P99 = 24, 2.1, 31
	history := make([]float64, 29)
	for i := range history {
		history[i] = float64(i)
	}
	m.overview = &Overview{
		ConfigVersion: "v7", Window: "1m",
		Fleet: FleetCounts{Total: 14, Connected: 12, Acked: 10, Nacked: 1, Pending: 1},
		Targets: []Target{
			{Name: "main", ListenPort: 9000, Endpoints: 6, InFlight: 4, Stats: busy, RateHistory: history},
			{Name: "archive", ListenPort: 9001, Endpoints: 0, RateHistory: make([]float64, 29)},
		},
	}
	vip := func(ep string, s Summary) VIPRow { return VIPRow{Target: "main", Endpoint: ep, InFlight: 2, Stats: s} }
	m.cnodes = &CNodes{Window: "1m", CNodes: []CNode{
		{Cluster: "lab", Name: "cnode-3-10", State: "ACTIVE", Enabled: true, Healthy: true, IsLeader: true, VIPs: 2, InFlight: 4,
			Stats: busy, RateHistory: history, Endpoints: []VIPRow{vip("192.0.2.28:80", busy), vip("192.0.2.31:80", Summary{})}},
		{Cluster: "lab", Name: "cnode-3-11", State: "INACTIVE", Enabled: true, Healthy: false, VIPs: 2, RateHistory: make([]float64, 29)},
		{Cluster: "lab", Name: "cnode-3-12", State: "ACTIVE", Enabled: true, Healthy: true, IsMgmt: true, VIPs: 2, RateHistory: make([]float64, 29)},
	}}
	m.fleet = &Fleet{ConfigVersion: "v7", Total: 14, Connected: 12, Matched: 3, Limit: 31, Members: []Member{
		{NodeID: "host-001", EnvoyVersion: "1.40.2", Address: "10.0.0.1:40000", Connected: true, LastSeen: now.Add(-3 * time.Second),
			ConfigVersion: "v7", ConfigState: "acked", InFlight: 2, Stats: busy},
		{NodeID: "host-002", EnvoyVersion: "1.40.2", Address: "10.0.0.2:40000", Connected: true, LastSeen: now.Add(-4 * time.Second),
			ConfigState: "nacked", ConfigError: "lds: cannot bind '127.0.0.1:9000': Address already in use"},
		{NodeID: "host-003", EnvoyVersion: "1.39.0", Address: "10.0.0.3:40000", Connected: false, LastSeen: now.Add(-2 * time.Hour), ConfigState: "acked", ConfigVersion: "v6"},
	}}
	last := now.Add(-5 * time.Second)
	m.clusters = []Cluster{{Name: "lab", Host: "vast.example.com", User: "admin", CNodes: 3, Pools: 48, VIPs: 187, LastPoll: &last}}
	m.targets = []TargetInfo{
		{Name: "main", ListenPort: 9000, Source: "vast", Cluster: "lab", Pool: "main", Endpoints: make([]apiclient.Endpoint, 4), Status: &TargetStatus{VIPs: 6, Healthy: 4}},
		{Name: "fixed", ListenPort: 9002, Source: "static", Endpoints: make([]apiclient.Endpoint, 3)},
	}
	m.loaded = true

	// A limit on main, in enforce mode, with cnode-3-10 over it; and
	// cnode-3-12 drained.
	over := Usage{Target: "main", Cluster: "lab", CNode: "cnode-3-10", InFlight: 130, PerCNode: 100, Mode: "enforce", Over: true}
	limit := &LimitStatus{PerCNode: 100, Mode: "enforce", Share: 5, Computed: 4, Envoys: 12, MaxVIPsPerCNode: 2, Workers: 1, Holds: true}
	m.overview.Targets[0].Limit, m.overview.Targets[0].Over = limit, []Usage{over}
	m.cnodes.CNodes[0].Limits = []Usage{over}
	m.cnodes.CNodes[1].Limits = []Usage{{Target: "main", Cluster: "lab", CNode: "cnode-3-11", InFlight: 0, PerCNode: 100}}
	m.cnodes.CNodes[2].Drained = true
	m.targets[0].Status.Limit, m.targets[0].Status.Drained, m.targets[0].Over = limit, 2, []Usage{over}
	m.control = m.cnodes.CNodes
	return m
}

func press(m *Model, keys ...string) {
	for _, k := range keys {
		msg := tea.KeyPressMsg{Code: rune(k[0]), Text: k}
		switch k {
		case "enter":
			msg = tea.KeyPressMsg{Code: tea.KeyEnter}
		case "esc":
			msg = tea.KeyPressMsg{Code: tea.KeyEscape}
		case "tab":
			msg = tea.KeyPressMsg{Code: tea.KeyTab}
		case "down":
			msg = tea.KeyPressMsg{Code: tea.KeyDown}
		case "up":
			msg = tea.KeyPressMsg{Code: tea.KeyUp}
		case "pgdown":
			msg = tea.KeyPressMsg{Code: tea.KeyPgDown}
		case "backspace":
			msg = tea.KeyPressMsg{Code: tea.KeyBackspace}
		}
		m.Update(msg) // the fetch it may return is not run: there is no daemon
	}
}

func screen(m *Model) string { return ansi.Strip(m.View().Content) }

// flat is a screen as running text: its lines joined, runs of space made
// one. Sentences wrap where the terminal's width puts the break.
func flat(s string) string { return strings.Join(strings.Fields(s), " ") }

func expect(t *testing.T, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) && !strings.Contains(flat(got), w) {
			t.Errorf("the screen lacks %q:\n%s", w, got)
		}
	}
}

// fits fails if a line of the screen is wider than the terminal, or was cut
// short to make it fit.
func fits(t *testing.T, m *Model, what string) string {
	t.Helper()
	s := screen(m)
	if n := strings.Count(s, "\n") + 1; n != m.height {
		t.Errorf("%s: the screen is %d lines tall, the terminal %d", what, n, m.height)
	}
	for _, l := range strings.Split(s, "\n") {
		if w := ansi.StringWidth(l); w > m.width {
			t.Errorf("%s: line %q is %d wide in a %d-column terminal", what, l, w, m.width)
		}
		if strings.HasSuffix(l, "…") {
			t.Errorf("%s: a line was cut off at the edge: %q", what, l)
		}
	}
	return s
}

// A terminal of 80 columns by 24 rows, the size many open at. Tables leave
// out whole columns, the least needed first, and say which; they never cut
// a row at the edge, because the right-hand end is where limits and
// failures are. Text and key hints wrap.
func TestNarrowTerminal(t *testing.T) {
	m := sample()
	m.width, m.height = 80, 24

	s := fits(t, m, "overview")
	expect(t, s, "TARGET", "IN FLIGHT", "REQ/S", "P99", "403/S", "503/S", "LIMIT", "100 enforce OVER", "31ms",
		"Not shown at this width: LAST 5 MIN, MB/S OUT, MB/S IN, P50, LISTEN", "1-4 view", "q quit")
	for _, gone := range []string{"MB/S OUT ", "127.0.0.1:9000", "▁"} {
		if strings.Contains(strings.Split(s, "Not shown")[0], gone) {
			t.Errorf("overview at 80 columns still shows %q", gone)
		}
	}

	press(m, "2", "enter")
	s = fits(t, m, "cnodes")
	expect(t, s, "CNODE", "STATE", "IN FLIGHT", "P99", "503/S", "OF LIMIT", "130/100 OVER", "ACTIVE drained", "├ 192.0.2.28:80",
		"Idle keep-alive connections are not counted.", "Not shown at this width: LAST 5 MIN, MB/S OUT, MB/S IN, P50", "enter show or hide VIPs", "q quit")

	press(m, "3")
	s = fits(t, m, "fleet")
	expect(t, s, "NODE", "WORKERS", "CONFIG", "LAST SEEN", "REQ/S", "IN FLIGHT", "host-002", "rejected",
		"/ filter", "c connected", "pgup/pgdn page", "r refresh", "q quit")

	press(m, "4")
	s = fits(t, m, "controls")
	expect(t, s, "VAST clusters", "vast.example.com", "Targets", "100 enforce OVER: 5 per Envoy per VIP (4 held back)",
		"CNodes", "130/100 OVER", "e set limit", "d drain/undrain", "q quit")

	// A question must show how to answer it, however narrow the terminal.
	press(m, "down", "down", "d")
	if m.confirm == nil {
		t.Fatal("d on a CNode did not ask for confirmation")
	}
	for _, width := range []int{120, 80, 60, 40} {
		m.width = width
		s = screen(m)
		expect(t, s, "Drain cnode-3-10?", "requests in flight finish.", "y drain", "any other key cancel")
		lines := strings.Split(s, "\n")
		if len(lines) != m.height {
			t.Errorf("drain question at %d columns: the screen is %d lines tall, the terminal %d", width, len(lines), m.height)
		}
		// The question is the end of the screen; none of its lines is cut.
		for _, l := range lines[len(lines)-4:] {
			if ansi.StringWidth(l) > width || strings.HasSuffix(l, "…") {
				t.Errorf("drain question at %d columns: %q does not fit", width, l)
			}
		}
	}
	press(m, "n")
	m.width = 60
	press(m, "up", "up", "e")
	expect(t, screen(m), "Connections per CNode for main, 0 for no limit: 100█", "enter apply", "esc cancel")
	press(m, "esc")
	m.width = 80

	// The state of the connection is not what falls off a narrow header.
	press(m, "1")
	m.width, m.live = 60, false
	m.lastEvent = m.now().Add(-12 * time.Second)
	expect(t, fits(t, m, "header at 60 columns"), "4 Controls", "window 1m", "○ disconnected 12s ago")
}

// On a short terminal the notes on limits do not fit under the Controls
// tables; "?" gives them the screen.
func TestAboutLimits(t *testing.T) {
	m := sample()
	m.width, m.height = 80, 24
	press(m, "?")
	if m.about {
		t.Error("? did something outside the Controls view")
	}
	press(m, "4")
	expect(t, screen(m), "? about limits")
	press(m, "?")
	s := fits(t, m, "about limits")
	expect(t, s, "About limits and drain", "requests in flight", "alert reports CNodes over it and enforces nothing.",
		"limit ÷ (connected Envoys × most VIPs on one CNode), less one for each worker thread",
		"never below 1", "makes requests wait", "makes Envoys reconnect", "Nothing changes on the VAST cluster.", "any key to go back")
	if strings.Contains(s, "VAST clusters") {
		t.Error("the notes should have the screen to themselves")
	}
	press(m, "e") // any key goes back, and is not then taken as a command
	if m.about || m.editing != nil {
		t.Errorf("after a key: about %v, editing %v", m.about, m.editing)
	}
	expect(t, screen(m), "VAST clusters", "Targets")
}

// What an enforced limit cannot do is said in full on the Controls view.
func TestControlsCaveats(t *testing.T) {
	m := sample()
	m.height = 50
	l := m.targets[0].Status.Limit
	l.PerCNode, l.Share, l.Computed, l.Envoys, l.Workers, l.Holds, l.Undeclared = 9, 1, 1, 3, 2, false, 2
	m.fleet.Members[0].Workers = 2
	press(m, "4")
	expect(t, screen(m), "1 per Envoy per VIP, cannot hold",
		"main cannot hold its limit: 3 Envoys × 2 VIPs on one CNode × 2 worker threads is more than 9, and no Envoy's cap can be below 1.",
		"Raise the limit, or run the Envoys with fewer worker threads.",
		"main: 2 connected Envoys do not say how many worker threads they run (see the Fleet view)")
	// The Fleet view shows which: a count for those that say, "?" for a
	// connected Envoy that does not, nothing for one that is gone.
	press(m, "3")
	rows := map[string]string{}
	for _, line := range strings.Split(screen(m), "\n") {
		if f := strings.Fields(line); len(f) > 2 && strings.HasPrefix(f[0], "host-") {
			rows[f[0]] = f[2]
		}
	}
	if rows["host-001"] != "2" || rows["host-002"] != "?" || rows["host-003"] != "disconnected" {
		t.Errorf("WORKERS column: %v (host-003 is disconnected, so its third field is its CONFIG)", rows)
	}
}

func TestOverviewView(t *testing.T) {
	m := sample()
	s := screen(m)
	expect(t, s,
		"Delta-V", "1 Overview", "2 CNodes", "3 Fleet", "4 Controls", "window 1m", "● live",
		"Fleet  12 Envoys connected (10 in sync, 1 rejected the config, 1 pending), 2 disconnected",
		"TARGET", "LISTEN", "IN FLIGHT", "LAST 5 MIN",
		"main", "127.0.0.1:9000", "120", "3.2", "10.1", "2.1ms", "31ms", "0.4", "▁", "█",
		"LIMIT", "100 enforce OVER", "Over the limit: main on cnode-3-10 has 130 in flight, limit 100",
		"archive", "127.0.0.1:9001", "·····",
		"q quit")
	if v := m.View(); !v.AltScreen {
		t.Error("the TUI must use the alternate screen, so it leaves the terminal as it found it")
	}
	if n := strings.Count(s, "\n") + 1; n != 40 {
		t.Errorf("the screen is %d lines tall, the terminal 40", n)
	}
	// A narrow terminal: nothing wraps.
	m.width = 50
	for _, l := range strings.Split(m.View().Content, "\n") {
		if w := ansi.StringWidth(l); w > 50 {
			t.Errorf("line %q is %d wide in a 50-column terminal", ansi.Strip(l), w)
		}
	}
	// Before the daemon has answered.
	empty := New(context.Background(), &Client{})
	expect(t, screen(empty), "Waiting for the daemon", "○ connecting")
}

func TestCNodesView(t *testing.T) {
	m := sample()
	press(m, "2")
	s := screen(m)
	expect(t, s, "CNODE", "STATE", "VIPS", "cnode-3-10 leader", "cnode-3-11", "INACTIVE", "cnode-3-12 mgmt", "Idle keep-alive connections are not counted",
		"OF LIMIT", "130/100 OVER", "0/100", "ACTIVE drained")
	if strings.Contains(s, "192.0.2.28:80") {
		t.Error("VIP rows are shown before the CNode is expanded")
	}
	press(m, "enter")
	s = screen(m)
	expect(t, s, "├ 192.0.2.28:80", "└ 192.0.2.31:80")
	press(m, "enter")
	if strings.Contains(screen(m), "192.0.2.28:80") {
		t.Error("the VIP rows did not collapse")
	}
	// The cursor stops at the last CNode.
	press(m, "down", "down", "down", "down", "enter")
	expect(t, screen(m), "hosts no VIP of a configured target")
	if m.cursor[tabCNodes] != 2 {
		t.Errorf("cursor = %d", m.cursor[tabCNodes])
	}
}

func TestFleetView(t *testing.T) {
	m := sample()
	press(m, "3")
	s := screen(m)
	expect(t, s, "Sort node_id ↑", "Filter none", "State any", "Showing all",
		"NODE", "ENVOY", "CONFIG", "LAST SEEN",
		"host-001", "1.40.2", "in sync", "3s", "host-002", "rejected", "host-003", "disconnected", "2h",
		"Rows 1-3 of 3 matching · 14 Envoys, 12 connected")
	if strings.Contains(s, "cannot bind") {
		t.Error("a rejection reason is shown for a row that is not selected")
	}
	press(m, "down")
	expect(t, screen(m), "host-002 rejected the config: lds: cannot bind '127.0.0.1:9000': Address already in use")

	// Sorting, ordering and the two filters change the query the daemon is
	// sent, and go back to the first page.
	m.query.Offset = 60
	press(m, "s")
	if m.query.Sort != "request_rate" || !m.query.Desc || m.query.Offset != 0 {
		t.Errorf("after s: %+v", m.query)
	}
	press(m, "o", "a", "c")
	if m.query.Desc || m.query.State != "nacked" || m.query.Connected != "true" {
		t.Errorf("after o, a, c: %+v", m.query)
	}
	expect(t, screen(m), "Sort request_rate ↑", "State nacked", "Showing connected")

	// Typing a filter: keys go to the filter, not to the commands.
	press(m, "/", "q", "1", "x", "backspace")
	if !m.filtering || m.filterDraft != "q1" || m.tab != tabFleet {
		t.Fatalf("while typing: filtering %v, draft %q, tab %d", m.filtering, m.filterDraft, m.tab)
	}
	expect(t, screen(m), "Filter q1█", "enter apply")
	press(m, "enter")
	if m.filtering || m.query.Filter != "q1" {
		t.Errorf("after enter: %+v", m.query)
	}
	press(m, "/", "z", "esc")
	if m.query.Filter != "q1" {
		t.Errorf("esc changed the filter: %q", m.query.Filter)
	}
	press(m, "x")
	if m.query.Filter != "" {
		t.Errorf("x did not clear the filter: %q", m.query.Filter)
	}

	// Paging stops at the last page.
	m.fleet.Matched = 100
	size := m.pageSize()
	press(m, "pgdown")
	if m.query.Offset != size {
		t.Errorf("offset after one page = %d, want %d", m.query.Offset, size)
	}
	m.query.Offset = 100 - size
	press(m, "pgdown")
	if m.query.Offset != 100-size {
		t.Errorf("paged past the end: offset %d", m.query.Offset)
	}
}

func TestControlsView(t *testing.T) {
	m := sample()
	m.height = 50
	press(m, "4")
	expect(t, screen(m), "VAST clusters", "lab", "vast.example.com", "187", "5s ago", "ok",
		"Targets", "main", "lab / main", "100 enforce OVER: 5 per Envoy per VIP (4 held back)", "4 of 6 VIPs on healthy CNodes, 2 withheld by drain",
		"fixed", "static", "fixed endpoint list",
		"CNodes", "lab / cnode-3-10", "130/100 OVER", "lab / cnode-3-12", "ACTIVE drained",
		// The two things to know about enforce, next to the control.
		"limit ÷ (connected Envoys × most VIPs on", "never below 1", "makes Envoys reconnect",
		"e set limit", "m alert/enforce", "d drain/undrain")
	m.clusters[0].LastError = "status 500"
	m.targets[0].Status = &TargetStatus{Problem: `cluster "lab" has no VIP pool "main"`, Held: true}
	expect(t, screen(m), "failing: status 500", `serving the last known endpoints: cluster "lab" has no VIP pool "main"`)
}

func TestControlsActions(t *testing.T) {
	m := sample()
	m.height = 50
	press(m, "4")

	// e on a target: a prompt, holding the current limit, that takes digits
	// only.
	press(m, "e")
	if m.editing == nil || m.editing.target != "main" || m.editing.draft != "100" || m.editing.mode != "enforce" {
		t.Fatalf("after e: %+v", m.editing)
	}
	expect(t, screen(m), "Connections per CNode for main, 0 for no limit: 100█", "enter apply")
	press(m, "backspace", "backspace", "5", "x", "q", "2")
	if m.editing.draft != "152" || m.tab != tabControls {
		t.Errorf("while typing: draft %q, view %d", m.editing.draft, m.tab)
	}
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.editing != nil || cmd == nil {
		t.Fatalf("after enter: editing %+v, command %v", m.editing, cmd != nil)
	}
	// esc leaves things alone.
	press(m, "e", "9", "esc")
	if m.editing != nil {
		t.Error("esc did not close the prompt")
	}
	// m flips the mode of a target that has a limit.
	if _, cmd = m.Update(tea.KeyPressMsg{Code: 'm', Text: "m"}); cmd == nil {
		t.Error("m on a target with a limit did nothing")
	}
	// A static target has no CNodes to limit.
	press(m, "down", "e")
	if m.editing != nil || !strings.Contains(m.errText, "static target") {
		t.Errorf("e on a static target: editing %+v, error %q", m.editing, m.errText)
	}
	press(m, "d")
	if m.confirm != nil || !strings.Contains(m.errText, "move to a CNode") {
		t.Errorf("d on a target: confirm %+v, error %q", m.confirm, m.errText)
	}

	// d on a CNode asks first, and only y goes ahead.
	press(m, "down", "d")
	if m.confirm == nil || m.confirm.cnode != "cnode-3-10" || m.confirm.cluster != "lab" || !m.confirm.drain {
		t.Fatalf("d on cnode-3-10: %+v", m.confirm)
	}
	expect(t, screen(m), "Drain cnode-3-10? Its VIPs leave every Envoy at once; requests in flight finish.", "y drain")
	if _, cmd = m.Update(tea.KeyPressMsg{Code: 'n', Text: "n"}); cmd != nil || m.confirm != nil {
		t.Error("n did not cancel the drain")
	}
	press(m, "d")
	if _, cmd = m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"}); cmd == nil || m.confirm != nil {
		t.Error("y did not go ahead with the drain")
	}
	// On a drained CNode the question is the other way round.
	press(m, "down", "down", "d")
	if m.confirm == nil || m.confirm.cnode != "cnode-3-12" || m.confirm.drain {
		t.Fatalf("d on the drained cnode-3-12: %+v", m.confirm)
	}
	expect(t, screen(m), "Undrain cnode-3-12?")
	press(m, "esc")

	// What a change did is shown; a failure replaces it.
	m.Update(actionMsg{done: "main: limit 152 connections per CNode, enforce mode"})
	expect(t, screen(m), "✓ main: limit 152 connections per CNode, enforce mode")
	m.Update(actionMsg{err: fmt.Errorf(`no target "main"`)})
	s := screen(m)
	expect(t, s, `! no target "main"`)
	if strings.Contains(s, "✓") {
		t.Error("a success notice stayed after a failure")
	}
}

func TestNavigationAndStatus(t *testing.T) {
	m := sample()
	press(m, "tab", "tab")
	if m.tab != tabFleet {
		t.Errorf("tab twice: view %d", m.tab)
	}
	press(m, "tab", "tab")
	if m.tab != tabOverview {
		t.Errorf("tab wraps: view %d", m.tab)
	}
	press(m, "w")
	expect(t, screen(m), "window 5m")
	press(m, "w", "w")
	expect(t, screen(m), "window 1m")

	// An event in another window must not replace that window's numbers,
	// but its fleet counts are current.
	press(m, "w")
	m.Update(eventMsg{Overview: &Overview{Fleet: FleetCounts{Total: 20, Connected: 20, Acked: 20}, Targets: []Target{{Name: "from-the-event"}}}})
	s := screen(m)
	expect(t, s, "20 Envoys connected (20 in sync)", "main")
	if strings.Contains(s, "from-the-event") {
		t.Error("a one-minute event replaced the five-minute overview")
	}

	// A broken stream shows, with the reason, and the data stays.
	m.Update(eventMsg{Err: fmt.Errorf("cannot reach the daemon at http://127.0.0.1:18080: connection refused")})
	s = screen(m)
	expect(t, s, "○ disconnected", "! cannot reach the daemon", "main")
	m.Update(overviewMsg{v: m.overview})
	if strings.Contains(screen(m), "! cannot reach") {
		t.Error("the error stayed after a successful read")
	}

	// q quits.
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	if cmd == nil {
		t.Fatal("q returned no command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("q did not quit")
	}
}

// daemon runs a real daemon with a mock VAST behind it and three Envoys
// (ADS streams) connected, so the client is tested against the real API.
func daemon(t *testing.T) *Client {
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
	base := "http://" + s.HTTPAddr()
	post := func(path string, body map[string]any) {
		b, _ := json.Marshal(body)
		resp, err := http.Post(base+path, "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			msg, _ := io.ReadAll(resp.Body)
			t.Fatalf("POST %s: %d %s", path, resp.StatusCode, msg)
		}
	}
	post("/api/v1/vast", map[string]any{"name": "lab", "host": mock.Host(), "user": vasttest.User, "password": vasttest.Password, "insecure": true})
	post("/api/v1/targets", map[string]any{"name": "main", "cluster": "lab", "pool": "main", "port": 80, "listen_port": 9000})

	conn, err := grpc.NewClient(s.GRPCAddr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	for i := 1; i <= 3; i++ {
		stream, err := discoverygrpc.NewAggregatedDiscoveryServiceClient(conn).StreamAggregatedResources(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := stream.Send(&discoverygrpc.DiscoveryRequest{TypeUrl: resource.ClusterType, Node: &corev3.Node{Id: fmt.Sprintf("host-%d", i)}}); err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Recv(); err != nil {
			t.Fatal(err)
		}
	}
	return &Client{Base: base}
}

// run executes a command and feeds what it produces back into the model,
// as the Bubble Tea runtime would, until nothing is left to run.
func run(t *testing.T, m *Model, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		return
	}
	switch msg := cmd().(type) {
	case nil:
	case tea.BatchMsg:
		for _, c := range msg {
			run(t, m, c)
		}
	default:
		_, next := m.Update(msg)
		run(t, m, next)
	}
}

func TestAgainstADaemon(t *testing.T) {
	api := daemon(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The event stream: an overview at once, and again each interval.
	events := api.Subscribe(ctx, 100*time.Millisecond)
	for i := 0; i < 2; i++ {
		select {
		case ev := <-events:
			if ev.Err != nil || ev.Overview == nil || len(ev.Overview.Targets) != 1 || ev.Overview.Targets[0].Name != "main" ||
				ev.Overview.Targets[0].Endpoints != 6 || len(ev.Overview.Targets[0].RateHistory) != 29 || ev.Overview.Fleet.Connected != 3 {
				t.Fatalf("event %d: %+v, err %v", i, ev.Overview, ev.Err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no event %d", i)
		}
	}

	// Each view, filled from the real API.
	m := New(ctx, api)
	m.width, m.height = 170, 40
	m.Update(eventMsg{Overview: func() *Overview { o, _ := api.Overview(ctx, "1m"); return o }()})
	s := screen(m)
	expect(t, s, "● live", "3 Envoys connected", "main", "127.0.0.1:9000", "·····")

	for _, step := range []struct {
		key   string
		wants []string
	}{
		{"2", []string{"cnode-3-10", "cnode-3-11", "cnode-3-12", "ACTIVE", "leader", "mgmt"}},
		{"3", []string{"host-1", "host-2", "host-3", "Rows 1-3 of 3 matching · 3 Envoys, 3 connected"}},
		{"4", []string{"VAST clusters", "lab", "187", "Targets", "lab / main", "6 of 6 VIPs on healthy CNodes"}},
	} {
		_, cmd := m.Update(tea.KeyPressMsg{Code: rune(step.key[0]), Text: step.key})
		run(t, m, cmd)
		if m.errText != "" {
			t.Errorf("view %s: %s", step.key, m.errText)
		}
		expect(t, screen(m), step.wants...)
	}

	// VIP rows of the first CNode: two VIPs of pool main.
	_, cmd := m.Update(tea.KeyPressMsg{Code: '2', Text: "2"})
	run(t, m, cmd)
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if s := screen(m); strings.Count(s, ":80") != 2 || !strings.Contains(s, "└ ") {
		t.Errorf("expanded CNode:\n%s", s)
	}

	// Changes from the Controls view go through the same API.
	_, cmd = m.Update(tea.KeyPressMsg{Code: '4', Text: "4"})
	run(t, m, cmd)
	m.height = 50
	press(m, "e", "5")
	_, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	run(t, m, cmd)
	expect(t, screen(m), "✓ main: limit 5 connections per CNode, alert mode", "5 alert")
	_, cmd = m.Update(tea.KeyPressMsg{Code: 'm', Text: "m"})
	run(t, m, cmd)
	// Three Envoys, two VIPs per CNode: 5 / 6 rounds down to the floor of 1.
	expect(t, screen(m), "✓ main: limit 5 is now in enforce mode", "5 enforce: 1 per Envoy per VIP")
	press(m, "down", "down", "d") // the second CNode
	_, cmd = m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	run(t, m, cmd)
	expect(t, screen(m), "✓ drained cnode-3-11: its VIPs have left every Envoy", "ACTIVE drained", "6 of 6 VIPs on healthy CNodes, 2 withheld by drain")
	press(m, "d")
	_, cmd = m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	run(t, m, cmd)
	if s := screen(m); strings.Contains(s, "ACTIVE drained") || !strings.Contains(s, "✓ undrained cnode-3-11") {
		t.Errorf("after undraining:\n%s", s)
	}
	m.height = 40

	// A filter is applied by the daemon.
	_, cmd = m.Update(tea.KeyPressMsg{Code: '3', Text: "3"})
	run(t, m, cmd)
	press(m, "/", "h", "o", "s", "t", "-", "2")
	_, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	run(t, m, cmd)
	s = screen(m)
	expect(t, s, "host-2", "Rows 1-1 of 1 matching · 3 Envoys, 3 connected")
	if strings.Contains(s, "host-1") || strings.Contains(s, "host-3") {
		t.Errorf("the filter did not narrow the fleet:\n%s", s)
	}

	// A daemon that is gone is reported, not hung on.
	gone := &Client{Base: "http://127.0.0.1:1"}
	if _, err := gone.Overview(ctx, "1m"); err == nil || !strings.Contains(err.Error(), "cannot reach the daemon") {
		t.Errorf("unreachable daemon: %v", err)
	}
	select {
	case ev := <-gone.Subscribe(ctx, time.Second):
		if ev.Err == nil {
			t.Error("a stream to nowhere reported no error")
		}
	case <-time.After(5 * time.Second):
		t.Error("a stream to nowhere reported nothing")
	}
}

// The whole program, run without a terminal: it draws, follows the stream
// and quits on q.
func TestProgramRuns(t *testing.T) {
	api := daemon(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var out bytes.Buffer
	in, feed := io.Pipe()
	defer feed.Close()
	p := tea.NewProgram(New(ctx, api), tea.WithContext(ctx), tea.WithInput(in), tea.WithOutput(&out),
		tea.WithWindowSize(150, 40), tea.WithoutSignalHandler())
	done := make(chan error, 1)
	go func() { _, err := p.Run(); done <- err }()
	time.Sleep(1500 * time.Millisecond) // let it draw and take an event
	p.Send(tea.KeyPressMsg{Code: 'q', Text: "q"})
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the program ended with: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the program did not quit on q")
	}
	expect(t, ansi.Strip(out.String()), "Delta-V", "Overview", "main", "127.0.0.1:9000", "3 Envoys connected")
}
