package remote

import (
	"context"
	"io"
	"strconv"
	"strings"
	"testing"
)

// The prefix command runs first with the binary and its args after it, and
// the driver it came from is not changed (arm A confines the refapp with
// taskset; a prefix on one cell must not leak into the next).
func TestLocal_WithPrefixRunsTheBinaryBehindTheCommand(t *testing.T) {
	base := NewLocal("/bin/sh")
	// env execs its argument, like taskset: sh -c 'echo "$ARM:$1"' _ x
	d := base.WithPrefix("/usr/bin/env", "ARM=cpus-2-3")
	proc, err := d.Start(context.Background(), []string{"-c", `echo "$ARM:$1"`, "_", "x"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	out, _ := io.ReadAll(proc.Stderr())
	if _, err := proc.Wait(context.Background()); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "cpus-2-3:x" {
		t.Errorf("output %q, want cpus-2-3:x (prefix first, then binary, then args)", got)
	}

	plain, err := base.Start(context.Background(), []string{"-c", `echo "[$ARM]"`})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	out, _ = io.ReadAll(plain.Stderr())
	_, _ = plain.Wait(context.Background())
	if got := strings.TrimSpace(string(out)); got != "[]" {
		t.Errorf("the base driver picked up the prefix: %q", got)
	}
}

func TestLocal_WithPrefixKeepsTheBinarysPID(t *testing.T) {
	// env execs /bin/sh, so the PID Start returns is the shell's own: the
	// shell reports it as $$.
	d := NewLocal("/bin/sh").WithPrefix("/usr/bin/env", "X=1")
	proc, err := d.Start(context.Background(), []string{"-c", `echo $$`})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	out, _ := io.ReadAll(proc.Stderr())
	_, _ = proc.Wait(context.Background())
	if got := strings.TrimSpace(string(out)); got == "" || got != strconv.Itoa(proc.PID()) {
		t.Errorf("script saw pid %q, driver says %d", got, proc.PID())
	}
}
