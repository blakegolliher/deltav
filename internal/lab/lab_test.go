package lab

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"
	"sync"
	"testing"
	"time"
)

var labCreds = &Credentials{AccessKey: "dvlab", SecretKey: "dvlab-secret"}

func startFake(t *testing.T, cfg FakeS3Config) (string, *FakeS3) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Name == "" {
		cfg.Name = ln.Addr().String()
	}
	f := NewFakeS3(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("fake S3: %v", err)
		}
	})
	return "http://" + ln.Addr().String(), f
}

func signedFake(t *testing.T) (string, *FakeS3) {
	return startFake(t, FakeS3Config{Keys: map[string]string{labCreds.AccessKey: labCreds.SecretKey}})
}

func mustRun(t *testing.T, cfg LoadConfig) *LoadResult {
	t.Helper()
	res, err := RunLoad(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func checked(endpoint string) LoadConfig {
	return LoadConfig{
		Endpoint: endpoint, Bucket: "lab-bucket", Creds: labCreds, Concurrency: 4, Requests: 200,
		PutRatio: 0.5, Size: 4096, SpecialKeys: true, Expect: true, Check: true,
	}
}

// With nothing in between, every check passes. This also proves the client
// sends each special key unchanged and that signer and verifier agree.
func TestLoadgenDirectToFake(t *testing.T) {
	endpoint, fake := signedFake(t)
	res := mustRun(t, checked(endpoint))
	if res.CheckFailures != 0 || res.TransportErrors != 0 {
		t.Fatalf("%d check failures, %d transport errors:\n%s", res.CheckFailures, res.TransportErrors, strings.Join(res.Samples, "\n"))
	}
	if res.Requests != 200 || res.Status[200] != 200 {
		t.Fatalf("requests %d, status %v", res.Requests, res.Status)
	}
	if res.BytesSent != 100*4096 || res.BytesReceived != 100*4096 {
		t.Errorf("bytes sent %d received %d, want %d each", res.BytesSent, res.BytesReceived, 100*4096)
	}
	if len(res.Backends) != 1 {
		t.Errorf("backends = %v", res.Backends)
	}
	// The backend counts a response's bytes just after writing them, which
	// can be a moment after the client has read them.
	st := fake.Stats()
	for deadline := time.Now().Add(2 * time.Second); st.BytesOut != 100*4096 && time.Now().Before(deadline); st = fake.Stats() {
		time.Sleep(5 * time.Millisecond)
	}
	if st.Requests != 200 || st.BytesIn != 100*4096 || st.BytesOut != 100*4096 || st.SigFailures != 0 {
		t.Errorf("fake stats = %+v", st)
	}
	if res.Latency.P50 <= 0 || res.Latency.Max < res.Latency.P99 || res.Latency.P99 < res.Latency.P50 {
		t.Errorf("latency = %+v", res.Latency)
	}
}

// tamperingProxy stands where Envoy will, and breaks one thing.
func tamperingProxy(t *testing.T, backend string, rewrite func(*httputil.ProxyRequest)) string {
	t.Helper()
	target, err := url.Parse(backend)
	if err != nil {
		t.Fatal(err)
	}
	p := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) {
		r.Out.URL.Scheme, r.Out.URL.Host = target.Scheme, target.Host
		r.Out.Host = r.In.Host
		rewrite(r)
	}}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return srv.URL
}

// The checks must catch what a misconfigured proxy would do; otherwise a
// passing run through Envoy proves nothing.
func TestCheckDetectsProxyTampering(t *testing.T) {
	backend, _ := signedFake(t)
	for name, tc := range map[string]struct {
		rewrite func(*httputil.ProxyRequest)
		want    string
	}{
		"host rewritten": {func(r *httputil.ProxyRequest) { r.Out.Host = "s3.internal.example" }, "backend saw Host"},
		"path normalized": {func(r *httputil.ProxyRequest) {
			r.Out.URL.Path, r.Out.URL.RawPath = path.Clean(r.Out.URL.Path), ""
		}, "backend saw path"},
		"escaped slash decoded": {func(r *httputil.ProxyRequest) { r.Out.URL.RawPath = "" }, "backend saw path"},
		"proxy header added":    {func(r *httputil.ProxyRequest) { r.Out.Header.Set("X-Envoy-Expected-Rq-Timeout-Ms", "15000") }, "proxy header"},
		"signed header altered": {func(r *httputil.ProxyRequest) { r.Out.Header.Set("X-Amz-Date", "20200101T000000Z") }, "rejected the signature"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := checked(tamperingProxy(t, backend, tc.rewrite))
			cfg.Expect = false // Go's reverse proxy is not the subject here
			res := mustRun(t, cfg)
			if res.CheckFailures == 0 {
				t.Fatal("tampering went unnoticed")
			}
			if !strings.Contains(strings.Join(res.Samples, "\n"), tc.want) {
				t.Errorf("no failure mentions %q:\n%s", tc.want, strings.Join(res.Samples, "\n"))
			}
		})
	}

	// And an honest proxy passes, so the failures above are the tampering.
	cfg := checked(tamperingProxy(t, backend, func(*httputil.ProxyRequest) {}))
	cfg.Expect = false
	if res := mustRun(t, cfg); res.CheckFailures != 0 || res.TransportErrors != 0 {
		t.Fatalf("honest proxy: %d check failures, %d transport errors:\n%s",
			res.CheckFailures, res.TransportErrors, strings.Join(res.Samples, "\n"))
	}
}

func TestInjectedFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cfg    FakeS3Config
		status int
	}{
		{"503 SlowDown", FakeS3Config{Rate503: 1}, 503},
		{"403 AccessDenied", FakeS3Config{Rate403: 1}, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			endpoint, fake := startFake(t, tc.cfg)
			cfg := checked(endpoint)
			cfg.Requests, cfg.PutRatio = 40, 1
			res := mustRun(t, cfg)
			if res.Status[tc.status] != 40 || res.Injected != 40 {
				t.Fatalf("status %v, injected %d", res.Status, res.Injected)
			}
			// Injected failures are expected, not check failures; and with
			// Expect: 100-continue a refused PUT never sends its body.
			if res.CheckFailures != 0 || res.TransportErrors != 0 {
				t.Fatalf("%d check failures, %d transport errors:\n%s", res.CheckFailures, res.TransportErrors, strings.Join(res.Samples, "\n"))
			}
			if res.BytesSent != 0 || fake.Stats().BytesIn != 0 {
				t.Errorf("bodies were sent to a backend that refused them: client %d bytes, backend %d", res.BytesSent, fake.Stats().BytesIn)
			}
			st := fake.Stats()
			if st.Injected403+st.Injected503 != 40 {
				t.Errorf("fake stats = %+v", st)
			}
		})
	}
}

// Every-Nth failures are exact and never consecutive.
func TestEveryNthFailure(t *testing.T) {
	endpoint, fake := startFake(t, FakeS3Config{Every403: 3, Every503: 7})
	var codes []int
	for i := 0; i < 42; i++ {
		resp, err := http.Get(endpoint + "/bucket/key")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		codes = append(codes, resp.StatusCode)
	}
	// Requests 7, 14, ... are 503; of the rest, 3, 6, 9, ... are 403.
	for i, code := range codes {
		n, want := i+1, 200
		switch {
		case n%7 == 0:
			want = 503
		case n%3 == 0:
			want = 403
		}
		if code != want {
			t.Errorf("request %d: %d, want %d", n, code, want)
		}
		if i > 0 && code == 503 && codes[i-1] == 503 {
			t.Errorf("requests %d and %d are both 503", n-1, n)
		}
	}
	if st := fake.Stats(); st.Injected503 != 6 || st.Injected403 != 12 {
		t.Errorf("stats: %+v", st)
	}
}

func TestMaxInFlight(t *testing.T) {
	endpoint, fake := startFake(t, FakeS3Config{Latency: 100 * time.Millisecond})
	res := mustRun(t, LoadConfig{Endpoint: endpoint, Bucket: "b", Concurrency: 6, Requests: 18})
	if res.Status[200] != 18 {
		t.Fatalf("status %v", res.Status)
	}
	if got := fake.Stats().MaxInFlight; got != 6 {
		t.Errorf("max in flight = %d with 6 workers and a slow backend", got)
	}
	fake.ResetMaxInFlight()
	mustRun(t, LoadConfig{Endpoint: endpoint, Bucket: "b", Concurrency: 2, Requests: 4})
	if got := fake.Stats().MaxInFlight; got != 2 {
		t.Errorf("max in flight after a reset = %d with 2 workers", got)
	}
}

func TestInjectedFailureBody(t *testing.T) {
	endpoint, _ := startFake(t, FakeS3Config{Rate503: 1})
	resp, err := http.Get(endpoint + "/bucket/a&b")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 503 || resp.Header.Get(HeaderInjected) != "503" ||
		!strings.Contains(string(body), "<Code>SlowDown</Code>") || !strings.Contains(string(body), "<Resource>/bucket/a&amp;b</Resource>") {
		t.Fatalf("%d %v\n%s", resp.StatusCode, resp.Header, body)
	}
}

func TestWrongSecretIsRejected(t *testing.T) {
	endpoint, fake := signedFake(t)
	cfg := checked(endpoint)
	cfg.Requests, cfg.Expect = 20, false
	cfg.Creds = &Credentials{AccessKey: labCreds.AccessKey, SecretKey: "wrong"}
	res := mustRun(t, cfg)
	if res.Status[403] != 20 || res.CheckFailures != 20 || fake.Stats().SigFailures != 20 {
		t.Fatalf("status %v, %d check failures, fake %+v", res.Status, res.CheckFailures, fake.Stats())
	}
	if !strings.Contains(res.Samples[0], "signature does not match") {
		t.Errorf("sample = %q", res.Samples[0])
	}
}

func TestUnsignedAndCustomHost(t *testing.T) {
	endpoint, _ := startFake(t, FakeS3Config{})
	cfg := checked(endpoint)
	cfg.Creds, cfg.Host, cfg.SpecialKeys, cfg.Requests = nil, "bucket.s3.example.com:9000", false, 20
	res := mustRun(t, cfg)
	if res.CheckFailures != 0 || res.Status[200] != 20 {
		t.Fatalf("status %v, %d check failures:\n%s", res.Status, res.CheckFailures, strings.Join(res.Samples, "\n"))
	}
}

func TestLoadByDuration(t *testing.T) {
	endpoint, _ := startFake(t, FakeS3Config{Latency: 5 * time.Millisecond})
	res := mustRun(t, LoadConfig{Endpoint: endpoint, Bucket: "b", Concurrency: 2, Duration: 200 * time.Millisecond})
	if res.Requests < 10 || res.Status[200] != res.Requests {
		t.Fatalf("requests %d, status %v", res.Requests, res.Status)
	}
	if res.Latency.P50 < 5 {
		t.Errorf("p50 %.2f ms is below the injected 5 ms latency", res.Latency.P50)
	}
}

func TestRunLoadRejectsBadConfig(t *testing.T) {
	for _, cfg := range []LoadConfig{
		{Endpoint: "127.0.0.1:9000", Bucket: "b", Requests: 1},
		{Endpoint: "http://127.0.0.1:9000", Requests: 1},
		{Endpoint: "http://127.0.0.1:9000", Bucket: "a/b", Requests: 1},
		{Endpoint: "http://127.0.0.1:9000", Bucket: "b"},
		{Endpoint: "http://127.0.0.1:9000", Bucket: "b", Requests: 1, PutRatio: 1.5},
		{Endpoint: "http://127.0.0.1:9000", Bucket: "b", Requests: 1, Size: -1},
	} {
		if _, err := RunLoad(context.Background(), cfg); err == nil {
			t.Errorf("%+v: want an error", cfg)
		}
	}
}

func TestFakeMethods(t *testing.T) {
	endpoint, _ := startFake(t, FakeS3Config{GetSize: 10})
	do := func(method string) *http.Response {
		req, _ := http.NewRequest(method, endpoint+"/bucket/key", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	if r := do("HEAD"); r.StatusCode != 200 || r.ContentLength != 10 {
		t.Errorf("HEAD: %d, length %d", r.StatusCode, r.ContentLength)
	}
	if r := do("DELETE"); r.StatusCode != 204 {
		t.Errorf("DELETE: %d", r.StatusCode)
	}
	if r := do("PATCH"); r.StatusCode != 405 {
		t.Errorf("PATCH: %d", r.StatusCode)
	}
}

func TestPatternReader(t *testing.T) {
	read := func(seed string, n int64) []byte {
		b, err := io.ReadAll(PatternReader(seed, n))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for _, n := range []int64{0, 1, 31, 4096, 4097, 100000} {
		if got := int64(len(read("seed", n))); got != n {
			t.Errorf("PatternReader(%d) produced %d bytes", n, got)
		}
	}
	a, b := read("/bucket/a", 10000), read("/bucket/b", 10000)
	if string(a) == string(b) {
		t.Error("different seeds produced the same bytes")
	}
	if string(a) != string(read("/bucket/a", 10000)) {
		t.Error("the same seed produced different bytes")
	}
	if string(a[:5000]) != string(read("/bucket/a", 5000)) {
		t.Error("a shorter read is not a prefix of a longer one")
	}
	sum := sha256.Sum256(a)
	if PatternSHA256("/bucket/a", 10000) != hex.EncodeToString(sum[:]) {
		t.Error("PatternSHA256 disagrees with PatternReader")
	}
}

func TestPercentileMS(t *testing.T) {
	var d []time.Duration
	for i := 1; i <= 100; i++ {
		d = append(d, time.Duration(i)*time.Millisecond)
	}
	for p, want := range map[float64]float64{0.50: 50, 0.90: 90, 0.99: 99, 1: 100} {
		if got := percentileMS(d, p); got != want {
			t.Errorf("p%.0f = %v, want %v", p*100, got, want)
		}
	}
	if percentileMS(nil, 0.5) != 0 {
		t.Error("empty input")
	}
	if got := percentileMS(d[:1], 0.99); got != 1 {
		t.Errorf("single sample p99 = %v", got)
	}
}

// memS3 is the least S3 that makes a round trip meaningful: it stores what
// is PUT and returns it on GET. corrupt makes it return something else.
type memS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	corrupt bool
	deletes int
}

func (m *memS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _, _ := strings.Cut(r.RequestURI, "?")
	secret := func(k string) (string, bool) { return labCreds.SecretKey, k == labCreds.AccessKey }
	if err := Verify(r, raw, secret, time.Now(), 15*time.Minute); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	switch r.Method {
	case http.MethodPut:
		b, _ := io.ReadAll(r.Body)
		m.objects[raw] = b
	case http.MethodGet:
		b, ok := m.objects[raw]
		if !ok {
			http.Error(w, "NoSuchKey", http.StatusNotFound)
			return
		}
		if m.corrupt && len(b) > 0 {
			b = append([]byte{b[0] ^ 0xff}, b[1:]...)
		}
		_, _ = w.Write(b)
	case http.MethodDelete:
		delete(m.objects, raw)
		m.deletes++
		w.WriteHeader(http.StatusNoContent)
	}
}

func TestRoundtrip(t *testing.T) {
	store := &memS3{objects: map[string][]byte{}}
	srv := httptest.NewServer(store)
	defer srv.Close()
	cfg := LoadConfig{Endpoint: srv.URL, Bucket: "bucket", Creds: labCreds, Concurrency: 4, Requests: 40, Size: 10000, Roundtrip: true, Expect: true}

	res := mustRun(t, cfg)
	if res.Roundtrips != 40 || res.Requests != 40 || res.CheckFailures != 0 || res.TransportErrors != 0 {
		t.Fatalf("round trips %d of %d, %d failures, %d transport errors:\n%s", res.Roundtrips, res.Requests, res.CheckFailures, res.TransportErrors, strings.Join(res.Samples, "\n"))
	}
	// Three requests per cycle, and nothing left behind.
	if res.Status[200] != 80 || res.Status[204] != 40 || res.BytesSent != 400000 || res.BytesReceived != 400000 {
		t.Errorf("status %v, %d bytes sent, %d received", res.Status, res.BytesSent, res.BytesReceived)
	}
	if len(store.objects) != 0 || store.deletes != 40 {
		t.Errorf("%d objects left, %d deletes", len(store.objects), store.deletes)
	}

	// Bytes that come back different are a failure, and the object is
	// still cleaned up.
	store.corrupt = true
	res = mustRun(t, cfg)
	if res.Roundtrips != 0 || res.CheckFailures != 40 || !strings.Contains(res.Samples[0], "were stored") || len(store.objects) != 0 {
		t.Errorf("corrupted GETs: %d verified, %d failures, %d objects left: %v", res.Roundtrips, res.CheckFailures, len(store.objects), res.Samples[:1])
	}

	// Wrong credentials: the PUT is refused and the cycle stops there.
	bad := cfg
	bad.Creds = &Credentials{AccessKey: labCreds.AccessKey, SecretKey: "wrong"}
	res = mustRun(t, bad)
	if res.Roundtrips != 0 || res.CheckFailures != 40 || res.Status[403] != 40 || !strings.Contains(res.Samples[0], "PUT answered 403") {
		t.Errorf("wrong secret: %d verified, status %v, %v", res.Roundtrips, res.Status, res.Samples[:1])
	}
	both := cfg
	both.Check = true
	if _, err := RunLoad(context.Background(), both); err == nil {
		t.Error("--check and --roundtrip together were accepted")
	}
}
