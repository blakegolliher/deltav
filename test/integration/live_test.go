//go:build integration && live

package integration

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"deltav/internal/lab"
)

// TestLiveVASTThroughEnvoy is the whole thing on real equipment: a real VAST
// cluster read over its API (GETs only), real Envoy, and PUT/GET/DELETE
// round trips against a real bucket through the loopback listener. Run it
// with `make live ENV=<file>`; see the Makefile for what the file sets.
// Every object it writes it deletes.
func TestLiveVASTThroughEnvoy(t *testing.T) {
	need := func(name string) string {
		v := os.Getenv(name)
		if v == "" {
			t.Fatalf("%s is not set; see `make live`", name)
		}
		return v
	}
	or := func(name, fallback string) string {
		if v := os.Getenv(name); v != "" {
			return v
		}
		return fallback
	}
	number := func(name, fallback string) int {
		n, err := strconv.Atoi(or(name, fallback))
		if err != nil || n <= 0 {
			t.Fatalf("%s must be a positive number", name)
		}
		return n
	}
	host, user, password := need("VAST_HOST"), need("VAST_USER"), need("VAST_PASSWORD")
	keys := &lab.Credentials{AccessKey: need("AWS_ACCESS_KEY_ID"), SecretKey: need("AWS_SECRET_ACCESS_KEY"), Region: or("AWS_REGION", "us-east-1")}
	bucket := need("DELTAV_LAB_BUCKET")
	pool := or("DELTAV_LAB_POOL", "main")
	port := number("DELTAV_LAB_PORT", "80")
	cycles := number("DELTAV_LAB_CYCLES", "200")
	size := number("DELTAV_LAB_SIZE", "1048576")

	listen := freePort(t)
	s := daemon(t)
	postAPI(t, s, "/api/v1/vast", map[string]any{
		"name": "live", "host": host, "user": user, "password": password, "insecure": os.Getenv("DELTAV_LAB_INSECURE") == "1",
	})
	postAPI(t, s, "/api/v1/targets", map[string]any{
		"name": "live", "cluster": "live", "pool": pool, "port": port, "listen_port": listen,
	})
	for name, c := range cnodeRows(t, s) {
		t.Logf("CNode %s: healthy %v, hosts %d of the pool's VIPs", name, c.Healthy, c.VIPs)
	}

	envoy(t, s, "live-envoy-1")
	waitMember(t, s, "live-envoy-1", "connected and in sync", inSync)
	waitListener(t, listen)

	res, err := lab.RunLoad(context.Background(), lab.LoadConfig{
		Endpoint: "http://127.0.0.1:" + strconv.Itoa(int(listen)), Bucket: bucket, Prefix: "deltav-live/", Creds: keys,
		Concurrency: 4, Requests: cycles, Size: int64(size), Roundtrip: true, Expect: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d of %d round trips verified in %.1fs; status %v; p50 %.1f ms, p99 %.1f ms; %d bytes up, %d down",
		res.Roundtrips, res.Requests, res.ElapsedSeconds, res.Status, res.Latency.P50, res.Latency.P99, res.BytesSent, res.BytesReceived)
	if res.Roundtrips != cycles || res.TransportErrors != 0 || res.CheckFailures != 0 {
		t.Errorf("%d transport errors, %d failed cycles:\n%s", res.TransportErrors, res.CheckFailures, strings.Join(res.Samples, "\n"))
	}

	// What the daemon saw, per CNode and per VIP: three requests a cycle.
	want := uint64(3 * res.Requests)
	var rows map[string]cnodeRow
	deadline := time.Now().Add(15 * time.Second)
	for {
		rows = cnodeRows(t, s)
		var total uint64
		for _, c := range rows {
			total += c.Stats.Requests
		}
		if total >= want || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	var total uint64
	for name, c := range rows {
		total += c.Stats.Requests
		t.Logf("CNode %s: %d requests (%d 2xx, %d 403, %d 503), p50 %.1f ms, p99 %.1f ms, %d bytes in, %d out",
			name, c.Stats.Requests, c.Stats.Status.S2xx, c.Stats.Status.S403, c.Stats.Status.S503,
			c.Stats.LatencyMS.P50, c.Stats.LatencyMS.P99, c.Stats.BytesIn, c.Stats.BytesOut)
		for _, v := range c.VIPRows {
			t.Logf("    VIP %s: %d requests", v.Endpoint, v.Stats.Requests)
		}
		if c.Healthy && c.VIPs > 0 && c.Stats.Requests == 0 {
			t.Errorf("CNode %s hosts %d VIPs of the pool and received nothing", name, c.VIPs)
		}
	}
	if total != want {
		t.Errorf("the daemon counted %d requests across CNodes; the load generator sent %d", total, want)
	}
}
