package checker

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/probatorium/report"
	"github.com/goceleris/probatorium/validation/properties"
)

// celeris#646 split EngineMetrics.ErrorCount into eleven cause buckets
// because celeris#645 could not be answered by a total: the adaptive engine
// recorded 421 engine errors in a 112-second cell against io_uring's 63 and
// epoll's 0, and one number can bound the cause but never name it.
//
// The set is written down in two places that must agree: report.ErrorClasses,
// which says what each bucket counts and whether the series samples it, and
// recordErrorClasses, which reads it off the snapshot. Adding to one and not
// the other is silent in both directions, exactly as it is for the
// must-stay-zero witnesses next door.

func TestEveryErrorClassIsBothDeclaredAndRecorded(t *testing.T) {
	// Every bucket is set, so a key the recorder writes but nobody declares
	// actually lands in the map. With an all-zero snapshot the recorder
	// writes nothing beyond the seed and the comparison is vacuous.
	e := NewEvaluator(nil)
	e.recordErrorClasses(properties.Snapshot{
		EngineErrorAcceptFDLimit:    1,
		EngineErrorAcceptCancelled:  1,
		EngineErrorAcceptOther:      1,
		EngineErrorConnTableCap:     1,
		EngineErrorConnRegister:     1,
		EngineErrorListenerRecreate: 1,
		EngineErrorTransplantAdopt:  1,
		EngineErrorSendPeerGone:     1,
		EngineErrorSend:             1,
		EngineErrorRequestBody:      1,
		EngineErrorHandler:          1,
	})
	recorded := e.Tally().EngineErrorClasses

	declared := make([]string, 0, len(report.ErrorClasses))
	for k := range report.ErrorClasses {
		declared = append(declared, k)
	}
	got := make([]string, 0, len(recorded))
	for k := range recorded {
		got = append(got, k)
	}
	sort.Strings(declared)
	sort.Strings(got)
	if !reflect.DeepEqual(declared, got) {
		t.Fatalf("report.ErrorClasses and recordErrorClasses disagree:\n  declared: %v\n  recorded: %v", declared, got)
	}
	for _, k := range declared {
		if recorded[k] != 1 {
			t.Errorf("bucket %q = %d after a snapshot that set every field to 1", k, recorded[k])
		}
	}
}

// Each bucket must read its OWN snapshot field. Two keys reading the same
// field is the copy-paste failure a block of eleven near-identical lines
// invites, and it is invisible from the outside: the duplicated counter
// would simply never move, which reads exactly like an engine that cannot
// reach that cause -- and "this bucket is structurally zero on this engine"
// is a real and expected reading here, which is what makes the mistake so
// easy to miss.
func TestEachErrorClassReadsItsOwnField(t *testing.T) {
	const sentinel = 7

	// field name on properties.Snapshot -> bucket key it must feed.
	wiring := map[string]string{
		"EngineErrorAcceptFDLimit":    "engine_error_accept_fd_limit",
		"EngineErrorAcceptCancelled":  "engine_error_accept_cancelled",
		"EngineErrorAcceptOther":      "engine_error_accept_other",
		"EngineErrorConnTableCap":     "engine_error_conn_table_cap",
		"EngineErrorConnRegister":     "engine_error_conn_register",
		"EngineErrorListenerRecreate": "engine_error_listener_recreate",
		"EngineErrorTransplantAdopt":  "engine_error_transplant_adopt",
		"EngineErrorSendPeerGone":     "engine_error_send_peer_gone",
		"EngineErrorSend":             "engine_error_send",
		"EngineErrorRequestBody":      "engine_error_request_body",
		"EngineErrorHandler":          "engine_error_handler",
	}
	if len(wiring) != len(report.ErrorClasses) {
		t.Fatalf("this test covers %d buckets but %d are declared -- a new bucket needs a case here",
			len(wiring), len(report.ErrorClasses))
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
		e.recordErrorClasses(snap)
		for k, got := range e.Tally().EngineErrorClasses {
			want := int64(0)
			if k == key {
				want = sentinel
			}
			if got != want {
				t.Errorf("with only %s set: bucket %q = %d, want %d", field, k, got, want)
			}
		}
	}
}

// The parse end of the same wire. Every key gets a DISTINCT value, so a
// parser that reads the wrong key -- or reads one key into two fields --
// lands the wrong number somewhere rather than the right number everywhere.
func TestParseDebugVarsReadsEveryErrorClassKey(t *testing.T) {
	doc := map[string]any{
		"goroutines":                             61,
		"celeris.engine_error_count":             1_031,
		"celeris.engine_error_accept_fd_limit":   2,
		"celeris.engine_error_accept_cancelled":  3,
		"celeris.engine_error_accept_other":      5,
		"celeris.engine_error_conn_table_cap":    7,
		"celeris.engine_error_conn_register":     11,
		"celeris.engine_error_listener_recreate": 13,
		"celeris.engine_error_transplant_adopt":  17,
		"celeris.engine_error_send_peer_gone":    19,
		"celeris.engine_error_send":              23,
		"celeris.engine_error_request_body":      29,
		"celeris.engine_error_handler":           31,
		"celeris.engine_standby_error_count":     37,
	}
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var snap properties.Snapshot
	if err := ParseDebugVars(body, &snap); err != nil {
		t.Fatalf("ParseDebugVars: %v", err)
	}
	for _, tc := range []struct {
		name string
		got  int64
		want int64
	}{
		{"EngineErrorCount", snap.EngineErrorCount, 1_031},
		{"EngineErrorAcceptFDLimit", snap.EngineErrorAcceptFDLimit, 2},
		{"EngineErrorAcceptCancelled", snap.EngineErrorAcceptCancelled, 3},
		{"EngineErrorAcceptOther", snap.EngineErrorAcceptOther, 5},
		{"EngineErrorConnTableCap", snap.EngineErrorConnTableCap, 7},
		{"EngineErrorConnRegister", snap.EngineErrorConnRegister, 11},
		{"EngineErrorListenerRecreate", snap.EngineErrorListenerRecreate, 13},
		{"EngineErrorTransplantAdopt", snap.EngineErrorTransplantAdopt, 17},
		{"EngineErrorSendPeerGone", snap.EngineErrorSendPeerGone, 19},
		{"EngineErrorSend", snap.EngineErrorSend, 23},
		{"EngineErrorRequestBody", snap.EngineErrorRequestBody, 29},
		{"EngineErrorHandler", snap.EngineErrorHandler, 31},
		{"EngineStandbyErrorCount", snap.EngineStandbyErrorCount, 37},
	} {
		if tc.got != tc.want {
			t.Errorf("snap.%s = %d, want %d", tc.name, tc.got, tc.want)
		}
	}
}

// The negative control for the parse: a refapp pinned to a celeris without
// the split publishes none of these keys, and the fields must stay zero
// rather than pick up a neighbour's reading.
func TestParseDebugVarsLeavesTheErrorClassesZeroWhenAbsent(t *testing.T) {
	var snap properties.Snapshot
	if err := ParseDebugVars([]byte(`{"goroutines":61,"celeris.engine_error_count":421}`), &snap); err != nil {
		t.Fatalf("ParseDebugVars: %v", err)
	}
	if snap.EngineErrorCount != 421 {
		t.Fatalf("EngineErrorCount = %d, want 421", snap.EngineErrorCount)
	}
	for _, tc := range []struct {
		name string
		got  int64
	}{
		{"EngineErrorAcceptFDLimit", snap.EngineErrorAcceptFDLimit},
		{"EngineErrorAcceptCancelled", snap.EngineErrorAcceptCancelled},
		{"EngineErrorAcceptOther", snap.EngineErrorAcceptOther},
		{"EngineErrorConnTableCap", snap.EngineErrorConnTableCap},
		{"EngineErrorConnRegister", snap.EngineErrorConnRegister},
		{"EngineErrorListenerRecreate", snap.EngineErrorListenerRecreate},
		{"EngineErrorTransplantAdopt", snap.EngineErrorTransplantAdopt},
		{"EngineErrorSendPeerGone", snap.EngineErrorSendPeerGone},
		{"EngineErrorSend", snap.EngineErrorSend},
		{"EngineErrorRequestBody", snap.EngineErrorRequestBody},
		{"EngineErrorHandler", snap.EngineErrorHandler},
		{"EngineStandbyErrorCount", snap.EngineStandbyErrorCount},
	} {
		if tc.got != 0 {
			t.Errorf("absent key produced a reading: %s = %d", tc.name, tc.got)
		}
	}
}

// DebugVarsKeys is the document's contract in prose, and the property loop's
// operators read it rather than the parser. A key parsed but undocumented is
// how a counter comes to exist that nobody knows to look at.
func TestDebugVarsKeysDocumentsEveryErrorClassKey(t *testing.T) {
	for name := range report.ErrorClasses {
		if !strings.Contains(DebugVarsKeys, "celeris."+name) {
			t.Errorf("DebugVarsKeys does not mention celeris.%s", name)
		}
	}
	for _, k := range []string{"celeris.engine_error_count", "celeris.engine_standby_error_count"} {
		if !strings.Contains(DebugVarsKeys, k) {
			t.Errorf("DebugVarsKeys does not mention %s", k)
		}
	}
}

// celeris derives ErrorCount from the buckets in engine.FillErrorClasses and
// keeps no separate running total, and the adaptive engine sums both
// sub-engines bucket by bucket -- so the total and the parts cannot drift
// inside one /debug/vars document. Every counter is cumulative, so the
// tally's per-bucket max is each bucket's last reading and the identity
// survives the reduction across samples too.
//
// Asserted here and NOT in the gate. celeris documents its bucket snapshot
// as individually atomic but not mutually consistent, and the adaptive
// engine reads two sub-engines in sequence, so a live cell can legitimately
// publish a document whose parts are a few errors behind its total. The
// identity is worth recording in the artifact; failing a cell on it would be
// failing on a race in the measurement.
func TestTheTallyErrorClassesSumToTheErrorCount(t *testing.T) {
	e := NewEvaluator(nil)
	base := time.Unix(1_700_000_000, 0)
	// Two samples of a monotone, self-consistent engine: every bucket grows
	// and ErrorCount is their sum at each step.
	for i, s := range []properties.Snapshot{
		{
			EngineErrorAcceptFDLimit: 1, EngineErrorAcceptCancelled: 2,
			EngineErrorAcceptOther: 3, EngineErrorConnTableCap: 4,
			EngineErrorConnRegister: 5, EngineErrorListenerRecreate: 6,
			EngineErrorTransplantAdopt: 7, EngineErrorSendPeerGone: 8,
			EngineErrorSend: 9, EngineErrorRequestBody: 10,
			EngineErrorHandler: 11, EngineErrorCount: 66,
			EngineStandbyErrorCount: 4,
		},
		{
			EngineErrorAcceptFDLimit: 2, EngineErrorAcceptCancelled: 4,
			EngineErrorAcceptOther: 6, EngineErrorConnTableCap: 8,
			EngineErrorConnRegister: 10, EngineErrorListenerRecreate: 12,
			EngineErrorTransplantAdopt: 14, EngineErrorSendPeerGone: 16,
			EngineErrorSend: 18, EngineErrorRequestBody: 20,
			EngineErrorHandler: 22, EngineErrorCount: 132,
			EngineStandbyErrorCount: 9,
		},
	} {
		at := base.Add(time.Duration(i) * time.Second)
		s.TS = at.Unix()
		e.Observe(s, at)
	}
	got := e.Tally()
	var sum int64
	for _, v := range got.EngineErrorClasses {
		sum += v
	}
	if sum != got.EngineErrorCount {
		t.Errorf("buckets sum to %d but EngineErrorCount = %d; the split does not add up",
			sum, got.EngineErrorCount)
	}
	if got.EngineErrorCount != 132 {
		t.Errorf("EngineErrorCount = %d, want 132", got.EngineErrorCount)
	}
	if got.EngineStandbyErrorCount != 9 {
		t.Errorf("EngineStandbyErrorCount = %d, want 9", got.EngineStandbyErrorCount)
	}
	// The standby share is NOT one of the buckets: it cuts the same total
	// along the other axis, so a reader summing the map must not find it.
	if _, ok := got.EngineErrorClasses["engine_standby_error_count"]; ok {
		t.Error("engine_standby_error_count is in EngineErrorClasses; summing the map now double counts")
	}
}

// A failed poll returns a stamped ZERO snapshot, and these counters are
// cumulative. Taking the last reading rather than the peak would let one
// dead poll erase the cause a cell was about to be diagnosed from.
func TestAZeroPollCannotRetractAnErrorClassAlreadyCounted(t *testing.T) {
	e := NewEvaluator(nil)
	base := time.Unix(1_700_000_000, 0)

	e.Observe(properties.Snapshot{
		TS: base.Unix(), EngineErrorSendPeerGone: 88_010, EngineErrorCount: 88_010,
		EngineStandbyErrorCount: 421,
	}, base)
	e.Observe(properties.Snapshot{TS: base.Add(time.Second).Unix()}, base.Add(time.Second))

	tl := e.Tally()
	if got := tl.EngineErrorClasses["engine_error_send_peer_gone"]; got != 88_010 {
		t.Errorf("bucket after a dead poll = %d, want 88010 -- a zero snapshot retracted a reading", got)
	}
	if tl.EngineErrorCount != 88_010 {
		t.Errorf("EngineErrorCount after a dead poll = %d, want 88010", tl.EngineErrorCount)
	}
	if tl.EngineStandbyErrorCount != 421 {
		t.Errorf("EngineStandbyErrorCount after a dead poll = %d, want 421", tl.EngineStandbyErrorCount)
	}
}

// Nothing in the split is gated, and that has to be checked rather than
// remembered. The must-stay-zero witnesses next door each count an event
// that cannot happen in a correct engine, so the gate fails a cell on one;
// every bucket here counts something a correct engine does under load, and
// celeris#646 measured 88,010 ErrorSendPeerGone over 88,776 accepts on a
// healthy io_uring abandon-churn load. A bucket that quietly became a
// witness would fail the next nightly on normal traffic.
func TestNoErrorClassIsAZeroWitness(t *testing.T) {
	for name := range report.ErrorClasses {
		if _, gated := report.ZeroWitnessMeaning[name]; gated {
			t.Errorf("%q is declared both an error class and a must-stay-zero witness; the gate would fail a cell on load-proportional traffic", name)
		}
	}
	if _, gated := report.ZeroWitnessMeaning["engine_standby_error_count"]; gated {
		t.Error("engine_standby_error_count is a must-stay-zero witness; it is a share of a total that legitimately moves")
	}
}
