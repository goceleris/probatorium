package validation

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/probatorium/report"
	"github.com/goceleris/probatorium/validation/checker"
	"github.com/goceleris/probatorium/validation/internal/enginekeys"
	"github.com/goceleris/probatorium/validation/properties"
)

// The series and the end-of-cell projection are the last two of the four
// hand-lists probatorium#391 found between a published engine key and a
// cluster artifact. The guards here hold both to the published key set
// (validation/internal/enginekeys) through the real parse, so the chain from
// engine.EngineMetrics to properties_series.csv and validate-results.json is
// checked hop by hop, and one test drives it end to end.

// TestTheSeriesCarriesExactlyTheEngineCountersDeclaredForIt: the column-or-
// total call for each counter is written down in report.EngineCounters, and
// seriesColumns must agree with it in both directions -- a counter declared
// for the series and forgotten here publishes a decision the artifact does not
// carry, and a column nothing declares is width nobody justified.
func TestTheSeriesCarriesExactlyTheEngineCountersDeclaredForIt(t *testing.T) {
	inSeries := make(map[string]bool, len(seriesColumns))
	for _, c := range seriesColumns {
		inSeries[c] = true
	}
	var carried, tallyOnly int
	for name, c := range report.EngineCounters {
		switch {
		case c.Series && !inSeries[name]:
			t.Errorf("report.EngineCounters marks %q as a series counter and seriesColumns does not carry it", name)
		case !c.Series && inSeries[name]:
			t.Errorf("seriesColumns carries %q but report.EngineCounters declares it tally-only", name)
		case c.Series:
			carried++
		default:
			tallyOnly++
		}
	}
	if want := len(report.SeriesEngineCounters()); carried != want {
		t.Errorf("series carries %d declared counters, want %d", carried, want)
	}
	t.Logf("checked %d declared engine counter(s): %d carried in the series, %d tally-only", carried+tallyOnly, carried, tallyOnly)
	if carried+tallyOnly == 0 {
		t.Fatal("report.EngineCounters is empty -- this guard is vacuous")
	}
}

// seriesColumnForKey names the columns whose header is not the key without
// its "celeris." prefix. Historical, like the debugvars aliases: `active`
// predates every engine_ column.
var seriesColumnForKey = map[string]string{
	"celeris.active_conns": "active",
}

// seriesDeclaration reports whether a declaration exists for the counter
// name, and what it says about the series.
func seriesDeclaration(name string) (series, declared bool) {
	if c, ok := report.EngineCounters[name]; ok {
		return c.Series, true
	}
	if c, ok := report.ErrorClasses[name]; ok {
		return c.Series, true
	}
	return false, false
}

// TestEveryPublishedEngineKeyHasAColumnOrADeclaration is the series hop's
// guard. Each published key is parsed at its own sentinel and recorded, and
// then must either land in exactly one column -- the one named after it -- or
// be declared tally-only in report.EngineCounters or report.ErrorClasses. A
// key with neither is a silent drop; a key in the wrong column is a positional
// mis-wire between seriesColumns and Record, which are two lists joined only
// by index.
func TestEveryPublishedEngineKeyHasAColumnOrADeclaration(t *testing.T) {
	keys, err := enginekeys.All()
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(enginekeys.SentinelDocument(keys))
	if err != nil {
		t.Fatal(err)
	}
	var snap properties.Snapshot
	if err := checker.ParseDebugVars(body, &snap); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "series.csv")
	w := newSeriesWriter(path)
	if w == nil {
		t.Fatal("newSeriesWriter returned nil for a writable path")
	}
	w.Record(snap)
	w.Close()
	rows := readCSV(t, path)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want header + 1 sample", len(rows))
	}
	columnsHolding := map[float64][]string{}
	for i, col := range rows[0] {
		v, err := strconv.ParseFloat(rows[1][i], 64)
		if err != nil {
			t.Fatalf("column %q: %v", col, err)
		}
		columnsHolding[v] = append(columnsHolding[v], col)
	}

	var columns, tallyOnly, notParsed int
	var problems []string
	for i, k := range keys {
		if _, ok := checker.EngineKeysNotParsed[k.Key]; ok {
			notParsed++
			continue
		}
		cols := columnsHolding[enginekeys.Sentinel(i, k)]
		want := k.Name()
		if c, ok := seriesColumnForKey[k.Key]; ok {
			want = c
		}
		series, declared := seriesDeclaration(k.Name())
		switch {
		case len(cols) > 1:
			problems = append(problems, k.Key+" lands in more than one column: "+strings.Join(cols, ","))
		case len(cols) == 1 && cols[0] != want:
			problems = append(problems, k.Key+" lands in column "+cols[0]+", want "+want+" (a positional mis-wire)")
		case len(cols) == 1 && declared && !series:
			problems = append(problems, k.Key+" is declared tally-only but the series carries it")
		case len(cols) == 1:
			columns++
		case declared && series:
			problems = append(problems, k.Key+" is declared a series counter but no column carries it")
		case declared:
			tallyOnly++
		default:
			problems = append(problems, k.Key+" reaches no series column and nothing declares it tally-only")
		}
	}
	for _, p := range problems {
		t.Error(p)
	}
	t.Logf("checked %d published engine key(s): %d in a column, %d declared tally-only, %d not parsed, %d problem(s)",
		columns+tallyOnly, columns, tallyOnly, notParsed, len(problems))
	if columns == 0 {
		t.Fatal("no key reached a column at all -- this guard is vacuous")
	}
}

func readCSV(t *testing.T, path string) [][]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return rows
}

// TestAPublishedEngineKeyReachesValidateResultsAndTheSeries drives the whole
// chain the way a cell does: a refapp-shaped /debug/vars over HTTP, the real
// property loop polling it and writing properties_series.csv, the tally it
// returns projected through Tier1Summary and written by writeValidateResults.
// Every hop is the production code; only the refapp is faked, with every
// published key at its own sentinel.
//
// The readings RISE between polls, the way a live engine's counters do, and
// the cell must yield at least two samples. With one sample, or the same
// document on every poll, a tally that summed the samples or kept the first
// of them holds the same number as one that kept the last, and passes.
//
// celeris.engine_transplant_stranded is the key the issue was filed about: it
// was emitted by every refapp and recorded in no artifact.
func TestAPublishedEngineKeyReachesValidateResultsAndTheSeries(t *testing.T) {
	keys, err := enginekeys.All()
	if err != nil {
		t.Fatal(err)
	}
	sentinel := map[string]int64{}
	for i, k := range keys {
		sentinel[k.Name()] = int64(enginekeys.Sentinel(i, k))
	}
	// documentAt is the refapp's document at its n-th poll (from 0): every
	// published key at its own sentinel plus n. n stays far below the
	// spacing between sentinels, so a reading still names its key, and the
	// row that carries it says which poll it recorded.
	documentAt := func(n int64) ([]byte, error) {
		doc := enginekeys.SentinelDocument(keys)
		for i, k := range keys {
			doc[k.Key] = enginekeys.Sentinel(i, k) + float64(n)
		}
		doc["goroutines"] = 40
		doc["memstats"] = map[string]any{"HeapInuse": 4194304, "HeapAlloc": 3000000}
		return json.Marshal(doc)
	}

	// The refapp serves servePolls documents and cancels the loop on the
	// poll after the last one, so the cell ends on a count of polls rather
	// than on a timer a slow host could expire after a single sample. The
	// deadline is only a backstop.
	const servePolls = 6
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var served atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/debug/vars" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		n := served.Add(1) - 1
		if n >= servePolls {
			// Cancelled before this handler returns, so the loop sees its
			// context done and does not count the empty reply as a poll error.
			cancel()
			return
		}
		body, err := documentAt(n)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	cfg := Default()
	cfg.OutDir = t.TempDir()
	cfg.CelerisBin = "/usr/bin/true"
	seriesPath := filepath.Join(cfg.OutDir, "properties_series.csv")
	tally := runPropertyLoop(ctx, propertyLoopConfig{
		MetricsURL: srv.URL + "/debug/vars",
		Interval:   20 * time.Millisecond,
		SeriesPath: seriesPath,
	})
	if tally.PollErrors != 0 {
		t.Fatalf("the loop took %d sample(s) with %d poll error(s); nothing downstream can be judged", tally.Samples, tally.PollErrors)
	}
	if tally.Samples < 2 {
		t.Fatalf("the loop took %d sample(s) of %d served: a tally that sums its samples or keeps the first one is only distinguishable over at least two samples whose readings differ", tally.Samples, servePolls)
	}

	// Which poll each series row recorded, read off the row itself: a poll
	// the loop was cancelled out of leaves no row, so the rows, not the
	// server's count, say what the tally saw.
	rows := readCSV(t, seriesPath)
	if got := int64(len(rows) - 1); got != tally.Samples {
		t.Fatalf("properties_series.csv has %d sample row(s) and the tally %d sample(s): they are no longer the same samples, so neither can be checked against the other", got, tally.Samples)
	}
	index := map[string]int{}
	for i, c := range rows[0] {
		index[c] = i
	}
	const key = "engine_transplant_stranded"
	// Looked up with its presence checked: a missing column must not borrow
	// index 0 (ts) and print a plausible number under this name.
	col, ok := index[key]
	if !ok {
		t.Fatalf("properties_series.csv has no %s column, so the series half of this chain is not carried", key)
	}
	polls := make([]int64, 0, len(rows)-1)
	for r, row := range rows[1:] {
		v, err := strconv.ParseInt(row[col], 10, 64)
		if err != nil {
			t.Fatalf("properties_series.csv row %d %s = %q: %v", r+1, key, row[col], err)
		}
		n := v - sentinel[key]
		if n < 0 || n >= servePolls {
			t.Fatalf("properties_series.csv row %d %s = %d, which no served document carried (%d + 0..%d)", r+1, key, v, sentinel[key], servePolls-1)
		}
		if r > 0 && n <= polls[r-1] {
			t.Fatalf("properties_series.csv row %d recorded poll %d after poll %d: the rows are not the documents in the order they were served", r+1, n, polls[r-1])
		}
		polls = append(polls, n)
	}
	first, last := polls[0], polls[len(polls)-1]

	o, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	o.tier1Ran = true
	o.tier1Snapshot = tier1TallySnapshot{RequestsSent: 1, Properties: tally}
	if err := o.writeValidateResults(time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("writeValidateResults: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(cfg.OutDir, "validate-results.json"))
	if err != nil {
		t.Fatalf("read validate-results.json: %v", err)
	}
	var out report.Document
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("parse validate-results.json: %v", err)
	}
	if out.Validation == nil || out.Validation.Tier1 == nil {
		t.Fatalf("validate-results.json has no tier_1:\n%s", raw)
	}
	counters := out.Validation.Tier1.EngineCounters
	// reduced names what a tally value is, against the readings the recorded
	// samples carried for that counter.
	reduced := func(name string, got int64) string {
		var sum int64
		for _, n := range polls {
			sum += sentinel[name] + n
		}
		switch got {
		case sentinel[name] + last:
			return "the last sample's reading"
		case sum:
			return "the SUM of the samples"
		case sentinel[name] + first:
			return "the FIRST sample's reading"
		default:
			return "not the last sample's reading"
		}
	}
	// Every counter's want is the last recorded sample's reading. The
	// readings rise, so for a running maximum and a peak gauge that is also
	// its max: this
	// test separates last from sum and first, and
	// checker.TestEachEngineCounterIsReducedByItsDeclaredKind separates max
	// from last.
	if got, want := counters[key], sentinel[key]+last; got != want {
		t.Fatalf("validate-results.json tier_1.engine_counters.%s = %d, %s; want %d, the reading of the last of %d samples (polls %v)",
			key, got, reduced(key, got), want, len(polls), polls)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, `"`+key+`"`) {
			t.Logf("validate-results.json (schema %s): %s", out.SchemaVersion, strings.TrimSpace(line))
		}
	}
	var checkedDoc int
	for name, c := range report.EngineCounters {
		if got, want := counters[name], sentinel[name]+last; got != want {
			t.Errorf("validate-results.json tier_1.engine_counters.%s (%s) = %d, %s; want %d, the reading of the last of %d samples (polls %v)",
				name, c.Kind, got, reduced(name, got), want, len(polls), polls)
			continue
		}
		checkedDoc++
	}

	var checkedSeries int
	for _, name := range report.SeriesEngineCounters() {
		col, ok := index[name]
		if !ok {
			t.Errorf("properties_series.csv has no %s column", name)
			continue
		}
		for r, row := range rows[1:] {
			// The same document as the stranded column read above: a column
			// holding another poll's reading, or another key's, fails here.
			if want := strconv.FormatInt(sentinel[name]+polls[r], 10); row[col] != want {
				t.Errorf("properties_series.csv row %d %s = %q, want %s (poll %d)", r+1, name, row[col], want, polls[r])
			}
		}
		checkedSeries++
	}
	t.Logf("properties_series.csv: %d sample row(s) recording polls %v of %d served; column %s (index %d) = %s in the first row and %s in the last",
		len(rows)-1, polls, served.Load(), key, col, rows[1][col], rows[len(rows)-1][col])
	t.Logf("checked %d engine counter(s) in validate-results.json and %d series column(s) over %d sample(s)",
		checkedDoc, checkedSeries, tally.Samples)
	if checkedDoc == 0 || checkedSeries == 0 {
		t.Fatal("nothing was checked end to end -- this test is vacuous")
	}
}
