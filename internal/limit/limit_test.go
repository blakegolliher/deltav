package limit

import (
	"testing"
	"time"
)

func TestShare(t *testing.T) {
	for _, tc := range []struct {
		perCNode        uint32
		envoys, maxVIPs int
		want            uint32
		why             string
	}{
		{0, 10, 2, 0, "no limit: no threshold"},
		{100, 1, 1, 100, "one Envoy, one VIP per CNode: the whole limit"},
		{100, 10, 1, 10, "divided among the Envoys"},
		{100, 10, 2, 5, "and by the VIPs a CNode hosts: 10 Envoys x 2 VIPs x 5 = 100"},
		{100, 3, 2, 16, "rounded down: 3 x 2 x 16 = 96, never above the limit"},
		{100, 7, 3, 4, "rounded down: 7 x 3 x 4 = 84"},
		{100, 50, 2, 1, "exactly at the floor: 50 x 2 x 1 = 100"},
		{100, 51, 2, 1, "never below 1, although 51 x 2 x 1 exceeds the limit"},
		{100, 5000, 16, 1, "a large fleet: still 1"},
		{5, 1, 8, 1, "a limit smaller than the VIPs on a CNode: still 1"},
		{100, 0, 2, 50, "no Envoy connected yet counts as one"},
		{100, 10, 0, 10, "no VIPs resolved yet counts as one per CNode"},
		{4000000000, 1, 1, 4000000000, "large limits do not overflow"},
	} {
		if got := Share(tc.perCNode, tc.envoys, tc.maxVIPs); got != tc.want {
			t.Errorf("Share(%d, %d Envoys, %d VIPs per CNode) = %d, want %d: %s", tc.perCNode, tc.envoys, tc.maxVIPs, got, tc.want, tc.why)
		}
	}
	// The property the formula exists for: wherever the share is above the
	// floor of 1, the fleet's worst case stays within the limit.
	for limit := uint32(1); limit <= 300; limit += 7 {
		for envoys := 1; envoys <= 40; envoys++ {
			for vips := 1; vips <= 4; vips++ {
				share := Share(limit, envoys, vips)
				worst := uint64(share) * uint64(envoys) * uint64(vips)
				if share > 1 && worst > uint64(limit) {
					t.Fatalf("limit %d, %d Envoys, %d VIPs: share %d allows %d connections to one CNode", limit, envoys, vips, share, worst)
				}
				if share < 1 {
					t.Fatalf("limit %d, %d Envoys, %d VIPs: share %d", limit, envoys, vips, share)
				}
			}
		}
	}
}

func TestDebouncer(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	d := &Debouncer{MinChange: 0.2, MinInterval: time.Minute}
	step := func(after time.Duration, want, expect uint32, expectChange bool, why string) {
		t.Helper()
		now = now.Add(after)
		got, changed := d.Next(now, want)
		if got != expect || changed != expectChange || d.Current() != expect {
			t.Errorf("%s: Next(%d) = %d, %v; want %d, %v", why, want, got, changed, expect, expectChange)
		}
	}
	step(0, 0, 0, false, "nothing to enforce")
	step(0, 50, 50, true, "turning enforcement on is immediate")
	step(time.Second, 50, 50, false, "no change")
	step(time.Second, 25, 50, false, "a big change, but too soon after the last push")
	step(time.Minute, 45, 50, false, "after the interval, but only 10% different")
	step(time.Second, 40, 50, false, "exactly 20% different is not more than 20%")
	step(time.Second, 39, 39, true, "more than 20% different, and the interval has passed")
	step(time.Second, 80, 39, false, "an Envoy burst: held for the interval")
	step(time.Minute, 80, 80, true, "then pushed")
	step(time.Second, 0, 0, true, "turning enforcement off is immediate")
	step(time.Second, 80, 80, true, "and on again")

	// An operator changing the limit must not wait for the debounce.
	step(time.Second, 82, 80, false, "a small recomputation is held")
	d.Reset()
	step(0, 82, 82, true, "but after Reset the next share is pushed")
	step(time.Second, 83, 82, false, "and debouncing resumes")
	d.Reset()
	step(time.Second, 82, 82, false, "a Reset with nothing to change pushes nothing")
	step(time.Second, 60, 82, false, "and does not linger")
}

// After a restart the fleet reconnects one Envoy at a time. The share it
// already has must stay in force while that happens.
func TestDebouncerRestore(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	d := &Debouncer{MinChange: 0.2, MinInterval: time.Minute}
	d.Restore(5, now)
	if d.Current() != 5 {
		t.Fatalf("restored share = %d", d.Current())
	}
	// One Envoy back so far: the computed share is far too generous.
	if got, changed := d.Next(now.Add(2*time.Second), 50); got != 5 || changed {
		t.Errorf("while the fleet reconnects: %d, %v", got, changed)
	}
	// A minute on, the fleet is back and the computed share agrees.
	if got, changed := d.Next(now.Add(61*time.Second), 5); got != 5 || changed {
		t.Errorf("after the fleet is back: %d, %v", got, changed)
	}
}
