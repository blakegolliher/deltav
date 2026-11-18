// Package systemd holds the systemd units Delta-V ships: one for the daemon
// and one for Envoy on every client host. They are files here so that they
// can be read and installed as they are, and embedded so that "deltav unit"
// can print the ones that match the binary.
package systemd

import _ "embed"

// Daemon is deltav.service.
//
//go:embed deltav.service
var Daemon string

// Client is deltav-envoy.service.
//
//go:embed deltav-envoy.service
var Client string

// DaemonEnv and ClientEnv are the example environment files the units read.
//
//go:embed deltav.env.example
var DaemonEnv string

//go:embed envoy.env.example
var ClientEnv string
