package checker

import (
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/goceleris/probatorium/report"
	"github.com/goceleris/probatorium/validation/properties"
)

// The witness set grows every time a defect earns a counter, and it is
// written down in two places that must agree: report.ZeroWitnessMeaning, which says what
// each one means, and recordZeroWitnesses, which reads it off the snapshot.
// Adding to one and not the other is silent -- the gate would either judge a
// counter nobody records, or record one nobody judges.
//
// celeris#627 is why this test is reflective rather than a list. A
// field-by-field struct literal dropped ten fields there, two of them the
// celeris#484 witnesses, so that detector reported a structural zero on the
// adaptive engine and could never have fired. A test that enumerates today's
// names repeats the same mistake one file over.
func TestEveryZeroWitnessIsBothDeclaredAndRecorded(t *testing.T) {
	// Every witness field is set, so a key the recorder writes but nobody
	// declares actually lands in the map. With an all-zero snapshot the
	// recorder writes nothing at all and this test is vacuous -- it was,
	// on the first attempt, and passed against a deliberately misspelled
	// declaration.
	e := NewEvaluator(nil)
	e.recordZeroWitnesses(properties.Snapshot{
		EngineTransplantAdoptSlotOccupied: 1,
		EngineCloseMissingConnState:       1,
		EngineRecvDoubleArmed:             1,
		EngineRecvCQEUnaccounted:          1,
		EngineRecvSQFull:                  1,
		EngineRecvStallEpisodes:           1,
	})
	recorded := e.Tally().EngineZeroWitness

	declared := make([]string, 0, len(report.ZeroWitnessMeaning))
	for k := range report.ZeroWitnessMeaning {
		declared = append(declared, k)
	}
	got := make([]string, 0, len(recorded))
	for k := range recorded {
		got = append(got, k)
	}
	sort.Strings(declared)
	sort.Strings(got)
	if !reflect.DeepEqual(declared, got) {
		t.Fatalf("report.ZeroWitnessMeaning and recordZeroWitnesses disagree:\n  declared: %v\n  recorded: %v", declared, got)
	}
	for k, why := range report.ZeroWitnessMeaning {
		if why == "" {
			t.Errorf("witness %q has no meaning; the gate would print a bare counter name", k)
		}
	}
}

// Each witness must read its OWN snapshot field. Two keys reading the same
// field is the copy-paste failure this block invites, and it is invisible
// from the outside: the duplicated counter would simply never move, which
// reads exactly like a clean run.
func TestEachZeroWitnessReadsItsOwnField(t *testing.T) {
	const sentinel = 7

	// field name on properties.Snapshot -> witness key it must feed.
	wiring := map[string]string{
		"EngineTransplantAdoptSlotOccupied": "engine_transplant_adopt_slot_occupied",
		"EngineCloseMissingConnState":       "engine_close_missing_conn_state",
		"EngineRecvDoubleArmed":             "engine_recv_double_armed",
		"EngineRecvCQEUnaccounted":          "engine_recv_cqe_unaccounted",
		"EngineRecvSQFull":                  "engine_recv_sq_full",
		"EngineRecvStallEpisodes":           "engine_recv_stall_episodes",
	}
	if len(wiring) != len(report.ZeroWitnessMeaning) {
		t.Fatalf("this test covers %d witnesses but %d are declared -- a new witness needs a case here",
			len(wiring), len(report.ZeroWitnessMeaning))
	}

	for field, key := range wiring {
		var snap properties.Snapshot
		v := reflect.ValueOf(&snap).Elem().FieldByName(field)
		if !v.IsValid() {
			t.Errorf("properties.Snapshot has no field %q", field)
			continue
		}
		v.SetInt(sentinel)

		e := NewEvaluator(nil)
		e.recordZeroWitnesses(snap)
		for k, got := range e.Tally().EngineZeroWitness {
			want := int64(0)
			if k == key {
				want = sentinel
			}
			if got != want {
				t.Errorf("with only %s set: witness %q = %d, want %d", field, k, got, want)
			}
		}
	}
}

// A failed poll returns a stamped ZERO snapshot, and these counters are
// cumulative. Taking the last reading rather than the peak would let one
// dead poll erase a witness that had already fired -- which is the one
// reading that must survive.
func TestAZeroPollCannotRetractAWitnessAlreadyFired(t *testing.T) {
	e := NewEvaluator(nil)
	base := time.Unix(1_700_000_000, 0)

	e.Observe(properties.Snapshot{TS: base.Unix(), EngineRecvDoubleArmed: 3}, base)
	e.Observe(properties.Snapshot{TS: base.Add(time.Second).Unix()}, base.Add(time.Second))

	if got := e.Tally().EngineZeroWitness["engine_recv_double_armed"]; got != 3 {
		t.Fatalf("witness after a dead poll = %d, want 3 -- a zero snapshot retracted a reading", got)
	}
}

// The same guard for the engine-side connection accounting, which forks
// celeris#624: a retracted close count would invert the comparison against
// the hook count and point at the wrong hypothesis.
func TestAZeroPollCannotRetractTheEngineAccounting(t *testing.T) {
	e := NewEvaluator(nil)
	base := time.Unix(1_700_000_000, 0)

	e.Observe(properties.Snapshot{
		TS: base.Unix(), EngineAcceptCount: 900, EngineCloseCount: 880,
		EngineTransplantDetached: 12, EngineTransplantAdopted: 12,
		EngineAsyncPromotedConns: 40, EngineStandbyActiveConns: 17,
	}, base)
	e.Observe(properties.Snapshot{TS: base.Add(time.Second).Unix()}, base.Add(time.Second))

	tl := e.Tally()
	for _, tc := range []struct {
		name string
		got  int64
		want int64
	}{
		{"EngineAcceptCount", tl.EngineAcceptCount, 900},
		{"EngineCloseCount", tl.EngineCloseCount, 880},
		{"EngineTransplantDetached", tl.EngineTransplantDetached, 12},
		{"EngineTransplantAdopted", tl.EngineTransplantAdopted, 12},
		{"EngineAsyncPromotedConns", tl.EngineAsyncPromotedConns, 40},
		{"PeakStandbyActiveConns", tl.PeakStandbyActiveConns, 17},
	} {
		if tc.got != tc.want {
			t.Errorf("%s after a dead poll = %d, want %d", tc.name, tc.got, tc.want)
		}
	}
}
