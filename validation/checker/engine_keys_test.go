package checker

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"slices"
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
		report.CounterPeakGauge: true,
	}
	for name, c := range report.EngineCounters {
		if _, ok := sentinelOf[name]; !ok {
			t.Errorf("report.EngineCounters declares %q, which no refapp publishes as celeris.%s", name, name)
		}
		if !validKind[c.Kind] {
			t.Errorf("%s: kind %q is not one of the declared kinds", name, c.Kind)
		}
		// Whether the kind is the RIGHT one for the field is
		// TestEachEngineCounterKindFollowsItsEngineMetricsField's question.
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

// reductionReadings are the readings TestEachEngineCounterIsReducedByItsDeclaredKind
// publishes for every counter, one per sample, each counter offset by its own
// thousand. Chosen so that sum (18), max (9), first (5) and last (4) are four
// different numbers: with fewer samples, or with readings that only rise, two
// of those rules coincide and a reducer applying the wrong one of the two
// passes.
var reductionReadings = []int64{5, 9, 4}

// kindReductions is the rule each report.EngineCounterKind requires of the
// end-of-cell tally, written here independently of reduceEngineCounter so the
// guard is not the code restated. A kind with no entry fails the guard.
var kindReductions = map[report.EngineCounterKind]struct {
	rule   string
	reduce func([]int64) int64
}{
	// A running total since the engine started: its last reading IS the cell
	// total, and a sum multiplies it by the sample count.
	report.CounterCumulative: {"last", lastReading},
	// The longest single episode: combine only with max
	// (report.CounterRunningMax). Last agrees only while nothing resets it.
	report.CounterRunningMax: {"max", maxReading},
	// A level that can fall: the last reading is where the cell ended, and a
	// peak would keep a transient that settled back.
	report.CounterGauge: {"last", lastReading},
	// Fixed after Listen, so any reading is the value; the tally keeps the
	// last, the one the engine ended on.
	report.CounterStatic: {"last", lastReading},
	// A level whose question is whether it ever stood above zero
	// (report.CounterPeakGauge): the highest reading, so a zero clears every
	// sample and not only the last one.
	report.CounterPeakGauge: {"max", maxReading},
}

func lastReading(r []int64) int64 { return r[len(r)-1] }

func maxReading(r []int64) int64 { return slices.Max(r) }

func sumReadings(r []int64) int64 {
	var s int64
	for _, v := range r {
		s += v
	}
	return s
}

// TestEachEngineCounterIsReducedByItsDeclaredKind guards HOW the tally
// combines a cell's samples, which no single-sample guard can see: with one
// reading, sum, max, first and last are the same number, so a guard that
// observes once passes a reducer that sums, or one that keeps a running
// maximum's last reading instead of its highest.
//
// It walks report.EngineCounters, publishes every counter through the real
// parse at reductionReadings, and requires the tally to hold what the
// counter's declared Kind requires (kindReductions). A counter whose readings
// cannot tell the four rules apart is a failure, not a pass.
func TestEachEngineCounterIsReducedByItsDeclaredKind(t *testing.T) {
	names := make([]string, 0, len(report.EngineCounters))
	for name := range report.EngineCounters {
		names = append(names, name)
	}
	sort.Strings(names)
	readings := make(map[string][]int64, len(names))
	for j, name := range names {
		base := int64(j+1) * 1_000
		for _, r := range reductionReadings {
			readings[name] = append(readings[name], base+r)
		}
	}

	e := NewEvaluator(nil)
	start := time.Unix(1_700_000_000, 0)
	for s := range reductionReadings {
		doc := map[string]any{"celeris.engine": "io_uring"}
		for _, name := range names {
			doc["celeris."+name] = readings[name][s]
		}
		body, err := json.Marshal(doc)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var snap properties.Snapshot
		if err := ParseDebugVars(body, &snap); err != nil {
			t.Fatalf("ParseDebugVars: %v", err)
		}
		e.Observe(snap, start.Add(time.Duration(s)*time.Second))
	}
	tally := e.Tally().EngineCounters

	perKind := map[report.EngineCounterKind]int{}
	var checked, blind int
	for _, name := range names {
		kind := report.EngineCounters[name].Kind
		want, ok := kindReductions[kind]
		if !ok {
			t.Errorf("%s is declared %q, a kind kindReductions has no rule for: decide how the tally must reduce it, then add it", name, kind)
			continue
		}
		r := readings[name]
		candidates := []struct {
			rule string
			v    int64
		}{{"sum", sumReadings(r)}, {"max", maxReading(r)}, {"first", r[0]}, {"last", lastReading(r)}}
		coincide := ""
		for i := range candidates {
			for j := i + 1; j < len(candidates) && coincide == ""; j++ {
				if candidates[i].v == candidates[j].v {
					coincide = fmt.Sprintf("%s and %s are both %d", candidates[i].rule, candidates[j].rule, candidates[i].v)
				}
			}
		}
		if coincide != "" {
			blind++
			t.Errorf("%s (%s): readings %v cannot tell the reductions apart (%s), so a wrong rule would pass -- not checked", name, kind, r, coincide)
			continue
		}
		wantV := want.reduce(r)
		got, present := tally[name]
		if !present {
			t.Errorf("%s (%s) is absent from the tally after %d engine samples", name, kind, len(r))
			continue
		}
		if got != wantV {
			what := "none of sum, max, first or last"
			for _, c := range candidates {
				if c.v == got {
					what = "the " + strings.ToUpper(c.rule)
				}
			}
			t.Errorf("%s (%s) = %d, %s of readings %v; want the %s, %d", name, kind, got, what, r, want.rule, wantV)
			continue
		}
		perKind[kind]++
		checked++
	}
	t.Logf("checked the reduction of %d of %d declared engine counter(s) over %d sample(s): %d cumulative by last, %d running_max by max, %d gauge by last, %d static by last, %d peak_gauge by max; %d could not discriminate",
		checked, len(names), len(reductionReadings), perKind[report.CounterCumulative], perKind[report.CounterRunningMax],
		perKind[report.CounterGauge], perKind[report.CounterStatic], perKind[report.CounterPeakGauge], blind)
	if checked == 0 {
		t.Fatal("no counter's reduction was checked at all -- this guard is vacuous")
	}
}

// engineFieldKinds classifies every published EngineMetrics field whose kind
// its Go type does not settle, keyed by the field name the manifest lists.
// A uint64 field not named here is a cumulative counter.
var engineFieldKinds = map[string]struct {
	kind report.EngineCounterKind
	why  string
}{
	"ActiveConnections":         {report.CounterGauge, "a signed int64 celeris documents as the current number of open connections"},
	"StandbyActiveConnections":  {report.CounterGauge, "a signed int64, the standby sub-engine's share of ActiveConnections"},
	"DetachedConnections":       {report.CounterGauge, "a signed int64 celeris documents as the current number of connections handed to a detached middleware goroutine (celeris#584)"},
	"Workers":                   {report.CounterGauge, "an int celeris documents as static after Listen, but the adaptive engine reports pm.Workers + sm.Workers and its lazy standby adds nothing until it is built, so on that engine it steps up mid-cell"},
	"AsyncRoutes":               {report.CounterStatic, "an int derived from the router's per-route async flags and fixed after Listen"},
	"Throughput":                {report.CounterGauge, "a float64 recent requests-per-second rate, which falls as well as rises"},
	"RecvStallMaxNanos":         {report.CounterRunningMax, "a uint64 that is the longest single recv-stall episode, not a total"},
	"RecvLinkedBlockedMaxNanos": {report.CounterRunningMax, "a uint64 that is the longest single linked-recv wait, not a total"},
	// celeris#687's residual gauges are uint64 -- each loop publishes a delta
	// against what it published last, so the engine-wide value falls as well
	// as rises without ever going negative -- and celeris documents them as
	// GAUGES read at every switch verdict, hence peak_gauge.
	"TransplantResidualDetached":  {report.CounterPeakGauge, "a uint64 celeris#687 documents as a GAUGE: the WebSocket/SSE connections a draining engine still holds"},
	"TransplantResidualH2":        {report.CounterPeakGauge, "a uint64 celeris#687 documents as a GAUGE: the H2/h2c connections a draining engine still holds"},
	"TransplantResidualPinned":    {report.CounterPeakGauge, "a uint64 celeris#687 documents as a GAUGE: the connections a draining engine holds that cannot be handed over at all"},
	"TransplantResidualUnstarted": {report.CounterPeakGauge, "a uint64 celeris#687 documents as a GAUGE: the accepted connections a draining engine holds that have sent nothing yet"},
	"TransplantResidualBusy":      {report.CounterPeakGauge, "a uint64 celeris#687 documents as a GAUGE: the mid-request connections a draining engine holds, whose standing nonzero value after a switch settles is the placement bug"},
}

// TestEachEngineCounterKindFollowsItsEngineMetricsField ties each declared
// Kind to the field it describes. The kind decides the reduction, so a wrong
// kind is a wrong number in the artifact -- and
// TestEachEngineCounterIsReducedByItsDeclaredKind cannot see it, because it
// holds the reducer to whatever kind is declared.
//
// The authority is the field's Go type, which the manifest carries from a
// reflective walk of the pinned celeris: a uint64 only rises, so it is
// cumulative unless engineFieldKinds names it a running maximum; a field of
// any other type has to be classified in engineFieldKinds; and a signed int64
// exists to be decremented, so it can only be a gauge. The table holds the
// decisions a type cannot make (Workers against AsyncRoutes, both int), each
// with its reason.
func TestEachEngineCounterKindFollowsItsEngineMetricsField(t *testing.T) {
	keys, err := enginekeys.All()
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]enginekeys.Key{}
	byField := map[string]enginekeys.Key{}
	for _, k := range keys {
		byName[k.Name()] = k
		byField[k.Field] = k
	}
	for field, c := range engineFieldKinds {
		k, ok := byField[field]
		switch {
		case !ok:
			t.Errorf("engineFieldKinds classifies %q, which the manifest does not list: a stale entry", field)
		case c.why == "":
			t.Errorf("engineFieldKinds classifies %s without a reason", field)
		case k.Type == "int64" && c.kind != report.CounterGauge:
			t.Errorf("engineFieldKinds classifies %s as %q, but it is a signed int64, which the engine decrements: only a gauge", field, c.kind)
		}
	}
	requiredKind := func(k enginekeys.Key) (report.EngineCounterKind, string, bool) {
		if c, ok := engineFieldKinds[k.Field]; ok {
			return c.kind, c.why, true
		}
		if k.Type == "uint64" {
			return report.CounterCumulative, "a uint64, which only rises, and engineFieldKinds does not name it a running maximum", true
		}
		return "", "", false
	}
	for _, k := range keys {
		if _, _, ok := requiredKind(k); !ok {
			t.Errorf("%s (%s %s) is not a uint64 and engineFieldKinds does not classify it: its kind is a decision, not a default", k.Key, k.Field, k.Type)
		}
		if strings.HasSuffix(k.Key, "_max_nanos") {
			if c, ok := engineFieldKinds[k.Field]; !ok || c.kind != report.CounterRunningMax {
				t.Errorf("%s is an engine-side running maximum and engineFieldKinds does not classify %s as one", k.Key, k.Field)
			}
		}
		// A sixth refusal class celeris adds is a uint64 too, and would
		// default to cumulative -- kept as its last reading -- unless named.
		if strings.HasPrefix(k.Field, "TransplantResidual") {
			if c, ok := engineFieldKinds[k.Field]; !ok || c.kind != report.CounterPeakGauge {
				t.Errorf("%s is one of celeris#687's residual gauges and engineFieldKinds does not classify %s as a peak gauge", k.Key, k.Field)
			}
		}
	}

	var checked, byTable, byType, unpublished int
	for name, c := range report.EngineCounters {
		k, ok := byName[name]
		if !ok {
			unpublished++ // TestEachEngineCounterIsDeclaredAndReadsItsOwnKey names it
			continue
		}
		want, why, ok := requiredKind(k)
		if !ok {
			continue // reported above
		}
		if c.Kind != want {
			t.Errorf("report.EngineCounters declares %s %q, but %s is %s: want %q", name, c.Kind, k.Field, why, want)
			continue
		}
		if _, ok := engineFieldKinds[k.Field]; ok {
			byTable++
		} else {
			byType++
		}
		checked++
	}
	t.Logf("checked the kind of %d of %d declared engine counter(s) against the manifest: %d by engineFieldKinds, %d as uint64 cumulative (%d unpublished)",
		checked, len(report.EngineCounters), byTable, byType, unpublished)
	if checked == 0 {
		t.Fatal("no counter's kind was checked at all -- this guard is vacuous")
	}
}

// The tally reduces the ENGINE's readings, not every document's: a sample
// whose engine block was absent parses every counter as zero, and recording
// it would erase the cell's totals -- including a must-stay-zero
// engine_transplant_stranded that had already fired.
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
