package remote

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// TestLocal_WaitDrainsPipesBeforeReap: bytes a process (or a child still
// holding its stderr) writes before the pipe reaches EOF must reach the
// Stderr() reader even when Wait is called concurrently. cmd.Wait closes the
// pipe read ends on reap and discards whatever is still buffered, so Wait
// must drain to EOF first. The crash banner a refapp prints just before
// exiting was lost to this race (probatorium#276).
func TestLocal_WaitDrainsPipesBeforeReap(t *testing.T) {
	l := NewLocal("/bin/sh")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, err := l.Start(ctx, []string{"-c", `( sleep 0.2; echo "fatal error: late banner" >&2 ) & exit 2`})
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan string, 1)
	go func() { b, _ := io.ReadAll(p.Stderr()); got <- string(b) }()
	res, err := p.Wait(ctx)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if res.ExitCode != 2 {
		t.Fatalf("exit code: %+v", res)
	}
	select {
	case out := <-got:
		if !strings.Contains(out, "late banner") {
			t.Fatalf("bytes written before EOF were lost to the reap: %q", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stderr reader never reached EOF")
	}
}
