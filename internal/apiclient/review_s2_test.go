package apiclient

// Review finding S2, network API client half: a redirect from the daemon URL
// must not carry the API token or a request body (AddCluster's VAST
// password, SetLimit's PUT) to another destination.

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type sinkReq struct{ Method, Host, Path, Auth, Body string }

type sink struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []sinkReq
}

func newSink(t *testing.T, listen string) *sink {
	t.Helper()
	s := &sink{}
	s.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.reqs = append(s.reqs, sinkReq{r.Method, r.Host, r.URL.Path, r.Header.Get("Authorization"), string(b)})
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	l, err := net.Listen("tcp", listen)
	if err != nil {
		t.Skipf("cannot listen on %s: %v", listen, err)
	}
	s.srv.Listener.Close()
	s.srv.Listener = l
	s.srv.Start()
	t.Cleanup(s.srv.Close)
	return s
}

func (s *sink) got() []sinkReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sinkReq(nil), s.reqs...)
}

func TestReviewS2_APIClientRedirectLeavesDaemon(t *testing.T) {
	const (
		password = "VAST-PASSWORD-xyzzy"
		apiToken = "DAEMON-API-TOKEN-plugh"
	)
	calls := []struct {
		name string
		do   func(c *Client) error
	}{
		{"AddCluster", func(c *Client) error {
			return c.AddCluster(context.Background(), NewCluster{Name: "lab", Host: "vast.example.com", User: "admin", Password: password})
		}},
		{"SetLimit", func(c *Client) error { return c.SetLimit(context.Background(), "t", 5, "alert") }},
		{"Overview", func(c *Client) error { _, err := c.Overview(context.Background(), "1m"); return err }},
	}
	dests := []struct{ name, listen string }{
		{"same-host-other-port", "127.0.0.1:0"},
		{"other-host-ipv6-loopback", "[::1]:0"},
	}
	for _, call := range calls {
		for _, d := range dests {
			for _, code := range []int{301, 302, 303, 307, 308} {
				t.Run(fmt.Sprintf("%s/%s/%d", call.name, d.name, code), func(t *testing.T) {
					s := newSink(t, d.listen)
					daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						http.Redirect(w, r, s.srv.URL+r.URL.Path, code)
					}))
					defer daemon.Close()
					c := &Client{Base: daemon.URL, Token: apiToken} // HTTP nil: http.DefaultClient, as the TUI uses it
					err := call.do(c)
					got := s.got()
					if len(got) == 0 {
						return
					}
					var b strings.Builder
					for _, r := range got {
						fmt.Fprintf(&b, "\n      %s %s%s Authorization=%q body=%q passwordLeaked=%v tokenLeaked=%v",
							r.Method, r.Host, r.Path, r.Auth, r.Body, strings.Contains(r.Body, password), strings.Contains(r.Auth, apiToken))
					}
					t.Errorf("redirect target received %d request(s) (err=%v):%s", len(got), err, b.String())
				})
			}
		}
	}
}
