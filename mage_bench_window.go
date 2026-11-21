//go:build mage

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/goceleris/probatorium/report"
	"github.com/goceleris/probatorium/servers"
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
// loader runs rather than how it behaves, and the harness's own endpoints;
// a passthrough into a root-launched SUT must not be able to set them.
// PROBATORIUM_DEBUG_ADDR is where the bench server serves /debug/vars
// (servers/celeris/debugvars.go): run_bench_cell.yml sets it from
// bench_debug_port and points the observer at the same port, so an override
// would move the listener away from the observer and leave the cell's
// engine counters silently absent (CodeRabbit on probatorium#411).
var sutEnvForbiddenKeys = map[string]bool{
	"PATH":                   true,
	"LD_PRELOAD":             true,
	"LD_LIBRARY_PATH":        true,
	"LD_AUDIT":               true,
	"HOME":                   true,
	"PROBATORIUM_DEBUG_ADDR": true,
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

// validateSUTEnvMap applies parseSUTEnv's per-entry rules to an env given
// as a map (a registry column's SUTEnv, celeris#585): the same key and
// value charsets and the same forbidden keys, because the map reaches the
// same root-launched process through the same playbook.
func validateSUTEnvMap(env map[string]string) error {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !sutEnvKeyRE.MatchString(k) {
			return fmt.Errorf("key %q must match %s", k, sutEnvKeyRE)
		}
		if sutEnvForbiddenKeys[k] {
			return fmt.Errorf("key %q is not allowed through the SUT env passthrough", k)
		}
		if v := env[k]; !sutEnvValueRE.MatchString(v) {
			return fmt.Errorf("value of %s (%q) must match %s (no whitespace, quotes or shell metacharacters)", k, v, sutEnvValueRE)
		}
	}
	return nil
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

// armColumnPrefixes are the registry prefixes of the v1.6.0 attribution columns: the arm columns of servers/c1_arms.go and servers/c2x86_arms.go, the #858 ladder columns of servers/c2lad_arms.go (the fcbpon slot is told apart by its column env), the ARMED diagnostic columns of servers/c1pp_arms.go (told apart by the pprof gate in their column env), and the repinned P2 columns of
// servers/p2_arms.go (1a, main vs v1.5.11), which have no validator yet and must not be turned into a different arm by an override either.
var armColumnPrefixes = []string{"c1arm-", "c2x86-", "c2lad-", "c1pp-", "pinp38-", "l755a-"}

// pprofGateEnv arms ansible/files/pprofcap.py (servers/celeris/pprof_sidecar.go); only a registry column env (the c1pp- columns) may set it, so a dispatch-wide override can never arm a
// column together with its twins and peers.
const pprofGateEnv = "PROBATORIUM_PPROF"

// busyPollKnobEnv is the busy-poll arm's knob (servers/celeris/busypoll_knob.go); only a registry column env may set it.
const busyPollKnobEnv = "PROBATORIUM_SOCKET_BUSY_POLL"

// refuseSUTEnvOnArmColumns fails the dispatch, at the start of mage Bench (before any column runs or any SUT starts, but AFTER the workflow's `mage Deploy` step), when a dispatch-wide BENCH_SUT_ENV (the workflow's sut_env input) is combined with an attribution
// column, or sets the busy-poll knob at all. The arm columns are told apart ONLY by their column env, and arm-validate.py / x86-validate.py require the launched environment to be
// exactly the base env plus the column's own keys: any override (CELERIS_IOURING_FIXED_FILES=1 would even silence SocketBusyPoll while the startup line still reads 50us) would void
// every column after the hours are spent, and a knob in sut_env would make the default arm busy-poll. slugs are the columns the dispatch will bench.
func refuseSUTEnvOnArmColumns(slugs []string, sutEnv map[string]string) error {
	if len(sutEnv) == 0 {
		return nil
	}
	if v, ok := sutEnv[busyPollKnobEnv]; ok {
		return fmt.Errorf("BENCH_SUT_ENV sets %s=%s: the busy-poll arm comes only from the registry column env (servers/c1_arms.go), never from a dispatch-wide override", busyPollKnobEnv, v)
	}
	if v, ok := sutEnv[pprofGateEnv]; ok {
		return fmt.Errorf("BENCH_SUT_ENV sets %s=%s: the pprof collector is armed only by the registry column env of the c1pp- columns (servers/c1pp_arms.go), never by a dispatch-wide override that would arm every column, twins and peers included", pprofGateEnv, v)
	}
	for _, slug := range slugs {
		for _, p := range armColumnPrefixes {
			if strings.HasPrefix(slug, p) {
				return fmt.Errorf("BENCH_SUT_ENV (%s) must be empty for the attribution columns (%s is one): the arms differ only by their column env and the validators require the launched environment to be exactly the expected keys",
					sutEnvString(sutEnv), slug)
			}
		}
	}
	return nil
}

// armedScenarios are the scenarios an armed pprof column (servers/c1pp_arms.go) may run, by mode: the #854 dump is for the redis Pipeline stall, the #857 profile for the memcached rows.
var armedScenarios = map[string][]string{
	"dump":    {"driver-redis-pipeline"},
	"profile": {"driver-mc-get", "driver-mc-multiget", "driver-mc-set"},
}

// refuseArmedColumnsOnManyScenarios fails the dispatch, at mage start, when any in-scope c1pp-<mode>-* column would run anything but exactly one scenario of its mode. ansible/files/pprofcap.py
// watches the first measurement window after its rendezvous and the runner runs a column's scenarios in name order, so an armed column that also matches driver-mc-get would dump (or profile) the
// memcached GET window instead of the Pipeline stall. The plan's own LF-1 selection (driver-redis-pipeline/* plus driver-mc-*/*) selects every c1pp column with four scenarios and is refused;
// the working shape is driver-redis-pipeline/celeris-*,driver-mc-*/celeris-*,driver-redis-pipeline/c1pp-dump-*. Empty or "*" cells select every scenario and are refused too.
func refuseArmedColumnsOnManyScenarios(slugs []string, cells string) error {
	for _, slug := range slugs {
		if !strings.HasPrefix(slug, "c1pp-") {
			continue
		}
		mode, _, _ := strings.Cut(strings.TrimPrefix(slug, "c1pp-"), "-")
		allowed, ok := armedScenarios[mode]
		if !ok {
			return fmt.Errorf("armed column %s has no known mode (%q): expected c1pp-dump-... or c1pp-profile-...", slug, mode)
		}
		got, err := cellsGlobScenarios(cells, slug)
		if err != nil {
			return err
		}
		if len(got) != 1 || !slices.Contains(allowed, got[0]) {
			return fmt.Errorf("armed column %s would run %d scenario(s) %v under cells %q: the pprof collector watches the first window of a column, so it must run exactly one of %v "+
				"(select it with cells=%s/c1pp-%s-*, and keep the unarmed columns on their own /celeris-* globs)", slug, len(got), got, cells, allowed, allowed[0], mode)
		}
	}
	return nil
}

// scopeSlugs is the set of column slugs a dispatch will bench, before the registry expansion: an explicit competitors CSV, else the slugs the cells glob matches, else (nil: "*"
// or empty cells) every registered column.
func scopeSlugs(competitors string, cellsSlugs []string) []string {
	if c := strings.TrimSpace(competitors); c != "" && c != "all" {
		return splitTrimNonEmpty(competitors)
	}
	if cellsSlugs != nil {
		return cellsSlugs
	}
	return servers.Names()
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

// nicIRQPlacementFile is the record ansible/tasks/nic_irq_placement.yml
// writes into the bench run directory on a host that applies the standing
// NIC interrupt placement (goceleris/celeris#1036).
const nicIRQPlacementFile = "nic-irq-placement.json"

// readNICIRQPlacement returns the NIC interrupt placement the bench ran
// under on host, or nil when the host applied none (no file: every amd64 run,
// msr1 with the placement variable emptied). The run directory is
// resultsDir/<TS>-bench-<host>; the timestamp is taken per host by the
// controller, so the directory is found by glob. Bench makes a fresh resultsDir per
// invocation, so a host has one run dir in it; more than one would fold cells that ran
// under different placements into one document, so that is an error. A file that exists but does
// not parse, or that records an unverified pin, is an error: the artifact must
// not silently claim a placement the run did not have.
func readNICIRQPlacement(resultsDir, host string) (*report.NICIRQPlacement, error) {
	// Glob fails only on a malformed pattern, which a host name cannot make; no match means no placement.
	matches, _ := filepath.Glob(filepath.Join(resultsDir, "*-bench-"+host, nicIRQPlacementFile))
	if len(matches) == 0 {
		return nil, nil
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf("%d NIC IRQ placement records for %s in %s (%s): cells of different runs cannot share one placement",
			len(matches), host, resultsDir, strings.Join(matches, ", "))
	}
	path := matches[0]
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var p report.NICIRQPlacement
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if !p.Verified || len(p.IRQs) == 0 {
		return nil, fmt.Errorf("%s records a NIC IRQ placement that was not verified (verified=%t, %d irqs)",
			path, p.Verified, len(p.IRQs))
	}
	return &p, nil
}
