package vast

// Review finding S5: VAST responses are read with an unbounded io.ReadAll,
// and the echoed-token redaction runs after the error text is truncated.
// These tests assert the correct behaviour against the unchanged client.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// bigServer logs in normally and answers `path` with status and a body of
// size bytes, streamed in 1 MiB chunks. It reports how many bytes the
// handler managed to write, i.e. how much the client consumed (plus at most
// the socket buffers).
func bigServer(t *testing.T, method, path string, status int, size int64) (*httptest.Server, *atomic.Int64, chan struct{}) {
	t.Helper()
	var written atomic.Int64
	done := make(chan struct{}, 1)
	chunk := []byte(strings.Repeat(" ", 1<<20))
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == method && r.URL.Path == path {
			defer func() { done <- struct{}{} }()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			if method == http.MethodGet && status == http.StatusOK {
				n, _ := w.Write([]byte("["))
				written.Add(int64(n))
			}
			for written.Load() < size {
				n, err := w.Write(chunk)
				written.Add(int64(n))
				if err != nil {
					return
				}
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/token/":
			_ = json.NewEncoder(w).Encode(map[string]string{"access": "a", "refresh": "r"})
		default:
			_, _ = w.Write([]byte("[]"))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &written, done
}

func clientFor(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	o := Options{Host: strings.TrimPrefix(srv.URL, "https://"), User: testUser, Password: testPassword}
	o.rootCAs = srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	c, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func totalAlloc() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.TotalAlloc
}

func waitDone(t *testing.T, done chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
}

func TestReviewS5_OverLargeResponsesAreBounded(t *testing.T) {
	const MiB = 1 << 20
	cases := []struct {
		name         string
		method, path string
		status       int
		size         int64
		call         func(c *Client) error
	}{
		// Well over the 64 MiB inventory limit: the client reads limit+1
		// bytes to see that it is over, and socket buffers hold some more.
		{"inventory-200-96MiB", http.MethodGet, "/api/cnodes/", http.StatusOK, 96 * MiB,
			func(c *Client) error { _, err := c.GetRaw(context.Background(), "cnodes/"); return err }},
		{"error-500-16MiB", http.MethodGet, "/api/cnodes/", http.StatusInternalServerError, 16 * MiB,
			func(c *Client) error { _, err := c.GetRaw(context.Background(), "cnodes/"); return err }},
		{"login-200-16MiB", http.MethodPost, "/api/token/", http.StatusOK, 16 * MiB,
			func(c *Client) error { _, err := c.GetRaw(context.Background(), "cnodes/"); return err }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, written, done := bigServer(t, tc.method, tc.path, tc.status, tc.size)
			c := clientFor(t, srv)
			runtime.GC()
			before := totalAlloc()
			err := tc.call(c)
			after := totalAlloc()
			waitDone(t, done)
			consumed := written.Load()
			t.Logf("server wrote %d bytes (%.1f MiB); client allocated %.1f MiB during the call; err=%.120v",
				consumed, float64(consumed)/MiB, float64(after-before)/MiB, err)
			if consumed >= tc.size {
				t.Errorf("the client consumed the whole %d MiB response: no byte limit is enforced", tc.size/MiB)
			}
			if tc.status == http.StatusOK && err == nil {
				t.Errorf("an over-large %s response was accepted (want an error naming the limit)", tc.path)
			}
		})
	}
}

// The token is echoed by the server starting at character 180 of its error
// body. snippet() keeps the first 200 characters, and only then is the
// token replaced: the first 20 characters of the token survive.
func TestReviewS5_EchoedTokenIsRedactedBeforeTruncation(t *testing.T) {
	const token = "TOKEN0123456789abcdefghijklmnopqrstuvwxyzABCDEFGH" // 49 chars
	for _, offset := range []int{0, 180} {
		t.Run(map[int]string{0: "token-at-0", 180: "token-at-180"}[offset], func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/token/" {
					_ = json.NewEncoder(w).Encode(map[string]string{"access": token, "refresh": "r"})
					return
				}
				w.WriteHeader(http.StatusForbidden)
				body := strings.Repeat("x", offset) + strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") + " is not permitted"
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()
			c := clientFor(t, srv)
			_, err := c.GetRaw(context.Background(), "cnodes/")
			if err == nil {
				t.Fatal("want an error")
			}
			// Any 8-character piece of the token is too much.
			for i := 0; i+8 <= len(token); i++ {
				if strings.Contains(err.Error(), token[i:i+8]) {
					t.Fatalf("the error carries part of the token (%q...):\n    %s", token[i:i+8], err)
				}
			}
		})
	}
}
