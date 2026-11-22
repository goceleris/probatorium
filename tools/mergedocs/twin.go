package main

import (
	"sort"
	"strings"

	"github.com/goceleris/probatorium/report"
)

// TwinOutput is the v1.5.11 comparison written beside the merged document
// when --twin=separate: a values table (twin-comparison.json) and the lossless
// stripped rows and series (twin-rows.json). Neither is read by mage Publish.
type TwinOutput struct {
	Comparison TwinComparison
	Rows       TwinRows
}

// TwinRows is the lossless copy of what was stripped from the document.
type TwinRows struct {
	Benchmarks []report.ServerResult   `json:"benchmarks"`
	Series     []report.ScenarioSeries `json:"timeseries,omitempty"`
}

// TwinComparison pairs each twin column with the candidate column of the same
// engine and lists the saturation rate of both per scenario. It holds
// measurements and the candidate/twin quotient of one dispatch only; nothing in
// it is a rank.
type TwinComparison struct {
	TwinVersion string     `json:"twin_framework_version"`
	Rows        []TwinPair `json:"rows"`
	Unmatched   []string   `json:"unmatched_twins,omitempty"`
}

// TwinPair is one engine's candidate/twin pair.
type TwinPair struct {
	Twin      string                  `json:"twin"`
	Candidate string                  `json:"candidate"`
	Scenarios map[string]TwinScenario `json:"scenarios"`
}

// TwinScenario is one scenario of a pair. Ratio is candidate/twin, null when
// the twin rate is not positive.
type TwinScenario struct {
	CandidateRPS float64  `json:"candidate_saturation_rps"`
	TwinRPS      float64  `json:"twin_saturation_rps"`
	Ratio        *float64 `json:"ratio_candidate_over_twin"`
	CandidateOK  bool     `json:"candidate_ok"`
	TwinOK       bool     `json:"twin_ok"`
}

func buildTwin(twins []report.ServerResult, series []report.ScenarioSeries, candidateRows []report.ServerResult) *TwinOutput {
	byName := map[string]*report.ServerResult{}
	for i := range candidateRows {
		byName[candidateRows[i].Name] = &candidateRows[i]
	}
	sort.SliceStable(twins, func(i, j int) bool { return twins[i].Name < twins[j].Name })
	out := &TwinOutput{Rows: TwinRows{Benchmarks: twins, Series: series}}
	for i := range twins {
		t := &twins[i]
		if out.Comparison.TwinVersion == "" {
			out.Comparison.TwinVersion = t.FrameworkVersion
		}
		cand := byName["celeris-"+strings.TrimPrefix(t.Name, twinPrefix)]
		if cand == nil {
			out.Comparison.Unmatched = append(out.Comparison.Unmatched, t.Name)
			continue
		}
		pair := TwinPair{Twin: t.Name, Candidate: cand.Name, Scenarios: map[string]TwinScenario{}}
		for sc, tv := range t.SaturationModeRPS {
			cv, ok := cand.SaturationModeRPS[sc]
			if !ok {
				continue
			}
			ts := TwinScenario{CandidateRPS: cv, TwinRPS: tv, CandidateOK: cellOK(cand, sc), TwinOK: cellOK(t, sc)}
			if tv > 0 {
				r := cv / tv
				ts.Ratio = &r
			}
			pair.Scenarios[sc] = ts
		}
		out.Comparison.Rows = append(out.Comparison.Rows, pair)
	}
	return out
}
