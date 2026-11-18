// Package limit works out how a per-CNode connection limit, which is a
// property of the whole fleet, is enforced by Envoys that each know only
// about themselves.
//
// Envoy's per_host_thresholds.max_connections caps the connections one
// Envoy opens to one endpoint, which is one VIP. Three facts follow.
//
// The threshold is local to each Envoy, so the fleet's limit has to be
// divided among the Envoys; and it applies per VIP, with one value for the
// whole cluster, so it has to be divided again by the largest number of the
// target's VIPs that any one CNode hosts (DESIGN.md, amendment 1).
//
// The threshold leaks. Envoy lets a worker thread whose own pool to a host
// is empty open one connection whatever the count, so an Envoy with W
// workers can hold W - 1 more than the threshold; the share is reduced by
// that much (amendments 34 and 40).
//
// The threshold lives on Envoy's Cluster resource, where a change makes
// Envoy rebuild the cluster and reconnect its connection pools. The share
// moves whenever an Envoy joins or leaves, so changes are debounced.
package limit

import "time"

// Share is the threshold to give every Envoy so that the fleet stays at or
// under perCNode connections to any one CNode:
//
//	max(1, floor(perCNode / (envoys × maxVIPsPerCNode)) - (workers - 1))
//
// An Envoy with that threshold holds at most share + workers - 1 connections
// to a VIP, which is the first term whenever the result is above 1.
//
// It is never below 1: an Envoy that may open no connection cannot serve.
// Once envoys × maxVIPsPerCNode × workers exceeds perCNode (see Holds) the
// fleet can therefore exceed the limit, which alerting still reports. A
// perCNode of 0 means no limit and gives 0, which Envoy takes as "no
// threshold". envoys, maxVIPsPerCNode and workers below 1 count as 1.
func Share(perCNode uint32, envoys, maxVIPsPerCNode, workers int) uint32 {
	if perCNode == 0 {
		return 0
	}
	divisor := uint64(max(envoys, 1)) * uint64(max(maxVIPsPerCNode, 1))
	each := uint64(perCNode) / divisor // connections one Envoy may hold to one VIP
	leak := uint64(max(workers, 1) - 1)
	if each <= leak {
		return 1
	}
	return uint32(each - leak)
}

// Holds reports whether a fleet of this shape can be kept within perCNode
// at all: whether even the smallest threshold, 1, at which each Envoy can
// still hold `workers` connections to a VIP, keeps the total at or under
// the limit.
func Holds(perCNode uint32, envoys, maxVIPsPerCNode, workers int) bool {
	return uint64(max(envoys, 1))*uint64(max(maxVIPsPerCNode, 1))*uint64(max(workers, 1)) <= uint64(perCNode)
}

// Defaults for a Debouncer.
const (
	DefaultMinChange   = 0.2
	DefaultMinInterval = time.Minute
)

// Debouncer decides when a newly computed share is worth pushing to the
// fleet. A push costs every Envoy its connection pools for that target, so
// a share is pushed only if it differs from the one in force by more than
// MinChange (a fraction of the one in force), and never sooner than
// MinInterval after the previous push.
//
// Turning enforcement on or off is not debounced, and neither is the first
// share after Reset, which callers use when an operator changes the limit.
type Debouncer struct {
	MinChange   float64
	MinInterval time.Duration

	current  uint32
	pushedAt time.Time
	force    bool
}

// Current is the share in force: the last one Next told the caller to push.
func (d *Debouncer) Current() uint32 { return d.current }

// Restore sets the share in force without a push, as of now. A restarted
// daemon uses it to carry on with the share the fleet already has, so that
// reconnecting Envoys are not handed a different one while their number
// climbs back.
func (d *Debouncer) Restore(share uint32, now time.Time) {
	d.current, d.pushedAt, d.force = share, now, false
}

// Reset makes the next call to Next push whatever it is given.
func (d *Debouncer) Reset() { d.force = true }

// Next takes the share computed now and returns the share to serve, and
// whether that is a change to push.
func (d *Debouncer) Next(now time.Time, want uint32) (share uint32, changed bool) {
	if want == d.current {
		d.force = false
		return d.current, false
	}
	if !d.force && want != 0 && d.current != 0 {
		if now.Sub(d.pushedAt) < d.MinInterval {
			return d.current, false
		}
		diff := float64(want) - float64(d.current)
		if diff < 0 {
			diff = -diff
		}
		if diff/float64(d.current) <= d.MinChange {
			return d.current, false
		}
	}
	d.current, d.pushedAt, d.force = want, now, false
	return want, true
}
