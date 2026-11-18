package cli

import (
	"strings"
	"testing"

	bootstrapv3 "github.com/envoyproxy/go-control-plane/envoy/config/bootstrap/v3"
	"google.golang.org/protobuf/encoding/protojson"
)

func TestParseStaticTargets(t *testing.T) {
	got, err := parseStaticTargets([]string{
		"s3:9000:127.0.0.11:9100,127.0.0.12:9100, 127.0.0.13:9100",
		"tls:9443:[2001:db8::1]:443 ; other:9001:192.0.2.1:80;",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("parsed %d targets, want 3: %+v", len(got), got)
	}
	s3 := got[0]
	if s3.Name != "s3" || s3.ListenPort != 9000 || len(s3.Endpoints) != 3 ||
		s3.Endpoints[2].IP != "127.0.0.13" || s3.Endpoints[2].Port != 9100 {
		t.Errorf("s3 = %+v", s3)
	}
	if got[1].Name != "tls" || got[1].Endpoints[0].IP != "2001:db8::1" || got[1].Endpoints[0].Port != 443 {
		t.Errorf("IPv6 target = %+v", got[1])
	}
	if got[2].Name != "other" || got[2].ListenPort != 9001 {
		t.Errorf("third target = %+v", got[2])
	}
	// What was parsed must render: this is what serve hands to xDS.
	for _, target := range got {
		if _, _, _, err := target.Resources(); err != nil {
			t.Errorf("%s does not render: %v", target.Name, err)
		}
	}
}

func TestParseStaticTargetErrors(t *testing.T) {
	for _, in := range []string{
		"s3",
		"s3:9000",
		":9000:127.0.0.1:80",
		"s3:0:127.0.0.1:80",
		"s3:port:127.0.0.1:80",
		"s3:9000:127.0.0.1",
		"s3:9000:127.0.0.1:0",
		"s3:9000:s3.example.com:80",
		"s3:9000:127.0.0.1:80,",
	} {
		_, err := parseStaticTargets([]string{in})
		if err == nil || !strings.Contains(err.Error(), staticTargetSyntax) {
			t.Errorf("%q: want an error that shows the syntax, got %v", in, err)
		}
	}
}

func TestBootstrapCommand(t *testing.T) {
	got, err := run(t, "dv", "bootstrap", "--xds-address", "192.0.2.10:18000")
	if err != nil {
		t.Fatal(err)
	}
	var b bootstrapv3.Bootstrap
	if err := protojson.Unmarshal([]byte(got), &b); err != nil {
		t.Fatalf("output is not an Envoy bootstrap: %v\n%s", err, got)
	}
	// Envoy's admin interface has no authentication and can stop Envoy, so
	// it is off unless asked for.
	if b.GetAdmin() != nil {
		t.Errorf("the admin interface must be off by default: %v", b.GetAdmin())
	}

	got, err = run(t, "dv", "bootstrap", "--xds-address", "192.0.2.10:18000", "--admin-address", "127.0.0.1:9901")
	if err != nil {
		t.Fatal(err)
	}
	b.Reset()
	if err := protojson.Unmarshal([]byte(got), &b); err != nil || b.GetAdmin().GetAddress().GetSocketAddress().GetPortValue() != 9901 {
		t.Errorf("--admin-address must turn the admin interface on: %v %v", err, b.GetAdmin())
	}

	// The systemd units and their example environment files.
	for name, wants := range map[string][]string{
		"daemon":     {"ExecStart=/usr/local/bin/deltav serve", "StateDirectory=deltav", "EnvironmentFile=-/etc/deltav/deltav.env"},
		"client":     {"--config-path /etc/deltav/envoy.json", "--service-node %H", "--concurrency ${DELTAV_ENVOY_WORKERS}", "deltav_workers: ${DELTAV_ENVOY_WORKERS}"},
		"daemon-env": {"DELTAV_API_TOKEN", "DELTAV_HTTP_ADDR"},
		"client-env": {"DELTAV_ENVOY_WORKERS"},
	} {
		got, err := run(t, "dv", "unit", name)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range wants {
			if !strings.Contains(got, w) {
				t.Errorf("unit %s: missing %q", name, w)
			}
		}
	}
	if _, err := run(t, "dv", "unit", "nope"); err == nil {
		t.Error("an unknown unit name should be an error")
	}

	if _, err := run(t, "dv", "bootstrap"); err == nil || !strings.Contains(err.Error(), "--xds-address") {
		t.Errorf("want an error naming --xds-address, got %v", err)
	}
}

func TestServeRejectsBadStaticTarget(t *testing.T) {
	_, err := run(t, "deltav", "serve", "--static-target", "s3:9000")
	if err == nil || !strings.Contains(err.Error(), staticTargetSyntax) {
		t.Fatalf("got %v", err)
	}
}
