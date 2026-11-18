// Package webui is the web UI the daemon serves: four pages rendered on the
// server from Go templates, kept current over server-sent events with htmx.
//
// It is a client of the HTTP API like the CLI and the terminal UI. It is
// handed the API's handler and calls it in process through package
// apiclient; it has no other way to reach daemon state, so a page cannot
// show or change anything the API does not offer.
//
// The stylesheet static/app.css is generated from tailwind.css and the
// templates by `make css` and is committed, so that building the binary
// needs neither Node nor the Tailwind CLI. static/htmx.min.js and
// static/hx-sse.min.js are htmx 4.0.1 and its SSE extension, unmodified
// (0BSD; see THIRD_PARTY_LICENSES.md).
package webui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"deltav/internal/apiclient"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/app.css static/app.js static/htmx.min.js static/hx-sse.min.js
var staticFS embed.FS

// DefaultInterval is how often an open page is brought up to date.
const DefaultInterval = 2 * time.Second

// Session is the daemon's token check, as far as the login page needs it.
type Session interface {
	// Login checks a token and, if it is the daemon's, gives the browser
	// its session cookie.
	Login(w http.ResponseWriter, r *http.Request, token string) bool
	// Logout takes the cookie away again.
	Logout(w http.ResponseWriter, r *http.Request)
}

// Options configure the UI.
type Options struct {
	// API is the daemon's /api/v1 handler: the UI's only source of data and
	// its only way to change anything.
	API http.Handler
	// Session is set when the daemon requires a token; the UI then has a
	// login page and a sign-out button.
	Session Session
	// Interval is how often an open page is brought up to date; zero means
	// DefaultInterval.
	Interval time.Duration
	// Done ends the live streams, so that a daemon shutting down is not
	// kept waiting by open pages.
	Done   <-chan struct{}
	Logger *slog.Logger
}

// UI serves the pages.
type UI struct {
	api      *apiclient.Client
	session  Session
	interval time.Duration
	done     <-chan struct{}
	log      *slog.Logger
	tmpl     *template.Template
	assets   string // changes whenever a static file does
	mux      *http.ServeMux
	now      func() time.Time
}

// New parses the templates and builds the routes.
func New(o Options) (*UI, error) {
	u := &UI{
		api: apiclient.InProcess(o.API), session: o.Session, interval: o.Interval,
		done: o.Done, log: o.Logger, now: time.Now,
	}
	if u.interval <= 0 {
		u.interval = DefaultInterval
	}
	if u.log == nil {
		u.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	tmpl, err := template.New("ui").Funcs(u.funcs()).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("web UI templates: %w", err)
	}
	u.tmpl = tmpl

	// A hash of the static files goes into their URLs, so a browser may
	// cache them for good and still gets new ones after an upgrade.
	h := sha256.New()
	err = fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := staticFS.ReadFile(p)
		h.Write([]byte(p))
		h.Write(b)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("web UI assets: %w", err)
	}
	u.assets = hex.EncodeToString(h.Sum(nil))[:12]

	mux := http.NewServeMux()
	for _, p := range pages {
		mux.HandleFunc("GET "+p.path, u.page(p))
	}
	mux.HandleFunc("GET /live/{page}", u.live)
	mux.HandleFunc("GET /static/{file}", u.static)
	mux.HandleFunc("GET /login", u.loginPage)
	mux.HandleFunc("POST /login", u.login)
	mux.HandleFunc("POST /logout", u.logout)
	mux.HandleFunc("POST /controls/limit", u.action(u.setLimit))
	mux.HandleFunc("POST /controls/limit/clear", u.action(u.clearLimit))
	mux.HandleFunc("POST /controls/drain", u.action(u.setDrained))
	mux.HandleFunc("POST /controls/clusters", u.action(u.addCluster))
	mux.HandleFunc("POST /controls/clusters/remove", u.action(u.removeCluster))
	mux.HandleFunc("POST /controls/targets", u.action(u.addTarget))
	mux.HandleFunc("POST /controls/targets/remove", u.action(u.removeTarget))
	mux.HandleFunc("POST /controls/rules", u.action(u.setRule))
	mux.HandleFunc("POST /controls/rules/remove", u.action(u.removeRule))
	u.mux = mux
	return u, nil
}

// Handler serves the UI. Every response forbids framing, inline script and
// style, and anything loaded from another origin: the pages show names that
// VAST clusters and Envoys supply, and although the templates escape them,
// a second line of defence costs nothing here.
func (u *UI) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		u.mux.ServeHTTP(w, r)
	})
}

// pageSpec is one of the four pages.
type pageSpec struct {
	name, path, title string
	windowed          bool // has the 1m / 5m / 1h selector
	build             func(u *UI, ctx context.Context, v *view) error
}

var pages = []pageSpec{
	{name: "overview", path: "/{$}", title: "Overview", windowed: true, build: (*UI).buildOverview},
	{name: "cnodes", path: "/cnodes", title: "CNodes", windowed: true, build: (*UI).buildCNodes},
	{name: "fleet", path: "/fleet", title: "Fleet", windowed: true, build: (*UI).buildFleet},
	{name: "controls", path: "/controls", title: "Controls", build: (*UI).buildControls},
}

func (p pageSpec) href() string { return strings.TrimSuffix(p.path, "{$}") }

type link struct {
	Label  string
	URL    string
	Active bool
}

type flash struct {
	OK   bool
	Text string
}

// view is everything a page's templates are given.
type view struct {
	Page, Title string
	Nav         []link
	Windows     []link
	Window      string
	LiveURL     string
	Assets      string
	SignOut     bool
	Flash       *flash
	// Err is set when the daemon's API could not be read; the page then
	// shows that and whatever it did get.
	Err string
	Now time.Time

	Overview *overviewView
	CNodes   *cnodesView
	Fleet    *fleetView
	Controls *controlsView

	spec  pageSpec
	query url.Values // the page's own parameters, defaults left out
}

// url returns the page's address with its parameters changed by set.
func (v *view) url(set func(url.Values)) string {
	q := url.Values{}
	for k, vals := range v.query {
		q[k] = append([]string(nil), vals...)
	}
	if set != nil {
		set(q)
	}
	return withQuery(v.spec.href(), q)
}

func withQuery(p string, q url.Values) string {
	if enc := q.Encode(); enc != "" {
		return p + "?" + enc
	}
	return p
}

var windowNames = []string{"1m", "5m", "1h"}

// newView reads a page's parameters from the request's query, keeping only
// those the page knows, so a link built from them cannot carry anything else.
func (u *UI) newView(spec pageSpec, in url.Values) *view {
	v := &view{
		Page: spec.name, Title: spec.title, Assets: u.assets, SignOut: u.session != nil,
		Now: u.now(), spec: spec, query: url.Values{}, Window: "1m",
	}
	if spec.windowed {
		for _, w := range windowNames {
			if in.Get("window") == w {
				v.Window = w
			}
		}
		if v.Window != "1m" {
			v.query.Set("window", v.Window)
		}
	}
	for _, p := range pages {
		l := link{Label: p.title, URL: p.href(), Active: p.name == spec.name}
		// Moving between the windowed pages keeps the window.
		if p.windowed && v.Window != "1m" {
			l.URL = withQuery(l.URL, url.Values{"window": {v.Window}})
		}
		v.Nav = append(v.Nav, l)
	}
	return v
}

// finish fills in the links that depend on the page's final parameters.
func (v *view) finish() {
	if v.spec.windowed {
		for _, w := range windowNames {
			v.Windows = append(v.Windows, link{Label: w, Active: w == v.Window, URL: v.url(func(q url.Values) {
				q.Del("window")
				if w != "1m" {
					q.Set("window", w)
				}
				q.Del("offset") // a different window can reorder the fleet
			})})
		}
	}
	v.LiveURL = withQuery("/live/"+v.spec.name, v.query)
}

// build makes a page's view. A failure to read the API is shown on the page
// and does not stop it rendering.
func (u *UI) build(ctx context.Context, spec pageSpec, in url.Values) *view {
	v := u.newView(spec, in)
	v.readParams(in)
	if err := spec.build(u, ctx, v); err != nil {
		v.Err = err.Error()
	}
	v.finish()
	return v
}

func (u *UI) page(spec pageSpec) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u.render(w, http.StatusOK, "layout", u.build(r.Context(), spec, r.URL.Query()))
	}
}

func (u *UI) render(w http.ResponseWriter, status int, name string, data any) {
	var buf bytes.Buffer
	if err := u.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		u.log.Error("web UI: rendering failed", "template", name, "error", err)
		http.Error(w, "the page could not be rendered; see the daemon's log", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// live keeps one open page current: it renders the page's changing part
// again every interval and sends it as a server-sent event, which htmx
// merges into the page. It takes the same parameters as the page itself.
func (u *UI) live(w http.ResponseWriter, r *http.Request) {
	var spec pageSpec
	for _, p := range pages {
		if p.name == r.PathValue("page") {
			spec = p
		}
	}
	flusher, ok := w.(http.Flusher)
	if spec.name == "" || !ok {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no") // ask reverse proxies not to hold events back
	w.WriteHeader(http.StatusOK)

	tick := time.NewTicker(u.interval)
	defer tick.Stop()
	var last []byte
	lastSent := time.Time{}
	for {
		var buf bytes.Buffer
		if err := u.tmpl.ExecuteTemplate(&buf, spec.name+"-live", u.build(r.Context(), spec, r.URL.Query())); err != nil {
			u.log.Error("web UI: rendering failed", "template", spec.name+"-live", "error", err)
			return
		}
		var err error
		switch {
		case !bytes.Equal(buf.Bytes(), last):
			last, lastSent = buf.Bytes(), time.Now()
			err = writeEvent(w, last)
		case time.Since(lastSent) > 15*time.Second:
			// Nothing changed: a comment keeps proxies from closing an
			// idle connection.
			lastSent = time.Now()
			_, err = io.WriteString(w, ": nothing new\n\n")
		}
		if err != nil {
			return
		}
		flusher.Flush()
		select {
		case <-tick.C:
		case <-r.Context().Done():
			return
		case <-u.done:
			return
		}
	}
}

// writeEvent sends markup as one unnamed server-sent event, a "data:" line
// for each line of it. The event stream format ends a line at CR, LF or
// CRLF. Markup can hold a bare CR that came from a name an Envoy or a VAST
// cluster supplied, and such a CR would begin a new field, with which the
// name could forge an event of its own. So every kind of line end is made
// LF before the lines are framed.
func writeEvent(w io.Writer, markup []byte) error {
	var b bytes.Buffer
	text := strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(string(markup))
	for _, line := range strings.Split(text, "\n") {
		// Indentation and blank lines mean nothing to the browser here.
		if line = strings.TrimLeft(line, " \t"); line != "" {
			b.WriteString("data: ")
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	b.WriteByte('\n')
	_, err := w.Write(b.Bytes())
	return err
}

var contentTypes = map[string]string{
	".css": "text/css; charset=utf-8",
	".js":  "text/javascript; charset=utf-8",
}

func (u *UI) static(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	b, err := staticFS.ReadFile("static/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", contentTypes[path.Ext(name)])
	if r.URL.Query().Get("v") == u.assets {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	_, _ = w.Write(b)
}

// loginView is what the login page is given.
type loginView struct {
	Assets string
	Next   string
	Failed bool
}

// localPath returns next if it is a path on this site, and "/" otherwise,
// so the login form cannot be made to send anyone elsewhere.
//
// A browser does not read an address the way a server does. It drops tabs
// and line breaks from anywhere inside it and takes a backslash for a
// slash, so "/\t/evil.example" and "/\\evil.example" both lead off the
// site as "//evil.example". Anything with a control character or a
// backslash is refused outright, and the rest must parse as a path and
// nothing more.
func localPath(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.Contains(next, "\\") {
		return "/"
	}
	for _, c := range next {
		if c < 0x20 || c == 0x7f {
			return "/"
		}
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || strings.HasPrefix(u.Path, "//") {
		return "/"
	}
	return next
}

func (u *UI) loginPage(w http.ResponseWriter, r *http.Request) {
	if u.session == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	u.render(w, http.StatusOK, "login", loginView{Assets: u.assets, Next: localPath(r.URL.Query().Get("next"))})
}

func (u *UI) login(w http.ResponseWriter, r *http.Request) {
	if u.session == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "the form could not be read", http.StatusBadRequest)
		return
	}
	next := localPath(r.PostForm.Get("next"))
	if !u.session.Login(w, r, strings.TrimSpace(r.PostForm.Get("token"))) {
		u.render(w, http.StatusUnauthorized, "login", loginView{Assets: u.assets, Next: next, Failed: true})
		return
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (u *UI) logout(w http.ResponseWriter, r *http.Request) {
	if u.session != nil {
		u.session.Logout(w, r)
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
