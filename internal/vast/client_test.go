package vast

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeVAST is a stand-in for the management API's token and GET behaviour.
// It records every request it receives, which is how the tests prove that
// a refused request never left the client.
type fakeVAST struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	requests []string // "METHOD /path"
	logins   int
	refreshs int
	valid    map[string]bool // access tokens the server accepts
	refresh  map[string]bool // refresh tokens the server accepts
	next     int
	handlers map[string]http.HandlerFunc
}

const (
	testUser     = "admin"
	testPassword = "correct horse battery staple"
)

func newFakeVAST(t *testing.T) *fakeVAST {
	f := &fakeVAST{t: t, valid: map[string]bool{}, refresh: map[string]bool{}, handlers: map[string]http.HandlerFunc{}}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeVAST) issue(prefix string) string {
	f.next++
	return prefix + "-" + string(rune('a'+f.next))
}

func (f *fakeVAST) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	reply := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	if h, ok := f.handlers[r.Method+" "+r.URL.Path]; ok {
		h(w, r)
		return
	}
	switch {
	case r.Method == http.MethodPost && (r.URL.Path == "/api/token/" || r.URL.Path == "/api/token/tenant-a"):
		var in struct{ Username, Password string }
		body, _ := io.ReadAll(r.Body)
		if json.Unmarshal(body, &in) != nil || in.Username != testUser || in.Password != testPassword {
			reply(http.StatusUnauthorized, map[string]string{"detail": "No active account found with the given credentials"})
			return
		}
		f.logins++
		access, refresh := f.issue("access"), f.issue("refresh")
		f.valid[access], f.refresh[refresh] = true, true
		reply(http.StatusOK, map[string]string{"access": access, "refresh": refresh, "csrftoken": "x"})
	case r.Method == http.MethodPost && r.URL.Path == "/api/token/refresh/":
		var in struct{ Refresh string }
		body, _ := io.ReadAll(r.Body)
		if json.Unmarshal(body, &in) != nil || !f.refresh[in.Refresh] {
			reply(http.StatusUnauthorized, map[string]string{"detail": "Token is invalid or expired"})
			return
		}
		f.refreshs++
		access := f.issue("access")
		f.valid[access] = true
		reply(http.StatusOK, map[string]string{"access": access})
	case r.Method == http.MethodGet && r.URL.Path == "/api/cnodes/":
		if !f.valid[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")] {
			reply(http.StatusUnauthorized, map[string]string{"detail": "Given token not valid for any token type"})
			return
		}
		reply(http.StatusOK, []map[string]any{{"id": 1, "name": "cnode-1"}, {"id": 2, "name": "cnode-2"}})
	default:
		reply(http.StatusNotFound, map[string]string{"detail": "Not found."})
	}
}

// expire makes the server forget every access token and, optionally, every
// refresh token, as a restart or a timeout would.
func (f *fakeVAST) expire(refreshToo bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.valid = map[string]bool{}
	if refreshToo {
		f.refresh = map[string]bool{}
	}
}

func (f *fakeVAST) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func (f *fakeVAST) client(t *testing.T, mutate func(*Options)) *Client {
	t.Helper()
	o := Options{Host: strings.TrimPrefix(f.srv.URL, "https://"), User: testUser, Password: testPassword}
	o.rootCAs = f.srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	if mutate != nil {
		mutate(&o)
	}
	c, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

type cnode struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

func TestAllowed(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		want         bool
	}{
		{"GET", "/api/cnodes/", true},
		{"GET", "/api/vips/?page=2", true},
		{"GET", "/anything", true},
		{"POST", "/api/token/", true},
		{"POST", "/api/token", false}, // not the token endpoint's path
		{"POST", "/api/token/refresh/", true},
		{"POST", "/api/token/refresh", true},
		{"POST", "/api/token/tenant-a", true},
		{"POST", "/api/token/tenant-a/", true},
		{"POST", "/api/token/tenant-a/extra", false},
		{"POST", "/api/cnodes/", false},
		{"POST", "/api/cnodes/1/control_led/", false},
		{"POST", "/api/cnodes/add_cnodes/", false},
		{"POST", "/api/tokens/", false},
		{"POST", "/token/", false},
		{"POST", "/api/v5/token/", false},
		{"PATCH", "/api/cnodes/1/", false},
		{"PUT", "/api/vippools/2/", false},
		{"DELETE", "/api/vips/7/", false},
		{"HEAD", "/api/cnodes/", false},
		{"OPTIONS", "/api/cnodes/", false},
		{"get", "/api/cnodes/", false}, // methods are case-sensitive
	} {
		path, _, _ := strings.Cut(tc.path, "?")
		if got := allowed(tc.method, path); got != tc.want {
			t.Errorf("allowed(%s %s) = %v, want %v", tc.method, tc.path, got, tc.want)
		}
	}
}

// The rule lives in the transport: a mutating request made through the
// client's own HTTP client is refused before it reaches the network.
func TestMutatingRequestsNeverLeaveTheClient(t *testing.T) {
	f := newFakeVAST(t)
	c := f.client(t, nil)
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		req, _ := http.NewRequest(method, f.srv.URL+"/api/cnodes/1/highlight/", strings.NewReader("{}"))
		resp, err := c.http.Do(req)
		if err == nil {
			resp.Body.Close()
			t.Fatalf("%s was sent", method)
		}
		if !strings.Contains(err.Error(), "only GET and token calls are allowed") {
			t.Errorf("%s: %v", method, err)
		}
	}
	if got := f.seen(); len(got) != 0 {
		t.Fatalf("the server received %v", got)
	}
}

// A redirect is a new request and passes through the same check: a token
// POST redirected (method preserved) to another path is refused.
func TestRedirectedRequestsAreChecked(t *testing.T) {
	f := newFakeVAST(t)
	f.handlers["POST /api/token/"] = func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/api/cnodes/add_cnodes/", http.StatusTemporaryRedirect)
	}
	c := f.client(t, nil)
	var out []cnode
	err := c.Get(context.Background(), "cnodes/", &out)
	if err == nil || !strings.Contains(err.Error(), "refused POST /api/cnodes/add_cnodes/") {
		t.Fatalf("got %v", err)
	}
	if got := f.seen(); len(got) != 1 || got[0] != "POST /api/token/" {
		t.Fatalf("the server received %v, want only the token call", got)
	}
}

func TestGetLogsInOnceAndReusesTheToken(t *testing.T) {
	f := newFakeVAST(t)
	c := f.client(t, nil)
	for i := 0; i < 3; i++ {
		var out []cnode
		if err := c.Get(context.Background(), "cnodes/", &out); err != nil {
			t.Fatal(err)
		}
		if len(out) != 2 || out[1].Name != "cnode-2" {
			t.Fatalf("decoded %+v", out)
		}
	}
	want := []string{"POST /api/token/", "GET /api/cnodes/", "GET /api/cnodes/", "GET /api/cnodes/"}
	if got := f.seen(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("requests: %v\n    want: %v", got, want)
	}
}

func TestExpiredTokenIsRefreshedThenRenewed(t *testing.T) {
	f := newFakeVAST(t)
	c := f.client(t, nil)
	var out []cnode
	if err := c.Get(context.Background(), "cnodes/", &out); err != nil {
		t.Fatal(err)
	}

	// The access token expires: one refresh call, then the GET again.
	f.expire(false)
	if err := c.Get(context.Background(), "cnodes/", &out); err != nil {
		t.Fatalf("after access token expiry: %v", err)
	}
	if f.logins != 1 || f.refreshs != 1 {
		t.Fatalf("logins %d, refreshes %d; want 1 and 1", f.logins, f.refreshs)
	}

	// Both tokens expire: the refresh is rejected, so log in again.
	f.expire(true)
	if err := c.Get(context.Background(), "cnodes/", &out); err != nil {
		t.Fatalf("after both tokens expired: %v", err)
	}
	if f.logins != 2 || f.refreshs != 1 {
		t.Fatalf("logins %d, refreshes %d; want 2 and 1", f.logins, f.refreshs)
	}
	got := f.seen()
	tail := strings.Join(got[len(got)-4:], ",")
	if tail != "GET /api/cnodes/,POST /api/token/refresh/,POST /api/token/,GET /api/cnodes/" {
		t.Errorf("last requests: %s", tail)
	}
}

func TestTenantLogin(t *testing.T) {
	f := newFakeVAST(t)
	c := f.client(t, func(o *Options) { o.Tenant = "tenant-a" })
	var out []cnode
	if err := c.Get(context.Background(), "cnodes/", &out); err != nil {
		t.Fatal(err)
	}
	if got := f.seen(); got[0] != "POST /api/token/tenant-a" {
		t.Fatalf("requests: %v", got)
	}
}

// Nothing the client returns may contain the password or a token.
func TestErrorsCarryNoSecrets(t *testing.T) {
	f := newFakeVAST(t)
	secrets := func(err error) {
		t.Helper()
		if err == nil {
			t.Fatal("want an error")
		}
		for _, s := range []string{testPassword, "wrong-password", "access-", "refresh-"} {
			if strings.Contains(err.Error(), s) {
				t.Errorf("error leaks %q: %v", s, err)
			}
		}
	}

	bad := f.client(t, func(o *Options) { o.Password = "wrong-password" })
	err := bad.Get(context.Background(), "cnodes/", new([]cnode))
	secrets(err)
	if !strings.Contains(err.Error(), `login as "admin" failed: status 401`) {
		t.Errorf("login failure: %v", err)
	}

	c := f.client(t, nil)
	err = c.Get(context.Background(), "nope/", new([]cnode))
	secrets(err)
	if !strings.Contains(err.Error(), "GET /api/nope/: status 404") || !strings.Contains(err.Error(), "Not found.") {
		t.Errorf("404: %v", err)
	}

	// A server that echoes the bearer token back in an error body.
	f.handlers["GET /api/echo/"] = func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad token: "+r.Header.Get("Authorization"), http.StatusInternalServerError)
	}
	err = c.Get(context.Background(), "echo/", new([]cnode))
	secrets(err)
	if !strings.Contains(err.Error(), "bad token: Bearer [token]") {
		t.Errorf("echoed token: %v", err)
	}

	// An unreachable host: the transport error must not print the URL.
	f.srv.Close()
	err = c.Get(context.Background(), "cnodes/", new([]cnode))
	secrets(err)
	if strings.Contains(err.Error(), "https://") {
		t.Errorf("transport error prints the URL: %v", err)
	}
}

func TestCertificateVerification(t *testing.T) {
	f := newFakeVAST(t)
	// Without the test CA the server's certificate is untrusted, as a
	// lab cluster's self-signed one would be: refused by default.
	strict := f.client(t, func(o *Options) { o.rootCAs = nil })
	err := strict.Get(context.Background(), "cnodes/", new([]cnode))
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("an untrusted certificate was accepted: %v", err)
	}
	if got := f.seen(); len(got) != 0 {
		t.Fatalf("credentials were sent to an unverified server: %v", got)
	}
	// The per-cluster opt-out.
	lab := f.client(t, func(o *Options) { o.rootCAs, o.Insecure = nil, true })
	if err := lab.Get(context.Background(), "cnodes/", new([]cnode)); err != nil {
		t.Fatalf("with Insecure: %v", err)
	}
}

func TestNewRejectsBadOptions(t *testing.T) {
	for _, o := range []Options{
		{User: "u", Password: "p"},
		{Host: "http://vast.example.com", User: "u", Password: "p"},
		{Host: "vast.example.com/api", User: "u", Password: "p"},
		{Host: "vast.example.com", Password: "p"},
		{Host: "vast.example.com", User: "u"},
	} {
		if _, err := New(o); err == nil {
			t.Errorf("%+v: want an error", Options{Host: o.Host, User: o.User})
		}
	}
	for _, host := range []string{"vast.example.com", "https://vast.example.com/", "vast.example.com:8443", " 192.0.2.5 "} {
		if _, err := New(Options{Host: host, User: "u", Password: "p"}); err != nil {
			t.Errorf("host %q: %v", host, err)
		}
	}
}

func TestGetRawAndBadJSON(t *testing.T) {
	f := newFakeVAST(t)
	c := f.client(t, nil)
	raw, err := c.GetRaw(context.Background(), "/cnodes/")
	if err != nil || !strings.Contains(string(raw), `"cnode-1"`) {
		t.Fatalf("GetRaw: %v %s", err, raw)
	}
	var wrong map[string]string
	if err := c.Get(context.Background(), "cnodes/", &wrong); err == nil || !strings.Contains(err.Error(), "decoding the response") {
		t.Errorf("decoding an array into a map: %v", err)
	}
}
