package checker

import (
	"slices"
	"testing"
	"time"

	"github.com/goceleris/probatorium/validation/properties"
)

// TestRFCPredicatesAreNotInstrumentedWithoutTheScraper is the guard against
// the failure mode this change nearly shipped.
//
// I-RFC-1 and I-RFC-2 read counters the Tier 1 wire scraper fills. The
// scraper only runs at or above streamingWalkerMinConcurrency, so in a smoke
// cell those counters are structurally zero. Simply deleting the two from
// Uninstrumented made them PASS in exactly those cells — a clean verdict from
// a probe that never ran, which is the vacuity the waiver list exists to
// stop. Two pre-existing evaluator tests caught it, and this one states the
// rule directly so a future refactor cannot quietly re-introduce it.
func TestRFCPredicatesAreNotInstrumentedWithoutTheScraper(t *testing.T) {
	e := NewEvaluator(SelectPredicates("core"))
	now := time.Unix(1_700_000_000, 0)
	// A snapshot that declares nothing: the scraper never ran.
	e.Observe(properties.Snapshot{TS: now.Unix(), GoroutineCount: 10, HeapInuseBytes: 1 << 20}, now)

	ni := e.Tally().NotInstrumented
	for _, id := range []string{"I-RFC-1", "I-RFC-2"} {
		if !slices.Contains(ni, id) {
			t.Errorf("%s reported as instrumented with no scraper declaration; "+
				"its counters are structurally zero, so a pass would verify nothing.\n"+
				"not_instrumented = %v", id, ni)
		}
	}
}

// TestRFCPredicatesBecomeInstrumentedWhenTheScraperDeclares is the other half:
// once the scraper has parsed at least one response it declares both ids on
// the snapshot, and they must then count as real coverage. Without this the
// previous test could be satisfied by leaving them permanently waived, which
// would close nothing.
func TestRFCPredicatesBecomeInstrumentedWhenTheScraperDeclares(t *testing.T) {
	e := NewEvaluator(SelectPredicates("core"))
	now := time.Unix(1_700_000_000, 0)
	e.Observe(properties.Snapshot{
		TS: now.Unix(), GoroutineCount: 10, HeapInuseBytes: 1 << 20,
		InstrumentedProperties: "I-RFC-1,I-RFC-2",
	}, now)

	ni := e.Tally().NotInstrumented
	for _, id := range []string{"I-RFC-1", "I-RFC-2"} {
		if slices.Contains(ni, id) {
			t.Errorf("%s still reported as not instrumented after the scraper declared it; "+
				"not_instrumented = %v", id, ni)
		}
	}
}

// TestRFCPredicatesFireOnScrapedViolations closes the loop: a declared cell
// whose scraper saw a malformed response must produce a violation, not just
// count as covered. Coverage that cannot fail is decoration.
func TestRFCPredicatesFireOnScrapedViolations(t *testing.T) {
	for _, tc := range []struct {
		name string
		snap properties.Snapshot
		want string
	}{
		{"framing", properties.Snapshot{ResponsesBadFraming: 1}, "I-RFC-1"},
		{"HEAD with body", properties.Snapshot{ResponsesHeadWithBody: 1}, "I-RFC-1"},
		{"204 with body", properties.Snapshot{Responses204WithBody: 1}, "I-RFC-1"},
		{"missing chunk terminator", properties.Snapshot{ResponsesMissingChunkEnd: 1}, "I-RFC-1"},
		{"NUL in header", properties.Snapshot{ResponsesNULInHeader: 1}, "I-RFC-2"},
		{"CRLF residue in header", properties.Snapshot{ResponsesCRLFInHeader: 1}, "I-RFC-2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := NewEvaluator(SelectPredicates("core"))
			now := time.Unix(1_700_000_000, 0)
			s := tc.snap
			s.TS = now.Unix()
			s.GoroutineCount = 10
			s.HeapInuseBytes = 1 << 20
			s.InstrumentedProperties = "I-RFC-1,I-RFC-2"

			var ids []string
			for _, v := range e.Observe(s, now) {
				ids = append(ids, v.ID)
			}
			if !slices.Contains(ids, tc.want) {
				t.Fatalf("scraped violation did not raise %s; violations = %v", tc.want, ids)
			}
		})
	}
}
