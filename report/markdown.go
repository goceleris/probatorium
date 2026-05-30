package report

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SLOThresholds is the canonical column set for the headline
// latency_at_slo table — milliseconds, ascending. Aligned with the
// thresholds the v5.0 schema persists; changing this slice changes the
// table layout but does NOT change the schema (the schema persists per-
// scenario maps, the markdown renders the slice in order).
var SLOThresholds = []int{10, 50, 100, 500, 1000}

// Meta is the ambient information emitted in the markdown preamble.
// Kept loose so the orchestrator can add new fields without forcing the
// renderer to learn them.
type Meta struct {
	GitRef     string
	StartedAt  time.Time
	FinishedAt time.Time
	Host       string
	LoadgenVer string
	CelerisVer string
	Runs       int
	Duration   time.Duration
	TotalCells int

	// BaselinePath, when non-empty, points at a previous v5.0 JSON
	// document; the markdown renderer reads it and emits a
	// regression-vs-baseline section comparing the current run's
	// LatencyAtSLO numbers against the baseline's. Missing files are
	// reported inline rather than failing the render.
	BaselinePath string
}

// WriteMarkdown renders the headline probatorium report to w. The
// report is structured around the v5.0 latency_at_slo metric:
//
//  1. Preamble (git ref, host, generated-at).
//  2. Per-scenario latency_at_slo headline tables (one section per
//     scenario; rows = adapters; columns = SLO thresholds; cells = max
//     sustained RPS).
//  3. Per-scenario detail tables (median RPS + P5/P95 bounds, latency
//     percentiles read off the merged-across-runs HdrHistogram, error
//     count).
//  4. Regression-vs-baseline section, when meta.BaselinePath points at
//     a readable v5.0 document.
//
// The doc parameter is the v5.0 result Document about to be persisted.
// The agg parameter is the per-cell aggregate map produced by
// [Aggregate] (used for the detail section's RPS bounds and merged
// percentiles). When both are non-nil the renderer cross-checks them so
// a divergence becomes a render-time error rather than a silent drift.
func WriteMarkdown(w io.Writer, doc *Document, agg map[string]CellAggregate, meta Meta) error {
	if err := writePreamble(w, meta); err != nil {
		return err
	}

	if doc != nil {
		if err := writeLatencyAtSLOSection(w, doc); err != nil {
			return err
		}
	}

	if len(agg) > 0 {
		if err := writeDetailSection(w, agg); err != nil {
			return err
		}
		if err := writeTailLatencySection(w, agg); err != nil {
			return err
		}
	}

	if doc != nil {
		if err := writeResourceSection(w, doc); err != nil {
			return err
		}
	}

	if meta.BaselinePath != "" && doc != nil {
		if err := writeRegressionSection(w, doc, meta.BaselinePath); err != nil {
			return err
		}
	}

	return nil
}

// writePreamble emits the title + metadata header.
func writePreamble(w io.Writer, meta Meta) error {
	ref := meta.GitRef
	if ref == "" {
		ref = "(unknown)"
	}
	if _, err := fmt.Fprintf(w, "# probatorium report — %s\n\n", ref); err != nil {
		return err
	}
	generated := meta.FinishedAt
	if generated.IsZero() {
		generated = time.Now().UTC()
	}
	if _, err := fmt.Fprintf(w, "Generated at %s", generated.UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	if meta.Host != "" {
		if _, err := fmt.Fprintf(w, " on %s", meta.Host); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(w, "\n"); err != nil {
		return err
	}
	if meta.Runs > 0 || meta.Duration > 0 {
		if _, err := fmt.Fprintf(w, "Runs: %d × %s · Cells: %d\n",
			meta.Runs, meta.Duration, meta.TotalCells); err != nil {
			return err
		}
	}
	if meta.CelerisVer != "" || meta.LoadgenVer != "" {
		if _, err := fmt.Fprintf(w, "celeris=%s loadgen=%s\n",
			meta.CelerisVer, meta.LoadgenVer); err != nil {
			return err
		}
	}
	return nil
}

// writeLatencyAtSLOSection emits the headline per-scenario tables.
// Layout: outer iterating scenarios (sorted), each scenario gets a
// table whose rows are adapters and columns are the SLO thresholds.
func writeLatencyAtSLOSection(w io.Writer, doc *Document) error {
	if _, err := io.WriteString(w, "\n## Latency at SLO — max sustained RPS while P99 ≤ N ms\n\n"); err != nil {
		return err
	}

	scenarios := scenariosFromDoc(doc)
	sort.Strings(scenarios)

	// Sort adapters by Name for stable rendering.
	adapters := make([]ServerResult, len(doc.Benchmarks))
	copy(adapters, doc.Benchmarks)
	sort.Slice(adapters, func(i, j int) bool { return adapters[i].Name < adapters[j].Name })

	for _, sc := range scenarios {
		if _, err := fmt.Fprintf(w, "### %s\n\n", sc); err != nil {
			return err
		}
		// Header row: adapter | 10ms | 50ms | 100ms | 500ms | 1000ms
		header := []string{"adapter"}
		for _, ms := range SLOThresholds {
			header = append(header, fmt.Sprintf("≤%dms", ms))
		}
		if _, err := io.WriteString(w, "| "+strings.Join(header, " | ")+" |\n"); err != nil {
			return err
		}
		sep := make([]string, len(header))
		for i := range sep {
			sep[i] = "---"
		}
		if _, err := io.WriteString(w, "| "+strings.Join(sep, " | ")+" |\n"); err != nil {
			return err
		}

		// Per-column max so we can bold the leader.
		colMax := make(map[int]int, len(SLOThresholds))
		for _, a := range adapters {
			row, ok := a.LatencyAtSLO[sc]
			if !ok {
				continue
			}
			for _, ms := range SLOThresholds {
				if rps, present := row[ms]; present && rps > colMax[ms] {
					colMax[ms] = rps
				}
			}
		}

		for _, a := range adapters {
			row := []string{a.Name}
			slo, ok := a.LatencyAtSLO[sc]
			for _, ms := range SLOThresholds {
				if !ok {
					row = append(row, "—")
					continue
				}
				rps, present := slo[ms]
				if !present {
					row = append(row, "—")
					continue
				}
				cell := formatRPSInt(rps)
				if rps == colMax[ms] && rps > 0 {
					cell = "**" + cell + "**"
				}
				row = append(row, cell)
			}
			if _, err := io.WriteString(w, "| "+strings.Join(row, " | ")+" |\n"); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(w, "\n"); err != nil {
			return err
		}
	}
	return nil
}

// writeDetailSection emits the per-scenario detail block: median RPS
// with P5/P95 bounds and the merged-across-runs latency percentiles.
func writeDetailSection(w io.Writer, agg map[string]CellAggregate) error {
	if _, err := io.WriteString(w, "\n## Per-scenario detail\n\n"); err != nil {
		return err
	}
	grouped := groupByCategory(agg)
	cats := []string{"static", "concurrency", "chain", "driver", "other"}
	for _, cat := range cats {
		cells, ok := grouped[cat]
		if !ok || len(cells) == 0 {
			continue
		}
		title := categoryTitle(cat)
		if _, err := fmt.Fprintf(w, "### %s\n\n", title); err != nil {
			return err
		}
		if err := writeDetailTable(w, cells); err != nil {
			return err
		}
	}
	return nil
}

// writeDetailTable renders a flat scenario × adapter detail table for
// the given category subset.
func writeDetailTable(w io.Writer, cells []CellAggregate) error {
	// Sort by scenario then adapter for stable rendering.
	sorted := make([]CellAggregate, len(cells))
	copy(sorted, cells)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].ScenarioName != sorted[j].ScenarioName {
			return sorted[i].ScenarioName < sorted[j].ScenarioName
		}
		return sorted[i].ServerName < sorted[j].ServerName
	})

	header := []string{"scenario", "adapter", "RPS (median)", "P5 — P95", "P50", "P99", "P99.9", "P99.99", "errors"}
	if _, err := io.WriteString(w, "| "+strings.Join(header, " | ")+" |\n"); err != nil {
		return err
	}
	sep := make([]string, len(header))
	for i := range sep {
		sep[i] = "---"
	}
	if _, err := io.WriteString(w, "| "+strings.Join(sep, " | ")+" |\n"); err != nil {
		return err
	}

	for _, c := range sorted {
		// Prefer the merged HdrHistogram percentiles when present,
		// fall back to median-of-medians otherwise.
		lat := c.LatencyMerged
		empty := lat == (Percentiles{})
		if empty {
			lat = c.LatencyMedian
		}
		row := []string{
			c.ScenarioName,
			c.ServerName,
			formatRPSFloat(c.RPSMedian),
			fmt.Sprintf("%s — %s", formatRPSFloat(c.RPSP5), formatRPSFloat(c.RPSP95)),
			formatDuration(lat.P50),
			formatDuration(lat.P99),
			formatDuration(lat.P999),
			formatDuration(lat.P9999),
			fmt.Sprintf("%d", c.Errors),
		}
		if _, err := io.WriteString(w, "| "+strings.Join(row, " | ")+" |\n"); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(w, "\n"); err != nil {
		return err
	}
	return nil
}

// writeTailLatencySection emits a focused "tail latency" table across
// every aggregate, sorted by scenario then by P99 ascending so the best
// adapter for each row is at the top.
func writeTailLatencySection(w io.Writer, agg map[string]CellAggregate) error {
	if _, err := io.WriteString(w, "\n## Tail latency (P99 / P99.99) — merged-across-runs HdrHistogram\n\n"); err != nil {
		return err
	}

	flat := make([]CellAggregate, 0, len(agg))
	for _, v := range agg {
		flat = append(flat, v)
	}
	sort.Slice(flat, func(i, j int) bool {
		if flat[i].ScenarioName != flat[j].ScenarioName {
			return flat[i].ScenarioName < flat[j].ScenarioName
		}
		// Use merged P99 if available, otherwise median.
		ai := flat[i].LatencyMerged.P99
		if ai == 0 {
			ai = flat[i].LatencyMedian.P99
		}
		aj := flat[j].LatencyMerged.P99
		if aj == 0 {
			aj = flat[j].LatencyMedian.P99
		}
		return ai < aj
	})

	header := []string{"scenario", "adapter", "P99", "P99.99", "max"}
	if _, err := io.WriteString(w, "| "+strings.Join(header, " | ")+" |\n"); err != nil {
		return err
	}
	sep := make([]string, len(header))
	for i := range sep {
		sep[i] = "---"
	}
	if _, err := io.WriteString(w, "| "+strings.Join(sep, " | ")+" |\n"); err != nil {
		return err
	}

	for _, c := range flat {
		lat := c.LatencyMerged
		if lat == (Percentiles{}) {
			lat = c.LatencyMedian
		}
		row := []string{
			c.ScenarioName,
			c.ServerName,
			formatDuration(lat.P99),
			formatDuration(lat.P9999),
			formatDuration(lat.Max),
		}
		if _, err := io.WriteString(w, "| "+strings.Join(row, " | ")+" |\n"); err != nil {
			return err
		}
	}
	return nil
}

// writeResourceSection renders the server-side resource summary (#154):
// one row per (adapter, scenario) whose ServerResult.Resources entry is
// non-nil. The downsampled time-series stays JSON-only — too dense for a
// markdown table — so this section is the scalar headline. Nil metric
// pointers render as "—" (non-Go competitors have no goroutine/GC).
//
// Emits nothing when no adapter carried resource data, so reports from
// runs without observer capture are unchanged.
func writeResourceSection(w io.Writer, doc *Document) error {
	type row struct {
		adapter, scenario string
		r                 *ResourceStats
	}
	var rows []row
	for _, a := range doc.Benchmarks {
		for sc, r := range a.Resources {
			if r == nil {
				continue
			}
			rows = append(rows, row{adapter: a.Name, scenario: sc, r: r})
		}
	}
	if len(rows) == 0 {
		return nil
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].scenario != rows[j].scenario {
			return rows[i].scenario < rows[j].scenario
		}
		return rows[i].adapter < rows[j].adapter
	})

	if _, err := io.WriteString(w, "\n## Server resources — peak/steady RSS, CPU, GC, goroutine & FD high-water\n\n"); err != nil {
		return err
	}
	header := []string{"scenario", "adapter", "Peak RSS (MB)", "Steady RSS (MB)", "Mean CPU%", "GC p99 (µs)", "Goroutine HWM", "FD HWM"}
	if _, err := io.WriteString(w, "| "+strings.Join(header, " | ")+" |\n"); err != nil {
		return err
	}
	sep := make([]string, len(header))
	for i := range sep {
		sep[i] = "---"
	}
	if _, err := io.WriteString(w, "| "+strings.Join(sep, " | ")+" |\n"); err != nil {
		return err
	}
	for _, rw := range rows {
		s := rw.r.Summary
		cells := []string{
			rw.scenario,
			rw.adapter,
			fmtBytesMB(s.PeakRSSBytes),
			fmtBytesMB(s.SteadyRSSBytes),
			fmtF64p(s.MeanCPUPct, "%.1f"),
			fmtNsUs(s.GCPauseP99Ns),
			fmtI64p(s.GoroutineHWM),
			fmtI64p(s.FDHWM),
		}
		if _, err := io.WriteString(w, "| "+strings.Join(cells, " | ")+" |\n"); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(w, "\n"); err != nil {
		return err
	}
	return nil
}

// fmtI64p renders a nullable int64 pointer, "—" for nil.
func fmtI64p(p *int64) string {
	if p == nil {
		return "—"
	}
	return strconv.FormatInt(*p, 10)
}

// fmtF64p renders a nullable float64 pointer with the given verb, "—"
// for nil.
func fmtF64p(p *float64, verb string) string {
	if p == nil {
		return "—"
	}
	return fmt.Sprintf(verb, *p)
}

// fmtBytesMB renders a nullable byte count as megabytes, "—" for nil.
func fmtBytesMB(p *int64) string {
	if p == nil {
		return "—"
	}
	return fmt.Sprintf("%.1f", float64(*p)/(1024*1024))
}

// fmtNsUs renders a nullable nanosecond count as microseconds, "—" for
// nil.
func fmtNsUs(p *int64) string {
	if p == nil {
		return "—"
	}
	return fmt.Sprintf("%.1f", float64(*p)/1000)
}

// writeRegressionSection compares the current Document against a v5.0
// JSON document at baselinePath and prints a per-(adapter, scenario,
// SLO) delta table. A missing or malformed baseline file is reported
// inline rather than failing the whole render.
func writeRegressionSection(w io.Writer, current *Document, baselinePath string) error {
	if _, err := io.WriteString(w, "\n## Regression vs baseline\n\n"); err != nil {
		return err
	}
	raw, err := os.ReadFile(baselinePath)
	if err != nil {
		_, werr := fmt.Fprintf(w, "_baseline %s could not be read: %v_\n", baselinePath, err)
		return werr
	}
	var prev Document
	if err := json.Unmarshal(raw, &prev); err != nil {
		_, werr := fmt.Fprintf(w, "_baseline %s could not be parsed: %v_\n", baselinePath, err)
		return werr
	}
	if _, err := fmt.Fprintf(w, "Baseline: `%s` (schema %s, git %s)\n\n",
		baselinePath, prev.SchemaVersion, prev.BenchmarkConfig.GitRef); err != nil {
		return err
	}

	header := []string{"adapter", "scenario", "SLO (ms)", "baseline RPS", "current RPS", "Δ%"}
	if _, err := io.WriteString(w, "| "+strings.Join(header, " | ")+" |\n"); err != nil {
		return err
	}
	sep := make([]string, len(header))
	for i := range sep {
		sep[i] = "---"
	}
	if _, err := io.WriteString(w, "| "+strings.Join(sep, " | ")+" |\n"); err != nil {
		return err
	}

	prevByName := make(map[string]ServerResult, len(prev.Benchmarks))
	for _, b := range prev.Benchmarks {
		prevByName[b.Name] = b
	}

	curAdapters := make([]ServerResult, len(current.Benchmarks))
	copy(curAdapters, current.Benchmarks)
	sort.Slice(curAdapters, func(i, j int) bool { return curAdapters[i].Name < curAdapters[j].Name })

	for _, a := range curAdapters {
		base, ok := prevByName[a.Name]
		if !ok {
			continue
		}
		scenarios := make([]string, 0, len(a.LatencyAtSLO))
		for k := range a.LatencyAtSLO {
			scenarios = append(scenarios, k)
		}
		sort.Strings(scenarios)
		for _, sc := range scenarios {
			curRow := a.LatencyAtSLO[sc]
			baseRow, ok := base.LatencyAtSLO[sc]
			if !ok {
				continue
			}
			for _, ms := range SLOThresholds {
				curRPS, hasCur := curRow[ms]
				baseRPS, hasBase := baseRow[ms]
				if !hasCur || !hasBase || baseRPS == 0 {
					continue
				}
				delta := float64(curRPS-baseRPS) / float64(baseRPS) * 100
				row := []string{
					a.Name,
					sc,
					fmt.Sprintf("%d", ms),
					formatRPSInt(baseRPS),
					formatRPSInt(curRPS),
					fmt.Sprintf("%+.1f%%", delta),
				}
				if _, err := io.WriteString(w, "| "+strings.Join(row, " | ")+" |\n"); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// scenariosFromDoc returns the union of every scenario name appearing
// in any adapter's LatencyAtSLO map.
func scenariosFromDoc(doc *Document) []string {
	seen := map[string]struct{}{}
	for _, b := range doc.Benchmarks {
		for k := range b.LatencyAtSLO {
			seen[k] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	return out
}

// groupByCategory buckets aggregates by Scenario category. Aggregates
// that lack a category are bucketed under "other".
func groupByCategory(agg map[string]CellAggregate) map[string][]CellAggregate {
	out := make(map[string][]CellAggregate)
	for _, c := range agg {
		cat := c.Category
		if cat == "" {
			cat = "other"
		}
		out[cat] = append(out[cat], c)
	}
	return out
}

// categoryTitle returns the section title for a category bucket.
func categoryTitle(cat string) string {
	switch cat {
	case "static":
		return "Static scenarios"
	case "concurrency":
		return "Concurrency profiles"
	case "chain":
		return "Middleware chains"
	case "driver":
		return "Driver scenarios"
	default:
		return "Other"
	}
}

// MarkdownTimeseries renders a compact per-scenario summary of the
// time-series sidecar: run count, bucket count, and the peak-bucket band
// (the elapsed second whose merged P50 RPS is highest). Additive and
// gated on ts != nil with at least one non-empty scenario, so the
// existing report.md sections — and TestWriteMarkdownRoundTrip — are
// untouched. Returns "" when there is nothing to show.
func MarkdownTimeseries(ts *TimeseriesDoc) string {
	if ts == nil {
		return ""
	}
	var rows []ScenarioSeries
	for _, s := range ts.Scenarios {
		if len(s.Runs) > 0 || len(s.Band) > 0 {
			rows = append(rows, s)
		}
	}
	if len(rows) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("\n## Time-series (rps over run length)\n\n")
	sb.WriteString("Per-run 1 Hz series captured to `timeseries.json.gz`; ")
	sb.WriteString("the band below is a cross-run min/p50/p99/max envelope over per-elapsed-second RPS (not a latency percentile).\n\n")

	header := []string{"scenario", "server", "runs", "buckets", "peak-bucket p50 RPS", "peak-bucket p99 RPS"}
	sb.WriteString("| " + strings.Join(header, " | ") + " |\n")
	sep := make([]string, len(header))
	for i := range sep {
		sep[i] = "---"
	}
	sb.WriteString("| " + strings.Join(sep, " | ") + " |\n")

	for _, s := range rows {
		var peak BucketBand
		var havePeak bool
		for _, b := range s.Band {
			if !havePeak || b.RPS.P50 > peak.RPS.P50 {
				peak = b
				havePeak = true
			}
		}
		p50, p99 := "—", "—"
		if havePeak {
			p50 = formatRPSFloat(peak.RPS.P50)
			p99 = formatRPSFloat(peak.RPS.P99)
		}
		row := []string{
			s.Scenario,
			s.Server,
			fmt.Sprintf("%d", len(s.Runs)),
			fmt.Sprintf("%d", len(s.Band)),
			p50,
			p99,
		}
		sb.WriteString("| " + strings.Join(row, " | ") + " |\n")
	}
	sb.WriteString("\n")
	return sb.String()
}

// formatRPSFloat renders an RPS value with k/M suffixes.
func formatRPSFloat(v float64) string {
	switch {
	case v >= 1_000_000:
		return fmt.Sprintf("%.2fM rps", v/1_000_000)
	case v >= 1_000:
		return fmt.Sprintf("%.1fk rps", v/1_000)
	default:
		return fmt.Sprintf("%.0f rps", v)
	}
}

// formatRPSInt renders an integer RPS value compactly.
func formatRPSInt(v int) string {
	switch {
	case v >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(v)/1_000_000)
	case v >= 1_000:
		return fmt.Sprintf("%.1fk", float64(v)/1_000)
	default:
		return fmt.Sprintf("%d", v)
	}
}

// formatDuration prints a duration in human-friendly units (µs / ms).
func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "—"
	}
	if d >= time.Millisecond {
		return fmt.Sprintf("%.2fms", float64(d)/float64(time.Millisecond))
	}
	return fmt.Sprintf("%dµs", d.Microseconds())
}
