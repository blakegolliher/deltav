package tui

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/term"
)

type tab int

const (
	tabOverview tab = iota
	tabCNodes
	tabFleet
	tabControls
)

var (
	tabNames = []string{"Overview", "CNodes", "Fleet", "Controls"}
	windows  = []string{"1m", "5m", "1h"}
	// fleetSorts are the sort keys the "s" key cycles through, with the
	// direction each starts in.
	fleetSorts = []struct {
		key  string
		desc bool
	}{{"node_id", false}, {"request_rate", true}, {"last_seen", true}, {"config_state", true}, {"envoy_version", false}}
	fleetStates    = []string{"", "nacked", "pending", "acked"}
	fleetConnected = []string{"", "true", "false"}
)

// limitEdit is a per-CNode limit being typed for a target.
type limitEdit struct {
	target string
	mode   string // kept as it is; "m" changes it
	draft  string
}

// drainConfirm is a drain or undrain waiting for a yes.
type drainConfirm struct {
	cluster, cnode string
	drain          bool
}

// eventInterval is how often the daemon is asked to send an overview.
const eventInterval = 2 * time.Second

// Model is the TUI's state. It follows the daemon's event stream: every
// event carries the overview, and prompts a fresh read of whatever the
// current view shows.
type Model struct {
	ctx    context.Context
	api    *Client
	events <-chan Event
	now    func() time.Time

	width, height int
	tab           tab
	window        int

	live      bool      // the event stream is connected
	lastEvent time.Time // when the last event arrived
	streamErr string    // why the event stream broke, until an overview arrives
	// readErr is, per view, why its last read failed. Only a read of the
	// same view clears it: another view's success says nothing about it.
	readErr [4]string
	// errText is a failed change, or a key that could not be used. It stays
	// until the next key or change, so the refresh a failure starts does
	// not erase it.
	errText string

	// seq numbers each read; shown is, per view, the number of the read it
	// shows. Answers can arrive out of order.
	seq   uint64
	shown [4]uint64

	overview *Overview
	cnodes   *CNodes
	fleet    *Fleet
	targets  []TargetInfo
	clusters []Cluster
	control  []CNode // the CNodes listed on the Controls view
	rules    []RuleStatus
	loaded   bool // controls data has arrived at least once

	// Controls: at most one of these is in progress.
	editing *limitEdit    // typing a target's limit
	confirm *drainConfirm // asked whether to drain or undrain a CNode
	notice  string        // what the last change did

	cursor   [4]int
	expanded map[string]bool // CNodes whose VIP rows are shown

	query       FleetQuery
	sortIndex   int
	stateIndex  int
	connIndex   int
	filtering   bool   // typing a filter
	filterDraft string // what has been typed so far

	// about is true while the notes on limits and drain fill the screen
	// ("?" on the Controls view): on a short terminal they do not fit
	// under the tables.
	about bool
}

// New builds the model for a daemon's API.
func New(ctx context.Context, api *Client) *Model {
	return &Model{
		ctx: ctx, api: api, now: time.Now, width: 100, height: 30,
		expanded: map[string]bool{},
		query:    FleetQuery{Sort: fleetSorts[0].key, Desc: fleetSorts[0].desc},
	}
}

// Run shows the TUI until the user quits or ctx ends.
func Run(ctx context.Context, api *Client) error {
	if !term.IsTerminal(os.Stdout.Fd()) {
		return fmt.Errorf("the TUI needs a terminal; for scripts use the list commands, which take --json")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	_, err := tea.NewProgram(New(ctx, api), tea.WithContext(ctx)).Run()
	return err
}

// reqKey is what a view's read asks for: the window, and for the fleet the
// page, sort and filters.
type reqKey struct {
	window string
	query  FleetQuery
}

// request is a read as it was sent: what it asked for, and its number.
type request struct {
	key reqKey
	seq uint64
}

// Messages carrying what a command fetched.
type (
	eventMsg    Event
	overviewMsg struct {
		request
		v   *Overview
		err error
	}
	cnodesMsg struct {
		request
		v   *CNodes
		err error
	}
	fleetMsg struct {
		request
		v   *Fleet
		err error
	}
	controlsMsg struct {
		request
		targets  []TargetInfo
		clusters []Cluster
		cnodes   []CNode
		rules    []RuleStatus
		err      error
	}
	// actionMsg reports a change made from the Controls view.
	actionMsg struct {
		done string
		err  error
	}
)

func (m *Model) Init() tea.Cmd {
	m.events = m.api.Subscribe(m.ctx, eventInterval)
	return tea.Batch(m.waitEvent(), m.fetch(true))
}

// waitEvent delivers the next item of the event stream.
func (m *Model) waitEvent() tea.Cmd {
	events := m.events
	return func() tea.Msg {
		ev, ok := <-events
		if !ok {
			return nil
		}
		return eventMsg(ev)
	}
}

// pageSize is how many fleet rows fit on the screen.
func (m *Model) pageSize() int {
	return max(m.height-9, 3)
}

// fetch reads what the current view shows. The overview comes with every
// event in the one-minute window, so it is fetched only when asked for
// (withOverview) or when another window is selected.
func (m *Model) fetch(withOverview bool) tea.Cmd {
	api, ctx, win := m.api, m.ctx, windows[m.window]
	var cmds []tea.Cmd
	if m.tab == tabOverview && (withOverview || m.window != 0) {
		r := m.ask(tabOverview)
		cmds = append(cmds, func() tea.Msg {
			v, err := api.Overview(ctx, win)
			return overviewMsg{r, v, err}
		})
	}
	switch m.tab {
	case tabCNodes:
		r := m.ask(tabCNodes)
		cmds = append(cmds, func() tea.Msg {
			v, err := api.CNodes(ctx, win)
			return cnodesMsg{r, v, err}
		})
	case tabFleet:
		r := m.ask(tabFleet)
		cmds = append(cmds, func() tea.Msg {
			v, err := api.Fleet(ctx, win, r.key.query)
			return fleetMsg{r, v, err}
		})
	case tabControls:
		r := m.ask(tabControls)
		cmds = append(cmds, func() tea.Msg {
			targets, err := api.Targets(ctx)
			if err != nil {
				return controlsMsg{request: r, err: err}
			}
			clusters, err := api.Clusters(ctx)
			if err != nil {
				return controlsMsg{request: r, err: err}
			}
			cnodes, err := api.CNodes(ctx, win)
			if err != nil {
				return controlsMsg{request: r, err: err}
			}
			rules, err := api.Rules(ctx)
			if err != nil {
				return controlsMsg{request: r, err: err}
			}
			return controlsMsg{r, targets, clusters, cnodes.CNodes, rules, nil}
		})
	}
	return tea.Batch(cmds...)
}

// want is what a read of view t asks for now.
func (m *Model) want(t tab) reqKey {
	k := reqKey{window: windows[m.window]}
	if t == tabFleet {
		k.query = m.query
		k.query.Limit = m.pageSize()
	}
	return k
}

// ask numbers a read of view t.
func (m *Model) ask(t tab) request {
	m.seq++
	return request{m.want(t), m.seq}
}

// current reports whether an answer is for what view t asks for now, and no
// older than what it shows; if so, the view takes it. An answer for an
// earlier page, filter or window, or overtaken by a later read, is dropped.
func (m *Model) current(t tab, r request) bool {
	if r.key != m.want(t) || r.seq < m.shown[t] {
		return false
	}
	m.shown[t] = r.seq
	return true
}

// read records how view t's read ended, and reports whether it succeeded.
func (m *Model) read(t tab, err error) bool {
	if err != nil {
		m.readErr[t] = err.Error()
		return false
	}
	m.readErr[t] = ""
	return true
}

// problem is the failure the status line shows: a failed change or key
// first, then the current view's read, then the event stream.
func (m *Model) problem() string {
	switch {
	case m.errText != "":
		return m.errText
	case m.readErr[m.tab] != "":
		return m.readErr[m.tab]
	}
	return m.streamErr
}

// rows is how many selectable rows the current view has.
func (m *Model) rows() int {
	switch m.tab {
	case tabOverview:
		if m.overview != nil {
			return len(m.overview.Targets)
		}
	case tabCNodes:
		if m.cnodes != nil {
			return len(m.cnodes.CNodes)
		}
	case tabFleet:
		if m.fleet != nil {
			return len(m.fleet.Members)
		}
	case tabControls:
		return len(m.targets) + len(m.control)
	}
	return 0
}

// selection returns what the Controls cursor is on: a target or a CNode.
func (m *Model) selection() (*TargetInfo, *CNode) {
	i := m.cursor[tabControls]
	switch {
	case i < 0:
	case i < len(m.targets):
		return &m.targets[i], nil
	case i-len(m.targets) < len(m.control):
		return nil, &m.control[i-len(m.targets)]
	}
	return nil, nil
}

func (m *Model) clampCursor() {
	m.cursor[m.tab] = max(0, min(m.cursor[m.tab], m.rows()-1))
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, m.fetch(false)

	case eventMsg:
		if msg.Err != nil {
			m.live = false
			m.streamErr = msg.Err.Error()
			return m, m.waitEvent()
		}
		m.live, m.lastEvent, m.streamErr = true, m.now(), ""
		if m.window == 0 || m.overview == nil {
			m.overview = msg.Overview
		} else {
			m.overview.Fleet = msg.Overview.Fleet // fleet counts are not windowed
		}
		m.clampCursor()
		return m, tea.Batch(m.waitEvent(), m.fetch(false))

	case overviewMsg:
		if !m.current(tabOverview, msg.request) {
			break
		}
		if m.read(tabOverview, msg.err) {
			// The daemon answered with what the stream carries.
			m.overview, m.streamErr = msg.v, ""
		}
	case cnodesMsg:
		if m.current(tabCNodes, msg.request) && m.read(tabCNodes, msg.err) {
			m.cnodes = msg.v
		}
	case fleetMsg:
		if m.current(tabFleet, msg.request) && m.read(tabFleet, msg.err) {
			m.fleet = msg.v
			// A page past the end (the fleet shrank): go back to the last.
			if len(msg.v.Members) == 0 && msg.v.Matched > 0 && m.query.Offset > 0 {
				m.query.Offset = max(0, (msg.v.Matched-1)/m.pageSize()*m.pageSize())
				return m, m.fetch(false)
			}
		}
	case controlsMsg:
		if m.current(tabControls, msg.request) && m.read(tabControls, msg.err) {
			m.targets, m.clusters, m.control, m.rules, m.loaded = msg.targets, msg.clusters, msg.cnodes, msg.rules, true
		}
	case actionMsg:
		m.errText, m.notice = "", msg.done
		if msg.err != nil {
			m.errText, m.notice = msg.err.Error(), ""
		}
		return m, m.fetch(true)

	case tea.KeyPressMsg:
		return m.key(msg)
	}
	m.clampCursor()
	return m, nil
}

func (m *Model) key(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	m.errText = "" // a key moves on from the last failed change
	if m.about {
		// Any key puts the notes away; quitting still quits.
		m.about = false
		if k == "q" || k == "ctrl+c" {
			return m, tea.Quit
		}
		return m, nil
	}
	if m.editing != nil {
		return m.editKey(k, msg)
	}
	if m.confirm != nil {
		c := m.confirm
		m.confirm = nil
		if k == "ctrl+c" {
			return m, tea.Quit
		}
		if k != "y" && k != "Y" {
			return m, nil
		}
		api, ctx := m.api, m.ctx
		done := "undrained " + c.cnode + ": its VIPs are served again"
		if c.drain {
			done = "drained " + c.cnode + ": its VIPs have left every Envoy"
		}
		return m, func() tea.Msg { return actionMsg{done, api.SetDrained(ctx, c.cluster, c.cnode, c.drain)} }
	}
	if m.filtering {
		switch k {
		case "enter":
			m.filtering = false
			m.query.Filter, m.query.Offset = m.filterDraft, 0
			return m, m.fetch(false)
		case "esc":
			m.filtering = false
		case "backspace":
			if r := []rune(m.filterDraft); len(r) > 0 {
				m.filterDraft = string(r[:len(r)-1])
			}
		case "ctrl+c":
			return m, tea.Quit
		default:
			if t := msg.Key().Text; t != "" {
				m.filterDraft += t
			}
		}
		return m, nil
	}

	switchTo := func(t tab) (tea.Model, tea.Cmd) {
		m.tab = (t + tab(len(tabNames))) % tab(len(tabNames))
		return m, m.fetch(true)
	}
	switch k {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "?":
		if m.tab == tabControls {
			m.about = true
		}
		return m, nil
	case "1", "2", "3", "4":
		return switchTo(tab(k[0] - '1'))
	case "tab", "right", "l":
		return switchTo(m.tab + 1)
	case "shift+tab", "left", "h":
		return switchTo(m.tab - 1)
	case "w":
		m.window = (m.window + 1) % len(windows)
		return m, m.fetch(true)
	case "r":
		return m, m.fetch(true)
	case "up", "k":
		m.cursor[m.tab]--
	case "down", "j":
		m.cursor[m.tab]++
	case "home", "g":
		m.cursor[m.tab] = 0
	case "end", "G":
		m.cursor[m.tab] = m.rows() - 1
	}

	switch m.tab {
	case tabCNodes:
		if (k == "enter" || k == "space") && m.cnodes != nil && m.cursor[tabCNodes] < len(m.cnodes.CNodes) {
			c := m.cnodes.CNodes[m.cursor[tabCNodes]]
			key := c.Cluster + "/" + c.Name
			m.expanded[key] = !m.expanded[key]
		}
	case tabFleet:
		refetch := true
		switch k {
		// Pages move from the page on the screen, which a read in flight
		// may not have replaced yet.
		case "pgdown", "n":
			if m.fleet != nil && m.fleet.Offset+m.pageSize() < m.fleet.Matched {
				m.query.Offset = m.fleet.Offset + m.pageSize()
				m.cursor[tabFleet] = 0
			}
		case "pgup", "p":
			from := m.query.Offset
			if m.fleet != nil {
				from = m.fleet.Offset
			}
			m.query.Offset = max(0, from-m.pageSize())
			m.cursor[tabFleet] = 0
		case "s":
			m.sortIndex = (m.sortIndex + 1) % len(fleetSorts)
			m.query.Sort, m.query.Desc, m.query.Offset = fleetSorts[m.sortIndex].key, fleetSorts[m.sortIndex].desc, 0
		case "o":
			m.query.Desc, m.query.Offset = !m.query.Desc, 0
		case "a":
			m.stateIndex = (m.stateIndex + 1) % len(fleetStates)
			m.query.State, m.query.Offset = fleetStates[m.stateIndex], 0
		case "c":
			m.connIndex = (m.connIndex + 1) % len(fleetConnected)
			m.query.Connected, m.query.Offset = fleetConnected[m.connIndex], 0
		case "/":
			m.filtering, m.filterDraft = true, m.query.Filter
			refetch = false
		case "x":
			m.query.Filter, m.query.Offset = "", 0
		default:
			refetch = false
		}
		if refetch {
			return m, m.fetch(false)
		}
	case tabControls:
		m.clampCursor()
		target, cnode := m.selection()
		switch k {
		case "e", "m":
			switch {
			case target == nil:
				m.errText = "move to a target to change its limit"
			case target.Source != "vast":
				m.errText = "a static target has no CNodes, so it cannot have a per-CNode limit"
			case k == "e":
				m.editing = &limitEdit{target: target.Name, mode: "alert"}
				if target.Status != nil && target.Status.Limit != nil {
					m.editing.mode = target.Status.Limit.Mode
					m.editing.draft = fmt.Sprint(target.Status.Limit.PerCNode)
				}
			case target.Status == nil || target.Status.Limit == nil:
				m.errText = "set a limit first (e); it starts in alert mode"
			default:
				l, name := target.Status.Limit, target.Name
				mode := "enforce"
				if l.Mode == "enforce" {
					mode = "alert"
				}
				api, ctx := m.api, m.ctx
				return m, func() tea.Msg {
					return actionMsg{fmt.Sprintf("%s: limit %d is now in %s mode", name, l.PerCNode, mode), api.SetLimit(ctx, name, l.PerCNode, mode)}
				}
			}
		case "d":
			if cnode == nil {
				m.errText = "move to a CNode to drain or undrain it"
			} else {
				m.confirm = &drainConfirm{cluster: cnode.Cluster, cnode: cnode.Name, drain: !cnode.Drained}
			}
		}
	}
	m.clampCursor()
	return m, nil
}

// editKey handles a key while a limit is being typed.
func (m *Model) editKey(k string, msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	e := m.editing
	switch k {
	case "esc":
		m.editing = nil
	case "ctrl+c":
		return m, tea.Quit
	case "backspace":
		if len(e.draft) > 0 {
			e.draft = e.draft[:len(e.draft)-1]
		}
	case "enter":
		m.editing = nil
		n, err := strconv.ParseUint(e.draft, 10, 32)
		if err != nil {
			m.errText = fmt.Sprintf("%q is not a number of connections", e.draft)
			return m, nil
		}
		api, ctx := m.api, m.ctx
		done := fmt.Sprintf("%s: limit %d connections per CNode, %s mode", e.target, n, e.mode)
		if n == 0 {
			done = e.target + ": limit removed"
		}
		return m, func() tea.Msg { return actionMsg{done, api.SetLimit(ctx, e.target, uint32(n), e.mode)} }
	default:
		if t := msg.Key().Text; len(t) == 1 && t[0] >= '0' && t[0] <= '9' && len(e.draft) < 9 {
			e.draft += t
		}
	}
	return m, nil
}
