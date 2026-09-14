//go:build mage

package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/goceleris/probatorium/report"
)

// mage_bench_window.go — the two harness prerequisites for the
// celeris#585 SEND_ZC A/B, both pure enough to unit-test under the mage
// build tag:
//
//   - cellRawResources: the per-cell sidecar (observer.sqlite + cpu.log)
//     read ONCE and served both as the column-wide aggregate (unchanged
//     behaviour) and as per-scenario window slices (schema v5.9).
//   - parseSUTEnv / sutEnvExtraVars: the BENCH_SUT_ENV passthrough that
//     lands KEY=VALUE overrides in the SUT process environment via an
//     ansible JSON extra-var, validated to a shell-safe charset.

// cellRawResources is one column's raw resource sampling: the observer
// rows and the parsed mpstat series, kept so the same data can be
// summarised column-wide and sliced per scenario without re-reading.
type cellRawResources struct {
	samples   []report.ObserverSample
	cpuMean   float64
	cpuOK     bool
	cpuSeries []report.CPUPoint
	// present is false when neither input exists (no sampler ran).
	present bool
}

// readCellRawResources parses the per-cell observer.sqlite + cpu.log.
// Best-effort: any missing/unreadable input yields the partial set it can
// build (present=false when neither exists), so a cluster cell that ran
// without the sampler never fails the merge.
func readCellRawResources(cellDir string) cellRawResources {
	samples, dbErr := report.ParseObserverDB(filepath.Join(cellDir, "observer.sqlite"))
	cpuMean, cpuSeries, cpuOK, _ := report.ParseMPStat(filepath.Join(cellDir, "cpu.log"))
	return cellRawResources{
		samples:   samples,
		cpuMean:   cpuMean,
		cpuOK:     cpuOK,
		cpuSeries: cpuSeries,
		present:   dbErr == nil || cpuOK,
	}
}

// columnWide is the pre-v5.9 aggregate over the whole sidecar: the number
// stamped onto every scenario of the column (cellRecord.Resources). Nil
// when no sampler ran.
func (r cellRawResources) columnWide() *report.ResourceStats {
	if !r.present {
		return nil
	}
	stats := report.SummarizeResources(r.samples, r.cpuMean, r.cpuOK, r.cpuSeries)
	return &stats
}

// scenario slices the sidecar to one scenario's window: the runner's
// started_at plus the warm-up through completed_at. Returns the slice and
// a report.Window* status; the slice is nil for every status but "ok".
func (r cellRawResources) scenario(startedAt, completedAt time.Time, warmup time.Duration) (*report.ResourceStats, string) {
	if !r.present {
		return nil, report.WindowNoSidecar
	}
	start, end := report.ScenarioWindow(startedAt, completedAt, warmup)
	return report.WindowResources(r.samples, r.cpuSeries, start, end)
}

// formatStatusCounts renders a status→count map as "ok=27 no_data=1"
// with the keys sorted, for the merge log.
func formatStatusCounts(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[k]))
	}
	return strings.Join(parts, " ")
}

// sutEnvKeyRE / sutEnvValueRE bound BENCH_SUT_ENV to what can travel
// safely through `ansible --extra-vars` JSON, the task-level environment
// mapping and the single-quoted `echo '[sut-env] ...'` that records the
// merged env in server.log: no whitespace, quotes, commas (the entry
// separator), `$`, backticks or shell metacharacters. '=' is allowed in a
// value (split is on the FIRST '='), '/' ':' '@' cover URLs and paths.
var (
	sutEnvKeyRE   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	sutEnvValueRE = regexp.MustCompile(`^[A-Za-z0-9_./:@+=-]*$`)
)

// sutEnvForbiddenKeys are the variables that change WHICH binary or
// loader runs rather than how it behaves; a passthrough into a
// root-launched SUT must not be able to set them.
var sutEnvForbiddenKeys = map[string]bool{
	"PATH":            true,
	"LD_PRELOAD":      true,
	"LD_LIBRARY_PATH": true,
	"LD_AUDIT":        true,
	"HOME":            true,
}

// parseSUTEnv parses the BENCH_SUT_ENV format "KEY=VALUE[,KEY=VALUE]".
// Entries are trimmed; an empty input yields an empty map (no
// overrides). Every entry must have a '=', a key matching sutEnvKeyRE, a
// value matching sutEnvValueRE, no key may repeat, and the forbidden keys
// are refused — each with a specific error so a bad dispatch fails at
// mage start, not on the cluster.
func parseSUTEnv(s string) (map[string]string, error) {
	out := map[string]string{}
	if strings.TrimSpace(s) == "" {
		return out, nil
	}
	for i, raw := range strings.Split(s, ",") {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			return nil, fmt.Errorf("entry %d is empty (stray comma in %q)", i+1, s)
		}
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, fmt.Errorf("entry %q is not KEY=VALUE", entry)
		}
		if !sutEnvKeyRE.MatchString(key) {
			return nil, fmt.Errorf("key %q must match %s", key, sutEnvKeyRE)
		}
		if sutEnvForbiddenKeys[key] {
			return nil, fmt.Errorf("key %q is not allowed through the SUT env passthrough", key)
		}
		if !sutEnvValueRE.MatchString(value) {
			return nil, fmt.Errorf("value of %s (%q) must match %s (no whitespace, quotes or shell metacharacters)", key, value, sutEnvValueRE)
		}
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("key %q given twice", key)
		}
		out[key] = value
	}
	return out, nil
}

// sutEnvExtraVars renders the override set as the JSON `--extra-vars`
// payload ({"bench_sut_env": {KEY: VALUE}}) so the playbook receives a
// real dict for `combine()`.
func sutEnvExtraVars(env map[string]string) string {
	b, err := json.Marshal(map[string]any{"bench_sut_env": env})
	if err != nil {
		// A map[string]string cannot fail to marshal; keep the signature
		// simple and make any surprise loud.
		panic(fmt.Sprintf("marshal bench_sut_env: %v", err))
	}
	return string(b)
}

// sutEnvString renders the override set as sorted "KEY=VALUE KEY=VALUE"
// for logs.
func sutEnvString(env map[string]string) string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+env[k])
	}
	return strings.Join(parts, " ")
}
