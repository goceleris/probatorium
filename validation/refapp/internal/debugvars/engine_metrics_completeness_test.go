package debugvars

import (
	"reflect"
	"sort"
	"testing"

	"github.com/goceleris/celeris/engine"
)

// engineMetricsKeyAliases names the EngineMetrics fields whose document
// key is NOT the default "celeris.engine_" + snakeCase(field).
//
// These are historical, and every one of them predates the guard below.
// ActiveConnections and AdaptiveSwitches were in the document before any
// engine_ prefix existed and the checker parses them by those names;
// RequestCount is published as a _total because that is what it is; and
// the two Connections gauges are abbreviated to _conns to match
// celeris.active_conns and celeris.open_conns_tracked, which the rest of
// the document already uses.
//
// This map is deliberately small and deliberately explicit. Every entry
// is asserted to name a real field, so a celeris rename cannot leave a
// stale alias behind that quietly excuses a key nobody publishes.
var engineMetricsKeyAliases = map[string]string{
	"ActiveConnections":        "celeris.active_conns",
	"AdaptiveSwitches":         "celeris.adaptive_switches",
	"RequestCount":             "celeris.engine_requests_total",
	"StandbyActiveConnections": "celeris.engine_standby_active_conns",
	"DetachedConnections":      "celeris.engine_detached_conns",
}

// engineMetricsNotPublished is the opt-out list: EngineMetrics fields
// this document deliberately does not carry, each with the reason.
//
// It is EMPTY, and that is the point. The projection is total today, so
// adding a field to celeris fails the guard below until somebody makes a
// decision about it -- publish it, or write down here why not. A silent
// skip inside the walk would be the same hand-list this test exists to
// abolish, one level further in.
var engineMetricsNotPublished = map[string]string{}

// TestDebugVarsPublishesEveryEngineMetricsField guards a
// silent-data-loss trap, in the shape of validation's
// TestTier1SummaryExportsEveryTallyField one layer downstream.
//
// Vars.Document() projects engine.EngineMetrics into /debug/vars by
// hand-listing fields. Nothing connects that list to the struct, so a
// counter added in celeris compiles here, passes every test here, ships
// -- and then never reaches a cluster artifact, where a missing key
// parses as zero and reads exactly like a clean counter.
//
// That is not hypothetical. celeris#647 added TransplantStranded and
// TransplantAdoptRefused specifically so its own fix for celeris#624
// would be falsifiable; race tier run 34961642523 was then dispatched to
// test that prediction, and two of its four clauses could not be
// evaluated at all, because neither counter was in the document. Twenty
// of celeris's fifty-two EngineMetrics fields were in that state
// (probatorium#386).
//
// So: walk the struct by reflection and require a published key for
// every scalar field. Adding a field in celeris now fails HERE, in the
// refapp that has to publish it, instead of in a nightly that quietly
// reports one counter short.
func TestDebugVarsPublishesEveryEngineMetricsField(t *testing.T) {
	base, _ := startRefapp(t)
	doc := getVars(t, base)

	// The engine keys all live behind one `if info != nil`. Without this
	// check a server whose EngineInfo came back nil would report all
	// fifty-two as missing and read like a projection bug.
	if _, ok := doc["celeris.engine"]; !ok {
		t.Fatal("celeris.engine absent: the whole EngineMetrics projection was skipped, so this test cannot judge it")
	}

	mt := reflect.TypeOf(engine.EngineMetrics{})
	// Neither list may name a field that no longer exists. A stale entry
	// in either one is an excuse that outlived its field, and the alias
	// case is the dangerous one: it would send the walk looking for a key
	// under the old name and find it, while the renamed field goes
	// unpublished.
	for name := range engineMetricsKeyAliases {
		if _, ok := mt.FieldByName(name); !ok {
			t.Errorf("engineMetricsKeyAliases names %q, which is not a field of engine.EngineMetrics any more", name)
		}
	}
	for name := range engineMetricsNotPublished {
		if _, ok := mt.FieldByName(name); !ok {
			t.Errorf("engineMetricsNotPublished names %q, which is not a field of engine.EngineMetrics any more", name)
		}
	}

	var checked, optedOut, nonScalar int
	var missing, wrongType []string
	for i := range mt.NumField() {
		f := mt.Field(i)
		if !f.IsExported() {
			continue
		}
		if !isScalarKind(f.Type.Kind()) {
			// Every field is scalar today. A composite one would need a
			// projection decision of its own rather than a key, so name
			// it in the output instead of passing over it in silence.
			t.Logf("non-scalar field not covered by this guard: %s %s", f.Name, f.Type)
			nonScalar++
			continue
		}
		if why, ok := engineMetricsNotPublished[f.Name]; ok {
			t.Logf("deliberately not published: %s (%s)", f.Name, why)
			optedOut++
			continue
		}
		key, ok := engineMetricsKeyAliases[f.Name]
		if !ok {
			key = "celeris.engine_" + snakeCase(f.Name)
		}
		v, present := doc[key]
		if !present {
			missing = append(missing, f.Name+" -> "+key)
			continue
		}
		// encoding/json decodes every number into a float64, so anything
		// else here is a key published as a string, a bool or null --
		// present but unreadable by checker.readInt64, which is the same
		// absence with extra steps.
		if _, isNum := v.(float64); !isNum {
			wrongType = append(wrongType, f.Name+" -> "+key)
			continue
		}
		checked++
	}

	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%d of %d engine.EngineMetrics field(s) reach no /debug/vars key -- on the cluster they are ABSENT, which parses as zero and cannot be told from a clean counter: %v",
			len(missing), mt.NumField(), missing)
	}
	sort.Strings(wrongType)
	if len(wrongType) > 0 {
		t.Errorf("%d key(s) are published as something other than a JSON number, so checker.readInt64 reads them as zero: %v", len(wrongType), wrongType)
	}

	// Non-vacuity. A guard that walks nothing passes everything, and a
	// reflective walk is exactly the kind that can quietly stop matching.
	//
	// The floor is on fields REACHED, not on keys found, so it stays a
	// statement about the walk: a field that is missing a key is already
	// reported above, and counting it here too would turn every missing
	// key into a second, wrong diagnosis. Fifty-two is what celeris
	// carries today; growth above it is fine.
	walked := checked + optedOut + nonScalar + len(missing) + len(wrongType)
	t.Logf("checked %d published key(s) over %d EngineMetrics field(s) walked (%d opted out, %d non-scalar, %d missing, %d wrong type)",
		checked, walked, optedOut, nonScalar, len(missing), len(wrongType))
	if want := 52; walked < want {
		t.Fatalf("the walk reached only %d field(s) against a floor of %d: the reflective match is broken, not celeris", walked, want)
	}
	if checked == 0 {
		t.Fatal("no field was checked at all -- this guard is vacuous")
	}
}

// isScalarKind reports whether a struct field is a single number, which
// is the only shape that maps onto one flat /debug/vars key.
func isScalarKind(k reflect.Kind) bool {
	switch k {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	default:
		return false
	}
}
