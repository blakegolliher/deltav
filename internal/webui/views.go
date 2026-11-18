package webui

import (
	"context"
	"fmt"
	"hash/fnv"
	"html/template"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"deltav/internal/apiclient"
	"deltav/internal/ratelimit"
)

// The four pages' views: what each asks the API for and how it lays the
// answers out for its templates.

// readParams takes the page's own parameters from a query, dropping what is
// not valid and what equals the default.
func (v *view) readParams(in url.Values) {
	switch v.spec.name {
	case "cnodes":
		seen := map[string]bool{}
		for _, key := range in["open"] {
			if key != "" && len(key) <= 300 && !seen[key] && len(seen) < 200 {
				seen[key] = true
				v.query.Add("open", key)
			}
		}
		sort.Strings(v.query["open"])
	case "fleet":
		if q := strings.TrimSpace(in.Get("q")); q != "" && len(q) <= 200 {
			v.query.Set("q", q)
		}
		switch s := in.Get("state"); s {
		case "acked", "nacked", "pending":
			v.query.Set("state", s)
		}
		switch c := in.Get("connected"); c {
		case "true", "false":
			v.query.Set("connected", c)
		}
		for _, c := range fleetColumns {
			if c.key != "" && c.key != "node_id" && in.Get("sort") == c.key {
				v.query.Set("sort", c.key)
			}
		}
		if in.Get("order") == "desc" {
			v.query.Set("order", "desc")
		}
		if n, err := strconv.Atoi(in.Get("offset")); err == nil && n > 0 {
			v.query.Set("offset", strconv.Itoa(n))
		}
		for _, size := range fleetPageSizes {
			if size != defaultFleetPage && in.Get("limit") == strconv.Itoa(size) {
				v.query.Set("limit", strconv.Itoa(size))
			}
		}
	}
}

// ---- Overview ----

type overviewTarget struct {
	apiclient.Target
	Source  string
	Cluster string
	Pool    string
	Status  *apiclient.TargetStatus
}

type overviewView struct {
	ConfigVersion string
	Fleet         apiclient.FleetCounts
	Targets       []overviewTarget

	// Totals over every target.
	RequestRate, InRate, OutRate float64
	Rate403, Rate503             float64
	Requests, S403, S503         uint64
	InFlight                     uint64
	History                      []float64
	Limits                       int // targets with a limit
	Over                         []apiclient.Usage
	RateRules                    apiclient.RateRules
}

func (u *UI) buildOverview(ctx context.Context, v *view) error {
	ov := &overviewView{}
	v.Overview = ov
	o, err := u.api.Overview(ctx, v.Window)
	if err != nil {
		return err
	}
	ov.ConfigVersion, ov.Fleet, ov.RateRules = o.ConfigVersion, o.Fleet, o.RateRules
	// The targets list adds what the overview leaves out: where a target
	// comes from and how its pool resolved.
	infos, infoErr := u.api.Targets(ctx)
	byName := map[string]apiclient.TargetInfo{}
	for _, t := range infos {
		byName[t.Name] = t
	}
	for _, t := range o.Targets {
		row := overviewTarget{Target: t}
		if info, ok := byName[t.Name]; ok {
			row.Source, row.Cluster, row.Pool, row.Status = info.Source, info.Cluster, info.Pool, info.Status
		}
		ov.Targets = append(ov.Targets, row)
		ov.RequestRate += t.Stats.RequestRate
		ov.InRate += t.Stats.BytesInRate
		ov.OutRate += t.Stats.BytesOutRate
		ov.Rate403 += t.Stats.Rate403
		ov.Rate503 += t.Stats.Rate503
		ov.Requests += t.Stats.Requests
		ov.S403 += t.Stats.Status.S403
		ov.S503 += t.Stats.Status.S503
		ov.InFlight += t.InFlight
		if len(ov.History) < len(t.RateHistory) {
			ov.History = append(ov.History, make([]float64, len(t.RateHistory)-len(ov.History))...)
		}
		for i, r := range t.RateHistory {
			ov.History[i] += r
		}
		if t.Limit != nil {
			ov.Limits++
		}
		ov.Over = append(ov.Over, t.Over...)
	}
	return infoErr
}

// ---- CNodes ----

type cnodeRow struct {
	apiclient.CNode
	ID     string // of the row's element
	Open   bool
	Toggle string // the page with this row opened or closed
}

type cnodesView struct {
	Rows []cnodeRow
	// ManyClusters is true when the rows come from more than one VAST
	// cluster, so the cluster needs showing.
	ManyClusters bool
	// Columns is how many columns the table has, for the row that holds a
	// CNode's VIPs.
	Columns int
}

// domID makes an element ID from a name that may hold anything.
func domID(prefix, name string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	return prefix + strconv.FormatUint(h.Sum64(), 36)
}

func (u *UI) buildCNodes(ctx context.Context, v *view) error {
	cv := &cnodesView{Columns: 12}
	v.CNodes = cv
	res, err := u.api.CNodes(ctx, v.Window)
	if err != nil {
		return err
	}
	open := map[string]bool{}
	for _, key := range v.query["open"] {
		open[key] = true
	}
	clusters := map[string]bool{}
	for _, c := range res.CNodes {
		key := c.Cluster + "/" + c.Name
		clusters[c.Cluster] = true
		sort.SliceStable(c.Endpoints, func(i, j int) bool {
			if c.Endpoints[i].Target != c.Endpoints[j].Target {
				return c.Endpoints[i].Target < c.Endpoints[j].Target
			}
			return c.Endpoints[i].Endpoint < c.Endpoints[j].Endpoint
		})
		cv.Rows = append(cv.Rows, cnodeRow{
			CNode: c, ID: domID("cnode-", key), Open: open[key],
			Toggle: v.url(func(q url.Values) {
				keys := q["open"][:0:0]
				for _, k := range q["open"] {
					if k != key {
						keys = append(keys, k)
					}
				}
				if !open[key] {
					keys = append(keys, key)
				}
				sort.Strings(keys)
				q.Del("open")
				for _, k := range keys {
					q.Add("open", k)
				}
			}),
		})
	}
	if cv.ManyClusters = len(clusters) > 1; cv.ManyClusters {
		cv.Columns++
	}
	return nil
}

// ---- Fleet ----

const defaultFleetPage = 100

var fleetPageSizes = []int{25, 50, 100, 250, 500}

// fleetColumns are the fleet table's columns. key is the API's sort key, or
// "" for a column that cannot be sorted by. down is true where the first
// click should put the largest or latest first.
var fleetColumns = []struct {
	label, key string
	numeric    bool
	down       bool
}{
	{label: "Envoy", key: "node_id"},
	{label: "Address", key: "address"},
	{label: "Version", key: "envoy_version"},
	{label: "Last seen", key: "last_seen", down: true},
	{label: "Config", key: "config_state"},
	{label: "Requests/s", key: "request_rate", numeric: true, down: true},
	{label: "In flight", numeric: true},
}

type sortColumn struct {
	Label   string
	URL     string // "" if the column cannot be sorted by
	Dir     string // "ascending" or "descending" if it is the sort column
	Numeric bool
}

type fleetRow struct {
	apiclient.Member
	ID string
	// Behind is true for a connected Envoy that has accepted a config, but
	// not the one now served.
	Behind bool
}

type fleetView struct {
	apiclient.Fleet
	Filter   string
	Rows     []fleetRow
	Columns  []sortColumn
	States   []link
	Links    []link // the connected / disconnected filter
	Sizes    []link
	Hidden   [][2]string // parameters the search form must carry along
	From, To int
	Prev     string
	Next     string
	First    string // set when the page is past the end of the list
	Filtered bool
	Clear    string // the page with no filters
}

func (u *UI) buildFleet(ctx context.Context, v *view) error {
	fv := &fleetView{Filter: v.query.Get("q")}
	v.Fleet = fv
	q := apiclient.FleetQuery{
		Limit: defaultFleetPage, Sort: v.query.Get("sort"), Desc: v.query.Get("order") == "desc",
		Filter: fv.Filter, State: v.query.Get("state"), Connected: v.query.Get("connected"),
	}
	q.Offset, _ = strconv.Atoi(v.query.Get("offset"))
	if n, err := strconv.Atoi(v.query.Get("limit")); err == nil {
		q.Limit = n
	}
	sortKey := q.Sort
	if sortKey == "" {
		sortKey = "node_id"
	}

	// Everything that changes the list starts again at its first page.
	change := func(set func(url.Values)) string {
		return v.url(func(q url.Values) { q.Del("offset"); set(q) })
	}
	option := func(param, value string) func(url.Values) {
		return func(q url.Values) {
			q.Del(param)
			if value != "" {
				q.Set(param, value)
			}
		}
	}
	for _, c := range fleetColumns {
		col := sortColumn{Label: c.label, Numeric: c.numeric}
		if c.key != "" {
			desc := c.down
			if c.key == sortKey {
				desc = !q.Desc
				col.Dir = map[bool]string{false: "ascending", true: "descending"}[q.Desc]
			}
			col.URL = change(func(q url.Values) {
				q.Del("sort")
				q.Del("order")
				if c.key != "node_id" {
					q.Set("sort", c.key)
				}
				if desc {
					q.Set("order", "desc")
				}
			})
		}
		fv.Columns = append(fv.Columns, col)
	}
	for _, s := range [][2]string{{"Any config", ""}, {"Accepted", "acked"}, {"Pending", "pending"}, {"Rejected", "nacked"}} {
		fv.States = append(fv.States, link{Label: s[0], Active: q.State == s[1], URL: change(option("state", s[1]))})
	}
	for _, s := range [][2]string{{"All", ""}, {"Connected", "true"}, {"Disconnected", "false"}} {
		fv.Links = append(fv.Links, link{Label: s[0], Active: q.Connected == s[1], URL: change(option("connected", s[1]))})
	}
	for _, size := range fleetPageSizes {
		value := strconv.Itoa(size)
		if size == defaultFleetPage {
			value = ""
		}
		fv.Sizes = append(fv.Sizes, link{Label: strconv.Itoa(size), Active: q.Limit == size, URL: change(option("limit", value))})
	}
	for _, k := range []string{"window", "state", "connected", "sort", "order", "limit"} {
		if val := v.query.Get(k); val != "" {
			fv.Hidden = append(fv.Hidden, [2]string{k, val})
		}
	}
	fv.Filtered = q.Filter != "" || q.State != "" || q.Connected != ""
	fv.Clear = change(func(q url.Values) { q.Del("q"); q.Del("state"); q.Del("connected") })

	res, err := u.api.Fleet(ctx, v.Window, q)
	if err != nil {
		return err
	}
	fv.Fleet = *res
	for _, m := range res.Members {
		fv.Rows = append(fv.Rows, fleetRow{
			Member: m, ID: domID("envoy-", m.NodeID),
			Behind: m.Connected && m.ConfigState == "acked" && m.ConfigVersion != res.ConfigVersion,
		})
	}
	page := func(offset int) string {
		return v.url(func(q url.Values) {
			q.Del("offset")
			if offset > 0 {
				q.Set("offset", strconv.Itoa(offset))
			}
		})
	}
	if len(fv.Rows) > 0 {
		fv.From, fv.To = res.Offset+1, res.Offset+len(fv.Rows)
	}
	if res.Offset > 0 {
		fv.Prev = page(max(0, res.Offset-q.Limit))
		if len(fv.Rows) == 0 {
			fv.First = page(0)
		}
	}
	if res.Offset+len(fv.Rows) < res.Matched {
		fv.Next = page(res.Offset + q.Limit)
	}
	return nil
}

// ---- Controls ----

type controlsCluster struct {
	apiclient.Cluster
	Pools   []apiclient.Pool
	Targets int // targets that use it; it cannot be detached while any do
}

// ruleForm is the add-a-rule form's fields as they were typed.
type ruleForm struct {
	Name, Operation, AccessKey, Bucket, Prefix string
	Limit, Unit, Burst, Respond, Note          string
	Blocked                                    bool
	Per                                        map[string]bool
}

type controlsView struct {
	Settings apiclient.Settings
	Targets  []apiclient.TargetInfo
	Clusters []controlsCluster
	CNodes   []apiclient.CNode
	Rules    []apiclient.RuleStatus
	NewRule  ruleForm
	// What a failed form held, so it need not be typed again. A password is
	// never among it.
	NewCluster apiclient.NewCluster
	NewTarget  apiclient.NewTarget
	OpenForm   string // "cluster" or "target": the form to show open
}

func (u *UI) buildControls(ctx context.Context, v *view) error {
	cv := &controlsView{}
	v.Controls = cv
	var first error
	keep := func(err error) {
		if err != nil && first == nil {
			first = err
		}
	}
	settings, err := u.api.Settings(ctx)
	keep(err)
	if settings != nil {
		cv.Settings = *settings
	}
	cv.Targets, err = u.api.Targets(ctx)
	keep(err)
	clusters, err := u.api.Clusters(ctx)
	keep(err)
	uses := map[string]int{}
	for _, t := range cv.Targets {
		if t.Cluster != "" {
			uses[t.Cluster]++
		}
	}
	for _, c := range clusters {
		cc := controlsCluster{Cluster: c, Targets: uses[c.Name]}
		if c.LastPoll != nil {
			// Pools are only for the hints in the add-target form; a
			// cluster that cannot list them just offers none.
			cc.Pools, _ = u.api.Pools(ctx, c.Name)
		}
		cv.Clusters = append(cv.Clusters, cc)
	}
	if res, err := u.api.CNodes(ctx, "1m"); err != nil {
		keep(err)
	} else {
		cv.CNodes = res.CNodes
	}
	cv.Rules, err = u.api.Rules(ctx)
	keep(err)
	cv.NewRule = ruleForm{Unit: "second", Respond: "503", Per: map[string]bool{}}
	return first
}

// ---- Template functions ----

var icons = map[string]string{
	"good":    `<circle class="ico-fill" cx="8" cy="8" r="7"/><path class="ico-cut" d="M4.8 8.3l2.2 2.2 4.2-4.6"/>`,
	"warn":    `<path class="ico-fill ico-round" d="M8 2.2l6.3 11.3H1.7z"/><path class="ico-mark" d="M8 6.4v3.2"/><circle class="ico-dot" cx="8" cy="11.6" r=".9"/>`,
	"crit":    `<circle class="ico-fill" cx="8" cy="8" r="7"/><path class="ico-cut" d="M5.5 5.5l5 5M10.5 5.5l-5 5"/>`,
	"paused":  `<circle class="ico-fill" cx="8" cy="8" r="7"/><path class="ico-cut" d="M6.3 5.4v5.2M9.7 5.4v5.2"/>`,
	"pending": `<circle class="ico-ring" cx="8" cy="8" r="6.2"/><path class="ico-line" d="M8 4.8V8l2.1 1.5"/>`,
	"idle":    `<circle class="ico-ring" cx="8" cy="8" r="6.2"/>`,
}

// badge is a state shown as an icon and a word, never as a color alone.
// kind is one of the keys of icons.
func badge(kind, label string) template.HTML {
	shape, ok := icons[kind]
	if !ok {
		kind, shape = "idle", icons["idle"]
	}
	return template.HTML(fmt.Sprintf( //nolint:gosec // fixed markup and an escaped label
		`<span class="badge"><svg class="ico ico-%s" viewBox="0 0 16 16" aria-hidden="true">%s</svg>%s</span>`,
		kind, shape, template.HTMLEscapeString(label)))
}

// cnodeState names a CNode's state for a badge.
func cnodeState(c apiclient.CNode) template.HTML {
	switch {
	case c.Drained:
		return badge("paused", "Drained")
	case c.Healthy:
		return badge("good", "Active")
	case !c.Enabled:
		return badge("warn", "Disabled")
	}
	state := c.State
	if state == "" {
		state = "Unknown"
	}
	return badge("crit", state)
}

// memberState names an Envoy's config state for a badge.
func memberState(m apiclient.Member) template.HTML {
	switch {
	case !m.Connected:
		return badge("idle", "Disconnected")
	case m.ConfigState == "acked":
		return badge("good", "Accepted")
	case m.ConfigState == "nacked":
		return badge("crit", "Rejected")
	}
	return badge("pending", "Pending")
}

// partial is the htmx half of a link that changes the page's parameters:
// fetch the page, replace its main part and keep the address bar in step.
// The link's own href does the same without script.
func partial(href string) template.HTMLAttr {
	return template.HTMLAttr(fmt.Sprintf( //nolint:gosec // the address is escaped
		`hx-get="%s" hx-target="#page" hx-select="#page" hx-swap="outerHTML" hx-push-url="true"`,
		template.HTMLEscapeString(href)))
}

// submit is the htmx half of a form that changes something: post it and
// replace the page's main part with the answer.
func submit(action string) template.HTMLAttr {
	return template.HTMLAttr(fmt.Sprintf( //nolint:gosec // the address is escaped
		`hx-post="%s" hx-target="#page" hx-select="#page" hx-swap="outerHTML" hx-disable="find button"`,
		template.HTMLEscapeString(action)))
}

func (u *UI) funcs() template.FuncMap {
	return template.FuncMap{
		"count":       count,
		"rate":        rate,
		"mbps":        mbps,
		"ms":          millis,
		"share":       share,
		"ago":         ago,
		"spark":       sparkline,
		"meter":       meter,
		"badge":       badge,
		"cnodeState":  cnodeState,
		"memberState": memberState,
		"partial":     partial,
		"submit":      submit,
		"sum":         func(a, b float64) float64 { return a + b },
		"deref":       func(t *time.Time) time.Time { return *t },
		"less1":       func(n int) int { return max(n, 1) - 1 },
		"join":        strings.Join,
		"holdNote":    func(r apiclient.RuleStatus) string { return r.Rule.HoldNote(r.Status) },
		"first": func(n int, keys []ratelimit.KeyStatus) []ratelimit.KeyStatus {
			if len(keys) > n {
				return keys[:n]
			}
			return keys
		},
		"percent": func(f float64) string { return strconv.FormatFloat(f*100, 'f', -1, 64) + "%" },
		"seconds": func(s float64) string {
			if s >= 120 && s == float64(int(s/60))*60 {
				return fmt.Sprintf("%d minutes", int(s/60))
			}
			if s == 60 {
				return "a minute"
			}
			return strconv.FormatFloat(s, 'f', -1, 64) + " seconds"
		},
	}
}
