package cli

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"
)

func newVastCmd() *cobra.Command {
	return group("vast", "Attach the daemon to VAST clusters",
		newVastAddCmd(), newVastListCmd(), newVastRemoveCmd(), newVastPoolsCmd())
}

func newVastAddCmd() *cobra.Command {
	var (
		f                    apiFlags
		host, user, tenant   string
		password             string
		passwordStdin, insec bool
	)
	cmd := &cobra.Command{
		Use:   "add NAME",
		Short: "Attach a VAST cluster (management endpoint and credentials)",
		Long: "Attach a VAST cluster. The daemon logs in and reads the cluster once before storing\n" +
			"it, so wrong credentials fail here. It only ever issues GETs and the token call.\n\n" +
			"At a terminal you are asked for the password. Otherwise give it in DELTAV_PASSWORD or\n" +
			"on standard input with --password-stdin; --password works too but leaves it in your\n" +
			"shell history. The daemon keeps it in its state file (mode 0600) and never returns it.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if host == "" || user == "" {
				return errors.New("--host and --user are required")
			}
			if passwordStdin {
				if password != "" {
					return errors.New("give the password one way: --password-stdin, or DELTAV_PASSWORD / --password")
				}
				b, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 4096))
				if err != nil {
					return err
				}
				password = strings.TrimRight(string(b), "\r\n")
			}
			if password == "" && !passwordStdin && term.IsTerminal(os.Stdin.Fd()) {
				// At a terminal: ask, without echoing.
				fmt.Fprintf(cmd.ErrOrStderr(), "Password for %s on %s: ", user, host)
				b, err := term.ReadPassword(os.Stdin.Fd())
				fmt.Fprintln(cmd.ErrOrStderr())
				if err != nil {
					return err
				}
				password = string(b)
			}
			if password == "" {
				return errors.New("no password: set DELTAV_PASSWORD, or pipe it in with --password-stdin")
			}
			in := map[string]any{"name": args[0], "host": host, "user": user, "password": password, "tenant": tenant, "insecure": insec}
			var got struct {
				Name                string
				CNodes, Pools, VIPs int
			}
			if err := f.do(cmd, http.MethodPost, "/api/v1/vast", in, &got); err != nil {
				return err
			}
			fmt.Fprintf(out(cmd), "attached %s: %d CNodes, %d VIP pools, %d VIPs\n", got.Name, got.CNodes, got.Pools, got.VIPs)
			return nil
		},
	}
	clientFlags(cmd, &f)
	fl := cmd.Flags()
	fl.StringVar(&host, "host", "", "VAST management host, optionally host:port (HTTPS is always used)")
	fl.StringVar(&user, "user", "", "VAST user")
	fl.StringVar(&password, "password", "", "VAST password (prefer the env var or --password-stdin)")
	fl.BoolVar(&passwordStdin, "password-stdin", false, "read the password from standard input")
	fl.StringVar(&tenant, "tenant", "", "log in to this tenant instead of the default")
	fl.BoolVar(&insec, "insecure", false, "skip certificate verification (lab clusters with self-signed certificates)")
	return annotateEnv(cmd)
}

type clusterRow struct {
	Name, Host, User    string
	CNodes, Pools, VIPs int
	LastPoll            *time.Time `json:"last_poll"`
	LastError           string     `json:"last_error"`
}

func newVastListCmd() *cobra.Command {
	var f apiFlags
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List attached VAST clusters",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return show(cmd, &f, "/api/v1/vast", "NAME\tHOST\tUSER\tCNODES\tPOOLS\tVIPS\tLAST READ\tSTATUS",
				func(v struct{ Clusters []clusterRow }) [][]string {
					var rows [][]string
					for _, c := range v.Clusters {
						status := "ok"
						if c.LastError != "" {
							status = "failing: " + c.LastError
						}
						rows = append(rows, []string{c.Name, c.Host, c.User, fmt.Sprint(c.CNodes), fmt.Sprint(c.Pools), fmt.Sprint(c.VIPs), ago(c.LastPoll), status})
					}
					return rows
				})
		},
	}
	clientFlags(cmd, &f)
	cmd.Flags().BoolVar(&f.json, "json", false, "print the API response as JSON")
	return annotateEnv(cmd)
}

func newVastRemoveCmd() *cobra.Command {
	var f apiFlags
	cmd := &cobra.Command{
		Use:   "remove NAME",
		Short: "Detach a VAST cluster that no target uses",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := f.do(cmd, http.MethodDelete, "/api/v1/vast/"+url.PathEscape(args[0]), nil, nil); err != nil {
				return err
			}
			fmt.Fprintf(out(cmd), "detached %s\n", args[0])
			return nil
		},
	}
	clientFlags(cmd, &f)
	return annotateEnv(cmd)
}

func newVastPoolsCmd() *cobra.Command {
	var f apiFlags
	cmd := &cobra.Command{
		Use:   "pools NAME",
		Short: "List a cluster's VIP pools and how their VIPs spread over CNodes",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			type pool struct {
				Name, Tenant, Role string
				Enabled            bool
				VIPs               int
				PerCNode           map[string]int `json:"per_cnode"`
			}
			return show(cmd, &f, "/api/v1/vast/"+url.PathEscape(args[0])+"/pools", "POOL\tTENANT\tROLE\tVIPS\tVIPS PER CNODE",
				func(v struct{ Pools []pool }) [][]string {
					var rows [][]string
					for _, p := range v.Pools {
						names := make([]string, 0, len(p.PerCNode))
						for n := range p.PerCNode {
							names = append(names, n)
						}
						sort.Strings(names)
						spread := make([]string, len(names))
						for i, n := range names {
							spread[i] = fmt.Sprintf("%s:%d", n, p.PerCNode[n])
						}
						name := p.Name
						if !p.Enabled {
							name += " (disabled)"
						}
						rows = append(rows, []string{name, p.Tenant, p.Role, fmt.Sprint(p.VIPs), strings.Join(spread, " ")})
					}
					return rows
				})
		},
	}
	clientFlags(cmd, &f)
	cmd.Flags().BoolVar(&f.json, "json", false, "print the API response as JSON")
	return annotateEnv(cmd)
}

func newTargetCmd() *cobra.Command {
	return group("target", "Define what Envoys balance across (cluster, VIP pool, port, TLS)",
		newTargetAddCmd(), newTargetListCmd(), newTargetRemoveCmd())
}

func newTargetAddCmd() *cobra.Command {
	var (
		f                        apiFlags
		cluster, pool, tenant    string
		port, listenPort         uint32
		useTLS, tlsInsecure      bool
		tlsServerName, tlsCAFile string
	)
	cmd := &cobra.Command{
		Use:   "add NAME",
		Short: "Add a target: one VIP pool of one cluster, on one loopback port of every client",
		Long: "Add a target. Every Envoy then listens on 127.0.0.1:LISTEN-PORT and balances requests\n" +
			"across the pool's VIPs that sit on healthy CNodes. S3 clients must use path-style\n" +
			"addressing against that address.\n\n" +
			"With --tls, Envoy connects to the VIPs over TLS and needs --tls-server-name: the name\n" +
			"to send as SNI and to find in the certificate. The loopback side stays plain HTTP.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cluster == "" || pool == "" || port == 0 || listenPort == 0 {
				return errors.New("--cluster, --pool, --port and --listen-port are required")
			}
			in := map[string]any{"name": args[0], "cluster": cluster, "pool": pool, "tenant": tenant, "port": port, "listen_port": listenPort}
			if useTLS {
				in["tls"] = map[string]any{"server_name": tlsServerName, "ca_file": tlsCAFile, "insecure": tlsInsecure}
			} else if tlsServerName != "" || tlsCAFile != "" || tlsInsecure {
				return errors.New("the --tls-* options need --tls")
			}
			var got targetRow
			if err := f.do(cmd, http.MethodPost, "/api/v1/targets", in, &got); err != nil {
				return err
			}
			fmt.Fprintf(out(cmd), "added %s: 127.0.0.1:%d on every client, %d endpoints (%s)\n", got.Name, got.ListenPort, len(got.Endpoints), got.status())
			return nil
		},
	}
	clientFlags(cmd, &f)
	fl := cmd.Flags()
	fl.StringVar(&cluster, "cluster", "", "attached VAST cluster (see \"vast list\")")
	fl.StringVar(&pool, "pool", "", "VIP pool to balance across (see \"vast pools\")")
	fl.StringVar(&tenant, "tenant", "", "the pool's tenant, needed only if two tenants have a pool of that name")
	fl.Uint32Var(&port, "port", 0, "S3 port on the VIPs, e.g. 80 or 443")
	fl.Uint32Var(&listenPort, "listen-port", 0, "loopback port Envoy listens on, on every client host")
	fl.BoolVar(&useTLS, "tls", false, "connect to the VIPs over TLS")
	fl.StringVar(&tlsServerName, "tls-server-name", "", "with --tls: name to send as SNI and to verify in the certificate")
	fl.StringVar(&tlsCAFile, "tls-ca-file", "", "with --tls: CA bundle path on the client hosts (default: the host's trust store)")
	fl.BoolVar(&tlsInsecure, "tls-insecure", false, "with --tls: do not verify the certificate (lab use)")
	return annotateEnv(cmd)
}

type targetRow struct {
	Name       string
	ListenPort uint32 `json:"listen_port"`
	TLS        bool
	Source     string
	Cluster    string
	Pool       string
	Endpoints  []struct{ IP string }
	Status     *struct {
		VIPs     int
		Healthy  int
		Drained  int
		Unusable int
		Problem  string
		Held     bool
		Limit    *limitStatus
		// Why rate rules cannot see a bucket named in a host name.
		BucketHostsProblem string `json:"bucket_hosts_problem"`
	}
	Over []struct {
		CNode    string
		InFlight uint64 `json:"in_flight"`
	}
}

// limit is the target list's LIMIT cell.
func (t targetRow) limit() string {
	if t.Status == nil || t.Status.Limit == nil {
		return "-"
	}
	l := t.Status.Limit
	cell := fmt.Sprintf("%d %s", l.PerCNode, l.Mode)
	if l.Mode == "enforce" {
		cell += fmt.Sprintf(" (%d per Envoy per VIP)", l.Share)
	}
	return cell
}

func (t targetRow) status() string {
	switch {
	case t.Status == nil:
		return "static"
	case t.Status.Problem != "" && t.Status.Held:
		return "serving the last known endpoints: " + t.Status.Problem
	case t.Status.Problem != "":
		return t.Status.Problem
	case t.Status.Held:
		return "serving the last known endpoints: the pool resolved to none"
	}
	s := fmt.Sprintf("%d of %d VIPs on healthy CNodes", t.Status.Healthy, t.Status.VIPs)
	if t.Status.Drained > 0 {
		s += fmt.Sprintf(", %d withheld by drain", t.Status.Drained)
	}
	if t.Status.Unusable > 0 {
		s += fmt.Sprintf(", %d left out: VAST gave no usable address", t.Status.Unusable)
	}
	if t.Status.BucketHostsProblem != "" {
		s += "; rate rules cannot see a bucket named in a host name: " + t.Status.BucketHostsProblem
	}
	for _, o := range t.Over {
		s += fmt.Sprintf("; OVER THE LIMIT on %s (%d in flight)", o.CNode, o.InFlight)
	}
	return s
}

func newTargetListCmd() *cobra.Command {
	var f apiFlags
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List targets",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return show(cmd, &f, "/api/v1/targets", "NAME\tLISTEN\tSOURCE\tCLUSTER\tPOOL\tTLS\tENDPOINTS\tLIMIT\tSTATUS",
				func(v struct{ Targets []targetRow }) [][]string {
					var rows [][]string
					for _, t := range v.Targets {
						tls := "no"
						if t.TLS {
							tls = "yes"
						}
						rows = append(rows, []string{t.Name, fmt.Sprintf("127.0.0.1:%d", t.ListenPort), t.Source, t.Cluster, t.Pool, tls, fmt.Sprint(len(t.Endpoints)), t.limit(), t.status()})
					}
					return rows
				})
		},
	}
	clientFlags(cmd, &f)
	cmd.Flags().BoolVar(&f.json, "json", false, "print the API response as JSON")
	return annotateEnv(cmd)
}

func newTargetRemoveCmd() *cobra.Command {
	var f apiFlags
	cmd := &cobra.Command{
		Use:   "remove NAME",
		Short: "Remove a target; its listener disappears from every client",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := f.do(cmd, http.MethodDelete, "/api/v1/targets/"+url.PathEscape(args[0]), nil, nil); err != nil {
				return err
			}
			fmt.Fprintf(out(cmd), "removed %s\n", args[0])
			return nil
		},
	}
	clientFlags(cmd, &f)
	return annotateEnv(cmd)
}
