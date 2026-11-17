package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestBinaryAndSymlink builds the real binary, installs "dv" as a symlink the
// way the Makefile does, and checks both names behave identically while
// printing the name they were invoked by.
func TestBinaryAndSymlink(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "deltav")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	if err := os.Symlink("deltav", filepath.Join(dir, "dv")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"deltav", "dv"} {
		out, err := exec.Command(filepath.Join(dir, name), "--help").CombinedOutput()
		if err != nil {
			t.Fatalf("%s --help: %v\n%s", name, err, out)
		}
		if !strings.Contains(string(out), name+" [command]") {
			t.Errorf("%s --help does not show %q:\n%s", name, name+" [command]", out)
		}
		out, err = exec.Command(filepath.Join(dir, name), "bootstrap").CombinedOutput()
		if err == nil || !strings.HasPrefix(string(out), name+": ") {
			t.Errorf("%s bootstrap: want a %q-prefixed error, got err=%v out=%q", name, name+": ", err, out)
		}
	}
}
