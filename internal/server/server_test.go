package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discoverygrpc "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestIsLoopback(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:18080": true, "[::1]:18080": true, "localhost:1": true, "127.1.2.3:9": true,
		":18080": false, "0.0.0.0:18080": false, "[::]:18080": false, "10.0.0.5:18080": false,
		"deltav.example.com:18080": false, "garbage": false,
	}
	for addr, want := range cases {
		if got := isLoopback(addr); got != want {
			t.Errorf("isLoopback(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestNonLoopbackRequiresToken(t *testing.T) {
	_, err := New(Config{GRPCAddr: "127.0.0.1:0", HTTPAddr: "0.0.0.0:0"})
	if err == nil || !strings.Contains(err.Error(), "DELTAV_API_TOKEN") {
		t.Fatalf("want token error, got %v", err)
	}
	if _, err := New(Config{GRPCAddr: "127.0.0.1:0", HTTPAddr: "0.0.0.0:0", APIToken: "s3cret"}); err != nil {
		t.Fatalf("with token: %v", err)
	}
}

func start(t *testing.T, cfg Config) *Server {
	t.Helper()
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := New(cfg)
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
				t.Errorf("Run: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("Run did not return after cancel")
		}
	})
	return s
}

func get(t *testing.T, url, token string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestServeSmoke(t *testing.T) {
	s := start(t, Config{GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0"})
	base := "http://" + s.HTTPAddr()

	if code, body := get(t, base+"/healthz", ""); code != 200 || body != "ok\n" {
		t.Fatalf("/healthz: %d %q", code, body)
	}
	code, body := get(t, base+"/api/v1/version", "")
	var v map[string]string
	if code != 200 || json.Unmarshal([]byte(body), &v) != nil || v["name"] != "deltav" || v["envoy_pinned"] == "" {
		t.Fatalf("/api/v1/version: %d %s", code, body)
	}
	if code, body := get(t, base+"/metrics", ""); code != 200 || !strings.Contains(body, "deltav_build_info{") {
		t.Fatalf("/metrics: %d, deltav_build_info missing", code)
	}
	if code, _ := get(t, base+"/api/v1/nope", ""); code != 404 {
		t.Fatalf("unknown path: %d", code)
	}

	// An Envoy-style ADS stream opens and accepts a request. The cache is
	// empty in milestone 1, so no response is expected yet.
	conn, err := grpc.NewClient(s.GRPCAddr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := discoverygrpc.NewAggregatedDiscoveryServiceClient(conn).StreamAggregatedResources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&discoverygrpc.DiscoveryRequest{
		Node:    &corev3.Node{Id: "test-host"},
		TypeUrl: resource.ClusterType,
	}); err != nil {
		t.Fatalf("ADS send: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
}

func TestTokenEnforced(t *testing.T) {
	s := start(t, Config{GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0", APIToken: "s3cret"})
	base := "http://" + s.HTTPAddr()
	for _, tc := range []struct {
		path, token string
		want        int
	}{
		{"/api/v1/version", "", 401},
		{"/api/v1/version", "wrong", 401},
		{"/api/v1/version", "s3cret", 200},
		{"/metrics", "", 401},
		{"/metrics", "s3cret", 200},
		{"/healthz", "", 200},
	} {
		if code, _ := get(t, base+tc.path, tc.token); code != tc.want {
			t.Errorf("%s token=%q: %d, want %d", tc.path, tc.token, code, tc.want)
		}
	}
}
