// Command dvlab is the Delta-V test harness: a fake S3 backend and a load
// generator. It is a development tool and is not part of the deltav binary.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"deltav/internal/lab"
	"deltav/internal/vast"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	root := &cobra.Command{
		Use:           "dvlab",
		Short:         "Delta-V test harness: fake S3 backends and a load generator",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.CompletionOptions.HiddenDefaultCmd = true
	root.AddCommand(fakeS3Cmd(), loadgenCmd(), vastProbeCmd())
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "dvlab: %v\n", err)
		os.Exit(1)
	}
}

// credentials reads the access key pair from flags, falling back to the
// standard AWS environment variables. The secret is never printed.
func credentials(access, secret, region string) (*lab.Credentials, error) {
	if access == "" {
		access = os.Getenv("AWS_ACCESS_KEY_ID")
	}
	if secret == "" {
		secret = os.Getenv("AWS_SECRET_ACCESS_KEY")
	}
	switch {
	case access == "" && secret == "":
		return nil, nil
	case access == "" || secret == "":
		return nil, errors.New("an access key and a secret key must be given together")
	}
	return &lab.Credentials{AccessKey: access, SecretKey: secret, Region: region}, nil
}

func fakeS3Cmd() *cobra.Command {
	var (
		listen         []string
		cfg            lab.FakeS3Config
		access, secret string
	)
	cmd := &cobra.Command{
		Use:   "fakes3",
		Short: "Run fake S3 backends, one per listen address",
		Long: "Run fake S3 backends, one per listen address. On Linux every 127.x.y.z address is\n" +
			"loopback, so backends can share a port the way VIPs do:\n\n" +
			"  dvlab fakes3 --listen 127.0.0.11:9100,127.0.0.12:9100,127.0.0.13:9100\n\n" +
			"PUT reads and hashes the body; GET returns a pattern derived from the path. Nothing\n" +
			"is stored. Every response carries X-Fake-* headers that report what arrived.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if len(listen) == 0 {
				return errors.New("--listen is required")
			}
			creds, err := credentials(access, secret, "")
			if err != nil {
				return err
			}
			if creds != nil {
				cfg.Keys = map[string]string{creds.AccessKey: creds.SecretKey}
			}
			ctx := cmd.Context()
			var (
				wg    sync.WaitGroup
				fakes []*lab.FakeS3
				errc  = make(chan error, len(listen))
			)
			for _, addr := range listen {
				ln, err := net.Listen("tcp", addr)
				if err != nil {
					return err
				}
				c := cfg
				c.Name = ln.Addr().String()
				f := lab.NewFakeS3(c)
				fakes = append(fakes, f)
				wg.Add(1)
				go func() { defer wg.Done(); errc <- f.Serve(ctx, ln) }()
			}
			verify := "signatures not checked"
			if creds != nil {
				verify = "verifying SigV4 for access key " + creds.AccessKey
			}
			fmt.Fprintf(os.Stderr, "dvlab fakes3: %d backends on %s (%s)\n", len(listen), strings.Join(listen, ", "), verify)
			wg.Wait()
			close(errc)
			for err := range errc {
				if err != nil {
					return err
				}
			}
			for i, f := range fakes {
				st := f.Stats()
				fmt.Fprintf(os.Stderr, "%s: %d requests (%d injected 403, %d injected 503, %d bad signatures), %d bytes in, %d bytes out\n",
					listen[i], st.Requests, st.Injected403, st.Injected503, st.SigFailures, st.BytesIn, st.BytesOut)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringSliceVar(&listen, "listen", nil, "ip:port to serve on; repeat or separate with commas for several backends")
	f.DurationVar(&cfg.Latency, "latency", 0, "delay added to every request")
	f.DurationVar(&cfg.Jitter, "jitter", 0, "up to this much extra delay, uniformly random")
	f.Float64Var(&cfg.Rate403, "rate-403", 0, "fraction of requests answered 403 AccessDenied (0-1)")
	f.Float64Var(&cfg.Rate503, "rate-503", 0, "fraction of requests answered 503 SlowDown (0-1)")
	f.Int64Var(&cfg.GetSize, "get-size", 1024, "GET body size in bytes when the request does not ask for one")
	f.StringVar(&access, "access-key", "", "verify SigV4 signatures made with this access key (default $AWS_ACCESS_KEY_ID)")
	f.StringVar(&secret, "secret-key", "", "secret for --access-key (default $AWS_SECRET_ACCESS_KEY; prefer the environment)")
	return cmd
}

func loadgenCmd() *cobra.Command {
	var (
		cfg                    lab.LoadConfig
		access, secret, region string
		asJSON                 bool
	)
	cmd := &cobra.Command{
		Use:   "loadgen",
		Short: "Send path-style S3 requests through an endpoint and summarize the responses",
		Long: "Send path-style S3 requests (PUT and GET) to http://HOST:PORT/BUCKET/KEY and summarize\n" +
			"status codes, latency and, against dvlab fakes3, which backend answered.\n\n" +
			"With --check, every response is compared with what was sent using the fake backend's\n" +
			"X-Fake-* headers: the Host header, the path byte for byte, the Expect header, the body.\n" +
			"The exit status is 1 if any check fails. --check only works against dvlab fakes3.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			creds, err := credentials(access, secret, region)
			if err != nil {
				return err
			}
			cfg.Creds = creds
			res, err := lab.RunLoad(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				if err := enc.Encode(res); err != nil {
					return err
				}
			} else {
				printResult(res, cfg.Check, cfg.Roundtrip)
			}
			if res.CheckFailures > 0 {
				return fmt.Errorf("%d checks failed", res.CheckFailures)
			}
			if cfg.Roundtrip && (res.Roundtrips != res.Requests || res.Requests == 0) {
				return fmt.Errorf("%d of %d round trips verified", res.Roundtrips, res.Requests)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&cfg.Endpoint, "endpoint", "", "where to send requests, e.g. http://127.0.0.1:9000 (the Envoy listener)")
	f.StringVar(&cfg.Host, "host", "", "Host header to send and sign (default: the endpoint's host:port)")
	f.StringVar(&cfg.Bucket, "bucket", "", "bucket name")
	f.StringVar(&cfg.Prefix, "prefix", "dvlab/", "key prefix")
	f.IntVar(&cfg.Concurrency, "concurrency", 4, "parallel workers; each holds one connection")
	f.IntVar(&cfg.Requests, "requests", 0, "stop after this many requests (0: use --duration)")
	f.DurationVar(&cfg.Duration, "duration", 10*time.Second, "how long to run when --requests is 0")
	f.Float64Var(&cfg.PutRatio, "put-ratio", 0.5, "fraction of requests that are PUTs; the rest are GETs")
	f.Int64Var(&cfg.Size, "size", 64*1024, "PUT body size in bytes, and the GET size asked of a fake backend")
	f.BoolVar(&cfg.SpecialKeys, "special-keys", false, "use keys a proxy could mangle: //, %2F, /./, +, space, unicode")
	f.BoolVar(&cfg.Expect, "expect", false, "send and sign 'Expect: 100-continue' on PUTs, as S3 SDKs do")
	f.BoolVar(&cfg.Check, "check", false, "verify each response against what was sent (dvlab fakes3 only)")
	f.BoolVar(&cfg.Roundtrip, "roundtrip", false, "against a real S3 endpoint: PUT a new key, GET it back and compare, DELETE it; --requests then counts cycles")
	f.DurationVar(&cfg.Timeout, "timeout", 60*time.Second, "per-request timeout")
	f.StringVar(&access, "access-key", "", "sign requests with this access key (default $AWS_ACCESS_KEY_ID; unsigned if neither is set)")
	f.StringVar(&secret, "secret-key", "", "secret for --access-key (default $AWS_SECRET_ACCESS_KEY; prefer the environment)")
	f.StringVar(&region, "region", "us-east-1", "SigV4 region")
	f.BoolVar(&asJSON, "json", false, "print the result as JSON")
	return cmd
}

func printResult(r *lab.LoadResult, checked, roundtrip bool) {
	rate := 0.0
	if r.ElapsedSeconds > 0 {
		rate = float64(r.Requests) / r.ElapsedSeconds
	}
	fmt.Printf("requests   %d in %.2fs (%.0f/s), %d transport errors\n", r.Requests, r.ElapsedSeconds, rate, r.TransportErrors)
	codes := make([]int, 0, len(r.Status))
	for c := range r.Status {
		codes = append(codes, c)
	}
	sort.Ints(codes)
	parts := make([]string, 0, len(codes))
	for _, c := range codes {
		parts = append(parts, fmt.Sprintf("%d: %d", c, r.Status[c]))
	}
	fmt.Printf("status     %s\n", strings.Join(parts, ", "))
	fmt.Printf("latency    p50 %.1f ms, p90 %.1f ms, p99 %.1f ms, max %.1f ms\n", r.Latency.P50, r.Latency.P90, r.Latency.P99, r.Latency.Max)
	fmt.Printf("bytes      %d sent, %d received\n", r.BytesSent, r.BytesReceived)
	if len(r.Backends) > 0 {
		names := make([]string, 0, len(r.Backends))
		for n := range r.Backends {
			names = append(names, n)
		}
		sort.Strings(names)
		fmt.Println("backends")
		for _, n := range names {
			fmt.Printf("  %-24s %d\n", n, r.Backends[n])
		}
	}
	if r.Injected > 0 {
		fmt.Printf("injected   %d failures injected by the fake backend\n", r.Injected)
	}
	if checked {
		fmt.Printf("checks     %d failed\n", r.CheckFailures)
	}
	if r.Roundtrips > 0 || roundtrip {
		fmt.Printf("roundtrips %d of %d verified (PUT, GET with matching bytes, DELETE)\n", r.Roundtrips, r.Requests)
	}
	for _, s := range r.Samples {
		fmt.Printf("  %s\n", s)
	}
}

// vastProbeCmd reads a live VAST cluster and writes sanitized fixtures. It
// goes through internal/vast, whose transport can only issue GETs and token
// calls, so it cannot change anything on the cluster.
func vastProbeCmd() *cobra.Command {
	var (
		host, user, tenant, outDir string
		insecure                   bool
		paths                      []string
	)
	cmd := &cobra.Command{
		Use:   "vastprobe",
		Short: "Read a VAST cluster's API (GET only) and write sanitized test fixtures",
		Long: "GET a few list endpoints from a VAST cluster's management API and write the responses\n" +
			"to --out as JSON fixtures, with addresses, names, serials and other identifying values\n" +
			"replaced (see internal/lab/sanitize.go). Only GETs and the token call are possible.\n\n" +
			"The password is read from $VAST_PASSWORD and from nowhere else. A table of every field\n" +
			"seen, its type and what was rewritten is printed; it contains no values. Read it and\n" +
			"the fixtures before committing them.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if host == "" {
				host = os.Getenv("VAST_HOST")
			}
			if user == "" {
				user = os.Getenv("VAST_USER")
			}
			password := os.Getenv("VAST_PASSWORD")
			if host == "" || user == "" || password == "" {
				return errors.New("set VAST_HOST, VAST_USER and VAST_PASSWORD (host and user may be flags; the password may not)")
			}
			if outDir == "" {
				return errors.New("--out is required")
			}
			client, err := vast.New(vast.Options{Host: host, User: user, Password: password, Tenant: tenant, Insecure: insecure})
			if err != nil {
				return err
			}
			docs := map[string]any{}
			for _, p := range paths {
				name := strings.ReplaceAll(strings.Trim(p, "/"), "/", "_")
				raw, err := client.GetRaw(cmd.Context(), p)
				if err != nil {
					return err
				}
				dec := json.NewDecoder(bytes.NewReader(raw))
				dec.UseNumber() // keep numbers exactly as the cluster wrote them
				var doc any
				if err := dec.Decode(&doc); err != nil {
					return fmt.Errorf("GET %s: the response is not JSON: %w", p, err)
				}
				docs[name] = doc
				fmt.Fprintf(os.Stderr, "GET /api/%s: %d bytes\n", strings.TrimPrefix(p, "/"), len(raw))
			}
			clean, report, err := lab.NewSanitizer(host).Sanitize(docs)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(outDir, 0o755); err != nil {
				return err
			}
			for name, doc := range clean {
				b, err := json.MarshalIndent(doc, "", "  ")
				if err != nil {
					return err
				}
				if err := os.WriteFile(filepath.Join(outDir, name+".json"), append(b, '\n'), 0o644); err != nil {
					return err
				}
			}
			printFieldReport(report)
			fmt.Fprintf(os.Stderr, "wrote %d sanitized fixtures to %s\n", len(clean), outDir)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&host, "host", "", "VAST management host (default $VAST_HOST)")
	f.StringVar(&user, "user", "", "VAST user (default $VAST_USER)")
	f.StringVar(&tenant, "tenant", "", "log in to this tenant instead of the default")
	f.BoolVar(&insecure, "insecure", false, "skip certificate verification (lab clusters with self-signed certificates)")
	f.StringVar(&outDir, "out", "", "directory to write the fixtures to, e.g. testdata/vast/lab1")
	f.StringSliceVar(&paths, "path", []string{"cnodes/", "vippools/", "vips/"}, "API paths to GET, relative to /api/")
	return cmd
}

// printFieldReport prints the shape of what was read: one line per field
// with its JSON types, how many values and distinct values it had, and
// which sanitizer rules rewrote how many of them. No values are printed.
func printFieldReport(report []lab.FieldReport) {
	doc := ""
	for _, r := range report {
		if r.Doc != doc {
			doc = r.Doc
			fmt.Printf("\n%s\n  %-44s %-14s %7s %9s  %s\n", doc, "FIELD", "TYPE", "VALUES", "DISTINCT", "REWRITTEN")
		}
		rules := make([]string, 0, len(r.Rules))
		for name, n := range r.Rules {
			rules = append(rules, fmt.Sprintf("%s:%d", name, n))
		}
		sort.Strings(rules)
		path := r.Path
		if path == "" {
			path = "(top level)"
		}
		fmt.Printf("  %-44s %-14s %7d %9d  %s\n", path, strings.Join(r.Types, "|"), r.Count, r.Distinct, strings.Join(rules, " "))
	}
}
