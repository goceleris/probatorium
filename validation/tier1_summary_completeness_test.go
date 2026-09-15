package validation

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/goceleris/probatorium/report"
	"github.com/goceleris/probatorium/validation/checker"
)

// TestTier1SummaryExportsEveryTallyField guards a silent-data-loss trap.
//
// Tier1Summary() projects each tally snapshot into a map[string]int64 by
// hand-listing keys. Nothing connects that list to the snapshot structs,
// so adding a field to a snapshot compiles, passes every unit test, ships
// -- and then silently never appears in validate-results.json.
//
// That is not hypothetical: the celeris#470 hang-cause split (h2c_hang_eof
// / _timeout / _reset / _other / _max_elapsed_ms) was added to h2cSnapshot,
// merged green, and deployed to a full nightly whose output contained none
// of it. A whole run was spent reading an instrument that was not wired up.
//
// This asserts the projection is total: every JSON-tagged field of every
// snapshot struct must show up as a key. Adding a field now fails here
// until it is exported too.
func TestTier1SummaryExportsEveryTallyField(t *testing.T) {
	var snap tier1TallySnapshot
	summary := snap.Tier1Summary()
	if summary == nil {
		t.Fatal("Tier1Summary() returned nil")
	}

	cases := []struct {
		name     string
		snapshot any
		exported map[string]int64
	}{
		{"h2c_churn", h2cSnapshot{}, summary.H2CChurn},
		{"ws_torture", wsSnapshot{}, summary.WSTorture},
		{"sse_kill", sseSnapshot{}, summary.SSEKill},
		{"ws_echo", wsEchoSnapshot{}, summary.WSEcho},
	}

	// Top-level int64 fields of the snapshot must exist on the summary
	// struct itself (matched by json tag); nested walker snapshots are
	// covered by the map cases below.
	t.Run("top_level", func(t *testing.T) {
		st := reflect.TypeOf(snap)
		sum := reflect.TypeOf(*summary)
		have := map[string]bool{}
		for i := 0; i < sum.NumField(); i++ {
			have[strings.Split(sum.Field(i).Tag.Get("json"), ",")[0]] = true
		}
		for i := 0; i < st.NumField(); i++ {
			f := st.Field(i)
			if f.Type.Kind() != reflect.Int64 {
				continue
			}
			key := strings.Split(f.Tag.Get("json"), ",")[0]
			if key == "" || key == "-" {
				continue
			}
			if !have[key] {
				t.Errorf("tier1TallySnapshot.%s (json %q) has no counterpart field on report.Tier1Summary -- it will be silently absent from validate-results.json", f.Name, key)
			}
		}
	})

	topLevel := map[string]bool{}
	for st, i := reflect.TypeOf(*summary), 0; i < st.NumField(); i++ {
		topLevel[strings.Split(st.Field(i).Tag.Get("json"), ",")[0]] = true
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := reflect.TypeOf(tc.snapshot)
			var missing []string
			for i := 0; i < rt.NumField(); i++ {
				tag := rt.Field(i).Tag.Get("json")
				if tag == "" || tag == "-" {
					continue
				}
				key := strings.Split(tag, ",")[0]
				if key == "" {
					continue
				}
				if _, ok := tc.exported[key]; ok {
					continue
				}
				// A non-numeric field cannot live in the int64 map; it is
				// exported as a typed top-level field carrying the same json
				// tag instead (sse_early_errs is []string). That still
				// satisfies the guarantee this test exists for -- the field
				// reaches validate-results.json -- so accept it here.
				if topLevel[key] {
					continue
				}
				missing = append(missing, key)
			}
			if len(missing) > 0 {
				t.Fatalf("%s: %d field(s) defined on %s but never exported into "+
					"Tier1Summary -- they will be silently absent from "+
					"validate-results.json: %v",
					tc.name, len(missing), rt.Name(), missing)
			}
		})
	}
}

// tallyTier1Renames maps a checker.Tally json name to the Tier1Summary json
// name it is projected under, where the two differ. Property-loop counters
// gain a property_ prefix on the summary so they cannot be confused with the
// walker's own counters beside them.
var tallyTier1Renames = map[string]string{
	"evaluations":    "property_evaluations",
	"skips":          "property_skips",
	"violations":     "property_violations",
	"violation_ids":  "property_violation_ids",
	"poll_errors":    "property_poll_errors",
	"skipped_reason": "property_loop_skipped",
}

// tallyNotInTier1Summary names the checker.Tally fields deliberately NOT
// projected into tier_1, each with where the information goes instead. Every
// entry must name a real field, and must not also be projected.
var tallyNotInTier1Summary = map[string]string{
	"Samples":                "the count of successful polls; property_evaluations and property_poll_errors carry what the gate reads, and properties_tally.json keeps the whole Tally",
	"Predicates":             "rolled up into the cell's properties_passed / properties_failed (Tally.Passed and Failed)",
	"NotInstrumented":        "a cell-level field, ValidationCellResult.PropertiesNotInstrumented, not tier_1",
	"NotJudged":              "a cell-level field, ValidationCellResult.PropertiesNotJudged, not tier_1",
	"NotJudgedByDesign":      "a cell-level field, ValidationCellResult.PropertiesNotJudgedByDesign, not tier_1",
	"FailureSummaries":       "a cell- and document-level field (FailureSummaries), not tier_1",
	"PerPredicate":           "per-predicate violation counts; property_violation_ids and property_violations carry the gated part, properties_tally.json the rest",
	"Observed":               "the observation window, used by the tally itself to decide NotJudgedByDesign; kept in properties_tally.json",
	"IdleWindows":            "orchestrator idle-window bookkeeping for I-MEM-2; kept in properties_tally.json",
	"IdleBaselineGoroutines": "I-MEM-2's reference level; kept in properties_tally.json",
	"BaselineGoroutines":     "a soak-summary input (validation.soak), not tier_1",
	"LastGoroutines":         "a soak-summary input (validation.soak), not tier_1",
	"FirstHeapInuse":         "a soak-summary input (validation.soak heap_growth_mb), not tier_1",
	"LastHeapInuse":          "a soak-summary input (validation.soak heap_growth_mb), not tier_1",
	"FirstRSS":               "a soak-summary input, not tier_1",
	"LastRSS":                "a soak-summary input, not tier_1",
}

// TestTier1SummaryCarriesEveryPropertyTallyField closes the blind spot the
// test above documents: it walks only int64 fields of tier1TallySnapshot, and
// Properties is a struct, so the whole checker.Tally engine block -- where
// celeris#627 dropped ten fields, and where probatorium#391 adds the
// engine_counters map -- was unguarded.
//
// Every Tally field is set to a distinct sentinel by reflection, projected,
// marshalled, and looked for under its json name on tier_1 (or its rename).
// By value, so a line in the projection that copies the wrong source field
// fails here too, not only a missing one.
func TestTier1SummaryCarriesEveryPropertyTallyField(t *testing.T) {
	var tl checker.Tally
	tv := reflect.ValueOf(&tl).Elem()
	tt := tv.Type()
	for name := range tallyNotInTier1Summary {
		if _, ok := tt.FieldByName(name); !ok {
			t.Errorf("tallyNotInTier1Summary names %q, which is not a checker.Tally field", name)
		}
	}
	for i := range tt.NumField() {
		f, fv := tt.Field(i), tv.Field(i)
		n := int64(7_000_019 + i*104_729)
		tag := "sentinel_" + f.Name
		switch fv.Kind() {
		case reflect.Int, reflect.Int64:
			fv.SetInt(n)
		case reflect.Float64:
			fv.SetFloat(float64(n) + 0.25)
		case reflect.String:
			fv.SetString(tag)
		case reflect.Slice:
			fv.Set(reflect.ValueOf([]string{tag}))
		case reflect.Map:
			switch f.Type.Elem().Kind() {
			case reflect.Int64:
				fv.Set(reflect.ValueOf(map[string]int64{tag: n}))
			case reflect.String:
				fv.Set(reflect.ValueOf(map[string]string{tag: tag}))
			default:
				t.Fatalf("Tally.%s is a %s; teach this guard its shape rather than skip it", f.Name, f.Type)
			}
		default:
			t.Fatalf("Tally.%s is a %s; teach this guard its shape rather than skip it", f.Name, f.Type)
		}
	}

	raw, err := json.Marshal(tier1TallySnapshot{Properties: tl}.Tier1Summary())
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}

	var checked, optedOut int
	var missing, wrong []string
	for i := range tt.NumField() {
		f, fv := tt.Field(i), tv.Field(i)
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		key := name
		if r, ok := tallyTier1Renames[name]; ok {
			key = r
		}
		got, present := doc[key]
		if _, ok := tallyNotInTier1Summary[f.Name]; ok {
			optedOut++
			if present {
				t.Errorf("tallyNotInTier1Summary excuses Tally.%s, but tier_1 carries %q: the excuse is stale", f.Name, key)
			}
			continue
		}
		if !present {
			missing = append(missing, f.Name+" -> "+key)
			continue
		}
		want, err := json.Marshal(fv.Interface())
		if err != nil {
			t.Fatal(err)
		}
		gotRaw, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		if string(gotRaw) != string(want) {
			wrong = append(wrong, f.Name+": tier_1."+key+" = "+string(gotRaw)+", want "+string(want))
			continue
		}
		checked++
	}
	if len(missing) > 0 {
		t.Errorf("%d checker.Tally field(s) never reach tier_1 in validate-results.json and are not declared as going elsewhere: %v", len(missing), missing)
	}
	for _, w := range wrong {
		t.Error("projected from the wrong source: " + w)
	}
	walked := tt.NumField()
	t.Logf("checked %d Tally field(s) of %d walked (%d opted out, %d missing, %d wrong)", checked, walked, optedOut, len(missing), len(wrong))
	if want := 37; walked < want {
		t.Fatalf("walked %d Tally field(s) against a floor of %d: the reflective walk is broken", walked, want)
	}
	if checked == 0 {
		t.Fatal("no field was checked at all -- this guard is vacuous")
	}
}

// The projection at runner.go's report.Tier1Summary literal is field by
// field, and the test above cannot see the property tally because it is a
// nested struct rather than a top-level int64. That blind spot is where
// celeris#627 dropped ten fields, so the celeris#645 error split -- three
// more entries in the same literal -- is asserted by value instead.
//
// Every value here is distinct, so a line that projects the wrong source
// field lands the wrong number rather than the right number twice.
func TestTier1SummaryCarriesTheEngineErrorSplit(t *testing.T) {
	classes := map[string]int64{
		"engine_error_accept_fd_limit":   2,
		"engine_error_accept_cancelled":  3,
		"engine_error_accept_other":      5,
		"engine_error_conn_table_cap":    7,
		"engine_error_conn_register":     11,
		"engine_error_listener_recreate": 13,
		"engine_error_transplant_adopt":  17,
		"engine_error_send_peer_gone":    19,
		"engine_error_send":              23,
		"engine_error_request_body":      29,
		"engine_error_handler":           31,
	}
	if len(classes) != len(report.ErrorClasses) {
		t.Fatalf("this test covers %d buckets but %d are declared -- a new bucket needs a case here",
			len(classes), len(report.ErrorClasses))
	}
	sum := tier1TallySnapshot{Properties: checker.Tally{
		EngineErrorCount:        160, // the sum of the eleven above
		EngineErrorClasses:      classes,
		EngineStandbyErrorCount: 41,
	}}.Tier1Summary()

	if sum.EngineErrorCount != 160 {
		t.Errorf("engine_error_count = %d, want 160", sum.EngineErrorCount)
	}
	if sum.EngineStandbyErrorCount != 41 {
		t.Errorf("engine_standby_error_count = %d, want 41", sum.EngineStandbyErrorCount)
	}
	if !reflect.DeepEqual(sum.EngineErrorClasses, classes) {
		t.Errorf("engine_error_classes = %v, want %v", sum.EngineErrorClasses, classes)
	}
	var total int64
	for _, v := range sum.EngineErrorClasses {
		total += v
	}
	if total != sum.EngineErrorCount {
		t.Errorf("projected buckets sum to %d against engine_error_count %d", total, sum.EngineErrorCount)
	}
}
