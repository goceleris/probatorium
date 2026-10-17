package report

import (
	"fmt"
	"sort"
)

// InformationalLines renders, for ValidateGate's output, every cell in which
// the informational twin of a gated predicate (schema 5.20,
// [Tier1Summary.PropertyInformational]) declared a violation. For I-MEM-1 the
// twin is the rule as it was before the scatter floor, so the lines show
// everything the floor absolved.
//
// It reads cells and writes nothing: [Gate] never looks at the field, and the
// first line says so. It always returns at least that line, so a soak that
// absolved nothing says that too instead of saying nothing.
func InformationalLines(cells []ValidationCellResult) []string {
	type row struct {
		id, line string
		absolved bool
	}
	var rows []row
	for _, c := range cells {
		if c.Tier1 == nil {
			continue
		}
		for id, t := range c.Tier1.PropertyInformational {
			if t.Violations == 0 {
				continue
			}
			tag := "also tripped by the gated rule"
			if t.Absolved {
				tag = "ABSOLVED: the gated rule did not trip"
			} else if t.AbsolvedEvaluations > 0 {
				tag = fmt.Sprintf("the gated rule tripped too, %d of these evaluations are ones it absolved", t.AbsolvedEvaluations)
			}
			msg := t.FirstMessage
			if len(msg) > 160 {
				msg = msg[:157] + "..."
			}
			rows = append(rows, row{id: id, absolved: t.Absolved, line: fmt.Sprintf(
				"  %-24s %-8s %-12s %-8s %6d violation(s), %s; first: %s",
				c.Refapp, c.Engine, c.Arch, id, t.Violations, tag, msg)})
		}
	}
	out := []string{"informational, NOT gating (the pre-scatter-floor rule of I-MEM-1, run beside the gated one on the same series; the verdict above is the gated rule's only):"}
	if len(rows) == 0 {
		return append(out, "  the pre-scatter-floor rule trips in no cell")
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].line < rows[j].line })
	absolved := 0
	for _, r := range rows {
		if r.absolved {
			absolved++
		}
	}
	out = append(out, fmt.Sprintf("  %d cell(s) where the pre-scatter-floor rule would have tripped, %d of them absolved by the scatter floor (the gate does not fail on these):", len(rows), absolved))
	for _, r := range rows {
		out = append(out, r.line)
	}
	return out
}
