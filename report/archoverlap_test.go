package report

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func win(comp, scn, kind, class string, startS, endS int) LoadWindow {
	return LoadWindow{Competitor: comp, Scenario: scn, Kind: kind, Class: class,
		Start: t0.Add(time.Duration(startS) * time.Second), End: t0.Add(time.Duration(endS) * time.Second)}
}

// cleanRun is a both-arch run where the arches take turns on the heavy row and
// only share time on light ones: the fixture a correct run looks like.
func cleanRun() (x86, arm []LoadWindow) {
	x86 = []LoadWindow{
		win("celeris", "ws-large-echo", LoadKindSaturation, "heavy", 0, 110),
		win("celeris", "ws-large-echo", LoadKindRated, "heavy", 110, 160),
		win("celeris", "driver-pg-read", LoadKindSaturation, "light", 165, 275),
	}
	arm = []LoadWindow{
		win("celeris", "driver-pg-read", LoadKindSaturation, "light", 0, 110),
		win("celeris", "ws-large-echo", LoadKindSaturation, "heavy", 160, 270), // starts the instant x86's heavy cell ends: touching, not overlapping
		win("celeris", "ws-large-echo", LoadKindRated, "heavy", 270, 320),
	}
	return
}

// TestCheckArchOverlapFixtures is the self-test of the analyzer (probatorium#442):
// a clean pair reports nothing, and a pair with ONE overlapping heavy pass
// reports exactly that pass and nothing else.
func TestCheckArchOverlapFixtures(t *testing.T) {
	x86, arm := cleanRun()
	rep := CheckArchOverlap(x86, arm, ExclusionHeavy)
	if !rep.Clean() || rep.Total != 0 {
		t.Fatalf("clean fixture reported overlaps:\n%s", rep)
	}
	if rep.HeavyHeavy != 0 || rep.HeavyLight != 3 {
		// the light x86 driver pass (165..275) shares time with both arm64 heavy passes, and
		// the light arm64 driver pass (0..110) with x86's saturation pass: the accepted residual.
		t.Fatalf("heavy+heavy %d heavy+light %d, want 0 and 3", rep.HeavyHeavy, rep.HeavyLight)
	}

	// One heavy pass slides into the peer's heavy cell by 10 s.
	bad := append([]LoadWindow(nil), x86...)
	bad[1] = win("celeris", "ws-large-echo", LoadKindRated, "heavy", 110, 170)
	rep = CheckArchOverlap(bad, arm, ExclusionHeavy)
	if rep.Total != 1 || rep.Clean() {
		t.Fatalf("one overlapping heavy pass: %d violations, want 1\n%s", rep.Total, rep)
	}
	v := rep.Violations[0]
	if v.Overlap != 10*time.Second || v.A.Kind != LoadKindRated || v.B.Kind != LoadKindSaturation {
		t.Fatalf("violation = %+v, want the 10 s overlap of x86's rated pass with arm64's saturation pass", v)
	}
	if !strings.Contains(rep.String(), "overlap 10s") {
		t.Fatalf("report text lacks the overlap:\n%s", rep)
	}
}

// TestCheckArchOverlapPolicies: heavy ignores light-vs-anything, all forbids
// every overlap, off forbids none but still counts.
func TestCheckArchOverlapPolicies(t *testing.T) {
	a := []LoadWindow{win("c", "post-4k", LoadKindSaturation, "heavy", 0, 100)}
	light := []LoadWindow{win("c", "driver-pg-read", LoadKindSaturation, "light", 50, 150)}
	heavy := []LoadWindow{win("c", "get-json", LoadKindSaturation, "heavy", 50, 150)}

	if r := CheckArchOverlap(a, light, ExclusionHeavy); r.Total != 0 || r.HeavyLight != 1 {
		t.Errorf("heavy vs light under policy heavy: %+v, want 0 violations and 1 heavy+light", r)
	}
	if r := CheckArchOverlap(a, light, ExclusionAll); r.Total != 1 {
		t.Errorf("heavy vs light under policy all: %d violations, want 1", r.Total)
	}
	if r := CheckArchOverlap(a, heavy, ExclusionHeavy); r.Total != 1 || r.HeavyHeavy != 1 {
		t.Errorf("heavy vs heavy under policy heavy: %+v, want 1", r)
	}
	if r := CheckArchOverlap(a, heavy, ExclusionOff); r.Total != 0 || r.HeavyHeavy != 1 {
		t.Errorf("policy off: %+v, want 0 violations but the overlap counted", r)
	}
	// light vs light is never forbidden under heavy
	l2 := []LoadWindow{win("c", "driver-redis-get", LoadKindSaturation, "light", 0, 100)}
	if r := CheckArchOverlap(l2, light, ExclusionHeavy); r.Total != 0 || r.LightLight != 1 {
		t.Errorf("light vs light: %+v", r)
	}
	// an unclassified window counts as heavy
	u := []LoadWindow{win("c", "mystery", LoadKindSaturation, "", 0, 100)}
	if r := CheckArchOverlap(u, heavy, ExclusionHeavy); r.Total != 1 {
		t.Errorf("an unclassified window must count as heavy: %+v", r)
	}
}

// TestCheckArchOverlapMissingTimestampsAreNotClean: a window with a zero or
// inverted bound cannot be checked, and a run with one is not verified clean.
func TestCheckArchOverlapMissingTimestampsAreNotClean(t *testing.T) {
	x86, arm := cleanRun()
	x86 = append(x86, LoadWindow{Competitor: "c", Scenario: "get-json", Kind: LoadKindSaturation, Class: "heavy"})
	x86 = append(x86, win("c", "get-json", LoadKindRated, "heavy", 50, 40))
	rep := CheckArchOverlap(x86, arm, ExclusionHeavy)
	if rep.MissingA != 2 || rep.Clean() {
		t.Fatalf("missing = %d clean = %v, want 2 and false", rep.MissingA, rep.Clean())
	}
}

// TestArchExclusionRecordAndGate: the record stamped into the documents, and
// the problem the publish gate reads from it.
func TestArchExclusionRecordAndGate(t *testing.T) {
	x86, arm := cleanRun()
	ok := ArchExclusionFor(ExclusionHeavy, "arm64", "forward", x86, arm, 12.5)
	if ok.Violations != 0 || ok.Checked != 3 || ok.PeerChecked != 3 || ok.LockWaitSeconds != 12.5 || ok.Problem() != "" {
		t.Fatalf("clean record = %+v problem %q", ok, ok.Problem())
	}
	bad := append([]LoadWindow(nil), x86...)
	bad[1] = win("celeris", "ws-large-echo", LoadKindRated, "heavy", 110, 170)
	rec := ArchExclusionFor(ExclusionHeavy, "arm64", "forward", bad, arm, 0)
	if rec.Violations != 1 || !strings.Contains(rec.Problem(), "violated") {
		t.Fatalf("bad record = %+v problem %q", rec, rec.Problem())
	}
	if p := (&ArchExclusion{Policy: ExclusionOff}).Problem(); !strings.Contains(p, "off") {
		t.Fatalf("policy off must be a problem, got %q", p)
	}
	if p := (&ArchExclusion{Policy: ExclusionHeavy, MissingTimestamps: 3}).Problem(); !strings.Contains(p, "not verified") {
		t.Fatalf("missing timestamps must be a problem, got %q", p)
	}
	var none *ArchExclusion
	if none.Problem() != "" {
		t.Fatal("a document with no verdict (single-arch, or before 5.22) is not refused by the verdict")
	}
}

// TestArchExclusionSchemaAndSplit: both fields validate against schema_v5.json,
// survive a round trip, and the published split keeps the verdict (env.json)
// but drops the windows (summary.json).
func TestArchExclusionSchemaAndSplit(t *testing.T) {
	t.Parallel()
	sch := loadSchema(t)
	x86, arm := cleanRun()
	doc := minimalDoc(t)
	doc.LoadWindows = x86
	doc.Environment.ArchExclusion = ArchExclusionFor(ExclusionHeavy, "arm64", "forward", x86, arm, 3)
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	validateAny(t, sch, raw)

	var back Document
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.LoadWindows) != len(x86) || back.Environment.ArchExclusion == nil || back.Environment.ArchExclusion.PeerArch != "arm64" {
		t.Fatalf("round trip lost the fields: %+v", back.Environment.ArchExclusion)
	}

	summary, _, env := SplitDocument(&doc, SplitMeta{Version: "v1.6.0", Arch: "amd64", Date: "20261020", RunID: DefaultRunID})
	if len(summary.LoadWindows) != 0 {
		t.Errorf("summary.json keeps %d load windows, want none", len(summary.LoadWindows))
	}
	if env.Environment.ArchExclusion == nil || env.Environment.ArchExclusion.Policy != ExclusionHeavy {
		t.Errorf("env.json lost the arch_exclusion verdict: %+v", env.Environment.ArchExclusion)
	}
	if len(doc.LoadWindows) == 0 {
		t.Error("SplitDocument modified its input")
	}
}

// TestSingleArchDocumentHasNeitherField: a document with no verdict carries
// neither key, so a pre-5.22 document and a single-arch run read the same.
func TestSingleArchDocumentHasNeitherField(t *testing.T) {
	raw, err := json.Marshal(minimalDoc(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"arch_exclusion", "load_windows"} {
		if strings.Contains(string(raw), key) {
			t.Errorf("a document with no verdict serialises %q", key)
		}
	}
}

// TestArchExclusionNothingCheckedIsNotClean: an arch with no windows at all
// shows no overlap to a naive count. The verdict must say it checked nothing,
// and the gate must read that as a problem (policy off aside, which is its own).
func TestArchExclusionNothingCheckedIsNotClean(t *testing.T) {
	x86, _ := cleanRun()
	for name, rec := range map[string]*ArchExclusion{
		"peer has no windows": ArchExclusionFor(ExclusionHeavy, "arm64", "forward", x86, nil, 0),
		"own has no windows":  ArchExclusionFor(ExclusionHeavy, "arm64", "forward", nil, x86, 0),
		"neither":             ArchExclusionFor(ExclusionAll, "arm64", "forward", nil, nil, 0),
	} {
		if rec.Violations != 0 {
			t.Fatalf("%s: %d violations from nothing", name, rec.Violations)
		}
		if p := rec.Problem(); !strings.Contains(p, "not verified") {
			t.Errorf("%s: Problem() = %q, want a not-verified problem", name, p)
		}
	}
}
