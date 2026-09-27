//go:build mage

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/goceleris/probatorium/report"
)

// ValidateFaultControl judges the celeris#588 capture control
// (probatorium#351) on the most recent run under results/, in place of
// ValidateGate: a fault-injected run fails the absolute gate BY DESIGN, and
// what it must prove instead is that the capture caught the injected stall
// and names it from the artifact alone (report.CheckFaultControlCell).
//
//	VALIDATE_FAULT_CONTROL=<spec>           the PROBATORIUM_REFAPP_FAULT the run used
//	                                        ("/ws:8s@30s,/:40s@60s"); required
//	VALIDATE_FAULT_CONTROL_REFAPPS=...      refapps that implement the fault
//	                                        (default auth_session_ratelimit); every
//	                                        OTHER cell of the run is judged as an
//	                                        un-injected false-positive control.
//	                                        "none" judges EVERY cell as un-injected:
//	                                        the no-fault control, run on an
//	                                        artifact recorded without the fault
//	                                        (VALIDATE_FAULT_CONTROL may then be empty).
//	                                        To judge the stall capture itself, record
//	                                        it with PROBATORIUM_STALL_CAPTURE=1: a
//	                                        routine run takes no in-stall dossier and
//	                                        starts no side listener
//	                                        (validation.stallCaptureEnabled)
//	VALIDATE_FAULT_CONTROL_EXPECT_CELLS=N   injected cells the run must contain (0 = no check)
//	VALIDATE_FAULT_CONTROL_RESULTS=<dir>    judge this run dir (holding <host>/validate-results.json)
//	                                        instead of the newest under results/
func ValidateFaultControl() error {
	spec := strings.TrimSpace(os.Getenv("VALIDATE_FAULT_CONTROL"))
	refappList := strings.TrimSpace(envOrDefault("VALIDATE_FAULT_CONTROL_REFAPPS", "auth_session_ratelimit"))
	noFault := refappList == "none"
	if spec == "" && !noFault {
		return fmt.Errorf("ValidateFaultControl: VALIDATE_FAULT_CONTROL (the run's PROBATORIUM_REFAPP_FAULT) is required")
	}
	var paths []string
	var err error
	if !noFault {
		if paths, err = faultControlPaths(spec); err != nil {
			return err
		}
	}
	refapps := map[string]bool{}
	if !noFault {
		for _, r := range strings.Split(refappList, ",") {
			if r = strings.TrimSpace(r); r != "" {
				refapps[r] = true
			}
		}
	}
	var docs []string
	if dir := os.Getenv("VALIDATE_FAULT_CONTROL_RESULTS"); dir != "" {
		docs, _ = filepath.Glob(filepath.Join(dir, "*", "validate-results.json"))
		sort.Strings(docs)
		if len(docs) == 0 {
			return fmt.Errorf("ValidateFaultControl: no <host>/validate-results.json under %s", dir)
		}
	} else if docs, err = latestRunValidateResults(); err != nil {
		return err
	}
	fmt.Printf("ValidateFaultControl: spec %q on refapps %v; %d host document(s)\n", spec, sortedKeys(refapps), len(docs))
	injected, failed := 0, 0
	for _, p := range docs {
		doc, err := loadValidateDoc(p)
		if err != nil {
			return fmt.Errorf("load %s: %w", p, err)
		}
		hostDir := filepath.Dir(p)
		for _, cell := range doc.Validation.Cells {
			var want []string
			if refapps[cell.Refapp] {
				want = paths
				injected++
			}
			// Exactly one directory, or the cell's dossiers cannot be
			// attributed: a matrix that repeats a (refapp, engine) pair
			// leaves two, and taking the first would judge one cell's
			// artifacts for both (CodeRabbit on probatorium#412). The
			// document's slice index is no substitute: a resumed document
			// prepends inherited cells.
			m, _ := filepath.Glob(filepath.Join(hostDir, "cell-*-"+cell.Refapp+"-"+cell.Engine))
			if len(m) != 1 {
				failed++
				fmt.Printf("\n  FAIL  %s/%s/%s  want exactly one cell directory cell-*-%s-%s under %s, found %d: its artifacts cannot be attributed\n",
					cell.Refapp, cell.Engine, cell.Arch, cell.Refapp, cell.Engine, hostDir, len(m))
				continue
			}
			cellDir := m[0]
			r := report.CheckFaultControlCell(cellDir, cell, want)
			verdict := "PASS"
			if !r.Pass() {
				verdict = "FAIL"
				failed++
			}
			role := "un-injected control"
			if want != nil {
				role = "injected " + strings.Join(want, ",")
			}
			fmt.Printf("\n  %s  %s/%s/%s  (%s)  %s\n", verdict, cell.Refapp, cell.Engine, cell.Arch, role, filepath.Base(cellDir))
			for _, inj := range r.Injections {
				fmt.Printf("    fault: %s held %s from %s, %d request(s) blocked\n", inj.Path, inj.Hold, inj.Start.Format("15:04:05.000"), inj.Waiters)
			}
			for _, f := range r.Findings {
				fmt.Printf("    found: %s\n", f)
			}
			for _, f := range r.Failures {
				fmt.Printf("    FAIL:  %s\n", f)
			}
		}
	}
	if want := gateEnvInt("VALIDATE_FAULT_CONTROL_EXPECT_CELLS", 0); want > 0 && injected != want {
		return fmt.Errorf("ValidateFaultControl: %d injected cell(s), want %d", injected, want)
	}
	if failed > 0 {
		return fmt.Errorf("ValidateFaultControl: FAIL -- %d cell(s) failed the capture control", failed)
	}
	if noFault {
		fmt.Println("\nValidateFaultControl: PASS (no-fault control) -- no cell recorded an h2c hang, a WS handshake failure or a dossier for either.")
		return nil
	}
	if injected == 0 {
		return fmt.Errorf("ValidateFaultControl: no cell of %v in the run: nothing was injected, so nothing was proven", sortedKeys(refapps))
	}
	fmt.Printf("\nValidateFaultControl: PASS -- %d injected cell(s): every injected stall was counted, recorded and named from the artifact; every un-injected path stayed clean.\n", injected)
	return nil
}

// faultControlPaths is the set of paths a PROBATORIUM_REFAPP_FAULT spec
// holds, each of which the checker must know how to judge. The refapp
// validates the spec in full (debugvars.ParseFaults); this refuses a path no
// walker exercises and, like the refapp, holds that overlap: every hold has
// the same waiter and holder frames, so a dump taken in an overlap would be
// credited to both paths.
func faultControlPaths(spec string) ([]string, error) {
	known := map[string]bool{}
	for _, p := range report.FaultControlPaths() {
		known[p] = true
	}
	type window struct {
		entry     string
		from, end time.Duration
	}
	var out []string
	var windows []window
	for _, e := range strings.Split(spec, ",") {
		e = strings.TrimSpace(e)
		i := strings.LastIndex(e, ":")
		j := strings.LastIndex(e, "@")
		if i <= 0 || j < i {
			return nil, fmt.Errorf("VALIDATE_FAULT_CONTROL entry %q: want <path>:<hold>@<at>", e)
		}
		if !known[e[:i]] {
			return nil, fmt.Errorf("VALIDATE_FAULT_CONTROL entry %q: no walker judges path %s (judged: %v)", e, e[:i], report.FaultControlPaths())
		}
		hold, herr := time.ParseDuration(e[i+1 : j])
		at, aerr := time.ParseDuration(e[j+1:])
		if herr != nil || aerr != nil || hold <= 0 || at < 0 {
			return nil, fmt.Errorf("VALIDATE_FAULT_CONTROL entry %q: want <path>:<hold>@<at>, hold > 0, at >= 0", e)
		}
		out = append(out, e[:i])
		windows = append(windows, window{e, at, at + hold})
	}
	sort.Slice(windows, func(a, b int) bool { return windows[a].from < windows[b].from })
	for k := 1; k < len(windows); k++ {
		if windows[k].from < windows[k-1].end {
			return nil, fmt.Errorf("VALIDATE_FAULT_CONTROL entries %q and %q overlap: a dump taken in the overlap would be credited to both paths",
				windows[k-1].entry, windows[k].entry)
		}
	}
	return out, nil
}

func sortedKeys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
