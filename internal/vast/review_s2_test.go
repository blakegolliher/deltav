package vast

// Review finding S2: a credential-bearing redirect can leave the configured
// VAST origin. These tests assert the correct behaviour (nothing reaches a
// destination outside the configured https origin) and run it against the
// unchanged client.
//
// Names: the client is configured for vast.example.com:<origin port>. The
// test certificate httptest uses covers *.example.com, and the test points
// the client's dialer at loopback for any *.example.com name. The redirect
// policy under test (http.Client.CheckRedirect, readOnly) is not touched.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type sinkReq struct {
	Method, Host, Path, Auth, Body string
}

func (r sinkReq) String() string {
	return fmt.Sprintf("%s %s%s Authorization=%q body=%q", r.Method, r.Host, r.Path, r.Auth, r.Body)
}

// sink is a destination outside the configured origin. It records every
// request and answers like a VAST cluster would, so the client carries on.
type sink struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []sinkReq
}

func newSink(t *testing.T, tlsOn bool) *sink {
	s := &sink{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.reqs = append(s.reqs, sinkReq{r.Method, r.Host, r.URL.Path, r.Header.Get("Authorization"), string(b)})
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/api/token/") {
			_ = json.NewEncoder(w).Encode(map[string]string{"access": "sink-access", "refresh": "sink-refresh"})
			return
		}
		_, _ = io.WriteString(w, "[]")
	})
	if tlsOn {
		s.srv = httptest.NewTLSServer(h)
	} else {
		s.srv = httptest.NewServer(h)
	}
	t.Cleanup(s.srv.Close)
	return s
}

func (s *sink) got() []sinkReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sinkReq(nil), s.reqs...)
}

func port(srv *httptest.Server) string {
	_, p, _ := net.SplitHostPort(srv.Listener.Addr().String())
	return p
}

// reviewClient builds a client for https://vast.example.com:<origin port>
// that trusts the httptest certificate and dials loopback for any
// *.example.com name.
func reviewClient(t *testing.T, origin *httptest.Server) *Client {
	t.Helper()
	o := Options{Host: "vast.example.com:" + port(origin), User: testUser, Password: testPassword}
	o.rootCAs = origin.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	c, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	tr := c.http.Transport.(readOnly).next.(*http.Transport)
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, p, err := net.SplitHostPort(addr)
		if err == nil && strings.HasSuffix(host, ".example.com") {
			addr = net.JoinHostPort("127.0.0.1", p)
		}
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	return c
}

type destination struct {
	name  string
	tls   bool
	url   func(s *sink) string // scheme://host:port, no path
	cross bool                 // a different host name than the origin's
}

var reviewDestinations = []destination{
	{"other-host-https", true, func(s *sink) string { return "https://evil.example.com:" + port(s.srv) }, true},
	{"same-host-other-port-https", true, func(s *sink) string { return "https://vast.example.com:" + port(s.srv) }, false},
	{"same-host-http-downgrade", false, func(s *sink) string { return "http://vast.example.com:" + port(s.srv) }, false},
	{"other-host-http", false, func(s *sink) string { return "http://evil.example.com:" + port(s.srv) }, true},
}

var reviewCodes = []int{301, 302, 303, 307, 308}

// describe summarises what reached the sink, for the failure message.
func describe(reqs []sinkReq, secrets ...string) string {
	var b strings.Builder
	for _, r := range reqs {
		var leaked []string
		for _, s := range secrets {
			if strings.Contains(r.Body, s) {
				leaked = append(leaked, "BODY:"+s)
			}
			if strings.Contains(r.Auth, s) {
				leaked = append(leaked, "AUTH:"+s)
			}
		}
		fmt.Fprintf(&b, "\n      %s  leaked=%v", r, leaked)
	}
	return b.String()
}

// Login: the origin answers POST /api/token/ with a redirect off-origin.
func TestReviewS2_LoginRedirectLeavesOrigin(t *testing.T) {
	for _, d := range reviewDestinations {
		for _, code := range reviewCodes {
			t.Run(fmt.Sprintf("%s/%d", d.name, code), func(t *testing.T) {
				s := newSink(t, d.tls)
				origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/api/token/" {
						http.Redirect(w, r, d.url(s)+"/api/token/", code)
						return
					}
					w.WriteHeader(http.StatusUnauthorized)
				}))
				defer origin.Close()
				c := reviewClient(t, origin)
				var out []cnode
				err := c.Get(context.Background(), "cnodes/", &out)
				if got := s.got(); len(got) != 0 {
					t.Errorf("off-origin destination received %d request(s) (Get err=%v):%s", len(got), err, describe(got, testPassword, "sink-access"))
				}
			})
		}
	}
}

// Refresh: login and the first GET go to the origin; the GET is rejected
// with 401, and the origin answers the refresh POST with a redirect.
func TestReviewS2_RefreshRedirectLeavesOrigin(t *testing.T) {
	const refreshSecret = "REFRESH-SECRET-0123456789"
	for _, d := range reviewDestinations {
		for _, code := range reviewCodes {
			t.Run(fmt.Sprintf("%s/%d", d.name, code), func(t *testing.T) {
				s := newSink(t, d.tls)
				var mu sync.Mutex
				logins := 0
				origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					defer mu.Unlock()
					w.Header().Set("Content-Type", "application/json")
					switch {
					case r.Method == http.MethodPost && r.URL.Path == "/api/token/":
						logins++
						_ = json.NewEncoder(w).Encode(map[string]string{"access": fmt.Sprintf("access-%d", logins), "refresh": refreshSecret})
					case r.URL.Path == "/api/token/refresh/":
						http.Redirect(w, r, d.url(s)+"/api/token/refresh/", code)
					case r.URL.Path == "/api/cnodes/" && r.Header.Get("Authorization") == "Bearer access-1":
						w.WriteHeader(http.StatusUnauthorized)
					default:
						_, _ = io.WriteString(w, "[]")
					}
				}))
				defer origin.Close()
				c := reviewClient(t, origin)
				var out []cnode
				err := c.Get(context.Background(), "cnodes/", &out)
				if got := s.got(); len(got) != 0 {
					t.Errorf("off-origin destination received %d request(s) (Get err=%v, access now %q):%s", len(got), err, c.access, describe(got, refreshSecret))
				}
			})
		}
	}
}

// GET: login succeeds at the origin; the inventory GET is redirected.
func TestReviewS2_GetRedirectLeavesOrigin(t *testing.T) {
	for _, d := range reviewDestinations {
		for _, code := range reviewCodes {
			t.Run(fmt.Sprintf("%s/%d", d.name, code), func(t *testing.T) {
				s := newSink(t, d.tls)
				origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					switch r.URL.Path {
					case "/api/token/":
						_ = json.NewEncoder(w).Encode(map[string]string{"access": "ACCESS-SECRET", "refresh": "r"})
					case "/api/cnodes/":
						http.Redirect(w, r, d.url(s)+"/api/cnodes/", code)
					}
				}))
				defer origin.Close()
				c := reviewClient(t, origin)
				var out []cnode
				err := c.Get(context.Background(), "cnodes/", &out)
				if got := s.got(); len(got) != 0 {
					t.Errorf("off-origin destination received %d request(s) (Get err=%v):%s", len(got), err, describe(got, "ACCESS-SECRET"))
				}
			})
		}
	}
}
