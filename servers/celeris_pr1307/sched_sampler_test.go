package main

import (
	"math"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// schedLineRE is the line contract with the P3a reader (see the header of sched_sampler.go).
var schedLineRE = regexp.MustCompile(`^\[sched\] \d+ lat_n=(\d+|na) lat=(\S+) pin_n=(\d+|na) pin_run_ns=(\d+|na) pin_wait_ns=(\d+|na) pin_slices=(\d+|na) oth_n=(\d+|na) oth_run_ns=(\d+|na) oth_wait_ns=(\d+|na) oth_slices=(\d+|na)$`)

func TestParseSchedSample(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
		err  bool
	}{{"1", true, false}, {"0", false, false}, {"", false, true}, {"yes", false, true}, {"true", false, true}, {"01", false, true}, {"2", false, true}} {
		got, err := parseSchedSample(tc.in)
		if got != tc.want || (err != nil) != tc.err {
			t.Errorf("parseSchedSample(%q) = %v, %v", tc.in, got, err)
		}
	}
}

type fakeProc struct {
	mu      sync.Mutex
	counts  []uint64
	buckets []float64
	threads map[int]threadSched
	hok     bool
	tok     bool
}

func (f *fakeProc) sampler(emit func(string), now time.Time) *schedSampler {
	return &schedSampler{
		emit: emit,
		hist: func() ([]uint64, []float64, bool) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return append([]uint64(nil), f.counts...), f.buckets, f.hok
		},
		threads: func() (map[int]threadSched, bool) {
			f.mu.Lock()
			defer f.mu.Unlock()
			m := map[int]threadSched{}
			for k, v := range f.threads {
				m[k] = v
			}
			return m, f.tok
		},
		now: func() time.Time { return now },
	}
}

// The deltas: histogram buckets (non-zero only, -Inf first bucket) and per-thread schedstat split into pinned and other threads.
func TestSchedSamplerDeltas(t *testing.T) {
	f := &fakeProc{
		hok: true, tok: true,
		buckets: []float64{math.Inf(-1), 1e-6, 1e-3, math.Inf(1)},
		counts:  []uint64{5, 10, 1},
		threads: map[int]threadSched{1: {100, 10, 5, true}, 2: {200, 20, 6, false}},
	}
	now := time.Unix(1700000000, 0)
	s := f.sampler(nil, now)
	s.baseline()

	f.counts = []uint64{5, 14, 1}
	f.threads = map[int]threadSched{1: {160, 15, 8, true}, 2: {260, 25, 9, false}, 3: {7, 8, 9, true}} // 3 is new: counts from zero
	l := s.sample()
	want := "[sched] 1700000000 lat_n=4 lat=1e-06:4 pin_n=2 pin_run_ns=67 pin_wait_ns=13 pin_slices=12 oth_n=1 oth_run_ns=60 oth_wait_ns=5 oth_slices=3"
	if l != want {
		t.Errorf("got  %s\nwant %s", l, want)
	}
	if !schedLineRE.MatchString(l) {
		t.Errorf("line does not match the contract: %s", l)
	}

	// no change in a second: "-"; the first bucket prints -Inf
	l = s.sample()
	if !strings.Contains(l, "lat_n=0 lat=- ") || !strings.Contains(l, "pin_run_ns=0 ") {
		t.Errorf("idle second: %s", l)
	}
	f.counts = []uint64{6, 14, 1}
	if l = s.sample(); !strings.Contains(l, "lat_n=1 lat=-Inf:1 ") {
		t.Errorf("first bucket: %s", l)
	}
	// a thread that exited vanishes; a counter that goes down (impossible, defensive) clamps to 0
	f.threads = map[int]threadSched{1: {100, 10, 5, true}}
	if l = s.sample(); !strings.Contains(l, "pin_n=1 pin_run_ns=0 ") || !strings.Contains(l, "oth_n=0 ") {
		t.Errorf("exited thread: %s", l)
	}
}

func TestSchedSamplerUnavailable(t *testing.T) {
	f := &fakeProc{hok: false, tok: false}
	s := f.sampler(nil, time.Unix(5, 0))
	s.baseline()
	l := s.sample()
	if !schedLineRE.MatchString(l) || !strings.Contains(l, "lat_n=na lat=na pin_n=na ") {
		t.Errorf("unavailable sources must print na: %s", l)
	}
}

type sink struct {
	mu    sync.Mutex
	lines []string
	hold  chan struct{} // when non-nil, emit blocks until it is closed
	in    chan struct{} // signalled when a blocked emit starts
}

func (k *sink) emit(l string) {
	if k.hold != nil {
		k.in <- struct{}{}
		<-k.hold
	}
	k.mu.Lock()
	k.lines = append(k.lines, l)
	k.mu.Unlock()
}
func (k *sink) n() int { k.mu.Lock(); defer k.mu.Unlock(); return len(k.lines) }

// THE property of the file: nothing is written once stop() has returned, even with ticks still pending.
func TestSchedSamplerNothingAfterStop(t *testing.T) {
	f := &fakeProc{hok: true, tok: true, buckets: []float64{0, 1}, counts: []uint64{1}}
	k := &sink{}
	ticks := make(chan time.Time, 64)
	stop := startSchedSampler(f.sampler(k.emit, time.Unix(1, 0)), ticks)
	ticks <- time.Now()
	ticks <- time.Now()
	deadline := time.Now().Add(5 * time.Second)
	for k.n() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if k.n() != 2 {
		t.Fatalf("%d lines before stop, want 2", k.n())
	}
	for range 50 { // a backlog of pending ticks
		ticks <- time.Now()
	}
	stop()
	at := k.n()
	for range 50 {
		select {
		case ticks <- time.Now():
		default:
		}
	}
	time.Sleep(50 * time.Millisecond)
	runtime.Gosched()
	if got := k.n(); got != at {
		t.Fatalf("%d lines after stop() returned, %d at the return", got, at)
	}
	stop() // idempotent
}

// stop() waits for a write in flight: after it returns that write is finished and no later one starts.
func TestSchedSamplerStopWaitsForInFlightWrite(t *testing.T) {
	f := &fakeProc{hok: true, tok: true, buckets: []float64{0, 1}, counts: []uint64{0}}
	k := &sink{hold: make(chan struct{}), in: make(chan struct{}, 1)}
	ticks := make(chan time.Time, 8)
	stop := startSchedSampler(f.sampler(k.emit, time.Unix(1, 0)), ticks)
	ticks <- time.Now()
	ticks <- time.Now()
	<-k.in // the first write is in flight (blocked)
	returned := make(chan struct{})
	go func() { stop(); close(returned) }()
	select {
	case <-returned:
		t.Fatal("stop() returned while a write was in flight")
	case <-time.After(100 * time.Millisecond):
	}
	close(k.hold) // release the write
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("stop() did not return after the write finished")
	}
	if n := k.n(); n != 1 {
		t.Fatalf("%d lines, want exactly the one in flight (the second tick must not write after the stop)", n)
	}
}

func TestSchedSamplerPanicDoesNotEscape(t *testing.T) {
	k := &sink{}
	calls := 0
	s := &schedSampler{
		emit: k.emit,
		hist: func() ([]uint64, []float64, bool) {
			calls++
			if calls > 1 { // the baseline (call 1) works, the first tick panics
				panic("boom")
			}
			return nil, nil, false
		},
		threads: func() (map[int]threadSched, bool) { return nil, false },
		now:     time.Now,
	}
	ticks := make(chan time.Time, 1)
	stop := startSchedSampler(s, ticks)
	ticks <- time.Now()
	deadline := time.Now().Add(5 * time.Second)
	for k.n() < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	stop()
	if k.n() != 1 || !strings.Contains(k.lines[0], "sampler stopped: panic") {
		t.Errorf("lines %v", k.lines)
	}
}

// The real readers: runtime/metrics works everywhere; /proc only on Linux (elsewhere the line says na).
func TestSchedSamplerRealSources(t *testing.T) {
	var out []string
	s := &schedSampler{emit: func(l string) { out = append(out, l) }, hist: runtimeSchedHist, threads: procThreads, now: time.Now}
	s.baseline()
	time.Sleep(20 * time.Millisecond)
	l := s.sample()
	if !schedLineRE.MatchString(l) {
		t.Fatalf("real line does not match the contract: %s", l)
	}
	if strings.Contains(l, "lat_n=na") {
		t.Errorf("runtime/metrics %s is unavailable on this Go: %s", schedLatMetric, l)
	}
	if runtime.GOOS == "linux" && strings.Contains(l, "pin_n=na") {
		t.Errorf("/proc/self/task unreadable on linux: %s", l)
	}
}

func TestThreadPinnedMissingThread(t *testing.T) {
	if threadPinned("999999999") {
		t.Error("a thread that does not exist must not be pinned")
	}
}

// A bad PROBATORIUM_SCHED_SAMPLE must end the process; unset and 0 are no-ops; 1 starts a sampler that stops.
func TestMustStartSchedSampler(t *testing.T) {
	if os.Getenv("P3A_SCHED_FATAL_CHILD") == "1" {
		mustStartSchedSampler()
		os.Exit(0)
	}
	for _, raw := range []string{"yes", "", "2"} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestMustStartSchedSampler$")
		cmd.Env = append(os.Environ(), "P3A_SCHED_FATAL_CHILD=1", schedSampleEnv+"="+raw)
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "sched sampler") {
			t.Errorf("env %q: err %v output %s, want a fatal start error naming the sampler", raw, err, out)
		}
	}
	t.Setenv(schedSampleEnv, "")
	os.Unsetenv(schedSampleEnv)
	mustStartSchedSampler()() // off
	t.Setenv(schedSampleEnv, "0")
	mustStartSchedSampler()() // explicit off
	t.Setenv(schedSampleEnv, "1")
	mustStartSchedSampler()() // on, stopped at once
}

// server.go: the sampler starts after the [sut-env]-era startup lines (after the [workers] line) and
// is stopped before the "signal received" line is printed (the CRASH rule), synchronously and first.
func TestServerStopsSamplerBeforeSignalLine(t *testing.T) {
	b, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	idx := func(sub string) int {
		i := strings.Index(s, sub)
		if i < 0 {
			t.Fatalf("server.go has no %q", sub)
		}
		return i
	}
	if n := strings.Count(s, "stopSched()"); n != 1 {
		t.Fatalf("server.go calls stopSched() %d times, want exactly 1 (in the signal goroutine)", n)
	}
	start, wk, stop, sig := idx("mustStartSchedSampler()"), idx("log.Print(wk.line())"), idx("stopSched()"), idx(`log.Printf("celeris: signal received, shutting down")`)
	if !(wk < start) {
		t.Errorf("the sampler starts (%d) before the [workers] line (%d)", start, wk)
	}
	if !(stop < sig) {
		t.Errorf("stopSched() (%d) comes after the signal line (%d): every celeris column would CRASH-void", stop, sig)
	}
	wait := idx("<-sig")
	if !(wait < stop) {
		t.Errorf("stopSched() (%d) is before the signal wait (%d)", stop, wait)
	}
	// nothing between the signal wait and the stop may log
	between := s[wait:stop]
	if strings.Contains(between, "log.") {
		t.Errorf("a log call between <-sig and stopSched(): %q", between)
	}
}
