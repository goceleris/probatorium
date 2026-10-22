package report

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func shapedCell() ValidationCellResult {
	c := cleanCell("auth_session_ratelimit", "adaptive", "amd64")
	c.Tier1.LoadShaping = &LoadShaping{Reason: "race build", Walkers: 110, MarkovWalkers: 67, MarkovMinPeriodUs: 6500}
	return c
}

// tier_1.load_shaping is additive and does NOT bump the schema: it validates,
// round-trips and is omitted when the cell was not shaped, so every other tier's
// document is byte-identical to before (a bump would re-stamp schema_version in
// all of them). The field's presence is what marks a shaped cell.
func TestLoadShapingRoundTripsAndIsOmittedWhenAbsent(t *testing.T) {
	if SchemaVersion != "5.23" {
		t.Fatalf("SchemaVersion %q, want 5.23: shaping must not re-stamp the documents of tiers it does not touch", SchemaVersion)
	}
	sch := loadSchema(t)
	now := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	doc := minimalDoc(t)
	doc.Validation = &ValidationResults{StartedAt: now, FinishedAt: now.Add(time.Minute),
		Cells: []ValidationCellResult{shapedCell(), cleanCell("a", "std", "amd64")}}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	validateAny(t, sch, raw)
	var back Document
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	ls := back.Validation.Cells[0].Tier1.LoadShaping
	if ls == nil || ls.Walkers != 110 || ls.MarkovWalkers != 67 || ls.MarkovMinPeriodUs != 6500 || ls.Reason != "race build" {
		t.Errorf("round trip lost the record: %+v", ls)
	}
	if back.Validation.Cells[1].Tier1.LoadShaping != nil {
		t.Error("an unshaped cell gained a record")
	}
	// Exactly one cell serialises the key.
	if n := strings.Count(string(raw), `"load_shaping"`); n != 1 {
		t.Errorf("load_shaping serialised %d times for a document with one shaped cell, want 1", n)
	}
}

// A shaped cell must be a race-built cell: shaping is the race tier's, and a
// cell that was paced without the detector would be a nightly validated at a
// fraction of its load. The same I-RACE test the SLOW tolerance uses. The two
// ways to fail it have different causes and say so, word for word.
func TestGateFailsAShapedCellThatShowsNoRaceBuild(t *testing.T) {
	// The property loop ran and did not declare I-RACE: not a -race build.
	c := shapedCell()
	c.PropertiesNotInstrumented = []string{"I-RACE"}
	v := Gate([]ValidationCellResult{c}, nil, raceOpts())
	if len(v) != 1 || v[0].Field != "tier_1.load_shaping" || v[0].Value != 6500 || v[0].Why != loadShapingNoRaceWhy {
		t.Fatalf("want exactly the tier_1.load_shaping violation with value 6500 and the no-race-build reason, got %v", v)
	}
	if want := "this cell's Markov walkers were paced (load shaping, a race-tier measure for the detector's slowdown) but the cell does not show a -race build (I-RACE not declared by its property loop): it was validated at a fraction of the load it is gated for"; v[0].Why != want {
		t.Errorf("no-race-build reason changed:\n got %q\nwant %q", v[0].Why, want)
	}

	// The property loop never ran (the refapp died, /debug/vars unreachable):
	// that is not a refapp without the detector, and must not say so.
	n := shapedCell()
	n.Tier1.PropertyEvaluations, n.Tier1.PropertySkips = 0, 0
	var rows []Violation
	for _, x := range Gate([]ValidationCellResult{n}, nil, raceOpts()) {
		if x.Field == "tier_1.load_shaping" {
			rows = append(rows, x)
		}
	}
	if len(rows) != 1 || rows[0].Value != 6500 || rows[0].Why != loadShapingNoLoopWhy {
		t.Fatalf("want exactly one tier_1.load_shaping row with the property-loop reason, got %v", rows)
	}
	if want := "this cell's Markov walkers were paced (load shaping, a race-tier measure for the detector's slowdown) but its property loop did not run, so I-RACE could not be declared and the cell cannot be shown to be a -race build"; rows[0].Why != want {
		t.Errorf("property-loop reason changed:\n got %q\nwant %q", rows[0].Why, want)
	}
	if strings.Contains(rows[0].Why, "does not show a -race build") {
		t.Error("a cell whose property loop did not run is accused of lacking the detector")
	}

	// The rule is the record's, not the engine's: a shaped cell of any engine
	// is held to it.
	e := shapedCell()
	e.Engine = "epoll"
	e.PropertiesNotInstrumented = []string{"I-RACE"}
	if v := Gate([]ValidationCellResult{e}, nil, raceOpts()); len(v) != 1 || v[0].Field != "tier_1.load_shaping" || v[0].Engine != "epoll" {
		t.Errorf("a shaped cell of another engine without I-RACE must fail the rule, got %v", v)
	}

	// Declared I-RACE: clean, and an unshaped cell without I-RACE is not this rule's business.
	if v := Gate([]ValidationCellResult{shapedCell()}, nil, raceOpts()); len(v) != 0 {
		t.Errorf("a shaped race cell with I-RACE declared must pass, got %v", v)
	}
	u := cleanCell("auth_session_ratelimit", "adaptive", "amd64")
	u.PropertiesNotInstrumented = []string{"I-RACE"}
	for _, x := range Gate([]ValidationCellResult{u}, nil, GateOptions{}) {
		if x.Field == "tier_1.load_shaping" {
			t.Errorf("an unshaped cell tripped the shaping rule: %v", x)
		}
	}
}

// If the pacing stopped a cell from promoting, the cell says so: the gate already
// fails a missed promotion (VALIDATE_GATE_EXPECT_ADAPTIVE_SWITCH, on in every
// default race run); a shaped cell adds that its walkers were paced, so the
// reader's first suspect is the right one. An unshaped cell's wording is the
// literal it was, and the shaped one is that literal plus an exact sentence.
func TestMissedPromotionOfAShapedCellNamesTheShaping(t *testing.T) {
	opts := GateOptions{RequireTier3: true, RequireProperties: true, ExpectAdaptiveSwitch: true}
	plain := cleanCell("auth_session_ratelimit", "adaptive", "amd64")
	plain.Tier1.AdaptiveSwitches = 0
	plain.Tier1.PeakConnsPerWorker = 31.5
	plain.Tier1.MeanBytesPerReq = 1234
	pv := Gate([]ValidationCellResult{plain}, nil, opts)
	if len(pv) != 1 || pv[0].Field != "tier_1.adaptive_switches" {
		t.Fatalf("premise: want the adaptive_switches violation, got %v", pv)
	}
	const wantPlain = "this tier expects every adaptive cell to promote at least once (VALIDATE_GATE_EXPECT_ADAPTIVE_SWITCH) and the engine never left its start engine; the cell validated epoll, not the adaptive path. Offered load, as the property loop measured it: peak 31.5 conns/worker, mean 1234 bytes/req -- the controller's own two signals, so a peak below its conns/worker threshold is a sizing bug here and a bytes/req above its large-payload threshold is a deliberate suppression, not a defect"
	if pv[0].Why != wantPlain {
		t.Errorf("the unshaped wording changed:\n got %q\nwant %q", pv[0].Why, wantPlain)
	}
	for _, tc := range []struct {
		us   int64
		want string
	}{
		{6500, ". This cell's load was shaped (load shaping, tier_1.load_shaping): 67 of its 110 walkers started a request at most once every 6.5 ms while the walker, worker and connection counts were unchanged, so look at that pacing first -- the controller also refuses to promote when a tick's bytes/req is large, and a fleet paced below the echo's bytes gives it that"},
		{7000, ". This cell's load was shaped (load shaping, tier_1.load_shaping): 67 of its 110 walkers started a request at most once every 7 ms while the walker, worker and connection counts were unchanged, so look at that pacing first -- the controller also refuses to promote when a tick's bytes/req is large, and a fleet paced below the echo's bytes gives it that"},
	} {
		shaped := plain
		t1 := *plain.Tier1
		t1.LoadShaping = &LoadShaping{Reason: "race build", Walkers: 110, MarkovWalkers: 67, MarkovMinPeriodUs: tc.us}
		shaped.Tier1 = &t1
		sv := Gate([]ValidationCellResult{shaped}, nil, opts)
		if len(sv) != 1 || sv[0].Field != "tier_1.adaptive_switches" {
			t.Fatalf("a shaped cell that never promoted must fail on adaptive_switches, got %v", sv)
		}
		if sv[0].Why != wantPlain+tc.want {
			t.Errorf("period %d us: the shaped failure is not the unchanged text plus the exact sentence:\n got %q\nwant %q", tc.us, sv[0].Why, wantPlain+tc.want)
		}
	}
}
