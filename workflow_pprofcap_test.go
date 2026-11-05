package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// THROWAWAY (ab/c1-harness-h6h7-20261105, not for merge). H7' of the v1.6.0 attribution plan: ansible/files/pprofcap.py is the in-window collector (a CPU + goroutine profile for #857, a
// goroutine?debug=2 dump after a second with zero completions for #854). These tests run the REAL script against a fake SUT side listener whose request counter follows a scripted load
// model, and pin the properties the PREREG relies on: the dump is taken only inside the window, only after the zero second was written to the events file, never while no connection is
// open, a failed sample is not a zero, dumps are bounded, and a SUT without the gate or without a side listener is skipped, not failed. The YAML wiring is pinned at the end.

type fakeSUT struct {
	t       *testing.T
	srv     *httptest.Server
	t0      time.Time
	load    func(el time.Duration) (count uint64, active int64, ok bool) // the scripted engine counters; ok=false answers 500
	pprofOn bool
	pid     int
	events  string // path of pprofcap.events.jsonl, read at dump time to prove the order
	dumpErr bool

	mu          sync.Mutex
	dumpAt      []time.Duration
	eventsAtDmp []int // number of event lines on disk when each dump was requested
	profile     [][2]time.Duration
	order       []string
	varsCalls   atomic.Int64
}

func newFakeSUT(t *testing.T, load func(el time.Duration) (uint64, int64, bool)) *fakeSUT {
	f := &fakeSUT{t: t, load: load, pprofOn: true, pid: 4242, t0: time.Now()}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/vars", func(w http.ResponseWriter, _ *http.Request) {
		f.varsCalls.Add(1)
		n, a, ok := f.load(time.Since(f.t0))
		if !ok {
			http.Error(w, "boom", 500)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"pid": f.pid, "celeris.engine_metrics": map[string]any{"RequestCount": n, "ActiveConnections": a}})
	})
	mux.HandleFunc("/debug/pprof/", func(w http.ResponseWriter, r *http.Request) {
		if !f.pprofOn {
			http.NotFound(w, r)
			return
		}
		switch {
		case r.URL.Path == "/debug/pprof/goroutine" && r.URL.Query().Get("debug") == "2":
			el := time.Since(f.t0)
			lines := 0
			if b, err := os.ReadFile(f.events); err == nil {
				lines = strings.Count(string(b), "\n")
			}
			f.mu.Lock()
			f.dumpAt = append(f.dumpAt, el)
			f.eventsAtDmp = append(f.eventsAtDmp, lines)
			f.order = append(f.order, "dump")
			f.mu.Unlock()
			if f.dumpErr {
				http.Error(w, "stw too long", 500)
				return
			}
			_, _ = fmt.Fprint(w, "goroutine 1 [running]:\nmain.main()\n")
		case r.URL.Path == "/debug/pprof/goroutine":
			f.mu.Lock()
			f.order = append(f.order, "goroutine")
			f.mu.Unlock()
			_, _ = w.Write(append([]byte{0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 3}, make([]byte, 30)...))
		case r.URL.Path == "/debug/pprof/profile":
			var secs int
			_, _ = fmt.Sscanf(r.URL.Query().Get("seconds"), "%d", &secs)
			s := time.Since(f.t0)
			f.mu.Lock()
			f.order = append(f.order, "profile")
			f.mu.Unlock()
			time.Sleep(time.Duration(secs) * time.Second)
			f.mu.Lock()
			f.profile = append(f.profile, [2]time.Duration{s, time.Since(f.t0)})
			f.mu.Unlock()
			_, _ = w.Write(append([]byte{0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 3}, make([]byte, 30)...))
		default:
			_, _ = fmt.Fprint(w, "<html>profiles</html>")
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSUT) addr() string { return strings.TrimPrefix(f.srv.URL, "http://") }

// steady is a healthy closed-loop load: 1000 requests per second, 8 connections, except that the counter is frozen while el is in any of the freezes.
func steady(freezes ...[2]float64) func(time.Duration) (uint64, int64, bool) {
	return func(el time.Duration) (uint64, int64, bool) {
		s := el.Seconds()
		var n float64
		for t := 0.0; t < s; t += 0.01 { // integrate the rate with the freezes cut out
			frozen := false
			for _, z := range freezes {
				if t >= z[0] && t < z[1] {
					frozen = true
				}
			}
			if !frozen {
				n += 10
			}
		}
		return uint64(n) + 1, 8, true
	}
}

func runPprofcap(t *testing.T, f *fakeSUT, out string, extra ...string) (meta map[string]any) {
	t.Helper()
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	f.events = filepath.Join(out, "pprofcap.events.jsonl")
	args := append([]string{"-I", "ansible/files/pprofcap.py", "--addr", f.addr(), "--out", out, "--pid", fmt.Sprint(f.pid), "--min-conns", "4", "--guard", "90", "--attach-timeout", "3", "--rendezvous-timeout", "10"}, extra...)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, py, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("pprofcap.py: %v\n%s", err, b)
	}
	mb, err := os.ReadFile(filepath.Join(out, "pprofcap.meta.json"))
	if err != nil {
		t.Fatalf("no pprofcap.meta.json: %v\n%s", err, b)
	}
	if err := json.Unmarshal(mb, &meta); err != nil {
		t.Fatal(err)
	}
	return meta
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func eventLines(t *testing.T, out string) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(out, "pprofcap.events.jsonl"))
	if err != nil {
		return nil
	}
	var evs []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("events line %q: %v", l, err)
		}
		evs = append(evs, m)
	}
	return evs
}

func num(m map[string]any, k string) int { v, _ := m[k].(float64); return int(v) }

// A zero-completion second INSIDE the window: the counted second is on disk before the world is stopped, then one dump.
func TestPprofcapDumpsAfterTheZeroSecondIsWritten(t *testing.T) {
	t.Parallel()
	out := t.TempDir()
	f := newFakeSUT(t, steady([2]float64{3.0, 4.6}))
	meta := runPprofcap(t, f, out, "--mode", "dump", "--warm", "1", "--dur", "8", "--min-gap", "2")
	if meta["outcome"] != "done" {
		t.Fatalf("outcome %v, errors %v", meta["outcome"], meta["errors"])
	}
	if num(meta, "zero_seconds") < 1 || num(meta, "dumps") != 1 {
		t.Fatalf("zero_seconds %d dumps %d, want >=1 and exactly 1 (re-baselined after the dump, 2 s gap)", num(meta, "zero_seconds"), num(meta, "dumps"))
	}
	b, err := os.ReadFile(filepath.Join(out, "goroutine-dump-1.txt"))
	if err != nil || !strings.HasPrefix(string(b), "goroutine 1 [running]") {
		t.Fatalf("goroutine-dump-1.txt: %q, %v", b, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.eventsAtDmp) != 1 || f.eventsAtDmp[0] < 1 {
		t.Errorf("at the dump request the events file held %v lines, want >= 1: the dump must come AFTER the counted second is on disk", f.eventsAtDmp)
	}
	if f.dumpAt[0] < 3*time.Second || f.dumpAt[0] > 8*time.Second {
		t.Errorf("the dump was taken at %v, outside the freeze [3.0 s, 4.6 s] plus sampling slack", f.dumpAt[0])
	}
	evs := eventLines(t, out)
	if len(evs) < 2 {
		t.Errorf("events %v: want the zero-second line and the dump result line", evs)
	}
	if !exists(filepath.Join(out, "pprofcap.requests.csv")) {
		t.Error("no pprofcap.requests.csv: a run without a dump could not prove the trigger was armed")
	}
}

// A healthy run, a stall before the window, a stall after it, a flat counter with no open connection, and a failing sample are NOT triggers.
func TestPprofcapDoesNotFireOutsideTheWindowOrWithoutConnections(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		load func(time.Duration) (uint64, int64, bool)
		want string
	}{
		{"healthy", steady(), "no freeze"},
		{"frozen before the window", steady([2]float64{0, 2.4}), "warmup stall"},
		{"frozen after the window", steady([2]float64{8.0, 30}), "post-window stall"},
		{"flat counter, no open connection", func(el time.Duration) (uint64, int64, bool) {
			n, _, _ := steady([2]float64{2.0, 30})(el)
			if el.Seconds() >= 2.0 {
				return n, 0, true
			}
			return n, 8, true
		}, "idle server"},
		{"failing samples are not zeros", func(el time.Duration) (uint64, int64, bool) {
			n, a, _ := steady()(el)
			if el.Seconds() > 3.7 && el.Seconds() < 5.4 { // two in-window samples fail after the baseline
				return 0, 0, false
			}
			return n, a, true
		}, "500s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := t.TempDir()
			f := newFakeSUT(t, tc.load)
			meta := runPprofcap(t, f, out, "--mode", "dump", "--warm", "3", "--dur", "4", "--min-gap", "1")
			if meta["outcome"] != "done" || num(meta, "zero_seconds") != 0 || num(meta, "dumps") != 0 {
				t.Errorf("%s: outcome %v zero_seconds %d dumps %d, want done/0/0", tc.want, meta["outcome"], num(meta, "zero_seconds"), num(meta, "dumps"))
			}
			if exists(filepath.Join(out, "pprofcap.events.jsonl")) || len(f.dumpAt) != 0 {
				t.Errorf("%s: an events file or a dump request exists", tc.want)
			}
			if num(meta, "samples") < 2 {
				t.Errorf("%s: only %d samples, the trigger was not armed", tc.want, num(meta, "samples"))
			}
		})
	}
}

// Dumps are bounded: --max-dumps in total, --min-gap apart; the zero seconds beyond the cap are still written to the events file.
func TestPprofcapBoundsTheDumps(t *testing.T) {
	t.Parallel()
	out := t.TempDir()
	f := newFakeSUT(t, steady([2]float64{1.5, 30}))
	meta := runPprofcap(t, f, out, "--mode", "dump", "--warm", "1", "--dur", "10", "--min-gap", "3", "--max-dumps", "2")
	if num(meta, "dumps") != 2 {
		t.Fatalf("dumps %d, want exactly the cap 2 (errors %v)", num(meta, "dumps"), meta["errors"])
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.dumpAt) != 2 || f.dumpAt[1]-f.dumpAt[0] < 3*time.Second {
		t.Errorf("dump times %v: want 2 dumps at least 3 s apart", f.dumpAt)
	}
	if exists(filepath.Join(out, "goroutine-dump-3.txt")) {
		t.Error("a third dump file exists")
	}
	if num(meta, "zero_seconds") <= 2 {
		t.Errorf("zero_seconds %d: the seconds beyond the dump cap must still be counted", num(meta, "zero_seconds"))
	}
}

// A dump that fails (the SUT is too wedged to answer) is recorded in the events file and does not fail the collector.
func TestPprofcapRecordsAFailedDump(t *testing.T) {
	t.Parallel()
	out := t.TempDir()
	f := newFakeSUT(t, steady([2]float64{2.0, 30}))
	f.dumpErr = true
	meta := runPprofcap(t, f, out, "--mode", "dump", "--warm", "1", "--dur", "5", "--min-gap", "100")
	if meta["outcome"] != "done" || num(meta, "dumps") != 0 {
		t.Fatalf("outcome %v dumps %d", meta["outcome"], num(meta, "dumps"))
	}
	var sawErr bool
	for _, e := range eventLines(t, out) {
		if d, ok := e["dump"].(map[string]any); ok && d["error"] != nil {
			sawErr = true
		}
	}
	if !sawErr {
		t.Error("the failed dump is not recorded in the events file")
	}
}

// profile mode: the CPU profile runs inside the window, the goroutine profile follows it, both are saved.
func TestPprofcapProfileModeCapturesInsideTheWindow(t *testing.T) {
	t.Parallel()
	out := t.TempDir()
	f := newFakeSUT(t, steady())
	meta := runPprofcap(t, f, out, "--mode", "profile", "--warm", "2", "--dur", "10", "--profile-seconds", "3")
	if meta["outcome"] != "done" {
		t.Fatalf("outcome %v errors %v", meta["outcome"], meta["errors"])
	}
	for _, n := range []string{"cpu.pb.gz", "goroutine.pb.gz"} {
		if b, err := os.ReadFile(filepath.Join(out, n)); err != nil || len(b) < 4 || b[0] != 0x1f || b[1] != 0x8b {
			t.Errorf("%s: %v, %d bytes", n, err, len(b))
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.profile) != 1 {
		t.Fatalf("%d CPU profile requests, want 1", len(f.profile))
	}
	s, e := f.profile[0][0], f.profile[0][1]
	if s < 2*time.Second || e > 12*time.Second+time.Second {
		t.Errorf("the CPU profile ran %v..%v, outside the window [2 s, 12 s]", s, e)
	}
	if d := e - s; d < 3*time.Second || d > 4*time.Second {
		t.Errorf("the CPU profile lasted %v, want 3 s", d)
	}
	if strings.Join(f.order, ",") != "profile,goroutine" {
		t.Errorf("request order %v, want the CPU profile then the goroutine profile (no stop-the-world inside the CPU profile)", f.order)
	}
	if len(f.dumpAt) != 0 || exists(filepath.Join(out, "pprofcap.events.jsonl")) {
		t.Error("profile mode must not arm the dump trigger")
	}
}

// A profile that does not fit the window is shortened, never longer than dur-2.
func TestPprofcapShortWindowShortensTheProfile(t *testing.T) {
	t.Parallel()
	out := t.TempDir()
	f := newFakeSUT(t, steady())
	meta := runPprofcap(t, f, out, "--mode", "profile", "--warm", "1", "--dur", "6", "--profile-seconds", "30")
	if meta["outcome"] != "done" {
		t.Fatalf("outcome %v errors %v", meta["outcome"], meta["errors"])
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.profile) != 1 || f.profile[0][1]-f.profile[0][0] > 5*time.Second {
		t.Errorf("profile %v, want one profile of at most dur-2 = 4 s", f.profile)
	}
}

// A SUT that does not carry the gate, one without a side listener, and one that is another process are skipped, with the reason in the meta file and nothing else on disk.
func TestPprofcapSkipsWhatItCannotCollect(t *testing.T) {
	t.Parallel()
	t.Run("pprof not mounted", func(t *testing.T) {
		t.Parallel()
		out := t.TempDir()
		f := newFakeSUT(t, steady())
		f.pprofOn = false
		meta := runPprofcap(t, f, out, "--mode", "both", "--warm", "1", "--dur", "4")
		if o, _ := meta["outcome"].(string); !strings.HasPrefix(o, "skipped") || !strings.Contains(o, "PROBATORIUM_PPROF") {
			t.Errorf("outcome %q", meta["outcome"])
		}
	})
	t.Run("no side listener", func(t *testing.T) {
		t.Parallel()
		out := t.TempDir()
		f := newFakeSUT(t, steady())
		addr := f.addr()
		f.srv.Close()
		f.srv = httptest.NewServer(http.NotFoundHandler()) // keep Cleanup happy
		defer f.srv.Close()
		py, err := exec.LookPath("python3")
		if err != nil {
			t.Skip("python3 not installed")
		}
		b, err := exec.Command(py, "-I", "ansible/files/pprofcap.py", "--addr", addr, "--mode", "both", "--out", out, "--warm", "1", "--dur", "4", "--attach-timeout", "1.5").CombinedOutput()
		if err != nil {
			t.Fatalf("%v\n%s", err, b)
		}
		var meta map[string]any
		mb, _ := os.ReadFile(filepath.Join(out, "pprofcap.meta.json"))
		_ = json.Unmarshal(mb, &meta)
		if o, _ := meta["outcome"].(string); !strings.HasPrefix(o, "skipped: no /debug/vars") {
			t.Errorf("outcome %q", meta["outcome"])
		}
	})
	t.Run("another process", func(t *testing.T) {
		t.Parallel()
		out := t.TempDir()
		f := newFakeSUT(t, steady())
		f.pid = 1
		py, err := exec.LookPath("python3")
		if err != nil {
			t.Skip("python3 not installed")
		}
		if b, err := exec.Command(py, "-I", "ansible/files/pprofcap.py", "--addr", f.addr(), "--mode", "dump", "--out", out, "--pid", "4242", "--warm", "1", "--dur", "4").CombinedOutput(); err != nil {
			t.Fatalf("%v\n%s", err, b)
		}
		var meta map[string]any
		mb, _ := os.ReadFile(filepath.Join(out, "pprofcap.meta.json"))
		_ = json.Unmarshal(mb, &meta)
		if o, _ := meta["outcome"].(string); !strings.Contains(o, "skipped") || !strings.Contains(o, "pid") {
			t.Errorf("outcome %q", meta["outcome"])
		}
	})
}

// R5: a second in which every connection closed (the end of a scenario inside the window) with a flat counter is NOT a zero second: connections must be open at BOTH ends.
func TestPprofcapConnectionsClosingInsideTheWindowIsNotAStall(t *testing.T) {
	t.Parallel()
	out := t.TempDir()
	f := newFakeSUT(t, func(el time.Duration) (uint64, int64, bool) {
		n, _, _ := steady([2]float64{4.5, 60})(el)
		if el.Seconds() >= 4.5 {
			return n, 0, true // all connections closed together with the last request
		}
		return n, 8, true
	})
	meta := runPprofcap(t, f, out, "--mode", "dump", "--warm", "3", "--dur", "5", "--min-gap", "1")
	if meta["outcome"] != "done" || num(meta, "zero_seconds") != 0 || num(meta, "dumps") != 0 || len(f.dumpAt) != 0 {
		t.Errorf("outcome %v zero_seconds %d dumps %d: closing the connections must not read as a stall", meta["outcome"], num(meta, "zero_seconds"), num(meta, "dumps"))
	}
}

// P3: a load under which the request counter does not move although connections are open (a WebSocket or SSE window) never arms the dump.
func TestPprofcapSkipsALoadWhoseRequestCounterDoesNotMove(t *testing.T) {
	t.Parallel()
	out := t.TempDir()
	f := newFakeSUT(t, func(time.Duration) (uint64, int64, bool) { return 500, 8, true })
	meta := runPprofcap(t, f, out, "--mode", "dump", "--warm", "2", "--dur", "5", "--min-gap", "1")
	if _, ok := meta["trigger_skipped"].(string); !ok || num(meta, "dumps") != 0 || num(meta, "zero_seconds") != 0 || len(f.dumpAt) != 0 {
		t.Errorf("meta %v: want trigger_skipped, no zero seconds, no dump", meta)
	}
	if exists(filepath.Join(out, "pprofcap.events.jsonl")) {
		t.Error("an events file exists for a load the proxy cannot read")
	}
}

func startPprofcap(t *testing.T, f *fakeSUT, out string, extra ...string) *exec.Cmd {
	t.Helper()
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	f.events = filepath.Join(out, "pprofcap.events.jsonl")
	args := append([]string{"-I", "ansible/files/pprofcap.py", "--addr", f.addr(), "--out", out, "--pid", fmt.Sprint(f.pid), "--min-conns", "4", "--guard", "90", "--attach-timeout", "3"}, extra...)
	cmd := exec.Command(py, args...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd
}

func readMeta(t *testing.T, out string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(out, "pprofcap.meta.json"))
	if err != nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("pprofcap.meta.json is not valid JSON (a torn write): %v\n%s", err, b)
	}
	return m
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// P4: the teardown task stops a slow collector with SIGTERM. A collector still waiting for its rendezvous must leave its meta (outcome terminated), not nothing.
func TestPprofcapSigtermLeavesTheMeta(t *testing.T) {
	t.Parallel()
	out := t.TempDir()
	f := newFakeSUT(t, func(time.Duration) (uint64, int64, bool) { return 1, 0, true }) // never reaches the rendezvous
	cmd := startPprofcap(t, f, out, "--mode", "dump", "--warm", "1", "--dur", "4", "--rendezvous-timeout", "60")
	waitFor(t, "the early meta", func() bool { m := readMeta(t, out); return m != nil && m["outcome"] == "started" })
	time.Sleep(700 * time.Millisecond) // inside the rendezvous loop
	_ = cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the collector did not exit on SIGTERM")
	}
	m := readMeta(t, out)
	if o, _ := m["outcome"].(string); !strings.HasPrefix(o, "terminated") {
		t.Errorf("outcome %q after SIGTERM, want terminated...", o)
	}
}

// P4: even a SIGKILL (nothing runs) leaves the last atomic write: the meta says "running" with the window it was watching.
func TestPprofcapSigkillLeavesTheLastMeta(t *testing.T) {
	t.Parallel()
	out := t.TempDir()
	f := newFakeSUT(t, steady())
	cmd := startPprofcap(t, f, out, "--mode", "dump", "--warm", "1", "--dur", "30", "--rendezvous-timeout", "20")
	waitFor(t, "the running meta", func() bool { m := readMeta(t, out); return m != nil && m["outcome"] == "running" })
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	m := readMeta(t, out)
	if m["outcome"] != "running" || num(m, "window_s") != 30 {
		t.Errorf("after SIGKILL the meta is %v, want outcome running with window_s 30", m)
	}
}

// The wiring in run_bench_cell.yml: the gate is the SUT env key (merge of bench_sut_env and the column env), the collector exists only for dump|profile|both, runs on the SUT, is never
// root, and the free-form shell has no apostrophe (ansible's splitter).
func TestRunBenchCellStartsThePprofCollectorOnlyWhenTheColumnEnvAsks(t *testing.T) {
	y := mustRead(t, "ansible/tasks/run_bench_cell.yml")
	i := strings.Index(y, "copy the pprof collector")
	j := strings.Index(y, "run runner against bench target")
	k := strings.Index(y, "stop the pprof collector")
	if i < 0 || j < 0 || k < 0 || !(i < j && j < k) {
		t.Fatalf("the collector tasks are not placed copy < runner < stop (%d %d %d)", i, j, k)
	}
	block := y[i:j]
	if strings.Count(block, "cell_pprof_mode in ['dump', 'profile', 'both']") != 2 {
		t.Error("both collector tasks must be gated on cell_pprof_mode in ['dump', 'profile', 'both']")
	}
	if strings.Count(block, "((bench_sut_env | default({})) | combine(cell_col_env | default({}))).get('PROBATORIUM_PPROF', '')") != 2 {
		t.Error("the mode must be read from the same merge the server is launched with (bench_sut_env, then the column env)")
	}
	if strings.Count(block, "inventory_hostname == bench_target") != 2 || strings.Count(block, "cell_bound | bool") != 2 {
		t.Error("the collector must run on the SUT and only for a bound column")
	}
	if strings.Contains(block, "become: true") {
		t.Error("the collector needs no root: it talks to a loopback port and writes into the cell directory")
	}
	// the shell block: no apostrophe (the splitter), the loopback address, the pid check
	m := regexp.MustCompile(`(?s)ansible.builtin.shell: \|\n(.*?)\n  args:`).FindStringSubmatch(block)
	if m == nil || strings.Contains(m[1], "'") {
		t.Errorf("the collector shell block must exist and contain no apostrophe: %q", m)
	}
	for _, want := range []string{"--addr 127.0.0.1:{{ bench_debug_port | default(18089) }}", "--pid $(cat {{ cell_dir }}/server.pid)", "--mode {{ cell_pprof_mode }}", "--guard {{ cell_guard_seconds }}"} {
		if !strings.Contains(m[1], want) {
			t.Errorf("collector command lacks %q", want)
		}
	}
	// R7: the collector's window is the runner's window: --warm and --dur are the SAME expressions as the runner's -warmup and -duration
	runner := y[j:]
	for _, tc := range []struct{ flag, runnerFlag string }{{"--warm", "-warmup"}, {"--dur", "-duration"}} {
		cm := regexp.MustCompile(regexp.QuoteMeta(tc.flag) + ` (\{\{ [a-z_]+ \| default\(\d+\) \}\})`).FindStringSubmatch(m[1])
		rm := regexp.MustCompile(regexp.QuoteMeta(tc.runnerFlag) + ` (\{\{ [a-z_]+ \| default\(\d+\) \}\})s`).FindStringSubmatch(runner)
		if cm == nil || rm == nil || cm[1] != rm[1] {
			t.Errorf("collector %s %v and runner %s %v must be the same expression: a misaligned window profiles the wrong seconds", tc.flag, cm, tc.runnerFlag, rm)
		}
	}
	// R10: a failing collector must never drop the bench target from the play
	if strings.Count(block, "failed_when: false") != 2 {
		t.Error("both the copy and the start task of the collector must carry failed_when: false")
	}
	if !strings.Contains(y[k:k+1500], "failed_when: false") {
		t.Error("the stop task of the collector must carry failed_when: false")
	}
	// the debug address the collector reads is the one the server binds, loopback only
	if !strings.Contains(y, `PROBATORIUM_DEBUG_ADDR: "127.0.0.1:{{ bench_debug_port | default(18089) }}"`) {
		t.Error("the side listener address is no longer loopback: the pprof mount would be refused")
	}
	// the cell directory is what bench.yml tars: the files need no workflow change
	bench := mustRead(t, "ansible/bench.yml")
	if !strings.Contains(bench, "tar czf /tmp/bench-{{ inventory_hostname }}-{{ bench_target }}.tgz {{ bench_run_dir | basename }}/") {
		t.Error("bench.yml no longer tars the whole bench_run_dir: the collector files would not reach the artifact")
	}
}
