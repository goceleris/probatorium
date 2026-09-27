package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// A timing plan (mode timing, the cluster only) runs one observation per arm
// per block: one go test process of one package, the test binary run once
// through obs.sh, which measures the binary alone and writes one stress-obs
// line. The tally of each arm is the ordinary case report (every arm must
// PASS). What a timing adds is written here:
//
//   - observations.tsv, one row per observation in run order: the data a
//     pre-registered analysis reads. Nothing is dropped or reweighted; a
//     shard that did not complete is still a row, with its status.
//   - arms/<ARM>/, a single-case stress-summary per arm, so
//     `stresstally compare out/arms/A out/arms/B` is the ordinary
//     base-vs-branch comparison of the arms' failures.
//   - a DESCRIPTIVE section appended to summary.md: per-arm medians and the
//     per-block ratio against the first arm. It is not a verdict.

// obsColumns are observations.tsv's columns. The stress-obs keys are obs.sh's.
var obsColumns = []string{
	"arch", "host", "seq", "block", "arm", "position", "celeris_sha", "binary_sha256", "exit", "wall_ns", "user_s", "sys_s",
	"load1_before", "load1_after", "perf", "instructions_u", "cycles_u", "task_clock_ms", "cpuset", "shard_status",
}

// obsRow is one observation.
type obsRow struct {
	arch, arm   string
	block, seq  int
	status, sha string
	shard       ShardReport
	obs         map[string]string // nil when the shard carries no observation
}

// timingRows lists every observation of the plan, per arch in run order.
func timingRows(plan Plan, reports []CaseReport) []obsRow {
	var rows []obsRow
	for _, r := range reports {
		for _, s := range r.Shards {
			base := obsRow{arch: s.Arch, arm: r.Case, block: s.Shard, status: s.Status, sha: r.CelerisSHA, shard: s}
			if len(s.obs) == 0 {
				base.seq = 1 << 30
				rows = append(rows, base)
				continue
			}
			for _, o := range s.obs {
				row := base
				row.obs = o
				row.seq, _ = strconv.Atoi(o["seq"])
				rows = append(rows, row)
			}
		}
	}
	slices.SortStableFunc(rows, func(a, b obsRow) int {
		if c := strings.Compare(archOrder(a.arch), archOrder(b.arch)); c != 0 {
			return c
		}
		if a.seq != b.seq {
			return a.seq - b.seq
		}
		if a.block != b.block {
			return a.block - b.block
		}
		return strings.Compare(a.arm, b.arm)
	})
	return rows
}

// position is an arm's place within its block in the planned order (1-based).
func position(plan Plan, arm string, block int) int {
	rows := williams(len(plan.Cases))
	for i, idx := range rows[(block-1)%len(rows)] {
		if plan.Cases[idx].Name == arm {
			return i + 1
		}
	}
	return 0
}

// writeTiming writes observations.tsv, the per-arm reports and the
// descriptive section of summary.md.
func writeTiming(dir string, plan Plan, reports []CaseReport) error {
	rows := timingRows(plan, reports)
	var tsv strings.Builder
	tsv.WriteString(strings.Join(obsColumns, "\t") + "\n")
	for _, r := range rows {
		o := r.obs
		if o == nil {
			o = map[string]string{}
		}
		host := r.shard.Host
		vals := []string{r.arch, host, o["seq"], strconv.Itoa(r.block), r.arm, strconv.Itoa(position(plan, r.arm, r.block)), r.sha,
			o["binary_sha256"], o["exit"], o["wall_ns"], o["user_s"], o["sys_s"], o["load1_before"], o["load1_after"], o["perf"],
			o["instructions_u"], o["cycles_u"], o["task_clock_ms"], r.shard.CPUSet, r.status}
		for i, v := range vals {
			vals[i] = strings.NewReplacer("\t", " ", "\n", " ").Replace(v)
		}
		tsv.WriteString(strings.Join(vals, "\t") + "\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "observations.tsv"), []byte(tsv.String()), 0o644); err != nil {
		return err
	}

	for i, r := range reports {
		armDir := filepath.Join(dir, "arms", r.Case)
		if err := os.MkdirAll(armDir, 0o755); err != nil {
			return err
		}
		ref := ""
		if i < len(plan.Arms) {
			ref = plan.Arms[i].Ref
		}
		armPlan := Plan{Event: plan.Event, CelerisRef: ref, ProbatoriumSHA: plan.ProbatoriumSHA, Cases: []Case{plan.Cases[i]}}
		if err := writeReports(armDir, armPlan, r.CelerisSHA, []CaseReport{r}); err != nil {
			return err
		}
	}

	md, err := os.ReadFile(filepath.Join(dir, "summary.md"))
	if err != nil {
		return err
	}
	var b strings.Builder
	b.Write(md)
	b.WriteString(timingMarkdown(plan, reports, rows))
	return os.WriteFile(filepath.Join(dir, "summary.md"), []byte(b.String()), 0o644)
}

// timingMarkdown is the descriptive section: medians per arm and arch, the
// per-block ratio of each arm to the first, and what the witnesses say.
func timingMarkdown(plan Plan, reports []CaseReport, rows []obsRow) string {
	var b strings.Builder
	b.WriteString("## Timing (descriptive only)\n\n")
	b.WriteString("One observation is one run of the test binary, measured by obs.sh around the binary alone. These medians describe the run; " +
		"the verdict on an A/B question is the pre-registered analysis of `observations.tsv`, never this table.\n\n")
	order := plan.Sequence(0)
	var shown []string
	for i, tok := range order {
		if i == 16 {
			shown = append(shown, fmt.Sprintf("... (%d in all)", len(order)))
			break
		}
		name, rest, _ := strings.Cut(tok, ":")
		block, _, _ := strings.Cut(rest, ":")
		shown = append(shown, name+block)
	}
	say(&b, "Order on each host (arm and block): %s. Each observation pinned to %d CPU(s); pmu %s.\n\n",
		strings.Join(shown, " "), plan.Cases[0].CPUs, plan.Cases[0].PMU)
	b.WriteString("| arch | arm | commit | observations | median wall ms | median user+sys ms | median instructions:u | binaries (sha256) | per-block wall ratio to " +
		plan.Cases[0].Name + " (median, n) |\n|---|---|---|--:|--:|--:|--:|---|---|\n")
	for _, arch := range plan.Cases[0].Arches {
		first := map[int]float64{}
		for _, r := range rows {
			if r.arch == arch && r.arm == plan.Cases[0].Name && r.obs != nil {
				if w, ok := num(r.obs["wall_ns"]); ok {
					first[r.block] = w
				}
			}
		}
		for _, rep := range reports {
			var wall, cpu, ins, ratio []float64
			bins := map[string]bool{}
			n := 0
			for _, r := range rows {
				if r.arch != arch || r.arm != rep.Case || r.obs == nil {
					continue
				}
				n++
				if w, ok := num(r.obs["wall_ns"]); ok {
					wall = append(wall, w/1e6)
					if f, ok := first[r.block]; ok && f > 0 && rep.Case != plan.Cases[0].Name {
						ratio = append(ratio, w/f)
					}
				}
				u, ok1 := num(r.obs["user_s"])
				s, ok2 := num(r.obs["sys_s"])
				if ok1 && ok2 {
					cpu = append(cpu, (u+s)*1e3)
				}
				if x, ok := num(r.obs["instructions_u"]); ok {
					ins = append(ins, x)
				}
				if h := r.obs["binary_sha256"]; h != "" {
					bins[h] = true
				}
			}
			rat := "-"
			if len(ratio) > 0 {
				rat = fmt.Sprintf("%.4f (%d)", median(ratio), len(ratio))
			}
			say(&b, "| %s | %s | `%s` | %d | %s | %s | %s | %d | %s |\n", arch, rep.Case, short(rep.CelerisSHA), n,
				medianText(wall, "%.2f"), medianText(cpu, "%.2f"), medianText(ins, "%.0f"), len(bins), rat)
		}
	}
	b.WriteString("\nA binaries count above 1 means an arm did not run the same bytes in every observation; " +
		"two arms of one commit that each show 1 binary with the same sha256 (see observations.tsv) ran identical code.\n")
	return b.String()
}

func num(s string) (float64, bool) {
	if s == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	return f, err == nil
}

func median(x []float64) float64 {
	y := slices.Clone(x)
	slices.Sort(y)
	n := len(y)
	if n%2 == 1 {
		return y[n/2]
	}
	return (y[n/2-1] + y[n/2]) / 2
}

func medianText(x []float64, format string) string {
	if len(x) == 0 {
		return "-"
	}
	return fmt.Sprintf(format, median(x))
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
