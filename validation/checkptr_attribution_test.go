package validation

import (
	"testing"
)

// TestCheckptrSignaturesAreRecognised pins the four messages runtime/checkptr.go
// actually throws (Go 1.27), as the runtime prints them. A future fifth is
// caught by the substring; these four are the contract.
func TestCheckptrSignaturesAreRecognised(t *testing.T) {
	for _, line := range []string{
		"fatal error: checkptr: misaligned pointer conversion",
		"fatal error: checkptr: converted pointer straddles multiple allocations",
		"fatal error: checkptr: pointer arithmetic computed bad pointer value",
		"fatal error: checkptr: pointer arithmetic result points to invalid allocation",
	} {
		if !looksLikeCrash(line) {
			t.Errorf("liveness scanner does not treat %q as a crash; the fatal error: marker should", line)
		}
		if !isCheckptrSignature(line) {
			t.Errorf("%q not attributed to checkptr", line)
		}
	}
	// A different fatal error must NOT be counted as checkptr, or every
	// crash in a -tags=checkptr cell would be booked against the wrong oracle.
	for _, line := range []string{
		"fatal error: sync: unlock of unlocked mutex",
		"fatal error: concurrent map writes",
		"runtime: out of memory",
	} {
		if isCheckptrSignature(line) {
			t.Errorf("%q wrongly attributed to checkptr", line)
		}
	}
}

// TestLivenessCountsCheckptrCrashes: the scan that already catches the crash
// must also count it, because that count is the ONLY route to I-CHECKPTR --
// the process is dead before the property loop's next poll.
func TestLivenessCountsCheckptrCrashes(t *testing.T) {
	var l livenessTally
	l.recordSignature("fatal error: checkptr: misaligned pointer conversion")
	s := l.snapshot()
	if !s.Crashed {
		t.Fatal("crash not recorded")
	}
	if s.CheckptrReports != 1 {
		t.Fatalf("checkptr_reports = %d, want 1", s.CheckptrReports)
	}

	var other livenessTally
	other.recordSignature("fatal error: concurrent map writes")
	if n := other.snapshot().CheckptrReports; n != 0 {
		t.Fatalf("a non-checkptr fatal error counted as checkptr: %d", n)
	}
}
