package server

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

const testToken = "test-token-4f9c2b7e1a6d"

// browser is an HTTP client that keeps no cookies and follows no redirects,
// so a test sees each answer as it was given.
var browser = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// request makes one request with the given headers ("Name: value").
func request(t *testing.T, s *Server, method, path string, form url.Values, headers ...string) (*http.Response, string) {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, "http://"+s.HTTPAddr()+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, h := range headers {
		name, value, _ := strings.Cut(h, ": ")
		if name == "Host" {
			req.Host = value
		} else {
			req.Header.Set(name, value)
		}
	}
	resp, err := browser.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func tokenDaemon(t *testing.T) *Server {
	t.Helper()
	return start(t, Config{GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0", APIToken: testToken})
}

// login signs in through the form and returns the session cookie header.
func login(t *testing.T, s *Server) string {
	t.Helper()
	resp, body := request(t, s, "POST", "/login", url.Values{"token": {testToken}, "next": {"/fleet?state=nacked"}}, "Origin: http://"+s.HTTPAddr())
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/fleet?state=nacked" {
		t.Fatalf("login: status %d, Location %q, %s", resp.StatusCode, resp.Header.Get("Location"), body)
	}
	cookies := resp.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("login set %d cookies", len(cookies))
	}
	c := cookies[0]
	if c.Name != sessionCookie || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.MaxAge != int(sessionLife.Seconds()) || c.Secure {
		t.Errorf("session cookie: %+v", c)
	}
	if strings.Contains(c.Value, testToken) {
		t.Error("the session cookie holds the token itself")
	}
	return "Cookie: " + c.Name + "=" + c.Value
}

func TestWithoutCredentials(t *testing.T) {
	s := tokenDaemon(t)
	html := "Accept: text/html,application/xhtml+xml"

	// A person opening a page is sent to the login form, and back afterwards.
	for path, next := range map[string]string{"/": "%2F", "/fleet?state=nacked&q=a+b": "%2Ffleet%3Fstate%3Dnacked%26q%3Da%2Bb", "/controls": "%2Fcontrols"} {
		resp, _ := request(t, s, "GET", path, nil, html)
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login?next="+next {
			t.Errorf("GET %s: status %d, Location %q", path, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	// Everything else is refused outright, pages included when it is not a
	// browser asking.
	for _, c := range []struct{ method, path, header string }{
		{"GET", "/", "Accept: */*"}, {"GET", "/api/v1/overview", html}, {"GET", "/api/v1/vast", html}, {"GET", "/metrics", html},
		{"GET", "/live/overview", html}, {"POST", "/controls/drain", html}, {"POST", "/api/v1/vast", html}, {"POST", "/logout", html},
	} {
		resp, body := request(t, s, c.method, c.path, nil, c.header)
		if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") == "" || strings.TrimSpace(body) != "unauthorized" {
			t.Errorf("%s %s: status %d, body %q", c.method, c.path, resp.StatusCode, body)
		}
	}
	// A request from a page that is already open (its session ran out) gets
	// the header that makes htmx load the login form.
	resp, _ := request(t, s, "GET", "/cnodes", nil, html, "HX-Request: true", "HX-Current-URL: http://"+s.HTTPAddr()+"/cnodes?window=5m")
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("HX-Redirect") != "/login?next=%2Fcnodes%3Fwindow%3D5m" {
		t.Errorf("htmx request: status %d, HX-Redirect %q", resp.StatusCode, resp.Header.Get("HX-Redirect"))
	}

	// Open to all: liveness, the login form and what it needs to look right.
	for _, path := range []string{"/healthz", "/login", "/login?next=%2Ffleet", "/static/app.css", "/static/app.js"} {
		if resp, _ := request(t, s, "GET", path, nil, html); resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: status %d", path, resp.StatusCode)
		}
	}
	_, form := request(t, s, "GET", "/login?next=%2Ffleet%3Fstate%3Dnacked", nil, html)
	mustContain(t, "login form", form, `<form method="post" action="/login"`, `name="token"`, `type="password"`, `name="next" value="/fleet?state=nacked"`, "/static/app.css?v=")
	if err := wellFormed(form); err != nil {
		t.Errorf("login form: %v", err)
	}
	if strings.Contains(form, testToken) {
		t.Error("the login form shows the token")
	}
}

func TestLoginSession(t *testing.T) {
	s := tokenDaemon(t)
	origin := "Origin: http://" + s.HTTPAddr()

	// A wrong token: the form again, no cookie.
	started := time.Now()
	resp, body := request(t, s, "POST", "/login", url.Values{"token": {"not-it"}, "next": {"/fleet"}}, origin)
	if resp.StatusCode != http.StatusUnauthorized || len(resp.Cookies()) != 0 || !strings.Contains(body, "That is not this daemon's token") || !strings.Contains(body, `name="next" value="/fleet"`) {
		t.Errorf("wrong token: status %d, cookies %v", resp.StatusCode, resp.Cookies())
	}
	if time.Since(started) < 250*time.Millisecond {
		t.Error("a wrong token was refused without the pause that slows guessing")
	}

	cookie := login(t, s)

	// The cookie opens the pages, the API and the live streams.
	resp, body = request(t, s, "GET", "/controls", nil, cookie, "Accept: text/html")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Sign out") || !strings.Contains(body, `action="/logout"`) {
		t.Errorf("page with a session: status %d", resp.StatusCode)
	}
	if resp, _ := request(t, s, "GET", "/api/v1/overview", nil, cookie); resp.StatusCode != http.StatusOK {
		t.Errorf("API with a session: status %d", resp.StatusCode)
	}
	if resp, body := request(t, s, "GET", "/api/v1/settings", nil, cookie); resp.StatusCode != http.StatusOK || !strings.Contains(body, `"token_required": true`) {
		t.Errorf("settings: status %d, %s", resp.StatusCode, body)
	}
	// The name the browser used does not matter once there is a token.
	if resp, _ := request(t, s, "GET", "/", nil, cookie, "Host: deltav.example.com"); resp.StatusCode != http.StatusOK {
		t.Errorf("another host name, with a session: status %d", resp.StatusCode)
	}

	// A form from this site goes through; the same form from another site
	// does not, although the browser would attach the cookie.
	drain := url.Values{"cluster": {"lab"}, "cnode": {"cnode-1"}, "drained": {"true"}}
	if resp, _ := request(t, s, "POST", "/controls/drain", drain, cookie, origin, "Sec-Fetch-Site: same-origin"); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("own form: status %d, want 422 (there is no such cluster)", resp.StatusCode)
	}
	for _, from := range [][]string{
		{"Origin: http://evil.example"}, {"Origin: null"}, {"Sec-Fetch-Site: cross-site"}, {"Sec-Fetch-Site: same-site"},
		{origin, "Sec-Fetch-Site: cross-site"}, {"Origin: http://" + s.HTTPAddr() + ".evil.example"},
	} {
		for _, c := range []struct{ method, path string }{{"POST", "/controls/drain"}, {"POST", "/api/v1/vast/lab/cnodes/cnode-1/drain"}, {"DELETE", "/api/v1/targets/s3"}, {"PUT", "/api/v1/targets/s3/limit"}, {"POST", "/logout"}} {
			if resp, _ := request(t, s, c.method, c.path, drain, append([]string{cookie}, from...)...); resp.StatusCode != http.StatusForbidden {
				t.Errorf("%s %s with %v: status %d, want 403", c.method, c.path, from, resp.StatusCode)
			}
		}
	}
	// Reading is not restricted by origin: the browser already keeps another
	// site from seeing the answer.
	if resp, _ := request(t, s, "GET", "/api/v1/overview", nil, cookie, "Origin: http://evil.example"); resp.StatusCode != http.StatusOK {
		t.Errorf("cross-origin GET: status %d", resp.StatusCode)
	}

	// The bearer token still works on its own, with no origin to check.
	if resp, _ := request(t, s, "DELETE", "/api/v1/targets/nope", nil, "Authorization: Bearer "+testToken); resp.StatusCode != http.StatusNotFound {
		t.Errorf("bearer DELETE: status %d", resp.StatusCode)
	}

	// Sessions that must not work: altered, issued for another token, expired.
	g := s.http.Handler.(*guard)
	name, value, _ := strings.Cut(strings.TrimPrefix(cookie, "Cookie: "), "=")
	exp, mac, _ := strings.Cut(value, ".")
	later := "9" + exp[1:]
	for what, v := range map[string]string{
		"a later expiry with the old MAC": later + "." + mac,
		"a MAC with one digit changed":    exp + "." + flip(mac),
		"another token's session":         newGuard("some-other-token").newSession(time.Now().Add(time.Hour)),
		"no MAC":                          exp, "empty": "", "the token itself": testToken,
	} {
		if resp, _ := request(t, s, "GET", "/api/v1/overview", nil, "Cookie: "+name+"="+v); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status %d", what, resp.StatusCode)
		}
	}
	g.now = func() time.Time { return time.Now().Add(sessionLife + time.Minute) }
	if resp, _ := request(t, s, "GET", "/api/v1/overview", nil, cookie); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("an expired session: status %d", resp.StatusCode)
	}
	g.now = time.Now

	// Signing out takes the cookie away.
	resp, _ = request(t, s, "POST", "/logout", nil, cookie, origin)
	if c := resp.Cookies(); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" || len(c) != 1 || c[0].MaxAge >= 0 || c[0].Value != "" {
		t.Errorf("logout: status %d, cookies %+v", resp.StatusCode, c)
	}

	// Behind a proxy that adds TLS the cookie is kept to TLS, and the form
	// cannot be used to send anyone to another site.
	resp, _ = request(t, s, "POST", "/login", url.Values{"token": {testToken}, "next": {"//evil.example/"}}, origin, "X-Forwarded-Proto: https")
	if c := resp.Cookies(); resp.Header.Get("Location") != "/" || len(c) != 1 || !c[0].Secure {
		t.Errorf("login behind TLS: Location %q, cookies %+v", resp.Header.Get("Location"), c)
	}
}

func flip(s string) string {
	b := []byte(s)
	if b[0] == '0' {
		b[0] = '1'
	} else {
		b[0] = '0'
	}
	return string(b)
}

// Without a token the daemon is on loopback, and answers only to loopback
// names: a page that points its own name at 127.0.0.1 is turned away.
func TestLoopbackWithoutToken(t *testing.T) {
	s := start(t, Config{GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0"})
	_, port, _ := strings.Cut(s.HTTPAddr(), ":")
	for host, want := range map[string]int{
		"127.0.0.1:" + port: http.StatusOK, "localhost:" + port: http.StatusOK, "[::1]:" + port: http.StatusOK, "localhost": http.StatusOK,
		"evil.example:" + port: http.StatusForbidden, "evil.example": http.StatusForbidden, "127.0.0.1.evil.example:" + port: http.StatusForbidden,
		"10.1.2.3:" + port: http.StatusForbidden,
	} {
		for _, path := range []string{"/", "/api/v1/overview", "/healthz"} {
			resp, body := request(t, s, "GET", path, nil, "Host: "+host)
			if resp.StatusCode != want {
				t.Errorf("GET %s with Host %s: status %d, want %d", path, host, resp.StatusCode, want)
			}
			if want == http.StatusForbidden && !strings.Contains(body, "DELTAV_API_TOKEN") {
				t.Errorf("the refusal should say what to do: %q", body)
			}
		}
	}
	// No token: no login form and no sign-out button, and another site's
	// page still cannot change anything.
	resp, body := request(t, s, "GET", "/controls", nil)
	if resp.StatusCode != http.StatusOK || strings.Contains(body, "Sign out") {
		t.Errorf("controls without a token: status %d", resp.StatusCode)
	}
	if resp, _ := request(t, s, "GET", "/login", nil); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Errorf("login without a token: status %d", resp.StatusCode)
	}
	if resp, _ := request(t, s, "POST", "/api/v1/vast/lab/cnodes/cnode-1/drain", nil, "Origin: http://evil.example"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site POST without a token: status %d", resp.StatusCode)
	}
	if resp, _ := request(t, s, "POST", "/api/v1/vast/lab/cnodes/cnode-1/drain", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("the CLI's POST (no Origin): status %d, want 404 for the unknown cluster", resp.StatusCode)
	}
}
