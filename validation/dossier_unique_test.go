package validation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Two incidents of one predicate written in the same wall-clock second must
// get two dossiers. The directory was named <second>-<predicate>, so the
// second incident's incident.json, stderr tail and forensics overwrote the
// first's in place: the #588 round-2 container run's adaptive cell had two
// I-H2C-STALL incidents 1 ms apart and kept one dossier for them. The
// in-stall hook sends bursts like that by design, so this loses evidence.
func TestIncidentDossiersOfOneSecondNeverMerge(t *testing.T) {
	o := newTestOrch(t, "local")
	for attempt := 0; attempt < 5; attempt++ {
		d1, err := o.writeIncidentDossier(Incident{PredicateID: "I-H2C-STALL", Message: "first", ObservedAt: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		d2, err := o.writeIncidentDossier(Incident{PredicateID: "I-H2C-STALL", Message: "second", ObservedAt: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Base(d1)[:15] != filepath.Base(d2)[:15] {
			continue // the calls straddled a second boundary: not the case under test
		}
		if d1 == d2 {
			t.Fatalf("two incidents in one second share dossier %s: the second overwrote the first", filepath.Base(d1))
		}
		for dir, want := range map[string]string{d1: "first", d2: "second"} {
			var inc struct {
				Message string `json:"message"`
			}
			b, err := os.ReadFile(filepath.Join(dir, "incident.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(b, &inc); err != nil || inc.Message != want {
				t.Fatalf("%s: incident.json message %q (%v), want %q", filepath.Base(dir), inc.Message, err, want)
			}
			if got := filepath.Base(dir); got[len(got)-len("-I-H2C-STALL"):] != "-I-H2C-STALL" {
				t.Fatalf("dossier %s no longer ends in -<predicate> (report.CheckFaultControlCell and the dashboards match that suffix)", got)
			}
		}
		return
	}
	t.Skip("five attempts straddled a second boundary")
}

// A burst of in-stall triggers -- every read that started at the stall's
// onset crosses the threshold within the same millisecond -- is ONE stall and
// must cost ONE dossier. The orchestrator drains the channel between the
// timer callbacks, so each found it empty and the budget went on one moment:
// in the #588 round-2 container run the adaptive cell spent its h2c budget in
// the /ws hold and had none left for the / hold.
func TestStallDossierHookTakesOneDossierPerBurst(t *testing.T) {
	ch := make(chan Incident, 1)
	hook := newStallDossierHook(ch, func() int { return 1 }, 4)
	sent := 0
	for range 10 { // ten timers of one burst; the Run loop drains between them
		hook("h2c")
		select {
		case <-ch:
			sent++
		default:
		}
	}
	if sent != 1 {
		t.Fatalf("a burst of 10 simultaneous stall triggers took %d dossiers, want 1", sent)
	}
}
