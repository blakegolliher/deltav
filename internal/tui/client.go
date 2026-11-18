// Package tui is the terminal UI. It is a client of the daemon's HTTP API,
// like the CLI and the web UI, and has no other way to reach daemon state.
package tui

import "deltav/internal/apiclient"

// The API client and its shapes live in package apiclient, which the web UI
// shares. The names the TUI uses are kept here.
type (
	Client       = apiclient.Client
	Summary      = apiclient.Summary
	FleetCounts  = apiclient.FleetCounts
	LimitStatus  = apiclient.LimitStatus
	Usage        = apiclient.Usage
	Target       = apiclient.Target
	Overview     = apiclient.Overview
	VIPRow       = apiclient.VIPRow
	CNode        = apiclient.CNode
	CNodes       = apiclient.CNodes
	Member       = apiclient.Member
	Fleet        = apiclient.Fleet
	TargetStatus = apiclient.TargetStatus
	TargetInfo   = apiclient.TargetInfo
	Cluster      = apiclient.Cluster
	FleetQuery   = apiclient.FleetQuery
	Event        = apiclient.Event
)
