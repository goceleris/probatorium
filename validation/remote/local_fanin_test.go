package remote

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// buildSpliceDemo compiles testdata/splicedemo for this host.
func buildSpliceDemo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
	src, err := filepath.Abs("testdata/splicedemo")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "splicedemo")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = src
	build.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOOS="+runtime.GOOS, "GOARCH="+runtime.GOARCH)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build splicedemo: %v\n%s", err, out)
	}
	return bin
}

// runSpliceDemo starts the demo through the driver and returns every line the
// merged reader produced. The scanner is configured exactly like the one in
// validation.superviseStderr -- the real consumer of this reader -- so a line
// this fan-in emits that the supervisor could not scan fails here too.
func runSpliceDemo(t *testing.T, bin string, args ...string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	proc, err := NewLocal(bin).Start(ctx, args)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	type scanResult struct {
		lines []string
		err   error
	}
	done := make(chan scanResult, 1)
	go func() {
		sc := bufio.NewScanner(proc.Stderr())
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		var lines []string
		for sc.Scan() {
			lines = append(lines, sc.Text())
		}
		done <- scanResult{lines, sc.Err()}
	}()
	if _, err := proc.Wait(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("the merged reader could not be scanned by the supervisor's scanner: %v", r.err)
		}
		return r.lines
	case <-time.After(pipeOrphanGrace + 10*time.Second):
		t.Fatal("merged reader never reached EOF")
		return nil
	}
}

// TestLocal_FanInNeverSplicesOneStreamIntoAnothersLine is the control for
// probatorium#382. The driver merges the child's stdout and stderr into one
// reader on purpose -- many candidates dump panic traces to stdout -- but the
// merge must be line-atomic. A byte-oriented merge can cut a line in half and
// staple the other stream's text onto it, which is how "fatal error: " met
// stdout's "before" and produced the plausible-looking "fatal error: before":
// the crash signature was recorded, it just named nothing real.
//
// The demo writes fixed-shape tagged lines from many goroutines on both
// streams, each line in several writes, the way the runtime writes a fatal
// error. Interleaving BETWEEN lines is expected and fine; a fragment of one
// stream inside the other's line is the defect.
func TestLocal_FanInNeverSplicesOneStreamIntoAnothersLine(t *testing.T) {
	const workers, perWorker, gapUS = 8, 15, 300
	lines := runSpliceDemo(t, buildSpliceDemo(t), "splice",
		fmt.Sprint(workers), fmt.Sprint(perWorker), fmt.Sprint(gapUS))

	outRe := regexp.MustCompile(`^OUT-\d{2}-\d{4}-o{24}$`)
	errRe := regexp.MustCompile(`^ERR-\d{2}-\d{4}-e{24}$`)
	var spliced []string
	outs, errs := 0, 0
	for _, line := range lines {
		switch {
		case outRe.MatchString(line):
			outs++
		case errRe.MatchString(line):
			errs++
		default:
			spliced = append(spliced, line)
		}
	}
	if len(spliced) > 0 {
		t.Errorf("%d of %d merged lines carry a fragment of the other stream; first few:\n  %s",
			len(spliced), len(lines), strings.Join(firstFew(spliced), "\n  "))
	}
	if want := workers * perWorker; outs != want || errs != want {
		t.Errorf("whole lines: stdout %d, stderr %d; want %d each", outs, errs, want)
	}
}

// TestLocal_FanInKeepsLongLinesAndAFinalUnterminatedLine is the control for
// the two decisions a line-atomic merge forces.
//
// A line that fits the fan-in's budget must arrive whole, even while the
// other stream chatters through every gap in it. A line that does NOT fit is
// split across consecutive output lines and every byte kept: a stack frame
// can be long, and truncating a crash line is the same loss probatorium#382
// is about, wearing a tidier disguise. Each piece also has to stay inside the
// 1 MiB token budget the supervisor's scanner is given, or the scan stops
// there and the rest of the capture is gone.
//
// And a process that dies mid-write leaves a last line with no newline --
// often the interesting one -- so it must arrive as a line of its own rather
// than being held until EOF discards it or the other stream lands on it.
func TestLocal_FanInKeepsLongLinesAndAFinalUnterminatedLine(t *testing.T) {
	// One line just inside the budget, one well past it, and enough stderr
	// noise to cover the gaps in both.
	const fits = fanInMaxLine / 2
	const splits = fanInMaxLine + 40000
	const noiseLines, gapUS = 120, 300
	wantPieces := (splits + fanInMaxLine - 1) / fanInMaxLine
	lines := runSpliceDemo(t, buildSpliceDemo(t), "edges",
		fmt.Sprint(fits), fmt.Sprint(splits), fmt.Sprint(noiseLines), fmt.Sprint(gapUS))

	noiseRe := regexp.MustCompile(`^ERR-noise-\d{4}$`)
	fitsRe := regexp.MustCompile(`^L+$`)
	splitsRe := regexp.MustCompile(`^X+$`)
	var fitsPieces, splitsPieces []int
	noise, tail := 0, 0
	var foreign []string
	for _, line := range lines {
		if len(line) > 1024*1024 {
			t.Errorf("a %d-byte line exceeds the 1 MiB token budget the supervisor's scanner is given", len(line))
		}
		switch {
		case fitsRe.MatchString(line):
			fitsPieces = append(fitsPieces, len(line))
		case splitsRe.MatchString(line):
			splitsPieces = append(splitsPieces, len(line))
		case noiseRe.MatchString(line):
			noise++
		case line == "tail-without-newline":
			tail++
		default:
			foreign = append(foreign, line)
		}
	}
	if len(foreign) > 0 {
		t.Errorf("%d lines belong to neither stream cleanly; first few:\n  %s",
			len(foreign), strings.Join(firstFew(foreign), "\n  "))
	}
	if len(fitsPieces) != 1 || fitsPieces[0] != fits {
		t.Errorf("a %d-byte line fits the %d-byte budget and must arrive whole; got pieces %v",
			fits, fanInMaxLine, fitsPieces)
	}
	if total := sum(splitsPieces); len(splitsPieces) != wantPieces || total != splits {
		t.Errorf("a %d-byte line must be split into %d pieces with every byte kept; got %d pieces totalling %d %v",
			splits, wantPieces, len(splitsPieces), total, splitsPieces)
	}
	if noise != noiseLines {
		t.Errorf("stderr noise lines: got %d, want %d", noise, noiseLines)
	}
	if tail != 1 {
		t.Errorf("the final newline-less line arrived %d times, want exactly 1", tail)
	}
}

func sum(xs []int) int {
	t := 0
	for _, x := range xs {
		t += x
	}
	return t
}

// firstFew renders at most three lines for a failure message, collapsing runs
// of repeated filler so a splice buried in the middle of a long line is
// visible rather than scrolled off.
func firstFew(lines []string) []string {
	out := make([]string, 0, 3)
	for _, line := range lines[:min(3, len(lines))] {
		out = append(out, summarize(line))
	}
	if len(lines) > 3 {
		out = append(out, fmt.Sprintf("... and %d more", len(lines)-3))
	}
	return out
}

// summarize collapses any run of more than eight identical bytes to <byte>xN.
func summarize(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		j := i
		for j < len(s) && s[j] == s[i] {
			j++
		}
		if n := j - i; n > 8 {
			fmt.Fprintf(&b, "%cx%d", s[i], n)
		} else {
			b.WriteString(s[i:j])
		}
		i = j
	}
	out := b.String()
	if len(out) > 240 {
		out = out[:240] + "..."
	}
	return fmt.Sprintf("(%d bytes) %q", len(s), out)
}
