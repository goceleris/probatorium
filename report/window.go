package report

import "time"

// window.go is the per-SCENARIO slice of a bench column's resource
// sampling (schema v5.9, celeris#585).
//
// In the cluster harness a "cell" is one (run_index, competitor) column
// pass: the SUT is launched once, ONE `mpstat -P ALL 1 N` and ONE observer
// sidecar run for the whole guard window, and the single runner invocation
// drives every scenario the -cells glob matched back-to-back inside it.
// The column-wide ResourceStats (SummarizeResources over the whole log)
// therefore describes the average over every scenario the column ran plus
// the idle edges — the 27 scenarios of a celeris column all carried the
// identical mean_cpu_pct, and no per-cell CPU comparison was possible.
//
// WindowResources cuts the same raw series to one scenario's own runner
// window (its per-cell started_at + warm-up .. completed_at, both UTC)
// and summarises ONLY the samples that fell inside, adding mpstat %soft
// and the SUT process's own utime+stime CPU where the raw logs carry
// them. A window that caught no sample yields nil + WindowNoData — never
// a zero-valued summary — so an unjoinable window is loud.

// Window status values recorded next to a per-scenario slice so the
// absence of a slice is attributable at a glance.
const (
	// WindowOK: at least one sample fell in the window; the slice is set.
	WindowOK = "ok"
	// WindowNoData: the window is well-formed but no mpstat row and no
	// observer row carries a timestamp inside it. Typical causes: the
	// cpu.log has no parseable wall-clock (pre-ISO/TZ launch on a host
	// whose locale the parser does not know), the sampler died early, or
	// the SUT clock and the runner clock disagree.
	WindowNoData = "no_data"
	// WindowNoTimestamps: the scenario record carries no started_at /
	// completed_at (pre-v5.9 per-cell JSON), so no window can be formed.
	WindowNoTimestamps = "no_timestamps"
	// WindowEmpty: trimming the warm-up moved the start past the end (a
	// cell that died inside its warm-up); nothing to summarise.
	WindowEmpty = "empty_window"
	// WindowNoSidecar: the column ran without cpu.log and observer.sqlite
	// at all (no sampler), so there is nothing to slice.
	WindowNoSidecar = "no_sidecar"
)

// ScenarioWindow derives the measurement window of one scenario from the
// runner's per-cell timestamps: the warm-up is trimmed off the front
// (loadgen ramps during it and the numbers are discarded), the end is the
// cell's completed_at. Returns zero times when either input is zero.
func ScenarioWindow(startedAt, completedAt time.Time, warmup time.Duration) (start, end time.Time) {
	if startedAt.IsZero() || completedAt.IsZero() {
		return time.Time{}, time.Time{}
	}
	if warmup < 0 {
		warmup = 0
	}
	return startedAt.Add(warmup).UTC(), completedAt.UTC()
}

// WindowResources slices the column-wide raw series to [start, end]
// (inclusive, 1 s resolution) and summarises the slice. Only mpstat rows
// with a reconstructed wall-clock (CPUPoint.TSUnix != 0) can be placed,
// so a log without a parseable date contributes no CPU samples.
//
// The returned status is one of the Window* constants; the stats are nil
// for every status but WindowOK.
func WindowResources(samples []ObserverSample, cpu []CPUPoint, start, end time.Time) (*ResourceStats, string) {
	if len(samples) == 0 && len(cpu) == 0 {
		return nil, WindowNoSidecar
	}
	if start.IsZero() || end.IsZero() {
		return nil, WindowNoTimestamps
	}
	if end.Before(start) {
		return nil, WindowEmpty
	}
	s0, s1 := start.Unix(), end.Unix()

	var obs []ObserverSample
	for _, o := range samples {
		if o.TSUnix >= s0 && o.TSUnix <= s1 {
			obs = append(obs, o)
		}
	}
	var cpuW []CPUPoint
	var busySum, softSum float64
	softN := 0
	for _, c := range cpu {
		if c.TSUnix == 0 || c.TSUnix < s0 || c.TSUnix > s1 {
			continue
		}
		cpuW = append(cpuW, c)
		busySum += c.CPUPct
		if c.SoftPct >= 0 {
			softSum += c.SoftPct
			softN++
		}
	}
	if len(obs) == 0 && len(cpuW) == 0 {
		return nil, WindowNoData
	}

	cpuOK := len(cpuW) > 0
	cpuMean := 0.0
	if cpuOK {
		cpuMean = busySum / float64(len(cpuW))
	}
	stats := SummarizeResources(obs, cpuMean, cpuOK, cpuW)
	if softN > 0 {
		stats.Summary.MeanSoftPct = ptrF64(softSum / float64(softN))
	}
	if pct, ok := processCPUPct(obs); ok {
		stats.Summary.SUTProcessCPUPct = ptrF64(pct)
	}
	stats.Window = &ResourceWindow{
		Start:           start.UTC(),
		End:             end.UTC(),
		CPUSamples:      len(cpuW),
		ObserverSamples: len(obs),
	}
	return &stats, WindowOK
}

// processCPUPct derives the SUT process's CPU over the window from the
// first and last observer samples that carry cpu ticks: (utime+stime
// delta / USER_HZ) / wall seconds × 100, i.e. percent of ONE core. ok is
// false with fewer than two tick-bearing samples, a zero wall delta, or
// a tick counter that went backwards (the observer pinned a PID the
// respawn supervisor replaced, so the counter is not one process).
func processCPUPct(obs []ObserverSample) (float64, bool) {
	var first, last *ObserverSample
	for i := range obs {
		if !obs[i].CPUTicksOK {
			continue
		}
		if first == nil {
			first = &obs[i]
		}
		last = &obs[i]
	}
	if first == nil || last == nil || last == first {
		return 0, false
	}
	wall := last.TSUnix - first.TSUnix
	ticks := last.CPUTicks - first.CPUTicks
	if wall <= 0 || ticks < 0 {
		return 0, false
	}
	return float64(ticks) / userHZ / float64(wall) * 100, true
}
