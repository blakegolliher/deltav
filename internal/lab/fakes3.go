package lab

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Headers the fake backend adds to every response, so a client can tell
// exactly what arrived after passing through Envoy.
const (
	HeaderBackend     = "X-Fake-Backend"      // which backend answered
	HeaderSeenHost    = "X-Fake-Seen-Host"    // the Host header received
	HeaderSeenPath    = "X-Fake-Seen-Path"    // the request path, byte for byte
	HeaderSeenExpect  = "X-Fake-Seen-Expect"  // the Expect header received, if any
	HeaderSeenHeaders = "X-Fake-Seen-Headers" // lower-case names of all request headers received
	HeaderInjected    = "X-Fake-Injected"     // set on a deliberately injected 403 or 503
	HeaderSigError    = "X-Fake-Sig-Error"    // why signature verification failed
	HeaderBodyBytes   = "X-Fake-Body-Bytes"   // PUT: body bytes received
	HeaderBodySHA256  = "X-Fake-Body-Sha256"  // PUT: SHA-256 of the body received

	// HeaderSize is a request header: how many bytes a GET should return.
	HeaderSize = "X-Fake-Size"
)

// FakeS3Config configures one fake backend.
type FakeS3Config struct {
	Name    string        // reported in X-Fake-Backend
	Latency time.Duration // added to every request
	Jitter  time.Duration // up to this much more, uniformly
	Rate403 float64       // fraction of requests answered 403 AccessDenied
	Rate503 float64       // fraction of requests answered 503 SlowDown
	// Every403 and Every503 answer every Nth request with that status, on
	// top of the random rates. Unlike a rate they never fail several
	// requests in a row, so they do not trip Envoy's outlier detection.
	Every403 int
	Every503 int
	Keys     map[string]string // access key -> secret; empty: signatures are not checked
	GetSize  int64             // GET body size when the request does not say; default 1024
	MaxSkew  time.Duration     // allowed X-Amz-Date skew; default 15 minutes
}

// FakeStats counts what a backend has seen.
type FakeStats struct {
	Requests    int64 `json:"requests"`
	Injected403 int64 `json:"injected_403"`
	Injected503 int64 `json:"injected_503"`
	SigFailures int64 `json:"signature_failures"`
	BytesIn     int64 `json:"bytes_in"`
	BytesOut    int64 `json:"bytes_out"`
	// MaxInFlight is the most requests the backend has had in hand at
	// once, since it started or since ResetMaxInFlight.
	MaxInFlight int64 `json:"max_in_flight"`
}

// FakeS3 is a stateless stand-in for an S3 endpoint. PUT reads and hashes
// the body; GET returns a deterministic pattern derived from the path. It
// stores nothing, so any number of backends are interchangeable.
type FakeS3 struct {
	cfg FakeS3Config

	requests, injected403, injected503, sigFailures, bytesIn, bytesOut atomic.Int64
	inFlight, maxInFlight                                              atomic.Int64
}

// NewFakeS3 returns a backend with defaults applied.
func NewFakeS3(cfg FakeS3Config) *FakeS3 {
	if cfg.GetSize <= 0 {
		cfg.GetSize = 1024
	}
	if cfg.MaxSkew <= 0 {
		cfg.MaxSkew = 15 * time.Minute
	}
	return &FakeS3{cfg: cfg}
}

// Stats returns the backend's counters.
func (f *FakeS3) Stats() FakeStats {
	return FakeStats{
		Requests: f.requests.Load(), Injected403: f.injected403.Load(), Injected503: f.injected503.Load(),
		SigFailures: f.sigFailures.Load(), BytesIn: f.bytesIn.Load(), BytesOut: f.bytesOut.Load(),
		MaxInFlight: f.maxInFlight.Load(),
	}
}

// ResetMaxInFlight starts the high-water mark of concurrent requests again
// from what is in hand now.
func (f *FakeS3) ResetMaxInFlight() { f.maxInFlight.Store(f.inFlight.Load()) }

// Serve runs the backend on ln until ctx ends. It deliberately uses no
// ServeMux, which would redirect paths containing "//" or dot segments.
func (f *FakeS3) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{Handler: f, ReadHeaderTimeout: 10 * time.Second}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = srv.Close()
		case <-done:
		}
	}()
	err := srv.Serve(ln)
	close(done)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func (f *FakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	seq := f.requests.Add(1)
	now := f.inFlight.Add(1)
	defer f.inFlight.Add(-1)
	for {
		peak := f.maxInFlight.Load()
		if now <= peak || f.maxInFlight.CompareAndSwap(peak, now) {
			break
		}
	}
	rawPath, _, _ := strings.Cut(r.RequestURI, "?")

	h := w.Header()
	h.Set(HeaderBackend, f.cfg.Name)
	h.Set(HeaderSeenHost, r.Host)
	h.Set(HeaderSeenPath, rawPath)
	if e := r.Header.Get("Expect"); e != "" {
		h.Set(HeaderSeenExpect, e)
	}
	names := make([]string, 0, len(r.Header))
	for name := range r.Header {
		names = append(names, strings.ToLower(name))
	}
	sort.Strings(names)
	h.Set(HeaderSeenHeaders, strings.Join(names, ","))
	h.Set("Server", "dvlab-fakes3")
	h.Set("X-Amz-Request-Id", strconv.FormatUint(rand.Uint64(), 16))

	if d := f.cfg.Latency; d > 0 || f.cfg.Jitter > 0 {
		if f.cfg.Jitter > 0 {
			d += time.Duration(rand.Int64N(int64(f.cfg.Jitter)))
		}
		select {
		case <-time.After(d):
		case <-r.Context().Done():
			return
		}
	}

	// Injected failures answer before the body is read, as a server that
	// refuses a request does. With "Expect: 100-continue" the client then
	// never sends the body, provided the proxy passed the Expect through.
	every := func(n int) bool { return n > 0 && seq%int64(n) == 0 }
	u := rand.Float64()
	switch {
	case every(f.cfg.Every503) || u < f.cfg.Rate503:
		f.injected503.Add(1)
		h.Set(HeaderInjected, "503")
		s3Error(w, http.StatusServiceUnavailable, "SlowDown", "Please reduce your request rate.", rawPath)
		return
	case every(f.cfg.Every403) || u < f.cfg.Rate503+f.cfg.Rate403:
		f.injected403.Add(1)
		h.Set(HeaderInjected, "403")
		s3Error(w, http.StatusForbidden, "AccessDenied", "Access Denied", rawPath)
		return
	}

	if len(f.cfg.Keys) > 0 {
		secretFor := func(k string) (string, bool) { s, ok := f.cfg.Keys[k]; return s, ok }
		if err := Verify(r, rawPath, secretFor, time.Now(), f.cfg.MaxSkew); err != nil {
			f.sigFailures.Add(1)
			h.Set(HeaderSigError, err.Error())
			s3Error(w, http.StatusForbidden, "SignatureDoesNotMatch", err.Error(), rawPath)
			return
		}
	}

	switch r.Method {
	case http.MethodPut, http.MethodPost:
		sum := sha256.New()
		n, err := io.Copy(sum, r.Body)
		f.bytesIn.Add(n)
		if err != nil {
			s3Error(w, http.StatusBadRequest, "IncompleteBody", err.Error(), rawPath)
			return
		}
		got := hex.EncodeToString(sum.Sum(nil))
		if declared := r.Header.Get("X-Amz-Content-Sha256"); isHexSHA256(declared) && declared != got {
			s3Error(w, http.StatusBadRequest, "XAmzContentSHA256Mismatch",
				"The provided x-amz-content-sha256 header does not match what was computed.", rawPath)
			return
		}
		h.Set(HeaderBodyBytes, strconv.FormatInt(n, 10))
		h.Set(HeaderBodySHA256, got)
		h.Set("ETag", `"`+got[:32]+`"`)
		w.WriteHeader(http.StatusOK)
	case http.MethodGet, http.MethodHead:
		size := f.cfg.GetSize
		if v := r.Header.Get(HeaderSize); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				s3Error(w, http.StatusBadRequest, "InvalidArgument", "bad "+HeaderSize, rawPath)
				return
			}
			size = n
		}
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			n, _ := io.Copy(w, PatternReader(rawPath, size))
			f.bytesOut.Add(n)
		}
	case http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	default:
		s3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "The specified method is not allowed.", rawPath)
	}
}

func isHexSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func s3Error(w http.ResponseWriter, status int, code, message, resource string) {
	var esc strings.Builder
	_ = xml.EscapeText(&esc, []byte(message))
	msg := esc.String()
	esc.Reset()
	_ = xml.EscapeText(&esc, []byte(resource))
	body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>`+"\n"+
		`<Error><Code>%s</Code><Message>%s</Message><Resource>%s</Resource></Error>`+"\n", code, msg, esc.String())
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// PatternReader returns n bytes of a repeating pattern derived from seed.
// The fake serves it as a GET body and the load generator recomputes it to
// check what came back.
func PatternReader(seed string, n int64) io.Reader {
	block := make([]byte, 0, 4096)
	sum := sha256.Sum256([]byte(seed))
	for len(block) < cap(block) {
		block = append(block, sum[:]...)
		sum = sha256.Sum256(sum[:])
	}
	return &patternReader{block: block, left: n}
}

type patternReader struct {
	block []byte
	off   int
	left  int64
}

func (p *patternReader) Read(b []byte) (int, error) {
	if p.left <= 0 {
		return 0, io.EOF
	}
	n := 0
	for n < len(b) && p.left > 0 {
		c := copy(b[n:], p.block[p.off:])
		if int64(c) > p.left {
			c = int(p.left)
		}
		n += c
		p.left -= int64(c)
		p.off = (p.off + c) % len(p.block)
	}
	return n, nil
}

// PatternSHA256 is the hex SHA-256 of PatternReader(seed, n).
func PatternSHA256(seed string, n int64) string {
	sum := sha256.New()
	_, _ = io.Copy(sum, PatternReader(seed, n))
	return hex.EncodeToString(sum.Sum(nil))
}
