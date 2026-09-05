package validation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeLeakyRefapp stands in for a refapp whose recovery middleware has
// already counted a panic: /debug/vars reports panic_count=1 from the
// first poll, /debug/pprof/* serves recognisable bodies, /healthz is
// 200 and every other path is 404 -- a 200 to the adversarial walker's
// malformed bytes would trip I-ADV-ACCEPTED, a walker oracle that is
// (rightly) always a hard fail.
func fakeLeakyRefapp(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var pprofFetches atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/debug/vars":
			_, _ = w.Write([]byte(`{"goroutines": 40, "celeris.accepted_conn_total": 1, "celeris.closed_conn_total": 0,
				"celeris.active_conns": 1, "celeris.panic_count": 1, "memstats": {"HeapInuse": 4194304, "HeapAlloc": 3000000}}`))
		case strings.HasPrefix(r.URL.Path, "/debug/pprof/"):
			pprofFetches.Add(1)
			_, _ = w.Write([]byte("fake-profile:" + strings.TrimPrefix(r.URL.Path, "/debug/pprof/")))
		case r.URL.Path == "/healthz":
			w.WriteHeader(200)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &pprofFetches
}

// fakeRefappBinary writes an executable script the local driver forks
// as the refapp: it announces srv's address on the ready banner and
// then lives until signalled, so its pid stays valid for /proc reads
// and the forensics capture can be timed against the run teardown.
func fakeRefappBinary(t *testing.T, addr string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "refapp.sh")
	script := "#!/bin/sh\necho \"ready addr=" + addr + "\"\nexec sleep 120\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func incidentDirs(t *testing.T, outDir, predicate string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(outDir, "incidents", "*-"+predicate))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

func newLeakyOrchestrator(t *testing.T, hardFail bool, duration time.Duration) (*Orchestrator, *atomic.Int64) {
	t.Helper()
	srv, fetches := fakeLeakyRefapp(t)
	cfg := Default()
	cfg.OutDir = t.TempDir()
	cfg.Duration = duration
	cfg.CelerisBin = fakeRefappBinary(t, srv.Listener.Addr().String())
	cfg.CelerisListenAddr = srv.Listener.Addr().String()
	cfg.MarkovPath = "markov/auth_session_ratelimit.yaml"
	cfg.PropertyTier = "core"
	cfg.PropertyHardFail = hardFail
	o, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return o, fetches
}

// A hard-fail property violation must produce the forensics the
// dossier promises. Before this ordering the orchestrator cancelled the
// run (SIGKILLing the refapp) and only then captured forensics on the
// already-cancelled context, so every I-* incident was incident.json
// plus .missing markers -- no goroutine profile for the leak class the
// pprof mount exists for.
func TestRun_HardFailCapturesForensicsBeforeTeardown(t *testing.T) {
	o, fetches := newLeakyOrchestrator(t, true, 20*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	started := time.Now()
	err := o.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "I-PANIC") {
		t.Fatalf("hard fail must surface as the I-PANIC error, got %v", err)
	}
	if time.Since(started) > 20*time.Second {
		t.Fatalf("a hard fail must end the cell early, took %s", time.Since(started))
	}
	dirs := incidentDirs(t, o.cfg.OutDir, "I-PANIC")
	if len(dirs) != 1 {
		t.Fatalf("want exactly one I-PANIC dossier, got %v", dirs)
	}
	dir := dirs[0]
	raw, err := os.ReadFile(filepath.Join(dir, "incident.json"))
	if err != nil {
		t.Fatalf("incident.json: %v", err)
	}
	var dossier map[string]any
	if err := json.Unmarshal(raw, &dossier); err != nil {
		t.Fatal(err)
	}
	if dossier["predicate"] != "I-PANIC" || dossier["record_only"] != false {
		t.Fatalf("dossier: %v", dossier)
	}
	for _, name := range []string{"goroutine.pprof", "heap.pprof"} {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s missing from the dossier (forensics ran after the refapp was torn down?): %v", name, err)
		}
		if want := "fake-profile:" + strings.TrimSuffix(name, ".pprof"); string(body) != want {
			t.Fatalf("%s = %q want %q", name, body, want)
		}
		if _, err := os.Stat(filepath.Join(dir, name+".missing")); err == nil {
			t.Fatalf("%s.missing marker written although the profile was fetched", name)
		}
	}
	if fetches.Load() < 5 {
		t.Fatalf("every pprof profile must be fetched while the refapp is alive, got %d fetches", fetches.Load())
	}
	if _, err := os.Stat(filepath.Join(dir, "forensics_status.txt")); err != nil {
		t.Fatalf("forensics_status.txt: %v", err)
	}
	// The cell document still carries the violation for the gate.
	doc, err := os.ReadFile(filepath.Join(o.cfg.OutDir, "validate-results.json"))
	if err != nil {
		t.Fatalf("validate-results.json: %v", err)
	}
	if !strings.Contains(string(doc), `"property_violation_ids": [`) || !strings.Contains(string(doc), `"I-PANIC"`) {
		t.Fatalf("document lacks the violation:\n%s", doc)
	}
}

// Record-only (the default): the first violation of a predicate writes
// a dossier with live forensics, the cell keeps running to its budget,
// every further violation is counted, and Run returns nil -- the gate,
// not the run, fails on tier_1.property_violations.
func TestRun_RecordOnlyKeepsRunningAndCountsEveryViolation(t *testing.T) {
	o, fetches := newLeakyOrchestrator(t, false, 4*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := o.Run(ctx); err != nil {
		t.Fatalf("record-only run must complete: %v", err)
	}
	dirs := incidentDirs(t, o.cfg.OutDir, "I-PANIC")
	if len(dirs) != 1 {
		t.Fatalf("want exactly one I-PANIC dossier (first violation only), got %v", dirs)
	}
	raw, err := os.ReadFile(filepath.Join(dirs[0], "incident.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"record_only": true`) {
		t.Fatalf("dossier must be marked record-only:\n%s", raw)
	}
	if _, err := os.Stat(filepath.Join(dirs[0], "goroutine.pprof")); err != nil {
		t.Fatalf("record-only incidents still capture live forensics: %v", err)
	}
	if fetches.Load() < 5 {
		t.Fatalf("pprof fetches=%d", fetches.Load())
	}
	res := o.Result()
	if !res.Tier1Ran {
		t.Fatal("tier 1 did not complete")
	}
	p := res.Properties
	if p.Violations < 2 || p.Failed() != 1 || p.ViolationIDs[0] != "I-PANIC" {
		t.Fatalf("every sample's violation must be counted while the cell keeps running: %+v", p)
	}
	if p.Samples < 2 {
		t.Fatalf("the loop must keep polling after a record-only violation: samples=%d", p.Samples)
	}
	sum := res.Tier1.Tier1Summary()
	if sum.PropertyViolations != p.Violations || sum.PropertyLoopSkipped != "" {
		t.Fatalf("summary: %+v", sum)
	}
}
