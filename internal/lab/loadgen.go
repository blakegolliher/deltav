package lab

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// SpecialKeys are object keys, in the form they travel on the wire, that a
// proxy must pass through byte for byte. Envoy's path normalization, slash
// merging and escaped-slash handling would each change at least one.
var SpecialKeys = []string{
	"plain/key",
	"double//slash",
	"trailing/slash/",
	"dot/./segment",
	"dotdot/../segment",
	"escaped%2Fslash",
	"lowerhex%2fslash",
	"space%20here",
	"plus+literal",
	"plus%2Bencoded",
	"percent%25literal",
	"question%3Fmark",
	"semi;colon,comma",
	"unicode/%E2%9C%93-%D0%BA%D0%BB%D1%8E%D1%87",
}

// LoadConfig describes one load generator run. Requests are path-style:
// <endpoint>/<bucket>/<key>.
type LoadConfig struct {
	Endpoint    string        // http://127.0.0.1:9000
	Host        string        // Host header to send and sign; default: the endpoint's
	Bucket      string        //
	Prefix      string        // key prefix; default "dvlab/"
	Creds       *Credentials  // nil: requests are not signed
	Concurrency int           // default 4
	Requests    int           // stop after this many; 0: run for Duration
	Duration    time.Duration // used when Requests is 0
	PutRatio    float64       // fraction of requests that are PUTs; the rest are GETs
	Size        int64         // PUT body size, and the GET size asked of a fake backend
	SpecialKeys bool          // cycle through SpecialKeys instead of plain keys
	Expect      bool          // PUTs send (and sign) "Expect: 100-continue"
	// Check verifies each response against what was sent, using the fake
	// backend's X-Fake-* headers. It only works against dvlab fakes3.
	Check bool
	// Roundtrip replaces the PUT/GET mix with whole cycles against a real
	// S3 endpoint: PUT a new key, GET it back and compare the bytes, DELETE
	// it. Requests then counts cycles (three HTTP requests each). It does
	// not need a fake backend, and leaves nothing behind unless a cycle
	// fails part-way.
	Roundtrip     bool
	Timeout       time.Duration // per request; default 60s
	ExpectTimeout time.Duration // wait for 100 Continue before sending a body anyway; default 5s
}

// LatencyMS summarizes request latency in milliseconds.
type LatencyMS struct {
	P50 float64 `json:"p50"`
	P90 float64 `json:"p90"`
	P99 float64 `json:"p99"`
	Max float64 `json:"max"`
}

// LoadResult is the outcome of a run.
type LoadResult struct {
	Requests        int            `json:"requests"`
	TransportErrors int            `json:"transport_errors"`
	Status          map[int]int    `json:"status"`
	Backends        map[string]int `json:"backends,omitempty"` // responses per fake backend
	Injected        int            `json:"injected,omitempty"` // failures the fake injected on purpose
	BytesSent       int64          `json:"bytes_sent"`
	BytesReceived   int64          `json:"bytes_received"`
	ElapsedSeconds  float64        `json:"elapsed_seconds"`
	Latency         LatencyMS      `json:"latency_ms"`
	CheckFailures   int            `json:"check_failures"`
	Roundtrips      int            `json:"roundtrips,omitempty"` // PUT/GET/DELETE cycles that verified
	Samples         []string       `json:"samples,omitempty"`    // first few errors and check failures

	latencies []time.Duration
}

const maxSamples = 20

func (r *LoadResult) sample(format string, args ...any) {
	if len(r.Samples) < maxSamples {
		r.Samples = append(r.Samples, fmt.Sprintf(format, args...))
	}
}

func (r *LoadResult) merge(o *LoadResult) {
	r.Requests += o.Requests
	r.TransportErrors += o.TransportErrors
	r.Injected += o.Injected
	r.BytesSent += o.BytesSent
	r.BytesReceived += o.BytesReceived
	r.CheckFailures += o.CheckFailures
	r.Roundtrips += o.Roundtrips
	for k, v := range o.Status {
		r.Status[k] += v
	}
	for k, v := range o.Backends {
		r.Backends[k] += v
	}
	for _, s := range o.Samples {
		r.sample("%s", s)
	}
	r.latencies = append(r.latencies, o.latencies...)
}

func newLoadResult() *LoadResult {
	return &LoadResult{Status: map[int]int{}, Backends: map[string]int{}}
}

type loadRun struct {
	id          string // distinguishes this run's keys from any other's
	cfg         LoadConfig
	base        *url.URL
	host        string
	client      *http.Client
	payload     []byte
	payloadHash string
	next        atomic.Int64
	deadline    time.Time
}

// RunLoad sends requests until the configured count or duration is reached
// or ctx ends. It returns an error only for a configuration it cannot run.
func RunLoad(ctx context.Context, cfg LoadConfig) (*LoadResult, error) {
	base, err := url.Parse(cfg.Endpoint)
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") {
		return nil, fmt.Errorf("endpoint %q: want http://host:port", cfg.Endpoint)
	}
	if cfg.Bucket == "" || strings.Contains(cfg.Bucket, "/") {
		return nil, errors.New("a bucket name is required")
	}
	if cfg.Requests <= 0 && cfg.Duration <= 0 {
		return nil, errors.New("set a request count or a duration")
	}
	if cfg.PutRatio < 0 || cfg.PutRatio > 1 {
		return nil, errors.New("the PUT ratio must be between 0 and 1")
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 4
	}
	if cfg.Prefix == "" {
		cfg.Prefix = "dvlab/"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 60 * time.Second
	}
	if cfg.ExpectTimeout <= 0 {
		cfg.ExpectTimeout = 5 * time.Second
	}

	if cfg.Roundtrip && cfg.Check {
		return nil, errors.New("--check needs a fake backend and --roundtrip a real one: choose one")
	}
	run := &loadRun{cfg: cfg, base: base, host: cfg.Host, id: strconv.FormatInt(time.Now().UnixNano(), 36)}
	if run.host == "" {
		run.host = base.Host
	}
	if cfg.Requests <= 0 {
		run.deadline = time.Now().Add(cfg.Duration)
	}
	run.payload = make([]byte, cfg.Size)
	_, _ = io.ReadFull(PatternReader("dvlab-loadgen-payload", cfg.Size), run.payload)
	sum := sha256.Sum256(run.payload)
	run.payloadHash = hex.EncodeToString(sum[:])

	tr := &http.Transport{
		MaxIdleConns:          cfg.Concurrency * 2,
		MaxIdleConnsPerHost:   cfg.Concurrency * 2,
		IdleConnTimeout:       30 * time.Second,
		ExpectContinueTimeout: cfg.ExpectTimeout,
		DisableCompression:    true, // send no Accept-Encoding the caller did not ask for
	}
	defer tr.CloseIdleConnections()
	run.client = &http.Client{
		Transport: tr,
		// A redirect means something rewrote the request; report it as is.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	start := time.Now()
	parts := make([]*LoadResult, cfg.Concurrency)
	var wg sync.WaitGroup
	for w := range parts {
		parts[w] = newLoadResult()
		wg.Add(1)
		go func(res *LoadResult) {
			defer wg.Done()
			for ctx.Err() == nil {
				i := run.next.Add(1) - 1
				if cfg.Requests > 0 && i >= int64(cfg.Requests) {
					return
				}
				if cfg.Requests <= 0 && !time.Now().Before(run.deadline) {
					return
				}
				if cfg.Roundtrip {
					run.roundtrip(ctx, i, res)
				} else {
					run.one(ctx, i, res)
				}
			}
		}(parts[w])
	}
	wg.Wait()

	total := newLoadResult()
	for _, p := range parts {
		total.merge(p)
	}
	total.ElapsedSeconds = time.Since(start).Seconds()
	sort.Slice(total.latencies, func(i, j int) bool { return total.latencies[i] < total.latencies[j] })
	total.Latency = LatencyMS{
		P50: percentileMS(total.latencies, 0.50),
		P90: percentileMS(total.latencies, 0.90),
		P99: percentileMS(total.latencies, 0.99),
		Max: percentileMS(total.latencies, 1),
	}
	if len(total.Backends) == 0 {
		total.Backends = nil
	}
	return total, nil
}

// percentileMS returns the nearest-rank percentile of sorted durations.
func percentileMS(sorted []time.Duration, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(p*float64(len(sorted))+0.999999) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return float64(sorted[rank].Microseconds()) / 1000
}

// rawPath returns the wire path of request i.
func (run *loadRun) rawPath(i int64) string {
	key := fmt.Sprintf("obj-%06d", i%1000)
	if run.cfg.SpecialKeys {
		key = SpecialKeys[i%int64(len(SpecialKeys))]
	}
	return "/" + run.cfg.Bucket + "/" + run.cfg.Prefix + key
}

// newRequest builds a request whose path goes on the wire exactly as raw.
func (run *loadRun) newRequest(ctx context.Context, method, raw string, body io.Reader) (*http.Request, error) {
	decoded, err := url.PathUnescape(raw)
	if err != nil {
		return nil, fmt.Errorf("path %q: %w", raw, err)
	}
	u := &url.URL{Scheme: run.base.Scheme, Host: run.base.Host, Path: decoded, RawPath: raw}
	if u.EscapedPath() != raw {
		return nil, fmt.Errorf("path %q cannot be sent unchanged (it would go out as %q)", raw, u.EscapedPath())
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	req.URL = u // keep RawPath exactly as given
	req.Host = run.host
	return req, nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (run *loadRun) one(ctx context.Context, i int64, res *LoadResult) {
	cfg := run.cfg
	isPut := float64(i%100) < cfg.PutRatio*100
	raw := run.rawPath(i)
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	var (
		req     *http.Request
		err     error
		body    *countingReader
		expect  bool
		payload = EmptyPayloadHash
	)
	if isPut {
		payload = run.payloadHash
		if cfg.Size > 0 {
			body = &countingReader{r: bytes.NewReader(run.payload)}
			req, err = run.newRequest(ctx, http.MethodPut, raw, body)
		} else {
			req, err = run.newRequest(ctx, http.MethodPut, raw, http.NoBody)
		}
		if err == nil {
			req.ContentLength = cfg.Size
			if expect = cfg.Expect && cfg.Size > 0; expect {
				req.Header.Set("Expect", "100-continue")
			}
		}
	} else {
		req, err = run.newRequest(ctx, http.MethodGet, raw, nil)
		if err == nil && cfg.Check {
			req.Header.Set(HeaderSize, strconv.FormatInt(cfg.Size, 10))
		}
	}
	res.Requests++
	if err != nil {
		res.TransportErrors++
		res.sample("%v", err)
		return
	}
	if cfg.Creds != nil {
		Sign(req, *cfg.Creds, payload, time.Now())
	}
	req.Header.Set("User-Agent", "dvlab-loadgen")

	start := time.Now()
	resp, err := run.client.Do(req)
	if err != nil {
		res.TransportErrors++
		res.sample("%s %s: %v", req.Method, raw, err)
		return
	}
	sum := sha256.New()
	n, err := io.Copy(sum, resp.Body)
	resp.Body.Close()
	elapsed := time.Since(start)
	if err != nil {
		res.TransportErrors++
		res.sample("%s %s: reading the response: %v", req.Method, raw, err)
		return
	}
	res.latencies = append(res.latencies, elapsed)
	res.Status[resp.StatusCode]++
	res.BytesReceived += n
	var sent int64
	if body != nil {
		sent = body.n
		res.BytesSent += sent
	}
	if b := resp.Header.Get(HeaderBackend); b != "" {
		res.Backends[b]++
	}
	injected := resp.Header.Get(HeaderInjected) != ""
	if injected {
		res.Injected++
	}
	if !cfg.Check {
		return
	}

	fail := func(format string, args ...any) {
		res.CheckFailures++
		res.sample("%s %s: %s", req.Method, raw, fmt.Sprintf(format, args...))
	}
	if resp.Header.Get(HeaderBackend) == "" {
		fail("status %d did not come from a fake backend (no %s header)", resp.StatusCode, HeaderBackend)
		return
	}
	if got := resp.Header.Get(HeaderSeenHost); got != run.host {
		fail("backend saw Host %q, sent %q", got, run.host)
	}
	if got := resp.Header.Get(HeaderSeenPath); got != raw {
		fail("backend saw path %q", got)
	}
	if expect && !strings.EqualFold(resp.Header.Get(HeaderSeenExpect), "100-continue") {
		fail("backend did not receive the Expect header (saw %q)", resp.Header.Get(HeaderSeenExpect))
	}
	for _, name := range strings.Split(resp.Header.Get(HeaderSeenHeaders), ",") {
		if strings.HasPrefix(name, "x-envoy-") {
			fail("backend received the proxy header %q", name)
		}
	}
	if injected {
		// The backend refused before reading the body. If the Expect header
		// reached it, no 100 Continue was sent and no body should have left.
		if expect && sent > 0 && elapsed < cfg.ExpectTimeout {
			fail("sent %d body bytes although the backend refused the request before 100 Continue", sent)
		}
		return
	}
	if e := resp.Header.Get(HeaderSigError); e != "" {
		fail("backend rejected the signature: %s", e)
		return
	}
	if resp.StatusCode != http.StatusOK {
		fail("status %d", resp.StatusCode)
		return
	}
	if isPut {
		if got := resp.Header.Get(HeaderBodySHA256); got != run.payloadHash {
			fail("backend received a different body: %s bytes, SHA-256 %s", resp.Header.Get(HeaderBodyBytes), got)
		}
	} else if got := hex.EncodeToString(sum.Sum(nil)); n != cfg.Size || got != PatternSHA256(raw, cfg.Size) {
		fail("GET returned %d bytes (want %d) or the wrong content", n, cfg.Size)
	}
}

// send signs and sends one request, reads the whole response, and records
// it. It returns the status, the body's length and SHA-256, and the start
// of the body (an S3 error says why in XML); status 0 means the request
// failed in transport (already recorded).
func (run *loadRun) send(ctx context.Context, res *LoadResult, method, raw string, body []byte) (status int, n int64, sum, head string) {
	ctx, cancel := context.WithTimeout(ctx, run.cfg.Timeout)
	defer cancel()
	var rd io.Reader
	payload := EmptyPayloadHash
	if body != nil {
		rd, payload = bytes.NewReader(body), run.payloadHash
	}
	req, err := run.newRequest(ctx, method, raw, rd)
	if err != nil {
		res.TransportErrors++
		res.sample("%v", err)
		return 0, 0, "", ""
	}
	if body != nil {
		req.ContentLength = int64(len(body))
		if run.cfg.Expect && len(body) > 0 {
			req.Header.Set("Expect", "100-continue")
		}
	}
	if run.cfg.Creds != nil {
		Sign(req, *run.cfg.Creds, payload, time.Now())
	}
	req.Header.Set("User-Agent", "dvlab-loadgen")
	start := time.Now()
	resp, err := run.client.Do(req)
	if err != nil {
		res.TransportErrors++
		res.sample("%s %s: %v", method, raw, err)
		return 0, 0, "", ""
	}
	h := sha256.New()
	var first bytes.Buffer
	n, err = io.Copy(h, io.TeeReader(resp.Body, &limitedBuffer{b: &first, left: 300}))
	resp.Body.Close()
	if err != nil {
		res.TransportErrors++
		res.sample("%s %s: reading the response: %v", method, raw, err)
		return 0, 0, "", ""
	}
	res.latencies = append(res.latencies, time.Since(start))
	res.Status[resp.StatusCode]++
	res.BytesReceived += n
	res.BytesSent += int64(len(body))
	return resp.StatusCode, n, hex.EncodeToString(h.Sum(nil)), strings.Join(strings.Fields(first.String()), " ")
}

// limitedBuffer keeps the first bytes written to it and drops the rest.
type limitedBuffer struct {
	b    *bytes.Buffer
	left int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if take := min(len(p), l.left); take > 0 {
		l.b.Write(p[:take])
		l.left -= take
	}
	return len(p), nil
}

// roundtrip is one PUT, GET, DELETE cycle on a key only this run uses.
func (run *loadRun) roundtrip(ctx context.Context, i int64, res *LoadResult) {
	res.Requests++
	raw := fmt.Sprintf("/%s/%srt-%s-%06d", run.cfg.Bucket, run.cfg.Prefix, run.id, i)
	fail := func(format string, args ...any) {
		res.CheckFailures++
		res.sample("%s: %s", raw, fmt.Sprintf(format, args...))
	}
	status, _, _, head := run.send(ctx, res, http.MethodPut, raw, run.payload)
	if status == 0 {
		return
	}
	if status != http.StatusOK {
		fail("PUT answered %d: %s", status, head)
		return
	}
	ok := true
	status, n, sum, head := run.send(ctx, res, http.MethodGet, raw, nil)
	switch {
	case status == 0:
		ok = false
	case status != http.StatusOK:
		ok = false
		fail("GET answered %d: %s", status, head)
	case n != run.cfg.Size || sum != run.payloadHash:
		ok = false
		fail("GET returned %d bytes with SHA-256 %s; %d bytes with %s were stored", n, sum, run.cfg.Size, run.payloadHash)
	}
	// Delete even after a bad GET: the object is this run's to clean up.
	status, _, _, head = run.send(ctx, res, http.MethodDelete, raw, nil)
	switch {
	case status == 0:
		ok = false
	case status != http.StatusNoContent && status != http.StatusOK:
		ok = false
		fail("DELETE answered %d; the object may be left in the bucket: %s", status, head)
	}
	if ok {
		res.Roundtrips++
	}
}
