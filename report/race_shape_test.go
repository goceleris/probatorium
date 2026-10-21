package report

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func shapedCell() ValidationCellResult {
	c := cleanCell("auth_session_ratelimit", "adaptive", "amd64")
	c.Tier1.LoadShaping = &LoadShaping{Reason: "race build", Walkers: 110, MarkovWalkers: 67, MarkovThinkMs: 4}
	return c
}

// Schema 5.24 is additive: tier_1.load_shaping validates, round-trips and is
// omitted when the cell was not shaped, so every other cell's document differs
// from before by the version string alone.
func TestSchema524LoadShapingRoundTripsAndIsOmittedWhenAbsent(t *testing.T) {
	if SchemaVersion != "5.24" {
		t.Fatalf("SchemaVersion %q, want 5.24", SchemaVersion)
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
	if ls == nil || ls.Walkers != 110 || ls.MarkovWalkers != 67 || ls.MarkovThinkMs != 4 || ls.Reason != "race build" {
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
// fraction of its load. The same I-RACE test the SLOW tolerance uses.
func TestGateFailsAShapedCellThatShowsNoRaceBuild(t *testing.T) {
	c := shapedCell()
	c.PropertiesNotInstrumented = []string{"I-RACE"}
	v := Gate([]ValidationCellResult{c}, nil, raceOpts())
	if len(v) != 1 || v[0].Field != "tier_1.load_shaping" || !strings.Contains(v[0].Why, "-race build") {
		t.Fatalf("want exactly the tier_1.load_shaping violation, got %v", v)
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

// If the pause stopped a cell from promoting, the cell says so: the gate already
// fails a missed promotion (VALIDATE_GATE_EXPECT_ADAPTIVE_SWITCH, on in every
// default race run); a shaped cell adds that its walkers were paced, so the
// reader's first suspect is the right one. An unshaped cell's wording is the
// literal it was.
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
	shaped := plain
	shaped.Tier1.LoadShaping = &LoadShaping{Reason: "race build", Walkers: 110, MarkovWalkers: 67, MarkovThinkMs: 4}
	sv := Gate([]ValidationCellResult{shaped}, nil, opts)
	if len(sv) != 1 || sv[0].Field != "tier_1.adaptive_switches" {
		t.Fatalf("a shaped cell that never promoted must fail on adaptive_switches, got %v", sv)
	}
	if !strings.HasPrefix(sv[0].Why, wantPlain) || !strings.Contains(sv[0].Why, "4 ms") || !strings.Contains(sv[0].Why, "load shaping") {
		t.Errorf("a shaped cell's failure must name the shaping after the unchanged text: %q", sv[0].Why)
	}
}
