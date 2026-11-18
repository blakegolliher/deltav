package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"

	"deltav/internal/limit"
	"deltav/internal/server"
	"deltav/internal/telemetry"
	"deltav/internal/version"
	"deltav/internal/xds"
	"deltav/packaging/systemd"
)

func newServeCmd() *cobra.Command {
	var cfg server.Config
	var static []string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the daemon: xDS, telemetry ingest, VAST pollers, HTTP API, web UI, /metrics",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			targets, err := parseStaticTargets(static)
			if err != nil {
				return err
			}
			cfg.StaticTargets = targets
			s, err := server.New(cfg)
			if err != nil {
				return err
			}
			return s.Run(cmd.Context())
		},
	}
	f := cmd.Flags()
	f.StringVar(&cfg.GRPCAddr, "grpc-addr", "0.0.0.0:18000", "listen address for xDS (ADS) and Envoy telemetry streams")
	f.StringVar(&cfg.HTTPAddr, "http-addr", "127.0.0.1:18080", "listen address for the HTTP API, web UI and /metrics")
	f.StringVar(&cfg.APIToken, "api-token", "", "bearer token for the HTTP API; required when --http-addr is not loopback (prefer the env var)")
	f.StringVar(&cfg.StateDir, "state-dir", "/var/lib/deltav", "directory for the state file (VAST clusters with their credentials, targets), kept at mode 0600")
	f.DurationVar(&cfg.VASTPollInterval, "vast-poll-interval", server.DefaultVASTPollInterval, "how often each attached VAST cluster is read (three GETs)")
	f.Float64Var(&cfg.LimitMinChange, "limit-min-change", limit.DefaultMinChange, "enforce mode: push a new per-Envoy share only if it differs from the one in force by more than this fraction")
	f.DurationVar(&cfg.LimitMinInterval, "limit-min-interval", limit.DefaultMinInterval, "enforce mode: never push a new per-Envoy share sooner than this after the last one")
	f.DurationVar(&cfg.LoadReportInterval, "load-report-interval", telemetry.DefaultLoadReportInterval, "how often each Envoy reports in-flight requests per endpoint")
	f.BoolVar(&cfg.MetricsPerMember, "metrics-per-member", false, "add per-Envoy series to /metrics (many series on a large fleet)")
	f.StringArrayVar(&static, "static-target", nil, "serve a fixed endpoint list with no VAST cluster, for testing: "+staticTargetSyntax+"; repeat the flag, or separate targets with ';'")
	return annotateEnv(cmd)
}

// newUnitCmd prints the systemd units and their example environment files,
// so that the ones installed match the binary.
func newUnitCmd() *cobra.Command {
	files := map[string]string{
		"daemon":     systemd.Daemon,
		"client":     systemd.Client,
		"daemon-env": systemd.DaemonEnv,
		"client-env": systemd.ClientEnv,
	}
	return &cobra.Command{
		Use:   "unit daemon|client|daemon-env|client-env",
		Short: "Print a systemd unit, or the example environment file it reads",
		Long: "Print a systemd unit, or the example environment file it reads.\n\n" +
			"  daemon      deltav.service, for the host that runs \"deltav serve\"\n" +
			"  client      deltav-envoy.service, for every client host; it runs Envoy with\n" +
			"              /etc/deltav/envoy.json, which is the output of \"deltav bootstrap\"\n" +
			"  daemon-env  /etc/deltav/deltav.env: listen addresses and the API token\n" +
			"  client-env  /etc/deltav/envoy.env: Envoy's worker threads\n\n" +
			"For example, on the daemon host:\n\n" +
			"  deltav unit daemon > /etc/systemd/system/deltav.service\n" +
			"  systemctl daemon-reload && systemctl enable --now deltav",
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"daemon", "client", "daemon-env", "client-env"},
		RunE: func(cmd *cobra.Command, args []string) error {
			text, ok := files[args[0]]
			if !ok {
				return fmt.Errorf("unknown file %q: want daemon, client, daemon-env or client-env", args[0])
			}
			_, err := fmt.Fprint(out(cmd), text)
			return err
		},
	}
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintf(out(cmd), "%s %s (envoy %s, %s)\n",
				cmd.Root().Name(), version.Version, version.EnvoyPinned, runtime.Version())
		},
	}
}

func group(use, short string, children ...*cobra.Command) *cobra.Command {
	cmd := &cobra.Command{Use: use, Short: short, Args: cobra.NoArgs}
	cmd.AddCommand(children...)
	return cmd
}

func newBootstrapCmd() *cobra.Command {
	var o xds.BootstrapOptions
	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Print the Envoy bootstrap file for client hosts",
		Long: "Print the Envoy bootstrap file (JSON) for client hosts. One file serves the whole\n" +
			"fleet: the node ID is not in it, so start Envoy with --service-node, for example\n\n" +
			"  envoy -c bootstrap.json --service-node \"$(hostname)\"\n\n" +
			"\"deltav unit client\" prints the systemd unit that does this; it expects the file at\n" +
			"/etc/deltav/envoy.json.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if o.XDSAddress == "" {
				return fmt.Errorf("--xds-address is required: the daemon's gRPC address as client hosts reach it, e.g. deltav.example.com:18000")
			}
			b, err := xds.BootstrapJSON(o)
			if err != nil {
				return err
			}
			_, err = out(cmd).Write(b)
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.XDSAddress, "xds-address", "", "host:port of the daemon's gRPC listener, as client hosts reach it")
	f.StringVar(&o.NodeCluster, "node-cluster", xds.DefaultNodeCluster, "xDS node cluster name the Envoys report")
	f.StringVar(&o.AdminAddress, "admin-address", "", "ip:port for Envoy's admin interface, e.g. 127.0.0.1:9901; off by default, because it has no authentication and anyone on the host who can reach it can stop Envoy")
	return annotateEnv(cmd)
}
