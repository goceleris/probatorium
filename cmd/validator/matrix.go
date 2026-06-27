package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/goceleris/probatorium/report"
	"github.com/goceleris/probatorium/validation"
)

// MatrixConfig captures the extra knobs that turn the validator into
// a matrix-mode driver. Embedded in the main Config when -matrix is
// set on the command line. Per #103 / #113.
//
// All fields tolerate empty values: an unset Refapps defaults to
// "auto-discover under validation/refapp/", an unset Engines defaults
// to the per-OS production set.
type MatrixConfig struct {
	// Enabled flips the validator from single-cell to matrix mode.
	Enabled bool

	// Refapps is a comma-separated list of refapp slugs. The slug
	// is the directory name under validation/refapp/. Empty (or
	// "all") auto-discovers every refapp directory that contains a
	// buildable main.go.
	Refapps string

	// Engines is a comma-separated list of celeris engine names
	// (iouring|epoll|std|adaptive). Empty (or "auto") expands to
	// the platform's production set: iouring+epoll+std on linux,
	// std on every other GOOS. Skips engines unsupported on the
	// current host so a darwin developer can still smoke the
	// matrix locally (only the std cell runs).
	Engines string

	// RefappRoot is the directory under which refapps live; the
	// matrix runner scans for <RefappRoot>/<slug>/main.go entries.
	// Default: "validation/refapp".
	RefappRoot string

	// BinDir is where pre-built refapp binaries are looked up
	// before falling back to `go build`. The cluster deploy flow
	// stages binaries here (matches mage_cluster.go's
	// staging dir layout). Default: empty (always build).
	BinDir string
}

// Bind registers the matrix-mode flags. Called from Config.Bind
// alongside the single-cell flags so a single -h enumerates both.
func (m *MatrixConfig) Bind(fs interface {
	BoolVar(*bool, string, bool, string)
	StringVar(*string, string, string, string)
}) {
	fs.BoolVar(&m.Enabled, "matrix", m.Enabled,
		"enable matrix mode (iterate refapp × engine; populate ValidationResults.Cells[])")
	fs.StringVar(&m.Refapps, "matrix-refapps", m.Refapps,
		"comma-separated refapp slugs; empty or 'all' auto-discovers under validation/refapp/")
	fs.StringVar(&m.Engines, "matrix-engines", m.Engines,
		"comma-separated engine names; empty or 'auto' expands to the OS production set")
	fs.StringVar(&m.RefappRoot, "matrix-refapp-root", m.RefappRoot,
		"directory under which refapps live; default validation/refapp")
	fs.StringVar(&m.BinDir, "matrix-bin-dir", m.BinDir,
		"pre-built refapp binary dir; falls back to `go build` per cell when empty")
}

// matrixCell is one entry in the matrix runner's iteration plan.
type matrixCell struct {
	Refapp string
	Engine string
}

// resolveMatrixPlan expands the matrix-mode flag set into a
// concrete iteration plan. Returns the ordered cell list (refapp
// outer loop, engine inner loop) so the validate-results.json
// Cells[] order is stable across runs.
//
// Errors when:
//   - Refapps == "all" and no refapps were discovered (means the
//     RefappRoot doesn't exist or contains no main.go entries).
//   - An explicit refapp slug doesn't match any directory.
//   - An explicit engine name isn't recognised.
func resolveMatrixPlan(m MatrixConfig) ([]matrixCell, error) {
	root := m.RefappRoot
	if root == "" {
		root = "validation/refapp"
	}

	want := strings.TrimSpace(m.Refapps)
	if want == "" {
		want = "all"
	}
	var refapps []string
	if want == "all" {
		// Try the source tree first (dev-Mac workflow). When that's
		// missing or empty — the cluster case, where mage Deploy
		// stages binaries only — fall back to scanning BinDir for
		// executables. Each <BinDir>/<slug> file represents a
		// pre-built refapp.
		discovered, err := discoverRefapps(root)
		if err != nil || len(discovered) == 0 {
			if m.BinDir != "" {
				binDiscovered, binErr := discoverRefappBins(m.BinDir)
				if binErr == nil && len(binDiscovered) > 0 {
					discovered = binDiscovered
					err = nil
				}
			}
		}
		if err != nil {
			return nil, err
		}
		if len(discovered) == 0 {
			return nil, fmt.Errorf("matrix: no refapps discovered under %s (and BinDir %q has no binaries)", root, m.BinDir)
		}
		refapps = discovered
	} else {
		for _, slug := range strings.Split(want, ",") {
			slug = strings.TrimSpace(slug)
			if slug == "" {
				continue
			}
			// Source-tree lookup first; cluster fallback picks the
			// slug up from BinDir if the source tree isn't staged.
			sourcePath := filepath.Join(root, slug, "main.go")
			if _, err := os.Stat(sourcePath); err == nil {
				refapps = append(refapps, slug)
				continue
			}
			if m.BinDir != "" {
				binPath := filepath.Join(m.BinDir, slug)
				if st, err := os.Stat(binPath); err == nil && !st.IsDir() {
					refapps = append(refapps, slug)
					continue
				}
			}
			return nil, fmt.Errorf("matrix: refapp %q: not found under %s or %s", slug, root, m.BinDir)
		}
	}
	sort.Strings(refapps)

	engineSpec := strings.TrimSpace(m.Engines)
	if engineSpec == "" {
		engineSpec = "auto"
	}
	engines := expandEngines(engineSpec)
	if len(engines) == 0 {
		return nil, fmt.Errorf("matrix: no engines resolved from %q", engineSpec)
	}

	plan := make([]matrixCell, 0, len(refapps)*len(engines))
	for _, r := range refapps {
		for _, e := range engines {
			plan = append(plan, matrixCell{Refapp: r, Engine: e})
		}
	}
	return plan, nil
}

// discoverRefapps returns the sorted slug list of every directory
// under root that contains a main.go entry.
func discoverRefapps(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("matrix: read %s: %w", root, err)
	}
	var slugs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		main := filepath.Join(root, e.Name(), "main.go")
		if _, err := os.Stat(main); err == nil {
			slugs = append(slugs, e.Name())
		}
	}
	sort.Strings(slugs)
	return slugs, nil
}

// discoverRefappBins returns the sorted slug list of every executable
// file directly under binDir. Used as the cluster-side fallback when
// the validation/refapp source tree isn't staged (mage Deploy copies
// the built binaries only — there's no main.go to discover from).
func discoverRefappBins(binDir string) ([]string, error) {
	entries, err := os.ReadDir(binDir)
	if err != nil {
		return nil, fmt.Errorf("matrix: read %s: %w", binDir, err)
	}
	var slugs []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		// Pre-built refapp binaries are named after their slug (see
		// mage_cluster.go's refapp staging — dest=refapps/<slug>).
		// Skip hidden / dot files; everything else is treated as a
		// candidate binary. Caller validates each one is executable
		// before launch via findRefappBin.
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		slugs = append(slugs, name)
	}
	sort.Strings(slugs)
	return slugs, nil
}

// expandEngines turns the -matrix-engines flag value into a slug
// list. "auto" expands to the OS production set; an explicit list
// is parsed as-is but filtered by OS support so a darwin developer
// can smoke `-matrix-engines=iouring,std` and only `std` runs.
func expandEngines(spec string) []string {
	switch strings.ToLower(spec) {
	case "auto":
		if runtime.GOOS == "linux" {
			return []string{"iouring", "epoll", "std"}
		}
		return []string{"std"}
	}
	all := strings.Split(spec, ",")
	out := make([]string, 0, len(all))
	for _, e := range all {
		e = strings.TrimSpace(strings.ToLower(e))
		switch e {
		case "":
			continue
		case "iouring", "epoll":
			if runtime.GOOS != "linux" {
				// Skip linux-only engines on other OSes.
				continue
			}
			out = append(out, e)
		case "std", "adaptive":
			out = append(out, e)
		default:
			// Unknown engine: keep it; the refapp will error out
			// on -engine=<unknown> and the cell fails fast.
			out = append(out, e)
		}
	}
	return out
}

// findRefappBin returns the path to a pre-built binary for slug in
// dir. Empty dir or missing binary returns "" — the matrix runner
// falls back to `go build` in that case.
//
// Naming convention mirrors the cluster deploy flow:
// <dir>/refapp-<slug>-<arch> for cross-compiled binaries,
// <dir>/<slug> for native builds.
func findRefappBin(dir, slug string) string {
	if dir == "" {
		return ""
	}
	candidates := []string{
		filepath.Join(dir, "refapp-"+slug+"-"+runtime.GOARCH),
		filepath.Join(dir, slug),
	}
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode().Perm()&0o100 != 0 {
			return p
		}
	}
	return ""
}

// buildRefappBin compiles the refapp at root/slug into a temp binary
// and returns its path. The caller is responsible for cleanup.
func buildRefappBin(ctx context.Context, root, slug string) (string, error) {
	src := filepath.Join(root, slug)
	out, err := os.CreateTemp("", "refapp-"+slug+"-*")
	if err != nil {
		return "", err
	}
	binPath := out.Name()
	_ = out.Close()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", binPath, ".")
	cmd.Dir = src
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		_ = os.Remove(binPath)
		return "", fmt.Errorf("matrix: build %s: %w", slug, err)
	}
	return binPath, nil
}

// runMatrix is the matrix-mode entry point. Iterates the resolved
// cell list, runs a fresh Orchestrator per cell with a per-cell
// time budget = total_duration / len(cells), and writes a single
// v5.1 validate-results.json with Cells[] populated.
//
// Per-cell port allocation passes "-bind 127.0.0.1:0" to the refapp,
// which binds an OS-assigned port and announces the real one on its
// "ready addr=" banner — so cells never collide, with no pre-allocation
// race, even when run in parallel by a future extension.
//
// Returns the aggregate exit error: non-nil if any cell failed
// hard. Per-cell snapshots are still recorded in Cells[] regardless
// of cell-level failures so the report shows what ran.
func runMatrix(ctx context.Context, cfg Config, matrix MatrixConfig) error {
	plan, err := resolveMatrixPlan(matrix)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "matrix: %d cells (refapp × engine):\n", len(plan))
	for _, c := range plan {
		fmt.Fprintf(os.Stderr, "  - %s / %s\n", c.Refapp, c.Engine)
	}

	if cfg.OutDir == "" {
		ts := time.Now().UTC().Format("20060102-150405")
		cfg.OutDir = filepath.Join("results", ts+"-validate-matrix-"+cfg.Arch)
	}
	if err := os.MkdirAll(cfg.OutDir, 0o755); err != nil {
		return fmt.Errorf("matrix: mkdir out: %w", err)
	}

	cellBudget := cfg.Duration / time.Duration(len(plan))
	if cellBudget <= 0 {
		cellBudget = time.Minute
	}
	fmt.Fprintf(os.Stderr, "matrix: per-cell budget = %s (total %s / %d cells)\n",
		cellBudget, cfg.Duration, len(plan))

	startedAt := time.Now().UTC()
	cells := make([]report.ValidationCellResult, 0, len(plan))
	var aggErr error
	for i, mc := range plan {
		fmt.Fprintf(os.Stderr, "\nmatrix [%d/%d]: refapp=%s engine=%s\n",
			i+1, len(plan), mc.Refapp, mc.Engine)
		cell, err := runMatrixCell(ctx, cfg, matrix, mc, cellBudget, i)
		if err != nil {
			fmt.Fprintf(os.Stderr, "matrix [%d/%d]: %v\n", i+1, len(plan), err)
			if aggErr == nil {
				aggErr = err
			}
		}
		cells = append(cells, cell)
		if ctx.Err() != nil {
			// Whole-run cancelled — emit what we have and bail.
			break
		}
	}

	doc := report.Document{
		SchemaVersion: report.SchemaVersion,
		HostArchPair:  cfg.Target + "-" + cfg.Arch,
		Validation: &report.ValidationResults{
			StartedAt:  startedAt,
			FinishedAt: time.Now().UTC(),
			Cells:      cells,
		},
	}
	if err := writeJSON(filepath.Join(cfg.OutDir, "validate-results.json"), doc); err != nil {
		fmt.Fprintf(os.Stderr, "matrix: write results: %v\n", err)
		if aggErr == nil {
			aggErr = err
		}
	}
	return aggErr
}

// runMatrixCell drives one orchestrator instance for the given
// (refapp, engine) pair. Returns the per-cell ValidationCellResult
// suitable for inclusion in ValidationResults.Cells[].
//
// Best-effort: a cell-level failure produces a zero-tally
// ValidationCellResult (rather than dropping the cell entirely) so
// downstream consumers can see which cell didn't run.
func runMatrixCell(parent context.Context, cfg Config, matrix MatrixConfig,
	mc matrixCell, budget time.Duration, idx int,
) (report.ValidationCellResult, error) {
	cell := report.ValidationCellResult{
		Refapp: mc.Refapp,
		Engine: mc.Engine,
		Arch:   cfg.Arch,
	}

	// Resolve / build the refapp binary.
	root := matrix.RefappRoot
	if root == "" {
		root = "validation/refapp"
	}
	binPath := findRefappBin(matrix.BinDir, mc.Refapp)
	tempBin := ""
	if binPath == "" {
		built, err := buildRefappBin(parent, root, mc.Refapp)
		if err != nil {
			return cell, err
		}
		binPath = built
		tempBin = built
	}
	if tempBin != "" {
		defer func() { _ = os.Remove(tempBin) }()
	}

	// Bind the refapp to an OS-assigned loopback port: pass ":0" and let
	// the kernel choose. The refapp creates the listener itself, announces
	// the real port on its "ready addr=" banner, and the orchestrator
	// parses that for all traffic + forensics. This replaces the old
	// pickEphemeralPort scheme (bind :0 → close → hand the freed port to
	// the refapp, which rebound it seconds later): that close-then-rebind
	// window collided with the cell's own load test churning the ephemeral
	// range to exhaustion, so the refapp failed to bind ("address already
	// in use") and died — surfacing as a spurious std-engine I-LIVENESS.
	addr := "127.0.0.1:0"

	cellOut := filepath.Join(cfg.OutDir, fmt.Sprintf("cell-%02d-%s-%s", idx, mc.Refapp, mc.Engine))
	if err := os.MkdirAll(cellOut, 0o755); err != nil {
		return cell, fmt.Errorf("mkdir cell out: %w", err)
	}

	// Pick the per-refapp Markov yaml. Look next to the global default
	// MarkovPath first (e.g. validation/markov/<slug>.yaml or
	// /tmp/celeris-bench/markov/<slug>.yaml), falling back to
	// cfg.MarkovPath verbatim if the slug-specific file is missing —
	// preserves the single-cell default when matrix mode runs with
	// just one refapp.
	markovPath := cfg.MarkovPath
	if cfg.MarkovPath != "" {
		candidate := filepath.Join(filepath.Dir(cfg.MarkovPath), mc.Refapp+".yaml")
		if _, err := os.Stat(candidate); err == nil {
			markovPath = candidate
		}
	}

	cellCfg := validation.Config{
		Target:             cfg.Target,
		Arch:               cfg.Arch,
		CelerisCommit:      cfg.CelerisCommit,
		Duration:           budget,
		CheckpointInterval: cfg.CheckpointInterval,
		SoakMode:           cfg.SoakMode,
		DryRun:             cfg.DryRun,
		OutDir:             cellOut,
		CorpusPath:         cfg.CorpusPath,
		MarkovPath:         markovPath,
		OpenAPIPath:        cfg.OpenAPIPath,
		CelerisBin:         binPath,
		CelerisListenAddr:  addr,
		MetricsURL:         "http://" + addr + "/debug/vars",
		PropertyTier:       cfg.PropertyTier,
		ReplayBin:          cfg.ReplayBin,
		RefappEngine:       mc.Engine,
		DriverMode:         cfg.DriverMode,
		DriverSSHUser:      cfg.DriverSSHUser,
		DriverSSHHost:      cfg.DriverSSHHost,
	}
	o, err := validation.New(cellCfg)
	if err != nil {
		return cell, fmt.Errorf("orchestrator: %w", err)
	}

	cellCtx, cellCancel := context.WithTimeout(parent, budget+30*time.Second)
	defer cellCancel()
	runErr := o.Run(cellCtx)

	res := o.Result()
	if res.Tier1Ran {
		cell.Tier1 = res.Tier1.Tier1Summary()
	}
	if res.Tier3Ran {
		cell.Tier3 = res.Tier3.Tier3Summary()
	}
	if runErr != nil && !errors.Is(runErr, context.DeadlineExceeded) {
		return cell, fmt.Errorf("cell run: %w", runErr)
	}
	return cell, nil
}

// writeJSON marshals v to path with indent=2 + trailing newline. A
// thin wrapper around encoding/json so the matrix runner doesn't
// need to import the validation package's private helper.
func writeJSON(path string, v any) error {
	buf, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	buf = append(buf, '\n')
	return os.WriteFile(path, buf, 0o644)
}
