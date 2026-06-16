package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/loadgen"

	"github.com/goceleris/probatorium/interleave"
	"github.com/goceleris/probatorium/report"
	"github.com/goceleris/probatorium/servers"
)

// TestClassifyCompletedCell pins every classification transition with the
// REAL v3.8 cell numbers (evidence: 20260611T074835-bench-msa2-server).
// These exact inputs produced the misclassifications that motivated the
// v3.9 hardening, so each row is a regression lock.
func TestClassifyCompletedCell(t *testing.T) {
	const refused = "dial tcp 192.168.50.65:8080: connect: connection refused"
	cases := []struct {
		name         string
		in           completedCell
		wantPrefix   string // "" = clean
		wantContains string // "" = no substring assertion
		wantHard     bool
		wantStatus   report.CellStatus
	}{
		{
			// v3.8 chain-api-post-4k/celeris-iouring-h1-async: the io_uring
			// heap corruption killed the SUT 4029 requests in; the 33.1M
			// post-crash dial errors tripped the old ratio-based
			// capability-lie guard and the cell published as
			// not_applicable. With a dead post-probe it is a server death.
			name: "v3.8 crash cell: 4029 successes + 33.1M errors + dead server",
			in: completedCell{
				ScenarioName: "chain-api-post-4k", ServerName: "celeris-iouring-h1-async",
				Category: "chain", Requests: 4029, Errors: 33140345,
				Duration: 90001497661, ServerAlive: false, ProbeErr: refused,
				ErrorBudget: 0.05,
			},
			wantPrefix: "server-died-mid-cell:", wantHard: true, wantStatus: report.CellDNF,
		},
		{
			// v3.8 sse-fanout-128/celeris-iouring-h1-async: 90 s of refused
			// dials against the dead port, hot-looped to 34.7M errors,
			// published as zero-request not_applicable.
			name: "v3.8 dead-port streaming cell: 0 requests + 34.7M errors",
			in: completedCell{
				ScenarioName: "sse-fanout-128", ServerName: "celeris-iouring-h1-async",
				Category: "sse", Requests: 0, Errors: 34730615,
				Duration: 90000225614, ServerAlive: false, ProbeErr: refused,
				ErrorBudget: 0.05,
			},
			wantPrefix: "server-died-mid-cell:", wantHard: true, wantStatus: report.CellDNF,
		},
		{
			// v3.8 sse-fanout-1024/celeris-std-h1: the hang-guard SIGTERM
			// truncated the cell to 354.329µs (0 req / 0 err) and it
			// published as not_applicable.
			name: "v3.8 interrupted 354µs stub",
			in: completedCell{
				ScenarioName: "sse-fanout-1024", ServerName: "celeris-std-h1",
				Category: "sse", Requests: 0, Errors: 0,
				Duration: 354329, Interrupted: true, ServerAlive: true,
				ErrorBudget: 0.05,
			},
			wantPrefix: "interrupted:", wantHard: true, wantStatus: report.CellDNF,
		},
		{
			// Genuine route-missing: zero successes, errors, and the
			// post-probe proves the server is up — the ONLY runtime path
			// to not_applicable left.
			name: "capability-lie: zero successes against live server",
			in: completedCell{
				ScenarioName: "ws-echo", ServerName: "gnet-h1",
				Category: "ws", Requests: 0, Errors: 120000,
				Duration: 90 * time.Second, ServerAlive: true,
				ErrorBudget: 0.05,
			},
			wantPrefix: "capability-lie:", wantHard: true, wantStatus: report.CellNotApplicable,
		},
		{
			// The v3.8 crash-cell counters but with the server still alive:
			// 4029 successes mean capability-lie may NOT fire — the ratio
			// gate flags it suspect instead.
			name: "high error ratio with successes + live server is suspect, never capability-lie",
			in: completedCell{
				ScenarioName: "chain-api-post-4k", ServerName: "celeris-iouring-h1-async",
				Category: "chain", Requests: 4029, Errors: 33140345,
				Duration: 90001497661, ServerAlive: true,
				ErrorBudget: 0.05,
			},
			wantPrefix: "suspect:", wantHard: false, wantStatus: report.CellSuspect,
		},
		{
			// Zero requests, zero errors, live server, not interrupted: a
			// genuinely idle cell — loud DNF, never N/A (v3.9 change).
			name: "zero-request cell on live server is dnf",
			in: completedCell{
				ScenarioName: "get-json", ServerName: "stdhttp-h1",
				Category: "static", Requests: 0, Errors: 0,
				Duration: 90 * time.Second, ServerAlive: true,
				ErrorBudget: 0.05,
			},
			wantPrefix: "zero-request cell:", wantHard: true, wantStatus: report.CellDNF,
		},
		{
			// v3.8 churn-close/ntex: 12,081,484 requests vs
			// 290,204,598 errors (ratio 0.960) published as status=ok.
			// Over the explicit 0.5 churn budget → suspect, data kept.
			name: "v3.8 churn-close error storm is suspect",
			in: completedCell{
				ScenarioName: "churn-close", ServerName: "ntex",
				Category: "static", Requests: 12081484, Errors: 290204598,
				Duration: 90004545724, ServerAlive: true,
				ErrorBudget: 0.5,
			},
			wantPrefix: "suspect:", wantHard: false, wantStatus: report.CellSuspect,
		},
		// ── ConnectErrors variants (loadgen >= c902b92 pin). The rows
		// above all carry ConnectErrors=0 — pre-counter artefacts — and
		// pin that legacy classification is untouched.
		{
			// The v3.8 dead-port streaming cell as the NEW loadgen would
			// report it, against a SUT that systemd flapped back up just in
			// time to pass the post-probe: every error is connect-class, so
			// the probe's verdict is outranked — server-down, not
			// capability-lie/zero-request.
			name: "connect-class error storm with zero requests is server-down despite passing probe",
			in: completedCell{
				ScenarioName: "sse-fanout-128", ServerName: "celeris-iouring-h1-async",
				Category: "sse", Requests: 0, Errors: 34730615,
				ConnectErrors: 34730615,
				Duration:      90000225614, ServerAlive: true,
				ErrorBudget: 0.05,
			},
			wantPrefix: "server-down:", wantContains: "connect_errors=34730615",
			wantHard: true, wantStatus: report.CellDNF,
		},
		{
			// Same counters but the post-probe also failed: the stronger
			// "server-died-mid-cell" evidence wins and now carries the
			// connect-class count.
			name: "connect-class error storm with dead probe stays server-died-mid-cell",
			in: completedCell{
				ScenarioName: "sse-fanout-128", ServerName: "celeris-iouring-h1-async",
				Category: "sse", Requests: 0, Errors: 34730615,
				ConnectErrors: 34730615,
				Duration:      90000225614, ServerAlive: false, ProbeErr: refused,
				ErrorBudget: 0.05,
			},
			wantPrefix: "server-died-mid-cell:", wantContains: "connect_errors=34730615",
			wantHard: true, wantStatus: report.CellDNF,
		},
		{
			// A genuine route gap on an HTTP-class category: the 404s are
			// request errors, NOT connect-class (h1client only counts failed
			// reconnect dials), so capability-lie still fires with the new
			// counters present-but-zero-ish (a handful of churn reconnects).
			name: "chain capability-lie with marginal connect errors stays not_applicable",
			in: completedCell{
				ScenarioName: "chain-api-get-json-1c", ServerName: "gnet-h1",
				Category: "chain", Requests: 0, Errors: 120000,
				ConnectErrors: 37,
				Duration:      90 * time.Second, ServerAlive: true,
				ErrorBudget: 0.05,
			},
			wantPrefix: "capability-lie:", wantHard: true, wantStatus: report.CellNotApplicable,
		},
		{
			// v3.8 churn-close storm as the new loadgen would report it when
			// the failed dials ARE the overage: the suspect reason must
			// attribute the blow-up to reachability, not server behaviour.
			name: "churn-close over budget with connect-class overage says so",
			in: completedCell{
				ScenarioName: "churn-close", ServerName: "ntex",
				Category: "static", Requests: 12081484, Errors: 290204598,
				ConnectErrors: 290204598,
				Duration:      90004545724, ServerAlive: true,
				ErrorBudget: 0.5,
			},
			wantPrefix: "suspect:", wantContains: "overage is connect-class (connect_errors=290204598)",
			wantHard: false, wantStatus: report.CellSuspect,
		},
		{
			// Over budget where the errors are NOT connect-class (server
			// answering 5xx): no reachability claim may appear.
			name: "over budget with non-connect errors carries no connect-class claim",
			in: completedCell{
				ScenarioName: "churn-close", ServerName: "ntex",
				Category: "static", Requests: 12081484, Errors: 290204598,
				ConnectErrors: 1024,
				Duration:      90004545724, ServerAlive: true,
				ErrorBudget: 0.5,
			},
			wantPrefix: "suspect:", wantHard: false, wantStatus: report.CellSuspect,
		},
		{
			// Churn with failed dials under half of completed requests
			// stays clean under the 0.5 budget.
			name: "churn-close under budget is clean",
			in: completedCell{
				ScenarioName: "churn-close", ServerName: "axum",
				Category: "static", Requests: 1000000, Errors: 900000,
				Duration: 90 * time.Second, ServerAlive: true,
				ErrorBudget: 0.5,
			},
			wantPrefix: "", wantHard: false, wantStatus: report.CellOK,
		},
		{
			// v3.8 chain-api-get-json-1c/celeris-iouring-h1-async — a clean
			// cell (52.5M requests, 701 errors) must stay clean.
			name: "v3.8 healthy cell stays ok",
			in: completedCell{
				ScenarioName: "chain-api-get-json-1c", ServerName: "celeris-iouring-h1-async",
				Category: "chain", Requests: 52530160, Errors: 701,
				Duration: 90004745802, ServerAlive: true,
				ErrorBudget: 0.05,
			},
			wantPrefix: "", wantHard: false, wantStatus: report.CellOK,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := classifyCompletedCell(tc.in)
			if tc.wantPrefix == "" {
				if v.ErrMsg != "" {
					t.Fatalf("ErrMsg = %q, want clean", v.ErrMsg)
				}
			} else if !strings.HasPrefix(v.ErrMsg, tc.wantPrefix) {
				t.Fatalf("ErrMsg = %q, want prefix %q", v.ErrMsg, tc.wantPrefix)
			}
			if tc.wantContains != "" && !strings.Contains(v.ErrMsg, tc.wantContains) {
				t.Errorf("ErrMsg = %q, want substring %q", v.ErrMsg, tc.wantContains)
			}
			// A connect-class claim must never appear unless the table row
			// asked for one — reachability blame on a server that answered
			// (with errors) would be a lie in the other direction.
			if tc.wantContains == "" && strings.Contains(v.ErrMsg, "connect-class") {
				t.Errorf("ErrMsg = %q makes an unrequested connect-class claim", v.ErrMsg)
			}
			if v.Hard != tc.wantHard {
				t.Errorf("Hard = %v, want %v", v.Hard, tc.wantHard)
			}
			// The synthesised string must round-trip through the SAME
			// classifier both merge paths use.
			if got := report.ClassifyCellError(v.ErrMsg); got != tc.wantStatus {
				t.Errorf("ClassifyCellError(%q) = %q, want %q", v.ErrMsg, got, tc.wantStatus)
			}
		})
	}
}

// TestReduceCellStatus pins the multi-run reduction: an OK run keeps the
// cell's data but can never erase prior SUT-behaviour evidence (the v3.8
// OK-promotion bug), while harness-side interruptions never poison
// otherwise-clean data.
func TestReduceCellStatus(t *testing.T) {
	cases := []struct {
		name    string
		runs    []report.CellStatus
		hasData bool
		demoted bool
		want    report.CellStatus
	}{
		{"all ok", []report.CellStatus{report.CellOK, report.CellOK}, true, false, report.CellOK},
		{"crash then ok is suspect, not ok", []report.CellStatus{report.CellDNF, report.CellOK}, true, true, report.CellSuspect},
		{"ok then crash is suspect", []report.CellStatus{report.CellOK, report.CellDNF}, true, true, report.CellSuspect},
		{"ok then interrupted keeps ok", []report.CellStatus{report.CellOK, report.CellDNF}, true, false, report.CellOK},
		{"suspect run is suspect", []report.CellStatus{report.CellSuspect}, true, true, report.CellSuspect},
		{"dnf only", []report.CellStatus{report.CellDNF}, false, true, report.CellDNF},
		{"n/a only", []report.CellStatus{report.CellNotApplicable}, false, true, report.CellNotApplicable},
		{"n/a + dnf is dnf (loud)", []report.CellStatus{report.CellNotApplicable, report.CellDNF}, false, true, report.CellDNF},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reduceCellStatus(tc.runs, tc.hasData, tc.demoted); got != tc.want {
				t.Errorf("reduceCellStatus(%v, data=%v, demoted=%v) = %q, want %q",
					tc.runs, tc.hasData, tc.demoted, got, tc.want)
			}
		})
	}
}

// sinkCell builds an interleave.Cell against a registered scenario and a
// synthetic adapter for resultsSink tests.
func sinkCell(t *testing.T, scenario, server string) interleave.Cell {
	t.Helper()
	return interleave.Cell{
		Scenario: scenarioByName(t, scenario),
		Server:   &adapterServer{adapter: servers.Adapter{Name: server, Category: "remote"}},
	}
}

// TestResultsSinkRecordRun drives the sink through the v3.8 evidence-
// erasure sequence (run 1 crashes, run 2 passes) and asserts the crash
// survives into both the in-memory cell and the flushed results.json.
func TestResultsSinkRecordRun(t *testing.T) {
	cfg := Config{Out: t.TempDir()}
	sink := newResultsSink(cfg, time.Now().UTC())
	cell := sinkCell(t, "get-json", "celeris-iouring-h1-async")

	crash := "server-died-mid-cell: post-cell probe: dial tcp 192.168.50.65:8080: connect: connection refused (requests=4029 errors=33140345)"
	sink.recordRun(cell, cellOutcome{Status: report.CellDNF, ErrorMsg: crash}, false)
	sink.recordRun(cell, cellOutcome{
		Result: &loadgen.Result{Requests: 100, RequestsPerSec: 1000, Duration: time.Second},
	}, false)

	cells := sink.cellsSnapshot()
	if len(cells) != 1 {
		t.Fatalf("cells = %d, want 1", len(cells))
	}
	cr := cells[0]
	if cr.Status != report.CellSuspect {
		t.Errorf("Status = %q, want suspect (OK run must not erase the crash)", cr.Status)
	}
	if len(cr.Samples) != 1 {
		t.Errorf("Samples = %d, want 1 (the OK run's data is kept)", len(cr.Samples))
	}
	if want := []report.CellStatus{report.CellDNF, report.CellOK}; len(cr.RunStatuses) != 2 ||
		cr.RunStatuses[0] != want[0] || cr.RunStatuses[1] != want[1] {
		t.Errorf("RunStatuses = %v, want %v", cr.RunStatuses, want)
	}
	if cr.ErrorMsg != crash {
		t.Errorf("ErrorMsg = %q, want the ORIGINAL crash error", cr.ErrorMsg)
	}

	// The flushed document carries the evidence.
	if err := sink.flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(cfg.Out, "results.json"))
	if err != nil {
		t.Fatalf("read results.json: %v", err)
	}
	var doc report.Document
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("results.json did not parse: %v", err)
	}
	if len(doc.Benchmarks) != 1 {
		t.Fatalf("Benchmarks = %d, want 1", len(doc.Benchmarks))
	}
	sr := doc.Benchmarks[0]
	if sr.CellStatuses["get-json"] != string(report.CellSuspect) {
		t.Errorf("cell_statuses[get-json] = %q, want suspect", sr.CellStatuses["get-json"])
	}
	if got := sr.CellRunStatuses["get-json"]; len(got) != 2 || got[0] != "dnf" || got[1] != "ok" {
		t.Errorf("cell_run_statuses[get-json] = %v, want [dnf ok]", got)
	}
	if sr.SaturationModeRPS["get-json"] != 1000 {
		t.Errorf("suspect cell lost its data: saturation = %v, want 1000", sr.SaturationModeRPS["get-json"])
	}
}

// TestResultsSinkInterruptedDoesNotDemote asserts a harness-side
// interruption mark after a clean run leaves the cell OK — RunStatuses
// still records the interrupted run, but the data's integrity is not in
// question.
func TestResultsSinkInterruptedDoesNotDemote(t *testing.T) {
	cfg := Config{Out: t.TempDir()}
	sink := newResultsSink(cfg, time.Now().UTC())
	cell := sinkCell(t, "get-json", "axum")

	sink.recordRun(cell, cellOutcome{
		Result: &loadgen.Result{Requests: 100, RequestsPerSec: 1000, Duration: time.Second},
	}, false)
	sink.recordRun(cell, cellOutcome{
		Status:   report.CellDNF,
		ErrorMsg: "interrupted: run cancelled before cell start",
	}, false)

	cr := sink.cellsSnapshot()[0]
	if cr.Status != report.CellOK {
		t.Errorf("Status = %q, want ok (interruption is not SUT evidence)", cr.Status)
	}
	if cr.ErrorMsg != "" {
		t.Errorf("ErrorMsg = %q, want empty for an OK cell", cr.ErrorMsg)
	}
	if len(cr.RunStatuses) != 2 || cr.RunStatuses[1] != report.CellDNF {
		t.Errorf("RunStatuses = %v, want [ok dnf] (evidence kept)", cr.RunStatuses)
	}
}

// TestMarkCellsInterrupted asserts the first-signal path stamps every
// unstarted cell DNF "interrupted" with a per-cell JSON and a flushed
// results.json — the v3.8 hang-guard truncation left those cells either
// missing or as bogus 354µs zero-request N/As.
func TestMarkCellsInterrupted(t *testing.T) {
	cfg := Config{Out: t.TempDir()}
	sink := newResultsSink(cfg, time.Now().UTC())
	cells := []interleave.Cell{
		sinkCell(t, "get-json", "axum"),
		sinkCell(t, "get-simple", "axum"),
	}
	markCellsInterrupted(cfg, sink, cells)

	// Per-cell JSON written for each, classified dnf.
	raw, err := os.ReadFile(filepath.Join(cfg.Out, "run0", "get-json", "axum.json"))
	if err != nil {
		t.Fatalf("per-cell JSON missing: %v", err)
	}
	var cf cellResultFile
	if err := json.Unmarshal(raw, &cf); err != nil {
		t.Fatalf("per-cell JSON did not parse: %v", err)
	}
	if cf.Status != string(report.CellDNF) || !strings.HasPrefix(cf.Error, "interrupted:") {
		t.Errorf("per-cell status/error = %q/%q, want dnf/interrupted:*", cf.Status, cf.Error)
	}

	// results.json flushed with both cells marked dnf.
	var doc report.Document
	rawDoc, err := os.ReadFile(filepath.Join(cfg.Out, "results.json"))
	if err != nil {
		t.Fatalf("results.json missing after interrupt flush: %v", err)
	}
	if err := json.Unmarshal(rawDoc, &doc); err != nil {
		t.Fatalf("results.json did not parse: %v", err)
	}
	if len(doc.Benchmarks) != 1 {
		t.Fatalf("Benchmarks = %d, want 1", len(doc.Benchmarks))
	}
	for _, sc := range []string{"get-json", "get-simple"} {
		if got := doc.Benchmarks[0].CellStatuses[sc]; got != string(report.CellDNF) {
			t.Errorf("cell_statuses[%s] = %q, want dnf", sc, got)
		}
	}
}

func TestHostPortFromURL(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"http://10.0.0.2:8080", "10.0.0.2:8080", true},
		{"http://bench-target", "bench-target:80", true},
		{"https://bench-target", "bench-target:443", true},
		{"http://[::1]:9000", "[::1]:9000", true},
		{"not a url", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		got, ok := hostPortFromURL(tc.in)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("hostPortFromURL(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}

// TestLoadgenRunCellError pins the loadgen.Run error-path classification,
// in particular the fail-fast abort introduced by the c902b92 loadgen pin:
// Run returns a NIL Result and an ErrNeverConnected-wrapped "loadgen:
// dial: …" error instead of burning the window against a dead target. The
// cell must land DNF with the "server-down:" mark (cooldown skip + publish
// dead-SUT tally) whether or not the post-probe passes — a flapping
// listener answering three spaced probe dials is not a healthy server.
func TestLoadgenRunCellError(t *testing.T) {
	// Shaped exactly like loadgen's failFastTracker.failure synthesis.
	failFast := fmt.Errorf("loadgen: dial: %w (fail-fast: %w within %v)",
		errors.New("dial tcp 192.168.50.65:8080: connect: connection refused"),
		loadgen.ErrNeverConnected, 5*time.Second)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = l.Close() }()
	liveAddr := l.Addr().String()

	deadPort, err := freePort()
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	deadAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(deadPort))

	cases := []struct {
		name         string
		err          error
		probeAddr    string
		wantPrefix   string
		wantContains string
	}{
		{
			name: "fail-fast abort with dead probe is server-down",
			err:  failFast, probeAddr: deadAddr,
			wantPrefix: "server-down:", wantContains: "post-probe:",
		},
		{
			name: "fail-fast abort with passing probe is STILL server-down",
			err:  failFast, probeAddr: liveAddr,
			wantPrefix: "server-down:", wantContains: "post-probe passed",
		},
		{
			name: "fail-fast abort without probe addr is server-down",
			err:  failFast, probeAddr: "",
			wantPrefix: "server-down:", wantContains: "no post-probe addr",
		},
		{
			name:       "non-fail-fast Run error keeps the legacy prefix",
			err:        errors.New("h2: read server settings: connection reset"),
			wantPrefix: "loadgen.Run:",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := loadgenRunCellError(context.Background(), tc.err, tc.probeAddr)
			if !strings.HasPrefix(msg, tc.wantPrefix) {
				t.Fatalf("msg = %q, want prefix %q", msg, tc.wantPrefix)
			}
			if tc.wantContains != "" && !strings.Contains(msg, tc.wantContains) {
				t.Errorf("msg = %q, want substring %q", msg, tc.wantContains)
			}
			if got := report.ClassifyCellError(msg); got != report.CellDNF {
				t.Errorf("ClassifyCellError(%q) = %q, want dnf", msg, got)
			}
		})
	}
}

// TestProbeSUT covers both verdicts against real loopback sockets, and
// confirms a cancelled run context does not fake a server death (the
// probe runs detached).
func TestProbeSUT(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = l.Close() }()
	addr := l.Addr().String()

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := probeSUT(cancelled, addr); err != nil {
		t.Errorf("live server probed dead under a cancelled context: %v", err)
	}

	dead, err := freePort()
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	if err := probeSUT(context.Background(), net.JoinHostPort("127.0.0.1", strconv.Itoa(dead))); err == nil {
		t.Errorf("dead port probed alive")
	}
}

// TestWriteJSONAtomic asserts writeJSON lands complete bytes via rename
// (no temp leftovers, old content survives until the swap) so a SIGKILL
// mid-write can never leave a torn results.json.
func TestWriteJSONAtomic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "results.json")

	if err := writeJSON(p, map[string]int{"v": 1}); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}
	if err := writeJSON(p, map[string]int{"v": 2}); err != nil {
		t.Fatalf("writeJSON overwrite: %v", err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var got map[string]int
	if err := json.Unmarshal(raw, &got); err != nil || got["v"] != 2 {
		t.Fatalf("content = %s (err=%v), want {\"v\": 2}", raw, err)
	}
	entries, err := os.ReadDir(filepath.Dir(p))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temp file leaked: %s", e.Name())
		}
	}
}
