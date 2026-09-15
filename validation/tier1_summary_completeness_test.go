package validation

import (
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
