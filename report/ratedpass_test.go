package report

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// probatorium#441: a rated pass is recorded whatever became of it, and only
// kept passes feed latency_at_slo and the headline.

func okPass(frac, target float64, p99 time.Duration, achieved float64) RatedPassRecord {
	return RatedPassRecord{Fraction: frac, TargetRPS: target, P99: p99, Status: RatedPassOK,
		AchievedRPS: achieved, AchievedRatio: achieved / target, Requests: int64(achieved * 30)}
}

func lostPass(frac, target float64, status, reason string) RatedPassRecord {
	return RatedPassRecord{Fraction: frac, TargetRPS: target, Status: status, Reason: reason}
}

func ratedCell(samples [][]RatedSample, recs [][]RatedPassRecord) CellResult {
	return CellResult{
		ScenarioName: "get-json", ServerName: "celeris-epoll-h1-sync", Category: "static",
		Samples:          makeSamples([]float64{400000, 400000}, time.Millisecond),
		RatedSamples:     samples,
		RatedPassRecords: recs,
	}
}

func TestMergeRatedPassRecordsOrdersAndMarksLegacy(t *testing.T) {
	kept := []RatedPassRecord{
		{TargetRPS: 200000, P99: 30 * time.Millisecond}, // pre-5.24: no status, no fraction
		okPass(0.25, 100000, 8*time.Millisecond, 99000),
		okPass(0.75, 300000, 40*time.Millisecond, 299000),
	}
	notKept := []RatedPassRecord{
		lostPass(0.9, 360000, RatedPassSkipped, RatedReasonZeroRequests),
		lostPass(0.5, 200000, RatedPassNotRun, RatedReasonLoadgenNew),
	}
	keptBefore := append([]RatedPassRecord(nil), kept...)

	got := MergeRatedPassRecords(kept, notKept)
	if len(got) != 5 {
		t.Fatalf("%d records, want 5", len(got))
	}
	// Ordered by fraction, then target: the legacy pass (no fraction) first,
	// and a lost pass sits between the kept ones, not after them.
	var fr []float64
	for _, r := range got {
		fr = append(fr, r.Fraction)
	}
	if !reflect.DeepEqual(fr, []float64{0, 0.25, 0.5, 0.75, 0.9}) {
		t.Errorf("order by fraction = %v", fr)
	}
	if got[0].Status != "" || !got[0].Kept() {
		t.Errorf("a kept pass from a run that predates 5.24 has status %q (kept=%v), want it left empty and still kept: nothing is known about the passes that run dropped", got[0].Status, got[0].Kept())
	}
	if got[0].AchievedRPS != 0 {
		t.Errorf("the legacy pass was given an achieved rate (%v) nobody measured", got[0].AchievedRPS)
	}
	if !reflect.DeepEqual(kept, keptBefore) {
		t.Error("MergeRatedPassRecords modified its input")
	}
	if MergeRatedPassRecords(nil, nil) != nil {
		t.Error("no passes must give nil, not an empty list")
	}
}

func TestRatedPassKept(t *testing.T) {
	for _, c := range []struct {
		status string
		want   bool
	}{{"", true}, {RatedPassOK, true}, {RatedPassSkipped, false}, {RatedPassNotRun, false}} {
		if got := (RatedPassRecord{Status: c.status}).Kept(); got != c.want {
			t.Errorf("Kept(%q) = %v, want %v", c.status, got, c.want)
		}
	}
}

// TestAggregateListsEveryPassAndNamesTheHeadline: two runs, the first lost its
// 0.9 pass. The document lists all passes of both runs (run numbered among the
// runs that had rated data), the headline p99 is the highest KEPT target's, and
// rated_headline_target_rps says which, while latency_at_slo never sees the
// lost pass.
func TestAggregateListsEveryPassAndNamesTheHeadline(t *testing.T) {
	run0 := []RatedPassRecord{
		okPass(0.25, 100000, 8*time.Millisecond, 99000),
		okPass(0.5, 200000, 30*time.Millisecond, 190000),
		lostPass(0.9, 360000, RatedPassSkipped, RatedReasonZeroRequests),
	}
	run1 := []RatedPassRecord{
		okPass(0.25, 100000, 9*time.Millisecond, 100000),
		okPass(0.5, 200000, 31*time.Millisecond, 200000),
		okPass(0.9, 360000, 700*time.Millisecond, 301000),
	}
	cell := ratedCell(
		[][]RatedSample{
			{{TargetRPS: 100000, P99: 8 * time.Millisecond}, {TargetRPS: 200000, P99: 30 * time.Millisecond}},
			{{TargetRPS: 100000, P99: 9 * time.Millisecond}, {TargetRPS: 200000, P99: 31 * time.Millisecond}, {TargetRPS: 360000, P99: 700 * time.Millisecond}},
		},
		[][]RatedPassRecord{run0, nil, run1}, // a middle run without rated data adds nothing and takes no number
	)
	agg := Aggregate([]CellResult{cell})["get-json/celeris-epoll-h1-sync"]
	if len(agg.RatedPasses) != 6 {
		t.Fatalf("%d passes listed, want 6", len(agg.RatedPasses))
	}
	for i, p := range agg.RatedPasses {
		if want := i / 3; p.Run != want {
			t.Errorf("pass %d is run %d, want %d", i, p.Run, want)
		}
	}
	if agg.RatedPasses[2].Status != RatedPassSkipped || agg.RatedPasses[2].Reason != RatedReasonZeroRequests {
		t.Errorf("the lost pass = %+v", agg.RatedPasses[2])
	}
	// median over the runs that kept the pass: the 0.9 target has one sample (700 ms).
	if agg.RatedHeadlineTargetRPS != 360000 {
		t.Errorf("headline target = %d, want 360000 (run 1 kept its 0.9 pass)", agg.RatedHeadlineTargetRPS)
	}

	doc := BuildDocument(BuildInput{HostArchPair: "linux/amd64", Agg: map[string]CellAggregate{"get-json/celeris-epoll-h1-sync": agg}})
	sr := doc.Benchmarks[0]
	if got := len(sr.RatedPasses["get-json"]); got != 6 {
		t.Errorf("document lists %d passes, want 6", got)
	}
	if sr.RatedHeadlineTargetRPS["get-json"] != 360000 {
		t.Errorf("rated_headline_target_rps = %v", sr.RatedHeadlineTargetRPS)
	}
}

// TestLostTopPassDoesNotMoveTheHeadline: the only run lost its 0.9 pass. The
// headline stays the 0.5 pass's (as before 5.24) and the document names that
// target, so a reader can see the label "at target" is not the top fraction.
// The lost pass's target never enters latency_at_slo.
func TestLostTopPassDoesNotMoveTheHeadline(t *testing.T) {
	recs := []RatedPassRecord{
		okPass(0.25, 100000, 8*time.Millisecond, 99000),
		okPass(0.5, 200000, 30*time.Millisecond, 190000),
		lostPass(0.9, 360000, RatedPassNotRun, RatedReasonLoadgenNew),
	}
	cell := ratedCell(
		[][]RatedSample{{{TargetRPS: 100000, P99: 8 * time.Millisecond}, {TargetRPS: 200000, P99: 30 * time.Millisecond}}},
		[][]RatedPassRecord{recs})
	agg := Aggregate([]CellResult{cell})["get-json/celeris-epoll-h1-sync"]
	doc := BuildDocument(BuildInput{HostArchPair: "linux/amd64", Agg: map[string]CellAggregate{"x": agg}})
	sr := doc.Benchmarks[0]
	if sr.RatedModeP99AtTargetRPS["get-json"] != 30*time.Millisecond {
		t.Errorf("headline p99 = %v, want 30ms", sr.RatedModeP99AtTargetRPS["get-json"])
	}
	if sr.RatedHeadlineTargetRPS["get-json"] != 200000 {
		t.Errorf("headline target = %v, want 200000: the 0.9 pass was lost", sr.RatedHeadlineTargetRPS["get-json"])
	}
	for ms, rps := range sr.LatencyAtSLO["get-json"] {
		if rps == 360000 {
			t.Errorf("latency_at_slo[%d] = 360000: a pass that never ran was counted", ms)
		}
	}
	// The lost pass is listed.
	if l := sr.RatedPasses["get-json"]; len(l) != 3 || l[2].Status != RatedPassNotRun || l[2].Reason != RatedReasonLoadgenNew {
		t.Errorf("passes = %+v", l)
	}
}

// TestCellWithOnlyLostPassesKeepsItsRecordsAndNoRatedNumbers: no pass was kept.
func TestCellWithOnlyLostPassesKeepsItsRecordsAndNoRatedNumbers(t *testing.T) {
	cell := ratedCell(nil, [][]RatedPassRecord{{
		lostPass(0.25, 100000, RatedPassSkipped, RatedReasonZeroRequests),
		lostPass(0.5, 200000, RatedPassSkipped, RatedReasonZeroRequests),
	}})
	agg := Aggregate([]CellResult{cell})["get-json/celeris-epoll-h1-sync"]
	doc := BuildDocument(BuildInput{HostArchPair: "linux/amd64", Agg: map[string]CellAggregate{"x": agg}})
	sr := doc.Benchmarks[0]
	if len(sr.RatedPasses["get-json"]) != 2 {
		t.Errorf("passes = %+v, want the 2 skipped records", sr.RatedPasses)
	}
	if _, ok := sr.RatedModeP99AtTargetRPS["get-json"]; ok {
		t.Error("a headline p99 for a cell with no kept pass")
	}
	if _, ok := sr.RatedHeadlineTargetRPS["get-json"]; ok {
		t.Error("a headline target for a cell with no kept pass")
	}
	if _, ok := sr.LatencyAtSLO["get-json"]; ok {
		t.Error("latency_at_slo for a cell with no kept pass")
	}
}

// TestSchema524RatedPasses: the new fields validate against schema_v5.json,
// round-trip, survive SplitDocument into summary.json, and are absent from a
// document that has none, so every older document and reader is unaffected.
func TestSchema524RatedPasses(t *testing.T) {
	if SchemaVersion != "5.24" {
		t.Fatalf("SchemaVersion %q, want 5.24", SchemaVersion)
	}
	sch := loadSchema(t)
	t0 := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	full := okPass(0.5, 200000, 30*time.Millisecond, 190000)
	full.Errors, full.ConnectErrors, full.DialRetries, full.CloseAborts = 7, 2, 1, 3
	full.ShortfallRequests, full.Duration, full.P50, full.P999, full.Max = 300000, 30*time.Second, time.Millisecond, 40*time.Millisecond, 90*time.Millisecond
	full.Start, full.End, full.LoadgenVersion = t0, t0.Add(50*time.Second), "v1.4.14"
	lost := lostPass(0.9, 360000, RatedPassSkipped, RatedReasonRunError)
	lost.Detail = "pass aborted"

	doc := minimalDoc(t)
	doc.Benchmarks = []ServerResult{{
		Name: "celeris-epoll-h1-sync", Category: "celeris", Language: "go", LanguageVersion: "1.27", Framework: "celeris", FrameworkVersion: "v1.6.0",
		CompileOptions:          []string{},
		SaturationModeRPS:       map[string]float64{"get-json": 400000},
		RatedModeP99AtTargetRPS: map[string]time.Duration{"get-json": 30 * time.Millisecond},
		RatedHeadlineTargetRPS:  map[string]int{"get-json": 200000},
		RatedPasses: map[string][]RatedPassResult{"get-json": {
			{Run: 0, RatedPassRecord: full}, {Run: 0, RatedPassRecord: lost}}},
		LatencyAtSLO:          map[string]map[int]int{"get-json": {50: 200000}},
		HdrHistogramB64:       map[string]string{},
		LoadgenCPUP95:         map[string]float64{},
		SentVsHandledDeltaPct: map[string]float64{},
	}}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	validateAny(t, sch, raw)
	var back Document
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back.Benchmarks[0].RatedPasses, doc.Benchmarks[0].RatedPasses) ||
		back.Benchmarks[0].RatedHeadlineTargetRPS["get-json"] != 200000 {
		t.Errorf("round trip lost the rated passes: %+v", back.Benchmarks[0])
	}

	// summary.json is the split document with the HDR blobs out: the passes stay.
	summary, _, _ := SplitDocument(&doc, splitTestMeta())
	if got := len(summary.Benchmarks[0].RatedPasses["get-json"]); got != 2 {
		t.Errorf("summary.json carries %d rated passes, want 2", got)
	}

	// A document with no rated record emits neither key; a 5.23 document
	// without them validates and decodes.
	plain := minimalDoc(t)
	plain.SchemaVersion = "5.23"
	rawPlain, _ := json.Marshal(plain)
	for _, k := range []string{"rated_passes", "rated_headline_target_rps"} {
		if strings.Contains(string(rawPlain), k) {
			t.Errorf("%s is serialised for a document without rated passes", k)
		}
	}
	validateAny(t, sch, rawPlain)
}

// A schema rejects a status it does not know, so a typo in the runner cannot
// publish a pass no reader understands.
func TestSchema524RejectsAnUnknownStatus(t *testing.T) {
	sch := loadSchema(t)
	doc := minimalDoc(t)
	doc.Benchmarks = []ServerResult{{
		Name: "x", Category: "c", Language: "go", LanguageVersion: "1", Framework: "f", FrameworkVersion: "1",
		CompileOptions: []string{}, SaturationModeRPS: map[string]float64{}, RatedModeP99AtTargetRPS: map[string]time.Duration{},
		LatencyAtSLO: map[string]map[int]int{}, HdrHistogramB64: map[string]string{}, LoadgenCPUP95: map[string]float64{},
		SentVsHandledDeltaPct: map[string]float64{},
		RatedPasses:           map[string][]RatedPassResult{"get-json": {{RatedPassRecord: RatedPassRecord{TargetRPS: 1, Status: "lost"}}}},
	}}
	raw, _ := json.Marshal(doc)
	var v any
	_ = json.Unmarshal(raw, &v)
	if err := sch.Validate(v); err == nil {
		t.Fatal("the schema accepted status \"lost\"")
	}
}
