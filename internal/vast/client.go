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
		http: &http.Client{Transport: readOnly{next: tr}, Timeout: o.Timeout},
	}, nil
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
		// Should a server echo the request's token in its error, keep it
		// out of ours.
		detail := strings.ReplaceAll(snippet(body), token, "[token]")
		return nil, fmt.Errorf("vast: GET %s: status %d: %s", apiPath(path), status, detail)
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
	return c.do(req)
}

func (c *Client) do(req *http.Request) (int, []byte, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("vast: %s %s: %w", req.Method, req.URL.Path, scrub(err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("vast: %s %s: reading the response: %w", req.Method, req.URL.Path, err)
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

// snippet returns the start of a response body for an error message.
func snippet(body []byte) string {
	const max = 200
	s := strings.Join(strings.Fields(string(body)), " ")
	if len(s) > max {
		s = s[:max] + "…"
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
	status, body, err := c.do(req)
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
