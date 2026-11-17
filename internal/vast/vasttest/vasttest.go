// Package vasttest is a mock VAST management API driven by fixtures: the
// sanitized responses of a real cluster under testdata/vast. It serves the
// token calls and the three list endpoints Delta-V reads, and lets a test
// change the cluster's state (move a VIP, fail a CNode) between polls.
//
// It answers anything that is not a GET or a token call with 405 and counts
// it, so a test can prove Delta-V never tried.
package vasttest

import (
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Credentials the mock accepts.
const (
	User     = "admin"
	Password = "mock-password"
)

// Server is a running mock.
type Server struct {
	srv *httptest.Server

	mu       sync.Mutex
	docs     map[string][]map[string]any // "cnodes", "vippools", "vips"
	tokens   map[string]bool             // access tokens it accepts
	refresh  map[string]bool             // refresh tokens it accepts
	seq      int
	requests map[string]int // "METHOD /path" -> count
	mutating int
	fail     int // answer this many list requests with 500
}

// New loads cnodes.json, vippools.json and vips.json from dir and starts an
// HTTPS server with a self-signed certificate. Close it when done.
func New(dir string) (*Server, error) {
	s := &Server{docs: map[string][]map[string]any{}, tokens: map[string]bool{}, refresh: map[string]bool{}, requests: map[string]int{}}
	for _, name := range []string{"cnodes", "vippools", "vips"} {
		b, err := os.ReadFile(filepath.Join(dir, name+".json"))
		if err != nil {
			return nil, err
		}
		var doc []map[string]any
		if err := json.Unmarshal(b, &doc); err != nil {
			return nil, fmt.Errorf("%s.json: %w", name, err)
		}
		s.docs[name] = doc
	}
	s.srv = httptest.NewTLSServer(http.HandlerFunc(s.serve))
	return s, nil
}

// Close stops the server.
func (s *Server) Close() { s.srv.Close() }

// Host is the server's host:port, for vast.Options.Host.
func (s *Server) Host() string { return strings.TrimPrefix(s.srv.URL, "https://") }

// CertPool trusts the server's certificate.
func (s *Server) CertPool() *x509.CertPool {
	return s.srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests[r.Method+" "+r.URL.Path]++
	reply := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/token/":
		var in struct{ Username, Password string }
		if json.NewDecoder(r.Body).Decode(&in) != nil || in.Username != User || in.Password != Password {
			reply(http.StatusUnauthorized, map[string]string{"detail": "No active account found with the given credentials"})
			return
		}
		s.seq++
		access, refresh := fmt.Sprintf("mock-access-%d", s.seq), fmt.Sprintf("mock-refresh-%d", s.seq)
		s.tokens[access], s.refresh[refresh] = true, true
		reply(http.StatusOK, map[string]string{"access": access, "refresh": refresh})
	case r.Method == http.MethodPost && r.URL.Path == "/api/token/refresh/":
		var in struct{ Refresh string }
		if json.NewDecoder(r.Body).Decode(&in) != nil || !s.refresh[in.Refresh] {
			reply(http.StatusUnauthorized, map[string]string{"detail": "Token is invalid or expired"})
			return
		}
		s.seq++
		access := fmt.Sprintf("mock-access-%d", s.seq)
		s.tokens[access] = true
		reply(http.StatusOK, map[string]string{"access": access})
	case r.Method != http.MethodGet:
		s.mutating++
		reply(http.StatusMethodNotAllowed, map[string]string{"detail": "the mock VAST accepts only GET and the token call"})
	case !s.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]:
		reply(http.StatusUnauthorized, map[string]string{"detail": "Given token not valid for any token type"})
	default:
		name := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/"), "/")
		doc, ok := s.docs[name]
		if !ok {
			reply(http.StatusNotFound, map[string]string{"detail": "Not found."})
			return
		}
		if s.fail > 0 {
			s.fail--
			reply(http.StatusInternalServerError, map[string]string{"detail": "mock failure"})
			return
		}
		reply(http.StatusOK, doc)
	}
}

// find returns the first object of a document whose field equals value.
func (s *Server) find(doc, field, value string) map[string]any {
	for _, item := range s.docs[doc] {
		if item[field] == value {
			return item
		}
	}
	return nil
}

// SetCNode changes a CNode's state and enabled flag.
func (s *Server) SetCNode(name, state string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.find("cnodes", "name", name)
	if c == nil {
		return fmt.Errorf("no CNode %q in the fixtures", name)
	}
	c["state"], c["display_state"], c["enabled"] = state, state, enabled
	return nil
}

// MoveVIP moves a VIP to another CNode, as VAST does on failover.
func (s *Server) MoveVIP(ip, cnode string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.find("vips", "ip", ip)
	if v == nil {
		return fmt.Errorf("no VIP %q in the fixtures", ip)
	}
	if s.find("cnodes", "name", cnode) == nil {
		return fmt.Errorf("no CNode %q in the fixtures", cnode)
	}
	v["cnode"] = cnode
	return nil
}

// SetPoolVIPs gives a pool's VIPs new addresses, in order, keeping which
// CNode hosts each. A harness uses it to point a pool at servers it runs
// itself, such as fake S3 backends on loopback addresses. It fails unless
// the pool has exactly len(ips) VIPs.
func (s *Server) SetPoolVIPs(pool string, ips []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var vips []map[string]any
	for _, v := range s.docs["vips"] {
		if v["vippool"] == pool {
			vips = append(vips, v)
		}
	}
	if len(vips) != len(ips) {
		return fmt.Errorf("pool %q has %d VIPs, %d addresses given", pool, len(vips), len(ips))
	}
	for i, v := range vips {
		v["ip"], v["title"] = ips[i], ips[i]
	}
	return nil
}

// PoolCNodes returns, for each VIP of a pool in the order SetPoolVIPs uses,
// the name of the CNode hosting it.
func (s *Server) PoolCNodes(pool string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, v := range s.docs["vips"] {
		if v["vippool"] == pool {
			name, _ := v["cnode"].(string)
			out = append(out, name)
		}
	}
	return out
}

// RemovePoolVIPs deletes every VIP of a pool and returns how many it
// removed, as if the pool had been emptied.
func (s *Server) RemovePoolVIPs(pool string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.docs["vips"][:0:0]
	for _, v := range s.docs["vips"] {
		if v["vippool"] != pool {
			kept = append(kept, v)
		}
	}
	n := len(s.docs["vips"]) - len(kept)
	s.docs["vips"] = kept
	return n
}

// FailNext makes the next n list requests answer 500.
func (s *Server) FailNext(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = n
}

// ExpireTokens makes the server forget every access token it issued, and
// every refresh token too if refreshToo is set.
func (s *Server) ExpireTokens(refreshToo bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens = map[string]bool{}
	if refreshToo {
		s.refresh = map[string]bool{}
	}
}

// Requests returns how often each "METHOD /path" was requested.
func (s *Server) Requests() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.requests))
	for k, v := range s.requests {
		out[k] = v
	}
	return out
}

// Mutating is the number of requests received that were neither a GET nor
// the token call. It must stay 0.
func (s *Server) Mutating() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mutating
}
