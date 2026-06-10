// probatorium smoketest — pre-flight validator for the bench.
//
// Usage:
//
//	smoketest scan   -results-dir <path>          # build skip list from v3.7 cells
//	smoketest render -skip-file <path>           # emit cells-glob with !exclusions
//	smoketest verify -results-dir <path> -skip-file <path>   # CI check
//
// Scan walks the per-cell JSONs the bench produced (run0/<scenario>/<server>.json
// inside <results-dir>/00-<server>/) and classifies each cell as ok, dnf
// (runner couldn't talk to the SUT — typically the SUT segfaulted on first
// request), or not_applicable (capability gate false positive, server claims
// to support the scenario but emits 0 requests). The output is a JSON list
// of (server, scenario) pairs to skip.
//
// Render turns a skip list into a comma-separated glob compatible with the
// runner's -cells flag: the full grid plus !exclusion for every skip. The
// bench sets BENCH_CELLS to this rendered value so the runner simply does not
// schedule the broken cells.
//
// Verify is the CI guard: it asserts the current bench's per-cell results
// contain ZERO entries from the skip list (i.e. the bench never re-ran a
// known-broken cell). Exits 1 with a per-cell breakdown on mismatch.
//
// Why this exists: the v3.7 bench (celeris v1.4.15) hit 59 broken cells on
// the 7.0.0-22-generic kernel — 43 DNF where celeris-iouring-h1-async /
// -iouring-auto+upg-async SEGFAULT on first request, 15 not_applicable
// (celeris std/epoll SSE capability false positives, chi-h2 h2c upgrade
// false positive, actix-web 1MB body limit). The bench used to publish
// every cell regardless, so the docs site silently carried rows with zero
// data. This tool makes the broken set explicit, lets the bench skip them
// at schedule time, and trips CI when a previously-broken cell starts
// working (caller flips the skip list, bench eventually re-tries the cell
// and the verifier confirms success).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// cellResult mirrors the per-cell JSON the runner writes.
type cellResult struct {
	Status   string `json:"status"`
	Scenario string `json:"scenario"`
	Server   string `json:"server"`
	Error    string `json:"error,omitempty"`
}

// skipEntry is one (server, scenario) pair the bench must not run.
type skipEntry struct {
	Server   string `json:"server"`
	Scenario string `json:"scenario"`
	Status   string `json:"status"`
	Error    string `json:"error"`
	// FirstSeen is the results-dir filename this was first observed in
	// (a timestamp + celeris version, useful when comparing skip lists
	// across bench runs).
	FirstSeen string `json:"first_seen"`
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	switch cmd {
	case "scan":
		os.Exit(cmdScan(args))
	case "render":
		os.Exit(cmdRender(args))
	case "verify":
		os.Exit(cmdVerify(args))
	case "show":
		os.Exit(cmdShow(args))
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `probatorium smoketest — pre-flight validator for the bench

Usage:
  smoketest scan   -results-dir <path>                    # classify cells → skip list
  smoketest render -skip-file <path> [-include <glob>]    # cells glob with !exclusions
  smoketest verify -results-dir <path> -skip-file <path>  # assert no skip-list cell ran
  smoketest show   -skip-file <path>                      # print skip list as a table

The 'scan' output is a JSON list of (server, scenario) pairs that the bench
must not run, classified by the reason (dnf = SUT segfault or connection
refused; not_applicable = capability gate false positive, 0 requests).

The 'render' output is a comma-separated glob suitable for the runner's
-cells flag: every (server, scenario) pair is in the include set, the skip
list is appended as !exclusions. The bench sets BENCH_CELLS to this rendered
value so the runner simply does not schedule the broken cells.`)
}

// cmdScan walks the per-cell JSONs and writes the skip list to -out.
func cmdScan(args []string) int {
	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	resultsDir := fs.String("results-dir", "", "results dir from a bench run (e.g. results/20260609T171226-bench-msa2-server/)")
	out := fs.String("out", "", "output path for the skip list JSON (default stdout)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *resultsDir == "" {
		fmt.Fprintln(os.Stderr, "smoketest scan: -results-dir is required")
		return 2
	}

	skip, scanned, byStatus := scanResultsDir(*resultsDir)

	// Print a human summary to stderr so the operator sees what got
	// excluded and why without having to jq the JSON.
	fmt.Fprintf(os.Stderr, "smoketest scan: scanned %d cells in %s\n", scanned, *resultsDir)
	for status, n := range byStatus {
		fmt.Fprintf(os.Stderr, "  %-15s %d\n", status, n)
	}
	fmt.Fprintf(os.Stderr, "  total skip   %d\n", len(skip))

	// Marshal the skip list.
	data, err := json.MarshalIndent(skip, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "smoketest scan: marshal: %v\n", err)
		return 1
	}
	if *out != "" {
		if err := os.WriteFile(*out, data, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "smoketest scan: write %s: %v\n", *out, err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "smoketest scan: wrote %s\n", *out)
	} else {
		fmt.Println(string(data))
	}
	return 0
}

// scanResultsDir walks a results dir, classifies every per-cell JSON, and
// returns the skip list (failures only), the total scanned count, and a
// per-status histogram.
func scanResultsDir(resultsDir string) (skip []skipEntry, scanned int, byStatus map[string]int) {
	byStatus = map[string]int{}
	seen := map[string]bool{} // key = "server|scenario"
	// resultsDir is a host-scoped dir like
	//   20260609T171226-bench-msa2-server/00-<server>/run0/<scenario>/<server>.json
	// or a merged local results dir
	//   20260609T151223-bench-v1.4.15/raw/<host>.json
	// (in the merged form we have to dig through the host payloads).
	//
	// Walk both shapes.
	err := filepath.Walk(resultsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return nil
		}
		// Per-cell shape: <root>/00-<server>/run0/<scenario>/<server>.json
		if strings.HasSuffix(path, ".json") && !strings.Contains(path, "/raw/") && !strings.HasSuffix(path, "results.json") && !strings.HasSuffix(path, "manifest.json") {
			data, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			var cr cellResult
			if err := json.Unmarshal(data, &cr); err != nil {
				return nil
			}
			if cr.Server == "" || cr.Scenario == "" {
				return nil
			}
			scanned++
			byStatus[cr.Status]++
			if cr.Status == "dnf" || cr.Status == "not_applicable" {
				key := cr.Server + "|" + cr.Scenario
				if !seen[key] {
					seen[key] = true
					skip = append(skip, skipEntry{
						Server:    cr.Server,
						Scenario:  cr.Scenario,
						Status:    cr.Status,
						Error:     cr.Error,
						FirstSeen: filepath.Base(resultsDir),
					})
				}
			}
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "smoketest: walk %s: %v\n", resultsDir, err)
	}
	sort.Slice(skip, func(i, j int) bool {
		if skip[i].Server != skip[j].Server {
			return skip[i].Server < skip[j].Server
		}
		return skip[i].Scenario < skip[j].Scenario
	})
	return skip, scanned, byStatus
}

// cmdRender emits a cells-glob with the skip list appended as !exclusions.
func cmdRender(args []string) int {
	fs := flag.NewFlagSet("render", flag.ExitOnError)
	skipFile := fs.String("skip-file", "", "path to the skip list JSON (from `smoketest scan`)")
	include := fs.String("include", "*/*", "base include glob; the skip list is appended as !exclusions")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *skipFile == "" {
		fmt.Fprintln(os.Stderr, "smoketest render: -skip-file is required")
		return 2
	}
	data, err := os.ReadFile(*skipFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "smoketest render: read %s: %v\n", *skipFile, err)
		return 1
	}
	var skip []skipEntry
	if err := json.Unmarshal(data, &skip); err != nil {
		fmt.Fprintf(os.Stderr, "smoketest render: parse %s: %v\n", *skipFile, err)
		return 1
	}
	parts := []string{*include}
	for _, s := range skip {
		parts = append(parts, "!"+s.Scenario+"/"+s.Server)
	}
	// Sort the !exclusions for deterministic output (so two runs with the
	// same skip file produce the same BENCH_CELLS — handy for diffing).
	sort.Strings(parts[1:])
	fmt.Println(strings.Join(parts, ","))
	return 0
}

// cmdVerify asserts the current bench's results contain ZERO entries from
// the skip list — i.e. the bench never scheduled a known-broken cell.
// Exits 1 with a per-cell breakdown on mismatch.
func cmdVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	resultsDir := fs.String("results-dir", "", "results dir from a fresh bench run")
	skipFile := fs.String("skip-file", "", "path to the skip list JSON (from `smoketest scan`)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *resultsDir == "" || *skipFile == "" {
		fmt.Fprintln(os.Stderr, "smoketest verify: -results-dir and -skip-file are required")
		return 2
	}
	data, err := os.ReadFile(*skipFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "smoketest verify: read %s: %v\n", *skipFile, err)
		return 1
	}
	var skip []skipEntry
	if err := json.Unmarshal(data, &skip); err != nil {
		fmt.Fprintf(os.Stderr, "smoketest verify: parse %s: %v\n", *skipFile, err)
		return 1
	}
	want := map[string]bool{}
	for _, s := range skip {
		want[s.Server+"|"+s.Scenario] = true
	}
	_, scanned, _ := scanResultsDir(*resultsDir)
	mismatches := 0
	walkErr := filepath.Walk(*resultsDir, func(path string, info os.FileInfo, werr error) error {
		if werr != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".json") || strings.Contains(path, "/raw/") || strings.HasSuffix(path, "results.json") || strings.HasSuffix(path, "manifest.json") {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		var cr cellResult
		if uerr := json.Unmarshal(raw, &cr); uerr != nil {
			return nil
		}
		if cr.Server == "" || cr.Scenario == "" {
			return nil
		}
		key := cr.Server + "|" + cr.Scenario
		if want[key] {
			fmt.Fprintf(os.Stderr, "smoketest verify: FAIL: %s/%s ran but is in skip list (status=%q, error=%q)\n",
				cr.Server, cr.Scenario, cr.Status, cr.Error)
			mismatches++
		}
		return nil
	})
	if walkErr != nil {
		fmt.Fprintf(os.Stderr, "smoketest verify: walk %s: %v\n", *resultsDir, walkErr)
	}
	if mismatches > 0 {
		fmt.Fprintf(os.Stderr, "smoketest verify: %d cells from the skip list re-ran\n", mismatches)
		return 1
	}
	fmt.Printf("smoketest verify: OK (%d cells scanned, 0 from skip list)\n", scanned)
	return 0
}

// cmdShow prints the skip list as a table.
func cmdShow(args []string) int {
	fs := flag.NewFlagSet("show", flag.ExitOnError)
	skipFile := fs.String("skip-file", "", "path to the skip list JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *skipFile == "" {
		fmt.Fprintln(os.Stderr, "smoketest show: -skip-file is required")
		return 2
	}
	data, err := os.ReadFile(*skipFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "smoketest show: read %s: %v\n", *skipFile, err)
		return 1
	}
	var skip []skipEntry
	if err := json.Unmarshal(data, &skip); err != nil {
		fmt.Fprintf(os.Stderr, "smoketest show: parse %s: %v\n", *skipFile, err)
		return 1
	}
	fmt.Printf("%-15s %-30s %-30s %s\n", "STATUS", "SERVER", "SCENARIO", "ERROR")
	for _, s := range skip {
		fmt.Printf("%-15s %-30s %-30s %s\n", s.Status, s.Server, s.Scenario, s.Error)
	}
	return 0
}
