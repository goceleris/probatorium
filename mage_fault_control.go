//go:build mage

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

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
//	                                        (VALIDATE_FAULT_CONTROL may then be empty)
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
			cellDir := ""
			if m, _ := filepath.Glob(filepath.Join(hostDir, "cell-*-"+cell.Refapp+"-"+cell.Engine)); len(m) > 0 {
				cellDir = m[0]
			}
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
// holds, each of which the checker must know how to judge. The spec's full
// validation is the refapp's (debugvars.ParseFaults); this only needs the
// paths and refuses one no walker exercises.
func faultControlPaths(spec string) ([]string, error) {
	known := map[string]bool{}
	for _, p := range report.FaultControlPaths() {
		known[p] = true
	}
	var out []string
	for _, e := range strings.Split(spec, ",") {
		e = strings.TrimSpace(e)
		i := strings.LastIndex(e, ":")
		if i <= 0 {
			return nil, fmt.Errorf("VALIDATE_FAULT_CONTROL entry %q: want <path>:<hold>@<at>", e)
		}
		if !known[e[:i]] {
			return nil, fmt.Errorf("VALIDATE_FAULT_CONTROL entry %q: no walker judges path %s (judged: %v)", e, e[:i], report.FaultControlPaths())
		}
		out = append(out, e[:i])
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
