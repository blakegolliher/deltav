package server

import "testing"

// TestReviewS4_UnexpandedShellTokenRefused: systemd's EnvironmentFile= does
// no command substitution, so uncommenting the example line in
// packaging/systemd/deltav.env.example hands the daemon the literal text
// "$(openssl rand -hex 32)" (verified with systemd-run on systemd 252). The
// daemon should not accept that as the API token of a network-bound API.
// The primary fix is the example's wording; this test covers the optional
// hardening in New.
func TestReviewS4_UnexpandedShellTokenRefused(t *testing.T) {
	const literal = "$(openssl rand -hex 32)"
	if _, err := New(Config{GRPCAddr: "127.0.0.1:0", HTTPAddr: "0.0.0.0:0", APIToken: literal}); err == nil {
		t.Errorf("New accepted the unexpanded example %q as the API token of a non-loopback API", literal)
	}
}
