package cli

// Review finding S2, daemon-facing half: `deltav vast add` posts the VAST
// password to the daemon's API. If the --api URL answers with a redirect,
// the password body and the daemon API token must not follow it to another
// destination.

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type reviewSinkReq struct{ Method, Host, Path, Auth, Body string }

type reviewSink struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []reviewSinkReq
}

func newReviewSink(t *testing.T, listen string) *reviewSink {
	t.Helper()
	s := &reviewSink{}
	s.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.reqs = append(s.reqs, reviewSinkReq{r.Method, r.Host, r.URL.Path, r.Header.Get("Authorization"), string(b)})
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"Name":"lab","CNodes":0,"Pools":0,"VIPs":0}`)
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

func (s *reviewSink) got() []reviewSinkReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]reviewSinkReq(nil), s.reqs...)
}

func TestReviewS2_VastAddRedirectLeavesDaemon(t *testing.T) {
	const (
		password = "VAST-PASSWORD-xyzzy"
		apiToken = "DAEMON-API-TOKEN-plugh"
	)
	t.Setenv("DELTAV_PASSWORD", "")
	t.Setenv("DELTAV_API_TOKEN", "")
	dests := []struct{ name, listen string }{
		{"same-host-other-port", "127.0.0.1:0"},
		{"other-host-ipv6-loopback", "[::1]:0"},
	}
	for _, d := range dests {
		for _, code := range []int{301, 302, 303, 307, 308} {
			t.Run(fmt.Sprintf("%s/%d", d.name, code), func(t *testing.T) {
				s := newReviewSink(t, d.listen)
				daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					http.Redirect(w, r, s.srv.URL+r.URL.Path, code)
				}))
				defer daemon.Close()
				out, err := run(t, "deltav", "vast", "add", "lab", "--api", daemon.URL, "--api-token", apiToken,
					"--host", "vast.example.com", "--user", "admin", "--password", password)
				got := s.got()
				if len(got) == 0 {
					return
				}
				var b strings.Builder
				for _, r := range got {
					fmt.Fprintf(&b, "\n      %s %s%s Authorization=%q body=%q passwordLeaked=%v tokenLeaked=%v",
						r.Method, r.Host, r.Path, r.Auth, r.Body, strings.Contains(r.Body, password), strings.Contains(r.Auth, apiToken))
				}
				t.Errorf("redirect target received %d request(s) (cmd err=%v out=%q):%s", len(got), err, strings.TrimSpace(out), b.String())
			})
		}
	}
}
