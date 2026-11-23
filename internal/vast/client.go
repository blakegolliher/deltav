package vast

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Response size limits. A VAST cluster is trusted, but a buggy VMS or a
// proxy in front of it must not exhaust the daemon's memory, which would
// take the control plane down for the whole fleet. Only an inventory list
// is large; a token response is small, and of an error only a snippet is
// kept.
const (
	maxInventoryBytes = 64 << 20 // a 200 response to a GET
	maxSmallBytes     = 1 << 20  // a token response, and any non-200 body
)

// Options describe one VAST cluster's management endpoint.
type Options struct {
	Host     string // management host, optionally host:port; HTTPS is always used
	User     string
	Password string
	Tenant   string // "" logs in at /api/token/; otherwise at /api/token/<tenant>
	// Insecure turns certificate verification off. It is for lab clusters
	// with self-signed certificates; verification is on by default.
	Insecure bool
	Timeout  time.Duration // per request; default 30s

	rootCAs *x509.CertPool // tests: trust a test server's certificate
}

// Client talks to one VAST cluster. It logs in on first use, refreshes or
// renews its token when the cluster rejects it, and can only issue GETs
// and token calls. It is safe for concurrent use.
//
// Credentials and tokens are never logged and never appear in errors.
type Client struct {
	base string // https://host
	opts Options
	http *http.Client

	mu      sync.Mutex
	access  string
	refresh string
}

// New builds a client. It does not contact the cluster.
func New(o Options) (*Client, error) {
	host := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(o.Host), "https://"), "/")
	if host == "" || strings.ContainsAny(host, "/ ") || strings.HasPrefix(host, "http:") {
		return nil, fmt.Errorf("vast: host %q: want a host name or host:port (HTTPS is implied)", o.Host)
	}
	if o.User == "" || o.Password == "" {
		return nil, errors.New("vast: a user and a password are required")
	}
	if o.Timeout <= 0 {
		o.Timeout = 30 * time.Second
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: o.Insecure, RootCAs: o.rootCAs}
	return &Client{
		base: "https://" + host,
		opts: o,
		http: &http.Client{Transport: readOnly{next: tr}, Timeout: o.Timeout, CheckRedirect: sameOrigin},
	}, nil
}

// sameOrigin is the client's redirect policy. A token call is never
// redirected: it carries the password or the refresh token, and a
// redirected login would adopt tokens minted by whoever answered. A GET may
// be redirected only within the origin it was sent to, scheme and port
// included: Go keeps the Authorization header whenever the host name stays
// the same, even from https to http, and whatever answered elsewhere would
// be taken for the cluster's inventory. readOnly still checks every hop.
func sameOrigin(req *http.Request, via []*http.Request) error {
	first := via[0]
	if first.Method != http.MethodGet {
		return fmt.Errorf("refused a redirect of %s %s", first.Method, first.URL.Path)
	}
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if req.URL.Scheme != first.URL.Scheme || !strings.EqualFold(req.URL.Hostname(), first.URL.Hostname()) ||
		effectivePort(req.URL) != effectivePort(first.URL) {
		return fmt.Errorf("refused a redirect to %s://%s: it leaves %s://%s", req.URL.Scheme, req.URL.Host, first.URL.Scheme, first.URL.Host)
	}
	return nil
}

// effectivePort is u's port, or the default for its scheme.
func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if u.Scheme == "http" {
		return "80"
	}
	return "443"
}

// Get fetches /api/<path> and decodes the JSON response into out. path is
// relative to the API base, for example "cnodes/".
func (c *Client) Get(ctx context.Context, path string, out any) error {
	body, err := c.GetRaw(ctx, path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("vast: GET %s: decoding the response: %w", apiPath(path), err)
	}
	return nil
}

// GetRaw fetches /api/<path> and returns the response body.
func (c *Client) GetRaw(ctx context.Context, path string) ([]byte, error) {
	token, err := c.token(ctx)
	if err != nil {
		return nil, err
	}
	status, body, err := c.get(ctx, path, token)
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized {
		// The token expired or was revoked: get a new one, once.
		if token, err = c.renew(ctx, token); err != nil {
			return nil, err
		}
		if status, body, err = c.get(ctx, path, token); err != nil {
			return nil, err
		}
	}
	if status != http.StatusOK {
		// Should a server echo a token in its error, keep it out of ours.
		// Redact before truncating, or the start of a token survives.
		detail := strings.ReplaceAll(string(body), token, "[token]")
		c.mu.Lock()
		if c.refresh != "" {
			detail = strings.ReplaceAll(detail, c.refresh, "[token]")
		}
		c.mu.Unlock()
		return nil, fmt.Errorf("vast: GET %s: status %d: %s", apiPath(path), status, snippet(detail))
	}
	return body, nil
}

func apiPath(path string) string {
	return apiBase + "/" + strings.TrimPrefix(path, "/")
}

func (c *Client) get(ctx context.Context, path, token string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+apiPath(path), nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	return c.do(req, maxInventoryBytes)
}

// do sends req and reads the response: up to limit bytes of a 200, and
// maxSmallBytes of anything else. A longer response is an error.
func (c *Client) do(req *http.Request, limit int64) (int, []byte, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("vast: %s %s: %w", req.Method, req.URL.Path, scrub(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		limit = maxSmallBytes
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return 0, nil, fmt.Errorf("vast: %s %s: reading the response: %w", req.Method, req.URL.Path, err)
	}
	if int64(len(body)) > limit {
		return 0, nil, fmt.Errorf("vast: %s %s: status %d: response larger than %d bytes", req.Method, req.URL.Path, resp.StatusCode, limit)
	}
	return resp.StatusCode, body, nil
}

// scrub removes the request URL from a transport error. Go's *url.Error
// prints it, and although Delta-V never puts credentials in a URL, an
// error is no place to depend on that.
func scrub(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// snippet returns the start of a response body for an error message, cut
// on a character boundary.
func snippet(body string) string {
	const max = 200
	s := strings.Join(strings.Fields(body), " ")
	if len(s) > max {
		cut := max
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "…"
	}
	return s
}

// token returns the current access token, logging in if there is none.
func (c *Client) token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.access == "" {
		if err := c.login(ctx); err != nil {
			return "", err
		}
	}
	return c.access, nil
}

// renew replaces a rejected access token: by refresh if the cluster still
// accepts the refresh token, otherwise by logging in again. If another
// goroutine already replaced it, the new one is returned as it is.
func (c *Client) renew(ctx context.Context, rejected string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.access != rejected && c.access != "" {
		return c.access, nil
	}
	if c.refresh != "" {
		var out tokens
		status, err := c.post(ctx, "token/refresh/", map[string]string{"refresh": c.refresh}, &out)
		if err == nil && status == http.StatusOK && out.Access != "" {
			c.access = out.Access
			return c.access, nil
		}
	}
	if err := c.login(ctx); err != nil {
		return "", err
	}
	return c.access, nil
}

type tokens struct {
	Access  string `json:"access"`
	Refresh string `json:"refresh"`
}

// login must be called with c.mu held.
func (c *Client) login(ctx context.Context) error {
	path := "token/"
	if c.opts.Tenant != "" {
		path += url.PathEscape(c.opts.Tenant)
	}
	var out tokens
	status, err := c.post(ctx, path, map[string]string{"username": c.opts.User, "password": c.opts.Password}, &out)
	if err != nil {
		return err
	}
	if status != http.StatusOK || out.Access == "" {
		// Not the body: a login response has no business in a log.
		return fmt.Errorf("vast: login as %q failed: status %d", c.opts.User, status)
	}
	c.access, c.refresh = out.Access, out.Refresh
	return nil
}

func (c *Client) post(ctx context.Context, path string, in, out any) (int, error) {
	payload, err := json.Marshal(in)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+apiPath(path), bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	status, body, err := c.do(req, maxSmallBytes)
	if err != nil {
		return 0, err
	}
	if status == http.StatusOK {
		if err := json.Unmarshal(body, out); err != nil {
			return status, fmt.Errorf("vast: POST %s: the response is not JSON", apiPath(path))
		}
	}
	return status, nil
}
