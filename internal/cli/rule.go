package cli

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"deltav/internal/ratelimit"
)

// Request-rate rules. The commands are thin: the daemon checks a rule and
// says what it means.

func newRuleCmd() *cobra.Command {
	return group("rule", "Request-rate rules: limit S3 requests by operation, access key, bucket or prefix",
		newRuleListCmd(), newRuleShowCmd(), newRuleSetCmd(), newRuleRemoveCmd(), newRuleOperationsCmd())
}

// ruleStatus is a rule as the API returns it.
type ruleStatus struct {
	ratelimit.Rule
	Limit    string           `json:"limit"`
	Sentence string           `json:"sentence"`
	Status   ratelimit.Status `json:"status"`
}

type rulesList struct {
	Rules    []ruleStatus `json:"rules"`
	Overflow uint64       `json:"overflow"`
}

func some(values []string) string {
	switch {
	case len(values) == 0:
		return "-"
	case len(values) > 2:
		return fmt.Sprintf("%s, %s +%d", values[0], values[1], len(values)-2)
	}
	return strings.Join(values, ", ")
}

// onlyFor is a rule's access key, bucket and prefix filters in one cell.
func onlyFor(r ratelimit.Rule) string {
	var parts []string
	if len(r.AccessKey) > 0 {
		parts = append(parts, "key "+some(r.AccessKey))
	}
	if len(r.Bucket) > 0 {
		parts = append(parts, "bucket "+some(r.Bucket))
	}
	if len(r.Prefix) > 0 {
		parts = append(parts, "prefix "+some(r.Prefix))
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, "; ")
}

func perSecond(v float64) string {
	switch {
	case v == 0:
		return "0"
	case v < 10:
		return fmt.Sprintf("%.1f", v)
	}
	return fmt.Sprintf("%.0f", v)
}

func newRuleListCmd() *cobra.Command {
	var f apiFlags
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the rate rules, in the order they apply, with what Envoys last reported",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var sentences []string
			err := show(cmd, &f, "/api/v1/rules", "NAME\tLIMIT\tPER\tOPERATION\tONLY FOR\tRESPOND\tKEYS\tALLOWED/S\tREFUSED/S",
				func(v rulesList) [][]string {
					var rows [][]string
					for _, r := range v.Rules {
						per := strings.Join(r.Per, ", ")
						if per == "" {
							per = "-"
						}
						rows = append(rows, []string{r.Name, r.Limit, per, some(r.Operation), onlyFor(r.Rule), fmt.Sprint(r.Respond),
							fmt.Sprint(r.Status.Keys), perSecond(r.Status.AllowedRate), perSecond(r.Status.DeniedRate)})
						sentences = append(sentences, r.Name+": "+r.Sentence)
					}
					return rows
				})
			if err != nil || f.json {
				return err
			}
			if len(sentences) == 0 {
				fmt.Fprintln(out(cmd), "No rate rules. Add one with \"rule set\".")
				return nil
			}
			fmt.Fprintln(out(cmd))
			for _, s := range sentences {
				fmt.Fprintln(out(cmd), s)
			}
			return nil
		},
	}
	clientFlags(cmd, &f)
	cmd.Flags().BoolVar(&f.json, "json", false, "print the API response as JSON")
	return annotateEnv(cmd)
}

func newRuleShowCmd() *cobra.Command {
	var f apiFlags
	cmd := &cobra.Command{
		Use:   "show NAME",
		Short: "Show one rule: what it does, and the access keys, buckets or prefixes it is refusing most",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var v rulesList
			if err := f.do(cmd, http.MethodGet, "/api/v1/rules", nil, &v); err != nil {
				return err
			}
			for _, r := range v.Rules {
				if r.Name != args[0] {
					continue
				}
				w := out(cmd)
				fmt.Fprintf(w, "%s: %s\n", r.Name, r.Sentence)
				if r.Note != "" {
					fmt.Fprintf(w, "Note: %s\n", r.Note)
				}
				fmt.Fprintf(w, "Now: %s allowed and %s refused per second, on %d Envoys; %d allowed and %d refused since the daemon started.\n",
					perSecond(r.Status.AllowedRate), perSecond(r.Status.DeniedRate), r.Status.Envoys, r.Status.Totals.Allowed, r.Status.Totals.Denied)
				if len(r.Status.Top) == 0 {
					fmt.Fprintln(w, "Nothing has matched it in the last few seconds.")
					return nil
				}
				fmt.Fprintln(w)
				tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "COUNTED UNDER\tALLOWED/S\tREFUSED/S\tENVOYS")
				for _, k := range r.Status.Top {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%d\n", k.Key, perSecond(k.AllowedRate), perSecond(k.DeniedRate), k.Envoys)
				}
				return tw.Flush()
			}
			return fmt.Errorf("no rule %q", args[0])
		},
	}
	clientFlags(cmd, &f)
	return annotateEnv(cmd)
}

func newRuleSetCmd() *cobra.Command {
	var (
		f                          apiFlags
		r                          ratelimit.Rule
		perSecond, perMinute, hour float64
	)
	cmd := &cobra.Command{
		Use:     "set NAME",
		Aliases: []string{"add"},
		Short:   "Add a rate rule, or replace the one of that name",
		Long: "Add a rate rule, or replace the one of that name.\n\n" +
			"A rule limits the S3 requests it matches. The filters are optional, and a request\n" +
			"must pass all that are given: --operation (S3 operation names such as PutObject, or\n" +
			"a family such as 'List*'; see \"rule operations\"), --access-key, --bucket, --prefix.\n" +
			"A prefix is compared with the object key, or with a listing's prefix parameter.\n\n" +
			"The limit is one of --per-second, --per-minute, --per-hour, or --blocked to refuse\n" +
			"everything that matches. --per says what gets a count of its own:\n\n" +
			"  total       one count for everything the rule matches\n" +
			"  access_key  a count for each access key        bucket  a count for each bucket\n" +
			"  prefix      a count for each listed prefix     client  a count for each client host\n\n" +
			"The rate belongs to the whole fleet: the daemon divides it among the Envoys that are\n" +
			"using a count, by what each asks for, and follows changes within about 5 seconds.\n" +
			"With client in --per, every client host has the whole rate to itself instead.\n\n" +
			"A request over the limit is answered 503 SlowDown, which is what S3 itself sends and\n" +
			"what S3 SDKs retry. The AWS CLI retries it only twice by default: for a low limit set\n" +
			"AWS_RETRY_MODE=adaptive on the client, or give the rule a --burst as large as the\n" +
			"client's concurrency. --respond 429 answers 429 instead, which the AWS CLI does not\n" +
			"retry at all. A request that matches several rules must be allowed by each. If the\n" +
			"daemon is unreachable for more than ten minutes, rules stop being enforced until it\n" +
			"is back.\n\n" +
			"Examples:\n\n" +
			"  deltav rule set uploads --operation PutObject,UploadPart --per-second 2000 --per access_key\n" +
			"  deltav rule set listings --operation 'List*' --per-minute 600 --per total\n" +
			"  deltav rule set no-deletes --operation DeleteObject,DeleteObjects --bucket archive --blocked",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r.Name = args[0]
			r.LimitPerSecond, r.LimitPerMinute, r.LimitPerHour = perSecond, perMinute, hour
			if !r.Blocked && perSecond == 0 && perMinute == 0 && hour == 0 {
				return errors.New("give a limit: --per-second, --per-minute or --per-hour, or --blocked")
			}
			var got ruleStatus
			if err := f.do(cmd, http.MethodPut, "/api/v1/rules/"+url.PathEscape(r.Name), r, &got); err != nil {
				return err
			}
			fmt.Fprintf(out(cmd), "%s: %s\n", got.Name, got.Sentence)
			return nil
		},
	}
	clientFlags(cmd, &f)
	fl := cmd.Flags()
	fl.StringSliceVar(&r.Operation, "operation", nil, "S3 operations, comma-separated; a trailing * names a family (default: every operation)")
	fl.StringSliceVar(&r.AccessKey, "access-key", nil, "access key IDs, comma-separated; - is a request that carries none (default: any)")
	fl.StringSliceVar(&r.Bucket, "bucket", nil, "buckets, comma-separated (default: any)")
	fl.StringArrayVar(&r.Prefix, "prefix", nil, "a key prefix; repeat the flag for several (default: any)")
	fl.Float64Var(&perSecond, "per-second", 0, "requests per second")
	fl.Float64Var(&perMinute, "per-minute", 0, "requests per minute")
	fl.Float64Var(&hour, "per-hour", 0, "requests per hour")
	fl.BoolVar(&r.Blocked, "blocked", false, "refuse every matching request")
	fl.Uint32Var(&r.Burst, "burst", 0, "how many may pass at once after a quiet spell (default: the limit's own number)")
	fl.StringSliceVar(&r.Per, "per", nil, "what gets a count of its own: total, or any of access_key, bucket, prefix, client")
	fl.IntVar(&r.Respond, "respond", 0, "status for a refused request: 503 (SlowDown, the default) or 429")
	fl.StringVar(&r.Note, "note", "", "a line to remember the rule by")
	return annotateEnv(cmd)
}

func newRuleRemoveCmd() *cobra.Command {
	var f apiFlags
	cmd := &cobra.Command{
		Use:   "remove NAME",
		Short: "Remove a rate rule",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := f.do(cmd, http.MethodDelete, "/api/v1/rules/"+url.PathEscape(args[0]), nil, nil); err != nil {
				return err
			}
			fmt.Fprintf(out(cmd), "removed rule %s\n", args[0])
			return nil
		},
	}
	clientFlags(cmd, &f)
	return annotateEnv(cmd)
}

func newRuleOperationsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "operations",
		Short: "List the S3 operation names a rule can use",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			for _, op := range ratelimit.Operations() {
				fmt.Fprintln(out(cmd), op)
			}
			return nil
		},
	}
}
