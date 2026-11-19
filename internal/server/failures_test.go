package server

import (
	"testing"
	"time"

	"deltav/internal/telemetry"
)

func TestFailuresPerTarget(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	f := newFailures()
	f.now = func() time.Time { return now }
	if f.of("s3") != nil {
		t.Fatal("a target with no failures says it has some")
	}
	f.record([]telemetry.Failure{
		{Target: "s3", Client: "host-a", Endpoint: "192.0.2.1:443", Reason: "first", At: now.Add(-time.Hour)},
		{Target: "s3", Client: "host-b", Endpoint: "192.0.2.2:443", Reason: "second"},
		{Target: "other", Client: "host-a", Reason: "its own"},
	})
	got := f.of("s3")
	// The latest one speaks for the target, timed by the daemon's clock.
	if got == nil || got.Reason != "second" || got.Client != "host-b" || got.Endpoint != "192.0.2.2:443" || got.LastMinute != 2 || !got.At.Equal(now) {
		t.Fatalf("s3: %+v", got)
	}
	if o := f.of("other"); o == nil || o.Reason != "its own" || o.LastMinute != 1 {
		t.Errorf("other: %+v", o)
	}

	// Half a minute on, more of them: the count is of the last minute.
	now = now.Add(30 * time.Second)
	f.record([]telemetry.Failure{{Target: "s3", Reason: "third"}, {Target: "s3", Reason: "third"}, {Target: "s3", Reason: "third"}})
	if got := f.of("s3"); got.LastMinute != 5 || got.Reason != "third" {
		t.Errorf("after 30 s: %+v", got)
	}
	now = now.Add(45 * time.Second)
	if got := f.of("s3"); got == nil || got.LastMinute != 3 {
		t.Errorf("75 s after the first two: %+v", got)
	}
	// Quiet for a while: still said, with nothing in the last minute.
	now = now.Add(3 * time.Minute)
	if got := f.of("s3"); got == nil || got.LastMinute != 0 || got.Reason != "third" {
		t.Errorf("three minutes on: %+v", got)
	}
	// And then it is history.
	now = now.Add(3 * time.Minute)
	if got := f.of("s3"); got != nil {
		t.Errorf("six minutes after the last failure the target still says %+v", got)
	}
	// A slot of the ring that comes round again does not carry its old count.
	f.record([]telemetry.Failure{{Target: "s3", Reason: "again"}})
	if got := f.of("s3"); got == nil || got.LastMinute != 1 {
		t.Errorf("after a long quiet spell: %+v", got)
	}
}
