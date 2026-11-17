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

// backends starts n fake S3 servers. On Linux they take distinct loopback
// addresses on one shared port, as VIPs do; where only 127.0.0.1 is usable
// they fall back to distinct ports.
func backends(t *testing.T, n int) ([]xds.Endpoint, []*lab.FakeS3) {
	t.Helper()
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
	for _, ln := range lns {
		addr := ln.Addr().(*net.TCPAddr)
		f := lab.NewFakeS3(lab.FakeS3Config{Name: addr.String(), Keys: map[string]string{creds.AccessKey: creds.SecretKey}})
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
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
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

// envoy starts one real Envoy with the bootstrap `deltav bootstrap` prints,
// naming itself through --service-node as a client host's unit does.
func envoy(t *testing.T, s *server.Server, nodeID string) {
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
		"--concurrency", "2",
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
				lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
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
	if _, _, err := s.XDS().Update(context.Background(), []xds.TargetSpec{
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
	if _, changed, err := s.XDS().Update(context.Background(), []xds.TargetSpec{target}); err != nil || !changed {
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
	if _, _, err := s.XDS().Update(context.Background(), []xds.TargetSpec{target, bad}); err != nil {
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
	if _, _, err := s.XDS().Update(context.Background(), []xds.TargetSpec{target}); err != nil {
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
