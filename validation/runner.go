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
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goceleris/probatorium/report"
	"github.com/goceleris/probatorium/validation/corpus"
	"github.com/goceleris/probatorium/validation/fault"
	"github.com/goceleris/probatorium/validation/markov"
	"github.com/goceleris/probatorium/validation/properties"
	"github.com/goceleris/probatorium/validation/remote"
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
	// (the refapp binary). On the local driver this is a path on the
	// orchestrator host; on the SSH driver it's a path on the remote
	// host. Empty means "skip process management" — useful for the
	// unit tests, where the validator does not actually launch a
	// server.
	CelerisBin string
	// ReplayBin is the path to cmd/validator-replay. Tier 3 forks
	// this per seed with -seed -celeris-pid -celeris-port -duration.
	// Empty disables Tier 3 (the orchestrator still keeps the goroutine
	// alive on ctx but emits no replays).
	ReplayBin string
	// DriverMode picks between "local" (default — exec.Cmd-backed
	// remote.Local) and "ssh" (golang.org/x/crypto/ssh, requires
	// SSH_AUTH_SOCK + DriverSSHHost). The orchestrator constructs
	// the driver from these fields at Tier 1 / Tier 3 entry; tests
	// can stub a custom Driver via the New(...) options surface
	// when one lands.
	DriverMode string
	// DriverSSHUser is the SSH login user. Required when DriverMode
	// is "ssh"; ignored otherwise.
	DriverSSHUser string
	// DriverSSHHost is the SSH host:port (or just host). Required
	// when DriverMode is "ssh"; ignored otherwise.
	DriverSSHHost string
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

	// RefappEngine, when non-empty, is appended to the refapp's
	// process argv as `-engine <value>`. Used to exercise specific
	// celeris engines (iouring / epoll / std / adaptive) under the
	// engine matrix runner — instead of trusting the refapp's
	// `auto` default. Empty leaves the refapp's default in place.
	//
	// Tracked under issue #103 — engine matrix coverage.
	RefappEngine string

	// RefappAsyncHandlers, when non-empty ("true"/"false"), is passed to the
	// refapp as `-async-handlers=<v>`; empty leaves the refapp default. The
	// sync/async coverage axis: AsyncHandlers=false WITH a .Async() route forces
	// l.async via hasAsyncRoutes — the bench epoll-h1-sync derivation that
	// crashed in celeris#309 and that the AsyncHandlers=true refapps never
	// exercised (validation gap C).
	RefappAsyncHandlers string

	// RefappWorkers, when > 0, caps the refapp's io_uring worker count
	// (passed as `-workers <n>`, plumbed to celeris.Config.Workers) for the
	// ring-allocating engines only — see buildRefappArgs. 0 leaves the
	// celeris GOMAXPROCS default. Lets a memory-constrained validation host
	// run the heaviest io_uring refapp without io_uring_setup ENOMEM, while
	// keeping every io_uring code path covered. Must be 0 or >= 2 (celeris
	// rejects Workers in [1,2)).
	RefappWorkers int
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
	matrix     *markov.Matrix
	seeds      []corpus.Seed
	predicates []properties.Spec

	// Final tier tallies, stashed by the tier funcs on clean exit so
	// Run() can compose the v5 validation document. Both stay
	// zero-value when the tier didn't run (e.g. dry-run, empty seeds).
	// Accessed only after wg.Wait() returns, so no mutex needed.
	tier1Snapshot tier1TallySnapshot
	tier3Snapshot tier3TallySnapshot
	tier1Ran      bool
	tier3Ran      bool

	// resolvedAddr is the refapp's REAL bound address (host:port),
	// learned from the "ready addr=" banner once Tier 1 brings the
	// refapp up. With a "-bind :0" launch the OS chooses the port, so
	// this — not cfg.CelerisListenAddr (which is "127.0.0.1:0" in matrix
	// mode) — is the address live forensics (pprof) must target.
	// atomic.Pointer because the Tier 1 goroutine publishes it while the
	// incident handler reads it.
	resolvedAddr atomic.Pointer[string]
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

// CellResult is the per-cell projection of an orchestrator's
// post-Run state, used by the matrix runner (cmd/validator -matrix)
// to assemble report.ValidationResults.Cells[] across (refapp,
// engine, arch) cartesian sweeps.
//
// Either Tier1 or Tier3 (or both) may be non-nil, mirroring the
// single-cell document shape. A cell whose Tier 1 walker ran and
// whose Tier 3 replay didn't (no corpus, no replay binary, etc.)
// returns a result with Tier1 set and Tier3 nil — same as the
// existing single-cell flow.
type CellResult struct {
	Tier1Ran bool
	Tier3Ran bool
	Tier1    tier1TallySnapshot
	Tier3    tier3TallySnapshot
}

// Result returns a value-typed snapshot of the per-tier tallies
// after Run returns. Safe to call without locking — the snapshots
// are populated under the orchestrator's own mutex during the
// tier-completion callbacks; Result must only be called after Run
// has returned.
//
// Used by the matrix runner to extract per-cell results without
// reaching into orchestrator internals.
func (o *Orchestrator) Result() CellResult {
	return CellResult{
		Tier1Ran: o.tier1Ran,
		Tier3Ran: o.tier3Ran,
		Tier1:    o.tier1Snapshot,
		Tier3:    o.tier3Snapshot,
	}
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
	Target             string        `json:"target"`
	Arch               string        `json:"arch"`
	CelerisCommit      string        `json:"celeris_commit"`
	Duration           time.Duration `json:"duration"`
	CheckpointInterval time.Duration `json:"checkpoint_interval"`
	SoakMode           bool          `json:"soak_mode"`
	OpenAPIPath        string        `json:"openapi_path"`
	CelerisListenAddr  string        `json:"celeris_listen_addr"`
	CorpusSize         int           `json:"corpus_size"`
	MatrixStates       []string      `json:"matrix_states"`
	Properties         []specJSON    `json:"properties"`
	Tiers              []TierPlan    `json:"tiers"`
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

	startedAt := time.Now().UTC()
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
			// Two kinds of incidents land on this channel:
			//   - Hard predicate violations (I-PANIC, I-MW-*, ...) and
			//     T1-DRIVE / T3-SEED-FAIL — genuine bugs. Halt the run
			//     and return non-zero (CI flags it).
			//   - T*-DRIVE infra flakes — refapp boot failure, ctx
			//     cancellation killing a per-seed replay, etc. Record
			//     a dossier but DON'T halt: a 6h soak shouldn't bail
			//     because one of 1200 seeds got killed by a ssh
			//     hiccup. The orchestrator runs to completion.
			if isInfraDriveIncident(inc) {
				dir := infraIncidentDir(o.cfg.OutDir, inc)
				_ = writeJSON(filepath.Join(dir, "incident.json"), map[string]any{
					"tier":        inc.Tier.String(),
					"seed":        fmt.Sprintf("0x%x", inc.Seed),
					"predicate":   inc.PredicateID,
					"message":     inc.Message,
					"observed_at": inc.ObservedAt.UTC().Format(time.RFC3339Nano),
				})
				continue
			}
			cancel()
			wg.Wait()
			// Hard-fail path: emit the validate-results doc with
			// whatever partial tallies the tiers managed to stash
			// before the violation triggered abort. The incident
			// dossier carries the bug specifics; the v5 doc gives
			// dashboards the same shape as a clean exit.
			_ = o.writeValidateResults(startedAt)
			return o.handleIncident(rootCtx, inc)
		case <-checkpoint.C:
			_ = o.writeCheckpoint()
		case <-rootCtx.Done():
			wg.Wait()
			// Compose the canonical v5 validation document from the
			// per-tier snapshots stashed on the orchestrator. The
			// publish flow picks this up; the sidecar tier1_tally.json
			// / tier3_tally.json stay in the OutDir for postmortem
			// inspection.
			_ = o.writeValidateResults(startedAt)
			return nil
		}
	}
}

// isInfraDriveIncident reports whether inc is an infrastructure
// flake (driver / fork / ssh / ctx-cancel) rather than a celeris
// bug. T*-DRIVE classifier is the orchestrator's contract for that
// distinction: predicate code from the Tier 1 / Tier 3 drivers when
// the candidate binary itself didn't crash, just the wrapper around
// it bailed.
func isInfraDriveIncident(inc Incident) bool {
	switch inc.PredicateID {
	case "T1-DRIVE", "T3-DRIVE":
		return true
	}
	return false
}

// infraIncidentDir creates + returns the per-incident directory for
// infra-flake records. Same shape as handleIncident's dossier dir
// so postmortem tooling can read both with one walker.
func infraIncidentDir(outDir string, inc Incident) string {
	ts := time.Now().UTC().Format("20060102-150405")
	dir := filepath.Join(outDir, "incidents", ts+"-"+inc.PredicateID)
	_ = os.MkdirAll(dir, 0o755)
	return dir
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
	// RefappPID, when > 0, identifies the refapp process the
	// orchestrator should sample /proc + pprof from during forensics
	// capture. Tier 1 captures this at refapp Start; Tier 3 captures
	// it per-seed. Zero suppresses /proc-dependent forensics (the
	// dossier still records what we have).
	RefappPID int
}

// buildDriver constructs the remote.Driver per the orchestrator's
// Config.DriverMode. Default is "local" (exec.Cmd-backed). "ssh"
// uses golang.org/x/crypto/ssh against DriverSSHUser@DriverSSHHost;
// the binary path lives on the REMOTE host. ssh-agent (SSH_AUTH_SOCK)
// is the only supported auth surface.
//
// The same Driver instance serves both Tier 1 and Tier 3; they
// share the underlying ssh.Client connection because each tier
// constructs its own Driver via this helper at entry time.
func (o *Orchestrator) buildDriver() (remote.Driver, error) {
	mode := o.cfg.DriverMode
	if mode == "" {
		mode = "local"
	}
	switch mode {
	case "local":
		return remote.NewLocal(o.cfg.CelerisBin), nil
	case "ssh":
		if o.cfg.DriverSSHUser == "" || o.cfg.DriverSSHHost == "" {
			return nil, fmt.Errorf("ssh driver requires DriverSSHUser + DriverSSHHost")
		}
		return remote.NewSSH(
			o.cfg.DriverSSHUser,
			o.cfg.DriverSSHHost,
			o.cfg.CelerisBin,
			remote.SSHConfig{},
		), nil
	default:
		return nil, fmt.Errorf("unknown driver mode %q (want local|ssh)", mode)
	}
}

// runTierProperty is the always-on Tier 1 driver. Stub for wave 6 —
// the production driver lives in a separate subprocess (the loadgen +
// the validator-checker), here we only set up the rendezvous so the
// dry-run plan reflects reality.
func (o *Orchestrator) runTierProperty(ctx context.Context, violations chan<- Incident) {
	// CelerisBin empty = unit-test / dry-run-style mode: no refapp
	// fork, no real traffic. Tier 1 still has to keep the goroutine
	// alive for the orchestrator's WaitGroup, so it park on ctx.
	if o.cfg.CelerisBin == "" {
		<-ctx.Done()
		return
	}
	if o.matrix == nil {
		// Missing Markov matrix means the refapp can't be exercised
		// deterministically. Park rather than synthesise random
		// traffic — every Tier 1 result must be replayable.
		<-ctx.Done()
		return
	}
	driver, err := o.buildDriver()
	if err != nil {
		violations <- Incident{
			Tier:        TierProperty,
			PredicateID: "T1-DRIVE",
			Message:     fmt.Sprintf("build driver: %v", err),
			ObservedAt:  time.Now().UTC(),
		}
		return
	}
	defer func() { _ = driver.Close() }()

	// Tier 1 concurrency sweep cycles 1 -> 10 -> 100 -> 1k -> 10k -> 1
	// on a 6h period. For short runs we cap at 10 to keep the cell
	// small enough to bisect on hard fail; production soak picks up
	// the full sweep via the cadence ramp.
	//
	// Walker-budget activation thresholds (see driveTier1):
	//   adv:     >= 1  (always at least one walker)
	//   h2c:     >= 10 (one walker per 10)
	//   ws:      >= 20 (one walker per 20)
	//   sse:     >= 20 (one walker per 20)
	//
	// → Default of 10 leaves WS + SSE slices inactive. Long soaks
	// (>= 1h) bump to 50 so all five slices fire. Short smoke runs
	// (<5min) drop to 1 for fast iteration.
	//
	// VALIDATE_CONCURRENCY env override wins over the duration-tiered
	// default — ops can manually tune for capacity tests.
	concurrency := 10
	switch {
	case o.cfg.Duration < 5*time.Minute:
		concurrency = 1
	case o.cfg.Duration >= time.Hour:
		concurrency = 50
	}
	if v := os.Getenv("VALIDATE_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			concurrency = n
		}
	}

	pidCh := make(chan int, 1)
	var pid int
	go func() { pid = <-pidCh }() // best-effort: stays 0 until startup

	// addrCh receives the refapp's real bound addr once it's up (the
	// "-bind :0" port the OS chose). Published to the orchestrator so
	// handleIncident can point live pprof forensics at the right port.
	addrCh := make(chan string, 1)
	go func() {
		if a := <-addrCh; a != "" {
			o.resolvedAddr.Store(&a)
		}
	}()

	// alertedCounters tracks which sub-tally counters we've already
	// emitted an Incident for, so the periodic callback fires AT MOST
	// ONCE per counter per run. Without this, a steady-rate adversarial
	// bug (server accepts every malformed request) would generate
	// hundreds of incidents per second once the bug catches.
	alertedCounters := make(map[string]bool)
	tallyCB := func(snap tier1TallySnapshot) {
		// HIGH-severity sub-counters: anything non-zero is a bug.
		// Mirror the canonical list from report.invariantCounters so
		// the per-arch incident emission stays in sync with the
		// cross-arch DiffValidation gate.
		fire := func(counter, msg string, ok bool) {
			if !ok || alertedCounters[counter] {
				return
			}
			alertedCounters[counter] = true
			select {
			case violations <- Incident{
				Tier:        TierProperty,
				PredicateID: counter,
				Message:     msg,
				ObservedAt:  time.Now().UTC(),
				RefappPID:   pid,
			}:
			default:
				// Channel full or closed — orchestrator already
				// handling a hard fail; nothing more to do.
			}
		}
		fire(properties.IADVAccepted.ID,
			fmt.Sprintf("server accepted malformed adversarial bytes (count=%d) — RFC violation", snap.Adversarial.WrongAccepted),
			snap.Adversarial.WrongAccepted > 0)
		fire(properties.IH2CCrashed.ID,
			fmt.Sprintf("h2c churn returned non-HTTP babble (count=%d) — engine crash on upgrade", snap.H2CChurn.Crashed),
			snap.H2CChurn.Crashed > 0)
		fire(properties.IWSAccepted.ID,
			fmt.Sprintf("server accepted bad WebSocket frame (count=%d) — RFC 6455 violation", snap.WSTorture.AcceptedBadFrame),
			snap.WSTorture.AcceptedBadFrame > 0)
		fire(properties.IWSHang.ID,
			fmt.Sprintf("WebSocket connection hung past close timeout (count=%d) — likely goroutine wedge", snap.WSTorture.HangNoClose),
			snap.WSTorture.HangNoClose > 0)
		// Engine-agnostic crash oracle: the refapp process died mid-run. This
		// is the catch-all that the per-protocol counters above can't see — a
		// dead server just looks like connection-refused to every walker.
		fire(properties.ILiveness.ID,
			livenessDeathMessage(snap.Liveness),
			snap.Liveness.Crashed)
		// Deadlock oracle: alive but unresponsive (a wedge the walkers read as
		// connection errors). Complements I-LIVENESS for the hang class.
		fire(properties.IHang.ID,
			fmt.Sprintf("refapp wedged mid-run: %s", snap.Liveness.Reason()),
			snap.Liveness.Hung)
	}

	cfg := tier1Config{
		Driver:      driver,
		RefappArgs:  buildRefappArgs(o.cfg.CelerisListenAddr, o.cfg.RefappEngine, o.cfg.RefappAsyncHandlers, o.cfg.RefappWorkers),
		BaseURL:     "http://" + o.cfg.CelerisListenAddr,
		Matrix:      o.matrix,
		Seed:        0x6c656c6f, // 'lelo' — distinct from Tier 3's
		Concurrency: concurrency,
		// Driver.Start happens inside driveTier1 so the PID isn't
		// available until after waitForReady. The PIDChan callback
		// fires once the refapp is bound; the orchestrator stashes
		// the value so handleIncident can drive /proc + pprof
		// forensics against the same process Tier 1 is exercising.
		PIDChan:               pidCh,
		AddrChan:              addrCh,
		TallyCallback:         tallyCB,
		TallyCallbackInterval: 2 * time.Second,
		// Periodic snapshot to disk so long-running soaks (24h, 72h,
		// 10d) surface mid-run progress without waiting for the
		// orchestrator's end-of-run flush. Same path as the final
		// snapshot — the periodic writer just overwrites it as the
		// run advances.
		SnapshotPath: filepath.Join(o.cfg.OutDir, "tier1_tally.json"),
	}
	tally, err := driveTier1(ctx, cfg)
	if err != nil && ctx.Err() == nil {
		// driveTier1 failure that isn't the parent-cancel — surface
		// as a synthetic incident so the orchestrator captures
		// forensics and aborts. The "predicate" name is a coarse
		// classifier; the message carries the real cause.
		violations <- Incident{
			Tier:        TierProperty,
			PredicateID: "T1-DRIVE",
			Message:     err.Error(),
			ObservedAt:  time.Now().UTC(),
			RefappPID:   pid,
		}
		return
	}
	// Clean exit (ctx done). The final tally lands in the run
	// directory for postmortem inspection AND is stashed on the
	// orchestrator so Run() can compose the canonical v5 document
	// at the end of the run.
	o.tier1Snapshot = tally
	o.tier1Ran = true
	_ = writeJSON(filepath.Join(o.cfg.OutDir, "tier1_tally.json"), tally)
}

// runTierRESTler is Tier 2 — RESTler-style stateful fuzzer over the
// OpenAPI spec. Wave 6 lands the scaffolding; the actual sequence
// generator is exercised by the cmd/validator-checker process.
func (o *Orchestrator) runTierRESTler(ctx context.Context, violations chan<- Incident) {
	<-ctx.Done()
	_ = violations
}

// runTierReplay is Tier 3. Walks the seed corpus, replaying each
// seed deterministically on a fresh celeris. Per-seed lifecycle
// lives in driveTier3 (validation/tier3.go); this method's job is
// to set up the (Driver, ReplayBin, Seeds) tuple and surface every
// failing seed as an Incident on the orchestrator's channel.
//
// Same short-circuits as runTierProperty: empty CelerisBin parks
// (unit-test / dry-run mode), empty seeds parks (corpus loading
// must have failed; the dry-run plan already surfaced that). A
// missing ReplayBin (cmd/validator-replay) is also fatal because
// per-seed determinism depends on the replay binary's deterministic
// expansion of the fault schedule.
func (o *Orchestrator) runTierReplay(ctx context.Context, violations chan<- Incident) {
	if o.cfg.CelerisBin == "" {
		<-ctx.Done()
		return
	}
	if len(o.seeds) == 0 {
		<-ctx.Done()
		return
	}
	replayBin := o.cfg.ReplayBin
	if replayBin == "" {
		// Conventional install layout: validator-replay sits beside
		// the validator binary itself, under bench_root in the
		// production playbook. The orchestrator can't infer that
		// path on its own, so the absence is a (logged) noop rather
		// than a hard error — keeps Tier 1 running even if Tier 3
		// can't.
		<-ctx.Done()
		return
	}
	driver, err := o.buildDriver()
	if err != nil {
		violations <- Incident{
			Tier:        TierReplay,
			PredicateID: "T3-DRIVE",
			Message:     fmt.Sprintf("build driver: %v", err),
			ObservedAt:  time.Now().UTC(),
		}
		return
	}
	defer func() { _ = driver.Close() }()

	results := make(chan tier3Result, 16)
	// Funnel non-zero exits into the orchestrator's incident channel
	// in a background goroutine so driveTier3 itself stays focused
	// on the per-seed loop.
	go func() {
		for res := range results {
			if res.ExitCode == 0 {
				continue
			}
			predicate := "T3-SEED-FAIL"
			if res.ExitCode < 0 {
				predicate = "T3-DRIVE"
			}
			select {
			case violations <- Incident{
				Tier:        TierReplay,
				Seed:        res.Seed,
				PredicateID: predicate,
				Message:     summariseTier3Stderr(res),
				ObservedAt:  time.Now().UTC(),
				RefappPID:   res.RefappPID,
			}:
			case <-ctx.Done():
				return
			}
		}
	}()

	cfg := tier3Config{
		Driver:        driver,
		RefappArgs:    buildRefappArgs(o.cfg.CelerisListenAddr, o.cfg.RefappEngine, o.cfg.RefappAsyncHandlers, o.cfg.RefappWorkers),
		ReplayBin:     replayBin,
		Seeds:         o.seeds,
		CelerisCommit: o.cfg.CelerisCommit,
		// Mid-run snapshot to disk — same path the orchestrator
		// overwrites at run end. Long-running soaks get visibility
		// into seed-loop progress without waiting for ctx-cancel.
		SnapshotPath: filepath.Join(o.cfg.OutDir, "tier3_tally.json"),
		// Durable per-seed failure log — closes the channel-drop
		// gap exposed by the 3-day soak (counter=1 errored but no
		// dossier on disk). One JSON per non-zero exit, written
		// synchronously before the orchestrator-side incident
		// channel send.
		SeedLogDir: filepath.Join(o.cfg.OutDir, "tier3_failures"),
	}
	tally, err := driveTier3(ctx, cfg, results)
	close(results)
	if err != nil && ctx.Err() == nil {
		violations <- Incident{
			Tier:        TierReplay,
			PredicateID: "T3-DRIVE",
			Message:     err.Error(),
			ObservedAt:  time.Now().UTC(),
		}
		return
	}
	o.tier3Snapshot = tally
	o.tier3Ran = true
	_ = writeJSON(filepath.Join(o.cfg.OutDir, "tier3_tally.json"), tally)
}

// summariseTier3Stderr collapses a tier3Result into the one-line
// Message stored on the Incident. Prefer stderr (where the helper
// binaries log violations) over stdout when both are present;
// CombinedOutput stuffs both into Stdout so we fall back to a
// truncated stdout when stderr is empty.
func summariseTier3Stderr(res tier3Result) string {
	switch {
	case res.Stderr != "":
		if len(res.Stderr) > 256 {
			return res.Stderr[:256] + "..."
		}
		return res.Stderr
	case res.Stdout != "":
		if len(res.Stdout) > 256 {
			return res.Stdout[:256] + "..."
		}
		return res.Stdout
	default:
		return fmt.Sprintf("exit=%d (no output)", res.ExitCode)
	}
}

// livenessDeathMessage renders the I-LIVENESS incident message. Reason()
// already names a death that left a recognised crash signature; for a clean
// exit with NO signature (e.g. the refapp's own log.Fatalf -> os.Exit(1)) it
// appends the captured stdout+stderr tail so the persisted incident records WHY
// (the refapp's final output) instead of only "process exited unexpectedly
// (code=1)". The message is always written to incident.json, so this guarantees
// the cause reaches the run evidence.
func livenessDeathMessage(s livenessSnapshot) string {
	msg := "refapp process died mid-run: " + s.Reason()
	if s.Signature == "" && s.Trace != "" {
		msg += "\n" + s.Trace
	}
	return msg
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
		"tier":        inc.Tier.String(),
		"seed":        fmt.Sprintf("0x%x", inc.Seed),
		"predicate":   inc.PredicateID,
		"message":     inc.Message,
		"observed_at": inc.ObservedAt.UTC().Format(time.RFC3339Nano),
		"snapshot":    inc.Snapshot,
		"commit":      o.cfg.CelerisCommit,
		"arch":        o.cfg.Arch,
		"target":      o.cfg.Target,
		"go_versions": []string{"runtime+target"},
	}
	if err := writeJSON(filepath.Join(dir, "incident.json"), dossier); err != nil {
		return err
	}
	if err := o.captureForensics(ctx, dir, inc); err != nil {
		// Don't propagate — the incident is already recorded.
		fmt.Fprintf(os.Stderr, "validation: forensics: %v\n", err)
	}
	// Seed shrinker: bisect the replay duration to find the smallest
	// window that still reproduces. Only fires for Tier 3 seed
	// failures (T3-SEED-FAIL); other incidents don't have a seed +
	// duration tuple to shrink. Best-effort: any shrink failure is
	// logged in shrink_log.json, never propagated.
	if inc.PredicateID == "T3-SEED-FAIL" && inc.Seed != 0 && o.cfg.ReplayBin != "" {
		shrinkDir := filepath.Join(dir, "shrink")
		if err := os.MkdirAll(shrinkDir, 0o755); err == nil {
			scfg := shrinkCfg{
				ReplayBin:        o.cfg.ReplayBin,
				RefappBin:        o.cfg.CelerisBin,
				RefappListenAddr: o.cfg.CelerisListenAddr,
				CelerisCommit:    o.cfg.CelerisCommit,
				// Shrink up to the configured Duration; can't be
				// larger because we don't know the actual original.
				// 15s mirrors tier3Config.PerSeedDuration default —
				// the seed replay always ran for at most that.
				OriginalDuration: 15 * time.Second,
				MaxAttempts:      6,
			}
			// Run on a fresh ctx with its own deadline so a long
			// shrink (worst case ~6 × 15s = 90s) doesn't block
			// the outer Run from returning.
			shrinkCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			_ = shrinkFailingSeed(shrinkCtx, shrinkDir, inc.Seed, scfg)
			cancel()
		}
	}
	return fmt.Errorf("validation: %s violated by %s: %s", inc.PredicateID, inc.Tier, inc.Message)
}

// captureForensics gathers gcore, /proc/<pid>/{maps,status,smaps,
// fd,stack} snapshots, pprof profiles, and dmesg into dir. Delegates
// to captureForensicsLive (validation/forensics.go); per-file errors
// are turned into `.missing` markers so a partial capture still
// produces a usable dossier.
//
// inc.RefappPID drives the /proc + gcore legs. Zero (or a stale PID
// from a process that died before forensics fired) writes a
// forensics_status.txt stub; the dossier still includes incident.json
// and shrink_plan.json so the orchestrator can act on what it has.
func (o *Orchestrator) captureForensics(ctx context.Context, dir string, inc Incident) error {
	// Prefer the refapp's REAL bound addr (the "-bind :0" port the OS
	// chose, learned from the ready banner); fall back to the configured
	// addr for fixed-port / single-cell runs where they're the same.
	addr := o.cfg.CelerisListenAddr
	if r := o.resolvedAddr.Load(); r != nil && *r != "" {
		addr = *r
	}
	return captureForensicsLive(ctx, dir, inc.RefappPID, addr)
}

// Tier1Summary projects a tier1TallySnapshot into the public
// report.Tier1Summary shape the matrix runner emits in
// ValidationResults.Cells[]. Mirrors the inline projection in
// writeValidateResults; kept exported here so callers outside the
// validation package don't have to duplicate the map keys.
func (s tier1TallySnapshot) Tier1Summary() *report.Tier1Summary {
	return &report.Tier1Summary{
		RequestsSent:  s.RequestsSent,
		Requests2xx:   s.Requests2xx,
		Requests4xx:   s.Requests4xx,
		Requests5xx:   s.Requests5xx,
		RequestsError: s.RequestsError,
		Adversarial: map[string]int64{
			"adv_sent":               s.Adversarial.Sent,
			"adv_well_rejected":      s.Adversarial.WellRejected,
			"adv_wrong_accepted":     s.Adversarial.WrongAccepted,
			"adv_hang_until_timeout": s.Adversarial.HangUntilTimeout,
		},
		H2CChurn: map[string]int64{
			"h2c_sent":            s.H2CChurn.Sent,
			"h2c_upgraded":        s.H2CChurn.Upgraded,
			"h2c_declined":        s.H2CChurn.Declined,
			"h2c_crashed":         s.H2CChurn.Crashed,
			"h2c_hang":            s.H2CChurn.Hang,
			"h2c_intentional_rst": s.H2CChurn.IntentionalRST,
			// Cause split for h2c_hang (celeris#470). The bare total
			// cannot separate a real >10s stall from an immediate
			// close; without these the classification never reaches
			// the report and the whole measurement is inert.
			"h2c_hang_eof":            s.H2CChurn.HangEOF,
			"h2c_hang_timeout":        s.H2CChurn.HangTimeout,
			"h2c_hang_reset":          s.H2CChurn.HangReset,
			"h2c_hang_other":          s.H2CChurn.HangOther,
			"h2c_hang_max_elapsed_ms": s.H2CChurn.HangMaxElapsedMs,
		},
		WSTorture: map[string]int64{
			"ws_sent":               s.WSTorture.Sent,
			"ws_upgraded":           s.WSTorture.Upgraded,
			"ws_handshake_fail":     s.WSTorture.HandshakeFail,
			"ws_closed_correctly":   s.WSTorture.ClosedCorrectly,
			"ws_accepted_bad_frame": s.WSTorture.AcceptedBadFrame,
			"ws_hang_no_close":      s.WSTorture.HangNoClose,
			"ws_endpoint_absent":    s.WSTorture.EndpointAbsent,
		},
		SSEKill: map[string]int64{
			"sse_sent":                s.SSEKill.Sent,
			"sse_established":         s.SSEKill.Established,
			"sse_events_read":         s.SSEKill.EventsRead,
			"sse_killed_mid_stream":   s.SSEKill.KilledMidStream,
			"sse_server_closed_early": s.SSEKill.ServerClosedEarly,
			"sse_handshake_fail":      s.SSEKill.HandshakeFail,
			"sse_endpoint_absent":     s.SSEKill.EndpointAbsent,
		},
	}
}

// Tier3Summary projects a tier3TallySnapshot into the public
// report.Tier3Summary shape.
func (s tier3TallySnapshot) Tier3Summary() *report.Tier3Summary {
	return &report.Tier3Summary{
		SeedsAttempted: s.SeedsAttempted,
		SeedsPassed:    s.SeedsPassed,
		SeedsFailed:    s.SeedsFailed,
		SeedsErrored:   s.SeedsErrored,
	}
}

// writeValidateResults composes the canonical v5 validation document
// from per-tier snapshots stashed by the tier funcs on clean exit,
// and writes it to OutDir/validate-results.json. The publish flow
// picks this up; per-tier sidecar JSONs (tier1_tally.json,
// tier3_tally.json) stay in OutDir for postmortem inspection.
//
// Returns nil silently when neither tier ran (dry-run / empty seeds),
// so callers can `defer o.writeValidateResults(...)` without
// branching.
func (o *Orchestrator) writeValidateResults(startedAt time.Time) error {
	if !o.tier1Ran && !o.tier3Ran {
		return nil
	}
	doc := report.Document{
		SchemaVersion: report.SchemaVersion,
		HostArchPair:  o.cfg.Target + "-" + o.cfg.Arch,
		Validation: &report.ValidationResults{
			StartedAt:  startedAt,
			FinishedAt: time.Now().UTC(),
		},
	}
	if o.tier1Ran {
		s := o.tier1Snapshot
		doc.Validation.Tier1 = &report.Tier1Summary{
			RequestsSent:  s.RequestsSent,
			Requests2xx:   s.Requests2xx,
			Requests4xx:   s.Requests4xx,
			Requests5xx:   s.Requests5xx,
			RequestsError: s.RequestsError,
			Adversarial: map[string]int64{
				"adv_sent":               s.Adversarial.Sent,
				"adv_well_rejected":      s.Adversarial.WellRejected,
				"adv_wrong_accepted":     s.Adversarial.WrongAccepted,
				"adv_hang_until_timeout": s.Adversarial.HangUntilTimeout,
			},
			H2CChurn: map[string]int64{
				"h2c_sent":            s.H2CChurn.Sent,
				"h2c_upgraded":        s.H2CChurn.Upgraded,
				"h2c_declined":        s.H2CChurn.Declined,
				"h2c_crashed":         s.H2CChurn.Crashed,
				"h2c_hang":            s.H2CChurn.Hang,
				"h2c_intentional_rst": s.H2CChurn.IntentionalRST,
			},
			WSTorture: map[string]int64{
				"ws_sent":               s.WSTorture.Sent,
				"ws_upgraded":           s.WSTorture.Upgraded,
				"ws_handshake_fail":     s.WSTorture.HandshakeFail,
				"ws_closed_correctly":   s.WSTorture.ClosedCorrectly,
				"ws_accepted_bad_frame": s.WSTorture.AcceptedBadFrame,
				"ws_hang_no_close":      s.WSTorture.HangNoClose,
				"ws_endpoint_absent":    s.WSTorture.EndpointAbsent,
			},
			SSEKill: map[string]int64{
				"sse_sent":                s.SSEKill.Sent,
				"sse_established":         s.SSEKill.Established,
				"sse_events_read":         s.SSEKill.EventsRead,
				"sse_killed_mid_stream":   s.SSEKill.KilledMidStream,
				"sse_server_closed_early": s.SSEKill.ServerClosedEarly,
				"sse_handshake_fail":      s.SSEKill.HandshakeFail,
				"sse_endpoint_absent":     s.SSEKill.EndpointAbsent,
			},
		}
	}
	if o.tier3Ran {
		s := o.tier3Snapshot
		doc.Validation.Tier3 = &report.Tier3Summary{
			SeedsAttempted: s.SeedsAttempted,
			SeedsPassed:    s.SeedsPassed,
			SeedsFailed:    s.SeedsFailed,
			SeedsErrored:   s.SeedsErrored,
		}
	}
	// Populate SoakSummary when the run was driven in soak mode
	// (`mage Soak` flips Config.SoakMode=true) OR when Duration
	// exceeds 6h (the documented soak threshold from schema.go).
	// Empty / brief Validate runs leave doc.Soak nil so the report
	// stays minimal.
	//
	// Fields currently populated:
	//   - Duration:         elapsed wall time from tier-start.
	//   - PerHourErrorRate: (RequestsError / RequestsSent) × 100,
	//     averaged across the entire window — close
	//     enough to "per-hour" for the docs intent
	//     (long soaks dampen short-term spikes).
	//
	// Fields left zero pending follow-up infrastructure:
	//   - RestartedProcesses:    needs an orchestrator-level counter
	//     incremented by the per-tier driver
	//     restart path (none currently restarts
	//     a refapp inside a single run).
	//   - GoroutineLeakDetected: needs heap-slope-vs-baseline
	//     comparison from the validator-checker
	//     /debug/vars stream (Snapshot history
	//     in properties.Context).
	//   - HeapGrowthMB:          same Snapshot-history dependency.
	//
	// Per probatorium#103-followup: keeping the Soak block written
	// (with the easy fields) lets downstream report-diff tooling
	// distinguish soak from non-soak runs; the missing fields are
	// flagged as known-pending in the schema-test fixture.
	elapsed := time.Since(startedAt)
	const soakThreshold = 6 * time.Hour
	if o.cfg.SoakMode || elapsed >= soakThreshold {
		var errRate float64
		if o.tier1Ran && o.tier1Snapshot.RequestsSent > 0 {
			errRate = float64(o.tier1Snapshot.RequestsError) /
				float64(o.tier1Snapshot.RequestsSent) * 100.0
		}
		doc.Soak = &report.SoakSummary{
			Duration:         elapsed,
			PerHourErrorRate: errRate,
			// RestartedProcesses, GoroutineLeakDetected, HeapGrowthMB:
			// zero until per-tier driver-restart counter + per-snapshot
			// heap/goroutine-history tracking are wired in.
		}
	}
	return writeJSON(filepath.Join(o.cfg.OutDir, "validate-results.json"), doc)
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

// buildRefappArgs builds the argv for a refapp invocation. Every
// refapp accepts `-bind <addr>`; when an explicit engine is requested
// (engine matrix runner) the helper also appends `-engine <name>` so
// the refapp's auto-default is bypassed.
//
// Per issue #103 (engine matrix coverage): the orchestrator threads
// the engine choice through Config.RefappEngine so a single mage
// Validate run can pin to a specific celeris engine without rebuilding
// the refapp.
//
// workers caps the refapp's io_uring worker count (celeris.Config.Workers)
// and is emitted ONLY for the ring-allocating engines (iouring / adaptive),
// and only when > 0. Memory-constrained validation hosts can't pin 16
// io_uring rings (one per worker at GOMAXPROCS) alongside the heaviest
// refapp's heap — kitchen_sink-iouring hit io_uring_setup ENOMEM on a 28GB
// host. Capping workers shrinks the locked-page footprint (4 workers ≈ 4×
// less, below the 12-worker host that already survives) while preserving
// every io_uring code path. std ignores Workers; epoll honours it but has
// no locked memory to save, so it's deliberately left at GOMAXPROCS to keep
// its cross-loop SO_REUSEPORT coverage intact. celeris requires Workers>=2
// when set; 0 = leave the GOMAXPROCS default.
func buildRefappArgs(addr, engine, asyncHandlers string, workers int) []string {
	args := []string{"-bind", addr}
	if engine != "" {
		args = append(args, "-engine", engine)
	}
	if asyncHandlers != "" {
		args = append(args, "-async-handlers="+asyncHandlers)
	}
	if workers > 0 && (engine == "iouring" || engine == "adaptive") {
		args = append(args, "-workers", strconv.Itoa(workers))
	}
	return args
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
