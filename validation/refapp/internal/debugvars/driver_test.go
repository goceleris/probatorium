package debugvars

import "testing"

func TestDriverCounters_InDocumentAndCounted(t *testing.T) {
	dv := New()
	keys := []string{
		"celeris.driver_writes_issued", "celeris.driver_reads_issued",
		"celeris.driver_read_hits", "celeris.driver_read_misses",
	}
	doc := dv.Document()
	for _, k := range keys {
		if v, ok := doc[k].(int64); !ok || v != 0 {
			t.Errorf("%s: fresh Vars must publish int64 0, got %#v", k, doc[k])
		}
	}
	dv.DriverWrite()
	dv.DriverWrite()
	dv.DriverRead(true)
	dv.DriverRead(false)
	doc = dv.Document()
	want := map[string]int64{keys[0]: 2, keys[1]: 2, keys[2]: 1, keys[3]: 1}
	for k, n := range want {
		if doc[k].(int64) != n {
			t.Errorf("%s = %d, want %d", k, doc[k], n)
		}
	}
}
