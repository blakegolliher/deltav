// Package version holds build metadata injected by the Makefile.
package version

// Version is set with -ldflags "-X deltav/internal/version.Version=...".
var Version = "dev"

// EnvoyPinned is the upstream Envoy release Delta-V is built and tested
// against. Keep in step with ENVOY_VERSION in the Makefile and the README.
const EnvoyPinned = "1.40.2"
