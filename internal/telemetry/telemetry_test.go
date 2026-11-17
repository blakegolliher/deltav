package telemetry

import (
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
