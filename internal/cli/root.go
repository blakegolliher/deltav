// Package cli defines the deltav command tree. The daemon, the CLI, the TUI
// and the web UI share one HTTP API; commands in this package other than
// "serve" are clients of that API and never touch daemon state directly.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// EnvPrefix is prepended to a flag's upper-cased name to form the
// environment variable that sets it, e.g. --http-addr -> DELTAV_HTTP_ADDR.
const EnvPrefix = "DELTAV_"

// Main runs the CLI. args[0] is the invoked path; its base name ("deltav" or
// "dv") is what help and usage text print. It returns the process exit code.
func Main(args []string) int {
	name := invokedName(args)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	root := NewRoot(name)
	root.SetArgs(args[1:])
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintf(root.ErrOrStderr(), "%s: %v\n", name, err)
		return 1
	}
	return 0
}

func invokedName(args []string) string {
	if len(args) == 0 {
		return "deltav"
	}
	name := filepath.Base(args[0])
	if name == "" || name == "." || name == string(filepath.Separator) {
		return "deltav"
	}
	return name
}

// NewRoot builds the command tree. name is used in every usage line.
func NewRoot(name string) *cobra.Command {
	root := &cobra.Command{
		Use:   name,
		Short: "Delta-V: control plane for client-side S3 load balancing with Envoy",
		Long: "Delta-V steers S3 traffic from a fleet of loopback Envoys across VAST VIPs.\n" +
			"\"" + name + " serve\" runs the daemon; the other commands are clients of its HTTP API.",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			return applyEnv(cmd.Flags(), os.LookupEnv)
		},
	}
	root.CompletionOptions.HiddenDefaultCmd = true

	root.AddCommand(
		newServeCmd(),
		newVersionCmd(),
		newTUICmd(),
		newVastCmd(),
		newTargetCmd(),
		newFleetCmd(),
		newNodeCmd(),
		newLimitCmd(),
		newBootstrapCmd(),
		newUnitCmd(),
	)
	return root
}

// envKey maps a flag name to its environment variable.
func envKey(flag string) string {
	return EnvPrefix + strings.ToUpper(strings.ReplaceAll(flag, "-", "_"))
}

// annotateEnv appends the environment variable name to each local flag's
// help text, so --help documents both ways of setting it.
func annotateEnv(cmd *cobra.Command) *cobra.Command {
	cmd.LocalFlags().VisitAll(func(f *pflag.Flag) {
		if f.Name != "help" {
			f.Usage += " (env " + envKey(f.Name) + ")"
		}
	})
	return cmd
}

// applyEnv sets every flag the user did not pass on the command line from
// its DELTAV_ environment variable, if present. Command-line flags win.
func applyEnv(fs *pflag.FlagSet, lookup func(string) (string, bool)) error {
	var errs []error
	fs.VisitAll(func(f *pflag.Flag) {
		if f.Changed || f.Name == "help" {
			return
		}
		key := envKey(f.Name)
		if v, ok := lookup(key); ok {
			if err := fs.Set(f.Name, v); err != nil {
				errs = append(errs, fmt.Errorf("%s=%q: %w", key, v, err))
			}
		}
	})
	return errors.Join(errs...)
}

// out returns the command's stdout writer (overridable in tests).
func out(cmd *cobra.Command) io.Writer { return cmd.OutOrStdout() }
