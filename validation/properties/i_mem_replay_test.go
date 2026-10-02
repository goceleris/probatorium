package properties_test

import (
	"bufio"
	"compress/gzip"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/probatorium/validation/checker"
	"github.com/goceleris/probatorium/validation/properties"
)

// Replays of real soak series through the real Evaluator and I-MEM-1
// (probatorium#466). Each fixture in testdata/imem1 is a cell's own 1 Hz
// heap_inuse with the idle window stamped on every sample. The idle window
// is inferred from engine_requests_total, exactly as the soak analysis'
// replay did; that replay reproduced the gate's tally in 64 of 64 cells.

func loadSeries(t *testing.T, name string) []properties.Snapshot {
	t.Helper()
	f, err := os.Open("testdata/imem1/" + name + ".csv.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(gz)
	var out []properties.Snapshot
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "ts,") {
			continue
		}
		v := strings.Split(sc.Text(), ",")
		if len(v) != 3 {
			t.Fatalf("%s: bad line %q", name, sc.Text())
		}
		ts, e1 := strconv.ParseInt(v[0], 10, 64)
		heap, e2 := strconv.ParseInt(v[1], 10, 64)
		idle, e3 := strconv.Atoi(v[2])
		if e1 != nil || e2 != nil || e3 != nil {
			t.Fatalf("%s: bad line %q", name, sc.Text())
		}
		out = append(out, properties.Snapshot{TS: ts, HeapInuseBytes: heap, IdleWindow: idle})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(out) < 2400 {
		t.Fatalf("%s: %d samples, want a whole soak cell", name, len(out))
	}
	return out
}

// imem1Violations runs series through a fresh Evaluator holding only spec
// and returns its I-MEM-1 count, after the evaluator's own persistence and
// warm-up rules.
func imem1Violations(spec properties.Spec, series []properties.Snapshot, leakBps float64) int64 {
	ev := checker.NewEvaluator([]properties.Spec{spec})
	load := series[0].TS
	for i := 1; i < len(series); i++ {
		if series[i-1].IdleWindow == 1 && series[i].IdleWindow == 0 {
			load = series[i].TS
			break
		}
	}
	for _, s := range series {
		if dt := s.TS - (load + 300); leakBps > 0 && dt > 0 {
			s.HeapInuseBytes += int64(leakBps * float64(dt))
		}
		ev.Observe(s, time.Unix(s.TS, 0))
	}
	return ev.Tally().PerPredicate["I-MEM-1"]
}

// The two cells soak 36433207097 failed on I-MEM-1, arm64 auth_session
// epoll (353) and io_uring (1), are trough scatter, not leaks: with the
// bound they replay to 0. Without it they replay to exactly the gate's
// recorded counts, which is what shows each fixture is the series the
// gate judged.
func TestIMEM1Replay_soak36433207097ScatterCellsAreClean(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		recorded int64
	}{
		{"soak36433207097-arm64-auth_session_ratelimit-epoll", 353},
		{"soak36433207097-arm64-auth_session_ratelimit-iouring", 1},
	} {
		s := loadSeries(t, c.name)
		if got := imem1Violations(properties.IMEM1WithoutBoundForTest(), s, 0); got != c.recorded {
			t.Fatalf("%s: without the bound the replay gives %d, the gate recorded %d: the fixture is not the series the gate judged", c.name, got, c.recorded)
		}
		if got := imem1Violations(properties.IMEM1, s, 0); got != 0 {
			t.Errorf("%s: I-MEM-1 still fails %d times on trough scatter alone (probatorium#466)", c.name, got)
		}
	}
}

// The positive control. The 2026-09-09 true positive (soak 34368601903,
// auth_session_ratelimit/io_uring, celeris#573) cannot be replayed: its
// artifact holds no properties_series.csv. The control is the same refapp,
// engine and host from the 2026-09-11 soak (1 h cells, no ws_echo, after
// the #574 fix), which is clean by itself, plus a leak from the end of
// warm-up at the true positive's own recorded trough slopes: 3.2 KB/s on
// msa2, 2.2 KB/s on msr1. I-MEM-1 must still catch it on both hosts.
// These carriers' troughs scatter by only 72-84 KB, so this control shows
// that the bound keeps the old catch on a quiet cell, not what it costs on
// a noisy one: see the next test. The leak must be called for at least
// half of the hour-long cell (1800 evaluations; 2326-2476 at this head),
// not merely called: a bound 10x too strict still calls it, but only
// 826-1576 times.
func TestIMEM1Replay_positiveControlStillFails(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"soak34616620237-amd64-auth_session_ratelimit-iouring",
		"soak34616620237-arm64-auth_session_ratelimit-iouring",
	} {
		s := loadSeries(t, name)
		if got := imem1Violations(properties.IMEM1, s, 0); got != 0 {
			t.Fatalf("%s: the carrier alone fails I-MEM-1 %d times; it must be clean for the leak to be what is caught", name, got)
		}
		for _, leak := range []float64{3277, 2253} {
			got := imem1Violations(properties.IMEM1, s, leak)
			t.Logf("%s + %.0f B/s: %d I-MEM-1 violations", name, leak, got)
			if got < 1800 {
				t.Errorf("%s: a %.0f B/s leak (the celeris#573 true positive's slope) fails I-MEM-1 only %d times, want at least 1800 (half the cell)", name, leak, got)
			}
		}
	}
}

// The near-the-bar control, on the soak shape the gate judges today. The
// 09-11 carriers above scatter about 20x less than these cells, so they
// cannot see a bound that is too strict. This is celeris#573's own cell in
// soak 36433207097 (arm64 auth_session_ratelimit/io_uring, 45 min, ws_echo
// walker on), clean by itself under the bound, plus a 4 KB/s leak from the
// end of warm-up: I-MEM-1 must still catch it. 4 KB/s is close to what
// this cell can show. 3 KB/s is caught here by 226 evaluations, 2.5 KB/s
// not at all, so celeris#573's 2.2 KB/s would go uncalled in this cell
// (probatorium#478). It must be called for at least one persistence period
// (IMEM1.Persist, 150 evaluations; 376 at this head), so a bound 1.5x too
// strict, which leaves 76, fails here too.
func TestIMEM1Replay_soak36433207097NearTheBarStillFails(t *testing.T) {
	t.Parallel()
	const name = "soak36433207097-arm64-auth_session_ratelimit-iouring"
	s := loadSeries(t, name)
	if got := imem1Violations(properties.IMEM1, s, 0); got != 0 {
		t.Fatalf("%s: the carrier alone fails I-MEM-1 %d times; it must be clean for the leak to be what is caught", name, got)
	}
	got := imem1Violations(properties.IMEM1, s, 4096)
	t.Logf("%s + 4096 B/s: %d I-MEM-1 violations", name, got)
	if got < int64(properties.IMEM1.Persist) {
		t.Errorf("%s: a 4 KB/s leak fails I-MEM-1 only %d times in the soak's highest-scatter cell, want at least one persistence period (%d): the bound is stricter than its 99.9 %% quantile", name, got, properties.IMEM1.Persist)
	}
}
