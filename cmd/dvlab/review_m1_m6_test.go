package main

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
)

// reviewClosedPort returns a loopback address with nothing listening on it.
func reviewClosedPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func reviewQuietStdout(t *testing.T) {
	t.Helper()
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = devnull
	t.Cleanup(func() { os.Stdout = old; devnull.Close() })
}

// TestReviewM1_CheckFailsWhenEveryDialFails: --check promises exit status 1
// when a check fails; a run in which no response was checked at all (every
// request failed to dial) must not report success.
func TestReviewM1_CheckFailsWhenEveryDialFails(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	reviewQuietStdout(t)
	cmd := loadgenCmd()
	cmd.SetArgs([]string{"--check", "--endpoint", "http://" + reviewClosedPort(t), "--bucket", "b", "--requests", "1", "--concurrency", "1", "--json"})
	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Errorf("loadgen --check with every request failing to dial returned success (exit status 0)")
	}
}

// TestReviewM6_NegativeSizeIsRejected: a negative --size must be refused
// with an error, not reach make([]byte, n).
func TestReviewM6_NegativeSizeIsRejected(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	reviewQuietStdout(t)
	cmd := loadgenCmd()
	cmd.SetArgs([]string{"--endpoint", "http://" + reviewClosedPort(t), "--bucket", "b", "--requests", "1", "--size", "-1"})
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("--size -1 panicked: %v", r)
			}
		}()
		err = cmd.ExecuteContext(context.Background())
	}()
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "size") {
		t.Errorf("--size -1: err = %v, want an error naming the size", err)
	}
}
