// Package validation glues the validation-tier sub-packages
// (properties, markov, corpus, fault, spec, refapp) into a
// three-tier orchestrator. Tier 1 runs always-on property stress
// driven by Markov-shaped traffic; Tier 2 runs RESTler-style
// stateful fuzzing over the OpenAPI spec; Tier 3 walks the seed
// corpus on a fixed cadence (200 seeds/h) replaying the deterministic
// (workload, fault_schedule) pair behind each seed.
//
// Bug = (seed, commit, arch). The orchestrator captures forensics
// on the first invariant violation, shrinks the offending seed, and
// writes a minimal repro under results/<run-ts>/incidents/.
package validation

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/goceleris/probatorium/validation/corpus"
	"github.com/goceleris/probatorium/validation/fault"
	"github.com/goceleris/probatorium/validation/markov"
	"github.com/goceleris/probatorium/validation/properties"
)

// Tier identifies which of the three concurrent property-stress
// pipelines is active.
type Tier int

const (
	// TierProperty is the always-on Tier 1 — Markov traffic, adversarial
	// HTTP/1.1, h2c upgrade churn, WS frame torture, SSE long-poll.
	TierProperty Tier = iota
	// TierRESTler is Tier 2 — RESTler-style stateful fuzzing over the
	// OpenAPI spec, 8h windows.
	TierRESTler
	// TierReplay is Tier 3 — corpus replay, 200 seeds/h on a fresh
	// celeris per seed.
	TierReplay
)

// String returns the tier's human-readable name.
func (t Tier) String() string {
	switch t {
	case TierProperty:
		return "tier-1-property"
	case TierRESTler:
		return "tier-2-restler"
	case TierReplay:
		return "tier-3-replay"
	}
	return "unknown"
}

// Config parameterises [Orchestrator]. Built from cmd/validator's
// parsed flags.
type Config struct {
	// Target is the inventory host the validator drives celeris on
	// ("msa2-server" in production; "localhost" / "" in dev).
	Target string
	// Arch is the architecture stamp embedded in incident reports.
	// Mirrors the failure-identity tuple (seed, commit, arch).
	Arch string
	// CelerisCommit is the git SHA of celeris under test. Recorded in
	// incident reports so a triage attempt can `git checkout` it.
	CelerisCommit string
	// Duration is the total run budget. Tier 3 reseeds itself from the
	// corpus repeatedly inside this window; tiers 1 and 2 run for the
	// full duration.
	Duration time.Duration
	// CheckpointInterval is how often the orchestrator persists a
	// progress checkpoint (cells run, seeds replayed, snapshot tail).
	// Zero = 24h.
	CheckpointInterval time.Duration
	// SoakMode flips on the extended invariant set and bumps the
	// checkpoint cadence to 1h. The CLI '-soak-mode' flag drives this.
	SoakMode bool
	// DryRun, when true, enumerates the cell schedule, property
	// predicates, and faults that would run, then exits 0. Required
	// for the acceptance test.
	DryRun bool
	// OutDir is the results directory; the orchestrator writes the
	// per-tier subdirectories under it.
	OutDir string
	// CorpusPath is the path to the gob+lz4 seed corpus. Empty falls
	// back to [corpus.InitialSeeds] in memory.
	CorpusPath string
	// MarkovPath is the path to the Markov transition YAML.
	MarkovPath string
	// OpenAPIPath is the path to the OpenAPI 3.1 spec for the refapp.
	OpenAPIPath string
	// CelerisBin is the path to the validation-build celeris executable
	// (the refapp binary). Empty means "skip process management" —
	// useful for the unit tests, where the validator does not actually
	// launch a server.
	CelerisBin string
	// CelerisListenAddr is the addr to bind celeris to inside the
	// orchestrator's lifecycle. Default "127.0.0.1:8080".
	CelerisListenAddr string
	// MetricsURL is the celeris /debug/vars (wave 6) or
	// /metrics (wave 7) endpoint validator-checker polls. Falls back
	// to "http://" + CelerisListenAddr + "/debug/vars".
	MetricsURL string
	// PropertyTier filters which property predicates fire. Empty =
	// every registered predicate. Wave 6 default = "core" + "middleware"
	// (the engine + driver tiers wait for wave 7's instrumentation).
	PropertyTier string
}

// Default returns Config defaults; CLI flag binders use these as the
// initial values.
func Default() Config {
	return Config{
		Duration:           6 * time.Hour,
		CheckpointInterval: 24 * time.Hour,
		PropertyTier:       "",
		Arch:               runtimeArch(),
		Target:             "localhost",
		CelerisListenAddr:  "127.0.0.1:8080",
	}
}

// runtimeArch returns the arch stamp recorded in incident reports.
// The package itself avoids importing runtime so test output stays
// deterministic; cmd/validator passes the real runtime.GOARCH through
// [Config.Arch]. GOARCH env overrides for cross-host replay drills.
func runtimeArch() string {
	if v := os.Getenv("GOARCH"); v != "" {
		return v
	}
	return ""
}

// Orchestrator is the three-tier driver. Construct via [New], run via
// [Orchestrator.Run].
type Orchestrator struct {
	cfg Config

	// Plan is the lazy-resolved dry-run plan. Populated by [Plan] and
	// by [Run] on entry.
	plan *Plan

	// Pre-loaded artifacts. nil values are loaded on demand.
	matrix    *markov.Matrix
	seeds     []corpus.Seed
	predicates []properties.Spec
}

// Plan is the deterministic schedule [Orchestrator.Run] would execute.
// Exposed so the dry-run mode can print it without actually running.
type Plan struct {
	Tiers              []TierPlan
	Properties         []properties.Spec
	CorpusSize         int
	MatrixStates       []string
	OpenAPIPath        string
	CelerisListenAddr  string
	Target             string
	Arch               string
	CelerisCommit      string
	Duration           time.Duration
	CheckpointInterval time.Duration
	SoakMode           bool
}

// TierPlan is the planned activity for one tier.
type TierPlan struct {
	Tier        Tier
	Description string
	BudgetUnits string
	Cadence     string
}

// New constructs an orchestrator and pre-loads artifacts. Returns an
// error if any required artifact (markov YAML, corpus, OpenAPI) is
// missing.
func New(cfg Config) (*Orchestrator, error) {
	o := &Orchestrator{cfg: cfg}

	if cfg.MarkovPath != "" {
		m, err := markov.LoadMatrixFile(cfg.MarkovPath)
		if err != nil {
			return nil, fmt.Errorf("validation: load markov %s: %w", cfg.MarkovPath, err)
		}
		o.matrix = m
	}
	if cfg.CorpusPath != "" {
		_, seeds, err := corpus.ReadFile(cfg.CorpusPath)
		if err != nil && err != corpus.ErrTruncated {
			return nil, fmt.Errorf("validation: read corpus %s: %w", cfg.CorpusPath, err)
		}
		o.seeds = seeds
	} else {
		o.seeds = append([]corpus.Seed(nil), corpus.InitialSeeds...)
	}

	if cfg.PropertyTier == "" {
		o.predicates = properties.All()
	} else {
		// Multi-tier filter: comma-separated.
		tiers := splitComma(cfg.PropertyTier)
		seen := map[string]bool{}
		for _, t := range tiers {
			for _, p := range properties.ByTier(t) {
				if !seen[p.ID] {
					o.predicates = append(o.predicates, p)
					seen[p.ID] = true
				}
			}
		}
		sort.Slice(o.predicates, func(i, j int) bool { return o.predicates[i].ID < o.predicates[j].ID })
	}
	return o, nil
}

// splitComma splits a comma-separated list, trimming spaces and
// dropping empty entries.
func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			tok := s[start:i]
			for len(tok) > 0 && tok[0] == ' ' {
				tok = tok[1:]
			}
			for len(tok) > 0 && tok[len(tok)-1] == ' ' {
				tok = tok[:len(tok)-1]
			}
			if tok != "" {
				out = append(out, tok)
			}
			start = i + 1
		}
	}
	return out
}

// Plan returns the dry-run plan without executing anything.
func (o *Orchestrator) Plan() *Plan {
	if o.plan != nil {
		return o.plan
	}
	p := &Plan{
		Properties:         o.predicates,
		CorpusSize:         len(o.seeds),
		OpenAPIPath:        o.cfg.OpenAPIPath,
		CelerisListenAddr:  o.cfg.CelerisListenAddr,
		Target:             o.cfg.Target,
		Arch:               o.cfg.Arch,
		CelerisCommit:      o.cfg.CelerisCommit,
		Duration:           o.cfg.Duration,
		CheckpointInterval: o.cfg.CheckpointInterval,
		SoakMode:           o.cfg.SoakMode,
	}
	if o.matrix != nil {
		p.MatrixStates = append([]string{}, o.matrix.States...)
	}
	p.Tiers = []TierPlan{
		{
			Tier:        TierProperty,
			Description: "always-on Markov-driven session traffic with adversarial HTTP/1.1, h2c upgrade, WS torture, SSE long-poll",
			BudgetUnits: fmt.Sprintf("for %s", o.cfg.Duration),
			Cadence:     "continuous",
		},
		{
			Tier:        TierRESTler,
			Description: "RESTler-style stateful fuzzing over the OpenAPI 3.1 spec with dependency-inference value mutation",
			BudgetUnits: "8h windows",
			Cadence:     fmt.Sprintf("rolling, max %d windows", restlerWindowsFor(o.cfg.Duration)),
		},
		{
			Tier:        TierReplay,
			Description: "deterministic seed replay (workload + fault schedule) on fresh celeris per seed",
			BudgetUnits: fmt.Sprintf("%d seeds total", replaySeedsFor(o.cfg.Duration)),
			Cadence:     "~200 seeds/h",
		},
	}
	o.plan = p
	return p
}

// restlerWindowsFor returns how many full 8h windows fit in d.
func restlerWindowsFor(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	n := int(d / (8 * time.Hour))
	if n == 0 && d > time.Hour {
		// Show one partial window so dry-run is informative.
		return 1
	}
	return n
}

// replaySeedsFor returns how many seeds Tier 3 would walk at 200/h.
func replaySeedsFor(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	return int(d.Hours() * 200.0)
}

// planJSON is the JSON-marshalable projection of [Plan]. [Plan]
// itself carries [properties.Spec] entries with a Predicate function
// field, which encoding/json refuses to handle. planToJSON projects
// every Spec to its (ID, Description, Tier) triple.
type planJSON struct {
	Target             string         `json:"target"`
	Arch               string         `json:"arch"`
	CelerisCommit      string         `json:"celeris_commit"`
	Duration           time.Duration  `json:"duration"`
	CheckpointInterval time.Duration  `json:"checkpoint_interval"`
	SoakMode           bool           `json:"soak_mode"`
	OpenAPIPath        string         `json:"openapi_path"`
	CelerisListenAddr  string         `json:"celeris_listen_addr"`
	CorpusSize         int            `json:"corpus_size"`
	MatrixStates       []string       `json:"matrix_states"`
	Properties         []specJSON     `json:"properties"`
	Tiers              []TierPlan     `json:"tiers"`
}

type specJSON struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Tier        string `json:"tier"`
}

func planToJSON(p *Plan) planJSON {
	out := planJSON{
		Target:             p.Target,
		Arch:               p.Arch,
		CelerisCommit:      p.CelerisCommit,
		Duration:           p.Duration,
		CheckpointInterval: p.CheckpointInterval,
		SoakMode:           p.SoakMode,
		OpenAPIPath:        p.OpenAPIPath,
		CelerisListenAddr:  p.CelerisListenAddr,
		CorpusSize:         p.CorpusSize,
		MatrixStates:       p.MatrixStates,
		Tiers:              p.Tiers,
	}
	for _, s := range p.Properties {
		out.Properties = append(out.Properties, specJSON{ID: s.ID, Description: s.Description, Tier: s.Tier})
	}
	return out
}

// PrintPlan writes a human-readable version of the orchestrator's
// plan to w. Used by cmd/validator -dry-run.
func PrintPlan(w io.Writer, p *Plan) {
	_, _ = fmt.Fprintf(w, "validator plan\n")
	_, _ = fmt.Fprintf(w, "  target=%s arch=%s celeris=%s duration=%s soak=%t\n",
		p.Target, p.Arch, p.CelerisCommit, p.Duration, p.SoakMode)
	_, _ = fmt.Fprintf(w, "  bind=%s openapi=%s\n", p.CelerisListenAddr, p.OpenAPIPath)
	_, _ = fmt.Fprintf(w, "  corpus=%d seed(s) | markov=%d state(s)\n", p.CorpusSize, len(p.MatrixStates))
	_, _ = fmt.Fprintf(w, "  properties (%d):\n", len(p.Properties))
	for _, s := range p.Properties {
		_, _ = fmt.Fprintf(w, "    %-18s [%s] %s\n", s.ID, s.Tier, s.Description)
	}
	_, _ = fmt.Fprintf(w, "  tiers:\n")
	for _, t := range p.Tiers {
		_, _ = fmt.Fprintf(w, "    %s\n", t.Tier)
		_, _ = fmt.Fprintf(w, "      %s\n", t.Description)
		_, _ = fmt.Fprintf(w, "      cadence=%s budget=%s\n", t.Cadence, t.BudgetUnits)
	}
}

// Run executes every tier in parallel for cfg.Duration. Returns the
// first invariant violation (or context cancellation) to surface.
//
// Run is intentionally short: the heavy lifting lives in the
// validator-checker (per-second predicate evaluation) and in the
// per-tier goroutines. The orchestrator's job is to wire them
// together and to drive the auto-bisect on HARD fail.
func (o *Orchestrator) Run(ctx context.Context) error {
	if err := os.MkdirAll(o.cfg.OutDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", o.cfg.OutDir, err)
	}
	plan := o.Plan()
	if err := writeJSON(filepath.Join(o.cfg.OutDir, "plan.json"), planToJSON(plan)); err != nil {
		return err
	}
	if o.cfg.DryRun {
		PrintPlan(os.Stdout, plan)
		return nil
	}

	rootCtx, cancel := context.WithTimeout(ctx, o.cfg.Duration)
	defer cancel()

	violations := make(chan Incident, 1)
	var wg sync.WaitGroup

	wg.Add(3)
	go func() { defer wg.Done(); o.runTierProperty(rootCtx, violations) }()
	go func() { defer wg.Done(); o.runTierRESTler(rootCtx, violations) }()
	go func() { defer wg.Done(); o.runTierReplay(rootCtx, violations) }()

	checkpointInterval := o.cfg.CheckpointInterval
	if checkpointInterval <= 0 {
		checkpointInterval = 24 * time.Hour
	}
	checkpoint := time.NewTicker(checkpointInterval)
	defer checkpoint.Stop()

	for {
		select {
		case inc := <-violations:
			cancel()
			wg.Wait()
			return o.handleIncident(rootCtx, inc)
		case <-checkpoint.C:
			_ = o.writeCheckpoint()
		case <-rootCtx.Done():
			wg.Wait()
			return nil
		}
	}
}

// Incident is one invariant violation. Captured by the validator-
// checker, surfaced to the orchestrator over a channel.
type Incident struct {
	Tier        Tier
	Seed        uint64
	PredicateID string
	Message     string
	Snapshot    properties.Snapshot
	ObservedAt  time.Time
}

// runTierProperty is the always-on Tier 1 driver. Stub for wave 6 —
// the production driver lives in a separate subprocess (the loadgen +
// the validator-checker), here we only set up the rendezvous so the
// dry-run plan reflects reality.
func (o *Orchestrator) runTierProperty(ctx context.Context, violations chan<- Incident) {
	<-ctx.Done()
	_ = violations // wave 7 wires the real driver.
}

// runTierRESTler is Tier 2 — RESTler-style stateful fuzzer over the
// OpenAPI spec. Wave 6 lands the scaffolding; the actual sequence
// generator is exercised by the cmd/validator-checker process.
func (o *Orchestrator) runTierRESTler(ctx context.Context, violations chan<- Incident) {
	<-ctx.Done()
	_ = violations
}

// runTierReplay is Tier 3. Walks the seed corpus, replaying each
// seed deterministically on a fresh celeris. The replay loop is in
// cmd/validator-replay; this method's job is to feed seeds at the
// configured cadence and surface violations.
func (o *Orchestrator) runTierReplay(ctx context.Context, violations chan<- Incident) {
	if len(o.seeds) == 0 {
		<-ctx.Done()
		return
	}
	// 200 seeds/h = 18s between seeds. Cap the ticker for short runs.
	cadence := 18 * time.Second
	if o.cfg.Duration < 10*time.Minute {
		cadence = o.cfg.Duration / time.Duration(len(o.seeds)+1)
		if cadence < 100*time.Millisecond {
			cadence = 100 * time.Millisecond
		}
	}
	idx := 0
	tick := time.NewTicker(cadence)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			seed := o.seeds[idx%len(o.seeds)]
			// Wave 6: we record the seed we WOULD have replayed; the
			// real subprocess fork lives in cmd/validator-replay.
			_ = seed
			idx++
		}
	}
}

// handleIncident captures forensics on a violation and writes the
// incident dossier. Wave 6 emits the dossier; wave 7 wires the
// shrinker into the same pipeline.
func (o *Orchestrator) handleIncident(ctx context.Context, inc Incident) error {
	ts := time.Now().UTC().Format("20060102-150405")
	dir := filepath.Join(o.cfg.OutDir, "incidents", ts+"-"+inc.PredicateID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	dossier := map[string]any{
		"tier":         inc.Tier.String(),
		"seed":         fmt.Sprintf("0x%x", inc.Seed),
		"predicate":    inc.PredicateID,
		"message":      inc.Message,
		"observed_at":  inc.ObservedAt.UTC().Format(time.RFC3339Nano),
		"snapshot":     inc.Snapshot,
		"commit":       o.cfg.CelerisCommit,
		"arch":         o.cfg.Arch,
		"target":       o.cfg.Target,
		"go_versions":  []string{"runtime+target"},
	}
	if err := writeJSON(filepath.Join(dir, "incident.json"), dossier); err != nil {
		return err
	}
	if err := o.captureForensics(ctx, dir); err != nil {
		// Don't propagate — the incident is already recorded.
		fmt.Fprintf(os.Stderr, "validation: forensics: %v\n", err)
	}
	// Shrinker hand-off (wave 7): for now we record the seed and the
	// last 2h of the corpus would be the replay band.
	shrinkPlan := map[string]any{
		"strategy":      "replay-last-2h-bisect-by-prefix",
		"seed":          inc.Seed,
		"max_attempts":  256,
		"prefix_halver": "split-at-floor(n/2)",
	}
	if err := writeJSON(filepath.Join(dir, "shrink_plan.json"), shrinkPlan); err != nil {
		return err
	}
	return fmt.Errorf("validation: %s violated by %s: %s", inc.PredicateID, inc.Tier, inc.Message)
}

// captureForensics gathers gcore, /proc/<pid>/{maps,status,smaps,fd,stack}
// snapshots, and pprof profiles into dir. Best-effort: every step is
// independently fault-tolerant.
func (o *Orchestrator) captureForensics(ctx context.Context, dir string) error {
	// Lookup the celeris pid we launched (left at o.cfg.CelerisListenAddr).
	// For wave 6 we just emit a forensics manifest with the commands
	// the operator would run; wave 7 (which owns the pidfile) does
	// the real capture.
	manifest := []string{
		"gcore -o " + filepath.Join(dir, "celeris.core") + " <celeris-pid>",
		"cat /proc/<celeris-pid>/maps > " + filepath.Join(dir, "maps"),
		"cat /proc/<celeris-pid>/status > " + filepath.Join(dir, "status"),
		"cat /proc/<celeris-pid>/smaps > " + filepath.Join(dir, "smaps"),
		"ls /proc/<celeris-pid>/fd > " + filepath.Join(dir, "fd.txt"),
		"cat /proc/<celeris-pid>/stack > " + filepath.Join(dir, "stack"),
		"curl -s http://<addr>/debug/pprof/heap     > " + filepath.Join(dir, "heap.pprof"),
		"curl -s http://<addr>/debug/pprof/goroutine > " + filepath.Join(dir, "goroutine.pprof"),
		"curl -s http://<addr>/debug/pprof/block    > " + filepath.Join(dir, "block.pprof"),
		"curl -s http://<addr>/debug/pprof/mutex    > " + filepath.Join(dir, "mutex.pprof"),
	}
	return os.WriteFile(filepath.Join(dir, "forensics_commands.txt"),
		[]byte(joinLines(manifest)), 0o644)
}

func joinLines(lines []string) string {
	out := ""
	for _, l := range lines {
		out += l + "\n"
	}
	return out
}

// writeCheckpoint persists a JSON snapshot of orchestrator state. The
// post-mortem replay tooling reads checkpoints to figure out where a
// crashed run was when it died.
func (o *Orchestrator) writeCheckpoint() error {
	cp := map[string]any{
		"checkpoint_at": time.Now().UTC().Format(time.RFC3339Nano),
		"target":        o.cfg.Target,
		"arch":          o.cfg.Arch,
		"commit":        o.cfg.CelerisCommit,
		"corpus_size":   len(o.seeds),
		"properties":    propertyIDs(o.predicates),
	}
	return writeJSON(filepath.Join(o.cfg.OutDir, "checkpoint.json"), cp)
}

func propertyIDs(specs []properties.Spec) []string {
	out := make([]string, 0, len(specs))
	for _, s := range specs {
		out = append(out, s.ID)
	}
	return out
}

// ReplayPlan deterministically expands a single seed into the workload
// + fault schedule the validator-replay subcommand would run. Used
// by both `cmd/validator-replay` and the dry-run output of
// `cmd/validator-replay --dry-run`.
func ReplayPlan(seed uint64, runDuration time.Duration, celerisPID, listenPort int) ReplayedSeed {
	rng := rand.New(rand.NewPCG(seed, ^seed^0x9e3779b97f4a7c15))
	// Workload pre-roll: pick a Markov step budget and a starting
	// jitter so the same seed deterministically reproduces.
	steps := 1000 + rng.IntN(9000)
	jitter := time.Duration(rng.IntN(1000)) * time.Millisecond

	schedule := fault.Generate(fault.GenerateConfig{
		Seed:              seed,
		RunDuration:       runDuration,
		CelerisPID:        celerisPID,
		CelerisListenPort: listenPort,
		LoadgenIface:      "eth0",
	})
	return ReplayedSeed{
		Seed:          seed,
		MarkovSteps:   steps,
		StartupJitter: jitter,
		Schedule:      schedule,
	}
}

// ReplayedSeed is the deterministic expansion of one seed.
type ReplayedSeed struct {
	Seed          uint64
	MarkovSteps   int
	StartupJitter time.Duration
	Schedule      fault.Schedule
}

// PrintReplayPlan writes a human-readable plan to w.
func PrintReplayPlan(w io.Writer, rs ReplayedSeed) {
	_, _ = fmt.Fprintf(w, "replay plan for seed 0x%x\n", rs.Seed)
	_, _ = fmt.Fprintf(w, "  markov_steps=%d startup_jitter=%s\n", rs.MarkovSteps, rs.StartupJitter)
	_, _ = fmt.Fprintf(w, "  fault schedule (%d entries):\n", len(rs.Schedule))
	_, _ = fmt.Fprint(w, rs.Schedule.String())
}

// writeJSON marshals v to path with 2-space indent. Mirrors the helper
// in cmd/runner so the on-disk shape is consistent across artefacts.
func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	buf, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, buf, 0o644)
}

