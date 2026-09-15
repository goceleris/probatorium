//go:build mage

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/loadgen"
	"github.com/goceleris/probatorium/report"
)

// TestParseSUTEnv pins the BENCH_SUT_ENV format (celeris#585): every
// accepted shape and every rejection the dispatch must fail on before
// touching the cluster.
func TestParseSUTEnv(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, in string
		want     map[string]string
		errPart  string
	}{
		{name: "empty", in: "", want: map[string]string{}},
		{name: "blank", in: "   ", want: map[string]string{}},
		{name: "one", in: "CELERIS_IOURING_SEND_ZC=on", want: map[string]string{"CELERIS_IOURING_SEND_ZC": "on"}},
		{name: "off arm", in: "CELERIS_IOURING_SEND_ZC=off", want: map[string]string{"CELERIS_IOURING_SEND_ZC": "off"}},
		{name: "two trimmed", in: " A=1 , B_2=x.y/z:9@h+q-1 ", want: map[string]string{"A": "1", "B_2": "x.y/z:9@h+q-1"}},
		{name: "empty value", in: "A=", want: map[string]string{"A": ""}},
		{name: "equals in value", in: "A=b=c", want: map[string]string{"A": "b=c"}},
		{name: "no equals", in: "novalue", errPart: "not KEY=VALUE"},
		{name: "empty key", in: "=x", errPart: "key \"\""},
		{name: "digit-led key", in: "1A=x", errPart: "key \"1A\""},
		{name: "dash in key", in: "A-B=x", errPart: "key \"A-B\""},
		{name: "space in value", in: "A=x y", errPart: "value of A"},
		{name: "quote in value", in: "A='x'", errPart: "value of A"},
		{name: "dollar in value", in: "A=$HOME", errPart: "value of A"},
		{name: "duplicate key", in: "A=1,A=2", errPart: "given twice"},
		{name: "stray comma", in: "A=1,,B=2", errPart: "entry 2 is empty"},
		{name: "trailing comma", in: "A=1,", errPart: "entry 2 is empty"},
		{name: "PATH refused", in: "PATH=/x", errPart: "not allowed"},
		{name: "LD_PRELOAD refused", in: "LD_PRELOAD=/x.so", errPart: "not allowed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseSUTEnv(tc.in)
			if tc.errPart != "" {
				if err == nil {
					t.Fatalf("parseSUTEnv(%q) = %v, want error containing %q", tc.in, got, tc.errPart)
				}
				if !strings.Contains(err.Error(), tc.errPart) {
					t.Fatalf("parseSUTEnv(%q) error %q does not contain %q", tc.in, err, tc.errPart)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSUTEnv(%q): %v", tc.in, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("parseSUTEnv(%q) = %v, want %v", tc.in, got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("parseSUTEnv(%q)[%s] = %q, want %q", tc.in, k, got[k], v)
				}
			}
		})
	}
}

// TestSUTEnvExtraVars: the ansible payload is a JSON dict under
// bench_sut_env (so combine() gets a real mapping), and the log form is
// sorted KEY=VALUE.
func TestSUTEnvExtraVars(t *testing.T) {
	t.Parallel()
	env := map[string]string{"CELERIS_IOURING_SEND_ZC": "off", "A": "1"}
	raw := sutEnvExtraVars(env)
	if !strings.HasPrefix(raw, "{") {
		t.Fatalf("extra-vars must be JSON (ansible treats a leading '{' as JSON), got %q", raw)
	}
	var got struct {
		SUT map[string]string `json:"bench_sut_env"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("unmarshal %q: %v", raw, err)
	}
	if got.SUT["CELERIS_IOURING_SEND_ZC"] != "off" || got.SUT["A"] != "1" || len(got.SUT) != 2 {
		t.Errorf("bench_sut_env = %v", got.SUT)
	}
	if s := sutEnvString(env); s != "A=1 CELERIS_IOURING_SEND_ZC=off" {
		t.Errorf("sutEnvString = %q", s)
	}
}

// TestResolveBenchCellsPrecedence pins the BENCH_CELLS rule: a non-blank
// preset (the workflow's `cells` input) wins over the profile glob;
// blank/whitespace (the input's default) falls through to the profile.
func TestResolveBenchCellsPrecedence(t *testing.T) {
	t.Parallel()
	const profile = "*/*"
	if g, o := resolveBenchCells("", profile); g != profile || o {
		t.Errorf("empty preset: got (%q,%v) want (%q,false)", g, o, profile)
	}
	if g, o := resolveBenchCells("  ", profile); g != profile || o {
		t.Errorf("blank preset: got (%q,%v) want (%q,false)", g, o, profile)
	}
	const scoped = "ws-large-echo/*,ws-echo/*,get-json/*"
	if g, o := resolveBenchCells(scoped, profile); g != scoped || !o {
		t.Errorf("scoped preset: got (%q,%v) want (%q,true)", g, o, scoped)
	}
	if g, o := resolveBenchCells(" "+scoped+" ", profile); g != scoped || !o {
		t.Errorf("padded preset: got (%q,%v) want (%q,true)", g, o, scoped)
	}
}

// cpuLogISO is a column-wide `mpstat -P ALL 1 N` log as run_bench_cell.yml
// launches it (S_TIME_FORMAT=ISO TZ=UTC): 12 `all` rows 10:00:00..10:00:11.
const cpuLogISO = `Linux 7.0.0-30-generic (msa2-server) 	2026-09-13 	_x86_64_	(32 CPU)

10:00:00     CPU    %usr   %nice    %sys %iowait    %irq   %soft  %steal  %guest  %gnice   %idle
10:00:00     all   10.00    0.00    0.00    0.00    0.00    1.00    0.00    0.00    0.00   90.00
10:00:01     all   20.00    0.00    0.00    0.00    0.00    2.00    0.00    0.00    0.00   80.00
10:00:02     all   30.00    0.00    0.00    0.00    0.00    3.00    0.00    0.00    0.00   70.00
10:00:03     all   40.00    0.00    0.00    0.00    0.00    4.00    0.00    0.00    0.00   60.00
10:00:04     all   50.00    0.00    0.00    0.00    0.00    5.00    0.00    0.00    0.00   50.00
10:00:05     all   60.00    0.00    0.00    0.00    0.00    6.00    0.00    0.00    0.00   40.00
10:00:06     all   70.00    0.00    0.00    0.00    0.00    7.00    0.00    0.00    0.00   30.00
10:00:07     all   80.00    0.00    0.00    0.00    0.00    8.00    0.00    0.00    0.00   20.00
10:00:08     all   90.00    0.00    0.00    0.00    0.00    9.00    0.00    0.00    0.00   10.00
10:00:09     all   96.00    0.00    0.00    0.00    0.00   10.00    0.00    0.00    0.00    4.00
10:00:10     all   12.00    0.00    0.00    0.00    0.00   11.00    0.00    0.00    0.00   88.00
10:00:11     all   14.00    0.00    0.00    0.00    0.00   12.00    0.00    0.00    0.00   86.00
Average:     all   47.67    0.00    0.00    0.00    0.00    6.50    0.00    0.00    0.00   52.33
`

// writeRunnerCellTimed is writeRunnerCell plus the runner's per-scenario
// started_at / completed_at (cmd/runner cellResultFile).
func writeRunnerCellTimed(t *testing.T, cellDir, scenario, server string, started, completed time.Time, result *loadgen.Result) {
	t.Helper()
	dir := filepath.Join(cellDir, "run0", scenario)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	cell := map[string]any{
		"run_idx":      0,
		"scenario":     scenario,
		"server":       server,
		"status":       "ok",
		"started_at":   started,
		"completed_at": completed,
		"result":       result,
	}
	data, err := json.MarshalIndent(cell, "", "  ")
	if err != nil {
		t.Fatalf("marshal cell: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, server+".json"), data, 0o644); err != nil {
		t.Fatalf("write cell: %v", err)
	}
}

// TestAggregateWindowsScenarioResources drives the whole merge path on a
// synthetic column: ONE cpu.log shared by three scenarios. The column-wide
// Resources must stay the identical shared mean on every record (unchanged
// behaviour), each scenario's scenario_resources must be ITS OWN
// warm-up-trimmed window with the hand-computed mean, the scenario whose
// window lies outside the log must read no_data with no stats, and the
// merged Document must carry the per-scenario map plus the SUT env arm.
func TestAggregateWindowsScenarioResources(t *testing.T) {
	resultsDir := t.TempDir()
	benchDir := filepath.Join(resultsDir, "20260913T100000-bench-msa2-server")
	const comp = "celeris-iouring-h1-async"
	col := filepath.Join(benchDir, "00-"+comp)
	if err := os.MkdirAll(col, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(col, "cpu.log"), []byte(cpuLogISO), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(col, "results.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	ts := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	res := &loadgen.Result{Requests: 1000, Duration: 3 * time.Second, RequestsPerSec: 333,
		Latency: loadgen.Percentiles{P50: time.Millisecond, P99: 2 * time.Millisecond}}
	// ws-large-echo 10:00:00..10:00:05 → trimmed rows 02..05 → 45.
	writeRunnerCellTimed(t, col, "ws-large-echo", comp, ts("2026-09-13T10:00:00Z"), ts("2026-09-13T10:00:05Z"), res)
	// ws-echo 10:00:06..10:00:09 → rows 08..09 → 93.
	writeRunnerCellTimed(t, col, "ws-echo", comp, ts("2026-09-13T10:00:06Z"), ts("2026-09-13T10:00:09Z"), res)
	// get-json after the sampler stopped → no_data (NEGATIVE CONTROL).
	writeRunnerCellTimed(t, col, "get-json", comp, ts("2026-09-13T10:00:30Z"), ts("2026-09-13T10:00:40Z"), res)

	// A second column with NO sidecar at all: no_sidecar, nil everywhere.
	const bare = "gin-h1"
	bareCol := filepath.Join(benchDir, "01-"+bare)
	writeRunnerCellTimed(t, bareCol, "get-json", bare, ts("2026-09-13T10:00:00Z"), ts("2026-09-13T10:00:05Z"), res)

	if err := aggregatePerCellResults(resultsDir, 2*time.Second); err != nil {
		t.Fatalf("aggregatePerCellResults: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(resultsDir, "raw", "msa2-server.json"))
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Cells []cellRecord `json:"cells"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Cells) != 4 {
		t.Fatalf("cells: want 4 got %d", len(payload.Cells))
	}
	const colMean = 572.0 / 12
	want := map[string]struct {
		status string
		mean   float64
		soft   float64
		n      int
	}{
		"ws-large-echo": {report.WindowOK, 45, 4.5, 4},
		"ws-echo":       {report.WindowOK, 93, 9.5, 2},
		"get-json":      {report.WindowNoData, 0, 0, 0},
	}
	for _, c := range payload.Cells {
		if c.Competitor == bare {
			if c.Resources != nil || c.ScenarioResources != nil || c.ScenarioResourcesStatus != report.WindowNoSidecar {
				t.Errorf("%s: want no resources + status %q, got res=%v sr=%v status=%q", bare, report.WindowNoSidecar, c.Resources, c.ScenarioResources, c.ScenarioResourcesStatus)
			}
			continue
		}
		// Column-wide: the SAME shared mean on every scenario, untouched.
		if c.Resources == nil || c.Resources.Summary.MeanCPUPct == nil || absF(*c.Resources.Summary.MeanCPUPct-colMean) > 1e-9 {
			t.Errorf("%s: column-wide mean_cpu_pct=%v want %v", c.Scenario, c.Resources, colMean)
		}
		if c.Resources != nil && (c.Resources.Window != nil || c.Resources.Summary.MeanSoftPct != nil) {
			t.Errorf("%s: column-wide aggregate grew per-scenario fields: %+v", c.Scenario, c.Resources)
		}
		w, ok := want[c.Scenario]
		if !ok {
			t.Fatalf("unexpected scenario %q", c.Scenario)
		}
		if c.ScenarioResourcesStatus != w.status {
			t.Errorf("%s: status=%q want %q", c.Scenario, c.ScenarioResourcesStatus, w.status)
		}
		if w.status != report.WindowOK {
			if c.ScenarioResources != nil {
				t.Errorf("%s: no-data scenario must carry NO stats, got %+v", c.Scenario, c.ScenarioResources.Summary)
			}
			continue
		}
		sr := c.ScenarioResources
		if sr == nil || sr.Summary.MeanCPUPct == nil || absF(*sr.Summary.MeanCPUPct-w.mean) > 1e-9 {
			t.Errorf("%s: scenario mean_cpu_pct=%v want %v", c.Scenario, sr, w.mean)
			continue
		}
		if sr.Summary.MeanSoftPct == nil || absF(*sr.Summary.MeanSoftPct-w.soft) > 1e-9 {
			t.Errorf("%s: mean_soft_pct=%v want %v", c.Scenario, sr.Summary.MeanSoftPct, w.soft)
		}
		if sr.Window == nil || sr.Window.CPUSamples != w.n || sr.Window.ObserverSamples != 0 {
			t.Errorf("%s: window=%+v want cpu_samples=%d observer_samples=0", c.Scenario, sr.Window, w.n)
		}
		if sr.Summary.SUTProcessCPUPct != nil {
			t.Errorf("%s: sut_process_cpu_pct=%v want nil (no observer)", c.Scenario, *sr.Summary.SUTProcessCPUPct)
		}
	}

	// Merge: the typed Document carries scenario_resources per scenario and
	// the SUT env arm.
	out, err := mergeBenchResults(resultsDir, "msa2-server", benchParams{
		CelerisVer: "v1.6.0-test", Duration: "3s", Warmup: "2s", Conns: "64", Runs: "1",
		StartedAt: ts("2026-09-13T10:00:00Z"),
		SUTEnv:    map[string]string{"CELERIS_IOURING_SEND_ZC": "off"},
	})
	if err != nil {
		t.Fatalf("mergeBenchResults: %v", err)
	}
	docRaw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var doc report.Document
	if err := json.Unmarshal(docRaw, &doc); err != nil {
		t.Fatalf("parse document: %v", err)
	}
	if doc.SchemaVersion != "5.13" {
		t.Errorf("schema_version=%q want 5.13", doc.SchemaVersion)
	}
	if doc.Environment.SUTEnv["CELERIS_IOURING_SEND_ZC"] != "off" {
		t.Errorf("environment.sut_env=%v want the OFF arm", doc.Environment.SUTEnv)
	}
	var seen int
	for _, b := range doc.Benchmarks {
		if len(b.ScenarioResources) == 0 {
			continue
		}
		seen++
		if got := b.ScenarioResources["ws-large-echo"]; got == nil || got.Summary.MeanCPUPct == nil || absF(*got.Summary.MeanCPUPct-45) > 1e-9 {
			t.Errorf("document ws-large-echo scenario_resources=%v want mean 45", got)
		}
		if got := b.ScenarioResources["ws-echo"]; got == nil || got.Summary.MeanCPUPct == nil || absF(*got.Summary.MeanCPUPct-93) > 1e-9 {
			t.Errorf("document ws-echo scenario_resources=%v want mean 93", got)
		}
		if _, present := b.ScenarioResources["get-json"]; present {
			t.Errorf("document get-json must have NO scenario_resources entry (no data)")
		}
		// Column-wide map untouched: the identical shared mean on every scenario.
		for _, sc := range []string{"ws-large-echo", "ws-echo", "get-json"} {
			if r := b.Resources[sc]; r == nil || r.Summary.MeanCPUPct == nil || absF(*r.Summary.MeanCPUPct-colMean) > 1e-9 {
				t.Errorf("document %s resources=%v want the column-wide %v", sc, r, colMean)
			}
		}
	}
	if seen != 1 {
		t.Errorf("exactly one benchmark entry should carry scenario_resources, got %d", seen)
	}
	// The raw JSON must use the agreed field names.
	for _, name := range []string{`"scenario_resources"`, `"sut_env"`, `"mean_soft_pct"`, `"window"`, `"cpu_samples"`} {
		if !strings.Contains(string(docRaw), name) {
			t.Errorf("results.json lacks %s", name)
		}
	}
}

func absF(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
