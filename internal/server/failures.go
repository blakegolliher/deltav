package server

import (
	"sync"
	"time"

	"deltav/internal/telemetry"
)

// A target whose VIPs cannot be reached, or whose certificate cannot be
// verified, answers every request with a 503 that VAST never sent. In the
// counts it looks like VAST throttling; Envoy logs nothing about it at its
// usual level, and the client is told only "upstream connect error". So the
// daemon keeps the reason, per target, from Envoys' access logs.

const (
	// failureSlot and failureSlots make up the time over which failures
	// are counted: the last minute, in 5-second steps.
	failureSlot  = 5 * time.Second
	failureSlots = 12
	// failureShown is how long after its last failure a target still says
	// so. Past that it is history, and the counts have it.
	failureShown = 5 * time.Minute
)

// noAnswer says that Envoys are answering requests for a target themselves,
// because the target gives no answer.
type noAnswer struct {
	At       time.Time `json:"at"`                 // the latest such request
	Client   string    `json:"client"`             // the client host it was on
	Endpoint string    `json:"endpoint,omitempty"` // the VIP it tried
	Reason   string    `json:"reason"`
	// LastMinute counts such requests in the minute before now, on every
	// client host.
	LastMinute uint64 `json:"last_minute"`
}

type targetFailures struct {
	last  telemetry.Failure
	slots [failureSlots + 1]struct {
		slot int64
		n    uint64
	}
}

type failures struct {
	mu  sync.Mutex
	now func() time.Time
	by  map[string]*targetFailures
}

func newFailures() *failures { return &failures{now: time.Now, by: map[string]*targetFailures{}} }

// record takes a batch from the telemetry server.
func (f *failures) record(batch []telemetry.Failure) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	slot := now.UnixNano() / int64(failureSlot)
	for _, b := range batch {
		t := f.by[b.Target]
		if t == nil {
			t = &targetFailures{}
			f.by[b.Target] = t
		}
		// When it was noticed, not when Envoy says the request began: the
		// two clocks need not agree, and a long request began long ago.
		b.At = now
		t.last = b
		s := &t.slots[slot%int64(len(t.slots))]
		if s.slot != slot {
			s.slot, s.n = slot, 0
		}
		s.n++
	}
}

// of returns what to say of a target, or nil if it has had an answer to
// everything lately.
func (f *failures) of(target string) *noAnswer {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.by[target]
	if t == nil {
		return nil
	}
	now := f.now()
	if now.Sub(t.last.At) > failureShown {
		delete(f.by, target)
		return nil
	}
	out := &noAnswer{At: t.last.At, Client: t.last.Client, Endpoint: t.last.Endpoint, Reason: t.last.Reason}
	slot := now.UnixNano() / int64(failureSlot)
	for _, s := range t.slots {
		if s.slot > slot-failureSlots && s.slot <= slot {
			out.LastMinute += s.n
		}
	}
	return out
}
