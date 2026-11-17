// Package tui is the terminal UI. It is a client of the daemon's HTTP API,
// like the CLI and the web UI, and has no other way to reach daemon state.
package tui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"deltav/internal/metrics"
)

// The API's shapes, as far as the TUI reads them.

type Summary = metrics.Summary

type FleetCounts struct {
	Total     int `json:"total"`
	Connected int `json:"connected"`
	Acked     int `json:"acked"`
	Nacked    int `json:"nacked"`
	Pending   int `json:"pending"`
}

// LimitStatus is a target's per-CNode connection limit and, in enforce
// mode, the share each Envoy is given.
type LimitStatus struct {
	PerCNode        uint32 `json:"per_cnode"`
	Mode            string `json:"mode"`
	Share           uint32 `json:"share"`
	Computed        uint32 `json:"computed"`
	Envoys          int    `json:"envoys"`
	MaxVIPsPerCNode int    `json:"max_vips_per_cnode"`
}

// Usage is one CNode's requests in flight against one target's limit.
type Usage struct {
	Target   string `json:"target"`
	Cluster  string `json:"cluster"`
	CNode    string `json:"cnode"`
	InFlight uint64 `json:"in_flight"`
	PerCNode uint32 `json:"per_cnode"`
	Mode     string `json:"mode"`
	Over     bool   `json:"over"`
}

type Target struct {
	Name        string       `json:"name"`
	ListenPort  uint32       `json:"listen_port"`
	TLS         bool         `json:"tls"`
	Endpoints   int          `json:"endpoints"`
	InFlight    uint64       `json:"in_flight"`
	Stats       Summary      `json:"stats"`
	RateHistory []float64    `json:"rate_history"`
	Limit       *LimitStatus `json:"limit"`
	Over        []Usage      `json:"over"`
}

type Overview struct {
	ConfigVersion string      `json:"config_version"`
	Window        string      `json:"window"`
	Fleet         FleetCounts `json:"fleet"`
	Targets       []Target    `json:"targets"`
}

type VIPRow struct {
	Target   string  `json:"target"`
	Endpoint string  `json:"endpoint"`
	InFlight uint64  `json:"in_flight"`
	Stats    Summary `json:"stats"`
}

type CNode struct {
	Cluster     string    `json:"cluster"`
	Name        string    `json:"name"`
	State       string    `json:"state"`
	Enabled     bool      `json:"enabled"`
	Healthy     bool      `json:"healthy"`
	Drained     bool      `json:"drained"`
	Limits      []Usage   `json:"limits"`
	IsLeader    bool      `json:"is_leader"`
	IsMgmt      bool      `json:"is_mgmt"`
	VIPs        int       `json:"vips"`
	InFlight    uint64    `json:"in_flight"`
	Stats       Summary   `json:"stats"`
	RateHistory []float64 `json:"rate_history"`
	Endpoints   []VIPRow  `json:"endpoints"`
}

type CNodes struct {
	Window string  `json:"window"`
	CNodes []CNode `json:"cnodes"`
}

type Member struct {
	NodeID        string    `json:"node_id"`
	EnvoyVersion  string    `json:"envoy_version"`
	Address       string    `json:"address"`
	Connected     bool      `json:"connected"`
	LastSeen      time.Time `json:"last_seen"`
	ConfigVersion string    `json:"config_version"`
	ConfigState   string    `json:"config_state"`
	ConfigError   string    `json:"config_error"`
	InFlight      uint64    `json:"in_flight"`
	Stats         Summary   `json:"stats"`
}

type Fleet struct {
	ConfigVersion string   `json:"config_version"`
	Total         int      `json:"total"`
	Connected     int      `json:"connected"`
	Matched       int      `json:"matched"`
	Offset        int      `json:"offset"`
	Limit         int      `json:"limit"`
	Members       []Member `json:"members"`
}

type TargetStatus struct {
	VIPs    int          `json:"vips"`
	Healthy int          `json:"healthy"`
	Drained int          `json:"drained"`
	Problem string       `json:"problem"`
	Held    bool         `json:"held"`
	Limit   *LimitStatus `json:"limit"`
}

type TargetInfo struct {
	Name       string        `json:"name"`
	ListenPort uint32        `json:"listen_port"`
	TLS        bool          `json:"tls"`
	Source     string        `json:"source"`
	Cluster    string        `json:"cluster"`
	Pool       string        `json:"pool"`
	Endpoints  []struct{}    `json:"endpoints"`
	Status     *TargetStatus `json:"status"`
	Over       []Usage       `json:"over"`
}

type Cluster struct {
	Name      string     `json:"name"`
	Host      string     `json:"host"`
	User      string     `json:"user"`
	CNodes    int        `json:"cnodes"`
	Pools     int        `json:"pools"`
	VIPs      int        `json:"vips"`
	LastPoll  *time.Time `json:"last_poll"`
	LastError string     `json:"last_error"`
}

// FleetQuery selects one page of the fleet. Paging, sorting and filtering
// happen in the daemon, so the fleet view stays usable at thousands of rows.
type FleetQuery struct {
	Offset    int
	Limit     int
	Sort      string // an API sort key
	Desc      bool
	Filter    string // substring of node ID, address or version
	State     string // "", acked, nacked, pending
	Connected string // "", true, false
}

// Client reads the daemon's API.
type Client struct {
	Base  string // e.g. http://127.0.0.1:18080
	Token string // bearer token, if the daemon requires one
}

func (c *Client) request(ctx context.Context, path string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(c.Base, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	return req, nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := c.request(ctx, path)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach the daemon at %s: %w", c.Base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		switch {
		case e.Error != "":
			return fmt.Errorf("%s", e.Error)
		case resp.StatusCode == http.StatusUnauthorized:
			return fmt.Errorf("the daemon requires a token: set DELTAV_API_TOKEN or --api-token")
		}
		return fmt.Errorf("GET %s: status %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) Overview(ctx context.Context, window string) (*Overview, error) {
	var out Overview
	return &out, c.get(ctx, "/api/v1/overview?window="+url.QueryEscape(window), &out)
}

func (c *Client) CNodes(ctx context.Context, window string) (*CNodes, error) {
	var out CNodes
	return &out, c.get(ctx, "/api/v1/cnodes?window="+url.QueryEscape(window), &out)
}

func (c *Client) Fleet(ctx context.Context, window string, q FleetQuery) (*Fleet, error) {
	v := url.Values{"window": {window}, "offset": {strconv.Itoa(q.Offset)}, "limit": {strconv.Itoa(q.Limit)}}
	if q.Sort != "" {
		v.Set("sort", q.Sort)
	}
	if q.Desc {
		v.Set("order", "desc")
	}
	for name, value := range map[string]string{"q": q.Filter, "state": q.State, "connected": q.Connected} {
		if value != "" {
			v.Set(name, value)
		}
	}
	var out Fleet
	return &out, c.get(ctx, "/api/v1/fleet?"+v.Encode(), &out)
}

func (c *Client) Targets(ctx context.Context) ([]TargetInfo, error) {
	var out struct {
		Targets []TargetInfo `json:"targets"`
	}
	return out.Targets, c.get(ctx, "/api/v1/targets", &out)
}

func (c *Client) Clusters(ctx context.Context) ([]Cluster, error) {
	var out struct {
		Clusters []Cluster `json:"clusters"`
	}
	return out.Clusters, c.get(ctx, "/api/v1/vast", &out)
}

// send makes a change through the API and returns the daemon's error, if
// any, in its own words.
func (c *Client) send(ctx context.Context, method, path string, body any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.Base, "/")+path, rd)
	if err != nil {
		return err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach the daemon at %s: %w", c.Base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		return nil
	}
	var e struct {
		Error string `json:"error"`
	}
	if json.NewDecoder(resp.Body).Decode(&e) == nil && e.Error != "" {
		return fmt.Errorf("%s", e.Error)
	}
	return fmt.Errorf("%s %s: status %d", method, path, resp.StatusCode)
}

// SetLimit sets a target's per-CNode connection limit; 0 removes it.
func (c *Client) SetLimit(ctx context.Context, target string, perCNode uint32, mode string) error {
	path := "/api/v1/targets/" + url.PathEscape(target) + "/limit"
	if perCNode == 0 {
		return c.send(ctx, http.MethodDelete, path, nil)
	}
	return c.send(ctx, http.MethodPut, path, map[string]any{"per_cnode": perCNode, "mode": mode})
}

// SetDrained drains or undrains a CNode.
func (c *Client) SetDrained(ctx context.Context, cluster, cnode string, drained bool) error {
	method := http.MethodDelete
	if drained {
		method = http.MethodPost
	}
	return c.send(ctx, method, "/api/v1/vast/"+url.PathEscape(cluster)+"/cnodes/"+url.PathEscape(cnode)+"/drain", nil)
}

// Event is one item from the daemon's event stream: an overview, or the
// reason the stream broke.
type Event struct {
	Overview *Overview
	Err      error
}

// Subscribe follows the daemon's server-sent event stream until ctx ends,
// reconnecting when it breaks. Each overview arrives as an Event; a broken
// stream is reported once per attempt.
func (c *Client) Subscribe(ctx context.Context, interval time.Duration) <-chan Event {
	out := make(chan Event)
	send := func(e Event) bool {
		select {
		case out <- e:
			return true
		case <-ctx.Done():
			return false
		}
	}
	go func() {
		defer close(out)
		for wait := time.Second; ctx.Err() == nil; {
			started := time.Now()
			err := c.stream(ctx, interval, func(o *Overview) bool { return send(Event{Overview: o}) })
			if ctx.Err() != nil {
				return
			}
			if err == nil {
				err = fmt.Errorf("the daemon closed the event stream")
			}
			if !send(Event{Err: err}) {
				return
			}
			// Back off while it keeps failing; start over once a
			// connection has lasted.
			if time.Since(started) > 10*time.Second {
				wait = time.Second
			} else if wait < 5*time.Second {
				wait += time.Second
			}
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

// stream reads one connection's worth of events.
func (c *Client) stream(ctx context.Context, interval time.Duration, emit func(*Overview) bool) error {
	req, err := c.request(ctx, "/api/v1/events?interval="+interval.String())
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req) // no timeout: the stream is meant to stay open
	if err != nil {
		return fmt.Errorf("cannot reach the daemon at %s: %w", c.Base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("the daemon requires a token: set DELTAV_API_TOKEN or --api-token")
		}
		return fmt.Errorf("event stream: status %d", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 8<<20) // one overview is one line
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var o Overview
		if err := json.Unmarshal([]byte(data), &o); err != nil {
			return fmt.Errorf("event stream: %w", err)
		}
		if !emit(&o) {
			return nil
		}
	}
	return sc.Err()
}
