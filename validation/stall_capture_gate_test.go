package validation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// probatorium#412 review: the celeris#588 stall capture -- in-stall
// dossiers, synchronous forensics on the serial incident loop, and a debug
// side listener in every refapp -- was switched on in EVERY validation run,
// while only fault-control runs had exercised it. It is now on in a
// fault-control run (PROBATORIUM_REFAPP_FAULT set) or with
// PROBATORIUM_STALL_CAPTURE=1, and nowhere else.

// A routine run asks no refapp for a side listener.
func TestRoutineRunAsksNoRefappForASideListener(t *testing.T) {
	t.Setenv("PROBATORIUM_REFAPP_FAULT", "")
	t.Setenv("PROBATORIUM_STALL_CAPTURE", "")
	t.Setenv(refappDebugAddrEnv, "")
	newTestOrch(t, "local")
	if got := os.Getenv(refappDebugAddrEnv); got != "" {
		t.Fatalf("routine run, local driver: %s=%q, want it left unset", refappDebugAddrEnv, got)
	}
}

// When Tier 1 returns its refapp is gone, and its side listener with it: a
// later dossier (Tier 3's, or one still queued) must not aim its pprof leg
// at the dead address and label the dump "side-listener" (CodeRabbit on
// probatorium#412, runner.go:1161).
func TestRunTierPropertyForgetsTheSideListenerWhenTier1Returns(t *testing.T) {
	cfg := Default()
	cfg.Duration = time.Second
	cfg.OutDir = t.TempDir()
	cfg.CelerisBin = "/this/path/does/not/exist/probatorium-test"
	cfg.MarkovPath = "markov/auth_session_ratelimit.yaml"
	o, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	violations := make(chan Incident, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	o.runTierProperty(ctx, violations)
	if inc := <-violations; inc.PredicateID != "T1-DRIVE" {
		t.Fatalf("premise: want Tier 1 to have run and failed its start (T1-DRIVE), got %q", inc.PredicateID)
	}
	if o.liveTail.Load() == nil {
		t.Fatal("premise: Tier 1 never published its dossier accessors, so there was nothing to forget")
	}
	if f := o.debugAddr.Load(); f != nil {
		t.Fatalf("after Tier 1 returned the side-listener accessor is still installed (returns %q)", (*f)())
	}
}

// Every dossier says WHEN its text goroutine dump was fetched
// (forensics_status.txt stacks_from/stacks_to): the #588 checker credits a
// dump to a hold only if it was fetched inside it, because a dossier's
// trigger instant and its dump instant can be seconds apart. The window
// brackets the refapp's handling of the request; a failed fetch has none.
func TestForensicsStampsTheGoroutineDumpFetchWindow(t *testing.T) {
	const serve = 60 * time.Millisecond
	var dumpedAt time.Time
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("debug") == "2" {
			time.Sleep(serve)
			mu.Lock()
			dumpedAt = time.Now()
			mu.Unlock()
			_, _ = w.Write([]byte("goroutine 1 [running]:"))
			return
		}
		_, _ = w.Write([]byte("pprof-bytes"))
	}))
	defer srv.Close()
	dir := t.TempDir()
	before := time.Now()
	if err := captureForensicsLiveOpts(context.Background(), dir, 0, strings.TrimPrefix(srv.URL, "http://"), forensicsOpts{SkipCore: true}); err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	from, to := statusWindow(t, dir)
	mu.Lock()
	at := dumpedAt
	mu.Unlock()
	if from.IsZero() || to.IsZero() {
		t.Fatal("forensics_status.txt carries no stacks_from/stacks_to")
	}
	if from.Before(before.Add(-time.Millisecond)) || to.After(after.Add(time.Millisecond)) || to.Sub(from) < serve {
		t.Errorf("window [%s, %s] does not lie inside the capture [%s, %s] or is shorter than the %s the dump took",
			from.Format(time.RFC3339Nano), to.Format(time.RFC3339Nano), before.Format(time.RFC3339Nano), after.Format(time.RFC3339Nano), serve)
	}
	if at.Before(from) || at.After(to) {
		t.Errorf("the refapp dumped at %s, outside the stamped window [%s, %s]", at.Format(time.RFC3339Nano), from.Format(time.RFC3339Nano), to.Format(time.RFC3339Nano))
	}

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	defer failing.Close()
	dir2 := t.TempDir()
	if err := captureForensicsLiveOpts(context.Background(), dir2, 0, strings.TrimPrefix(failing.URL, "http://"), forensicsOpts{SkipCore: true}); err != nil {
		t.Fatal(err)
	}
	if from, to := statusWindow(t, dir2); !from.IsZero() || !to.IsZero() {
		t.Errorf("a failed dump fetch was stamped [%v, %v]", from, to)
	}
}

func statusWindow(t *testing.T, dir string) (from, to time.Time) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "forensics_status.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range strings.Fields(string(b)) {
		k, v, _ := strings.Cut(f, "=")
		switch k {
		case "stacks_from":
			if from, err = time.Parse(time.RFC3339Nano, v); err != nil {
				t.Fatalf("stacks_from=%q: %v", v, err)
			}
		case "stacks_to":
			if to, err = time.Parse(time.RFC3339Nano, v); err != nil {
				t.Fatalf("stacks_to=%q: %v", v, err)
			}
		}
	}
	return from, to
}
