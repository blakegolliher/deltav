package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// reviewT5FleetPage builds the response the daemon would send for one page.
func reviewT5FleetPage(offset, limit, matched int) *Fleet {
	f := &Fleet{ConfigVersion: "v1", Window: "1m", Total: matched, Connected: matched, Matched: matched, Offset: offset, Limit: limit}
	for i := offset; i < min(offset+limit, matched); i++ {
		f.Members = append(f.Members, Member{NodeID: fmt.Sprintf("node-%03d", i), ConfigVersion: "v1", ConfigState: "acked", Connected: true})
	}
	return f
}

// TestReviewT5_OutOfOrderPages: page 1 is requested, then page 2; page 2's
// response arrives first and page 1's (slow) response arrives after it. The
// view must keep showing page 2, the page the operator asked for last.
func TestReviewT5_OutOfOrderPages(t *testing.T) {
	m := New(context.Background(), &Client{Base: "http://127.0.0.1:1"})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.tab = tabFleet
	ps := m.pageSize()
	matched := ps * 3

	// Page 1 is requested (switching to the tab) and arrives.
	m.Update(fleetMsg{request: m.ask(tabFleet), v: reviewT5FleetPage(0, ps, matched)})
	// A refresh of page 1 is started (an event arrives) but is slow; the
	// operator pages down meanwhile, requesting page 2.
	slow := m.ask(tabFleet)
	m.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
	if m.query.Offset != ps {
		t.Fatalf("after n, query offset = %d, want %d", m.query.Offset, ps)
	}
	// Before page 2 arrives, p moves from the page on the screen (page 1).
	m.Update(tea.KeyPressMsg{Code: 'p', Text: "p"})
	if m.query.Offset != 0 {
		t.Fatalf("p before page 2 arrived: query offset = %d, want 0", m.query.Offset)
	}
	m.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
	// Page 2 arrives first, then the stale page-1 refresh.
	m.Update(fleetMsg{request: m.ask(tabFleet), v: reviewT5FleetPage(ps, ps, matched)})
	m.Update(fleetMsg{request: slow, v: reviewT5FleetPage(0, ps, matched)})

	s := screen(m)
	want := fmt.Sprintf("node-%03d", ps)
	if !strings.Contains(s, want) || strings.Contains(s, "node-000") {
		t.Errorf("view shows a stale page: query offset %d, displayed fleet offset %d; row %q expected, screen:\n%s",
			m.query.Offset, m.fleet.Offset, want, s)
	}
}

// TestReviewT5_StaleFilter: a filter is applied while an unfiltered read is
// still outstanding; the unfiltered response must not replace the filtered.
func TestReviewT5_StaleFilter(t *testing.T) {
	m := New(context.Background(), &Client{Base: "http://127.0.0.1:1"})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.tab = tabFleet
	ps := m.pageSize()
	m.Update(fleetMsg{request: m.ask(tabFleet), v: reviewT5FleetPage(0, ps, 50)})
	slow := m.ask(tabFleet)
	// Type "/node-04" and enter.
	m.Update(tea.KeyPressMsg{Code: '/', Text: "/"})
	for _, r := range "node-04" {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.query.Filter != "node-04" {
		t.Fatalf("filter = %q", m.query.Filter)
	}
	filtered := &Fleet{ConfigVersion: "v1", Total: 50, Matched: 1, Limit: ps, Members: []Member{{NodeID: "node-042", ConfigVersion: "v1"}}}
	m.Update(fleetMsg{request: m.ask(tabFleet), v: filtered})
	m.Update(fleetMsg{request: slow, v: reviewT5FleetPage(0, ps, 50)}) // stale, unfiltered
	if m.fleet.Matched != 1 {
		t.Errorf("filter %q active but view shows %d matched (an unfiltered response)", m.query.Filter, m.fleet.Matched)
	}
}

// TestReviewT5_StaleWindow: the window is changed from 1m to 5m on the CNodes
// view; a slow 1m response arriving after the 5m one must be discarded.
func TestReviewT5_StaleWindow(t *testing.T) {
	m := New(context.Background(), &Client{Base: "http://127.0.0.1:1"})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.tab = tabCNodes
	slow := m.ask(tabCNodes)
	m.Update(tea.KeyPressMsg{Code: 'w', Text: "w"})
	if windows[m.window] != "5m" {
		t.Fatalf("window = %s", windows[m.window])
	}
	m.Update(cnodesMsg{request: m.ask(tabCNodes), v: &CNodes{Window: "5m"}})
	m.Update(cnodesMsg{request: slow, v: &CNodes{Window: "1m"}}) // stale
	if m.cnodes.Window != windows[m.window] {
		t.Errorf("selected window %s but view holds %s data", windows[m.window], m.cnodes.Window)
	}
}

// TestReviewT5_FailedActionErrorErasedByItsOwnRefresh: a failed drain or
// limit change sets the error, and the Update for actionMsg itself starts a
// controls refresh; that refresh's success must not erase the failure.
func TestReviewT5_FailedActionErrorErasedByItsOwnRefresh(t *testing.T) {
	m := New(context.Background(), &Client{Base: "http://127.0.0.1:1"})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.tab = tabControls
	_, cmd := m.Update(actionMsg{err: errors.New(`drain c1/cn-3: 409 conflict`)})
	if cmd == nil || !strings.Contains(screen(m), "409 conflict") {
		t.Fatalf("failed action not shown, or no refresh started:\n%s", screen(m))
	}
	m.Update(controlsMsg{request: m.ask(tabControls)}) // the refresh the action started succeeds
	if !strings.Contains(screen(m), "409 conflict") {
		t.Errorf("the refresh that follows a failed change erased its error; status=%q", m.problem())
	}
	// The next key moves on from it.
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if strings.Contains(screen(m), "409 conflict") {
		t.Errorf("a key press did not clear the failed change's error")
	}
}

// TestReviewT5_UnrelatedSuccessClearsError: a failed fleet read sets the
// error; a successful overview read must not erase it while the fleet view
// still shows old data.
func TestReviewT5_UnrelatedSuccessClearsError(t *testing.T) {
	m := New(context.Background(), &Client{Base: "http://127.0.0.1:1"})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.tab = tabFleet
	m.Update(fleetMsg{request: m.ask(tabFleet), err: errors.New("fleet read failed: 500")})
	if !strings.Contains(screen(m), "fleet read failed") {
		t.Fatalf("error not shown at all:\n%s", screen(m))
	}
	m.Update(overviewMsg{request: m.ask(tabOverview), v: &Overview{}})
	if !strings.Contains(screen(m), "fleet read failed") {
		t.Errorf("an unrelated successful overview read cleared the fleet error; status=%q", m.problem())
	}
	// A successful fleet read does.
	m.Update(fleetMsg{request: m.ask(tabFleet), v: reviewT5FleetPage(0, m.pageSize(), 3)})
	if strings.Contains(screen(m), "fleet read failed") {
		t.Errorf("a successful fleet read did not clear the fleet error")
	}
}
