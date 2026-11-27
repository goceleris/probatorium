package main

import (
	"fmt"
	"math"
	"sort"
	"strconv"

	"github.com/goceleris/probatorium/report"
)

// Drift constants fixed by the release bot (msg_ccb8c21ee221).
const (
	tieMarginFloor     = 0.05 // margin(scenario) = max(tieMarginFloor, largest anchor drift on that scenario)
	driftWarnThreshold = 0.15 // a larger anchor drift prints a loud warning
	driftMetric        = "saturation_mode_rps"
	driftDefinition    = "max(r, 1/r) - 1 with r = d1/d2 (d1 = candidate+twin dispatch, d2 = frameworks dispatch); " +
		"measured per anchor and scenario on the scenarios measured ok in both dispatches; the tie margin of a scenario is the largest drift any anchor showed on THAT scenario, floored; max_anchor_drift is the maximum over all anchors and scenarios (reporting only)"
)

// MetricPair is one d1/d2 measurement of an anchor.
type MetricPair struct {
	Key   string   `json:"key"` // "<scenario>" or "<scenario>/<slo>"
	D1    float64  `json:"d1"`
	D2    float64  `json:"d2"`
	Ratio *float64 `json:"ratio_d1_d2"` // null when d2 is not positive
}

// AnchorReport is one anchor's two measurements.
type AnchorReport struct {
	Name        string                  `json:"name"`
	Metrics     map[string][]MetricPair `json:"metrics"`
	MaxDrift    float64                 `json:"max_drift_saturation"`
	MedianDrift float64                 `json:"median_drift_saturation"`
	MaxDriftAt  string                  `json:"max_drift_scenario"`
	Paired      int                     `json:"paired_scenarios"`
	Unpaired    []string                `json:"unpaired_scenarios,omitempty"`
	NotOK       []string                `json:"not_ok_cells,omitempty"`
}

// AnchorSection is COMPOSITION.json's `anchors`.
type AnchorSection struct {
	Names          []string       `json:"names"`
	RowSource      string         `json:"row_source"`
	Metric         string         `json:"drift_metric"`
	Definition     string         `json:"drift_definition"`
	Per            []AnchorReport `json:"per_anchor"`
	MaxAnchorDrift float64        `json:"max_anchor_drift"`
	MaxDriftAnchor string         `json:"max_drift_anchor"`
	TieMarginFloor float64        `json:"tie_margin_floor"`
	// ByScenario is the tie margin of every scenario of the merged document.
	ByScenario    map[string]ScenarioMargin `json:"tie_margin_by_scenario"`
	WarnThreshold float64                   `json:"warn_threshold"`
	DriftWarning  bool                      `json:"drift_warning"`
	Warnings      []string                  `json:"-"`
}

// ScenarioMargin is one scenario's tie margin: max(floor, the largest drift any
// anchor showed on it). A scenario no anchor measured in both dispatches has no
// drift evidence: it takes the largest margin of the architecture (NoAnchor).
type ScenarioMargin struct {
	Margin   float64 `json:"margin"`
	DrivenBy string  `json:"driven_by,omitempty"` // the anchor with the largest drift, when above the floor
	Anchors  int     `json:"anchors_paired"`
	NoAnchor bool    `json:"no_anchor,omitempty"`
	MaxDrift float64 `json:"max_anchor_drift"`
}

// metricOrder is the fixed iteration order of the anchor metrics (output and
// warnings must be deterministic).
var metricOrder = []string{"saturation_mode_rps", "rated_mode_p99_at_target_rps", "latency_at_slo", "loadgen_cpu_p95"}

// metricValues flattens an anchor row's per-scenario numbers by metric.
func metricValues(r *report.ServerResult) map[string]map[string]float64 {
	out := map[string]map[string]float64{
		"saturation_mode_rps":          {},
		"rated_mode_p99_at_target_rps": {},
		"latency_at_slo":               {},
		"loadgen_cpu_p95":              {},
	}
	for sc, v := range r.SaturationModeRPS {
		out["saturation_mode_rps"][sc] = v
	}
	for sc, v := range r.RatedModeP99AtTargetRPS {
		out["rated_mode_p99_at_target_rps"][sc] = float64(v)
	}
	for sc, m := range r.LatencyAtSLO {
		for slo, v := range m {
			out["latency_at_slo"][sc+"/"+strconv.Itoa(slo)] = float64(v)
		}
	}
	for sc, v := range r.LoadgenCPUP95 {
		out["loadgen_cpu_p95"][sc] = v
	}
	return out
}

func cellOK(r *report.ServerResult, sc string) bool {
	st, ok := r.CellStatuses[sc]
	return !ok || st == "" || st == string(report.CellOK)
}

func hasPairedSaturation(a, b *report.ServerResult) bool {
	for sc, va := range a.SaturationModeRPS {
		if vb, ok := b.SaturationModeRPS[sc]; ok && va > 0 && vb > 0 && cellOK(a, sc) && cellOK(b, sc) {
			return true
		}
	}
	return false
}

func symDrift(r float64) float64 {
	if r <= 0 {
		return math.Inf(1)
	}
	if r < 1 {
		r = 1 / r
	}
	return r - 1
}

func buildAnchors(names []string, scenarios []string, byC, byP map[string]*report.ServerResult) AnchorSection {
	sec := AnchorSection{
		Names: names, RowSource: "candidate", Metric: driftMetric, Definition: driftDefinition,
		TieMarginFloor: tieMarginFloor, WarnThreshold: driftWarnThreshold,
		ByScenario: map[string]ScenarioMargin{},
	}
	scDrift := map[string]ScenarioMargin{}
	for _, n := range names {
		rc, rp := byC[n], byP[n]
		ar := AnchorReport{Name: n, Metrics: map[string][]MetricPair{}}
		mc, mp := metricValues(rc), metricValues(rp)
		for _, metric := range metricOrder {
			vc := mc[metric]
			keys := map[string]bool{}
			for k := range vc {
				keys[k] = true
			}
			for k := range mp[metric] {
				keys[k] = true
			}
			var ks []string
			for k := range keys {
				ks = append(ks, k)
			}
			sort.Strings(ks)
			var pairs []MetricPair
			for _, k := range ks {
				d1, ok1 := vc[k]
				d2, ok2 := mp[metric][k]
				if !ok1 || !ok2 {
					if metric == driftMetric {
						ar.Unpaired = append(ar.Unpaired, k)
						sec.Warnings = append(sec.Warnings, fmt.Sprintf("anchor %s: scenario %s is measured in only one dispatch", n, k))
					}
					continue
				}
				mpair := MetricPair{Key: k, D1: d1, D2: d2}
				if d2 > 0 {
					r := d1 / d2
					mpair.Ratio = &r
				}
				pairs = append(pairs, mpair)
			}
			ar.Metrics[metric] = pairs
		}
		var drifts []float64
		for _, mp := range ar.Metrics[driftMetric] {
			sc := mp.Key
			if !cellOK(rc, sc) || !cellOK(rp, sc) {
				ar.NotOK = append(ar.NotOK, sc)
				sec.Warnings = append(sec.Warnings, fmt.Sprintf("anchor %s: scenario %s is not ok in a dispatch (candidate %q, peers %q): excluded from the drift", n, sc, rc.CellStatuses[sc], rp.CellStatuses[sc]))
				continue
			}
			if mp.Ratio == nil || mp.D1 <= 0 {
				ar.NotOK = append(ar.NotOK, sc)
				sec.Warnings = append(sec.Warnings, fmt.Sprintf("anchor %s: scenario %s has a non-positive saturation rate: excluded from the drift", n, sc))
				continue
			}
			dr := symDrift(*mp.Ratio)
			drifts = append(drifts, dr)
			sm := scDrift[sc]
			sm.Anchors++
			if dr >= sm.MaxDrift {
				sm.MaxDrift, sm.DrivenBy = dr, n
			}
			scDrift[sc] = sm
			ar.Paired++
			if dr >= ar.MaxDrift {
				ar.MaxDrift, ar.MaxDriftAt = dr, sc
			}
		}
		if len(drifts) > 0 {
			sort.Float64s(drifts)
			if m := len(drifts); m%2 == 1 {
				ar.MedianDrift = drifts[m/2]
			} else {
				ar.MedianDrift = (drifts[m/2-1] + drifts[m/2]) / 2
			}
		}
		if ar.MaxDrift >= sec.MaxAnchorDrift {
			sec.MaxAnchorDrift, sec.MaxDriftAnchor = ar.MaxDrift, n
		}
		if ar.MaxDrift > driftWarnThreshold {
			sec.DriftWarning = true
			sec.Warnings = append(sec.Warnings, fmt.Sprintf("LOUD: anchor %s differs between the dispatches by %.1f%% (scenario %s, d1/d2 = %s), above the %.0f%% threshold",
				n, ar.MaxDrift*100, ar.MaxDriftAt, ratioStr(ar, driftMetric, ar.MaxDriftAt), driftWarnThreshold*100))
		}
		sec.Per = append(sec.Per, ar)
	}
	widest := tieMarginFloor
	for _, sc := range scenarios {
		if sm, ok := scDrift[sc]; ok {
			widest = math.Max(widest, sm.MaxDrift)
		}
	}
	for _, sc := range scenarios {
		sm, ok := scDrift[sc]
		if !ok {
			sec.ByScenario[sc] = ScenarioMargin{Margin: widest, NoAnchor: true}
			sec.Warnings = append(sec.Warnings, fmt.Sprintf("scenario %s: no anchor measured ok in both dispatches; its tie margin is the architecture's widest (%.4f)", sc, widest))
			continue
		}
		sm.Margin = math.Max(tieMarginFloor, sm.MaxDrift)
		if sm.MaxDrift <= tieMarginFloor {
			sm.DrivenBy = ""
		}
		sec.ByScenario[sc] = sm
	}
	return sec
}

func ratioStr(ar AnchorReport, metric, key string) string {
	for _, p := range ar.Metrics[metric] {
		if p.Key == key && p.Ratio != nil {
			return strconv.FormatFloat(*p.Ratio, 'f', 4, 64)
		}
	}
	return "n/a"
}
