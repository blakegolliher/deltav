package cli

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

func newLimitCmd() *cobra.Command {
	return group("limit", "Per-CNode connection limits", newLimitSetCmd())
}

// limitStatus is how the API reports a target's limit.
type limitStatus struct {
	PerCNode        uint32 `json:"per_cnode"`
	Mode            string `json:"mode"`
	Share           uint32 `json:"share"`
	Computed        uint32 `json:"computed"`
	Envoys          int    `json:"envoys"`
	MaxVIPsPerCNode int    `json:"max_vips_per_cnode"`
}

// describe says what a limit means for the fleet as it is now.
func (l *limitStatus) describe() string {
	if l == nil {
		return "no limit"
	}
	if l.Mode != "enforce" {
		return fmt.Sprintf("%d connections per CNode, alert only: CNodes over it are reported, nothing is enforced", l.PerCNode)
	}
	s := fmt.Sprintf("%d connections per CNode, enforced: each of %d Envoys may open %d to each VIP (a CNode hosts up to %d of this target's VIPs)",
		l.PerCNode, max(l.Envoys, 1), l.Share, max(l.MaxVIPsPerCNode, 1))
	if worst := uint64(l.Share) * uint64(max(l.Envoys, 1)) * uint64(max(l.MaxVIPsPerCNode, 1)); worst > uint64(l.PerCNode) {
		s += fmt.Sprintf("; the share cannot go below 1, so the fleet can reach %d", worst)
	}
	if l.Computed != l.Share {
		s += fmt.Sprintf("; the fleet has changed and %d would be right now, held back to spare Envoys a reconnect", l.Computed)
	}
	return s
}

func newLimitSetCmd() *cobra.Command {
	var (
		f        apiFlags
		perCNode uint32
		mode     string
	)
	cmd := &cobra.Command{
		Use:   "set TARGET",
		Short: "Set a target's per-CNode connection limit and mode (alert|enforce)",
		Long: "Set how many connections the whole fleet should hold to any one CNode for a target.\n" +
			"\"Connections\" are requests in flight: S3 over HTTP/1.1 holds one request per\n" +
			"connection, and idle keep-alive connections are not counted.\n\n" +
			"  alert    (default) CNodes over the limit are reported in the API, the TUI and\n" +
			"           /metrics. Nothing is enforced.\n" +
			"  enforce  Each Envoy is also given a cap on its connections to each VIP.\n\n" +
			"Two things to know about enforce:\n\n" +
			"  The cap is local to each Envoy. The limit is therefore divided by the number of\n" +
			"  connected Envoys, and by the most VIPs of the target that any one CNode hosts, and\n" +
			"  rounded down, but never below 1. A CNode hosting fewer VIPs gets less than the limit;\n" +
			"  and once Envoys x VIPs exceeds the limit, the fleet can exceed it. At its cap an\n" +
			"  Envoy queues requests for that VIP; it does not send them elsewhere.\n\n" +
			"  Changing the cap makes every Envoy reconnect its connections for the target. As\n" +
			"  Envoys come and go, a new cap is pushed only when it differs enough and not too soon\n" +
			"  after the last one (see \"serve --limit-min-change\" and \"--limit-min-interval\").\n\n" +
			"--per-cnode 0 removes the limit.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("per-cnode") {
				return errors.New("--per-cnode is required (0 removes the limit)")
			}
			path := "/api/v1/targets/" + url.PathEscape(args[0]) + "/limit"
			var got struct {
				Name   string
				Status *struct {
					Limit *limitStatus
				}
			}
			var err error
			if perCNode == 0 {
				err = f.do(cmd, http.MethodDelete, path, nil, &got)
			} else {
				err = f.do(cmd, http.MethodPut, path, map[string]any{"per_cnode": perCNode, "mode": mode}, &got)
			}
			if err != nil {
				return err
			}
			var l *limitStatus
			if got.Status != nil {
				l = got.Status.Limit
			}
			fmt.Fprintf(out(cmd), "%s: %s\n", got.Name, l.describe())
			return nil
		},
	}
	clientFlags(cmd, &f)
	fl := cmd.Flags()
	fl.Uint32Var(&perCNode, "per-cnode", 0, "connections the fleet should hold to any one CNode; 0 removes the limit")
	fl.StringVar(&mode, "mode", "alert", "alert: report only; enforce: also cap each Envoy")
	return annotateEnv(cmd)
}

// drainCmd builds "node drain" and "node undrain".
func drainCmd(drain bool) *cobra.Command {
	var (
		f       apiFlags
		cluster string
	)
	use, short, method, done := "undrain CNODE", "Return a drained CNode's VIPs to service", http.MethodDelete, "undrained"
	long := "Serve a drained CNode's VIPs to Envoys again, if the CNode is healthy."
	if drain {
		use, short, method, done = "drain CNODE", "Remove a CNode's VIPs from every Envoy", http.MethodPost, "drained"
		long = "Withhold a CNode's VIPs from every Envoy, for every target, until it is undrained.\n" +
			"Envoys stop sending it new requests at once; requests already in flight finish.\n\n" +
			"This is Delta-V's own setting: nothing is changed on the VAST cluster, and the CNode\n" +
			"keeps its VIPs there. It is remembered across daemon restarts."
	}
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Long:  long,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cluster == "" {
				// With one cluster attached there is nothing to choose.
				var v struct{ Clusters []struct{ Name string } }
				if err := f.do(cmd, http.MethodGet, "/api/v1/vast", nil, &v); err != nil {
					return err
				}
				switch len(v.Clusters) {
				case 0:
					return errors.New("no VAST cluster is attached")
				case 1:
					cluster = v.Clusters[0].Name
				default:
					names := make([]string, len(v.Clusters))
					for i, c := range v.Clusters {
						names[i] = c.Name
					}
					return fmt.Errorf("several clusters are attached (%s): say which with --cluster", strings.Join(names, ", "))
				}
			}
			path := "/api/v1/vast/" + url.PathEscape(cluster) + "/cnodes/" + url.PathEscape(args[0]) + "/drain"
			if err := f.do(cmd, method, path, nil, nil); err != nil {
				return err
			}
			fmt.Fprintf(out(cmd), "%s %s on %s\n", done, args[0], cluster)
			return nil
		},
	}
	clientFlags(cmd, &f)
	cmd.Flags().StringVar(&cluster, "cluster", "", "the CNode's cluster; needed only when several are attached")
	return annotateEnv(cmd)
}
