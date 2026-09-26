package validation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/probatorium/report"
	"github.com/goceleris/probatorium/validation/remote"
)

// The celeris#588 capture control's second live run (probatorium 5f6b6e6,
// evidence 588/20260926T160745Z-5f6b6e6) failed on four capture defects the
// tests below pin, one each:
//   - the dossier's pprof leg went through the engine, which the stall had
//     parked on io_uring/epoll: dumps came seconds late (post-release,
//     naming nothing) or not at all;
//   - the dossier's stderr tail was the tally tick's copy, which predated the
//     stall's own "[fault] hold" line;
//   - the refapp's side-listener banner had no reader;
//   - a later engine-wide wedge overwrote the 32-deep slow-fire ring, losing
//     the first episode's records.

func newTestOrch(t *testing.T, mode string) *Orchestrator {
	t.Helper()
	o, err := New(Config{Target: "localhost", Arch: "amd64", Duration: time.Minute, OutDir: t.TempDir(), DriverMode: mode})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return o
}

// TestNewAsksLocalRefappsForTheSideListener: a local launch inherits
// PROBATORIUM_REFAPP_DEBUG_ADDR=127.0.0.1:0 from the validator; an ssh
// launch does not (its loopback is another host's); an operator's own value
// stands.
func TestNewAsksLocalRefappsForTheSideListener(t *testing.T) {
	t.Setenv(refappDebugAddrEnv, "")
	newTestOrch(t, "local")
	if got := os.Getenv(refappDebugAddrEnv); got != "127.0.0.1:0" {
		t.Fatalf("local driver: %s=%q, want 127.0.0.1:0", refappDebugAddrEnv, got)
	}
	t.Setenv(refappDebugAddrEnv, "")
	newTestOrch(t, "ssh")
	if got := os.Getenv(refappDebugAddrEnv); got != "" {
		t.Fatalf("ssh driver: %s=%q, want it left unset", refappDebugAddrEnv, got)
	}
	t.Setenv(refappDebugAddrEnv, "127.0.0.1:18090")
	newTestOrch(t, "local")
	if got := os.Getenv(refappDebugAddrEnv); got != "127.0.0.1:18090" {
		t.Fatalf("operator value overwritten: %q", got)
	}
}

// TestSuperviseStderrLearnsTheDebugAddr: the pre-ready banner is parsed;
// the same text after ready is refapp output, not an announcement.
func TestSuperviseStderrLearnsTheDebugAddr(t *testing.T) {
	l := &livenessTally{}
	superviseStderr(strings.NewReader("debug addr=127.0.0.1:41999\nready addr=127.0.0.1:8080\ndebug addr=127.0.0.1:1\n"),
		l, func(string) {}, func(error) {}, func() {})
	if got := l.debugAddrLoad(); got != "127.0.0.1:41999" {
		t.Fatalf("debug addr = %q, want the pre-ready banner's 127.0.0.1:41999", got)
	}
	none := &livenessTally{}
	superviseStderr(strings.NewReader("ready addr=127.0.0.1:8080\n"), none, func(string) {}, func(error) {}, func() {})
	if got := none.debugAddrLoad(); got != "" {
		t.Fatalf("no banner: debug addr = %q, want empty", got)
	}
}

// TestRefappDebugBannerMatchesDebugvars keeps this package's copies of the
// env name and banner prefix equal to the refapp module's, which it cannot
// import (validation/refapp/internal).
func TestRefappDebugBannerMatchesDebugvars(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("refapp", "internal", "debugvars", "sidelistener.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`const DebugAddrEnv = "` + refappDebugAddrEnv + `"`,
		`const DebugBannerPrefix = "` + refappDebugBannerPrefix + `"`,
	} {
		if !strings.Contains(string(src), want) {
			t.Errorf("debugvars/sidelistener.go lacks %s", want)
		}
	}
}

// TestDossierPprofComesFromTheSideListener: with a side listener announced,
// every profile is fetched from it and the engine is not asked at all; the
// text dump is fetched first; forensics_status.txt names the source.
func TestDossierPprofComesFromTheSideListener(t *testing.T) {
	var engineHits atomic.Int64
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		engineHits.Add(1)
		_, _ = w.Write([]byte("engine"))
	}))
	defer engine.Close()
	var mu sync.Mutex
	var order []string
	side := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		order = append(order, r.URL.RequestURI())
		mu.Unlock()
		if r.URL.Query().Get("debug") == "2" {
			_, _ = w.Write([]byte("goroutine 7 [sync.Mutex.Lock]:\ndebugvars.(*FaultHold).wait"))
			return
		}
		_, _ = w.Write([]byte("side"))
	}))
	defer side.Close()

	o := newTestOrch(t, "local")
	o.cfg.CelerisListenAddr = strings.TrimPrefix(engine.URL, "http://")
	sideAddr := strings.TrimPrefix(side.URL, "http://")
	addrFn := func() string { return sideAddr }
	o.debugAddr.Store(&addrFn)
	dir := t.TempDir()
	if err := o.captureForensics(context.Background(), dir, Incident{SkipCore: true}); err != nil {
		t.Fatal(err)
	}
	if n := engineHits.Load(); n != 0 {
		t.Errorf("the engine was asked for %d profile(s); with a side listener it must not be", n)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "goroutine-stacks.txt")); !strings.Contains(string(b), "FaultHold).wait") {
		t.Errorf("goroutine-stacks.txt = %q, want the side listener's dump", b)
	}
	mu.Lock()
	first := ""
	if len(order) > 0 {
		first = order[0]
	}
	mu.Unlock()
	if first != "/debug/pprof/goroutine?debug=2" {
		t.Errorf("first profile fetched = %q, want the text goroutine dump", first)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "forensics_status.txt")); !strings.Contains(string(b), "pprof_source=side-listener") ||
		!strings.Contains(string(b), sideAddr) {
		t.Errorf("forensics_status.txt = %q, want the side listener named as the pprof source", b)
	}

	// No side listener announced: the engine address, as before.
	o2 := newTestOrch(t, "local")
	o2.cfg.CelerisListenAddr = strings.TrimPrefix(engine.URL, "http://")
	empty := func() string { return "" }
	o2.debugAddr.Store(&empty)
	dir2 := t.TempDir()
	if err := o2.captureForensics(context.Background(), dir2, Incident{SkipCore: true}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir2, "goroutine-stacks.txt")); string(b) != "engine" {
		t.Errorf("fallback: goroutine-stacks.txt = %q, want the engine's", b)
	}
}

// TestDossierTailIsLive: the dossier reads Tier 1's live ring, not the tally
// tick's older copy.
func TestDossierTailIsLive(t *testing.T) {
	o := newTestOrch(t, "local")
	stale := []string{"2026/09/26 16:16:11 INFO std engine listening"}
	o.stderrTail.Store(&stale)
	live := func() []string {
		return append(append([]string(nil), stale...), "[fault] hold path=/ws hold=8s start=2026-09-26T16:16:41.960783295Z")
	}
	o.liveTail.Store(&live)
	dir, err := o.writeIncidentDossier(Incident{PredicateID: "I-WS-STALL", RecordOnly: true, ObservedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "refapp_stderr_tail.txt"))
	if !strings.Contains(string(b), "[fault] hold path=/ws") {
		t.Fatalf("dossier tail lacks the line written after the last tick:\n%s", b)
	}
}

// TestSlowFireRingPinsTheFirstFailures: the first failed fires survive a
// flood that overwrites the ring, in arrival order ahead of it; a pinned
// fire still inside the ring is not duplicated; only the first
// slowFirePinnedFailures are pinned; slow successes are never pinned.
func TestSlowFireRingPinsTheFirstFailures(t *testing.T) {
	var r slowFireRing
	for i := 0; i < 3; i++ {
		r.add(report.SlowFire{ReadMs: int64(i), Outcome: "declined"})
	}
	for i := 0; i < 4; i++ {
		r.add(report.SlowFire{ReadMs: int64(100 + i), Outcome: "handshake-fail-timeout"})
	}
	if got := r.snapshot(); len(got) != 7 {
		t.Fatalf("under the bound, pinned fires still in the ring must not repeat: %d entries", len(got))
	}
	for i := 0; i < 3*slowFireRingSize; i++ {
		r.add(report.SlowFire{ReadMs: int64(1000 + i), Outcome: "upgraded"})
	}
	got := r.snapshot()
	if len(got) != 4+slowFireRingSize {
		t.Fatalf("snapshot holds %d, want 4 pinned + %d", len(got), slowFireRingSize)
	}
	for i := 0; i < 4; i++ {
		if got[i].ReadMs != int64(100+i) || got[i].Outcome != "handshake-fail-timeout" {
			t.Fatalf("entry %d = %+v, want the %d-th first failure", i, got[i], i)
		}
	}
	if got[4].ReadMs != int64(1000+2*slowFireRingSize) {
		t.Fatalf("ring part starts at %d, want the last %d fires", got[4].ReadMs, slowFireRingSize)
	}

	var flood slowFireRing
	for i := 0; i < 3*slowFireRingSize; i++ {
		flood.add(report.SlowFire{ReadMs: int64(i), Outcome: "hang-timeout"})
	}
	got = flood.snapshot()
	if len(got) != slowFirePinnedFailures+slowFireRingSize || got[slowFirePinnedFailures-1].ReadMs != slowFirePinnedFailures-1 {
		t.Fatalf("a failure flood keeps the first %d + the ring: got %d entries", slowFirePinnedFailures, len(got))
	}
}

// TestDriveTier1_PublishesDossierInputs: Tier 1 hands the orchestrator live
// accessors for the refapp's announced side listener and its stderr tail --
// the tail as of the call, not as of the last tick.
func TestDriveTier1_PublishesDossierInputs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	var addrFn atomic.Pointer[func() string]
	var tailFn atomic.Pointer[func() []string]
	cfg := tier1Config{
		Driver: remote.NewLocal("/bin/sh"),
		RefappArgs: []string{"-c", `echo "debug addr=127.0.0.1:41999"; echo "ready addr=` + srv.URL +
			`"; echo "[fault] hold path=/ws hold=8s"; sleep 120`},
		BaseURL:     srv.URL,
		Matrix:      minimalMatrix(t),
		Seed:        42,
		Concurrency: 1,
		OnDossierInputs: func(a func() string, tl func() []string) {
			addrFn.Store(&a)
			tailFn.Store(&tl)
		},
	}
	liveHasHold := func() bool {
		f := tailFn.Load()
		if f == nil {
			return false
		}
		for _, l := range (*f)() {
			if strings.Contains(l, "[fault] hold path=/ws") {
				return true
			}
		}
		return false
	}
	if _, err := runTier1Until(t, cfg, func(tier1TallySnapshot) bool { return liveHasHold() }); err != nil {
		t.Fatalf("driveTier1: %v", err)
	}
	if !liveHasHold() {
		t.Fatal("the live tail accessor never showed the post-ready line")
	}
	if f := addrFn.Load(); f == nil || (*f)() != "127.0.0.1:41999" {
		t.Fatal("the debug-addr accessor did not return the announced side listener")
	}
}
