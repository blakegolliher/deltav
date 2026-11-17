// Package vast is Delta-V's client for the VAST management REST API.
//
// Hard rule: against a VAST cluster Delta-V issues only token calls and
// GETs. The rule is enforced here, in the HTTP transport, so no caller can
// break it by mistake and redirects are checked like any other request.
package vast

import (
	"fmt"
	"net/http"
	"strings"
)

// apiBase is the path every VAST REST call lives under.
const apiBase = "/api"

// readOnly is an http.RoundTripper that refuses every request except GET
// and the token POSTs, before anything is sent.
type readOnly struct {
	next http.RoundTripper
}

func (t readOnly) RoundTrip(req *http.Request) (*http.Response, error) {
	if !allowed(req.Method, req.URL.Path) {
		// The path is safe to print: credentials travel in the body and
		// headers, never in it.
		return nil, fmt.Errorf("vast: refused %s %s: only GET and token calls are allowed against a VAST cluster", req.Method, req.URL.Path)
	}
	return t.next.RoundTrip(req)
}

// allowed reports whether a request may be sent: any GET, and POST only to
// /api/token/, /api/token/refresh/ and the tenant form /api/token/<tenant>.
func allowed(method, path string) bool {
	switch method {
	case http.MethodGet:
		return true
	case http.MethodPost:
		rest, ok := strings.CutPrefix(path, apiBase+"/token/")
		if !ok {
			return false
		}
		rest = strings.TrimSuffix(rest, "/")
		// "" is /api/token/; anything else must be one path segment
		// (refresh, or a tenant name).
		return !strings.Contains(rest, "/")
	}
	return false
}
