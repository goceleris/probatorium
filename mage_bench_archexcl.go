//go:build mage

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/goceleris/probatorium/budget"
	"github.com/goceleris/probatorium/report"
	"github.com/goceleris/probatorium/scenarios"
)

// Arch exclusion for BENCH_TARGET=both (probatorium#442).
//
// Bench drives the two arches at the same time from msa2-client. The runner
// holds a host-wide lock around every heavy cell (cmd/runner/clientlock.go),
// and the arm64 pass walks each column's cells in reverse so the two arches'
// heavy cells fall in different halves of a column. This file is the mage side:
// the policy knob, the ansible extra-vars, the hang-guard allowance, and the
// check of a finished run.

// benchClientLockPath is the lock file on msa2-client both arch runners open.
// It sits outside bench_run_dir on purpose: that dir is per arch.
const benchClientLockPath = "/tmp/probatorium-client.lock"

// resolveArchExclusion reads BENCH_ARCH_EXCLUSIVE for a run of target.
// The default for BENCH_TARGET=both is heavy; a single-arch run has no peer, so
// the knob is ignored and it returns "" (no lock). Values: heavy, all (the strict option: the arches
// never load the client together, about twice the wall-clock), off (the
// pre-fix behaviour, overlap allowed; the publish gate refuses such a run).
func resolveArchExclusion(target string) (string, error) {
	v := strings.TrimSpace(os.Getenv("BENCH_ARCH_EXCLUSIVE"))
	if target != "both" {
		// A single-arch run has no peer to exclude: the knob is ignored, as the
		// workflow input documents.
		if v != "" {
			fmt.Printf("  NOTE: BENCH_ARCH_EXCLUSIVE=%q ignored: BENCH_TARGET=%s has no peer arch\n", v, target)
		}
		return "", nil
	}
	switch v {
	case "":
		return report.ExclusionHeavy, nil
	case report.ExclusionHeavy, report.ExclusionAll, report.ExclusionOff:
		return v, nil
	}
	return "", fmt.Errorf("BENCH_ARCH_EXCLUSIVE=%q: want heavy, all or off", v)
}

// archCellOrder is the order host's runner walks each column. Under a policy
// that locks (heavy, all) the arm64 pass walks it in reverse; every other pass
// is forward, and so is policy off (the pre-fix control). Under all the order
// does not change the wall-clock (the planner gives 99.7 h either way), but it
// keeps every cell's position in its column the same as under heavy, so the
// validation's run A against run B differs in what the arches shared and not in
// where each cell sat on a freshly started SUT (probatorium#442).
func archCellOrder(policy, host string) string {
	if (policy == report.ExclusionHeavy || policy == report.ExclusionAll) && host == "msr1" {
		return "reverse"
	}
	return "forward"
}

// archExclusionExtraVars are the ansible extra-vars that turn the lock and the
// cell order on for host's pass. None for a single-arch run or policy off, which
// keeps run_bench_cell.yml byte-for-byte what it was.
func archExclusionExtraVars(policy, host string) []string {
	if policy == "" || policy == report.ExclusionOff {
		return nil
	}
	vars := []string{
		"--extra-vars", "bench_client_lock=" + benchClientLockPath,
		"--extra-vars", "bench_client_lock_policy=" + policy,
	}
	if archCellOrder(policy, host) == "reverse" {
		vars = append(vars, "--extra-vars", "bench_cell_order=reverse")
	}
	return vars
}

// lockWaitAllowance is the time a column's hang guard must add for the waits the
// client lock can cause: in the worst case every cell that takes the lock waits
// out one cell of the other arch. A wedged runner is still killed; it only takes
// that much longer to be noticed. It is the allowance for ordinary queueing. A
// wedged peer does not make a healthy runner wait for it: the holder releases
// the lock itself after one cell's longest run and a waiter gives up a little
// later (cmd/runner clientLockLimits), so the peer costs at most that once, not
// the holder's guard.
func lockWaitAllowance(policy string, ratedPasses int, warmup, duration, ratedDuration time.Duration) time.Duration {
	var n int
	switch policy {
	case report.ExclusionAll:
		n = len(scenarios.Registry())
	case report.ExclusionHeavy:
		for _, s := range scenarios.Registry() {
			if scenarios.ClientLoadOf(s.Name()) == scenarios.LoadHeavy {
				n++
			}
		}
	default:
		return 0
	}
	return budget.ColumnWallClock(n, ratedPasses, warmup, duration, ratedDuration)
}

// loadWindowsFromRaw lifts one host's pass windows out of resultsDir/raw/<host>.json
// (the per-cell records aggregatePerCellResults wrote) and sums its lock wait.
// Cells that drove no load carry none.
func loadWindowsFromRaw(resultsDir, host string) (wins []report.LoadWindow, lockWaitS float64, err error) {
	data, err := os.ReadFile(filepath.Join(resultsDir, "raw", host+".json"))
	if err != nil {
		return nil, 0, err
	}
	var payload struct {
		Cells []cellRecord `json:"cells"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, 0, fmt.Errorf("parse raw/%s.json: %w", host, err)
	}
	for _, c := range payload.Cells {
		lockWaitS += c.ClientLockWaitS
		class := c.LoadClass
		if class == "" {
			class = string(scenarios.ClientLoadOf(c.Scenario))
		}
		for _, w := range c.PassWindows {
			wins = append(wins, report.LoadWindow{
				Competitor: c.Competitor, Scenario: c.Scenario, Kind: w.Kind,
				TargetRPS: w.TargetRPS, Class: class, Start: w.Start, End: w.End,
			})
		}
		// A cell that measured something drove load, so it must have a window.
		// One without (runner output from before 5.22, a column rebuilt from its
		// per-cell files, an ingest slip) is recorded as a window with no
		// timestamps: the check counts it as missing instead of reading the run
		// clean for want of anything to compare.
		if len(c.PassWindows) == 0 && cellHasData(c) {
			wins = append(wins, report.LoadWindow{
				Competitor: c.Competitor, Scenario: c.Scenario, Kind: report.LoadKindSaturation, Class: class,
			})
		}
	}
	sort.SliceStable(wins, func(i, j int) bool { return wins[i].Start.Before(wins[j].Start) })
	return wins, lockWaitS, nil
}

// archExclusionParams computes, for a finished BENCH_TARGET=both run in
// resultsDir, each host's load windows and the verdict to stamp into its
// document. The hosts are the two arch passes; policy is the one the run used.
func archExclusionParams(resultsDir, policy string, hosts [2]string) (map[string]benchParams, report.OverlapReport, error) {
	var wins [2][]report.LoadWindow
	var wait [2]float64
	for i, h := range hosts {
		w, ws, err := loadWindowsFromRaw(resultsDir, h)
		if err != nil {
			return nil, report.OverlapReport{}, err
		}
		wins[i], wait[i] = w, ws
	}
	out := map[string]benchParams{}
	for i, h := range hosts {
		peer := hosts[1-i]
		out[h] = benchParams{
			LoadWindows: wins[i],
			ArchExclusion: report.ArchExclusionFor(policy, benchTargetArch(peer),
				archCellOrder(policy, h), wins[i], wins[1-i], wait[i]),
		}
	}
	return out, report.CheckArchOverlap(wins[0], wins[1], policy), nil
}

// BenchArchOverlap checks a finished BENCH_TARGET=both run for overlap between
// the two arches' load windows and fails when any pair the policy forbids
// intersects, or any window has no usable timestamps (probatorium#442). It reads
// results-amd64.json and results-arm64.json from BENCH_RESULTS_DIR (default: the
// latest bench run). The policy is BENCH_ARCH_EXCLUSIVE when set, else the one
// recorded in the documents, else heavy. A document with no load_windows (written
// before schema 5.22) cannot be checked, which is an error, not a pass.
func BenchArchOverlap() error {
	dir := os.Getenv("BENCH_RESULTS_DIR")
	if dir == "" {
		d, err := latestBenchDir("")
		if err != nil {
			return err
		}
		dir = d
	}
	rep, err := checkArchOverlapDir(dir, strings.TrimSpace(os.Getenv("BENCH_ARCH_EXCLUSIVE")))
	fmt.Println(rep.String())
	return err
}

// checkArchOverlapDir is BenchArchOverlap's body: it loads the two arch
// documents of a both-arch run from dir and checks them.
func checkArchOverlapDir(dir, policy string) (report.OverlapReport, error) {
	var docs [2]report.Document
	for i, arch := range []string{"amd64", "arm64"} {
		data, err := os.ReadFile(filepath.Join(dir, "results-"+arch+".json"))
		if err != nil {
			return report.OverlapReport{}, err
		}
		if err := json.Unmarshal(data, &docs[i]); err != nil {
			return report.OverlapReport{}, fmt.Errorf("parse results-%s.json: %w", arch, err)
		}
	}
	if policy == "" {
		if ae := docs[0].Environment.ArchExclusion; ae != nil && ae.Policy != "" {
			policy = ae.Policy
		} else {
			policy = report.ExclusionHeavy
		}
	}
	if len(docs[0].LoadWindows) == 0 || len(docs[1].LoadWindows) == 0 {
		return report.OverlapReport{Policy: policy}, fmt.Errorf(
			"%s has no load_windows (a run before schema 5.22, or a single-arch run): its arch overlap cannot be checked, and a both-arch run of that age ran the arches concurrently (probatorium#442)", dir)
	}
	rep := report.CheckArchOverlap(docs[0].LoadWindows, docs[1].LoadWindows, policy)
	if !rep.Clean() {
		return rep, fmt.Errorf("arch overlap check FAILED for %s: %d forbidden overlap(s), %d+%d window(s) without usable timestamps",
			dir, rep.Total, rep.MissingA, rep.MissingB)
	}
	return rep, nil
}

// mergeBothArchs merges a finished BENCH_TARGET=both run into its two per-arch
// documents. Each arch is published alone and its publish gate sees only its
// own document, so the cross-arch verdict is computed here, from both arches'
// pass windows, and stamped into both with the windows themselves
// (probatorium#442). It prints the overlap report and returns the written paths.
func mergeBothArchs(resultsDir, target, policy string, bp benchParams, hosts [2]string) ([]string, error) {
	perArch, overlap, err := archExclusionParams(resultsDir, policy, hosts)
	if err != nil {
		return nil, fmt.Errorf("arch exclusion verdict: %w", err)
	}
	fmt.Printf("\n=== %s ===\n", overlap.String())
	if !overlap.Clean() {
		fmt.Printf("WARNING: the two arches' load windows overlap or could not be checked; the publish gate will refuse these documents (probatorium#442)\n")
	}
	var outs []string
	for _, h := range hosts {
		hp := bp
		hp.LoadWindows, hp.ArchExclusion = perArch[h].LoadWindows, perArch[h].ArchExclusion
		out, err := mergeBenchResultsFor(resultsDir, target, hp, h, "results-"+benchTargetArch(h)+".json")
		if err != nil {
			return nil, fmt.Errorf("merge results (%s): %w", h, err)
		}
		outs = append(outs, out)
	}
	return outs, nil
}

// cellHasData reports whether a raw cell carries a measurement (ok or suspect
// with a loadgen payload), i.e. whether it drove load.
func cellHasData(c cellRecord) bool {
	st := report.CellStatus(c.Status)
	if st == "" {
		st = report.ClassifyCellError(c.Error)
	}
	return st.HasData() && len(c.Loadgen) > 0 && string(c.Loadgen) != "null"
}
