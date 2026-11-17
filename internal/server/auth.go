package server

import (
	"crypto/subtle"
	"net"
	"net/http"
	"strings"
)

// isLoopback reports whether a listen address binds only to loopback.
// Wildcards ("", "0.0.0.0", "::") and hostnames other than "localhost" are
// treated as non-loopback, so they require a token.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// requireToken rejects requests without "Authorization: Bearer <token>".
// Paths in open are served without a token (liveness only; they expose nothing).
func requireToken(token string, open map[string]bool, next http.Handler) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if open[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		got := []byte(strings.TrimSpace(r.Header.Get("Authorization")))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="deltav"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
