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
)

// probatorium#412 review round 3 (nit): the text goroutine dump
// (goroutine?debug=2) is runtime.Stack(all) -- the refapp's world stops for
// the whole traceback -- and it ran in EVERY dossier, including the
// record-only dossiers of routine runs: cells that keep running, where the
// stall capture is off because it was never measured on a routine-shaped
// run. It is now taken in a stall-capture run (fault control, or
// PROBATORIUM_STALL_CAPTURE=1) and on a hard fail, and nowhere else.
func TestRecordOnlyDossierOfARoutineRunTakesNoTextDump(t *testing.T) {
	var mu sync.Mutex
	var textDumps int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("debug") == "2" {
			mu.Lock()
			textDumps++
			mu.Unlock()
			_, _ = w.Write([]byte("goroutine 1 [running]:\n"))
			return
		}
		_, _ = w.Write([]byte("pprof-bytes"))
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	for _, tc := range []struct {
		name        string
		fault, knob string
		recordOnly  bool
		wantText    bool
	}{
		{"routine run, record-only", "", "", true, false},
		{"routine run, hard fail", "", "", false, true},
		{"stall capture on, record-only", "", "1", true, true},
		{"fault control, record-only", "/ws:8s@30s", "", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(refappFaultEnv, tc.fault)
			t.Setenv(stallCaptureEnv, tc.knob)
			mu.Lock()
			textDumps = 0
			mu.Unlock()
			o := &Orchestrator{cfg: Config{CelerisListenAddr: addr}}
			dir := t.TempDir()
			inc := Incident{PredicateID: "I-H2C-HANG", RecordOnly: tc.recordOnly, SkipCore: true}
			if err := o.captureForensics(context.Background(), dir, inc); err != nil {
				t.Fatalf("captureForensics: %v", err)
			}
			mu.Lock()
			got := textDumps
			mu.Unlock()
			_, statErr := os.Stat(filepath.Join(dir, "goroutine-stacks.txt"))
			_, skipErr := os.Stat(filepath.Join(dir, "goroutine-stacks.txt.skipped"))
			if _, err := os.Stat(filepath.Join(dir, "goroutine.pprof")); err != nil {
				t.Errorf("the proto goroutine profile is missing: %v", err)
			}
			if tc.wantText {
				if got != 1 || statErr != nil {
					t.Errorf("want one text goroutine dump on disk, got %d request(s), stat: %v", got, statErr)
				}
				return
			}
			if got != 0 {
				t.Errorf("a record-only dossier of a routine run fetched goroutine?debug=2 %d time(s): runtime.Stack(all) stops the world of a cell that keeps running", got)
			}
			if statErr == nil || skipErr != nil {
				t.Errorf("want goroutine-stacks.txt absent and a goroutine-stacks.txt.skipped marker (stat: %v, marker: %v)", statErr, skipErr)
			}
		})
	}
}
