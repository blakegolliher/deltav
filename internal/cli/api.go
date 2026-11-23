package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// Client commands talk to a running daemon over its HTTP API and have no
// other way to reach its state.

const defaultAPI = "http://127.0.0.1:18080"

type apiFlags struct {
	base  string
	token string
	json  bool
}

// clientFlags adds the flags every API client command takes.
func clientFlags(cmd *cobra.Command, f *apiFlags) {
	cmd.Flags().StringVar(&f.base, "api", defaultAPI, "base URL of the daemon's HTTP API")
	cmd.Flags().StringVar(&f.token, "api-token", "", "bearer token for the API, if the daemon requires one (prefer the env var)")
}

// do sends one request. in is sent as JSON if not nil; a 2xx response is
// decoded into out if not nil. Any other status becomes an error carrying
// the daemon's message.
func (f *apiFlags) do(cmd *cobra.Command, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(cmd.Context(), method, strings.TrimSuffix(f.base, "/")+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if f.token != "" {
		req.Header.Set("Authorization", "Bearer "+f.token)
	}
	// Redirects are not followed: one would carry the token and the body
	// (a VAST password, say) elsewhere, and a 301-303 turns a change into
	// a GET whose success would be reported as the change's.
	hc := &http.Client{
		Timeout:       2 * time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach the daemon at %s: %w (is \"%s serve\" running? see --api)", f.base, err, cmd.Root().Name())
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode <= 399 {
		return fmt.Errorf("%s %s: status %d, a redirect to %q, which is not followed: point --api at the daemon itself", method, path, resp.StatusCode, resp.Header.Get("Location"))
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		if resp.StatusCode == http.StatusUnauthorized {
			return errors.New("the daemon requires a token: set DELTAV_API_TOKEN or --api-token")
		}
		return fmt.Errorf("%s %s: status %d", method, path, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	if raw, ok := out.(*json.RawMessage); ok {
		*raw = data
		return nil
	}
	return json.Unmarshal(data, out)
}

// show prints a list command's result: the raw JSON with --json, otherwise
// a table built by rows from the decoded value.
func show[T any](cmd *cobra.Command, f *apiFlags, path string, header string, rows func(T) [][]string) error {
	var raw json.RawMessage
	if err := f.do(cmd, http.MethodGet, path, nil, &raw); err != nil {
		return err
	}
	if f.json {
		_, err := out(cmd).Write(raw)
		return err
	}
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(out(cmd), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, header)
	for _, r := range rows(v) {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	return tw.Flush()
}

// ago renders a time as a short age, such as "12s ago".
func ago(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "never"
	}
	d := time.Since(*t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh ago", int(d.Hours()))
}
