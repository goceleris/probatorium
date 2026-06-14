package main

import (
	"bufio"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/probatorium/budget"
	"github.com/goceleris/probatorium/interleave"
	"github.com/goceleris/probatorium/scenarios"
	"github.com/goceleris/probatorium/servers"
)

func TestParseArgs_Defaults(t *testing.T) {
	cfg, err := ParseArgs(nil, io.Discard)
	if err != nil {
		t.Fatalf("ParseArgs: %v", err)
	}
	if cfg.Runs != 5 {
		t.Errorf("Runs = %d, want 5", cfg.Runs)
	}
	if cfg.Duration != 120*time.Second {
		t.Errorf("Duration = %v, want 120s", cfg.Duration)
	}
	if cfg.Services != "local" {
		t.Errorf("Services = %q, want local", cfg.Services)
	}
	if cfg.Target != "" {
		t.Errorf("Target = %q, want empty", cfg.Target)
	}
	if cfg.ServerName != "" {
		t.Errorf("ServerName = %q, want empty", cfg.ServerName)
	}
	if cfg.Timeseries != "" {
		t.Errorf("Timeseries = %q, want empty (derived from -out)", cfg.Timeseries)
	}
}

func TestParseArgs_OverrideAll(t *testing.T) {
	args := []string{
		"-runs", "3",
		"-duration", "60s",
		"-warmup", "10s",
		"-cooldown", "2s",
		"-cells", "get-simple/*",
		"-out", "/tmp/out",
		"-timeseries", "/tmp/ts.json.gz",
		"-services", "none",
		"-fail-fast",
		"-fd-trace",
		"-seed", "42",
		"-target", "http://10.0.0.2:8080",
		"-server-name", "celeris-iouring-h1-async",
		"-dry-run",
	}
	cfg, err := ParseArgs(args, io.Discard)
	if err != nil {
		t.Fatalf("ParseArgs: %v", err)
	}
	if cfg.Runs != 3 {
		t.Errorf("Runs = %d, want 3", cfg.Runs)
	}
	if cfg.Duration != 60*time.Second {
		t.Errorf("Duration = %v, want 60s", cfg.Duration)
	}
	if cfg.Cells != "get-simple/*" {
		t.Errorf("Cells = %q, want get-simple/*", cfg.Cells)
	}
	if cfg.Services != "none" {
		t.Errorf("Services = %q, want none", cfg.Services)
	}
	if cfg.Timeseries != "/tmp/ts.json.gz" {
		t.Errorf("Timeseries = %q, want /tmp/ts.json.gz", cfg.Timeseries)
	}
	if cfg.Target != "http://10.0.0.2:8080" {
		t.Errorf("Target = %q, want http://10.0.0.2:8080", cfg.Target)
	}
	if cfg.ServerName != "celeris-iouring-h1-async" {
		t.Errorf("ServerName = %q, want celeris-iouring-h1-async", cfg.ServerName)
	}
	if !cfg.DryRun {
		t.Errorf("DryRun = false, want true")
	}
}

func TestRun_DryRunNoCells(t *testing.T) {
	// DryRun with an impossible cells filter should be a no-op (no
	// adapters started, no error).
	cfg := Config{
		Runs:     1,
		Cells:    "nonexistent/*",
		Services: "none",
		DryRun:   true,
	}
	if err := run(cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
}

// scenarioByName looks up a registered scenario for buildCellConfig
// tests; fails the test if the catalogue no longer carries it so the
// assertions below never silently skip.
func scenarioByName(t *testing.T, name string) scenarios.Scenario {
	t.Helper()
	for _, s := range scenarios.Registry() {
		if s.Name() == name {
			return s
		}
	}
	t.Fatalf("scenario %q not in registry", name)
	return nil
}

func TestBuildCellConfig(t *testing.T) {
	const base = "http://10.0.0.2:8080"
	cfg := Config{Duration: 2 * time.Second, Warmup: time.Second}

	t.Run("get-json", func(t *testing.T) {
		cell := interleave.Cell{Scenario: scenarioByName(t, "get-json")}
		lg := buildCellConfig(cell, base, cfg)
		if lg.URL != base+"/json" {
			t.Errorf("URL = %q, want %s/json", lg.URL, base)
		}
		if lg.Method != "GET" {
			t.Errorf("Method = %q, want GET", lg.Method)
		}
		if lg.Connections != 128 {
			t.Errorf("Connections = %d, want 128", lg.Connections)
		}
		if lg.Duration != 2*time.Second {
			t.Errorf("Duration = %v, want 2s", lg.Duration)
		}
		if lg.Warmup != time.Second {
			t.Errorf("Warmup = %v, want 1s", lg.Warmup)
		}
		if lg.Workers != 128 {
			t.Errorf("Workers = %d, want 128 (mapped from declared Connections)", lg.Workers)
		}
	})

	// loadgen sizes every driver's concurrency from Workers (the
	// keep-alive H1 pool dials Workers conns; Mode drivers run one
	// stream per worker) and never reads Config.Connections, so the
	// runner maps each scenario's declared Connections onto Workers.
	// Before this mapping every H1 cell ran 64 conns regardless of its
	// label: get-json (128), get-json-1c (1) and get-simple-1024c (1024)
	// were all the same 64-conn workload.
	t.Run("declared connections map to workers", func(t *testing.T) {
		for _, tc := range []struct {
			scenario string
			want     int
		}{
			{"get-json-1c", 1},
			{"get-simple-1024c", 1024},
			{"get-simple-128c", 128},
			{"churn-close", 32},
			{"get-json-h2", 32},
		} {
			cell := interleave.Cell{Scenario: scenarioByName(t, tc.scenario)}
			lg := buildCellConfig(cell, base, cfg)
			if lg.Workers != tc.want {
				t.Errorf("%s: Workers = %d, want %d (declared Connections)",
					tc.scenario, lg.Workers, tc.want)
			}
		}
	})

	t.Run("post-4k", func(t *testing.T) {
		cell := interleave.Cell{Scenario: scenarioByName(t, "post-4k")}
		lg := buildCellConfig(cell, base, cfg)
		if lg.URL != base+"/upload" {
			t.Errorf("URL = %q, want %s/upload", lg.URL, base)
		}
		if lg.Method != "POST" {
			t.Errorf("Method = %q, want POST", lg.Method)
		}
		if len(lg.Body) != 4096 {
			t.Errorf("len(Body) = %d, want 4096", len(lg.Body))
		}
	})

	t.Run("get-json-h2", func(t *testing.T) {
		cell := interleave.Cell{Scenario: scenarioByName(t, "get-json-h2")}
		lg := buildCellConfig(cell, base, cfg)
		if !lg.HTTP2 {
			t.Errorf("HTTP2 = false, want true")
		}
		if lg.HTTP2Options.Connections != 32 {
			t.Errorf("HTTP2Options.Connections = %d, want 32", lg.HTTP2Options.Connections)
		}
	})

	// loadgen (≤ v1.4.7) Mode drivers run ONE stream per worker and
	// ignore Config.Connections, so streaming cells must map the
	// scenario's Connections onto Workers — otherwise sse-fanout-1024 /
	// ws-hub-broadcast-1024 only ever open 64 streams (v3.8: the -128
	// and -1024 variants recorded identical RPS in every archived run).
	t.Run("sse-fanout-1024 maps connections to workers", func(t *testing.T) {
		cell := interleave.Cell{Scenario: scenarioByName(t, "sse-fanout-1024")}
		lg := buildCellConfig(cell, base, cfg)
		if lg.Mode != "sse-fanout" {
			t.Fatalf("Mode = %q, want sse-fanout", lg.Mode)
		}
		if lg.Workers != 1024 {
			t.Errorf("Workers = %d, want 1024 (one stream per worker)", lg.Workers)
		}
	})
	t.Run("ws-hub-broadcast-1024 maps connections to workers", func(t *testing.T) {
		cell := interleave.Cell{Scenario: scenarioByName(t, "ws-hub-broadcast-1024")}
		lg := buildCellConfig(cell, base, cfg)
		if lg.Workers != 1024 {
			t.Errorf("Workers = %d, want 1024 (one stream per worker)", lg.Workers)
		}
	})
	t.Run("ws-echo maps connections to workers", func(t *testing.T) {
		cell := interleave.Cell{Scenario: scenarioByName(t, "ws-echo")}
		lg := buildCellConfig(cell, base, cfg)
		if lg.Workers != 128 {
			t.Errorf("Workers = %d, want 128", lg.Workers)
		}
	})
}

func TestRun_RemoteDryRun(t *testing.T) {
	// Remote dry-run must short-circuit before any StartAdapter and
	// resolve the schedule as (matched get-* scenarios) × 1 synthetic
	// server × runs. -target on an unreachable host is fine: DryRun
	// returns before any dial. We capture os.Stdout (where run() prints
	// one "runN scenario/server" line per cell) to count the schedule
	// and confirm every line carries the -server-name slug.
	cfg := Config{
		Target:     "http://127.0.0.1:1",
		ServerName: "celeris",
		Runs:       2,
		DryRun:     true,
		Cells:      "get-*/*",
		Services:   "none",
	}

	lines := captureStdout(t, func() {
		if err := run(cfg); err != nil {
			t.Fatalf("run: %v", err)
		}
	})

	// Expected: (get-* scenarios applicable to the permissive remote
	// FeatureSet) × 1 server × Runs. Every get-* scenario is applicable
	// under the permissive set, so the count is exactly that product.
	var getScenarios int
	for _, s := range scenarios.Registry() {
		if strings.HasPrefix(s.Name(), "get-") {
			getScenarios++
		}
	}
	if getScenarios == 0 {
		t.Fatal("no get-* scenarios in registry; test premise broken")
	}
	want := getScenarios * cfg.Runs

	var got int
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		got++
		// Each line is "runN <scenario>/<server>"; the server half must
		// be the -server-name slug, not an internal adapter name.
		if !strings.HasSuffix(ln, "/celeris") {
			t.Errorf("schedule line %q does not carry the -server-name slug", ln)
		}
	}
	if got != want {
		t.Errorf("schedule cells = %d, want %d (%d get-* scenarios × %d runs)",
			got, want, getScenarios, cfg.Runs)
	}
}

// captureStdout redirects os.Stdout for the duration of fn and returns
// the lines it wrote. Used to assert run()'s dry-run schedule without a
// production hook.
func captureStdout(t *testing.T, fn func()) []string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	done := make(chan []string, 1)
	go func() {
		var out []string
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			out = append(out, sc.Text())
		}
		done <- out
	}()

	fn()
	_ = w.Close()
	return <-done
}

// TestFeatureSetTLSGating locks the #160/#161 contract: an adapter that
// declares Capabilities.TLS only advertises fs.TLS when a shared terminator
// is wired (-tls-terminator). Without one, tls-* cells must NOT be scheduled
// (they would otherwise trip the executeCell capability-lie guard).
func TestFeatureSetTLSGating(t *testing.T) {
	tlsCapable := servers.Adapter{
		Name:         "celeris-tls",
		Engine:       "h1",
		Capabilities: servers.Capabilities{Static: true, TLS: true},
	}
	noTLS := servers.Adapter{
		Name:         "plain",
		Engine:       "h1",
		Capabilities: servers.Capabilities{Static: true},
	}

	if got := featureSetFor(tlsCapable, false).TLS; got {
		t.Fatalf("TLS-capable adapter must NOT advertise fs.TLS without a terminator; got true")
	}
	if got := featureSetFor(tlsCapable, true).TLS; !got {
		t.Fatalf("TLS-capable adapter must advertise fs.TLS when a terminator is wired; got false")
	}
	if got := featureSetFor(noTLS, true).TLS; got {
		t.Fatalf("non-TLS adapter must never advertise fs.TLS even with a terminator; got true")
	}
}

// TestDefaultRatedFractionsMatchBudgetModel pins the runner's default
// rated-sweep step count to budget.DefaultRatedPasses. The ansible
// per-column hang guard is sized from budget.ColumnWallClock using that
// constant (the bench playbook never passes -rated-fractions), so a
// drift here would silently under-budget every rated column — the exact
// v3.8 failure mode (guard SIGTERM at cell 28/33).
func TestDefaultRatedFractionsMatchBudgetModel(t *testing.T) {
	if got := len(defaultRatedFractions); got != budget.DefaultRatedPasses {
		t.Fatalf("len(defaultRatedFractions) = %d, want budget.DefaultRatedPasses = %d; "+
			"update both together (and re-check the ansible guard sizing)",
			got, budget.DefaultRatedPasses)
	}
}
