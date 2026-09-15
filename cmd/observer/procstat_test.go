package main

import "testing"

// TestParseProcStatCPU pins the /proc/<pid>/stat field arithmetic: a comm
// with a space AND an embedded ')' must not shift utime/stime, and a
// truncated line yields ok=false rather than a wrong pair.
func TestParseProcStatCPU(t *testing.T) {
	t.Parallel()
	const stat = "4242 (bench (sut) srv) S 1 4242 4242 0 -1 4194560 100 0 0 0 250 75 0 0 20 0 8 0 12345 1000000 500 18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 17 3 0 0 0 0 0"
	u, s, ok := parseProcStatCPU(stat)
	if !ok || u != 250 || s != 75 {
		t.Fatalf("got utime=%d stime=%d ok=%v want 250/75/true", u, s, ok)
	}
	if _, _, ok := parseProcStatCPU("4242 (x) S 1 2 3"); ok {
		t.Errorf("truncated line: want ok=false")
	}
	if _, _, ok := parseProcStatCPU("garbage"); ok {
		t.Errorf("no comm parenthesis: want ok=false")
	}
}

// TestReadCPUTicks_MissingPIDReturnsZero: a vanished process reads as a
// zero pair, never an error, so the sampler keeps ticking.
func TestReadCPUTicks_MissingPIDReturnsZero(t *testing.T) {
	t.Parallel()
	if u, s := readCPUTicks(1 << 30); u != 0 || s != 0 {
		t.Errorf("got %d/%d want 0/0", u, s)
	}
}
