package validation

import (
	"reflect"
	"strings"
	"testing"
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
				if _, ok := tc.exported[key]; !ok {
					missing = append(missing, key)
				}
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
