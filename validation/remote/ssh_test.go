package remote

import (
	"context"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// Integration tests in this file require a live SSH endpoint with
// ssh-agent auth. They're skipped by default; opt-in by setting
// PROBATORIUM_SSH_TEST_HOST (and optionally PROBATORIUM_SSH_TEST_USER
// — defaults to current $USER).
//
// On the cluster runner the canonical invocation is:
//   PROBATORIUM_SSH_TEST_USER=mini \
//     PROBATORIUM_SSH_TEST_HOST=192.168.50.65 \
//     go test -v -run SSH ./validation/remote
//
// Skipping (default) means we still exercise the unit-only paths
// (shellQuote, sshSignalCode) below.

func testSSHHost(t *testing.T) (user, host string) {
	t.Helper()
	host = os.Getenv("PROBATORIUM_SSH_TEST_HOST")
	if host == "" {
		t.Skip("set PROBATORIUM_SSH_TEST_HOST=<addr> to enable SSH integration tests")
	}
	user = os.Getenv("PROBATORIUM_SSH_TEST_USER")
	if user == "" {
		user = os.Getenv("USER")
	}
	if user == "" {
		t.Skip("no SSH user (set PROBATORIUM_SSH_TEST_USER or have $USER set)")
	}
	if os.Getenv("SSH_AUTH_SOCK") == "" {
		t.Skip("no SSH_AUTH_SOCK — SSH driver needs ssh-agent")
	}
	return user, host
}

func TestSSH_StartAndCleanExit(t *testing.T) {
	user, host := testSSHHost(t)
	d := NewSSH(user, host, "/usr/bin/true", SSHConfig{})
	defer func() { _ = d.Close() }()
	proc, err := d.Start(context.Background(), nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	res, err := proc.Wait(context.Background())
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode: got %d, want 0", res.ExitCode)
	}
}

func TestSSH_NonZeroExitCode(t *testing.T) {
	user, host := testSSHHost(t)
	d := NewSSH(user, host, "/usr/bin/false", SSHConfig{})
	defer func() { _ = d.Close() }()
	proc, err := d.Start(context.Background(), nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	res, err := proc.Wait(context.Background())
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if res.ExitCode == 0 {
		t.Errorf("expected non-zero exit, got 0")
	}
}

func TestSSH_StderrIsCaptured(t *testing.T) {
	user, host := testSSHHost(t)
	d := NewSSH(user, host, "/bin/sh", SSHConfig{})
	defer func() { _ = d.Close() }()
	proc, err := d.Start(context.Background(),
		[]string{"-c", "echo to-stdout; echo to-stderr >&2"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	buf, err := io.ReadAll(proc.Stderr())
	if err != nil {
		t.Fatalf("read stderr: %v", err)
	}
	if _, err := proc.Wait(context.Background()); err != nil {
		t.Fatalf("wait: %v", err)
	}
	body := string(buf)
	if !strings.Contains(body, "to-stdout") {
		t.Errorf("stdout not captured: %q", body)
	}
	if !strings.Contains(body, "to-stderr") {
		t.Errorf("stderr not captured: %q", body)
	}
}

func TestSSH_PIDIsRemote(t *testing.T) {
	user, host := testSSHHost(t)
	d := NewSSH(user, host, "/bin/sh", SSHConfig{})
	defer func() { _ = d.Close() }()
	proc, err := d.Start(context.Background(), []string{"-c", "sleep 5"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := proc.PID()
	if pid <= 0 {
		t.Errorf("PID: got %d, want > 0", pid)
	}
	// Hard-stop so we don't wait 5s. SIGKILL via the pkill-tree
	// path may return non-zero rc if pkill found no children; the
	// driver collapses that into nil intentionally, but bench it
	// once more to be sure under race detection.
	_ = proc.Signal(9)
	// Allow up to 5s for the SIGKILL to round-trip.
	doneCh := make(chan struct{})
	go func() { _, _ = proc.Wait(context.Background()); close(doneCh) }()
	select {
	case <-doneCh:
	case <-time.After(5 * time.Second):
		t.Error("SIGKILL didn't reap process within 5s")
	}
}

func TestSSH_SignalTermStopsLongRunner(t *testing.T) {
	user, host := testSSHHost(t)
	d := NewSSH(user, host, "/bin/sh", SSHConfig{})
	defer func() { _ = d.Close() }()
	proc, err := d.Start(context.Background(), []string{"-c", "sleep 30"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	time.Sleep(100 * time.Millisecond) // let the sleep actually start
	if err := proc.Signal(15); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	doneCh := make(chan WaitResult, 1)
	go func() {
		r, _ := proc.Wait(context.Background())
		doneCh <- r
	}()
	// 8s deadline: SIGTERM round-trip + ssh.Session.Wait notice
	// fluctuates between 1-3s on the LAN cluster (separate session
	// for the kill, separate session for the original command's
	// exit). Generous bound keeps the test stable under load.
	select {
	case <-doneCh:
	case <-time.After(8 * time.Second):
		t.Fatal("process did not exit within 8s of SIGTERM")
	}
}

func TestSSH_CloseIsIdempotent(t *testing.T) {
	user, host := testSSHHost(t)
	d := NewSSH(user, host, "/usr/bin/true", SSHConfig{})
	if err := d.Close(); err != nil {
		t.Errorf("Close 1: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Errorf("Close 2: %v", err)
	}
}

// Unit tests for the helpers — run unconditionally.

func TestShellQuote(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "''"},
		{"foo", "'foo'"},
		{"foo bar", "'foo bar'"},
		{`foo'bar`, `'foo'\''bar'`},
		{`a"b`, `'a"b'`},
	}
	for _, tc := range cases {
		if got := shellQuote(tc.in); got != tc.want {
			t.Errorf("shellQuote(%q): got %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestQuoteAll(t *testing.T) {
	got := quoteAll([]string{"foo", "bar baz", `qu'ux`})
	want := []string{"'foo'", "'bar baz'", `'qu'\''ux'`}
	if len(got) != len(want) {
		t.Fatalf("len: got %d, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSSHSignalCode(t *testing.T) {
	cases := map[string]int{
		"HUP":     1,
		"INT":     2,
		"KILL":    9,
		"TERM":    15,
		"unknown": 0,
	}
	for name, want := range cases {
		if got := sshSignalCode(name); got != want {
			t.Errorf("sshSignalCode(%q): got %d, want %d", name, got, want)
		}
	}
}

// silenceUnusedSSHImports keeps the net import live in test contexts
// where the integration tests skip (avoiding "imported and not used").
var _ = net.JoinHostPort
