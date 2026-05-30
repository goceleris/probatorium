package report

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// FlattenLatencyAtSLO walks an arbitrary decoded JSON tree and returns every
// numeric value living under a "latency_at_slo" key, keyed by its full dotted
// path. Maps descend with "/<key>" and arrays with "[i]". A latency_at_slo node
// that is itself a map (ms->rps) contributes one leaf per inner entry; a scalar
// latency_at_slo contributes a single leaf at the key path. The path is what the
// gate diffs on, so it must be stable across baseline and current runs.
func FlattenLatencyAtSLO(tree any) map[string]float64 {
	out := map[string]float64{}
	var walk func(prefix string, node any, underSLO bool)
	walk = func(prefix string, node any, underSLO bool) {
		switch v := node.(type) {
		case map[string]any:
			keys := make([]string, 0, len(v))
			for k := range v {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				child := prefix + "/" + k
				if underSLO {
					if f, ok := sloNumeric(v[k]); ok {
						out[child] = f
						continue
					}
				}
				walk(child, v[k], underSLO || k == "latency_at_slo")
			}
		case []any:
			for i, e := range v {
				walk(fmt.Sprintf("%s[%d]", prefix, i), e, underSLO)
			}
		default:
			if underSLO {
				if f, ok := sloNumeric(node); ok {
					out[prefix] = f
				}
			}
		}
	}
	walk("", tree, false)
	return out
}

// sloNumeric coerces a decoded JSON scalar to float64. json.Number is handled
// for trees loaded via LoadResultsTree (UseNumber); float64 for the standard
// decoder. The package-level numeric in document.go covers the typed path; this
// adds json.Number support for the generic-tree gate.
func sloNumeric(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

// Regression is one cell whose latency_at_slo moved relative to baseline.
type Regression struct {
	Path      string
	Baseline  float64
	Current   float64
	Delta     float64 // (current-baseline)/baseline; negative == worse
	Regressed bool
	Missing   bool // present in baseline, absent in current
}

// DiffLatencyAtSLO compares two decoded results trees. latency_at_slo is a
// throughput-at-SLO metric (bigger is better), so a cell regresses when its
// value drops by more than threshold (a fractional drop, e.g. 0.05 == 5%), or
// when a baseline cell vanishes entirely. Returns the per-path diffs (sorted by
// path) and whether any regressed.
func DiffLatencyAtSLO(baseline, current any, threshold float64) ([]Regression, bool) {
	base := FlattenLatencyAtSLO(baseline)
	curr := FlattenLatencyAtSLO(current)
	paths := make([]string, 0, len(base))
	for p := range base {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	regs := make([]Regression, 0, len(paths))
	any := false
	for _, p := range paths {
		b := base[p]
		c, present := curr[p]
		r := Regression{Path: p, Baseline: b, Current: c}
		switch {
		case b == 0:
			// No baseline signal: nothing to regress against.
		case !present:
			r.Missing = true
			r.Regressed = true
		default:
			r.Delta = (c - b) / b
			if r.Delta < -threshold {
				r.Regressed = true
			}
		}
		if r.Regressed {
			any = true
		}
		regs = append(regs, r)
	}
	return regs, any
}

// RenderRegressionReport produces a human-readable diff. Regressed lines are
// marked with " !!" so callers (and tests) can detect them textually.
func RenderRegressionReport(regs []Regression, threshold float64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "latency_at_slo regression gate (threshold %.0f%%, bigger is better)\n",
		threshold*100)
	for _, r := range regs {
		switch {
		case r.Missing:
			fmt.Fprintf(&b, "  %s: baseline=%.0f current=missing !!\n", r.Path, r.Baseline)
		case r.Baseline == 0:
			fmt.Fprintf(&b, "  %s: baseline=0 current=%.0f n/a\n", r.Path, r.Current)
		default:
			mark := ""
			if r.Regressed {
				mark = " !!"
			}
			fmt.Fprintf(&b, "  %s: baseline=%.0f current=%.0f (%+.1f%%)%s\n",
				r.Path, r.Baseline, r.Current, r.Delta*100, mark)
		}
	}
	return b.String()
}

// LoadResultsTree decodes a results.json (or any JSON file) into a generic tree
// suitable for FlattenLatencyAtSLO / DiffLatencyAtSLO. Numbers are decoded as
// json.Number so large integer RPS values survive without float rounding.
func LoadResultsTree(path string) (any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	return tree, nil
}
