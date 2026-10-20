package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/probatorium/interleave"
	"github.com/goceleris/probatorium/report"
	"github.com/goceleris/probatorium/scenarios"
	"github.com/goceleris/probatorium/servers"
)

// runTwoArchCells runs one heavy cell on each of two "arch" runners at the same
// moment against a loopback server, as the two arch passes of a both-arch run
// do on msa2-client, with the given client-lock policy ("" = no lock). It
// returns each cell's per-cell JSON.
func runTwoArchCells(t *testing.T, policy string) [2]cellResultFile {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"message":"ok"}`))
	}))
	defer srv.Close()
	lockPath := filepath.Join(t.TempDir(), "client.lock")
	server := &adapterServer{adapter: servers.Adapter{Name: "e2e-remote", Category: "remote"}, features: remoteFeatureSet()}

	var out [2]cellResultFile
	var wg sync.WaitGroup
	for i := range out {
		args := []string{"-rated", "-rated-duration", "150ms", "-rated-fractions", "0.5", "-rated-cells", "*",
			"-target", srv.URL, "-server-name", "e2e-remote", "-duration", "300ms", "-warmup", "100ms",
			"-out", t.TempDir()}
		if policy != "" {
			args = append(args, "-client-lock", lockPath, "-client-lock-policy", policy)
		}
		cfg, err := ParseArgs(args, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		lock, err := newClientLock(cfg.ClientLock, cfg.ClientLockPolicy, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		lock.poll = 10 * time.Millisecond
		cfg.lock = lock
		wg.Add(1)
		go func() {
			defer wg.Done()
			cell := interleave.Cell{Scenario: scenarioByName(t, "get-json"), Server: server}
			if oc, err := executeCell(context.Background(), cfg, cell); err != nil || oc.Status != report.CellOK {
				t.Errorf("executeCell: status %q err %v", oc.Status, err)
				return
			}
			raw, err := os.ReadFile(filepath.Join(cfg.Out, "run0", "get-json", "e2e-remote.json"))
			if err != nil {
				t.Error(err)
				return
			}
			if err := json.Unmarshal(raw, &out[i]); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	return out
}

func windowsOf(cf cellResultFile, comp string) []report.LoadWindow {
	var w []report.LoadWindow
	for _, p := range cf.PassWindows {
		w = append(w, report.LoadWindow{Competitor: comp, Scenario: cf.ScenarioName, Kind: p.Kind,
			TargetRPS: p.TargetRPS, Class: cf.LoadClass, Start: p.Start, End: p.End})
	}
	return w
}

// TestTwoArchRunnersNeverOverlapOnAHeavyCell is probatorium#442 end to end at
// the runner: two runners whose heavy cells start at the same moment. With no
// lock the passes overlap (the control, so the test cannot pass vacuously);
// with the lock they never do, the second cell's started_at is stamped after
// its wait, and every pass of the cell, saturation and rated, was recorded.
func TestTwoArchRunnersNeverOverlapOnAHeavyCell(t *testing.T) {
	free := runTwoArchCells(t, "")
	rep := report.CheckArchOverlap(windowsOf(free[0], "a"), windowsOf(free[1], "b"), report.ExclusionHeavy)
	if rep.Total == 0 {
		t.Fatalf("control: two unlocked heavy cells started together never overlapped; the test proves nothing\n%s", rep)
	}

	locked := runTwoArchCells(t, report.ExclusionHeavy)
	for i, cf := range locked {
		if cf.LoadClass != "heavy" {
			t.Errorf("cell %d load_class = %q, want heavy", i, cf.LoadClass)
		}
		var sat, rated int
		for _, w := range cf.PassWindows {
			switch w.Kind {
			case report.LoadKindSaturation:
				sat++
			case report.LoadKindRated:
				rated++
			}
			if !w.End.After(w.Start) {
				t.Errorf("cell %d: pass window %+v is empty", i, w)
			}
		}
		if sat != 1 || rated != 1 {
			t.Errorf("cell %d: %d saturation and %d rated windows, want 1 and 1", i, sat, rated)
		}
	}
	rep = report.CheckArchOverlap(windowsOf(locked[0], "a"), windowsOf(locked[1], "b"), report.ExclusionHeavy)
	if !rep.Clean() {
		t.Fatalf("locked heavy cells overlap:\n%s", rep)
	}
	// One of the two waited for the other; its started_at follows the wait.
	first, second := locked[0], locked[1]
	if second.StartedAt.Before(first.StartedAt) {
		first, second = second, first
	}
	if second.ClientLockWaitS <= 0 {
		t.Errorf("the second cell reports no lock wait (client_lock_wait_s = %v)", second.ClientLockWaitS)
	}
	if second.StartedAt.Before(first.CompletedAt) {
		t.Errorf("second cell started_at %v precedes the first cell's completed_at %v: the wait is inside the cell window",
			second.StartedAt, first.CompletedAt)
	}
	if !strings.Contains(first.ScenarioName, "get-json") {
		t.Errorf("unexpected scenario %q", first.ScenarioName)
	}
}

func TestClientLockAndCellOrderFlags(t *testing.T) {
	cfg, err := ParseArgs([]string{"-client-lock", "/tmp/x.lock", "-client-lock-policy", "all", "-cell-order", "reverse"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClientLock != "/tmp/x.lock" || cfg.ClientLockPolicy != "all" || cfg.CellOrder != "reverse" {
		t.Fatalf("flags not parsed: %+v", cfg)
	}
	if err := run(Config{Runs: 1, Services: "none", DryRun: true, Cells: "get-json/*", CellOrder: "sideways"}); err == nil ||
		!strings.Contains(err.Error(), "-cell-order") {
		t.Errorf("a bad -cell-order must fail the run, got %v", err)
	}
	if err := run(Config{Runs: 1, Services: "none", DryRun: true, Cells: "get-json/*", ClientLock: "/tmp/x.lock"}); err == nil ||
		!strings.Contains(err.Error(), "remote-target") {
		t.Errorf("-client-lock without -target must fail, got %v", err)
	}
}

// TestCellOrderReverseWalksTheScheduleBackwards: the arm64 pass of a both-arch
// run must walk the same cells as x86 in the opposite order, and nothing else
// may change (probatorium#442).
func TestCellOrderReverseWalksTheScheduleBackwards(t *testing.T) {
	grid := realizedGrid(t)
	scs, advs, err := filterCells(scenarios.Registry(), servers.AdaptersSorted(), "*/celeris-std-h1")
	if err != nil {
		t.Fatal(err)
	}
	var srvs []servers.Server
	for _, a := range advs {
		srvs = append(srvs, &adapterServer{adapter: a, features: featureSetFor(a, false)})
	}
	fwd := interleave.Schedule(1, scs, srvs)
	if len(fwd) < 20 || len(grid) == 0 {
		t.Fatalf("schedule has %d cells", len(fwd))
	}
	same, err := orderSchedule(fwd, "forward")
	if err != nil || !slices.Equal(ids(same), ids(fwd)) {
		t.Fatalf("forward must keep the order: %v", err)
	}
	rev, err := orderSchedule(fwd, "reverse")
	if err != nil {
		t.Fatal(err)
	}
	want := ids(fwd)
	slices.Reverse(want)
	if !slices.Equal(ids(rev), want) {
		t.Fatalf("reverse order = %v, want %v", ids(rev), want)
	}
	if _, err := orderSchedule(fwd, "sideways"); err == nil {
		t.Fatal("an unknown order must be refused")
	}
}

func ids(cells []interleave.Cell) []string {
	var out []string
	for _, c := range cells {
		out = append(out, c.Scenario.Name()+"/"+c.Server.Name())
	}
	return out
}

// TestLockErrorIsNotAnInterruption: a lock that cannot be opened fails the cell
// with its own message; only a cancelled wait is "interrupted:", which the merge
// treats as harness-side and never demotes the cell for (probatorium#442).
func TestLockErrorIsNotAnInterruption(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	defer srv.Close()
	cfg, err := ParseArgs([]string{"-target", srv.URL, "-server-name", "e2e-remote", "-duration", "200ms", "-warmup", "50ms",
		"-out", t.TempDir()}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	// a lock file in a directory that does not exist cannot be opened
	cfg.lock = testLock(t, filepath.Join(t.TempDir(), "no-such-dir", "client.lock"), "")
	server := &adapterServer{adapter: servers.Adapter{Name: "e2e-remote", Category: "remote"}, features: remoteFeatureSet()}
	oc, err := executeCell(context.Background(), cfg, interleave.Cell{Scenario: scenarioByName(t, "get-json"), Server: server})
	if err == nil {
		t.Fatal("executeCell must fail when the lock cannot be opened")
	}
	if strings.HasPrefix(oc.ErrorMsg, "interrupted:") || !strings.Contains(oc.ErrorMsg, "client-lock:") {
		t.Fatalf("error %q: want a client-lock failure, not an interruption", oc.ErrorMsg)
	}
	// a cancelled wait IS an interruption
	cfg.lock = testLock(t, filepath.Join(t.TempDir(), "c.lock"), "")
	hold, _, _ := cfg.lock.acquire(context.Background(), "get-json")
	defer hold()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	oc, err = executeCell(ctx, cfg, interleave.Cell{Scenario: scenarioByName(t, "get-json"), Server: server})
	if err == nil || !strings.HasPrefix(oc.ErrorMsg, "interrupted:") {
		t.Fatalf("cancelled wait: err %v error %q, want interrupted:", err, oc.ErrorMsg)
	}
}
