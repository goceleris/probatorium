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
// celeris.engine_transplant_stranded is the key the issue was filed about: it
// was emitted by every refapp and recorded in no artifact.
func TestAPublishedEngineKeyReachesValidateResultsAndTheSeries(t *testing.T) {
	keys, err := enginekeys.All()
	if err != nil {
		t.Fatal(err)
	}
	doc := enginekeys.SentinelDocument(keys)
	doc["goroutines"] = 40
	doc["memstats"] = map[string]any{"HeapInuse": 4194304, "HeapAlloc": 3000000}
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	sentinel := map[string]int64{}
	for i, k := range keys {
		sentinel[k.Name()] = int64(enginekeys.Sentinel(i, k))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/debug/vars" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	cfg := Default()
	cfg.OutDir = t.TempDir()
	cfg.CelerisBin = "/usr/bin/true"
	seriesPath := filepath.Join(cfg.OutDir, "properties_series.csv")
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	tally := runPropertyLoop(ctx, propertyLoopConfig{
		MetricsURL: srv.URL + "/debug/vars",
		Interval:   20 * time.Millisecond,
		SeriesPath: seriesPath,
	})
	if tally.Samples == 0 || tally.PollErrors != 0 {
		t.Fatalf("the loop took %d sample(s) with %d poll error(s); nothing downstream can be judged", tally.Samples, tally.PollErrors)
	}

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
	const key = "engine_transplant_stranded"
	if got, want := counters[key], sentinel[key]; got != want {
		t.Fatalf("validate-results.json tier_1.engine_counters.%s = %d, want %d", key, got, want)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, `"`+key+`"`) {
			t.Logf("validate-results.json (schema %s): %s", out.SchemaVersion, strings.TrimSpace(line))
		}
	}
	var checkedDoc int
	for name := range report.EngineCounters {
		if got, want := counters[name], sentinel[name]; got != want {
			t.Errorf("validate-results.json tier_1.engine_counters.%s = %d, want %d", name, got, want)
			continue
		}
		checkedDoc++
	}

	rows := readCSV(t, seriesPath)
	if len(rows) < 2 {
		t.Fatalf("properties_series.csv has %d row(s), want a header and samples", len(rows))
	}
	index := map[string]int{}
	for i, c := range rows[0] {
		index[c] = i
	}
	var checkedSeries int
	for _, name := range report.SeriesEngineCounters() {
		col, ok := index[name]
		if !ok {
			t.Errorf("properties_series.csv has no %s column", name)
			continue
		}
		for r, row := range rows[1:] {
			if row[col] != strconv.FormatInt(sentinel[name], 10) {
				t.Errorf("properties_series.csv row %d %s = %q, want %d", r+1, name, row[col], sentinel[name])
			}
		}
		checkedSeries++
	}
	// Looked up with its presence checked: a missing column must not borrow
	// index 0 (ts) and print a plausible number under this name.
	col, ok := index[key]
	if !ok {
		t.Fatalf("properties_series.csv has no %s column, so the series half of this chain is not carried", key)
	}
	t.Logf("properties_series.csv: %d sample row(s), column %s (index %d) = %s in every row", len(rows)-1, key, col, rows[1][col])
	t.Logf("checked %d engine counter(s) in validate-results.json and %d series column(s) over %d sample(s)",
		checkedDoc, checkedSeries, tally.Samples)
	if checkedDoc == 0 || checkedSeries == 0 {
		t.Fatal("nothing was checked end to end -- this test is vacuous")
	}
}
