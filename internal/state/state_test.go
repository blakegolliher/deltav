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

func TestCache(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	if got, err := s.LoadCache(); err != nil || len(got.Endpoints) != 0 || len(got.Shares) != 0 {
		t.Fatalf("empty cache: %+v, %v", got, err)
	}
	want := Cache{
		Endpoints: map[string][]Endpoint{"main": {{IP: "192.0.2.1", Port: 80, CNode: "cnode-1"}, {IP: "192.0.2.2", Port: 80}}},
		Shares:    map[string]uint32{"main": 5},
	}
	if err := s.SaveCache(want); err != nil {
		t.Fatal(err)
	}
	got, err := open(t, dir).LoadCache()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Endpoints["main"]) != 2 || got.Endpoints["main"][0] != want.Endpoints["main"][0] || got.Endpoints["main"][1] != want.Endpoints["main"][1] || got.Shares["main"] != 5 {
		t.Errorf("cache after reopen: %+v", got)
	}
	// A damaged cache is only a cache: start without it, and say so.
	if err := os.WriteFile(filepath.Join(dir, "served.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := s.LoadCache(); err == nil || len(got.Endpoints) != 0 || got.Endpoints == nil || got.Shares == nil {
		t.Errorf("damaged cache: %+v, %v", got, err)
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
	if err := mem.AddCluster(lab); err != nil || len(mem.Clusters()) != 1 || mem.SaveCache(Cache{}) != nil {
		t.Errorf("in-memory store: %v", err)
	}
}

func TestLimitsAndDrains(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	is := func(err, want error, what string) {
		t.Helper()
		if !errors.Is(err, want) {
			t.Errorf("%s: got %v, want %v", what, err, want)
		}
	}
	is(s.AddCluster(lab), nil, "cluster")
	is(s.AddTarget(main, nil), nil, "target")

	is(s.SetTargetLimit("nope", &Limit{PerCNode: 10, Mode: ModeAlert}), ErrNotFound, "limit on an unknown target")
	is(s.SetTargetLimit("main", &Limit{PerCNode: 10, Mode: "strict"}), ErrInvalid, "unknown mode")
	before := s.Targets()
	is(s.SetTargetLimit("main", &Limit{PerCNode: 100, Mode: ModeEnforce}), nil, "setting a limit")
	if before[0].Limit != nil {
		t.Error("setting a limit changed a target list handed out earlier")
	}
	if got := s.Targets()[0].Limit; got == nil || *got != (Limit{PerCNode: 100, Mode: ModeEnforce}) {
		t.Errorf("limit = %+v", got)
	}

	is(s.SetDrained("nope", "cnode-1", true), ErrNotFound, "draining on an unknown cluster")
	is(s.SetDrained("lab", "cnode-1", false), ErrNotFound, "undraining a CNode that is not drained")
	is(s.SetDrained("lab", "cnode-2", true), nil, "draining")
	is(s.SetDrained("lab", "cnode-1", true), nil, "draining another")
	is(s.SetDrained("lab", "cnode-1", true), nil, "draining it again")
	if got := s.Drained(); len(got) != 2 || got[0] != (Drain{"lab", "cnode-1"}) || got[1] != (Drain{"lab", "cnode-2"}) {
		t.Errorf("drained = %+v", got)
	}

	// Both survive a restart.
	again := open(t, dir)
	if got := again.Targets()[0].Limit; got == nil || got.PerCNode != 100 || got.Mode != ModeEnforce {
		t.Errorf("limit after reopen = %+v", got)
	}
	if got := again.Drained(); len(got) != 2 {
		t.Errorf("drained after reopen = %+v", got)
	}

	is(again.SetDrained("lab", "cnode-2", false), nil, "undraining")
	is(again.SetTargetLimit("main", &Limit{PerCNode: 0, Mode: ModeAlert}), nil, "a limit of 0 removes it")
	if again.Targets()[0].Limit != nil || len(again.Drained()) != 1 {
		t.Errorf("after clearing: limit %+v, drained %+v", again.Targets()[0].Limit, again.Drained())
	}
	// Detaching a cluster forgets its drains.
	is(again.RemoveTarget("main"), nil, "removing the target")
	is(again.RemoveCluster("lab"), nil, "removing the cluster")
	if len(again.Drained()) != 0 {
		t.Errorf("drains left behind: %+v", again.Drained())
	}
}

// A limit means the same wherever it comes from: the limit call, a new
// target, or the state file.
func TestLimitsReadOneWay(t *testing.T) {
	s := open(t, "")
	if err := s.AddCluster(lab); err != nil {
		t.Fatal(err)
	}
	withLimit := func(name string, port uint32, l Limit) Target {
		tg := main
		tg.Name, tg.ListenPort, tg.Limit = name, port, &l
		return tg
	}
	limitOf := func(s *Store, name string) *Limit {
		for _, tg := range s.Targets() {
			if tg.Name == name {
				return tg.Limit
			}
		}
		t.Fatalf("no target %q", name)
		return nil
	}
	if err := s.AddTarget(withLimit("typo", 9001, Limit{PerCNode: 10, Mode: "enforced"}), nil); !errors.Is(err, ErrInvalid) {
		t.Errorf("new target with mode %q: %v", "enforced", err)
	}
	if err := s.AddTarget(withLimit("zero", 9002, Limit{PerCNode: 0, Mode: ModeEnforce}), nil); err != nil {
		t.Fatal(err)
	}
	if err := s.AddTarget(withLimit("nomode", 9003, Limit{PerCNode: 10}), nil); err != nil {
		t.Fatal(err)
	}
	if l := limitOf(s, "zero"); l != nil {
		t.Errorf("new target with a limit of 0: %+v", l)
	}
	if l := limitOf(s, "nomode"); l == nil || l.Mode != ModeAlert {
		t.Errorf("new target with a limit and no mode: %+v", l)
	}
	if err := s.SetTargetLimit("zero", &Limit{PerCNode: 5}); err != nil {
		t.Fatal(err)
	}
	if l := limitOf(s, "zero"); l == nil || l.Mode != ModeAlert {
		t.Errorf("limit set with no mode: %+v", l)
	}

	stored := func(limit string) (*Store, error) {
		dir := t.TempDir()
		b := `{"version":1,"clusters":[{"name":"lab","host":"h","user":"u","password":"p"}],
			"targets":[{"name":"main","cluster":"lab","pool":"main","port":80,"listen_port":9000,"limit":` + limit + `}]}`
		if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(b), 0o600); err != nil {
			t.Fatal(err)
		}
		return Open(dir)
	}
	if got, err := stored(`{"per_cnode":10}`); err != nil || limitOf(got, "main") == nil || limitOf(got, "main").Mode != ModeAlert {
		t.Errorf("stored limit with no mode: %v", err)
	}
	if got, err := stored(`{"per_cnode":0,"mode":"enforce"}`); err != nil || limitOf(got, "main") != nil {
		t.Errorf("stored limit of 0: %v", err)
	}
	if _, err := stored(`{"per_cnode":10,"mode":"enforced"}`); err == nil || !strings.Contains(err.Error(), `target "main"`) {
		t.Errorf("stored limit with mode %q: %v", "enforced", err)
	}
}
