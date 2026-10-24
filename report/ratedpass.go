package report

import (
	"sort"
	"time"
)

// Rated pass records (schema 5.24, probatorium#441).
//
// The runner drives one rated pass per fraction of a cell's saturation rate.
// Before 5.24 it kept {target, p99} for a pass that returned requests and
// dropped every other pass with one stderr line, so the artifact could not say
// how much load loadgen delivered, how many requests failed, or that a pass
// was missing. A RatedPassRecord is one pass, whatever became of it.
//
// Kept passes (status "ok") are the ones latency_at_slo and
// rated_mode_p99_at_target_rps are built from, exactly as before. A pass that
// is not kept (skipped, not_run) is recorded beside them and never enters
// either number. On the wire (the runner's per-cell JSON and raw/<host>.json)
// the two live in separate arrays, rated_passes and rated_passes_not_kept, so
// that a reader that predates 5.24 and reads target_rps and p99 out of
// rated_passes never counts a pass that sent nothing as a pass with a p99 of
// zero. The merged document lists them together, in ServerResult.RatedPasses.

// Rated pass statuses.
const (
	// RatedPassOK: loadgen returned a result with requests. The pass is
	// counted in latency_at_slo and the headline p99.
	RatedPassOK = "ok"
	// RatedPassSkipped: the pass ran and produced no usable result (loadgen
	// returned an error, a nil result, or zero requests). Reason says which.
	RatedPassSkipped = "skipped"
	// RatedPassNotRun: the pass never started (loadgen could not be built, or
	// the cell was flagged before its sweep, or the target was not positive).
	RatedPassNotRun = "not_run"
)

// Rated pass reasons, the class of why a pass is not kept.
const (
	RatedReasonLoadgenNew   = "loadgen_new"        // loadgen.New failed
	RatedReasonRunError     = "run_error"          // loadgen.Run returned an error
	RatedReasonNilResult    = "nil_result"         // loadgen.Run returned neither a result nor an error
	RatedReasonZeroRequests = "zero_requests"      // loadgen.Run returned a result with no requests
	RatedReasonInterrupted  = "interrupted"        // the run's context was cancelled during the pass
	RatedReasonCellFlagged  = "cell_flagged"       // the saturation pass was flagged (suspect), so no sweep ran
	RatedReasonNoSatRate    = "no_saturation_rate" // the saturation pass measured no rate to take fractions of
	RatedReasonBadTarget    = "non_positive_target"
)

// RatedPassRecord is one rated pass of one run of a cell. The first two keys
// and p99 are the ones every reader before 5.24 uses; the rest is additive.
//
// A count that is absent is zero when Status is present (the runner omits
// zeros to keep the file small). A record without Status comes from a runner
// that predates 5.24: only TargetRPS and P99 were recorded, and a missing
// AchievedRPS there means "not measured", not "nothing delivered".
type RatedPassRecord struct {
	// Fraction of the cell's saturation rate this pass offered (0.25, 0.5,
	// 0.75, 0.9 by default). Absent on a pre-5.24 record.
	Fraction float64 `json:"fraction,omitempty"`
	// TargetRPS is the offered rate. Zero on a not_run pass whose target was
	// never computed (cell_flagged, no_saturation_rate).
	TargetRPS float64 `json:"target_rps"`
	// P99 is the coordinated-omission-corrected p99 of the pass. Zero for a
	// pass that is not kept.
	P99 time.Duration `json:"p99"`

	// Status is RatedPassOK, RatedPassSkipped or RatedPassNotRun. Reason and
	// Detail say why a pass is not ok: Reason is one of the RatedReason*
	// classes, Detail the error text where there is one.
	Status string `json:"status,omitempty"`
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`

	// AchievedRPS is loadgen's requests per second over the measured window
	// and AchievedRatio is AchievedRPS / TargetRPS. A ratio below 1 is load
	// loadgen never sent: the p99 of such a pass is the pass's backlog, not the
	// server's latency (probatorium#441, loadgen#102).
	AchievedRPS   float64 `json:"achieved_rps,omitempty"`
	AchievedRatio float64 `json:"achieved_ratio,omitempty"`
	// Requests and Errors are loadgen's counts for the window; ConnectErrors is
	// the dial/handshake subset of Errors, so Errors - ConnectErrors are the
	// request failures. DialRetries and CloseAborts are loadgen's own counters.
	Requests      int64  `json:"requests,omitempty"`
	Errors        int64  `json:"errors,omitempty"`
	ConnectErrors uint64 `json:"connect_errors,omitempty"`
	DialRetries   uint64 `json:"dial_retries,omitempty"`
	CloseAborts   uint64 `json:"close_aborts,omitempty"`
	// ShortfallRequests is target x duration - requests - errors: the slots of
	// the schedule loadgen did not fill. Negative when it over-delivered.
	ShortfallRequests int64 `json:"shortfall_requests,omitempty"`

	// Duration is the measured window; P50, P999 and Max are loadgen's.
	Duration time.Duration `json:"duration,omitempty"`
	P50      time.Duration `json:"p50,omitempty"`
	P999     time.Duration `json:"p99_9,omitempty"`
	Max      time.Duration `json:"max,omitempty"`

	// Start and End bound the pass on the loadgen host's clock (warmup
	// included), the same clock as load_windows. LoadgenVersion is the loadgen
	// build that ran it.
	Start          time.Time `json:"start,omitzero"`
	End            time.Time `json:"end,omitzero"`
	LoadgenVersion string    `json:"loadgen_version,omitempty"`
}

// Kept reports whether the record is a pass latency_at_slo and the headline
// are built from. A record from a runner that predates 5.24 has no status and
// was kept by definition (the old runner recorded nothing else).
func (r RatedPassRecord) Kept() bool {
	return r.Status == "" || r.Status == RatedPassOK
}

// RatedPassResult is a RatedPassRecord in the merged document: Run is the
// position of the run among the cell's runs that carried rated data.
type RatedPassResult struct {
	Run int `json:"run"`
	RatedPassRecord
}

// MergeRatedPassRecords joins one run's kept passes (the wire array
// rated_passes) and its passes that were not kept (rated_passes_not_kept)
// into the run's list, ordered by fraction then target. A kept pass from a
// runner that predates 5.24 gets status "ok". Neither input is modified.
func MergeRatedPassRecords(kept, notKept []RatedPassRecord) []RatedPassRecord {
	if len(kept)+len(notKept) == 0 {
		return nil
	}
	out := make([]RatedPassRecord, 0, len(kept)+len(notKept))
	for _, k := range kept {
		if k.Status == "" {
			k.Status = RatedPassOK
		}
		out = append(out, k)
	}
	out = append(out, notKept...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Fraction != out[j].Fraction {
			return out[i].Fraction < out[j].Fraction
		}
		return out[i].TargetRPS < out[j].TargetRPS
	})
	return out
}

// ratedPassResults flattens the per-run records of a cell into the document
// list. Run counts only the runs that carried rated data.
func ratedPassResults(runs [][]RatedPassRecord) []RatedPassResult {
	var out []RatedPassResult
	run := 0
	for _, recs := range runs {
		if len(recs) == 0 {
			continue
		}
		for _, r := range recs {
			out = append(out, RatedPassResult{Run: run, RatedPassRecord: r})
		}
		run++
	}
	return out
}
