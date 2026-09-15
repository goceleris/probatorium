package validation

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/probatorium/validation/remote"
)

// TestCheckptrCrashIsDetectedEndToEnd is the detection half of I-CHECKPTR
// proven on a real process, not a synthetic line.
//
// It compiles testdata/checkptrdemo with -d=checkptr=1 at test time -- the
// program performs a misaligned unsafe.Pointer conversion after printing the
// ready banner -- launches it through the same remote.Local + superviseStderr
// path a refapp takes, and asserts the liveness scan both caught the crash
// and attributed it to the pointer checker. This is the path the property
// loop reads from at cell end; if it does not work here it works nowhere.
func TestCheckptrCrashIsDetectedEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
	src, err := filepath.Abs("testdata/checkptrdemo")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "checkptrdemo")
	build := exec.Command("go", "build", "-gcflags=all=-d=checkptr=1", "-o", bin, ".")
	build.Dir = src
	build.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOOS="+runtime.GOOS, "GOARCH="+runtime.GOARCH)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build checkptrdemo: %v\n%s", err, out)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	proc, err := remote.NewLocal(bin).Start(ctx, nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	l := &livenessTally{}
	scanned := make(chan struct{})
	go func() {
		defer close(scanned)
		superviseStderr(proc.Stderr(), l,
			func(string) {}, // ready
			func(error) {},  // ready-fail
			func() {},       // crash
		)
	}()
	_, _ = proc.Wait(ctx)
	select {
	case <-scanned:
	case <-time.After(10 * time.Second):
		t.Fatal("stderr supervisor never reached EOF")
	}

	s := l.snapshot()
	if !s.Crashed {
		t.Fatal("the checkptr throw was not recorded as a crash")
	}
	if !strings.Contains(s.Signature, "checkptr") {
		t.Fatalf("crash signature does not name the pointer checker: %q", s.Signature)
	}
	if s.CheckptrReports != 1 {
		t.Fatalf("checkptr_reports = %d, want 1 (signature was %q)", s.CheckptrReports, s.Signature)
	}
}
