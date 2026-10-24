package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/loadgen"

	"github.com/goceleris/probatorium/budget"
	"github.com/goceleris/probatorium/interleave"
	"github.com/goceleris/probatorium/report"
	"github.com/goceleris/probatorium/servers"
)

// probatorium#441: the runner kept a rated pass as {target_rps, p99} and
// dropped a failed one with a stderr line, so the artifact could not say how
// much load loadgen delivered, how many requests failed, or that a pass was
// missing at all. These tests drive the real executeCell (the cluster column's
// path, a loopback target) with a scripted loadgen in place of each rated pass
// and read the per-cell JSON the cluster merge ingests. They decode it as
// generic JSON on purpose: the keys are the contract, not the Go type.

// scriptedBench is a rated pass whose outcome the test chose.
type scriptedBench struct {
	res    *loadgen.Result
	err    error
	before func() // runs inside Run, before it returns (to cancel the parent)
}

func (s scriptedBench) Run(context.Context) (*loadgen.Result, error) {
	if s.before != nil {
		s.before()
	}
	return s.res, s.err
}

// withRatedScript replaces the rated-pass loadgen for the test. build gets the
// 0-based pass index and the loadgen.Config of that pass (its Rate is the
// pass's target) and returns the fake, or an error for a pass that cannot be
// constructed.
func withRatedScript(t *testing.T, build func(i int, cfg loadgen.Config) (ratedBench, error)) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	orig := newRatedBench
	newRatedBench = func(cfg loadgen.Config) (ratedBench, error) {
		return build(int(calls.Add(1))-1, cfg)
	}
	t.Cleanup(func() { newRatedBench = orig })
	return &calls
}

// ratedCellJSON runs get-json (a rated row) through executeCell against srv
// with the given rated fractions and returns the cell's per-cell JSON, decoded
// generically, and the context cancel the test may use as the parent.
func ratedCellJSON(t *testing.T, srv *httptest.Server, fractions string, parent context.Context) map[string]any {
	t.Helper()
	cfg, err := ParseArgs(append(ratedArgs(budget.Full()),
		"-target", srv.URL, "-server-name", "e2e-remote",
		"-duration", "300ms", "-warmup", "100ms", "-rated-duration", "150ms",
		"-rated-fractions", fractions, "-out", t.TempDir()), io.Discard)
	if err != nil {
		t.Fatalf("ParseArgs: %v", err)
	}
	server := &adapterServer{adapter: servers.Adapter{Name: "e2e-remote", Category: "remote"}, features: remoteFeatureSet()}
	cell := interleave.Cell{Scenario: scenarioByName(t, "get-json"), Server: server}
	if _, err := executeCell(parent, cfg, cell); err != nil {
		t.Fatalf("executeCell: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(cfg.Out, "run0", "get-json", "e2e-remote.json"))
	if err != nil {
		t.Fatalf("per-cell JSON: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode per-cell JSON: %v", err)
	}
	return m
}

func entries(t *testing.T, m map[string]any, key string) []map[string]any {
	t.Helper()
	arr, _ := m[key].([]any)
	out := make([]map[string]any, 0, len(arr))
	for _, e := range arr {
		em, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("%s entry %v is not an object", key, e)
		}
		out = append(out, em)
	}
	return out
}

func num(t *testing.T, e map[string]any, key string) float64 {
	t.Helper()
	v, ok := e[key].(float64)
	if !ok {
		t.Fatalf("entry %v: no numeric %q", e, key)
	}
	return v
}

func str(e map[string]any, key string) string { s, _ := e[key].(string); return s }

// TestEveryRatedPassIsRecordedWithItsRateAndErrors: one cell, four fractions,
// four outcomes. The kept pass carries what loadgen delivered (achieved rate,
// requests, errors, connect errors); each pass main dropped is recorded with a
// status and a reason; none is absent. A pass that is not kept stays OUT of
// rated_passes, so a reader that predates this change (it reads target_rps and
// p99 and nothing else) never counts a failed pass as a perfect one.
func TestEveryRatedPassIsRecordedWithItsRateAndErrors(t *testing.T) {
	dur := 150 * time.Millisecond
	withRatedScript(t, func(i int, cfg loadgen.Config) (ratedBench, error) {
		switch i {
		case 0: // delivered 95% of the target, a few errors
			rps := 0.95 * cfg.Rate
			return scriptedBench{res: &loadgen.Result{
				Requests: int64(rps * dur.Seconds()), Errors: 7, ConnectErrors: 2, DialRetries: 1,
				Duration: dur, RequestsPerSec: rps, LoadgenVersion: "v1.4.14",
				Latency: loadgen.Percentiles{P50: time.Millisecond, P99: 9 * time.Millisecond, P999: 12 * time.Millisecond, Max: 20 * time.Millisecond},
			}}, nil
		case 1: // the pass cannot even be built (the lithium cell, #424)
			return nil, errors.New("connect: connection refused")
		case 2: // loadgen ran and sent nothing: the pg store failing mid-cell (#476)
			return scriptedBench{res: &loadgen.Result{Duration: dur}}, nil
		default: // loadgen failed after sending part of the load
			return scriptedBench{
				res: &loadgen.Result{Requests: 50, Errors: 40, Duration: dur, RequestsPerSec: 333,
					Latency: loadgen.Percentiles{P99: 5 * time.Second}},
				err: errors.New("pass aborted"),
			}, nil
		}
	})
	m := ratedCellJSON(t, okServer(t), "0.25,0.5,0.75,0.9", context.Background())
	sat := num(t, m, "saturation_mode_rps")

	kept := entries(t, m, "rated_passes")
	notKept := entries(t, m, "rated_passes_not_kept")
	if len(kept)+len(notKept) != 4 {
		t.Fatalf("4 rated fractions ran: %d kept + %d not kept, want 4 recorded passes", len(kept), len(notKept))
	}

	// The kept pass.
	if len(kept) != 1 {
		t.Fatalf("rated_passes has %d entries, want only the delivered pass", len(kept))
	}
	k := kept[0]
	target := 0.25 * sat
	if got := num(t, k, "target_rps"); math.Abs(got-target) > 1 {
		t.Errorf("target_rps = %v, want %v (0.25 of the saturation rate)", got, target)
	}
	if num(t, k, "p99") != float64(9*time.Millisecond) {
		t.Errorf("p99 = %v, want 9ms in ns (the key old readers use)", k["p99"])
	}
	if str(k, "status") != "ok" {
		t.Errorf("status = %q, want ok", str(k, "status"))
	}
	if got := num(t, k, "fraction"); got != 0.25 {
		t.Errorf("fraction = %v, want 0.25", got)
	}
	if got := num(t, k, "achieved_rps"); math.Abs(got-0.95*target) > 1 {
		t.Errorf("achieved_rps = %v, want %v", got, 0.95*target)
	}
	if got := num(t, k, "achieved_ratio"); math.Abs(got-0.95) > 0.001 {
		t.Errorf("achieved_ratio = %v, want 0.95", got)
	}
	if num(t, k, "errors") != 7 || num(t, k, "connect_errors") != 2 || num(t, k, "dial_retries") != 1 {
		t.Errorf("errors/connect_errors/dial_retries = %v/%v/%v, want 7/2/1", k["errors"], k["connect_errors"], k["dial_retries"])
	}
	if num(t, k, "requests") < 1 || num(t, k, "duration") != float64(dur) {
		t.Errorf("requests/duration = %v/%v", k["requests"], k["duration"])
	}
	// target x duration - requests - errors: the load that never went out.
	wantShort := target*dur.Seconds() - num(t, k, "requests") - 7
	if got := num(t, k, "shortfall_requests"); math.Abs(got-wantShort) > 1 {
		t.Errorf("shortfall_requests = %v, want %v", got, wantShort)
	}
	if str(k, "start") == "" || str(k, "end") == "" || str(k, "loadgen_version") != "v1.4.14" {
		t.Errorf("start/end/loadgen_version = %v/%v/%v, want the pass's own window and the loadgen build", k["start"], k["end"], k["loadgen_version"])
	}
	if num(t, k, "p50") != float64(time.Millisecond) || num(t, k, "p99_9") != float64(12*time.Millisecond) || num(t, k, "max") != float64(20*time.Millisecond) {
		t.Errorf("p50/p99_9/max = %v/%v/%v", k["p50"], k["p99_9"], k["max"])
	}

	// The three dropped passes, in fraction order.
	want := []struct {
		fraction       float64
		status, reason string
		requests       float64
	}{
		{0.5, "not_run", "loadgen_new", 0},
		{0.75, "skipped", "zero_requests", 0},
		{0.9, "skipped", "run_error", 50},
	}
	if len(notKept) != len(want) {
		t.Fatalf("rated_passes_not_kept has %d entries, want %d", len(notKept), len(want))
	}
	for i, w := range want {
		e := notKept[i]
		if num(t, e, "fraction") != w.fraction || str(e, "status") != w.status || str(e, "reason") != w.reason {
			t.Errorf("not-kept[%d] = fraction %v status %q reason %q, want %v %q %q",
				i, e["fraction"], str(e, "status"), str(e, "reason"), w.fraction, w.status, w.reason)
		}
		if got := num(t, e, "target_rps"); math.Abs(got-w.fraction*sat) > 1 {
			t.Errorf("not-kept[%d] target_rps = %v, want %v", i, got, w.fraction*sat)
		}
		if got, _ := e["requests"].(float64); got != w.requests {
			t.Errorf("not-kept[%d] requests = %v, want %v (what loadgen reported before it failed)", i, got, w.requests)
		}
	}
	if d := str(notKept[0], "detail"); d != "connect: connection refused" {
		t.Errorf("loadgen_new detail = %q, want the construction error", d)
	}
	if d := str(notKept[2], "detail"); d != "pass aborted" {
		t.Errorf("run_error detail = %q, want the run error", d)
	}
	if got, _ := notKept[2]["errors"].(float64); got != 40 {
		t.Errorf("run_error errors = %v, want the partial 40", got)
	}
}

// TestInterruptedRatedPassIsNamedAsSuch: a pass cut short by the parent
// context is an interruption, not a server failure, and the record says so.
func TestInterruptedRatedPassIsNamedAsSuch(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	withRatedScript(t, func(int, loadgen.Config) (ratedBench, error) {
		return scriptedBench{before: cancel, err: context.Canceled}, nil
	})
	m := ratedCellJSON(t, okServer(t), "0.5,0.9", parent)
	notKept := entries(t, m, "rated_passes_not_kept")
	if len(entries(t, m, "rated_passes")) != 0 || len(notKept) != 2 {
		t.Fatalf("kept %d, not kept %d, want 0 kept and both fractions recorded", len(entries(t, m, "rated_passes")), len(notKept))
	}
	for i, e := range notKept {
		if str(e, "status") != "skipped" || str(e, "reason") != "interrupted" {
			t.Errorf("not-kept[%d] = %q/%q, want skipped/interrupted", i, str(e, "status"), str(e, "reason"))
		}
	}
}

// TestFlaggedCellRecordsItsRatedPassesAsNotRun: a cell in the rated scope
// whose saturation pass came back flagged (suspect) skips the sweep. Each
// fraction it would have run is recorded as not_run with the reason, so the cell
// does not read as "rated, and nothing to report".
func TestFlaggedCellRecordsItsRatedPassesAsNotRun(t *testing.T) {
	calls := withRatedScript(t, func(int, loadgen.Config) (ratedBench, error) {
		t.Error("no rated pass may start on a flagged cell")
		return scriptedBench{}, nil
	})
	var n atomic.Int64 // every other answer is a 500: far past the error budget, still a measurement
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1)%2 == 0 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"message":"ok"}`))
	}))
	defer failing.Close()
	m := ratedCellJSON(t, failing, "0.25,0.5", context.Background())
	if calls.Load() != 0 {
		t.Fatalf("%d rated passes started on a flagged cell", calls.Load())
	}
	if st := str(m, "status"); st != "suspect" {
		t.Fatalf("cell status = %q, want suspect (the premise of this test)", st)
	}
	notKept := entries(t, m, "rated_passes_not_kept")
	if len(notKept) != 2 || len(entries(t, m, "rated_passes")) != 0 {
		t.Fatalf("not kept %d, kept %d, want both fractions recorded as not run", len(notKept), len(entries(t, m, "rated_passes")))
	}
	for i, e := range notKept {
		if str(e, "status") != "not_run" || str(e, "reason") != "cell_flagged" {
			t.Errorf("not-kept[%d] = %q/%q, want not_run/cell_flagged", i, str(e, "status"), str(e, "reason"))
		}
		if num(t, e, "fraction") != []float64{0.25, 0.5}[i] {
			t.Errorf("not-kept[%d] fraction = %v", i, e["fraction"])
		}
		if str(e, "detail") == "" {
			t.Errorf("not-kept[%d] has no detail: the reason must carry the cell's flag", i)
		}
	}
}

// TestResultsSinkKeepsEveryRatedPassInProcess: the in-process runner (no
// cluster merge) builds its own results.json from the sink. A run's passes, kept
// and not, reach the document's rated_passes; the lost one is listed and does
// not enter latency_at_slo.
func TestResultsSinkKeepsEveryRatedPassInProcess(t *testing.T) {
	cfg := Config{Out: t.TempDir()}
	sink := newResultsSink(cfg, time.Now().UTC())
	cell := sinkCell(t, "get-json", "gin-h1")
	sink.recordRun(cell, cellOutcome{
		Result: &loadgen.Result{Requests: 100, RequestsPerSec: 400000, Duration: time.Second},
		RatedSamples: []report.RatedSample{
			{TargetRPS: 100000, P99: 8 * time.Millisecond},
			{TargetRPS: 200000, P99: 30 * time.Millisecond},
		},
		RatedPasses: []report.RatedPassRecord{
			{Fraction: 0.25, TargetRPS: 100000, P99: 8 * time.Millisecond, Status: report.RatedPassOK, AchievedRPS: 99000},
			{Fraction: 0.5, TargetRPS: 200000, P99: 30 * time.Millisecond, Status: report.RatedPassOK, AchievedRPS: 190000},
			{Fraction: 0.9, TargetRPS: 360000, Status: report.RatedPassSkipped, Reason: report.RatedReasonZeroRequests},
		},
	}, true)
	if err := sink.flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(cfg.Out, "results.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Benchmarks []struct {
			RatedPasses  map[string][]map[string]any `json:"rated_passes"`
			Headline     map[string]float64          `json:"rated_headline_target_rps"`
			LatencyAtSLO map[string]map[string]int   `json:"latency_at_slo"`
		} `json:"benchmarks"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Benchmarks) != 1 {
		t.Fatalf("%d benchmark rows, want 1", len(doc.Benchmarks))
	}
	b := doc.Benchmarks[0]
	got := b.RatedPasses["get-json"]
	if len(got) != 3 || got[2]["status"] != "skipped" || got[2]["reason"] != "zero_requests" || got[0]["achieved_rps"] != float64(99000) {
		t.Errorf("rated_passes = %v, want the 3 passes with the lost one listed", got)
	}
	if b.Headline["get-json"] != 200000 {
		t.Errorf("rated_headline_target_rps = %v, want 200000", b.Headline)
	}
	for ms, rps := range b.LatencyAtSLO["get-json"] {
		if rps == 360000 {
			t.Errorf("latency_at_slo[%s] counts the skipped pass", ms)
		}
	}
}

// TestRatedSweepRecordsANonPositiveTarget: a fraction whose target is not
// positive leaves a not_run record instead of vanishing from the sweep.
func TestRatedSweepRecordsANonPositiveTarget(t *testing.T) {
	withRatedScript(t, func(_ int, cfg loadgen.Config) (ratedBench, error) {
		return scriptedBench{res: &loadgen.Result{Requests: 10, Duration: time.Second, RequestsPerSec: cfg.Rate,
			Latency: loadgen.Percentiles{P99: time.Millisecond}}}, nil
	})
	sw := runRatedSweep(context.Background(), Config{RatedFractions: []float64{0, 0.5}, RatedDuration: time.Second, Warmup: time.Second},
		loadgen.Config{}, 1000)
	if len(sw.all) != 2 {
		t.Fatalf("%d records for 2 fractions, want 2", len(sw.all))
	}
	if r := sw.all[0]; r.Status != report.RatedPassNotRun || r.Reason != report.RatedReasonBadTarget || r.Fraction != 0 {
		t.Errorf("fraction 0 = %+v, want not_run/non_positive_target", r)
	}
	if r := sw.all[1]; r.Status != report.RatedPassOK || len(sw.kept) != 1 || len(sw.notKept) != 1 || len(sw.samples) != 1 {
		t.Errorf("fraction 0.5 = %+v, kept %d notKept %d samples %d", r, len(sw.kept), len(sw.notKept), len(sw.samples))
	}
}

// TestNotRunRatedPassesCoversEveryFraction: the record of a sweep that did not
// start has one not_run pass per configured fraction, with the reason and the
// detail. (The no_saturation_rate branch of executeCell is not reachable from a
// clean saturation pass, so this is where its reason is pinned.)
func TestNotRunRatedPassesCoversEveryFraction(t *testing.T) {
	got := notRunRatedPasses(Config{RatedFractions: []float64{0.25, 0.5, 0.75, 0.9}}, report.RatedReasonNoSatRate, "no rate")
	if len(got) != 4 {
		t.Fatalf("%d records, want 4", len(got))
	}
	for i, r := range got {
		if r.Fraction != []float64{0.25, 0.5, 0.75, 0.9}[i] || r.Status != report.RatedPassNotRun || r.Reason != "no_saturation_rate" || r.Detail != "no rate" {
			t.Errorf("record %d = %+v", i, r)
		}
	}
}

// TestSkippedPassStderrLineKeepsItsShape: the census scripts of the #441
// evidence match the runner's stderr line "rated pass target=N: skipped
// (err=<nil>)" and "rated pass target=N: loadgen.New: ..." to count the passes
// the artifact did not record. The reason class goes after the closing
// parenthesis, so those patterns keep matching.
func TestSkippedPassStderrLineKeepsItsShape(t *testing.T) {
	withRatedScript(t, func(i int, cfg loadgen.Config) (ratedBench, error) {
		if i == 0 {
			return nil, errors.New("connection refused")
		}
		return scriptedBench{res: &loadgen.Result{Duration: time.Second}}, nil // zero requests
	})
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	runRatedSweep(context.Background(), Config{RatedFractions: []float64{0.5, 0.9}, RatedDuration: time.Second, Warmup: time.Second},
		loadgen.Config{}, 1000)
	os.Stderr = orig
	_ = w.Close()
	out, _ := io.ReadAll(r)
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("stderr = %q, want 2 lines", out)
	}
	if want := "  rated pass target=500: loadgen.New: connection refused"; lines[0] != want {
		t.Errorf("line 0 = %q, want %q", lines[0], want)
	}
	if want := "  rated pass target=900: skipped (err=<nil>) reason=zero_requests"; lines[1] != want {
		t.Errorf("line 1 = %q, want %q", lines[1], want)
	}
}
