package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestClassifyCellError pins the error→status mapping that BOTH merge
// paths (the in-process runner and the cluster mage Bench path) call so
// they agree on field width. The split is load-bearing: "capability-lie"
// is the adapter not implementing the route (N/A) — but its legacy
// ratio-fired form (pre-v3.9, requests > 0) is DNF; "suspect:" is the
// runner's error-ratio gate (data kept, integrity flagged); "read server
// settings" is N/A only when it timed out (server never spoke H2), but
// DNF on reset/EOF (an H2 server that crashed mid-handshake); everything
// else — including "zero-request cell", which v3.8 proved is what every
// dead-SUT / interrupted cell wears (genuine capability gaps never reach
// loadgen; the scheduler skips them) — is an infra failure (DNF);
// ambiguous defaults to DNF, never N/A.
func TestClassifyCellError(t *testing.T) {
	cases := []struct {
		name string
		err  string
		want CellStatus
	}{
		{"empty is ok", "", CellOK},
		// v3.9: zero-request was N/A before v5.4. The v3.8 dead-port
		// streaming cells (0 req / 34.7M err) and the SIGTERM-truncated
		// 354µs cells (0 req / 0 err) all carried this string and were
		// silently excused as N/A — it is now loud by design.
		{"zero-request is dnf", "zero-request cell: errors=10 duration=20s", CellDNF},
		{"zero-request dead-port storm is dnf", "zero-request cell: errors=34730615 duration=1m30.000225614s", CellDNF},
		{"zero-request interrupted stub is dnf", "zero-request cell: errors=0 duration=354.329µs", CellDNF},
		{"server-down probe is dnf", "server-down: pre-cell probe: dial tcp 192.168.50.65:8080: connect: connection refused", CellDNF},
		{"server-died-mid-cell is dnf", "server-died-mid-cell: post-cell probe: dial tcp 192.168.50.65:8080: connect: connection refused (requests=4029 errors=33140345)", CellDNF},
		{"interrupted is dnf", "interrupted: cell cancelled mid-run (requests=0 errors=0 duration=354.329µs)", CellDNF},
		{"suspect is suspect", "suspect: error ratio 0.960 exceeds budget 0.50 (errors=290204598 requests=12081484)", CellSuspect},
		{"capability-lie (zero successes, live server) is n/a", `capability-lie: scheduled ws scenario "ws-echo" got zero successes from live server gnet-h1 (errors=120000) — adapter declared the capability but did not serve the route`, CellNotApplicable},
		// v3.9: the verbatim v3.8 io_uring crash cell. The pre-v3.9 guard
		// fired on a RATIO (requests > 0), which the zero-successes rule
		// says can never be a genuine gap — re-classified DNF so a
		// `smoketest scan` over stale results can't feed it to the skip
		// list as N/A.
		{"legacy ratio-fired capability-lie is dnf", `capability-lie: scheduled chain scenario "chain-api-post-4k" got high error ratio from celeris-iouring-h1-async (errors=33140345/requests=4029) — adapter declared the capability but did not serve the route`, CellDNF},
		{"read server settings timeout is n/a", "loadgen.Run: loadgen: dial: h2client: connect: read server settings: i/o timeout", CellNotApplicable},
		{"read server settings reset is dnf (crash mid-handshake)", "loadgen.New: loadgen: dial: h2client: dial conn[0]: read server settings: read tcp 10.0.0.2:5->10.0.0.1:8080: read: connection reset by peer", CellDNF},
		{"read server settings EOF is dnf (crash mid-handshake)", "loadgen.New: loadgen: dial: h2client: dial conn[0]: read server settings: unexpected EOF", CellDNF},
		{"address already in use is dnf", "adapter start: listen tcp 127.0.0.1:8080: bind: address already in use", CellDNF},
		{"adapter start is dnf", "adapter start: context deadline exceeded", CellDNF},
		{"ready-check is dnf", "ready-check: dial tcp 127.0.0.1:8080: connect: connection refused", CellDNF},
		{"loadgen.New is dnf", "loadgen.New: invalid URL", CellDNF},
		{"loadgen.Run dial is dnf", "loadgen.Run: dial tcp: connect: connection refused", CellDNF},
		{"loadgen.Run reset is dnf", "loadgen.Run: stream error: stream ID 1; INTERNAL_ERROR; received from peer", CellDNF},
		{"loadgen.Run EOF is dnf", "loadgen.Run: unexpected EOF", CellDNF},
		{"loadgen.Run timeout is dnf", "loadgen.Run: context deadline exceeded", CellDNF},
		{"unknown ambiguous defaults to dnf", "something nobody anticipated", CellDNF},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyCellError(tc.err); got != tc.want {
				t.Errorf("ClassifyCellError(%q) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestReduceCellStatus pins the shared multi-run reduction (the
// cluster-merge mirror of cmd/runner's private reduceCellStatus table —
// the two MUST stay in sync): an OK run keeps the cell's data but can
// never erase prior SUT-behaviour evidence (the v3.8 OK-promotion bug),
// while harness-side interruptions never poison otherwise-clean data.
func TestReduceCellStatus(t *testing.T) {
	cases := []struct {
		name    string
		runs    []CellStatus
		hasData bool
		demoted bool
		want    CellStatus
	}{
		{"all ok", []CellStatus{CellOK, CellOK}, true, false, CellOK},
		{"crash then ok is suspect, not ok", []CellStatus{CellDNF, CellOK}, true, true, CellSuspect},
		{"ok then crash is suspect", []CellStatus{CellOK, CellDNF}, true, true, CellSuspect},
		{"ok then interrupted keeps ok", []CellStatus{CellOK, CellDNF}, true, false, CellOK},
		{"suspect run is suspect", []CellStatus{CellSuspect}, true, true, CellSuspect},
		{"dnf only", []CellStatus{CellDNF}, false, true, CellDNF},
		{"n/a only", []CellStatus{CellNotApplicable}, false, true, CellNotApplicable},
		{"n/a + dnf is dnf (loud)", []CellStatus{CellNotApplicable, CellDNF}, false, true, CellDNF},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ReduceCellStatus(tc.runs, tc.hasData, tc.demoted); got != tc.want {
				t.Errorf("ReduceCellStatus(%v, data=%v, demoted=%v) = %q, want %q",
					tc.runs, tc.hasData, tc.demoted, got, tc.want)
			}
		})
	}
}

// TestFormatRunOutcome pins the markdown run-evidence rendering: the
// reduced verdict leads, the ok-run fraction follows, and non-OK runs
// are tallied in a fixed order so the note is byte-stable.
func TestFormatRunOutcome(t *testing.T) {
	cases := []struct {
		name string
		st   CellStatus
		seq  []string
		want string
	}{
		{"recovered interruption", CellOK, []string{"dnf", "ok", "ok"}, "ok (2/3 runs; 1 dnf)"},
		{"crash demotes", CellSuspect, []string{"dnf", "ok"}, "suspect (1/2 runs; 1 dnf)"},
		{"suspect run", CellSuspect, []string{"suspect"}, "suspect (0/1 runs; 1 suspect)"},
		{"mixed non-ok order is fixed", CellSuspect, []string{"suspect", "dnf", "not_applicable", "ok"},
			"suspect (1/4 runs; 1 dnf, 1 n/a, 1 suspect)"},
		{"all ok (defensive)", CellOK, []string{"ok", "ok"}, "ok (2/2 runs)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatRunOutcome(tc.st, tc.seq); got != tc.want {
				t.Errorf("formatRunOutcome(%q, %v) = %q, want %q", tc.st, tc.seq, got, tc.want)
			}
		})
	}
}

// TestAggregateNonOKExcludedFromHeadline asserts a not-applicable cell
// (no samples) produces a CellAggregate that surfaces its Status but
// contributes NO headline numbers, while an OK cell's numbers are
// unchanged vs the pre-classification behaviour (golden).
func TestAggregateNonOKExcludedFromHeadline(t *testing.T) {
	ok := CellResult{
		ScenarioName: "get-json", ServerName: "celeris-std-h1",
		ServerKind: "celeris", Category: "static",
		Samples: makeSamples([]float64{100, 200, 300}, 50*time.Microsecond),
	}
	na := CellResult{
		ScenarioName: "chain-mw5", ServerName: "gin-h1",
		ServerKind: "gin", Category: "chain",
		// No samples: the cell never produced a real number. Status set
		// explicitly to mirror the runner's collection loop.
		Status:   CellNotApplicable,
		ErrorMsg: `capability-lie: scheduled chain scenario "chain-mw5" …`,
	}
	dnf := CellResult{
		ScenarioName: "chain-mw5", ServerName: "stdhttp-h1",
		ServerKind: "stdhttp", Category: "chain",
		ErrorMsg: "adapter start: bind: address already in use",
	}

	agg := Aggregate([]CellResult{ok, na, dnf})

	// OK cell: golden — same numbers Aggregate produced before v5.3.
	c := agg[CellID("get-json", "celeris-std-h1")]
	if c.Status != CellOK {
		t.Errorf("ok cell Status = %q, want ok", c.Status)
	}
	if c.RPSMedian != 200 {
		t.Errorf("ok cell RPSMedian = %.2f, want 200 (golden)", c.RPSMedian)
	}
	if c.N != 3 {
		t.Errorf("ok cell N = %d, want 3", c.N)
	}

	// N/A cell: Status surfaced, every headline field left zero.
	n := agg[CellID("chain-mw5", "gin-h1")]
	if n.Status != CellNotApplicable {
		t.Errorf("n/a cell Status = %q, want not_applicable", n.Status)
	}
	if n.RPSMedian != 0 || n.RPSP5 != 0 || n.RPSP95 != 0 {
		t.Errorf("n/a cell leaked RPS: median=%.2f p5=%.2f p95=%.2f, want all 0", n.RPSMedian, n.RPSP5, n.RPSP95)
	}
	if n.MergedHistogramB64 != "" || len(n.LatencyAtSLO) != 0 {
		t.Errorf("n/a cell leaked latency/histogram into headline")
	}
	if n.ErrorMsg == "" {
		t.Errorf("n/a cell dropped ErrorMsg")
	}

	// DNF cell: classified from the error string (Status was unset).
	d := agg[CellID("chain-mw5", "stdhttp-h1")]
	if d.Status != CellDNF {
		t.Errorf("dnf cell Status = %q, want dnf", d.Status)
	}
	if d.RPSMedian != 0 {
		t.Errorf("dnf cell leaked RPS: %.2f, want 0", d.RPSMedian)
	}
}

// TestBuildDocumentCellStatuses asserts a chain scenario with one OK
// adapter, one not-applicable and one DNF produces a Document where the
// OK adapter carries the real number, the non-OK adapters carry NO
// headline entry but DO carry a cell_statuses entry, and the rendered
// markdown reads "ran=1 n/a=1 dnf=1" with N/A / DNF tokens (never
// "0 rps").
func TestBuildDocumentCellStatuses(t *testing.T) {
	celeris := CellResult{
		ScenarioName: "chain-mw5", ServerName: "celeris-std-h1",
		ServerKind: "celeris", Category: "chain",
		Samples:      makeSamples([]float64{500000}, 40*time.Microsecond),
		RatedSamples: [][]RatedSample{{{TargetRPS: 250000, P99: 2 * time.Millisecond}}},
	}
	gin := CellResult{
		ScenarioName: "chain-mw5", ServerName: "gin-h1",
		ServerKind: "gin", Category: "chain",
		Status:   CellNotApplicable,
		ErrorMsg: "capability-lie: …",
	}
	stdhttp := CellResult{
		ScenarioName: "chain-mw5", ServerName: "stdhttp-h1",
		ServerKind: "stdhttp", Category: "chain",
		Status:   CellDNF,
		ErrorMsg: "adapter start: bind: address already in use",
	}
	agg := Aggregate([]CellResult{celeris, gin, stdhttp})

	doc := BuildDocument(BuildInput{
		HostArchPair: "linux/amd64",
		Environment:  Environment{KernelSysctlsApplied: []string{}, LoadgenHost: "h", Fabric: "loopback"},
		BenchmarkConfig: BenchmarkConfig{
			StartedAt:  time.Unix(1700000000, 0).UTC(),
			FinishedAt: time.Unix(1700001000, 0).UTC(),
			Runs:       1, Duration: time.Second, GitRef: "x",
			LoadgenVer: "v1", CelerisVer: "v1",
		},
		Servers: map[string]ServerMeta{
			"celeris-std-h1": {Category: "chain", Language: "go", LanguageVersion: "go1.26", Framework: "celeris", FrameworkVersion: "v1", CompileOptions: []string{}},
			"gin-h1":         {Category: "chain", Language: "go", LanguageVersion: "go1.26", Framework: "gin", FrameworkVersion: "v1", CompileOptions: []string{}},
			"stdhttp-h1":     {Category: "chain", Language: "go", LanguageVersion: "go1.26", Framework: "net/http", FrameworkVersion: "v1", CompileOptions: []string{}},
		},
		Agg: agg,
	})

	byName := map[string]ServerResult{}
	for _, b := range doc.Benchmarks {
		byName[b.Name] = b
	}

	// OK adapter: real number present, no cell_statuses entry.
	cel := byName["celeris-std-h1"]
	if cel.SaturationModeRPS["chain-mw5"] != 500000 {
		t.Errorf("celeris saturation = %.0f, want 500000", cel.SaturationModeRPS["chain-mw5"])
	}
	if _, present := cel.CellStatuses["chain-mw5"]; present {
		t.Errorf("OK cell must not appear in cell_statuses, got %v", cel.CellStatuses)
	}

	// N/A adapter: cell_statuses carries gin→not_applicable, NO headline.
	g := byName["gin-h1"]
	if g.CellStatuses["chain-mw5"] != string(CellNotApplicable) {
		t.Errorf("gin cell_statuses[chain-mw5] = %q, want not_applicable", g.CellStatuses["chain-mw5"])
	}
	if _, present := g.SaturationModeRPS["chain-mw5"]; present {
		t.Errorf("N/A cell leaked a 0-RPS saturation row: %v", g.SaturationModeRPS)
	}
	if _, present := g.LatencyAtSLO["chain-mw5"]; present {
		t.Errorf("N/A cell leaked a latency_at_slo row")
	}

	// DNF adapter: cell_statuses carries stdhttp→dnf, NO headline.
	s := byName["stdhttp-h1"]
	if s.CellStatuses["chain-mw5"] != string(CellDNF) {
		t.Errorf("stdhttp cell_statuses[chain-mw5] = %q, want dnf", s.CellStatuses["chain-mw5"])
	}
	if _, present := s.SaturationModeRPS["chain-mw5"]; present {
		t.Errorf("DNF cell leaked a 0-RPS saturation row: %v", s.SaturationModeRPS)
	}

	// The emitted document (with cell_statuses) validates against the
	// JSON schema — confirms the v5.3 schema_v5.json addition is correct.
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal doc: %v", err)
	}
	validateAny(t, loadSchema(t), raw)

	// Markdown: field-width note + N/A / DNF tokens, no "0 rps" row.
	var buf bytes.Buffer
	if err := WriteMarkdown(&buf, doc, agg, Meta{GitRef: "t"}); err != nil {
		t.Fatalf("WriteMarkdown: %v", err)
	}
	md := buf.String()
	for _, want := range []string{
		"ran=1 n/a=1 dnf=1",
		"| gin-h1 | N/A |",
		"| stdhttp-h1 | DNF |",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q in:\n%s", want, md)
		}
	}
	if strings.Contains(md, "0 rps") || strings.Contains(md, "| gin-h1 | 0 ") {
		t.Errorf("markdown emitted a 0-RPS row for a non-OK cell:\n%s", md)
	}
}

// TestDocumentV52DecodesWithoutCellStatuses asserts a v5.2 document (no
// cell_statuses field) still decodes — the new field is additive and
// omitempty, so older artefacts round-trip with a nil CellStatuses.
func TestDocumentV52DecodesWithoutCellStatuses(t *testing.T) {
	const v52 = `{
		"schema_version": "5.2",
		"host_arch_pair": "linux/amd64",
		"environment": {"kernel_sysctls_applied": [], "loadgen_host": "h", "fabric": "loopback"},
		"benchmark_config": {"started_at":"2024-01-01T00:00:00Z","finished_at":"2024-01-01T00:01:00Z","runs":1,"duration":1,"warmup":0,"git_ref":"x","loadgen_version":"v1","celeris_version":"v1"},
		"benchmarks": [
			{"name":"celeris-std-h1","category":"static","language":"go","language_version":"go1.26","framework":"celeris","framework_version":"v1","compile_options":[],"saturation_mode_rps":{"get-json":100},"rated_mode_p99_at_target_rps":{},"latency_at_slo":{},"hdr_histogram_b64":{},"loadgen_cpu_p95":{},"sent_vs_handled_delta_pct":{}}
		]
	}`
	var doc Document
	if err := json.Unmarshal([]byte(v52), &doc); err != nil {
		t.Fatalf("v5.2 document failed to decode under v5.3 schema: %v", err)
	}
	if doc.SchemaVersion != "5.2" {
		t.Errorf("SchemaVersion = %q, want 5.2", doc.SchemaVersion)
	}
	if len(doc.Benchmarks) != 1 {
		t.Fatalf("Benchmarks = %d, want 1", len(doc.Benchmarks))
	}
	if doc.Benchmarks[0].CellStatuses != nil {
		t.Errorf("CellStatuses should be nil for a v5.2 doc, got %v", doc.Benchmarks[0].CellStatuses)
	}
}

// TestAggregateSuspectKeepsData asserts a suspect cell — real samples,
// error ratio over budget (the churn-close shape: v3.8 axum ran
// 12,081,484 requests against 290,204,598 errors and was published
// status=ok) — keeps its headline numbers while carrying the suspect
// status, unlike N/A / DNF cells which stay zero.
func TestAggregateSuspectKeepsData(t *testing.T) {
	samples := makeSamples([]float64{134231.93}, 50*time.Microsecond)
	samples[0].Requests = 12081484
	samples[0].Errors = 290204598
	cell := CellResult{
		ScenarioName: "churn-close", ServerName: "axum",
		ServerKind: "axum", Category: "static",
		Samples:     samples,
		Status:      CellSuspect,
		ErrorMsg:    "suspect: error ratio 0.960 exceeds budget 0.50 (errors=290204598 requests=12081484)",
		RunStatuses: []CellStatus{CellSuspect},
	}

	agg := Aggregate([]CellResult{cell})
	c := agg[CellID("churn-close", "axum")]
	if c.Status != CellSuspect {
		t.Errorf("Status = %q, want suspect", c.Status)
	}
	if c.RPSMedian != 134231.93 {
		t.Errorf("suspect cell lost its data: RPSMedian = %.2f, want 134231.93", c.RPSMedian)
	}
	if c.Errors != 290204598 {
		t.Errorf("Errors = %d, want 290204598", c.Errors)
	}
	if c.ErrorMsg == "" {
		t.Errorf("suspect cell dropped ErrorMsg")
	}
	if len(c.RunStatuses) != 1 || c.RunStatuses[0] != CellSuspect {
		t.Errorf("RunStatuses = %v, want [suspect]", c.RunStatuses)
	}
}

// TestBuildDocumentSuspectAndRunStatuses asserts (a) a suspect cell gets
// BOTH a cell_statuses entry and its headline saturation number — data
// exists, integrity flagged — and (b) a cell whose run sequence was
// [dnf, ok] surfaces the sequence in cell_run_statuses so the OK rerun
// does not erase the crash evidence. The emitted document must still
// validate against schema_v5.json.
func TestBuildDocumentSuspectAndRunStatuses(t *testing.T) {
	suspectSamples := makeSamples([]float64{134231.93}, 50*time.Microsecond)
	suspectSamples[0].Requests = 12081484
	suspectSamples[0].Errors = 290204598
	suspect := CellResult{
		ScenarioName: "churn-close", ServerName: "axum",
		ServerKind: "axum", Category: "static",
		Samples:     suspectSamples,
		Status:      CellSuspect,
		ErrorMsg:    "suspect: error ratio 0.960 exceeds budget 0.50 (errors=290204598 requests=12081484)",
		RunStatuses: []CellStatus{CellSuspect},
	}
	// A second adapter ran churn-close clean WITH rated data so the
	// scenario gets a Latency-at-SLO section for the suspect row to
	// render its token in.
	churnOK := CellResult{
		ScenarioName: "churn-close", ServerName: "celeris-std-h1",
		ServerKind: "celeris", Category: "static",
		Samples:      makeSamples([]float64{160000}, 45*time.Microsecond),
		RatedSamples: [][]RatedSample{{{TargetRPS: 120000, P99: 2 * time.Millisecond}}},
		RunStatuses:  []CellStatus{CellOK},
	}
	recovered := CellResult{
		ScenarioName: "get-json", ServerName: "axum",
		ServerKind: "axum", Category: "static",
		Samples:      makeSamples([]float64{500000}, 40*time.Microsecond),
		RatedSamples: [][]RatedSample{{{TargetRPS: 250000, P99: 2 * time.Millisecond}}},
		Status:       CellSuspect,
		ErrorMsg:     "server-down: pre-cell probe: dial tcp 10.0.0.2:8080: connect: connection refused",
		RunStatuses:  []CellStatus{CellDNF, CellOK},
	}
	clean := CellResult{
		ScenarioName: "get-simple", ServerName: "axum",
		ServerKind: "axum", Category: "static",
		Samples:     makeSamples([]float64{700000}, 30*time.Microsecond),
		RunStatuses: []CellStatus{CellOK, CellOK},
	}
	agg := Aggregate([]CellResult{suspect, churnOK, recovered, clean})

	doc := BuildDocument(BuildInput{
		HostArchPair: "linux/amd64",
		Environment:  Environment{KernelSysctlsApplied: []string{}, LoadgenHost: "h", Fabric: "loopback"},
		BenchmarkConfig: BenchmarkConfig{
			StartedAt:  time.Unix(1700000000, 0).UTC(),
			FinishedAt: time.Unix(1700001000, 0).UTC(),
			Runs:       2, Duration: time.Second, GitRef: "x",
			LoadgenVer: "v1", CelerisVer: "v1",
		},
		Servers: map[string]ServerMeta{
			"axum":           {Category: "static", Language: "rust", Framework: "axum", FrameworkVersion: "v0.7", CompileOptions: []string{}},
			"celeris-std-h1": {Category: "static", Language: "go", LanguageVersion: "go1.26", Framework: "celeris", FrameworkVersion: "v1", CompileOptions: []string{}},
		},
		Agg: agg,
	})
	if len(doc.Benchmarks) != 2 {
		t.Fatalf("Benchmarks = %d, want 2", len(doc.Benchmarks))
	}
	// Sorted by Name: axum first.
	sr := doc.Benchmarks[0]

	// Suspect cell: flagged AND ranked-with-data.
	if sr.CellStatuses["churn-close"] != string(CellSuspect) {
		t.Errorf("cell_statuses[churn-close] = %q, want suspect", sr.CellStatuses["churn-close"])
	}
	if got := sr.SaturationModeRPS["churn-close"]; got != 134231.93 {
		t.Errorf("suspect cell lost its headline number: saturation = %v, want 134231.93", got)
	}
	if got := sr.CellRunStatuses["churn-close"]; len(got) != 1 || got[0] != "suspect" {
		t.Errorf("cell_run_statuses[churn-close] = %v, want [suspect]", got)
	}

	// Recovered cell: the [dnf, ok] sequence survives.
	if got := sr.CellRunStatuses["get-json"]; len(got) != 2 || got[0] != "dnf" || got[1] != "ok" {
		t.Errorf("cell_run_statuses[get-json] = %v, want [dnf ok]", got)
	}

	// Clean cell: all-OK runs add no run-status bytes.
	if _, present := sr.CellRunStatuses["get-simple"]; present {
		t.Errorf("all-OK cell must not appear in cell_run_statuses, got %v", sr.CellRunStatuses)
	}
	if _, present := sr.CellStatuses["get-simple"]; present {
		t.Errorf("all-OK cell must not appear in cell_statuses, got %v", sr.CellStatuses)
	}

	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal doc: %v", err)
	}
	validateAny(t, loadSchema(t), raw)

	// Markdown: the suspect count appears in the field note and the
	// suspect cell never renders as a 0-RPS or bolded row.
	var buf bytes.Buffer
	if err := WriteMarkdown(&buf, doc, agg, Meta{GitRef: "t"}); err != nil {
		t.Fatalf("WriteMarkdown: %v", err)
	}
	md := buf.String()
	if !strings.Contains(md, "ran=1 n/a=0 dnf=0 suspect=1") {
		t.Errorf("markdown missing suspect count in field note:\n%s", md)
	}
	// churn-close: suspect with NO rated row → token spanning the SLO
	// columns, never "0 rps".
	if !strings.Contains(md, "| axum | SUSPECT | SUSPECT | SUSPECT | SUSPECT | SUSPECT |") {
		t.Errorf("markdown missing SUSPECT token row for rated-data-less suspect cell:\n%s", md)
	}
	// get-json: suspect WITH a rated row → numbers render but are never
	// bolded as a leader (colMax excludes non-OK cells).
	if strings.Contains(md, "**250.0k") {
		t.Errorf("suspect cell's rated numbers were bolded as a leader:\n%s", md)
	}
	// Per-run outcome evidence renders under each scenario carrying it:
	// the recovered cell shows the crash ("1 dnf"), the suspect churn
	// cell its suspect run — an OK rerun stays visible as 1/2, never 2/2.
	if !strings.Contains(md, "_runs: axum suspect (1/2 runs; 1 dnf)_") {
		t.Errorf("markdown missing run evidence for the [dnf ok] cell:\n%s", md)
	}
	if !strings.Contains(md, "_runs: axum suspect (0/1 runs; 1 suspect)_") {
		t.Errorf("markdown missing run evidence for the suspect churn cell:\n%s", md)
	}
}

// TestDocumentV53DecodesWithoutRunStatuses asserts a v5.3 document (no
// cell_run_statuses field) still decodes — the v5.4 fields are additive
// and omitempty, so older artefacts round-trip with nil maps.
func TestDocumentV53DecodesWithoutRunStatuses(t *testing.T) {
	const v53 = `{
		"schema_version": "5.3",
		"host_arch_pair": "linux/amd64",
		"environment": {"kernel_sysctls_applied": [], "loadgen_host": "h", "fabric": "loopback"},
		"benchmark_config": {"started_at":"2024-01-01T00:00:00Z","finished_at":"2024-01-01T00:01:00Z","runs":1,"duration":1,"warmup":0,"git_ref":"x","loadgen_version":"v1","celeris_version":"v1"},
		"benchmarks": [
			{"name":"celeris-std-h1","category":"static","language":"go","language_version":"go1.26","framework":"celeris","framework_version":"v1","compile_options":[],"saturation_mode_rps":{"get-json":100},"rated_mode_p99_at_target_rps":{},"latency_at_slo":{},"hdr_histogram_b64":{},"loadgen_cpu_p95":{},"sent_vs_handled_delta_pct":{},"cell_statuses":{"chain-mw5":"dnf"}}
		]
	}`
	var doc Document
	if err := json.Unmarshal([]byte(v53), &doc); err != nil {
		t.Fatalf("v5.3 document failed to decode under v5.4 schema: %v", err)
	}
	if doc.Benchmarks[0].CellRunStatuses != nil {
		t.Errorf("CellRunStatuses should be nil for a v5.3 doc, got %v", doc.Benchmarks[0].CellRunStatuses)
	}
	if doc.Benchmarks[0].CellStatuses["chain-mw5"] != "dnf" {
		t.Errorf("v5.3 cell_statuses lost in decode: %v", doc.Benchmarks[0].CellStatuses)
	}
}

// TestSplitMergeRoundTripWithCellStatuses asserts the four-file split is
// still lossless once CellStatuses is populated: MergeSplit of a
// SplitDocument's pieces reconstructs the original Document byte-for-byte
// under JSON, with CellStatuses carried through unchanged.
func TestSplitMergeRoundTripWithCellStatuses(t *testing.T) {
	doc := &Document{
		SchemaVersion: SchemaVersion,
		HostArchPair:  "linux/amd64",
		Environment:   Environment{KernelSysctlsApplied: []string{}, Fabric: "loopback"},
		Benchmarks: []ServerResult{
			{
				Name:                    "celeris-std-h1",
				SaturationModeRPS:       map[string]float64{"chain-mw5": 500000},
				RatedModeP99AtTargetRPS: map[string]time.Duration{},
				LatencyAtSLO:            map[string]map[int]int{},
				HdrHistogramB64:         map[string]string{"chain-mw5": "deadbeef"},
				LoadgenCPUP95:           map[string]float64{},
				SentVsHandledDeltaPct:   map[string]float64{},
			},
			{
				Name:                    "gin-h1",
				SaturationModeRPS:       map[string]float64{},
				RatedModeP99AtTargetRPS: map[string]time.Duration{},
				LatencyAtSLO:            map[string]map[int]int{},
				HdrHistogramB64:         map[string]string{},
				LoadgenCPUP95:           map[string]float64{},
				SentVsHandledDeltaPct:   map[string]float64{},
				CellStatuses:            map[string]string{"chain-mw5": string(CellNotApplicable)},
			},
		},
	}

	summary, hist, _ := SplitDocument(doc, SplitMeta{GeneratedAt: time.Unix(0, 0).UTC()})
	merged := MergeSplit(summary, hist)

	want, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal original: %v", err)
	}
	got, err := json.Marshal(merged)
	if err != nil {
		t.Fatalf("marshal merged: %v", err)
	}
	if !bytes.Equal(want, got) {
		t.Errorf("MergeSplit(SplitDocument(doc)) != doc\n want: %s\n got:  %s", want, got)
	}
	// CellStatuses specifically survives the round-trip.
	mergedByName := map[string]ServerResult{}
	for _, b := range merged.Benchmarks {
		mergedByName[b.Name] = b
	}
	if mergedByName["gin-h1"].CellStatuses["chain-mw5"] != string(CellNotApplicable) {
		t.Errorf("CellStatuses lost in split round-trip: %v", mergedByName["gin-h1"].CellStatuses)
	}
}
