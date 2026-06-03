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
// they agree on field width. The split is load-bearing: "zero-request" /
// "capability-lie" are the adapter not implementing the route (N/A);
// "read server settings" is N/A only when it timed out (server never spoke
// H2), but DNF on reset/EOF (an H2 server that crashed mid-handshake);
// everything else is an infra failure (DNF); ambiguous defaults to DNF,
// never N/A.
func TestClassifyCellError(t *testing.T) {
	cases := []struct {
		name string
		err  string
		want CellStatus
	}{
		{"empty is ok", "", CellOK},
		{"zero-request is n/a", "zero-request cell: errors=10 duration=20s", CellNotApplicable},
		{"capability-lie is n/a", `capability-lie: scheduled chain scenario "chain-mw5" got high error ratio from gin-h1 (errors=999000/requests=1000000) — adapter declared the capability but did not serve the route`, CellNotApplicable},
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
	var mergedByName map[string]ServerResult = map[string]ServerResult{}
	for _, b := range merged.Benchmarks {
		mergedByName[b.Name] = b
	}
	if mergedByName["gin-h1"].CellStatuses["chain-mw5"] != string(CellNotApplicable) {
		t.Errorf("CellStatuses lost in split round-trip: %v", mergedByName["gin-h1"].CellStatuses)
	}
}
