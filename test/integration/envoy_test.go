//go:build integration

// Package integration runs the daemon, fake S3 backends and real Envoy
// processes on one host and asserts on the daemon's HTTP API and on what
// the backends received. Run it with `make integration`, which fetches the
// pinned Envoy release binary first.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"deltav/internal/fleet"
	"deltav/internal/lab"
	"deltav/internal/server"
	"deltav/internal/vast/vasttest"
	"deltav/internal/version"
	"deltav/internal/xds"
)

var creds = &lab.Credentials{AccessKey: "dvlab", SecretKey: "dvlab-secret"}

// envoyBin finds the Envoy binary: $ENVOY_BIN, as `make integration` sets
// it, or the Makefile's download location.
func envoyBin(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("ENVOY_BIN")
	if bin == "" {
		matches, _ := filepath.Glob(filepath.Join("..", "..", ".cache", "envoy", "envoy-"+version.EnvoyPinned+"-linux-*"))
		if len(matches) > 0 {
			bin = matches[0]
		}
	}
	if bin == "" {
		t.Fatalf("no Envoy binary: run `make envoy`, or set ENVOY_BIN to an Envoy %s binary", version.EnvoyPinned)
	}
	abs, err := filepath.Abs(bin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("Envoy binary: %v", err)
	}
	return abs
}

// backends starts n identical fake S3 servers.
func backends(t *testing.T, n int) ([]xds.Endpoint, []*lab.FakeS3) {
	t.Helper()
	return backendsWith(t, make([]lab.FakeS3Config, n)...)
}

// backendsWith starts one fake S3 server per config. On Linux they take
// distinct loopback addresses on one shared port, as VIPs do; where only
// 127.0.0.1 is usable they fall back to distinct ports.
func backendsWith(t *testing.T, cfgs ...lab.FakeS3Config) ([]xds.Endpoint, []*lab.FakeS3) {
	t.Helper()
	n := len(cfgs)
	var lns []net.Listener
	first, err := net.Listen("tcp", "127.0.0.11:0")
	if err == nil {
		lns = append(lns, first)
		port := first.Addr().(*net.TCPAddr).Port
		for i := 1; i < n; i++ {
			ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.%d:%d", 11+i, port))
			if err != nil {
				t.Fatalf("backend %d: %v", i, err)
			}
			lns = append(lns, ln)
		}
	} else {
		t.Logf("127.0.0.11 is not usable (%v): backends share 127.0.0.1 on separate ports", err)
		for i := 0; i < n; i++ {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			lns = append(lns, ln)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var eps []xds.Endpoint
	var fakes []*lab.FakeS3
	for i, ln := range lns {
		addr := ln.Addr().(*net.TCPAddr)
		cfg := cfgs[i]
		cfg.Name, cfg.Keys = addr.String(), map[string]string{creds.AccessKey: creds.SecretKey}
		f := lab.NewFakeS3(cfg)
		go func() { _ = f.Serve(ctx, ln) }()
		eps = append(eps, xds.Endpoint{IP: addr.IP.String(), Port: uint32(addr.Port)})
		fakes = append(fakes, f)
	}
	return eps, fakes
}

// freePort returns a loopback port that was free a moment ago.
func freePort(t *testing.T) uint32 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return uint32(ln.Addr().(*net.TCPAddr).Port)
}

func daemon(t *testing.T, targets ...xds.TargetSpec) *server.Server {
	t.Helper()
	s, err := server.New(server.Config{
		GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0", StaticTargets: targets,
		StateDir: t.TempDir(), VASTPollInterval: 500 * time.Millisecond,
		LoadReportInterval: time.Second, // the default 5s would make these tests slow
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("daemon: %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	return s
}

// envoyWorkers is how many worker threads the tests' Envoys run unless a
// test asks otherwise. More than one, so that what holds only for a single
// worker does not pass by accident.
const envoyWorkers = 2

// envoy starts one real Envoy with the bootstrap `deltav bootstrap` prints,
// naming itself through --service-node as a client host's unit does.
func envoy(t *testing.T, s *server.Server, nodeID string) {
	t.Helper()
	envoyWith(t, s, nodeID, envoyWorkers)
}

// envoyWith is envoy with a chosen number of worker threads.
func envoyWith(t *testing.T, s *server.Server, nodeID string, workers int) {
	t.Helper()
	dir := t.TempDir()
	boot, err := xds.BootstrapJSON(xds.BootstrapOptions{XDSAddress: s.GRPCAddr()})
	if err != nil {
		t.Fatal(err)
	}
	bootPath := filepath.Join(dir, "bootstrap.json")
	if err := os.WriteFile(bootPath, boot, 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "envoy.log")
	cmd := exec.Command(envoyBin(t),
		"-c", bootPath,
		"--service-node", nodeID,
		"--disable-hot-restart", // several Envoys on one host; no shared memory to collide on
		"--concurrency", strconv.Itoa(workers),
		"--log-level", "info",
		"--log-path", logPath,
	)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting Envoy: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
		}
		if t.Failed() {
			if b, err := os.ReadFile(logPath); err == nil {
				// Leave out the startup listing of every compiled-in
				// extension and header: it would bury what matters.
				var lines []string
				for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
					if !strings.Contains(line, "server.cc:467]") && !strings.Contains(line, "server.cc:547]") {
						lines = append(lines, line)
					}
				}
				if len(lines) > 60 {
					lines = lines[len(lines)-60:]
				}
				t.Logf("Envoy %s log (last %d lines):\n%s", nodeID, len(lines), strings.Join(lines, "\n"))
			}
		}
	})
}

type fleetResponse struct {
	ConfigVersion string         `json:"config_version"`
	Total         int            `json:"total"`
	Connected     int            `json:"connected"`
	Members       []fleet.Member `json:"members"`
}

func getFleet(t *testing.T, s *server.Server) fleetResponse {
	t.Helper()
	resp, err := http.Get("http://" + s.HTTPAddr() + "/api/v1/fleet")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var f fleetResponse
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&f) != nil {
		t.Fatalf("/api/v1/fleet: status %d", resp.StatusCode)
	}
	return f
}

// waitMember polls the fleet API until ok accepts the member.
func waitMember(t *testing.T, s *server.Server, nodeID, what string, ok func(fleet.Member, fleetResponse) bool) fleet.Member {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last *fleet.Member
	for time.Now().Before(deadline) {
		f := getFleet(t, s)
		for i, m := range f.Members {
			if m.NodeID == nodeID {
				if last = &f.Members[i]; ok(m, f) {
					return m
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	b, _ := json.MarshalIndent(last, "", "  ")
	t.Fatalf("Envoy %s never reached: %s\nlast state from /api/v1/fleet:\n%s", nodeID, what, b)
	return fleet.Member{}
}

func inSync(m fleet.Member, f fleetResponse) bool {
	return m.Connected && m.ConfigState == fleet.StateAcked && m.ConfigVersion == f.ConfigVersion
}

// waitListener waits until something accepts connections on the port: an
// ACK means Envoy accepted the listener, slightly before workers serve it.
func waitListener(t *testing.T, port uint32) {
	t.Helper()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port)))
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			c.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("nothing is listening on %s", addr)
}

func load(t *testing.T, port uint32, mutate func(*lab.LoadConfig)) *lab.LoadResult {
	t.Helper()
	cfg := lab.LoadConfig{
		Endpoint: fmt.Sprintf("http://127.0.0.1:%d", port), Bucket: "lab-bucket", Creds: creds,
		Concurrency: 8, Requests: 400, PutRatio: 0.5, Size: 64 * 1024,
		SpecialKeys: true, Expect: true, Check: true,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	res, err := lab.RunLoad(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func requireClean(t *testing.T, res *lab.LoadResult) {
	t.Helper()
	if res.CheckFailures != 0 || res.TransportErrors != 0 || res.Status[200] != res.Requests {
		t.Fatalf("load through Envoy: %d requests, status %v, %d check failures, %d transport errors:\n%s",
			res.Requests, res.Status, res.CheckFailures, res.TransportErrors, strings.Join(res.Samples, "\n"))
	}
}

// Milestone 2: a static endpoint list served over xDS to real Envoy; requests
// through the listener reach the fake S3 backends with the Host header, the
// path and the Expect header intact, and signatures still verify.
func TestStaticTargetThroughEnvoy(t *testing.T) {
	eps, fakes := backends(t, 3)
	port := freePort(t)
	s := daemon(t, xds.TargetSpec{Name: "s3", ListenPort: port, Endpoints: eps})
	envoy(t, s, "it-envoy-1")

	m := waitMember(t, s, "it-envoy-1", "connected and in sync", inSync)
	if m.EnvoyVersion != version.EnvoyPinned {
		t.Errorf("Envoy reports version %q, pinned %q", m.EnvoyVersion, version.EnvoyPinned)
	}
	if m.Cluster != xds.DefaultNodeCluster || len(m.Types) != 3 {
		t.Errorf("member: cluster %q, types %+v", m.Cluster, m.Types)
	}
	waitListener(t, port)

	// One explicit request first: the Host an S3 client would sign must
	// arrive unchanged, and nothing may be added in Envoy's name.
	host := "bucket.s3.example.com:9000"
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/lab-bucket/hello//world%%2Fkey", port), nil)
	req.Host = host
	lab.Sign(req, *creds, lab.EmptyPayloadHash, time.Now())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET through Envoy: %d (signature error: %q)", resp.StatusCode, resp.Header.Get(lab.HeaderSigError))
	}
	if got := resp.Header.Get(lab.HeaderSeenHost); got != host {
		t.Errorf("backend saw Host %q, sent %q", got, host)
	}
	if got := resp.Header.Get(lab.HeaderSeenPath); got != "/lab-bucket/hello//world%2Fkey" {
		t.Errorf("backend saw path %q", got)
	}
	t.Logf("request headers the backend received: %s", resp.Header.Get(lab.HeaderSeenHeaders))
	if got := resp.Header.Get("Server"); got != "dvlab-fakes3" {
		t.Errorf("Server response header = %q: Envoy replaced the backend's", got)
	}
	for name := range resp.Header {
		if strings.HasPrefix(strings.ToLower(name), "x-envoy-") {
			t.Errorf("response carries %s", name)
		}
	}

	// Then load: special keys, PUTs with Expect: 100-continue, every
	// response checked against what was sent.
	res := load(t, port, nil)
	requireClean(t, res)
	if len(res.Backends) != 3 {
		t.Errorf("load reached %d of 3 backends: %v", len(res.Backends), res.Backends)
	}
	for i, f := range fakes {
		if st := f.Stats(); st.Requests == 0 || st.SigFailures != 0 {
			t.Errorf("backend %d: %+v", i, st)
		}
	}
	t.Logf("load: %d requests, backends %v, p50 %.1f ms, p99 %.1f ms", res.Requests, res.Backends, res.Latency.P50, res.Latency.P99)

	// A backend that refuses a PUT before reading it: with proxy_100_continue
	// the client must never send the body. It refuses with 403, not 503:
	// consecutive 503s would have outlier detection eject the only host.
	refusing, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rf := lab.NewFakeS3(lab.FakeS3Config{Name: "refusing", Rate403: 1})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = rf.Serve(ctx, refusing) }()
	raddr := refusing.Addr().(*net.TCPAddr)
	port2 := freePort(t)
	if _, _, err := s.SetStaticTargets(context.Background(), []xds.TargetSpec{
		{Name: "s3", ListenPort: port, Endpoints: eps},
		{Name: "refusing", ListenPort: port2, Endpoints: []xds.Endpoint{{IP: raddr.IP.String(), Port: uint32(raddr.Port)}}},
	}); err != nil {
		t.Fatal(err)
	}
	waitMember(t, s, "it-envoy-1", "in sync with the second target", inSync)
	waitListener(t, port2)
	res = load(t, port2, func(c *lab.LoadConfig) { c.Requests, c.PutRatio, c.Concurrency = 20, 1, 2 })
	if res.Status[403] != 20 || res.Injected != 20 || res.CheckFailures != 0 {
		t.Fatalf("refused PUTs: status %v, injected %d, %d check failures:\n%s", res.Status, res.Injected, res.CheckFailures, strings.Join(res.Samples, "\n"))
	}
	if res.BytesSent != 0 || rf.Stats().BytesIn != 0 {
		t.Errorf("bodies were sent for refused PUTs: client %d bytes, backend %d bytes", res.BytesSent, rf.Stats().BytesIn)
	}
}

// Endpoint changes reach Envoy; a config Envoy rejects is reported as a NACK
// with its reason and is not re-sent in a loop; the next good config recovers.
func TestEndpointUpdateAndNACK(t *testing.T) {
	eps, fakes := backends(t, 3)
	port := freePort(t)
	target := xds.TargetSpec{Name: "s3", ListenPort: port, Endpoints: eps}
	s := daemon(t, target)
	envoy(t, s, "it-envoy-1")
	waitMember(t, s, "it-envoy-1", "connected and in sync", inSync)
	waitListener(t, port)
	requireClean(t, load(t, port, nil))

	// Remove the third endpoint: it must stop receiving requests.
	target.Endpoints = eps[:2]
	if _, changed, err := s.SetStaticTargets(context.Background(), []xds.TargetSpec{target}); err != nil || !changed {
		t.Fatalf("update: changed=%v err=%v", changed, err)
	}
	waitMember(t, s, "it-envoy-1", "in sync after removing an endpoint", inSync)
	before := fakes[2].Stats().Requests
	res := load(t, port, nil)
	requireClean(t, res)
	if got := fakes[2].Stats().Requests - before; got != 0 {
		t.Errorf("the removed endpoint received %d more requests", got)
	}
	if len(res.Backends) != 2 {
		t.Errorf("backends after removal: %v", res.Backends)
	}

	// A cluster Envoy cannot build: its CA file does not exist.
	bad := xds.TargetSpec{Name: "broken", ListenPort: freePort(t), Endpoints: eps[:1],
		TLS: &xds.TLSSpec{ServerName: "s3.example.com", CAFile: "/nonexistent/deltav-test-ca.pem"}}
	if _, _, err := s.SetStaticTargets(context.Background(), []xds.TargetSpec{target, bad}); err != nil {
		t.Fatal(err)
	}
	m := waitMember(t, s, "it-envoy-1", "a NACK", func(m fleet.Member, _ fleetResponse) bool {
		return m.ConfigState == fleet.StateNacked
	})
	t.Logf("NACK as reported by /api/v1/fleet: %s", m.ConfigError)
	if m.ConfigError == "" {
		t.Error("NACK has no error text")
	}
	// It stays a NACK (no re-send loop), and the good target keeps serving.
	time.Sleep(2 * time.Second)
	m = waitMember(t, s, "it-envoy-1", "still a NACK", func(m fleet.Member, _ fleetResponse) bool { return true })
	if m.ConfigState != fleet.StateNacked {
		t.Errorf("two seconds after the NACK the state is %s: the rejected config was pushed again", m.ConfigState)
	}
	requireClean(t, load(t, port, nil))

	// Withdraw the bad target: Envoy recovers.
	if _, _, err := s.SetStaticTargets(context.Background(), []xds.TargetSpec{target}); err != nil {
		t.Fatal(err)
	}
	waitMember(t, s, "it-envoy-1", "in sync after the bad target is withdrawn", inSync)
}

// Several Envoys on one host share a listener port (SO_REUSEPORT, Envoy's
// default), which is how this harness stands in for a fleet.
func TestTwoEnvoysShareListenerPort(t *testing.T) {
	eps, _ := backends(t, 2)
	port := freePort(t)
	s := daemon(t, xds.TargetSpec{Name: "s3", ListenPort: port, Endpoints: eps})
	envoy(t, s, "it-envoy-1")
	envoy(t, s, "it-envoy-2")
	waitMember(t, s, "it-envoy-1", "connected and in sync", inSync)
	waitMember(t, s, "it-envoy-2", "connected and in sync", inSync)
	if f := getFleet(t, s); f.Total != 2 || f.Connected != 2 {
		t.Fatalf("fleet: total %d, connected %d", f.Total, f.Connected)
	}
	waitListener(t, port)
	requireClean(t, load(t, port, func(c *lab.LoadConfig) { c.Concurrency = 16 }))
}

// The shapes of the API responses this test reads; see internal/server/api.go.
type summary struct {
	Requests uint64 `json:"requests"`
	Status   struct {
		S2xx uint64 `json:"2xx"`
		S403 uint64 `json:"403"`
		S503 uint64 `json:"503"`
	} `json:"status"`
	BytesIn   uint64 `json:"bytes_in"`
	BytesOut  uint64 `json:"bytes_out"`
	LatencyMS struct {
		P50 float64 `json:"p50"`
		P99 float64 `json:"p99"`
	} `json:"latency_ms"`
}

type endpointRow struct {
	Endpoint   string  `json:"endpoint"`
	Configured bool    `json:"configured"`
	InFlight   uint64  `json:"in_flight"`
	Stats      summary `json:"stats"`
	Load       struct {
		Issued  uint64 `json:"issued"`
		Success uint64 `json:"success"`
		Errors  uint64 `json:"errors"`
	} `json:"load"`
}

func getAPI[T any](t *testing.T, s *server.Server, path string) T {
	t.Helper()
	resp, err := http.Get("http://" + s.HTTPAddr() + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out T
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: status %d", path, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return out
}

func endpointRows(t *testing.T, s *server.Server) map[string]endpointRow {
	t.Helper()
	resp := getAPI[struct {
		Endpoints []endpointRow `json:"endpoints"`
	}](t, s, "/api/v1/endpoints?window=5m")
	out := map[string]endpointRow{}
	for _, e := range resp.Endpoints {
		out[e.Endpoint] = e
	}
	return out
}

func targetInFlight(t *testing.T, s *server.Server) uint64 {
	t.Helper()
	ov := getAPI[struct {
		Targets []struct {
			InFlight uint64 `json:"in_flight"`
		} `json:"targets"`
	}](t, s, "/api/v1/overview")
	if len(ov.Targets) != 1 {
		t.Fatalf("overview has %d targets", len(ov.Targets))
	}
	return ov.Targets[0].InFlight
}

// Milestone 3: what Envoy streams back (access logs and load reports)
// matches what the backends saw, per endpoint and per Envoy, in the API and
// in /metrics.
func TestTelemetryThroughEnvoy(t *testing.T) {
	const slow = 300 * time.Millisecond
	eps, fakes := backendsWith(t,
		lab.FakeS3Config{},              // 0: healthy and fast
		lab.FakeS3Config{Latency: slow}, // 1: healthy and slow
		// 2: refuses a third with 403 and a seventh with 503, never two
		// 503s in a row. A random 15% of 503s got this backend ejected by
		// outlier detection after 17 requests in one run (DESIGN.md,
		// amendment 17), which is real but not what this test is about.
		lab.FakeS3Config{Every403: 3, Every503: 7},
	)
	names := make([]string, len(eps))
	for i, e := range eps {
		names[i] = net.JoinHostPort(e.IP, strconv.Itoa(int(e.Port)))
	}
	port := freePort(t)
	s := daemon(t, xds.TargetSpec{Name: "s3", ListenPort: port, Endpoints: eps})
	envoy(t, s, "it-envoy-1")
	waitMember(t, s, "it-envoy-1", "connected and in sync", inSync)
	waitListener(t, port)

	// Envoy starts counting a cluster for load reports at the first report
	// tick after the cluster exists, resetting its counters as it does, so
	// whatever a new cluster carries before that tick is never reported
	// (see DESIGN.md, amendment 20). Let that tick pass before the load.
	time.Sleep(2500 * time.Millisecond)

	// Six seconds of load, so several one-second load reports land while
	// requests to the slow backend are in flight.
	done := make(chan *lab.LoadResult, 1)
	go func() {
		done <- load(t, port, func(c *lab.LoadConfig) {
			c.Requests, c.Duration, c.Concurrency, c.Size = 0, 6*time.Second, 8, 16*1024
		})
	}()
	var res *lab.LoadResult
	var peak uint64
	for res == nil {
		select {
		case res = <-done:
		case <-time.After(200 * time.Millisecond):
			if n := targetInFlight(t, s); n > peak {
				peak = n
			}
		}
	}
	if res.CheckFailures != 0 || res.TransportErrors != 0 {
		t.Fatalf("load: %d check failures, %d transport errors:\n%s", res.CheckFailures, res.TransportErrors, strings.Join(res.Samples, "\n"))
	}
	t.Logf("load: %d requests, status %v, backends %v; peak in-flight seen in the API: %d", res.Requests, res.Status, res.Backends, peak)
	if peak == 0 {
		t.Error("the API never showed a request in flight during six seconds of load to a slow backend")
	}

	// Access logs are flushed every second: wait for the counts to settle
	// on what the backends themselves counted.
	var rows map[string]endpointRow
	deadline := time.Now().Add(15 * time.Second)
	for {
		rows = endpointRows(t, s)
		settled := true
		for i, f := range fakes {
			if rows[names[i]].Stats.Requests != uint64(f.Stats().Requests) {
				settled = false
			}
		}
		if settled || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	var total uint64
	for i, f := range fakes {
		st, row := f.Stats(), rows[names[i]]
		total += uint64(st.Requests)
		t.Logf("backend %d (%s): backend saw %+v; API reports %+v", i, names[i], st, row)
		if !row.Configured {
			t.Errorf("backend %d is not marked configured", i)
		}
		if row.Stats.Requests != uint64(st.Requests) {
			t.Errorf("backend %d: API counts %d requests, the backend %d", i, row.Stats.Requests, st.Requests)
		}
		if row.Stats.Status.S403 != uint64(st.Injected403) || row.Stats.Status.S503 != uint64(st.Injected503) {
			t.Errorf("backend %d: API counts %d 403s and %d 503s, the backend injected %d and %d",
				i, row.Stats.Status.S403, row.Stats.Status.S503, st.Injected403, st.Injected503)
		}
		if row.Stats.BytesIn != uint64(st.BytesIn) {
			t.Errorf("backend %d: API counts %d bytes in, the backend read %d", i, row.Stats.BytesIn, st.BytesIn)
		}
	}
	if got := rows[names[0]].Stats.BytesOut; got != uint64(fakes[0].Stats().BytesOut) {
		t.Errorf("backend 0: API counts %d bytes out, the backend wrote %d", got, fakes[0].Stats().BytesOut)
	}
	if fakes[2].Stats().Injected403 == 0 || fakes[2].Stats().Injected503 == 0 {
		t.Errorf("backend 2 injected no failures; the test proved nothing about 403 and 503: %+v", fakes[2].Stats())
	}
	if p50 := rows[names[1]].Stats.LatencyMS.P50; p50 < float64(slow.Milliseconds())*0.85 || p50 > float64(slow.Milliseconds())*2 {
		t.Errorf("slow backend p50 = %.0f ms, its injected latency is %d ms", p50, slow.Milliseconds())
	}
	if p50 := rows[names[0]].Stats.LatencyMS.P50; p50 <= 0 || p50 > 100 {
		t.Errorf("fast backend p50 = %.1f ms", p50)
	}

	// The Envoy's own row adds up to the same total.
	member := getAPI[struct {
		Windows map[string]summary `json:"windows"`
	}](t, s, "/api/v1/fleet/it-envoy-1")
	if got := member.Windows["5m"].Requests; got != total {
		t.Errorf("fleet member counts %d requests, the backends %d", got, total)
	}

	// Load reports: nothing is in flight once the load has stopped, and the
	// request counts, which do not depend on access logs, agree too. At
	// this layer a 5xx is an error and anything else, a 403 included, is a
	// success.
	deadline = time.Now().Add(10 * time.Second)
	for {
		rows = endpointRows(t, s)
		settled := targetInFlight(t, s) == 0
		for i, f := range fakes {
			if rows[names[i]].Load.Issued != uint64(f.Stats().Requests) {
				settled = false
			}
		}
		if settled || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if n := targetInFlight(t, s); n != 0 {
		t.Errorf("%d requests still reported in flight after the load stopped", n)
	}
	for i, f := range fakes {
		row := rows[names[i]]
		t.Logf("backend %d load report totals: %+v", i, row.Load)
		st := f.Stats()
		if row.Load.Issued != uint64(st.Requests) {
			t.Errorf("backend %d: load reports count %d issued requests, the backend saw %d", i, row.Load.Issued, st.Requests)
		}
		if row.Load.Errors != uint64(st.Injected503) || row.Load.Success != uint64(st.Requests-st.Injected503) {
			t.Errorf("backend %d: load reports count %d successes and %d errors; the backend answered %d requests, %d of them 503",
				i, row.Load.Success, row.Load.Errors, st.Requests, st.Injected503)
		}
	}

	// One scrape covers it all.
	resp, err := http.Get("http://" + s.HTTPAddr() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{
		fmt.Sprintf(`deltav_endpoint_requests_total{class="2xx",cnode="",endpoint=%q,target="s3"} %d`, names[0], fakes[0].Stats().Requests),
		fmt.Sprintf(`deltav_endpoint_responses_total{cnode="",code="503",endpoint=%q,target="s3"} %d`, names[2], fakes[2].Stats().Injected503),
		fmt.Sprintf(`deltav_endpoint_request_duration_seconds_count{cnode="",endpoint=%q,target="s3"} %d`, names[1], fakes[1].Stats().Requests),
		fmt.Sprintf(`deltav_endpoint_in_flight_requests{cnode="",endpoint=%q,target="s3"} 0`, names[1]),
		`deltav_fleet_members{state="connected"} 1`,
	} {
		if !strings.Contains(string(body), want+"\n") {
			t.Errorf("/metrics lacks: %s", want)
		}
	}
}

func postAPI(t *testing.T, s *server.Server, path string, body any) {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post("http://"+s.HTTPAddr()+path, "application/json", strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		msg, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST %s: status %d: %s", path, resp.StatusCode, msg)
	}
}

type cnodeRow struct {
	Name     string        `json:"name"`
	Healthy  bool          `json:"healthy"`
	VIPs     int           `json:"vips"`
	Stats    summary       `json:"stats"`
	VIPRows  []endpointRow `json:"endpoints"`
	InFlight uint64        `json:"in_flight"`
}

func cnodeRows(t *testing.T, s *server.Server) map[string]cnodeRow {
	t.Helper()
	resp := getAPI[struct {
		CNodes []cnodeRow `json:"cnodes"`
	}](t, s, "/api/v1/cnodes?window=5m")
	out := map[string]cnodeRow{}
	for _, c := range resp.CNodes {
		out[c.Name] = c
	}
	return out
}

// Milestone 4: endpoints come from a VAST cluster. The mock VAST serves a
// real cluster's (sanitized) responses, with the six VIPs of pool "main"
// pointed at six fake S3 backends. Requests through Envoy spread over the
// VIPs; telemetry rolls up per CNode; and when VAST reports a CNode out of
// service, Envoy stops using its VIPs.
func TestVASTTargetThroughEnvoy(t *testing.T) {
	eps, fakes := backends(t, 6)
	mock, err := vasttest.New(filepath.Join("..", "..", "testdata", "vast", "v5.5.3"))
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	ips := make([]string, len(eps))
	for i, e := range eps {
		ips[i] = e.IP
		if e.Port != eps[0].Port {
			t.Skip("the backends could not share one port on this host (no 127.0.0.11-16); a VIP pool has one port")
		}
	}
	if err := mock.SetPoolVIPs("main", ips); err != nil {
		t.Fatal(err)
	}
	// Which backend sits behind which CNode, as VAST reports it.
	cnodeOf := mock.PoolCNodes("main")
	byCNode := map[string][]int{}
	for i, name := range cnodeOf {
		byCNode[name] = append(byCNode[name], i)
	}
	if len(byCNode) != 3 {
		t.Fatalf("pool main spans %d CNodes in the fixtures, want 3", len(byCNode))
	}

	port := freePort(t)
	s := daemon(t)
	postAPI(t, s, "/api/v1/vast", map[string]any{
		"name": "lab", "host": mock.Host(), "user": vasttest.User, "password": vasttest.Password, "insecure": true,
	})
	postAPI(t, s, "/api/v1/targets", map[string]any{
		"name": "main", "cluster": "lab", "pool": "main", "port": eps[0].Port, "listen_port": port,
	})
	envoy(t, s, "it-envoy-1")
	waitMember(t, s, "it-envoy-1", "connected and in sync", inSync)
	waitListener(t, port)

	res := load(t, port, func(c *lab.LoadConfig) { c.Requests = 600 })
	requireClean(t, res)
	if len(res.Backends) != 6 {
		t.Errorf("load reached %d of 6 VIPs: %v", len(res.Backends), res.Backends)
	}

	// Per-CNode rollups: each CNode's requests are those of its two VIPs.
	var rows map[string]cnodeRow
	deadline := time.Now().Add(15 * time.Second)
	for {
		rows = cnodeRows(t, s)
		var total uint64
		for _, c := range rows {
			total += c.Stats.Requests
		}
		if total == uint64(res.Requests) || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	for name, idx := range byCNode {
		var want uint64
		for _, i := range idx {
			want += uint64(fakes[i].Stats().Requests)
		}
		c := rows[name]
		t.Logf("%s: VIPs %d, API counts %d requests, its backends %d", name, c.VIPs, c.Stats.Requests, want)
		if !c.Healthy || c.VIPs != len(idx) || len(c.VIPRows) != len(idx) || c.Stats.Requests != want || want == 0 {
			t.Errorf("%s: healthy %v, %d VIPs, %d VIP rows, %d requests; want healthy, %d VIPs, %d requests",
				name, c.Healthy, c.VIPs, len(c.VIPRows), c.Stats.Requests, len(idx), want)
		}
	}

	// VAST reports one CNode out of service. Within a poll or two, Envoy
	// has four endpoints and that CNode's VIPs get nothing more.
	var down string
	for name := range byCNode {
		if down == "" || name < down {
			down = name
		}
	}
	if err := mock.SetCNode(down, "INACTIVE", true); err != nil {
		t.Fatal(err)
	}
	waitMember(t, s, "it-envoy-1", "in sync with four endpoints", func(m fleet.Member, f fleetResponse) bool {
		targets := getAPI[struct {
			Targets []struct {
				Endpoints []struct{} `json:"endpoints"`
			} `json:"targets"`
		}](t, s, "/api/v1/targets").Targets
		return len(targets) == 1 && len(targets[0].Endpoints) == 4 && inSync(m, f)
	})
	before := make([]int64, len(fakes))
	for i, f := range fakes {
		before[i] = f.Stats().Requests
	}
	res = load(t, port, func(c *lab.LoadConfig) { c.Requests = 400 })
	requireClean(t, res)
	for i, f := range fakes {
		got := f.Stats().Requests - before[i]
		if cnodeOf[i] == down && got != 0 {
			t.Errorf("VIP %s on %s (out of service) received %d more requests", ips[i], down, got)
		}
		if cnodeOf[i] != down && got == 0 {
			t.Errorf("VIP %s on %s received nothing", ips[i], cnodeOf[i])
		}
	}
	if c := cnodeRows(t, s)[down]; c.Healthy {
		t.Errorf("%s is still shown as healthy", down)
	}
	if mock.Mutating() != 0 {
		t.Errorf("%d requests other than GET and the token call reached the VAST API", mock.Mutating())
	}
}

func sendAPI(t *testing.T, s *server.Server, method, path string, body any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, "http://"+s.HTTPAddr()+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s %s: status %d: %s", method, path, resp.StatusCode, msg)
	}
}

// vastTarget starts n fake backends with the given config behind a mock
// VAST's pool "main", a daemon with that pool as the target "main", and one
// Envoy, and waits until the listener answers. It returns the daemon, the
// mock, the backends, the CNode behind each backend, and the listener port.
func vastTarget(t *testing.T, cfg lab.FakeS3Config) (*server.Server, *vasttest.Server, []*lab.FakeS3, []string, uint32) {
	t.Helper()
	return vastTargetWith(t, cfg, envoyWorkers)
}

// vastTargetWith is vastTarget with a chosen number of Envoy worker threads.
func vastTargetWith(t *testing.T, cfg lab.FakeS3Config, workers int) (*server.Server, *vasttest.Server, []*lab.FakeS3, []string, uint32) {
	t.Helper()
	cfgs := make([]lab.FakeS3Config, 6)
	for i := range cfgs {
		cfgs[i] = cfg
	}
	eps, fakes := backendsWith(t, cfgs...)
	mock, err := vasttest.New(filepath.Join("..", "..", "testdata", "vast", "v5.5.3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	ips := make([]string, len(eps))
	for i, e := range eps {
		ips[i] = e.IP
		if e.Port != eps[0].Port {
			t.Skip("the backends could not share one port on this host (no 127.0.0.11-16); a VIP pool has one port")
		}
	}
	if err := mock.SetPoolVIPs("main", ips); err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	s := daemon(t)
	postAPI(t, s, "/api/v1/vast", map[string]any{
		"name": "lab", "host": mock.Host(), "user": vasttest.User, "password": vasttest.Password, "insecure": true,
	})
	postAPI(t, s, "/api/v1/targets", map[string]any{
		"name": "main", "cluster": "lab", "pool": "main", "port": eps[0].Port, "listen_port": port,
	})
	envoyWith(t, s, "it-envoy-1", workers)
	waitMember(t, s, "it-envoy-1", "connected and in sync", inSync)
	waitListener(t, port)
	return s, mock, fakes, mock.PoolCNodes("main"), port
}

// over returns the CNodes the API reports over the target's limit now.
func over(t *testing.T, s *server.Server) []string {
	t.Helper()
	targets := getAPI[struct {
		Targets []struct {
			Over []struct {
				CNode    string `json:"cnode"`
				InFlight uint64 `json:"in_flight"`
			} `json:"over"`
		} `json:"targets"`
	}](t, s, "/api/v1/targets").Targets
	var out []string
	for _, o := range targets[0].Over {
		out = append(out, fmt.Sprintf("%s (%d)", o.CNode, o.InFlight))
	}
	return out
}

// enforced sets the target's limit to 6 per CNode in enforce mode (one
// Envoy, two VIPs per CNode: a share of 3 per VIP), runs 48 workers through
// the listener for 5 seconds, and returns the most requests each backend
// had at once. Nothing may fail, and the API must not report a CNode over
// the limit unless allowOver.
func enforced(t *testing.T, s *server.Server, fakes []*lab.FakeS3, port uint32, allowOver bool) []int64 {
	t.Helper()
	sendAPI(t, s, http.MethodPut, "/api/v1/targets/main/limit", map[string]any{"per_cnode": 6, "mode": "enforce"})
	waitMember(t, s, "it-envoy-1", "in sync with the limit", inSync)
	time.Sleep(2500 * time.Millisecond) // let load reporting pick up the rebuilt cluster (DESIGN.md, amendment 20)
	for _, f := range fakes {
		f.ResetMaxInFlight()
	}
	done := make(chan *lab.LoadResult, 1)
	go func() {
		done <- load(t, port, func(c *lab.LoadConfig) { c.Requests, c.Duration, c.Concurrency, c.Size = 0, 5*time.Second, 48, 4096 })
	}()
	var res *lab.LoadResult
	var seenOver []string
	for res == nil {
		select {
		case res = <-done:
		case <-time.After(300 * time.Millisecond):
			seenOver = append(seenOver, over(t, s)...)
		}
	}
	requireClean(t, res)
	if len(seenOver) > 0 && !allowOver {
		t.Errorf("the API reported CNodes over the limit while it was enforced: %v", seenOver)
	}
	most := make([]int64, len(fakes))
	for i, f := range fakes {
		most[i] = f.Stats().MaxInFlight
	}
	t.Logf("enforced: %d requests, p50 %.0f ms, p99 %.0f ms; most at once per backend: %v; reported over: %v",
		res.Requests, res.Latency.P50, res.Latency.P99, most, seenOver)
	return most
}

// Milestone 7, enforce mode, with one Envoy worker thread: the cap holds
// each backend to exactly its share, and excess requests wait and are not
// failed.
func TestLimitEnforceExactWithOneWorker(t *testing.T) {
	// Slow backends, so that requests pile up.
	s, _, fakes, _, port := vastTargetWith(t, lab.FakeS3Config{Latency: 100 * time.Millisecond}, 1)
	for i, got := range enforced(t, s, fakes, port, false) {
		if got > 3 || got == 0 {
			t.Errorf("backend %d had up to %d requests at once; the cap is 3 per VIP and this Envoy has one worker", i, got)
		}
	}
}

// Milestone 7, enforce mode, with two Envoy worker threads. Envoy lets
// every worker open one connection to a host whatever the cap says, so a
// backend can have up to share + workers - 1 requests at once (DESIGN.md,
// amendment 34). Also: taking the cap away under load (which rebuilds
// Envoy's cluster) fails nothing; and with no cap the same load goes over
// the limit, which alert mode reports.
func TestLimitEnforceThroughEnvoy(t *testing.T) {
	s, _, fakes, _, port := vastTarget(t, lab.FakeS3Config{Latency: 100 * time.Millisecond})
	const bound = 3 + envoyWorkers - 1
	// A CNode has two VIPs, so it can be over its limit of 6 by as much as
	// the workers add: the API may say so, and that is the truth.
	for i, got := range enforced(t, s, fakes, port, true) {
		if got > bound || got == 0 {
			t.Errorf("backend %d had up to %d requests at once; the cap is 3 per VIP and %d workers allow at most %d", i, got, envoyWorkers, bound)
		}
	}
	done := make(chan *lab.LoadResult, 1)
	var res *lab.LoadResult
	var seenOver []string

	// The same load again; a second in, the limit goes back to alert. That
	// removes the cap, which makes Envoy rebuild the cluster and its pools
	// while requests are in flight. None may fail.
	go func() {
		done <- load(t, port, func(c *lab.LoadConfig) { c.Requests, c.Duration, c.Concurrency, c.Size = 0, 6*time.Second, 48, 4096 })
	}()
	time.Sleep(time.Second)
	sendAPI(t, s, http.MethodPut, "/api/v1/targets/main/limit", map[string]any{"per_cnode": 6, "mode": "alert"})
	waitMember(t, s, "it-envoy-1", "in sync without the cap", inSync)
	for _, f := range fakes {
		f.ResetMaxInFlight()
	}
	res, seenOver = nil, nil
	for res == nil {
		select {
		case res = <-done:
		case <-time.After(300 * time.Millisecond):
			seenOver = append(seenOver, over(t, s)...)
		}
	}
	requireClean(t, res)
	uncapped := false
	for _, f := range fakes {
		uncapped = uncapped || f.Stats().MaxInFlight > bound
	}
	if !uncapped {
		t.Errorf("with the cap removed no backend had more than %d requests at once: was it really removed?", bound)
	}
	if len(seenOver) == 0 {
		t.Error("48 workers against a limit of 6 per CNode, unenforced, and the API never reported a CNode over the limit")
	} else {
		t.Logf("alert mode reported, for example: %s", seenOver[0])
	}
}

// Milestone 7, drain: under load, a drained CNode's VIPs stop receiving
// requests and nothing fails; undrained, they are used again.
func TestDrainThroughEnvoy(t *testing.T) {
	s, _, fakes, cnodeOf, port := vastTarget(t, lab.FakeS3Config{Latency: 20 * time.Millisecond})
	const victim = "cnode-3-11"

	done := make(chan *lab.LoadResult, 1)
	go func() {
		done <- load(t, port, func(c *lab.LoadConfig) { c.Requests, c.Duration, c.Concurrency, c.Size = 0, 6*time.Second, 16, 4096 })
	}()
	time.Sleep(time.Second)
	sendAPI(t, s, http.MethodPost, "/api/v1/vast/lab/cnodes/"+victim+"/drain", nil)
	waitMember(t, s, "it-envoy-1", "in sync after the drain", inSync)
	time.Sleep(500 * time.Millisecond) // requests already on their way to the drained VIPs finish
	atDrain := make([]int64, len(fakes))
	for i, f := range fakes {
		atDrain[i] = f.Stats().Requests
	}
	time.Sleep(2 * time.Second)
	for i, f := range fakes {
		got := f.Stats().Requests - atDrain[i]
		if cnodeOf[i] == victim && got != 0 {
			t.Errorf("a VIP of the drained %s received %d more requests", victim, got)
		}
		if cnodeOf[i] != victim && got == 0 {
			t.Errorf("a VIP of %s received nothing while %s was drained", cnodeOf[i], victim)
		}
	}
	sendAPI(t, s, http.MethodDelete, "/api/v1/vast/lab/cnodes/"+victim+"/drain", nil)
	waitMember(t, s, "it-envoy-1", "in sync after the undrain", inSync)
	atUndrain := make([]int64, len(fakes))
	for i, f := range fakes {
		atUndrain[i] = f.Stats().Requests
	}
	requireClean(t, <-done) // drained and undrained under load: every request succeeded
	for i, f := range fakes {
		if cnodeOf[i] == victim && f.Stats().Requests == atUndrain[i] {
			t.Errorf("a VIP of %s received nothing after it was undrained", victim)
		}
	}
}
