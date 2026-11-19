//go:build integration

package integration

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"deltav/internal/lab"
	"deltav/internal/xds"
)

// testPKI makes a certificate authority and a server certificate it signed
// for the given DNS names, and writes the authority's certificate to a file
// as a client host would have it.
func testPKI(t *testing.T, names ...string) (server tls.Certificate, caFile string) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "deltav test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "deltav test backend"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: names,
	}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caFile = filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o644); err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, caFile
}

// TLS from Envoy to the backends, with real Envoy: the first time this
// configuration has met one (DESIGN.md amendment 6). Six backends that speak
// only TLS, and one target for each way of trusting them, or failing to.
func TestTLSToTheBackends(t *testing.T) {
	cert, caFile := testPKI(t, "s3.lab.example", "*.pools.lab.example")
	_, otherCA := testPKI(t, "s3.lab.example") // an authority that signed nothing the backends have
	var mu sync.Mutex
	asked := map[string]int{} // server names clients asked for, by SNI
	wrap := func(ln net.Listener) net.Listener {
		return tls.NewListener(ln, &tls.Config{
			Certificates: []tls.Certificate{cert},
			GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
				mu.Lock()
				asked[hello.ServerName]++
				mu.Unlock()
				return nil, nil
			},
		})
	}
	eps, fakes := backendsOn(t, wrap, make([]lab.FakeS3Config, 3)...)
	served := func() (n int64) {
		for _, f := range fakes {
			n += f.Stats().Requests
		}
		return n
	}

	type kind struct {
		name string
		tls  *xds.TLSSpec
		ok   bool
		why  string
	}
	kinds := []kind{
		{"verified", &xds.TLSSpec{ServerName: "s3.lab.example", CAFile: caFile}, true, "the authority's file and the certificate's own name"},
		{"wildcard", &xds.TLSSpec{ServerName: "main.pools.lab.example", CAFile: caFile}, true, "a name the certificate covers with a wildcard"},
		{"insecure", &xds.TLSSpec{ServerName: "whatever.example", Insecure: true}, true, "verification switched off"},
		{"wrong-name", &xds.TLSSpec{ServerName: "s3.other.example", CAFile: caFile}, false, "a name the certificate is not for"},
		{"too-deep", &xds.TLSSpec{ServerName: "a.b.pools.lab.example", CAFile: caFile}, false, "two labels under a wildcard, which covers one"},
		{"other-authority", &xds.TLSSpec{ServerName: "s3.lab.example", CAFile: otherCA}, false, "the right name, and an authority that did not sign the certificate"},
		{"plaintext", nil, false, "no TLS at all, to a backend that speaks only TLS"},
	}
	// The host's own bundle, as an operator would name it for a certificate
	// from a public authority. It does not know the test's authority, so
	// this must fail too; that Envoy loads the file at all is the point.
	for _, path := range []string{"/etc/ssl/certs/ca-certificates.crt", "/etc/pki/tls/certs/ca-bundle.crt", "/etc/ssl/ca-bundle.pem", "/etc/ssl/cert.pem"} {
		if _, err := os.Stat(path); err == nil {
			kinds = append(kinds, kind{"host-bundle", &xds.TLSSpec{ServerName: "s3.lab.example", CAFile: path}, false, "this host's own CA bundle (" + path + "), which does not know the authority"})
			break
		}
	}
	ports := map[string]uint32{}
	var targets []xds.TargetSpec
	for _, k := range kinds {
		ports[k.name] = freePort(t)
		targets = append(targets, xds.TargetSpec{Name: k.name, ListenPort: ports[k.name], Endpoints: eps, TLS: k.tls})
	}
	s := daemon(t, targets...)
	envoy(t, s, "it-envoy-1")
	waitMember(t, s, "it-envoy-1", "connected and in sync", inSync)

	for _, k := range kinds {
		waitListener(t, ports[k.name])
		before := served()
		res := load(t, ports[k.name], func(c *lab.LoadConfig) { c.Requests = 60 })
		got := served() - before
		t.Logf("%-18s %s: status %v, %d transport errors, %d requests reached a backend", k.name, k.why, res.Status, res.TransportErrors, got)
		if !k.ok {
			status, body := rawRequest(t, ports[k.name], "GET", "/lab-bucket/k", "127.0.0.1", nil, "")
			t.Logf("%-18s the client is told: %d %s", "", status, body)
		}
		if k.ok {
			requireClean(t, res)
			if got != int64(res.Requests) {
				t.Errorf("%s: %d requests sent, %d reached a backend", k.name, res.Requests, got)
			}
			continue
		}
		// Refused: nothing of any request may reach a backend, and the
		// client must be told so on every one.
		if got != 0 || res.Status[200] != 0 {
			t.Errorf("%s: with %s, %d requests reached a backend and %d were answered 200", k.name, k.why, got, res.Status[200])
		}
		// Envoy's own answer when it cannot get one from a backend is 503.
		// The plaintext target does get an answer, from the backend's TLS
		// layer, which tells an HTTP client it has the wrong protocol.
		if k.tls != nil && res.Status[503] != res.Requests {
			t.Errorf("%s: status %v, want 503 for all %d", k.name, res.Status, res.Requests)
		}
	}

	// Envoy asked each backend for the name its target was given, and for
	// no other: that name is what a backend with several certificates
	// would choose by.
	mu.Lock()
	var names []string
	for name := range asked {
		names = append(names, name)
	}
	mu.Unlock()
	sort.Strings(names)
	want := []string{"a.b.pools.lab.example", "main.pools.lab.example", "s3.lab.example", "s3.other.example", "whatever.example"}
	if len(names) != len(want) {
		t.Errorf("backends were asked for the names %q, want %q", names, want)
	} else {
		for i := range want {
			if names[i] != want[i] {
				t.Errorf("backends were asked for the names %q, want %q", names, want)
				break
			}
		}
	}

	// Why: nothing but the daemon says. Envoy logs no TLS failure at its
	// usual level, and the client above was told only that a connection
	// failed.
	time.Sleep(1500 * time.Millisecond)
	said := map[string]string{}
	for _, tg := range getAPI[struct {
		Targets []struct {
			Name     string `json:"name"`
			NoAnswer *struct {
				Client     string `json:"client"`
				Endpoint   string `json:"endpoint"`
				Reason     string `json:"reason"`
				LastMinute uint64 `json:"last_minute"`
			} `json:"no_answer"`
		} `json:"targets"`
	}](t, s, "/api/v1/targets").Targets {
		if tg.NoAnswer != nil {
			said[tg.Name] = tg.NoAnswer.Reason
			t.Logf("target %-18s no answer %d times in the last minute, last on %s to %s: %s", tg.Name, tg.NoAnswer.LastMinute, tg.NoAnswer.Client, tg.NoAnswer.Endpoint, tg.NoAnswer.Reason)
			if tg.NoAnswer.LastMinute != 61 || tg.NoAnswer.Client != "it-envoy-1" || tg.NoAnswer.Endpoint == "" {
				t.Errorf("target %s: %+v; want 61 requests (60 and the one by hand), on it-envoy-1, to an endpoint", tg.Name, *tg.NoAnswer)
			}
		}
	}
	const notForName = "VAST's certificate is not for the target's server name; it is for s3.lab.example, *.pools.lab.example"
	const notChained = "VAST's certificate does not chain to the target's CA file"
	for name, want := range map[string]string{
		"verified": "", "wildcard": "", "insecure": "",
		"plaintext":       "", // answered, if not by S3: the backend's TLS layer said 400
		"wrong-name":      notForName,
		"too-deep":        notForName,
		"other-authority": notChained,
		"host-bundle":     notChained,
	} {
		if _, tried := ports[name]; !tried {
			continue
		}
		if got := said[name]; (want == "") != (got == "") || !strings.HasPrefix(got, want) {
			t.Errorf("target %s: the daemon gives the reason %q, want one that starts %q", name, got, want)
		}
	}
	// The same, where someone would look: the web UI's first page.
	resp, err := http.Get("http://" + s.HTTPAddr() + "/")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if n := strings.Count(string(page), "No answer"); n != len(said) {
		t.Errorf("the overview page marks %d targets as getting no answer, want %d", n, len(said))
	}
	if !strings.Contains(string(page), "it is for s3.lab.example, *.pools.lab.example") || !strings.Contains(string(page), "does not chain to the target") {
		t.Error("the overview page does not give the reasons")
	}

	// What the daemon says of a target that cannot reach its backends.
	type overview struct {
		Targets []struct {
			Name  string `json:"name"`
			Stats struct {
				Requests uint64 `json:"requests"`
				Status   struct {
					S2xx uint64 `json:"2xx"`
					S5xx uint64 `json:"5xx"`
					S503 uint64 `json:"503"`
				} `json:"status"`
			} `json:"stats"`
		} `json:"targets"`
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		o := getAPI[overview](t, s, "/api/v1/overview?window=5m")
		done := true
		for _, tg := range o.Targets {
			for _, k := range kinds {
				if k.name != tg.Name {
					continue
				}
				if tg.Stats.Requests < 60 || (k.ok && tg.Stats.Status.S2xx != 60) || (!k.ok && tg.Stats.Status.S2xx != 0) {
					done = false
				}
			}
		}
		if done || time.Now().After(deadline) {
			for _, tg := range o.Targets {
				t.Logf("overview: %-18s %d requests, %d 2xx, %d 503", tg.Name, tg.Stats.Requests, tg.Stats.Status.S2xx, tg.Stats.Status.S503)
			}
			if !done {
				t.Error("the overview does not show every target's requests: 60 answered 2xx where TLS works, none where it does not")
			}
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
}
