package server

import (
	"bufio"
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Review finding S6: an authenticated live feed can outlive its browser
// session. A stream opened with the session cookie must end when the
// session does; a new request with the cookie is refused (the guard checks
// that already). Bearer-token streams are out of scope.
//
// The guard's clock (guard.now) is the only clock the session uses. Each
// test moves it forward to two seconds before the session's end, opens the
// stream, and then lets real time run. (The clock is moved before the
// stream opens, not during it: the fix sets the stream's deadline from the
// guard's clock when the request arrives, as a real clock would give it.)

type sseStream struct {
	events chan string   // each event's lines, joined
	ended  chan struct{} // closed when the server ends the stream
}

func openSSE(t *testing.T, s *Server, path, cookie string) *sseStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+s.HTTPAddr()+path, nil)
	name, value, _ := strings.Cut(cookie, ": ")
	req.Header.Set(name, value)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		resp.Body.Close()
		t.Fatalf("%s: status %d, content type %q", path, resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	st := &sseStream{events: make(chan string, 1024), ended: make(chan struct{})}
	go func() {
		defer close(st.ended)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		var ev []string
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				if len(ev) > 0 {
					select {
					case st.events <- strings.Join(ev, "\n"):
					default:
					}
				}
				ev = nil
				continue
			}
			if !strings.HasPrefix(line, ":") {
				ev = append(ev, line)
			}
		}
	}()
	return st
}

func (st *sseStream) first(t *testing.T) {
	t.Helper()
	select {
	case <-st.events:
	case <-st.ended:
		t.Fatal("the stream ended before its first event")
	case <-time.After(10 * time.Second):
		t.Fatal("no first event")
	}
}

// reviewS6Daemon starts a daemon with a token, signs in, and returns it with
// the session cookie and a function that moves the guard's clock.
func reviewS6Daemon(t *testing.T) (*Server, string, func(time.Duration)) {
	t.Helper()
	s := start(t, Config{GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0", APIToken: testToken, UIInterval: 50 * time.Millisecond})
	var offset atomic.Int64
	g, ok := s.http.Handler.(*guard)
	if !ok {
		t.Fatalf("the HTTP handler is a %T, not the guard", s.http.Handler)
	}
	g.now = func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	cookie := login(t, s)
	return s, cookie, func(d time.Duration) { offset.Store(int64(d)) }
}

// openExpiring moves the guard's clock to two seconds before the session
// ends, opens the stream and waits for its first event. It returns the
// stream and when the session ends.
func openExpiring(t *testing.T, s *Server, path, cookie string, setOffset func(time.Duration)) (*sseStream, time.Time) {
	t.Helper()
	setOffset(sessionLife - 2*time.Second)
	expiredAt := time.Now().Add(2 * time.Second)
	st := openSSE(t, s, path, cookie)
	st.first(t)
	return st, expiredAt
}

// expireAndWatch waits for the session to end, checks the guard then
// refuses the cookie, and waits for the stream to end. poke, if set, is
// called after the session has ended to make the stream have something new
// to send.
func expireAndWatch(t *testing.T, s *Server, st *sseStream, cookie, path string, expiredAt time.Time, poke func()) {
	t.Helper()
	// Let the session run out, then show the guard refuses the cookie.
	time.Sleep(time.Until(expiredAt) + 200*time.Millisecond)
	if resp, _ := request(t, s, "GET", "/api/v1/overview", nil, cookie); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("control: a new request with the expired cookie got %d, want 401", resp.StatusCode)
	}
	// Drain what was sent before the end.
	for len(st.events) > 0 {
		<-st.events
	}
	if poke != nil {
		poke()
	}
	after := 0
	var last string
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-st.ended:
			return // what a fixed daemon does
		case ev := <-st.events:
			after++
			last = ev
		case <-deadline:
			if len(last) > 160 {
				last = last[:160] + "..."
			}
			t.Errorf("%s: the stream is still open %.1fs after the session expired, and sent %d events after expiry; the last: %q",
				path, time.Since(expiredAt).Seconds(), after, last)
			return
		}
	}
}

func TestReviewS6_OverviewStreamEndsWithSession(t *testing.T) {
	s, cookie, setOffset := reviewS6Daemon(t)
	const path = "/api/v1/events?interval=100ms"
	st, expiredAt := openExpiring(t, s, path, cookie, setOffset)
	expireAndWatch(t, s, st, cookie, path, expiredAt, nil)
}

func TestReviewS6_WebUILiveStreamEndsWithSession(t *testing.T) {
	s, cookie, setOffset := reviewS6Daemon(t)
	const path = "/live/fleet"
	st, expiredAt := openExpiring(t, s, path, cookie, setOffset)
	// After expiry an Envoy connects: fresh fleet data for the page.
	expireAndWatch(t, s, st, cookie, path, expiredAt, func() { connect(s, 0, 1) })
}
