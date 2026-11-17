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
	if b.GetAdmin().GetAddress().GetSocketAddress().GetPortValue() != 9901 {
		t.Errorf("default admin address missing: %v", b.GetAdmin())
	}

	got, err = run(t, "dv", "bootstrap", "--xds-address", "192.0.2.10:18000", "--admin-address", "")
	if err != nil {
		t.Fatal(err)
	}
	b.Reset()
	if err := protojson.Unmarshal([]byte(got), &b); err != nil || b.GetAdmin() != nil {
		t.Errorf("--admin-address '' must turn the admin interface off: %v %v", err, b.GetAdmin())
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
