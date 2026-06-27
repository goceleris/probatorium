package validation

import (
	"strings"
	"testing"
)

// A refapp that comes up, then dies with a clean exit(1) (its own log.Fatalf)
// prints no Go runtime crash marker, so the signature scan keeps nothing. The
// rolling tail must still capture the final output that names the cause —
// otherwise the incident records only "process exited unexpectedly (code=1)"
// and the real reason is lost (the gap that left the std-engine I-LIVENESS
// undiagnosable).
func TestSuperviseStderr_TailCapturedOnCleanExit(t *testing.T) {
	in := strings.NewReader("ready addr=127.0.0.1:8080\n" +
		"serving requests\n" +
		"auth_session_ratelimit: start: accept tcp 127.0.0.1:8080: bad file descriptor\n")
	l := &livenessTally{}
	var readyCalled, crashCalled bool

	superviseStderr(in, l,
		func() { readyCalled = true },
		func(error) {},
		func() { crashCalled = true },
	)

	if !readyCalled {
		t.Fatal("onReady should have fired (the ready banner was present)")
	}
	if crashCalled {
		t.Fatal("onCrash must NOT fire — a clean exit has no runtime crash signature")
	}
	snap := l.snapshot()
	if snap.Signature != "" {
		t.Fatalf("expected no crash signature, got %q", snap.Signature)
	}
	if !strings.Contains(snap.Trace, "start: accept tcp 127.0.0.1:8080: bad file descriptor") {
		t.Fatalf("tail must capture the refapp's final fatal line; got trace:\n%s", snap.Trace)
	}
}

// A crash WITH a recognised signature must still keep its signature-anchored
// trace (the tail logic must not override the existing crash-trace path).
func TestSuperviseStderr_SignatureTracePreserved(t *testing.T) {
	in := strings.NewReader("ready addr=127.0.0.1:8080\n" +
		"fatal error: concurrent map writes\n" +
		"goroutine 1 [running]:\n")
	l := &livenessTally{}
	superviseStderr(in, l, func() {}, func(error) {}, func() {})

	snap := l.snapshot()
	if snap.Signature == "" {
		t.Fatal("expected the fatal-error line to be recorded as the crash signature")
	}
	if !strings.Contains(snap.Trace, "fatal error: concurrent map writes") {
		t.Fatalf("crash trace should retain the signature line; got:\n%s", snap.Trace)
	}
}
