package checker

import (
	"encoding/json"
	"math"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/probatorium/report"
	"github.com/goceleris/probatorium/validation/internal/enginekeys"
	"github.com/goceleris/probatorium/validation/properties"
)

// probatorium#386 made the refapps publish all fifty-two engine.EngineMetrics
// fields and guarded that by reflection. probatorium#391 is what it found one
// layer on: ParseDebugVars, properties.Snapshot, the series and the tally
// were each their own hand-list, twenty published keys reached none of them,
// and celeris.engine_transplant_stranded was emitted by every refapp and
// recorded nowhere.
//
// The guards in this file hold two of those hops -- the parse and the
// Snapshot it fills, and the tally the evaluator reduces it to -- to the
// PUBLISHED key set rather than to a list of today's names, which would be
// the same hand-list one file over. That set comes from
// validation/internal/enginekeys, generated from the struct by the one module
// that compiles against celeris.

// debugVarsKeySet splits DebugVarsKeys into a set. Membership, not
// strings.Contains: celeris.engine_error_send is a substring of
// celeris.engine_error_send_peer_gone, so a substring test passes with the
// shorter key gone.
func debugVarsKeySet() map[string]bool {
	set := map[string]bool{}
	for _, k := range strings.Split(DebugVarsKeys, ",") {
		if k = strings.TrimSpace(k); k != "" {
			set[k] = true
		}
	}
	return set
}

// parsedSentinels is one ParseDebugVars run over a document carrying every
// published key at its own sentinel.
type parsedSentinels struct {
	keys []enginekeys.Key
	snap properties.Snapshot
	// holders maps each nonzero numeric Snapshot value to the fields holding it.
	holders map[float64][]string
}

func parseSentinels(t *testing.T) parsedSentinels {
	t.Helper()
	keys, err := enginekeys.All()
	if err != nil {
		// Not a skip: every guard below is only as complete as this list.
		t.Fatal(err)
	}
	body, err := json.Marshal(enginekeys.SentinelDocument(keys))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var snap properties.Snapshot
	if err := ParseDebugVars(body, &snap); err != nil {
		t.Fatalf("ParseDebugVars: %v", err)
	}
	return parsedSentinels{keys: keys, snap: snap, holders: numericFieldsByValue(snap)}
}

func numericFieldsByValue(snap properties.Snapshot) map[float64][]string {
	out := map[float64][]string{}
	v := reflect.ValueOf(snap)
	for i := range v.NumField() {
		var x float64
		switch fv := v.Field(i); fv.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			x = float64(fv.Int())
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			x = float64(fv.Uint())
		case reflect.Float32, reflect.Float64:
			x = fv.Float()
		default:
			continue
		}
		if x != 0 {
			out[x] = append(out[x], v.Type().Field(i).Name)
		}
	}
	return out
}

// TestParseDebugVarsReadsEveryPublishedEngineKey is the parse hop's guard.
//
// Every published key must land in EXACTLY ONE Snapshot field of its own, or
// be named in EngineKeysNotParsed with a reason. Matching by value catches
// the three ways a hand-written parse line fails silently: no line (the key
// parses as zero on the cluster, indistinguishable from a clean counter), a
// line reading the wrong key (two fields claim one value), and a float read
// through readInt64 (the truncated value lands instead of the exact one). It
// also holds DebugVarsKeys, the prose contract operators read, to the same
// set.
func TestParseDebugVarsReadsEveryPublishedEngineKey(t *testing.T) {
	p := parseSentinels(t)
	documented := debugVarsKeySet()

	published := map[string]bool{}
	for _, k := range p.keys {
		published[k.Key] = true
	}
	for key, why := range EngineKeysNotParsed {
		if !published[key] {
			t.Errorf("EngineKeysNotParsed excuses %q, which is no longer published: a stale excuse", key)
		}
		if why == "" {
			t.Errorf("EngineKeysNotParsed excuses %q without a reason", key)
		}
	}

	claimedBy := map[string]string{} // Snapshot field -> the key whose sentinel it holds
	var checked, optedOut int
	var missing, fannedOut, truncated, shared, undocumented, excusedButRead []string
	for i, k := range p.keys {
		exact := enginekeys.Sentinel(i, k)
		holders := p.holders[exact]
		if _, ok := EngineKeysNotParsed[k.Key]; ok {
			optedOut++
			if len(holders) > 0 || len(p.holders[math.Trunc(exact)]) > 0 {
				excusedButRead = append(excusedButRead, k.Key)
			}
			if documented[k.Key] {
				t.Errorf("DebugVarsKeys documents %s, which EngineKeysNotParsed says Poll does not read", k.Key)
			}
			continue
		}
		if !documented[k.Key] {
			undocumented = append(undocumented, k.Key)
		}
		switch {
		case len(holders) == 0 && k.IsFloat() && len(p.holders[math.Trunc(exact)]) > 0:
			truncated = append(truncated, k.Key+" -> "+strings.Join(p.holders[math.Trunc(exact)], ","))
			continue
		case len(holders) == 0:
			missing = append(missing, k.Key)
			continue
		case len(holders) > 1:
			fannedOut = append(fannedOut, k.Key+" -> "+strings.Join(holders, ","))
		}
		for _, f := range holders {
			if prev, dup := claimedBy[f]; dup {
				shared = append(shared, f+" holds both "+prev+" and "+k.Key)
			}
			claimedBy[f] = k.Key
		}
		checked++
	}

	if len(missing) > 0 {
		t.Errorf("%d published key(s) reach no properties.Snapshot field -- on the cluster they parse as zero and read like a clean counter: %v", len(missing), missing)
	}
	if len(fannedOut) > 0 {
		t.Errorf("key(s) parsed into more than one field: %v", fannedOut)
	}
	if len(shared) > 0 {
		t.Errorf("field(s) written from more than one key (a copy-pasted parse line): %v", shared)
	}
	if len(truncated) > 0 {
		t.Errorf("float key(s) truncated to an integer by readInt64: %v", truncated)
	}
	if len(excusedButRead) > 0 {
		t.Errorf("key(s) EngineKeysNotParsed excuses ARE parsed -- the excuse is stale: %v", excusedButRead)
	}
	if len(undocumented) > 0 {
		t.Errorf("DebugVarsKeys does not document %d parsed key(s): %v", len(undocumented), undocumented)
	}
	t.Logf("checked %d published engine key(s) of %d (%d opted out, %d missing, %d fanned out, %d truncated)",
		checked, len(p.keys), optedOut, len(missing), len(fannedOut), len(truncated))
	if checked == 0 {
		t.Fatal("no key was checked at all -- this guard is vacuous")
	}
}

// snapshotEngineFieldsNotFromEngineMetrics names numeric properties.Snapshot
// fields that look like engine metrics but are fed by something other than
// an EngineMetrics key. EMPTY: every one of them is a projection today, and
// an entry here must name a real field.
var snapshotEngineFieldsNotFromEngineMetrics = map[string]string{}

// TestEverySnapshotEngineFieldIsFedByAPublishedKey is the Snapshot hop's
// guard, the converse of the parse guard above: a Snapshot field that no
// published key writes is the same silent zero from the other side -- the
// struct claims to carry a counter the parser never fills. A name-only
// reflective walk would pass it; this one asks the parse.
func TestEverySnapshotEngineFieldIsFedByAPublishedKey(t *testing.T) {
	p := parseSentinels(t)
	st := reflect.TypeOf(properties.Snapshot{})
	// The rolling History is copied by value and the checker's tests compare
	// whole snapshots with !=, so a new field must keep Snapshot comparable.
	if !st.Comparable() {
		t.Fatal("properties.Snapshot is no longer comparable: only scalar fields may be added to it")
	}
	for name := range snapshotEngineFieldsNotFromEngineMetrics {
		if _, ok := st.FieldByName(name); !ok {
			t.Errorf("snapshotEngineFieldsNotFromEngineMetrics names %q, which is not a Snapshot field", name)
		}
	}
	sentinelKey := map[float64]string{}
	for i, k := range p.keys {
		sentinelKey[enginekeys.Sentinel(i, k)] = k.Key
	}
	fed := map[string]string{}
	for value, fields := range p.holders {
		for _, f := range fields {
			if key, ok := sentinelKey[value]; ok {
				fed[f] = key
			}
		}
	}

	var walked, checked, optedOut int
	var unfed []string
	for i := range st.NumField() {
		f := st.Field(i)
		switch f.Type.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
			reflect.Float32, reflect.Float64:
		default:
			continue
		}
		// The engine block's fields carry the Engine prefix, except the two
		// that were in the document before the prefix existed.
		if !strings.HasPrefix(f.Name, "Engine") && f.Name != "ActiveConns" && f.Name != "AdaptiveSwitches" {
			continue
		}
		walked++
		if _, ok := snapshotEngineFieldsNotFromEngineMetrics[f.Name]; ok {
			optedOut++
			continue
		}
		if _, ok := fed[f.Name]; !ok {
			unfed = append(unfed, f.Name)
			continue
		}
		checked++
	}
	sort.Strings(unfed)
	if len(unfed) > 0 {
		t.Errorf("%d Snapshot engine field(s) are written by no published key -- they will read zero on every cell: %v", len(unfed), unfed)
	}
	t.Logf("checked %d Snapshot engine field(s) of %d walked (%d opted out, %d unfed)", checked, walked, optedOut, len(unfed))
	if want := len(p.keys) - len(EngineKeysNotParsed); walked < want {
		t.Fatalf("walked %d Snapshot engine field(s) against %d parsed keys: the walk no longer matches the struct", walked, want)
	}
	if checked == 0 {
		t.Fatal("no field was checked at all -- this guard is vacuous")
	}
}

// engineKeysNotInTally names the published, parsed keys whose sentinel
// reaches no value of the end-of-cell Tally, each with the reason. Every other
// key must land in the Tally somewhere -- a named field, EngineZeroWitness,
// EngineErrorClasses or EngineCounters -- because properties_tally.json and
// validate-results.json are built from nothing else.
var engineKeysNotInTally = map[string]string{
	"celeris.active_conns": "a gauge judged on every sample by I-CONN-2 (accepted - closed - active) and carried per second in the series as `active`. The tally keeps what the adaptive controller acts on, peak_conns_per_worker, and a last reading would be one instant of a load that has not drained",
}

// TestEveryPublishedEngineKeyReachesTheTally is the Snapshot -> Tally hop's
// guard. It found engine_standby_close_count on its first run: parsed,
// carried in the series since 5.11, and in no end-of-cell total.
func TestEveryPublishedEngineKeyReachesTheTally(t *testing.T) {
	p := parseSentinels(t)
	e := NewEvaluator(nil)
	e.Observe(p.snap, time.Unix(1_700_000_000, 0))
	raw, err := json.Marshal(e.Tally())
	if err != nil {
		t.Fatalf("marshal tally: %v", err)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal tally: %v", err)
	}
	present := map[float64]bool{}
	collectNumbers(doc, present)

	published := map[string]bool{}
	for _, k := range p.keys {
		published[k.Key] = true
	}
	for key, why := range engineKeysNotInTally {
		if !published[key] {
			t.Errorf("engineKeysNotInTally excuses %q, which is no longer published", key)
		}
		if why == "" {
			t.Errorf("engineKeysNotInTally excuses %q without a reason", key)
		}
	}

	var checked, optedOut, notParsed int
	var dropped []string
	for i, k := range p.keys {
		if _, ok := EngineKeysNotParsed[k.Key]; ok {
			notParsed++
			continue
		}
		reached := present[enginekeys.Sentinel(i, k)]
		if _, ok := engineKeysNotInTally[k.Key]; ok {
			optedOut++
			if reached {
				t.Errorf("engineKeysNotInTally excuses %s, but its reading does reach the tally: the excuse is stale", k.Key)
			}
			continue
		}
		if !reached {
			dropped = append(dropped, k.Key)
			continue
		}
		checked++
	}
	if len(dropped) > 0 {
		t.Errorf("%d parsed engine key(s) reach no value of the end-of-cell Tally, so no cell artifact carries their total: %v", len(dropped), dropped)
	}
	t.Logf("checked %d engine key(s) into the tally of %d published (%d opted out, %d not parsed, %d dropped)",
		checked, len(p.keys), optedOut, notParsed, len(dropped))
	if checked == 0 {
		t.Fatal("no key was checked at all -- this guard is vacuous")
	}
}

func collectNumbers(v any, out map[float64]bool) {
	switch x := v.(type) {
	case float64:
		out[x] = true
	case map[string]any:
		for _, e := range x {
			collectNumbers(e, out)
		}
	case []any:
		for _, e := range x {
			collectNumbers(e, out)
		}
	}
}

// TestEachEngineCounterIsDeclaredAndReadsItsOwnKey holds recordEngineCounters
// -- one more hand-written literal -- to report.EngineCounters and to the
// published keys, by value and through the real parse. A declared counter the
// recorder forgets is absent from the artifact; a recorder line reading a
// neighbour's field puts the neighbour's number under this name, which a
// sentinel shared by every key could never show.
func TestEachEngineCounterIsDeclaredAndReadsItsOwnKey(t *testing.T) {
	p := parseSentinels(t)
	sentinelOf := map[string]int64{}
	keyOf := map[int64]string{}
	for i, k := range p.keys {
		s := int64(enginekeys.Sentinel(i, k))
		sentinelOf[k.Name()] = s
		keyOf[s] = k.Name()
	}

	validKind := map[report.EngineCounterKind]bool{
		report.CounterCumulative: true, report.CounterRunningMax: true,
		report.CounterGauge: true, report.CounterStatic: true,
	}
	for name, c := range report.EngineCounters {
		if _, ok := sentinelOf[name]; !ok {
			t.Errorf("report.EngineCounters declares %q, which no refapp publishes as celeris.%s", name, name)
		}
		if !validKind[c.Kind] {
			t.Errorf("%s: kind %q is not one of the declared kinds", name, c.Kind)
		}
		// The one combining rule that is never safe to get wrong: a running
		// maximum summed or differenced is a plausible number nothing flags.
		if strings.HasSuffix(name, "_max_nanos") && c.Kind != report.CounterRunningMax {
			t.Errorf("%s is an engine-side running maximum but is declared %q", name, c.Kind)
		}
		if c.Counts == "" || c.Why == "" {
			t.Errorf("%s: Counts and Why must both say something; the next counter has to make the same call", name)
		}
		if _, alsoWitness := report.ZeroWitnessMeaning[name]; alsoWitness {
			t.Errorf("%s is declared in both EngineCounters and ZeroWitnessMeaning", name)
		}
		if _, alsoClass := report.ErrorClasses[name]; alsoClass {
			t.Errorf("%s is declared in both EngineCounters and ErrorClasses", name)
		}
	}

	e := NewEvaluator(nil)
	e.Observe(p.snap, time.Unix(1_700_000_000, 0))
	recorded := e.Tally().EngineCounters

	var declared, got []string
	for k := range report.EngineCounters {
		declared = append(declared, k)
	}
	for k := range recorded {
		got = append(got, k)
	}
	sort.Strings(declared)
	sort.Strings(got)
	if !reflect.DeepEqual(declared, got) {
		t.Fatalf("report.EngineCounters and recordEngineCounters disagree:\n  declared: %v\n  recorded: %v", declared, got)
	}
	var checked int
	for _, name := range declared {
		want := sentinelOf[name]
		if v := recorded[name]; v != want {
			t.Errorf("engine_counters[%q] = %d, which is the reading of %q; want its own, %d", name, v, keyOf[v], want)
			continue
		}
		checked++
	}
	t.Logf("checked %d declared engine counter(s) by value through ParseDebugVars and Observe", checked)
	if checked == 0 {
		t.Fatal("no counter was checked at all -- this guard is vacuous")
	}
}

// The tally keeps the LAST reading, and "last" must mean the last reading of
// the engine, not the last document: a sample whose engine block was absent
// parses every counter as zero, and recording it would erase the cell's
// totals -- including a must-stay-zero engine_transplant_stranded that had
// already fired.
func TestAnEngineLessSampleCannotRetractTheEngineCounters(t *testing.T) {
	e := NewEvaluator(nil)
	base := time.Unix(1_700_000_000, 0)

	e.Observe(properties.Snapshot{}, base)
	if got := e.Tally().EngineCounters; got != nil {
		t.Fatalf("engine_counters = %v before any sample carried the engine block; absent must mean never measured", got)
	}

	e.Observe(properties.Snapshot{EngineName: "adaptive", EngineTransplantStranded: 2, EngineDetachedConns: 9}, base.Add(time.Second))
	e.Observe(properties.Snapshot{}, base.Add(2*time.Second))
	tl := e.Tally().EngineCounters
	if tl["engine_transplant_stranded"] != 2 {
		t.Errorf("engine_transplant_stranded after an engine-less sample = %d, want 2", tl["engine_transplant_stranded"])
	}
	if tl["engine_recv_stall_max_nanos"] != 0 {
		t.Errorf("a counter the engine reported as zero must be present at zero, got %d", tl["engine_recv_stall_max_nanos"])
	}

	// The gauge takes the last ENGINE reading, not the peak: a drift that
	// settles back is the reading, and a peak would keep the transient.
	e.Observe(properties.Snapshot{EngineName: "adaptive", EngineTransplantStranded: 2, EngineDetachedConns: 4}, base.Add(3*time.Second))
	if got := e.Tally().EngineCounters["engine_detached_conns"]; got != 4 {
		t.Errorf("engine_detached_conns = %d after readings 9 then 4, want the last, 4", got)
	}
}
