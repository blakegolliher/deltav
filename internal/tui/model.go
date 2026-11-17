package tui

import (
	"context"
	"fmt"
	"os"
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
	errText   string    // the latest failure, shown until something succeeds

	overview *Overview
	cnodes   *CNodes
	fleet    *Fleet
	targets  []TargetInfo
	clusters []Cluster
	loaded   bool // controls data has arrived at least once

	cursor   [4]int
	expanded map[string]bool // CNodes whose VIP rows are shown

	query       FleetQuery
	sortIndex   int
	stateIndex  int
	connIndex   int
	filtering   bool   // typing a filter
	filterDraft string // what has been typed so far
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

// Messages carrying what a command fetched.
type (
	eventMsg    Event
	overviewMsg struct {
		v   *Overview
		err error
	}
	cnodesMsg struct {
		v   *CNodes
		err error
	}
	fleetMsg struct {
		v   *Fleet
		err error
	}
	controlsMsg struct {
		targets  []TargetInfo
		clusters []Cluster
		err      error
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
		cmds = append(cmds, func() tea.Msg {
			v, err := api.Overview(ctx, win)
			return overviewMsg{v, err}
		})
	}
	switch m.tab {
	case tabCNodes:
		cmds = append(cmds, func() tea.Msg {
			v, err := api.CNodes(ctx, win)
			return cnodesMsg{v, err}
		})
	case tabFleet:
		q := m.query
		q.Limit = m.pageSize()
		cmds = append(cmds, func() tea.Msg {
			v, err := api.Fleet(ctx, win, q)
			return fleetMsg{v, err}
		})
	case tabControls:
		cmds = append(cmds, func() tea.Msg {
			targets, err := api.Targets(ctx)
			if err != nil {
				return controlsMsg{err: err}
			}
			clusters, err := api.Clusters(ctx)
			return controlsMsg{targets, clusters, err}
		})
	}
	return tea.Batch(cmds...)
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
		return len(m.targets)
	}
	return 0
}

func (m *Model) clampCursor() {
	m.cursor[m.tab] = max(0, min(m.cursor[m.tab], m.rows()-1))
}

func (m *Model) fail(err error) {
	if err != nil {
		m.errText = err.Error()
	} else {
		m.errText = ""
	}
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, m.fetch(false)

	case eventMsg:
		if msg.Err != nil {
			m.live = false
			m.errText = msg.Err.Error()
			return m, m.waitEvent()
		}
		m.live, m.lastEvent = true, m.now()
		if m.window == 0 || m.overview == nil {
			m.overview = msg.Overview
		} else {
			m.overview.Fleet = msg.Overview.Fleet // fleet counts are not windowed
		}
		m.clampCursor()
		return m, tea.Batch(m.waitEvent(), m.fetch(false))

	case overviewMsg:
		if m.fail(msg.err); msg.err == nil {
			m.overview = msg.v
		}
	case cnodesMsg:
		if m.fail(msg.err); msg.err == nil {
			m.cnodes = msg.v
		}
	case fleetMsg:
		if m.fail(msg.err); msg.err == nil {
			m.fleet = msg.v
			// A page past the end (the fleet shrank): go back to the last.
			if len(msg.v.Members) == 0 && msg.v.Matched > 0 && m.query.Offset > 0 {
				m.query.Offset = max(0, (msg.v.Matched-1)/m.pageSize()*m.pageSize())
				return m, m.fetch(false)
			}
		}
	case controlsMsg:
		if m.fail(msg.err); msg.err == nil {
			m.targets, m.clusters, m.loaded = msg.targets, msg.clusters, true
		}

	case tea.KeyPressMsg:
		return m.key(msg)
	}
	m.clampCursor()
	return m, nil
}

func (m *Model) key(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
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
		case "pgdown", "n":
			if m.fleet != nil && m.query.Offset+m.pageSize() < m.fleet.Matched {
				m.query.Offset += m.pageSize()
				m.cursor[tabFleet] = 0
			}
		case "pgup", "p":
			m.query.Offset = max(0, m.query.Offset-m.pageSize())
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
	}
	m.clampCursor()
	return m, nil
}
