package systemd

import (
	"regexp"
	"strings"
	"testing"

	"deltav/internal/fleet"
)

// settings returns a unit's settings, with continuation lines joined.
func settings(unit string) map[string][]string {
	out := map[string][]string{}
	lines := strings.Split(unit, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
			continue
		}
		for strings.HasSuffix(line, "\\") && i+1 < len(lines) {
			i++
			line = strings.TrimSuffix(line, "\\") + " " + strings.TrimSpace(lines[i])
		}
		k, v, _ := strings.Cut(line, "=")
		out[k] = append(out[k], strings.Join(strings.Fields(v), " "))
	}
	return out
}

func TestDaemonUnit(t *testing.T) {
	u := settings(Daemon)
	// "deltav serve" keeps its state in /var/lib/deltav unless told
	// otherwise; StateDirectory is what makes that exist, privately.
	for k, want := range map[string]string{
		"ExecStart": "/usr/local/bin/deltav serve", "StateDirectory": "deltav", "StateDirectoryMode": "0700",
		"EnvironmentFile": "-/etc/deltav/deltav.env", "DynamicUser": "yes", "WantedBy": "multi-user.target",
	} {
		if len(u[k]) != 1 || u[k][0] != want {
			t.Errorf("%s = %q, want %q", k, u[k], want)
		}
	}
	if !strings.Contains(DaemonEnv, "DELTAV_API_TOKEN") || regexp.MustCompile(`(?m)^[A-Z_]+=`).MatchString(DaemonEnv) {
		t.Error("the example environment file should name the token and set nothing by itself")
	}
}

// The client unit's command line is run for real by the integration tests.
// Here: the things that must not drift apart.
func TestClientUnit(t *testing.T) {
	u := settings(Client)
	if len(u["ExecStart"]) != 1 {
		t.Fatalf("ExecStart: %q", u["ExecStart"])
	}
	cmd := u["ExecStart"][0]
	for _, want := range []string{
		"/usr/local/bin/envoy ",
		"--config-path /etc/deltav/envoy.json",
		"--service-node %H", // the host's name is the Envoy's name in the fleet
		// One variable sets Envoy's worker threads and tells the daemon
		// how many there are; the daemon's cap depends on the two agreeing.
		"--concurrency ${DELTAV_ENVOY_WORKERS}",
		`--config-yaml "node: {metadata: {` + fleet.WorkersMetadataKey + `: ${DELTAV_ENVOY_WORKERS}}}"`,
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("ExecStart lacks %q:\n%s", want, cmd)
		}
	}
	if strings.Count(cmd, "DELTAV_ENVOY_WORKERS") != 2 {
		t.Errorf("the worker count should be used exactly twice:\n%s", cmd)
	}
	env := u["Environment"]
	if len(env) != 1 || !regexp.MustCompile(`^DELTAV_ENVOY_WORKERS=[1-9][0-9]*$`).MatchString(env[0]) {
		t.Errorf("the unit must give DELTAV_ENVOY_WORKERS a default, a positive number: %q", env)
	}
	if got := u["EnvironmentFile"]; len(got) != 1 || got[0] != "-/etc/deltav/envoy.env" {
		t.Errorf("EnvironmentFile = %q", got)
	}
	if !strings.Contains(ClientEnv, "DELTAV_ENVOY_WORKERS") {
		t.Error("the example environment file should name DELTAV_ENVOY_WORKERS")
	}
}
