package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"

	"deltav/internal/server"
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

// notYet marks a command whose implementation lands in a later milestone, so
// the command tree and its help text are complete from the start.
func notYet(milestone int) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		return fmt.Errorf("%q is not implemented yet (milestone %d)", cmd.CommandPath(), milestone)
	}
}

func leaf(use, short string, milestone int) *cobra.Command {
	return &cobra.Command{Use: use, Short: short, RunE: notYet(milestone)}
}

func group(use, short string, children ...*cobra.Command) *cobra.Command {
	cmd := &cobra.Command{Use: use, Short: short, Args: cobra.NoArgs}
	cmd.AddCommand(children...)
	return cmd
}

func newTUICmd() *cobra.Command {
	return leaf("tui", "Terminal UI for a running daemon", 6)
}

func newVastCmd() *cobra.Command {
	return group("vast", "Attach the daemon to VAST clusters",
		leaf("add NAME", "Add a VAST cluster (management endpoint and credentials)", 4),
		leaf("list", "List attached VAST clusters", 4),
		leaf("remove NAME", "Detach a VAST cluster", 4),
	)
}

func newTargetCmd() *cobra.Command {
	return group("target", "Define what Envoys balance across (cluster, VIP pool, port, TLS)",
		leaf("add NAME", "Add a target", 6),
		leaf("list", "List targets", 6),
		leaf("remove NAME", "Remove a target", 6),
	)
}

func newFleetCmd() *cobra.Command {
	return group("fleet", "Inspect the Envoy fleet",
		leaf("list", "List connected Envoys with version, last seen and config status", 6),
	)
}

func newNodeCmd() *cobra.Command {
	return group("node", "Inspect and control VAST CNodes",
		leaf("list", "List CNodes with state, VIPs, connections and traffic", 6),
		leaf("drain CNODE", "Remove a CNode's VIPs from every Envoy", 7),
		leaf("undrain CNODE", "Return a drained CNode's VIPs to service", 7),
	)
}

func newLimitCmd() *cobra.Command {
	return group("limit", "Per-CNode connection limits",
		leaf("set TARGET", "Set a target's per-CNode connection limit and mode (alert|enforce)", 7),
	)
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
