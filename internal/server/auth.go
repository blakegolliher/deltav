package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// isLoopback reports whether a listen address binds only to loopback.
// Wildcards ("", "0.0.0.0", "::") and hostnames other than "localhost" are
// treated as non-loopback, so they require a token.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	return isLoopbackHost(host)
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

const (
	sessionCookie = "deltav_session"
	sessionLife   = 12 * time.Hour
)

// guard is the HTTP listener's access control. It does three things.
//
// A token, when the daemon has one, is required of every request, as an
// "Authorization: Bearer" header or as the session cookie the login form
// gives a browser (a browser's EventSource and page loads cannot send the
// header). The cookie does not hold the token: it holds an expiry time and
// a MAC of it keyed with the token, so it stops working at that time or
// when the token changes, and the daemon keeps no session state.
//
// Requests that change something must come from the daemon's own pages if
// they come from a browser at all. A browser attaches the session cookie,
// and reaches a loopback port, on behalf of any page the operator has open;
// the Origin check keeps another site's page from draining a CNode. Clients
// that are not browsers send neither header and are not affected.
//
// Without a token the daemon listens on loopback only, and then the Host
// header must name loopback too. Otherwise a hostile page could point its
// own name at 127.0.0.1 (DNS rebinding) and be, to the browser, the same
// origin as the daemon.
type guard struct {
	token string
	// open paths need no token: liveness, the login form and the files
	// the login form needs.
	open func(path string) bool
	next http.Handler
	now  func() time.Time
}

func newGuard(token string) *guard {
	return &guard{
		token: token, now: time.Now,
		open: func(p string) bool {
			return p == "/healthz" || p == "/login" || strings.HasPrefix(p, "/static/")
		},
	}
}

func (g *guard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if g.token == "" && !loopbackHostHeader(r.Host) {
		http.Error(w, "this daemon has no API token, so it answers only to loopback names such as 127.0.0.1 and localhost; set DELTAV_API_TOKEN to serve other names", http.StatusForbidden)
		return
	}
	if !safeMethod(r.Method) && !sameOrigin(r) {
		http.Error(w, "a request that changes something must come from this site", http.StatusForbidden)
		return
	}
	if g.token == "" || g.open(r.URL.Path) || g.authenticated(r) {
		g.next.ServeHTTP(w, r)
		return
	}
	// A person opening a page is sent to the login form; anything else
	// gets a plain 401. A request htmx makes from an open page (the session
	// ran out while it was open) gets the 401 with the header that makes
	// htmx load the login form: a redirect would be followed quietly and
	// the form swapped into the page.
	switch {
	case r.Header.Get("HX-Request") == "true":
		next := "/"
		if u, err := url.Parse(r.Header.Get("HX-Current-URL")); err == nil && strings.HasPrefix(u.Path, "/") {
			next = u.RequestURI()
		}
		w.Header().Set("HX-Redirect", "/login?next="+url.QueryEscape(next))
	case r.Method == http.MethodGet && isPage(r.URL.Path) && strings.Contains(r.Header.Get("Accept"), "text/html"):
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
		return
	}
	w.Header().Set("WWW-Authenticate", `Bearer realm="deltav"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// isPage reports whether a path is one a person would open in a browser.
func isPage(p string) bool {
	return !strings.HasPrefix(p, "/api/") && !strings.HasPrefix(p, "/live/") && p != "/metrics"
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// loopbackHostHeader reports whether a Host header names loopback.
func loopbackHostHeader(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	return isLoopbackHost(strings.Trim(host, "[]"))
}

// sameOrigin reports whether a request did not come from another site's
// page. Browsers say where a request comes from in Sec-Fetch-Site and, on
// every cross-origin request that is not a GET, in Origin.
func sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host != "" && strings.EqualFold(u.Host, r.Host)
}

func (g *guard) authenticated(r *http.Request) bool {
	got := []byte(strings.TrimSpace(r.Header.Get("Authorization")))
	if subtle.ConstantTimeCompare(got, []byte("Bearer "+g.token)) == 1 {
		return true
	}
	c, err := r.Cookie(sessionCookie)
	return err == nil && g.validSession(c.Value)
}

// A session is "<expiry as Unix seconds>.<hex MAC of that, keyed with the
// token>".
func (g *guard) newSession(expires time.Time) string {
	exp := strconv.FormatInt(expires.Unix(), 10)
	return exp + "." + g.mac(exp)
}

func (g *guard) mac(exp string) string {
	m := hmac.New(sha256.New, []byte(g.token))
	m.Write([]byte("deltav web session v1 " + exp))
	return hex.EncodeToString(m.Sum(nil))
}

func (g *guard) validSession(v string) bool {
	exp, mac, ok := strings.Cut(v, ".")
	if !ok || !hmac.Equal([]byte(mac), []byte(g.mac(exp))) {
		return false
	}
	at, err := strconv.ParseInt(exp, 10, 64)
	return err == nil && g.now().Before(time.Unix(at, 0))
}

// Login implements webui.Session: it checks a token typed into the login
// form and, if it is the daemon's, sets the session cookie.
func (g *guard) Login(w http.ResponseWriter, r *http.Request, token string) bool {
	if g.token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(g.token)) != 1 {
		// Slows guessing a little. The token should be long and random:
		// that, not this pause, is what makes guessing hopeless.
		time.Sleep(300 * time.Millisecond)
		return false
	}
	expires := g.now().Add(sessionLife)
	http.SetCookie(w, g.cookie(r, g.newSession(expires), int(sessionLife.Seconds())))
	return true
}

// Logout implements webui.Session.
func (g *guard) Logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, g.cookie(r, "", -1))
}

func (g *guard) cookie(r *http.Request, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name: sessionCookie, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		// The daemon speaks plain HTTP; behind a proxy that adds TLS the
		// cookie is kept to TLS.
		Secure: r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"),
	}
}
