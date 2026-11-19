package telemetry

import (
	"strings"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	accesslogdatav3 "github.com/envoyproxy/go-control-plane/envoy/data/accesslog/v3"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"deltav/internal/metrics"
)

// The streaming paths are tested against a running daemon in
// internal/server; this covers the conversion's edge cases.
func TestSampleFrom(t *testing.T) {
	upstream := &corev3.Address{Address: &corev3.Address_SocketAddress{SocketAddress: &corev3.SocketAddress{
		Address: "2001:db8::7", PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: 443},
	}}}
	full := &accesslogdatav3.HTTPAccessLogEntry{
		CommonProperties: &accesslogdatav3.AccessLogCommon{
			UpstreamCluster:            "s3",
			UpstreamRemoteAddress:      upstream,
			Duration:                   durationpb.New(250 * time.Millisecond),
			TimeToLastDownstreamTxByte: durationpb.New(200 * time.Millisecond),
			ResponseFlags:              &accesslogdatav3.ResponseFlags{UpstreamOverflow: true},
		},
		Request:  &accesslogdatav3.HTTPRequestProperties{RequestBodyBytes: 11},
		Response: &accesslogdatav3.HTTPResponseProperties{ResponseCode: wrapperspb.UInt32(503), ResponseBodyBytes: 22},
	}
	got, ok := sampleFrom(full, "log-name")
	want := metrics.Sample{
		Target: "s3", Endpoint: "[2001:db8::7]:443", Status: 503, Duration: 250 * time.Millisecond,
		BytesIn: 11, BytesOut: 22, Overflow: true,
	}
	if !ok || got != want {
		t.Errorf("full entry:\n got %+v\nwant %+v", got, want)
	}

	// No cluster was selected, no upstream, no response code, and only the
	// older timing field: the log name stands in for the target.
	bare := &accesslogdatav3.HTTPAccessLogEntry{CommonProperties: &accesslogdatav3.AccessLogCommon{
		TimeToLastDownstreamTxByte: durationpb.New(40 * time.Millisecond),
	}}
	got, ok = sampleFrom(bare, "log-name")
	if want := (metrics.Sample{Target: "log-name", Duration: 40 * time.Millisecond}); !ok || got != want {
		t.Errorf("bare entry:\n got %+v\nwant %+v", got, want)
	}

	if _, ok := sampleFrom(&accesslogdatav3.HTTPAccessLogEntry{}, "x"); !ok {
		t.Error("an empty entry must still count as a request")
	}
	running := &accesslogdatav3.HTTPAccessLogEntry{CommonProperties: &accesslogdatav3.AccessLogCommon{IntermediateLogEntry: true}}
	if _, ok := sampleFrom(running, "x"); ok {
		t.Error("an intermediate entry was counted as a finished request")
	}
}

// A 5xx that Envoy made up because the target gave no answer is told apart
// from one the target sent, and comes with a reason. The TLS wordings are
// what Envoy 1.40.2 put in its access log for each case, copied from a run.
func TestFailureFrom(t *testing.T) {
	entry := func(status uint32, details, transport string, flags *accesslogdatav3.ResponseFlags) *accesslogdatav3.HTTPAccessLogEntry {
		return &accesslogdatav3.HTTPAccessLogEntry{
			CommonProperties: &accesslogdatav3.AccessLogCommon{UpstreamCluster: "s3", ResponseFlags: flags, UpstreamTransportFailureReason: transport},
			Response:         &accesslogdatav3.HTTPResponseProperties{ResponseCode: wrapperspb.UInt32(status), ResponseCodeDetails: details},
		}
	}
	const reset = "upstream_reset_before_response_started{remote_connection_failure}"
	uf := &accesslogdatav3.ResponseFlags{UpstreamConnectionFailure: true}
	for _, c := range []struct {
		what string
		e    *accesslogdatav3.HTTPAccessLogEntry
		want string // "" if it is not a failure
	}{
		{"VAST's own 503", entry(503, "via_upstream", "", nil), ""},
		{"VAST's own 200", entry(200, "via_upstream", "", nil), ""},
		{"an Envoy that does not say where the response came from", entry(503, "", "", uf), ""},
		{"over the connection limit: Envoy's doing, and meant", entry(503, "overflow", "", &accesslogdatav3.ResponseFlags{UpstreamOverflow: true}), ""},
		{"over a rate rule: likewise", entry(503, "rate_limited_by_quota", "", nil), ""},
		{"a malformed request Envoy turned away", entry(400, "http1.codec_error", "", nil), ""},
		{"a certificate for other names", entry(503, reset,
			"TLS_error:|268435581:SSL routines:OPENSSL_internal:CERTIFICATE_VERIFY_FAILED:verify cert failed: SAN matcher, certificate SANs are [s3.lab.example, *.pools.lab.example]:TLS_error_end", uf),
			"VAST's certificate is not for the target's server name; it is for s3.lab.example, *.pools.lab.example (268435581:SSL routines:OPENSSL_internal:CERTIFICATE_VERIFY_FAILED:verify cert failed: SAN matcher, certificate SANs are [s3.lab.example, *.pools.lab.example])"},
		{"a certificate from another authority", entry(503, reset,
			"TLS_error:|268435581:SSL routines:OPENSSL_internal:CERTIFICATE_VERIFY_FAILED:verify cert failed: X509_verify_cert: certificate verification error at depth 0: unable to get local issuer certificate:TLS_error_end", uf),
			"VAST's certificate does not chain to the target's CA file (268435581:SSL routines:OPENSSL_internal:CERTIFICATE_VERIFY_FAILED:verify cert failed: X509_verify_cert: certificate verification error at depth 0: unable to get local issuer certificate)"},
		{"some other TLS failure", entry(503, reset, "TLS_error:|268435703:SSL routines:OPENSSL_internal:WRONG_VERSION_NUMBER:TLS_error_end", uf),
			"the TLS handshake failed (268435703:SSL routines:OPENSSL_internal:WRONG_VERSION_NUMBER)"},
		{"nothing listening", entry(503, reset, "", uf), "could not connect (" + reset + ")"},
		{"every endpoint failing its health check", entry(503, "no_healthy_upstream", "", &accesslogdatav3.ResponseFlags{NoHealthyUpstream: true}), "no endpoint is passing its health check (no_healthy_upstream)"},
		{"no answer in time", entry(504, "upstream_response_timeout", "", &accesslogdatav3.ResponseFlags{UpstreamRequestTimeout: true}), "no answer in time (upstream_response_timeout)"},
		{"closed before an answer", entry(503, "upstream_reset_before_response_started{connection_termination}", "", &accesslogdatav3.ResponseFlags{UpstreamConnectionTermination: true}),
			"the connection was closed before an answer (upstream_reset_before_response_started{connection_termination})"},
		{"something this does not know", entry(500, "something_new", "", nil), "Envoy answered in the target's place (something_new)"},
	} {
		sample, _ := sampleFrom(c.e, "s3")
		got, ok := failureFrom(c.e, "client-1", sample)
		switch {
		case c.want == "" && ok:
			t.Errorf("%s: taken for a failure: %q", c.what, got.Reason)
		case c.want != "" && (!ok || got.Reason != c.want || got.Client != "client-1" || got.Target != "s3"):
			t.Errorf("%s:\n got %+v (failure: %v)\nwant reason %q", c.what, got, ok, c.want)
		}
	}
	long := entry(503, reset, "TLS_error:|"+strings.Repeat("x", 5000)+":TLS_error_end", uf)
	sample, _ := sampleFrom(long, "s3")
	if got, _ := failureFrom(long, "c", sample); len(got.Reason) > 400 {
		t.Errorf("a reason of %d bytes was kept whole", len(got.Reason))
	}
}
