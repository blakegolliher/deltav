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
			"The systemd unit that does this arrives in milestone 8.",
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
	f.StringVar(&o.AdminAddress, "admin-address", "127.0.0.1:9901", "ip:port for Envoy's local admin interface; empty turns it off")
	return annotateEnv(cmd)
}
