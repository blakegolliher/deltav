package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

func run(t *testing.T, name string, args ...string) (string, error) {
	t.Helper()
	root := NewRoot(name)
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(args)
	err := root.Execute()
	return buf.String(), err
}

func TestHelpPrintsInvokedName(t *testing.T) {
	for _, name := range []string{"deltav", "dv"} {
		other := map[string]string{"deltav": "dv", "dv": "deltav"}[name]
		for _, args := range [][]string{{"--help"}, {"vast", "--help"}, {"serve", "--help"}} {
			got, err := run(t, name, args...)
			if err != nil {
				t.Fatalf("%s %v: %v", name, args, err)
			}
			if !strings.Contains(got, name+" ") {
				t.Errorf("%s %v: help does not mention %q:\n%s", name, args, name, got)
			}
			if strings.Contains(got, other+" ") {
				t.Errorf("%s %v: help mentions the other name %q:\n%s", name, args, other, got)
			}
		}
	}
}

func TestInvokedName(t *testing.T) {
	cases := map[string]string{"/usr/local/bin/dv": "dv", "./deltav": "deltav", "deltav": "deltav", "": "deltav"}
	for in, want := range cases {
		if got := invokedName([]string{in}); got != want {
			t.Errorf("invokedName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := invokedName(nil); got != "deltav" {
		t.Errorf("invokedName(nil) = %q", got)
	}
}

func TestApplyEnv(t *testing.T) {
	fs := pflag.NewFlagSet("t", pflag.ContinueOnError)
	addr := fs.String("http-addr", "127.0.0.1:1", "")
	grpcAddr := fs.String("grpc-addr", "0.0.0.0:1", "")
	n := fs.Int("count", 1, "")
	if err := fs.Parse([]string{"--grpc-addr", "flag:2"}); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"DELTAV_HTTP_ADDR": "env:3",
		"DELTAV_GRPC_ADDR": "env:4", // ignored: the flag was passed explicitly
		"DELTAV_COUNT":     "7",
	}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	if err := applyEnv(fs, lookup); err != nil {
		t.Fatal(err)
	}
	if *addr != "env:3" || *grpcAddr != "flag:2" || *n != 7 {
		t.Fatalf("got http=%q grpc=%q count=%d", *addr, *grpcAddr, *n)
	}

	env["DELTAV_COUNT"] = "many"
	fs2 := pflag.NewFlagSet("t", pflag.ContinueOnError)
	fs2.Int("count", 1, "")
	if err := applyEnv(fs2, lookup); err == nil || !strings.Contains(err.Error(), "DELTAV_COUNT") {
		t.Fatalf("want error naming DELTAV_COUNT, got %v", err)
	}
}

func TestServeHelpDocumentsEnv(t *testing.T) {
	got, err := run(t, "deltav", "serve", "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"DELTAV_GRPC_ADDR", "DELTAV_HTTP_ADDR", "DELTAV_API_TOKEN"} {
		if !strings.Contains(got, k) {
			t.Errorf("serve --help does not mention %s", k)
		}
	}
}
