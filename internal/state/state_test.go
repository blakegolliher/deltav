package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var (
	lab  = Cluster{Name: "lab", Host: "vast.example.com", User: "admin", Password: "s3cr3t-pw"}
	main = Target{Name: "main", Cluster: "lab", Pool: "main", Port: 80, ListenPort: 9000}
)

func open(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStatePersistsWithPrivatePermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "deltav")
	s := open(t, dir)
	if err := s.AddCluster(lab); err != nil {
		t.Fatal(err)
	}
	tls := Target{Name: "secure", Cluster: "lab", Pool: "main", Tenant: "default", Port: 443, ListenPort: 9443,
		TLS: &TLS{ServerName: "s3.example.com", CAFile: "/etc/deltav/ca.pem"}}
	for _, target := range []Target{tls, main} {
		if err := s.AddTarget(target, nil); err != nil {
			t.Fatal(err)
		}
	}

	// The directory and the file are private: the file holds a password.
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Errorf("state directory mode %o, want 700", st.Mode().Perm())
	}
	path := filepath.Join(dir, "state.json")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("state file mode %o, want 600", st.Mode().Perm())
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "s3cr3t-pw") {
		t.Error("the state file must hold the password: the daemon needs it after a restart")
	}
	// No temporary files are left behind.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("directory holds %d entries, want only state.json", len(entries))
	}

	// A new store over the same directory sees it all, sorted by name.
	again := open(t, dir)
	if c := again.Clusters(); len(c) != 1 || c[0] != lab {
		t.Errorf("clusters after reopen: %+v", c)
	}
	got := again.Targets()
	if len(got) != 2 || got[0].Name != "main" || got[1].Name != "secure" || got[1].TLS == nil || *got[1].TLS != *tls.TLS || got[1].Tenant != "default" {
		t.Errorf("targets after reopen: %+v", got)
	}
	if c, ok := again.Cluster("lab"); !ok || c.Password != "s3cr3t-pw" {
		t.Errorf("Cluster(lab) = %+v, %v", c, ok)
	}
}

func TestStateRules(t *testing.T) {
	s := open(t, t.TempDir())
	is := func(err, want error, what string) {
		t.Helper()
		if !errors.Is(err, want) {
			t.Errorf("%s: got %v, want %v", what, err, want)
		}
	}
	is(s.AddTarget(main, nil), ErrNotFound, "target for a cluster that does not exist")
	is(s.AddCluster(Cluster{Name: "bad name", Host: "h", User: "u", Password: "p"}), ErrInvalid, "cluster name with a space")
	is(s.AddCluster(Cluster{Name: "lab", Host: "h", User: "u"}), ErrInvalid, "cluster without a password")
	is(s.AddCluster(lab), nil, "first cluster")
	is(s.AddCluster(lab), ErrConflict, "same cluster again")

	is(s.AddTarget(main, map[uint32]string{9000: "static-s3"}), ErrConflict, "listen port taken by a static target")
	is(s.AddTarget(main, nil), nil, "first target")
	is(s.AddTarget(main, nil), ErrConflict, "same target again")
	other := main
	other.Name = "other"
	is(s.AddTarget(other, nil), ErrConflict, "second target on the same listen port")
	for what, mutate := range map[string]func(*Target){
		"no pool":             func(x *Target) { x.Pool = "" },
		"port 0":              func(x *Target) { x.Port = 0 },
		"listen port too big": func(x *Target) { x.ListenPort = 70000 },
		"TLS with no SNI":     func(x *Target) { x.TLS = &TLS{} },
		"bad name":            func(x *Target) { x.Name = "-x" },
	} {
		bad := other
		bad.ListenPort = 9001
		mutate(&bad)
		is(s.AddTarget(bad, nil), ErrInvalid, what)
	}

	is(s.RemoveCluster("lab"), ErrConflict, "removing a cluster a target uses")
	is(s.RemoveTarget("nope"), ErrNotFound, "removing an unknown target")
	is(s.RemoveTarget("main"), nil, "removing the target")
	is(s.RemoveCluster("nope"), ErrNotFound, "removing an unknown cluster")
	is(s.RemoveCluster("lab"), nil, "removing the cluster")
	if len(s.Clusters()) != 0 || len(s.Targets()) != 0 {
		t.Errorf("left over: %v %v", s.Clusters(), s.Targets())
	}
}

func TestEndpointCache(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	if got := s.LoadEndpoints(); len(got) != 0 {
		t.Fatalf("empty cache: %v", got)
	}
	want := map[string][]Endpoint{"main": {{IP: "192.0.2.1", Port: 80, CNode: "cnode-1"}, {IP: "192.0.2.2", Port: 80}}}
	if err := s.SaveEndpoints(want); err != nil {
		t.Fatal(err)
	}
	got := open(t, dir).LoadEndpoints()
	if len(got["main"]) != 2 || got["main"][0] != want["main"][0] || got["main"][1] != want["main"][1] {
		t.Errorf("cache after reopen: %v", got)
	}
	// A damaged cache is only a cache: ignore it.
	if err := os.WriteFile(filepath.Join(dir, "endpoints.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := s.LoadEndpoints(); len(got) != 0 {
		t.Errorf("damaged cache: %v", got)
	}
}

func TestOpenFailsClearly(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil || !strings.Contains(err.Error(), "state.json") {
		t.Errorf("damaged state file: %v", err)
	}
	// A path that cannot be a directory.
	if _, err := Open(filepath.Join(dir, "state.json", "sub")); err == nil || !strings.Contains(err.Error(), "--state-dir") {
		t.Errorf("unusable directory: %v", err)
	}
	// In-memory store: works, writes nothing.
	mem := open(t, "")
	if err := mem.AddCluster(lab); err != nil || len(mem.Clusters()) != 1 || mem.SaveEndpoints(nil) != nil {
		t.Errorf("in-memory store: %v", err)
	}
}
