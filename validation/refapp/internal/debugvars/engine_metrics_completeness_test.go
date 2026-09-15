package debugvars

import (
	"flag"
	"os"
	"reflect"
	"sort"
	"strings"
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
		key := engineMetricsKey(f.Name)
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

// engineMetricsKey is the /debug/vars key an EngineMetrics field is
// published under: its alias when it has one, the default convention
// otherwise. One function, so the publish guard above and the manifest
// below cannot resolve the same field to two different keys.
func engineMetricsKey(field string) string {
	if key, ok := engineMetricsKeyAliases[field]; ok {
		return key
	}
	return "celeris.engine_" + snakeCase(field)
}

// engineKeysManifestPath is the root module's copy of the published key set.
// It lives in the ROOT module (validation/internal/enginekeys embeds it)
// because that is where every consumer is; this module is the only one that
// can compute it, because it is the only one that compiles against celeris.
const engineKeysManifestPath = "../../../internal/enginekeys/engine_metrics_keys.txt"

var updateEngineKeys = flag.Bool("update-engine-keys", false,
	"rewrite "+engineKeysManifestPath+" from engine.EngineMetrics instead of comparing against it")

const engineKeysManifestHeader = `# Every engine.EngineMetrics scalar field the refapps publish, and the
# /debug/vars key it is published under: <key> TAB <EngineMetrics field> TAB <Go type>.
#
# GENERATED -- do not edit by hand. Written and checked by
# TestEngineKeysManifestMatchesEngineMetrics in validation/refapp/internal/debugvars:
#
#   cd validation/refapp/internal/debugvars && go test -run TestEngineKeysManifestMatchesEngineMetrics -update-engine-keys
#
# which fails whenever this file and the celeris that module pins disagree. The
# root module cannot import celeris, so its guards learn what is published from
# here and hold ParseDebugVars, properties.Snapshot, the per-cell series and the
# end-of-cell tally to it (probatorium#391).
`

// renderEngineKeysManifest walks engine.EngineMetrics and renders the
// manifest: one line per published scalar field, sorted by key.
func renderEngineKeysManifest() (text string, lines int) {
	mt := reflect.TypeOf(engine.EngineMetrics{})
	var rows []string
	for i := range mt.NumField() {
		f := mt.Field(i)
		if !f.IsExported() || !isScalarKind(f.Type.Kind()) {
			continue
		}
		if _, skip := engineMetricsNotPublished[f.Name]; skip {
			continue
		}
		rows = append(rows, engineMetricsKey(f.Name)+"\t"+f.Name+"\t"+f.Type.String())
	}
	sort.Strings(rows)
	return engineKeysManifestHeader + strings.Join(rows, "\n") + "\n", len(rows)
}

// TestEngineKeysManifestMatchesEngineMetrics carries the guard above across
// the module boundary.
//
// TestDebugVarsPublishesEveryEngineMetricsField proves every EngineMetrics
// field reaches a /debug/vars key. That was one hand-list of four: the
// checker's ParseDebugVars, properties.Snapshot, the per-cell series and the
// end-of-cell tally are each their own, and after probatorium#386 twenty
// published keys -- celeris.engine_transplant_stranded among them -- still
// reached none of them (probatorium#391). Those four live in the root module,
// which cannot reflect over engine.EngineMetrics because it does not depend
// on celeris. So the key set is written to a file the root module embeds,
// and this test fails whenever the file and the struct disagree -- a field
// added in celeris fails HERE first, and regenerating the file then fails
// every root-module hop that does not carry it yet.
func TestEngineKeysManifestMatchesEngineMetrics(t *testing.T) {
	want, lines := renderEngineKeysManifest()
	if *updateEngineKeys {
		if err := os.WriteFile(engineKeysManifestPath, []byte(want), 0o644); err != nil {
			t.Fatalf("write %s: %v", engineKeysManifestPath, err)
		}
		t.Logf("wrote %d key(s) to %s", lines, engineKeysManifestPath)
		return
	}
	got, err := os.ReadFile(engineKeysManifestPath)
	if err != nil {
		// An unreadable manifest is not a pass: every root-module guard
		// that reads it would have nothing to hold its hop to.
		t.Fatalf("read %s: %v", engineKeysManifestPath, err)
	}
	if string(got) != want {
		have := map[string]bool{}
		for _, l := range strings.Split(string(got), "\n") {
			have[l] = true
		}
		need := map[string]bool{}
		for _, l := range strings.Split(want, "\n") {
			need[l] = true
		}
		var added, removed []string
		for l := range need {
			if !have[l] {
				added = append(added, l)
			}
		}
		for l := range have {
			if !need[l] {
				removed = append(removed, l)
			}
		}
		sort.Strings(added)
		sort.Strings(removed)
		t.Errorf("%s does not match engine.EngineMetrics at the pinned celeris.\n  missing from the file: %q\n  in the file but not the struct: %q\n"+
			"Regenerate it with -update-engine-keys, then carry every new key through the root module "+
			"(go test ./validation/... names each hop that drops one).",
			engineKeysManifestPath, added, removed)
	}
	t.Logf("manifest covers %d published EngineMetrics key(s)", lines)
	if lines < 52 {
		t.Fatalf("the walk rendered only %d key(s) against a floor of 52: the reflective walk is broken, not celeris", lines)
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
