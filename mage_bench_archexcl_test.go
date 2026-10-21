//go:build mage

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/loadgen"

	"github.com/goceleris/probatorium/report"
)

func TestResolveArchExclusion(t *testing.T) {
	for _, tc := range []struct {
		target, env, want string
		wantErr           bool
	}{
		{"both", "", "all", false}, // the default since the validation read of 2026-10-21
		{"both", "heavy", "heavy", false},
		{"both", "all", "all", false},
		{"both", "off", "off", false},
		{"both", "sometimes", "", true},
		{"msa2-server", "", "", false},
		{"msr1", "off", "", false},
		{"msr1", "heavy", "", false}, // ignored: a single-arch run has no peer
	} {
		t.Setenv("BENCH_ARCH_EXCLUSIVE", tc.env)
		got, err := resolveArchExclusion(tc.target)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("target %s env %q: got %q err %v, want %q err %v", tc.target, tc.env, got, err, tc.want, tc.wantErr)
		}
	}
}

// TestArchExclusionExtraVars: only a both-arch run with the lock on gets the
// lock vars, and only the arm64 pass under a locking policy (heavy, all) gets the reversed order.
func TestArchExclusionExtraVars(t *testing.T) {
	join := func(v []string) string { return strings.Join(v, " ") }
	x86 := join(archExclusionExtraVars("heavy", "msa2-server"))
	arm := join(archExclusionExtraVars("heavy", "msr1"))
	for _, want := range []string{"bench_client_lock=" + benchClientLockPath, "bench_client_lock_policy=heavy"} {
		if !strings.Contains(x86, want) || !strings.Contains(arm, want) {
			t.Errorf("both passes must carry %q:\n x86 %s\n arm %s", want, x86, arm)
		}
	}
	if strings.Contains(x86, "bench_cell_order") || !strings.Contains(arm, "bench_cell_order=reverse") {
		t.Errorf("only the arm64 pass reverses its cells:\n x86 %s\n arm %s", x86, arm)
	}
	if got := join(archExclusionExtraVars("all", "msr1")); !strings.Contains(got, "bench_cell_order=reverse") || !strings.Contains(got, "policy=all") {
		t.Errorf("policy all reverses arm64 too (cell positions match run A): %s", got)
	}
	if got := join(archExclusionExtraVars("all", "msa2-server")); strings.Contains(got, "bench_cell_order") {
		t.Errorf("x86 stays forward under all: %s", got)
	}
	if archExclusionExtraVars("off", "msr1") != nil || archExclusionExtraVars("", "msr1") != nil {
		t.Error("policy off and a single-arch run must add nothing to the playbook command")
	}
}

func TestLockWaitAllowance(t *testing.T) {
	w, d, r := 20*time.Second, 90*time.Second, 30*time.Second
	off := lockWaitAllowance("off", 4, w, d, r)
	heavy := lockWaitAllowance("heavy", 4, w, d, r)
	all := lockWaitAllowance("all", 4, w, d, r)
	if off != 0 || !(heavy > 0 && heavy < all) {
		t.Fatalf("allowance off %v heavy %v all %v, want 0 < heavy < all", off, heavy, all)
	}
}

// writeWindowedCell writes a runner per-cell JSON with the 5.22 load evidence.
func writeWindowedCell(t *testing.T, cellDir, scenario, server, class string, wait float64, wins []passWindowWire) {
	t.Helper()
	dir := filepath.Join(cellDir, "run0", scenario)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cell := map[string]any{
		"run_idx": 0, "scenario": scenario, "server": server, "status": "ok",
		"result": &loadgen.Result{Requests: 1000, Duration: 5 * time.Second, RequestsPerSec: 200,
			Latency: loadgen.Percentiles{P50: time.Millisecond, P99: 2 * time.Millisecond}},
		"started_at": wins[0].Start, "completed_at": wins[len(wins)-1].End,
		"load_class": class, "client_lock_wait_s": wait, "pass_windows": wins,
	}
	data, err := json.Marshal(cell)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, server+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cellDir, "results.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
}

var archT0 = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func pw(kind string, startS, endS int) passWindowWire {
	return passWindowWire{Kind: kind, Start: archT0.Add(time.Duration(startS) * time.Second), End: archT0.Add(time.Duration(endS) * time.Second)}
}

// writeArchRun writes a two-arch run: x86 runs ws-large-echo (heavy) in
// [0,160); arm64 runs it from armStart for 160 s. armStart >= 160 is a run that
// took turns; less is one whose arches overlapped.
func writeArchRun(t *testing.T, resultsDir string, armStart int) {
	t.Helper()
	x86 := filepath.Join(resultsDir, "20261020T120000-bench-msa2-server", "00-celeris-epoll-h1-sync")
	arm := filepath.Join(resultsDir, "20261020T120000-bench-msr1", "00-celeris-epoll-h1-sync")
	writeWindowedCell(t, x86, "ws-large-echo", "celeris-epoll-h1-sync", "heavy", 0,
		[]passWindowWire{pw("saturation", 0, 110), pw("rated", 110, 160)})
	writeWindowedCell(t, arm, "ws-large-echo", "celeris-epoll-h1-sync", "heavy", 7.5,
		[]passWindowWire{pw("saturation", armStart, armStart+110), pw("rated", armStart+110, armStart+160)})
}

func mergeBoth(t *testing.T, resultsDir, policy string) {
	t.Helper()
	if err := aggregatePerCellResults(resultsDir, 0); err != nil {
		t.Fatal(err)
	}
	bp := benchParams{CelerisVer: "v0.0.0-test", Duration: "5s", Warmup: "1s", Runs: "1", StartedAt: archT0}
	if _, err := mergeBothArchs(resultsDir, "both", policy, bp, [2]string{"msa2-server", "msr1"}); err != nil {
		t.Fatal(err)
	}
}

func readDoc(t *testing.T, path string) report.Document {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var d report.Document
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// TestBothArchRunStampsVerdictIntoBothDocuments: windows flow from the runner's
// per-cell JSON through raw/ into each document's load_windows, the verdict is
// stamped into BOTH documents (each is published alone), and the finished-run
// check agrees, clean when the arches took turns.
func TestBothArchRunStampsVerdictIntoBothDocuments(t *testing.T) {
	dir := t.TempDir()
	writeArchRun(t, dir, 160)
	mergeBoth(t, dir, "heavy")

	amd, arm := readDoc(t, filepath.Join(dir, "results-amd64.json")), readDoc(t, filepath.Join(dir, "results-arm64.json"))
	for name, d := range map[string]report.Document{"amd64": amd, "arm64": arm} {
		if d.SchemaVersion != report.SchemaVersion {
			t.Errorf("%s: schema %s", name, d.SchemaVersion)
		}
		if len(d.LoadWindows) != 2 {
			t.Errorf("%s: %d load windows, want 2 (saturation + rated)", name, len(d.LoadWindows))
		}
		ae := d.Environment.ArchExclusion
		if ae == nil || ae.Policy != "heavy" || ae.Violations != 0 || ae.Checked != 2 || ae.PeerChecked != 2 || ae.Problem() != "" {
			t.Errorf("%s: arch_exclusion = %+v", name, ae)
		}
	}
	if ae := arm.Environment.ArchExclusion; ae.PeerArch != "amd64" || ae.CellOrder != "reverse" || ae.LockWaitSeconds != 7.5 {
		t.Errorf("arm64 verdict = %+v, want peer amd64, order reverse, lock wait 7.5", ae)
	}
	if ae := amd.Environment.ArchExclusion; ae.PeerArch != "arm64" || ae.CellOrder != "forward" {
		t.Errorf("amd64 verdict = %+v, want peer arm64, order forward", ae)
	}
	if err := checkDataCompleteness(&amd); err != nil && strings.Contains(err.Error(), "arch exclusion") {
		t.Errorf("a clean run is refused by the gate: %v", err)
	}
	if rep, err := checkArchOverlapDir(dir, ""); err != nil || !rep.Clean() {
		t.Errorf("checkArchOverlapDir on a clean run: %v\n%s", err, rep)
	}
}

// TestBothArchRunThatOverlappedIsRefused: the arm64 heavy cell starts 100 s
// into the x86 one. The verdict records it, the finished-run check fails, and
// the publish gate refuses the document (BENCH_PUBLISH_FORCE overrides).
func TestBothArchRunThatOverlappedIsRefused(t *testing.T) {
	dir := t.TempDir()
	writeArchRun(t, dir, 100)
	mergeBoth(t, dir, "heavy")

	amd := readDoc(t, filepath.Join(dir, "results-amd64.json"))
	ae := amd.Environment.ArchExclusion
	if ae == nil || ae.Violations == 0 || ae.HeavyHeavyOverlaps == 0 {
		t.Fatalf("overlapping run: verdict %+v, want violations", ae)
	}
	err := checkDataCompleteness(&amd)
	if err == nil || !strings.Contains(err.Error(), "arch exclusion") {
		t.Fatalf("publish gate must refuse an overlapping both-arch run, got %v", err)
	}
	if rep, err := checkArchOverlapDir(dir, ""); err == nil || rep.Clean() {
		t.Fatalf("checkArchOverlapDir must fail on an overlapping run, got err %v\n%s", err, rep)
	}
	// the same windows are fine when nothing was meant to be excluded... but off is itself a refusal
	if rep, _ := checkArchOverlapDir(dir, "off"); rep.Total != 0 {
		t.Errorf("policy off forbids nothing, got %d", rep.Total)
	}
	t.Setenv("BENCH_PUBLISH_FORCE", "1")
	if err := checkDataCompleteness(&amd); err != nil && strings.Contains(err.Error(), "arch exclusion") {
		t.Errorf("BENCH_PUBLISH_FORCE=1 must bypass: %v", err)
	}
}

// TestPre522DocumentsCannotBeChecked: results with no load windows (every run
// before 5.22) fail the check with the reason, not pass it.
func TestPre522DocumentsCannotBeChecked(t *testing.T) {
	dir := t.TempDir()
	for _, a := range []string{"amd64", "arm64"} {
		doc := report.Document{SchemaVersion: "5.21"}
		raw, _ := json.Marshal(doc)
		if err := os.WriteFile(filepath.Join(dir, "results-"+a+".json"), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, err := checkArchOverlapDir(dir, "")
	if err == nil || !strings.Contains(err.Error(), "no load_windows") {
		t.Fatalf("err = %v, want the no-load_windows refusal", err)
	}
}

// TestLoadWindowsFromRawKeepsOrderAndClass.
func TestLoadWindowsFromRawKeepsOrderAndClass(t *testing.T) {
	dir := t.TempDir()
	writeArchRun(t, dir, 160)
	if err := aggregatePerCellResults(dir, 0); err != nil {
		t.Fatal(err)
	}
	w, wait, err := loadWindowsFromRaw(dir, "msr1")
	if err != nil || wait != 7.5 || len(w) != 2 {
		t.Fatalf("windows %v wait %v err %v", w, wait, err)
	}
	if w[0].Kind != report.LoadKindSaturation || w[1].Kind != report.LoadKindRated || w[0].Class != "heavy" ||
		!reflect.DeepEqual(w[0].End, w[1].Start) {
		t.Errorf("windows = %+v", w)
	}
}

// TestCellsWithoutPassWindowsAreNotVerifiedClean: an arch whose cells measured
// something but carry no pass_windows (a runner older than 5.22, a rebuilt
// column) must not read as clean for want of anything to compare.
func TestCellsWithoutPassWindowsAreNotVerifiedClean(t *testing.T) {
	dir := t.TempDir()
	x86 := filepath.Join(dir, "20261020T120000-bench-msa2-server", "00-celeris-epoll-h1-sync")
	writeWindowedCell(t, x86, "ws-large-echo", "celeris-epoll-h1-sync", "heavy", 0,
		[]passWindowWire{pw("saturation", 0, 110), pw("rated", 110, 160)})
	arm := filepath.Join(dir, "20261020T120000-bench-msr1", "00-celeris-epoll-h1-sync")
	writeRunnerCell(t, arm, "ws-large-echo", "celeris-epoll-h1-sync", "ok", "", &loadgen.Result{
		Requests: 1000, Duration: 5 * time.Second, RequestsPerSec: 200,
		Latency: loadgen.Percentiles{P50: time.Millisecond, P99: 2 * time.Millisecond}})
	if err := os.WriteFile(filepath.Join(arm, "results.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	mergeBoth(t, dir, "heavy")

	arm64 := readDoc(t, filepath.Join(dir, "results-arm64.json"))
	ae := arm64.Environment.ArchExclusion
	if ae == nil || ae.MissingTimestamps != 1 || ae.Problem() == "" {
		t.Fatalf("arm64 verdict = %+v, want one unchecked window and a problem", ae)
	}
	if err := checkDataCompleteness(&arm64); err == nil || !strings.Contains(err.Error(), "not verified") {
		t.Fatalf("the publish gate must refuse a run that checked nothing, got %v", err)
	}
	if rep, err := checkArchOverlapDir(dir, ""); err == nil || rep.Clean() {
		t.Fatalf("checkArchOverlapDir must not call it clean: %v\n%s", err, rep)
	}
}

// TestSurvivingArchIsNotRefusedForAPeerThatDroveNoLoad: the arm64 host was down
// for the whole run, so its only cell is a server-down DNF with no loadgen
// pass. The x86 document still publishes: a peer that drove no load cannot have
// contaminated it (probatorium#442).
func TestSurvivingArchIsNotRefusedForAPeerThatDroveNoLoad(t *testing.T) {
	dir := t.TempDir()
	x86 := filepath.Join(dir, "20261020T120000-bench-msa2-server", "00-celeris-epoll-h1-sync")
	writeWindowedCell(t, x86, "ws-large-echo", "celeris-epoll-h1-sync", "heavy", 0,
		[]passWindowWire{pw("saturation", 0, 110), pw("rated", 110, 160)})
	arm := filepath.Join(dir, "20261020T120000-bench-msr1", "00-celeris-epoll-h1-sync")
	writeRunnerCell(t, arm, "ws-large-echo", "celeris-epoll-h1-sync", "dnf", "server-down: pre-cell probe: connection refused", nil)
	if err := os.WriteFile(filepath.Join(arm, "results.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	mergeBoth(t, dir, "heavy")

	amd := readDoc(t, filepath.Join(dir, "results-amd64.json"))
	ae := amd.Environment.ArchExclusion
	if ae == nil || ae.Checked != 2 || ae.PeerChecked != 0 || ae.Problem() != "" {
		t.Fatalf("amd64 verdict = %+v, want 2 own windows, 0 peer windows, no problem", ae)
	}
	if err := checkDataCompleteness(&amd); err != nil && strings.Contains(err.Error(), "arch exclusion") {
		t.Fatalf("the surviving arch is refused for its peer's absence: %v", err)
	}
}
