package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"deltav/internal/tui"
)

func newTUICmd() *cobra.Command {
	var f apiFlags
	cmd := &cobra.Command{
		Use:   "tui",
		Short: "Terminal UI for a running daemon",
		Long: "A live terminal view of a running daemon: targets, CNodes with their VIPs, the Envoy\n" +
			"fleet, and the attached clusters. It reads the same HTTP API as the other commands and\n" +
			"follows the daemon's event stream.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return tui.Run(cmd.Context(), &tui.Client{Base: f.base, Token: f.token})
		},
	}
	clientFlags(cmd, &f)
	return annotateEnv(cmd)
}

func newFleetCmd() *cobra.Command {
	return group("fleet", "Inspect the Envoy fleet", newFleetListCmd())
}

func newFleetListCmd() *cobra.Command {
	var (
		f                      apiFlags
		limit, offset          int
		sortKey, filter, state string
		connected, window      string
		desc                   bool
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List Envoys with version, last seen, config status and request rate",
		Long: "List the Envoys known to the daemon, one page at a time. Paging, sorting and filtering\n" +
			"happen in the daemon, so this stays quick with thousands of Envoys.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			q := url.Values{"limit": {strconv.Itoa(limit)}, "offset": {strconv.Itoa(offset)}, "sort": {sortKey}, "window": {window}}
			if desc {
				q.Set("order", "desc")
			}
			for name, v := range map[string]string{"q": filter, "state": state, "connected": connected} {
				if v != "" {
					q.Set(name, v)
				}
			}
			var raw json.RawMessage
			if err := f.do(cmd, http.MethodGet, "/api/v1/fleet?"+q.Encode(), nil, &raw); err != nil {
				return err
			}
			if f.json {
				_, err := out(cmd).Write(raw)
				return err
			}
			var v struct {
				ConfigVersion                            string `json:"config_version"`
				Total, Connected, Matched, Offset, Limit int
				Members                                  []struct {
					NodeID        string    `json:"node_id"`
					EnvoyVersion  string    `json:"envoy_version"`
					Address       string    `json:"address"`
					Connected     bool      `json:"connected"`
					LastSeen      time.Time `json:"last_seen"`
					ConfigVersion string    `json:"config_version"`
					ConfigState   string    `json:"config_state"`
					ConfigError   string    `json:"config_error"`
					InFlight      uint64    `json:"in_flight"`
					Stats         stats     `json:"stats"`
				}
			}
			if err := json.Unmarshal(raw, &v); err != nil {
				return err
			}
			tw := tabwriter.NewWriter(out(cmd), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NODE\tENVOY\tCONFIG\tLAST SEEN\tREQ/S\tIN FLIGHT\tP99\tADDRESS")
			var rejected []string
			for _, m := range v.Members {
				config := "in sync"
				switch {
				case !m.Connected:
					config = "disconnected"
				case m.ConfigState == "nacked":
					config = "rejected"
					rejected = append(rejected, m.NodeID+": "+m.ConfigError)
				case m.ConfigState == "pending":
					config = "pending"
				case m.ConfigVersion != v.ConfigVersion:
					config = "behind"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%.1f\t%d\t%s\t%s\n", m.NodeID, m.EnvoyVersion, config, ago(&m.LastSeen),
					m.Stats.RequestRate, m.InFlight, latency(m.Stats.LatencyMS.P99), m.Address)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			first := v.Offset + 1
			if len(v.Members) == 0 {
				first = 0
			}
			fmt.Fprintf(out(cmd), "\nRows %d-%d of %d matching; %d Envoys, %d connected\n", first, v.Offset+len(v.Members), v.Matched, v.Total, v.Connected)
			// Why each rejected the config, in Envoy's words.
			for _, r := range rejected {
				fmt.Fprintf(out(cmd), "rejected: %s\n", r)
			}
			return nil
		},
	}
	clientFlags(cmd, &f)
	fl := cmd.Flags()
	fl.IntVar(&limit, "limit", 50, "rows to show (at most 1000)")
	fl.IntVar(&offset, "offset", 0, "rows to skip")
	fl.StringVar(&sortKey, "sort", "node_id", "sort by node_id, request_rate, last_seen, connected_at, envoy_version, config_state or address")
	fl.BoolVar(&desc, "desc", false, "sort in descending order")
	fl.StringVar(&filter, "filter", "", "show only Envoys whose node ID, address or version contains this")
	fl.StringVar(&state, "state", "", "show only Envoys whose last config push is acked, nacked or pending")
	fl.StringVar(&connected, "connected", "", "true: only connected Envoys; false: only disconnected ones")
	fl.StringVar(&window, "window", "1m", "window for the rates: 1m, 5m or 1h")
	fl.BoolVar(&f.json, "json", false, "print the API response as JSON")
	return annotateEnv(cmd)
}

// stats is the part of the API's summary the list commands print.
type stats struct {
	RequestRate  float64 `json:"request_rate"`
	Rate403      float64 `json:"rate_403"`
	Rate503      float64 `json:"rate_503"`
	BytesInRate  float64 `json:"bytes_in_rate"`
	BytesOutRate float64 `json:"bytes_out_rate"`
	LatencyMS    struct {
		P50 float64 `json:"p50"`
		P99 float64 `json:"p99"`
	} `json:"latency_ms"`
}

// latency prints milliseconds, or "-" when there were no requests.
func latency(ms float64) string {
	if ms <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.1fms", ms)
}

func (s stats) cells(inFlight uint64) []string {
	return []string{
		strconv.FormatUint(inFlight, 10), fmt.Sprintf("%.1f", s.RequestRate),
		fmt.Sprintf("%.2f", s.BytesInRate/1e6), fmt.Sprintf("%.2f", s.BytesOutRate/1e6),
		latency(s.LatencyMS.P50), latency(s.LatencyMS.P99), fmt.Sprintf("%.1f", s.Rate403), fmt.Sprintf("%.1f", s.Rate503),
	}
}

func newNodeCmd() *cobra.Command {
	return group("node", "Inspect and control VAST CNodes",
		newNodeListCmd(), drainCmd(true), drainCmd(false))
}

func newNodeListCmd() *cobra.Command {
	var (
		f      apiFlags
		window string
		vips   bool
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List CNodes with state, VIPs, requests in flight and traffic",
		Long: "List the CNodes of the attached VAST clusters with the traffic Envoys send them. A\n" +
			"CNode's numbers are those of the VIPs it hosts now, in the pools of configured targets.\n\n" +
			"IN FLIGHT is the number of requests Envoys have outstanding, from load reports; idle\n" +
			"keep-alive connections are not counted.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			type row struct {
				Target, Endpoint string
				InFlight         uint64 `json:"in_flight"`
				Stats            stats  `json:"stats"`
			}
			type cnode struct {
				Cluster, Name, State      string
				Enabled, Healthy, Drained bool
				VIPs                      int
				InFlight                  uint64 `json:"in_flight"`
				Stats                     stats  `json:"stats"`
				Endpoints                 []row
				Limits                    []struct {
					Target   string
					InFlight uint64 `json:"in_flight"`
					PerCNode uint32 `json:"per_cnode"`
					Over     bool
				}
			}
			header := "CLUSTER\tCNODE\tSTATE\tVIPS\tIN FLIGHT\tREQ/S\tMB/S IN\tMB/S OUT\tP50\tP99\t403/S\t503/S\tLIMIT"
			return show(cmd, &f, "/api/v1/cnodes?window="+url.QueryEscape(window), header,
				func(v struct{ CNodes []cnode }) [][]string {
					var rows [][]string
					for _, c := range v.CNodes {
						state := c.State
						switch {
						case !c.Enabled:
							state += " (disabled)"
						case !c.Healthy:
							state += " (not serving)"
						case c.Drained:
							state += " (drained)"
						}
						// In flight against each target's limit: "s3 6 of 5 OVER".
						var limits []string
						for _, l := range c.Limits {
							cell := fmt.Sprintf("%s %d of %d", l.Target, l.InFlight, l.PerCNode)
							if l.Over {
								cell += " OVER"
							}
							limits = append(limits, cell)
						}
						if len(limits) == 0 {
							limits = []string{"-"}
						}
						row := append([]string{c.Cluster, c.Name, state, strconv.Itoa(c.VIPs)}, c.Stats.cells(c.InFlight)...)
						rows = append(rows, append(row, strings.Join(limits, ", ")))
						if vips {
							for _, e := range c.Endpoints {
								rows = append(rows, append([]string{"", "  " + e.Endpoint, strings.TrimSpace("target " + e.Target), ""}, e.Stats.cells(e.InFlight)...))
							}
						}
					}
					return rows
				})
		},
	}
	clientFlags(cmd, &f)
	fl := cmd.Flags()
	fl.StringVar(&window, "window", "1m", "window for the rates: 1m, 5m or 1h")
	fl.BoolVar(&vips, "vips", false, "show a row per VIP beneath each CNode")
	fl.BoolVar(&f.json, "json", false, "print the API response as JSON")
	return annotateEnv(cmd)
}
