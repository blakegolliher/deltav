package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// View draws the whole screen: a header with the tabs, the current view,
// the latest error if any, and the keys that work here.
func (m *Model) View() tea.View {
	header := m.header()
	body, focus := m.body()
	footer := m.footer()
	room := max(m.height-len(header)-len(footer), 1)
	body = window(body, room, focus)
	for len(body) < room {
		body = append(body, "")
	}
	lines := append(append(header, body...), footer...)
	for i, l := range lines {
		lines[i] = ansi.Truncate(l, m.width, "…")
	}
	v := tea.NewView(strings.Join(lines, "\n"))
	v.AltScreen = true
	v.WindowTitle = "Delta-V"
	return v
}

// window returns the part of lines that fits in height, scrolled so that
// the line at focus (if not negative) is visible.
func window(lines []string, height, focus int) []string {
	if len(lines) <= height {
		return lines
	}
	start := 0
	if focus >= height {
		start = focus - height + 1
	}
	start = min(start, len(lines)-height)
	out := append([]string(nil), lines[start:start+height]...)
	if start+height < len(lines) {
		out[height-1] = styleDim.Render(fmt.Sprintf("… %d more below", len(lines)-start-height+1))
	}
	return out
}

func (m *Model) header() []string {
	var tabs []string
	for i, name := range tabNames {
		label := fmt.Sprintf("%d %s", i+1, name)
		if tab(i) == m.tab {
			tabs = append(tabs, styleTabActive.Render(label))
		} else {
			tabs = append(tabs, styleTab.Render(label))
		}
	}
	left := styleTitle.Render("Delta-V") + "  " + strings.Join(tabs, "")
	status := styleGood.Render("● live")
	switch {
	case !m.live && m.lastEvent.IsZero():
		status = styleWarn.Render("○ connecting")
	case !m.live:
		status = styleBad.Render("○ disconnected " + age(m.now(), m.lastEvent) + " ago")
	}
	right := "window " + styleHead.Render(windows[m.window]) + "  " + status
	gap := max(m.width-lipgloss.Width(left)-lipgloss.Width(right), 2)
	return []string{left + strings.Repeat(" ", gap) + right, ""}
}

func (m *Model) footer() []string {
	// The view's own keys come first: on a narrow terminal the line is cut
	// from the right, and these are the ones not shown anywhere else.
	keys := "1-4 view  w window  ↑↓ move  r refresh  q quit"
	switch m.tab {
	case tabCNodes:
		keys = "enter show VIPs  │  " + keys
	case tabFleet:
		keys = "/ filter  x clear  s sort  o order  a state  c connected  pgup/pgdn page  │  " + keys
	case tabControls:
		keys = "e set limit  m alert/enforce  d drain/undrain  │  " + keys
	}
	if m.filtering {
		keys = "type to filter by node ID, address or version  │  enter apply  esc cancel"
	}
	last := styleDim.Render(keys)
	// A question takes the place of the key hints until it is answered.
	switch {
	case m.editing != nil:
		last = styleHead.Render(fmt.Sprintf("Connections per CNode for %s, 0 for no limit: %s█", m.editing.target, m.editing.draft)) +
			styleDim.Render("   enter apply  esc cancel")
	case m.confirm != nil && m.confirm.drain:
		last = styleHead.Render(fmt.Sprintf("Drain %s? Its VIPs leave every Envoy at once; requests in flight finish.", m.confirm.cnode)) +
			styleDim.Render("   y drain  any other key cancel")
	case m.confirm != nil:
		last = styleHead.Render(fmt.Sprintf("Undrain %s? Its VIPs are served to every Envoy again.", m.confirm.cnode)) +
			styleDim.Render("   y undrain  any other key cancel")
	}
	status := ""
	switch {
	case m.errText != "":
		status = styleBad.Render("! " + m.errText)
	case m.notice != "" && m.tab == tabControls:
		status = styleGood.Render("✓ " + m.notice)
	}
	return []string{status, last}
}

// limitCell is a target's limit for a table: "-", "100 alert", "100 enforce".
func limitCell(l *LimitStatus, over []Usage) string {
	if l == nil {
		return styleDim.Render("-")
	}
	cell := fmt.Sprintf("%d %s", l.PerCNode, l.Mode)
	if len(over) > 0 {
		return styleBad.Render(cell + " OVER")
	}
	return cell
}

// usageCell is a CNode's requests in flight against the limits that apply
// to it: "6/5" for one target, "s3 6/5, logs 1/9" for several.
func usageCell(limits []Usage) string {
	if len(limits) == 0 {
		return styleDim.Render("-")
	}
	var parts []string
	for _, u := range limits {
		part := fmt.Sprintf("%d/%d", u.InFlight, u.PerCNode)
		if len(limits) > 1 {
			part = u.Target + " " + part
		}
		if u.Over {
			part = styleBad.Render(part + " OVER")
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ", ")
}

// cnodeState is a CNode's state for a table.
func cnodeState(c CNode) string {
	switch {
	case !c.Enabled:
		return styleBad.Render(c.State + " disabled")
	case !c.Healthy:
		return styleBad.Render(c.State)
	case c.Drained:
		return styleWarn.Render(c.State + " drained")
	}
	return styleGood.Render(c.State)
}

// body returns the current view's lines and the index of the line holding
// the selected row, or -1.
func (m *Model) body() ([]string, int) {
	switch m.tab {
	case tabCNodes:
		return m.cnodesView()
	case tabFleet:
		return m.fleetView()
	case tabControls:
		return m.controlsView()
	}
	return m.overviewView()
}

var statColumns = []column{
	{"IN FLIGHT", true}, {"REQ/S", true}, {"MB/S IN", true}, {"MB/S OUT", true},
	{"P50", true}, {"P99", true}, {"403/S", true}, {"503/S", true},
}

// statCells are the cells for statColumns.
func statCells(inFlight uint64, s Summary) []string {
	return []string{
		fmt.Sprint(inFlight), rate(s.RequestRate), mbps(s.BytesInRate), mbps(s.BytesOutRate),
		millis(s.LatencyMS.P50), millis(s.LatencyMS.P99),
		failures(s.Rate403, s.Status.S403), failures(s.Rate503, s.Status.S503),
	}
}

func (m *Model) fleetLine() string {
	if m.overview == nil {
		return ""
	}
	f := m.overview.Fleet
	parts := []string{fmt.Sprintf("%d in sync", f.Acked)}
	if f.Nacked > 0 {
		parts = append(parts, styleBad.Render(fmt.Sprintf("%d rejected the config", f.Nacked)))
	}
	if f.Pending > 0 {
		parts = append(parts, styleWarn.Render(fmt.Sprintf("%d pending", f.Pending)))
	}
	line := fmt.Sprintf("%s  %d Envoys connected (%s)", styleHead.Render("Fleet"), f.Connected, strings.Join(parts, ", "))
	if gone := f.Total - f.Connected; gone > 0 {
		line += styleDim.Render(fmt.Sprintf(", %d disconnected", gone))
	}
	return line
}

func (m *Model) overviewView() ([]string, int) {
	if m.overview == nil {
		return []string{styleDim.Render("Waiting for the daemon…")}, -1
	}
	out := []string{m.fleetLine(), ""}
	if len(m.overview.Targets) == 0 {
		return append(out, "No targets yet.", "",
			styleDim.Render("Attach a VAST cluster and add a target:"),
			styleDim.Render("  deltav vast add NAME --host HOST --user USER"),
			styleDim.Render("  deltav target add NAME --cluster NAME --pool POOL --port 80 --listen-port 9000")), -1
	}
	cols := append([]column{{"TARGET", false}, {"LISTEN", false}, {"ENDPOINTS", true}}, statColumns...)
	cols = append(cols, column{"LIMIT", false}, column{"LAST 5 MIN", false})
	var rows [][]string
	breached := false
	for _, t := range m.overview.Targets {
		endpoints := fmt.Sprint(t.Endpoints)
		if t.Endpoints == 0 {
			endpoints = styleBad.Render("0")
		}
		row := []string{t.Name, fmt.Sprintf("127.0.0.1:%d", t.ListenPort), endpoints}
		row = append(row, statCells(t.InFlight, t.Stats)...)
		rows = append(rows, append(row, limitCell(t.Limit, t.Over), sparkline(t.RateHistory, 29)))
		// Limit breaches lead: they are what an operator opens this for.
		for _, o := range t.Over {
			out = append(out, styleBad.Render(fmt.Sprintf("Over the limit: %s on %s has %d in flight, limit %d", t.Name, o.CNode, o.InFlight, o.PerCNode)))
			breached = true
		}
	}
	if breached {
		out = append(out, "")
	}
	out = append(out, table(cols, rows, m.cursor[tabOverview], m.width)...)
	return out, len(out) - len(rows) + m.cursor[tabOverview]
}

func (m *Model) cnodesView() ([]string, int) {
	if m.cnodes == nil {
		return []string{styleDim.Render("Reading CNodes…")}, -1
	}
	if len(m.cnodes.CNodes) == 0 {
		return []string{"No CNodes to show.", "",
			styleDim.Render("CNodes come from attached VAST clusters. Static targets have none; their"),
			styleDim.Render("traffic is on the Overview.")}, -1
	}
	clusters := map[string]bool{}
	for _, c := range m.cnodes.CNodes {
		clusters[c.Cluster] = true
	}
	cols := append([]column{{"CNODE", false}, {"STATE", false}, {"VIPS", true}}, statColumns...)
	cols = append(cols, column{"OF LIMIT", false}, column{"LAST 5 MIN", false})
	var rows [][]string
	selected := -1
	for i, c := range m.cnodes.CNodes {
		name := c.Name
		if len(clusters) > 1 {
			name = c.Cluster + "/" + c.Name
		}
		if c.IsLeader {
			name += styleDim.Render(" leader")
		}
		if c.IsMgmt {
			name += styleDim.Render(" mgmt")
		}
		if i == m.cursor[tabCNodes] {
			selected = len(rows)
		}
		row := append([]string{name, cnodeState(c), fmt.Sprint(c.VIPs)}, statCells(c.InFlight, c.Stats)...)
		rows = append(rows, append(row, usageCell(c.Limits), sparkline(c.RateHistory, 29)))
		if m.expanded[c.Cluster+"/"+c.Name] {
			for j, v := range c.Endpoints {
				branch := "├ "
				if j == len(c.Endpoints)-1 {
					branch = "└ "
				}
				vip := append([]string{styleDim.Render("  "+branch) + v.Endpoint, styleDim.Render(v.Target), ""}, statCells(v.InFlight, v.Stats)...)
				rows = append(rows, vip)
			}
			if len(c.Endpoints) == 0 {
				rows = append(rows, []string{styleDim.Render("  └ hosts no VIP of a configured target")})
			}
		}
	}
	out := table(cols, rows, selected, m.width)
	out = append(out, "", styleDim.Render("In flight: requests Envoys have outstanding, from load reports. Idle keep-alive connections are not counted."))
	return out, selected + 1
}

func (m *Model) fleetView() ([]string, int) {
	q := m.query
	order := "↑"
	if q.Desc {
		order = "↓"
	}
	show := func(v, none string) string {
		if v == "" {
			return styleDim.Render(none)
		}
		return styleHead.Render(v)
	}
	filter := show(q.Filter, "none")
	if m.filtering {
		filter = styleHead.Render(m.filterDraft) + "█"
	}
	conn := map[string]string{"": "", "true": "connected", "false": "disconnected"}[q.Connected]
	out := []string{fmt.Sprintf("Sort %s %s   Filter %s   State %s   Showing %s",
		styleHead.Render(q.Sort), order, filter, show(q.State, "any"), show(conn, "all")), ""}
	if m.fleet == nil {
		return append(out, styleDim.Render("Reading the fleet…")), -1
	}
	cols := []column{{"NODE", false}, {"ENVOY", false}, {"CONFIG", false}, {"LAST SEEN", true},
		{"REQ/S", true}, {"IN FLIGHT", true}, {"P99", true}, {"ADDRESS", false}}
	var rows [][]string
	for _, e := range m.fleet.Members {
		config := styleGood.Render("in sync")
		switch {
		case !e.Connected:
			config = styleDim.Render("disconnected")
		case e.ConfigState == "nacked":
			config = styleBad.Render("rejected")
		case e.ConfigState == "pending":
			config = styleWarn.Render("pending")
		case e.ConfigVersion != m.fleet.ConfigVersion:
			config = styleWarn.Render("behind")
		}
		rows = append(rows, []string{e.NodeID, e.EnvoyVersion, config, age(m.now(), e.LastSeen),
			rate(e.Stats.RequestRate), fmt.Sprint(e.InFlight), millis(e.Stats.LatencyMS.P99), e.Address})
	}
	cur := m.cursor[tabFleet]
	out = append(out, table(cols, rows, cur, m.width)...)
	focus := len(out) - len(rows) + cur
	if len(rows) == 0 {
		out = append(out, styleDim.Render("No Envoy matches."))
		focus = -1
	}
	first := m.fleet.Offset + 1
	if len(rows) == 0 {
		first = 0
	}
	out = append(out, "", styleDim.Render(fmt.Sprintf("Rows %d-%d of %d matching · %d Envoys, %d connected",
		first, m.fleet.Offset+len(rows), m.fleet.Matched, m.fleet.Total, m.fleet.Connected)))
	// Why the selected Envoy rejected its config, in Envoy's words.
	if cur < len(m.fleet.Members) {
		if e := m.fleet.Members[cur]; e.ConfigError != "" {
			out = append(out, styleBad.Render(e.NodeID+" rejected the config: ")+e.ConfigError)
		}
	}
	return out, focus
}

func (m *Model) controlsView() ([]string, int) {
	if !m.loaded {
		return []string{styleDim.Render("Reading clusters and targets…")}, -1
	}
	out := []string{styleHead.Render("VAST clusters")}
	if len(m.clusters) == 0 {
		out = append(out, styleDim.Render("  none attached"))
	} else {
		var rows [][]string
		for _, c := range m.clusters {
			status := styleGood.Render("ok")
			if c.LastError != "" {
				status = styleBad.Render("failing: " + c.LastError)
			}
			last := "never"
			if c.LastPoll != nil {
				last = age(m.now(), *c.LastPoll) + " ago"
			}
			rows = append(rows, []string{c.Name, c.Host, fmt.Sprint(c.CNodes), fmt.Sprint(c.Pools), fmt.Sprint(c.VIPs), last, status})
		}
		out = append(out, table([]column{{"NAME", false}, {"HOST", false}, {"CNODES", true}, {"POOLS", true}, {"VIPS", true},
			{"LAST READ", true}, {"STATUS", false}}, rows, -1, m.width)...)
	}
	out = append(out, "", styleHead.Render("Targets"))
	focus := -1
	cur := m.cursor[tabControls]
	if len(m.targets) == 0 {
		out = append(out, styleDim.Render("  none"))
	} else {
		var rows [][]string
		for _, t := range m.targets {
			from := "static"
			if t.Source == "vast" {
				from = t.Cluster + " / " + t.Pool
			}
			tls := "no"
			if t.TLS {
				tls = "yes"
			}
			limit := styleDim.Render("-")
			status := styleDim.Render("fixed endpoint list")
			if st := t.Status; st != nil {
				switch {
				case st.Problem != "" && st.Held:
					status = styleWarn.Render("serving the last known endpoints: " + st.Problem)
				case st.Problem != "":
					status = styleBad.Render(st.Problem)
				case st.Held:
					status = styleWarn.Render("serving the last known endpoints: the pool resolved to none")
				case st.Healthy < st.VIPs || st.Drained > 0:
					status = fmt.Sprintf("%d of %d VIPs on healthy CNodes", st.Healthy, st.VIPs)
					if st.Drained > 0 {
						status += fmt.Sprintf(", %d withheld by drain", st.Drained)
					}
					status = styleWarn.Render(status)
				default:
					status = styleGood.Render(fmt.Sprintf("%d of %d VIPs on healthy CNodes", st.Healthy, st.VIPs))
				}
				if l := st.Limit; l != nil {
					limit = limitCell(l, t.Over)
					if l.Mode == "enforce" {
						limit += fmt.Sprintf(": %d per Envoy per VIP", l.Share)
						if l.Computed != l.Share {
							limit += styleDim.Render(fmt.Sprintf(" (%d held back)", l.Computed))
						}
					}
				}
			}
			rows = append(rows, []string{t.Name, fmt.Sprintf("127.0.0.1:%d", t.ListenPort), from, tls, fmt.Sprint(len(t.Endpoints)), limit, status})
		}
		selected := -1
		if cur < len(m.targets) {
			selected = cur
			focus = len(out) + 1 + cur
		}
		out = append(out, table([]column{{"NAME", false}, {"LISTEN", false}, {"FROM", false}, {"TLS", false}, {"ENDPOINTS", true},
			{"LIMIT", false}, {"STATUS", false}}, rows, selected, m.width)...)
	}
	out = append(out, "", styleHead.Render("CNodes"))
	if len(m.control) == 0 {
		out = append(out, styleDim.Render("  none: no VAST cluster is attached"))
	} else {
		var rows [][]string
		for _, c := range m.control {
			rows = append(rows, []string{c.Cluster + " / " + c.Name, cnodeState(c), fmt.Sprint(c.VIPs), fmt.Sprint(c.InFlight), usageCell(c.Limits)})
		}
		selected := -1
		if i := cur - len(m.targets); i >= 0 && i < len(m.control) {
			selected = i
			focus = len(out) + 1 + i
		}
		out = append(out, table([]column{{"CNODE", false}, {"STATE", false}, {"VIPS", true}, {"IN FLIGHT", true}, {"OF LIMIT", false}},
			rows, selected, m.width)...)
	}
	out = append(out, "",
		styleDim.Render("A limit is the connections (requests in flight) the fleet should hold to any one CNode for a target."),
		styleDim.Render("alert reports CNodes over it. enforce also caps every Envoy, at limit ÷ (connected Envoys × most VIPs on"),
		styleDim.Render("one CNode), never below 1. The cap is each Envoy's own, so the fleet total is approximate; and changing"),
		styleDim.Render("it makes Envoys reconnect, so as Envoys come and go a new cap is held back until it differs enough."),
		styleDim.Render("Drain withholds a CNode's VIPs from every Envoy until it is undrained. Nothing changes on the VAST cluster."),
		styleDim.Render("Clusters and targets are added and removed with \"deltav vast\" and \"deltav target\"."))
	return out, focus
}
