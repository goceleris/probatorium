package main

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The race tier's adaptive cell keeps its sizing and paces its Markov walkers
// (probatorium#481, celeris#871). Promotion depends on the NUMBER of open
// connections (conns per worker), so the walker and worker counts stay; what
// drops is the rate each walker offers.
func TestRaceAdaptiveCellKeepsItsCountsAndPacesItsWalkers(t *testing.T) {
	// Literals, not the constants: a change to the sizing must be a decision
	// made here, in review, not a side effect of editing the constant.
	if got := cellRefappWorkers("adaptive", 0); got != 2 {
		t.Errorf("race adaptive workers = %d, want 2 (unchanged)", got)
	}
	if got := cellConcurrencyFloor("adaptive"); got != 110 {
		t.Errorf("race adaptive walker floor = %d, want 110 (unchanged)", got)
	}
	if got := cellMarkovMinPeriod("auth_session_ratelimit", "adaptive", true); got != 6500*time.Microsecond {
		t.Errorf("race adaptive Markov min period = %v, want 6.5ms", got)
	}
	if raceAdaptiveMarkovMinPeriod != 6500*time.Microsecond {
		t.Errorf("raceAdaptiveMarkovMinPeriod = %v, want 6.5ms", raceAdaptiveMarkovMinPeriod)
	}
}

// The sizing of every engine is what it was: the same worker cap and the same
// walker floor, with and without -race-tier. A table of today's values as
// literals.
func TestCellSizingIsUnchangedForEveryEngine(t *testing.T) {
	for _, r := range []struct {
		engine  string
		capFlag int
		workers int
		floor   int
	}{
		{"adaptive", 0, 2, 110},
		{"adaptive", 4, 4, 110},
		{"iouring", 0, 0, 0},
		{"iouring", 8, 8, 0},
		{"epoll", 0, 0, 0},
		{"epoll", 8, 8, 0},
		{"std", 0, 0, 0},
		{"", 0, 0, 0},
	} {
		if got := cellRefappWorkers(r.engine, r.capFlag); got != r.workers {
			t.Errorf("engine=%q cap=%d: workers %d, want %d", r.engine, r.capFlag, got, r.workers)
		}
		if got := cellConcurrencyFloor(r.engine); got != r.floor {
			t.Errorf("engine=%q: walker floor %d, want %d", r.engine, got, r.floor)
		}
	}
}

// Exactly ONE cell of the matrix is shaped: the race tier's adaptive cell of
// auth_session_ratelimit (on each arch). Every other refapp, every other engine
// and every non-race tier keeps full load. The table is all eight refapps x the
// four engines x with and without -race-tier, as literals; the 7 other adaptive
// cells (run 39560945814: green at full load, 77 Markov walkers each, 21-46% of
// their volume kept on amd64 had they been shaped) join only by a pre-registered,
// data-driven decision (PREREG.md), which is a change to this table.
func TestExactlyOneCellIsShaped(t *testing.T) {
	refapps := []string{"auth_jwt_csrf", "auth_session_ratelimit", "driver_memcached", "driver_postgres",
		"driver_redis", "kitchen_sink", "observability", "static_swagger_proxy"}
	shaped := 0
	for _, refapp := range refapps {
		for _, engine := range []string{"adaptive", "iouring", "epoll", "std"} {
			for _, raceTier := range []bool{false, true} {
				want := time.Duration(0)
				if refapp == "auth_session_ratelimit" && engine == "adaptive" && raceTier {
					want = 6500 * time.Microsecond
				}
				got := cellMarkovMinPeriod(refapp, engine, raceTier)
				if got != want {
					t.Errorf("refapp=%s engine=%s raceTier=%t: Markov min period %v, want %v", refapp, engine, raceTier, got, want)
				}
				if got != 0 {
					shaped++
				}
			}
		}
	}
	if shaped != 1 {
		t.Errorf("%d of 64 (refapp, engine, tier) cells are shaped, want exactly 1", shaped)
	}
	// An engine or refapp name the matrix does not know is unshaped too.
	if got := cellMarkovMinPeriod("", "adaptive", true); got != 0 {
		t.Errorf("an empty refapp is shaped: %v", got)
	}
	if got := cellMarkovMinPeriod("auth_session_ratelimit", "", true); got != 0 {
		t.Errorf("an empty engine is shaped: %v", got)
	}
}

// Shaping is a matrix-only, adaptive-only, race-tier-only decision, like
// cellConcurrencyFloor, and nothing else in the validator may decide it. Read
// from the syntax tree, not the text, so a later override cannot hide behind a
// regexp that still matches the literal:
//   - exactly one composite-literal key MarkovMinPeriod in the package, in
//     matrix.go, whose value is cellMarkovMinPeriod(mc.Refapp, mc.Engine, cfg.RaceTier);
//   - no assignment to any MarkovMinPeriod field anywhere (a
//     `cellCfg.MarkovMinPeriod = ...` after the literal would shape every cell
//     it reaches);
//   - the constant is used once, inside cellMarkovMinPeriod;
//   - main.go (the single-cell path) never mentions MarkovMinPeriod.
func TestMarkovMinPeriodReachesTheMatrixCellConfigOnly(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no sources: %v", err)
	}
	var literals, constUses int
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		tree, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		src := func(e ast.Expr) string {
			var b strings.Builder
			_ = printer.Fprint(&b, fset, e)
			return b.String()
		}
		// Walk every top-level declaration with the name of its function ("" for
		// a var/const/type), so a use of the constant can be placed.
		for _, d := range tree.Decls {
			inFunc := ""
			if fd, ok := d.(*ast.FuncDecl); ok {
				inFunc = fd.Name.Name
			}
			ast.Inspect(d, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.ValueSpec:
					// The declaration's own names are not uses.
					for _, v := range x.Values {
						ast.Inspect(v, func(m ast.Node) bool {
							if id, ok := m.(*ast.Ident); ok && id.Name == "raceAdaptiveMarkovMinPeriod" {
								t.Errorf("%s uses raceAdaptiveMarkovMinPeriod in a package-level initialiser: only cellMarkovMinPeriod may", f)
							}
							return true
						})
					}
					return false
				case *ast.KeyValueExpr:
					if k, ok := x.Key.(*ast.Ident); ok && k.Name == "MarkovMinPeriod" {
						literals++
						if f != "matrix.go" {
							t.Errorf("%s sets MarkovMinPeriod in a Config literal: only matrix.go may", f)
						}
						if got := src(x.Value); got != "cellMarkovMinPeriod(mc.Refapp, mc.Engine, cfg.RaceTier)" {
							t.Errorf("%s: MarkovMinPeriod is set from %q, want cellMarkovMinPeriod(mc.Refapp, mc.Engine, cfg.RaceTier)", f, got)
						}
					}
				case *ast.AssignStmt:
					for _, l := range x.Lhs {
						if sel, ok := l.(*ast.SelectorExpr); ok && sel.Sel.Name == "MarkovMinPeriod" {
							t.Errorf("%s assigns %s: the period must be decided once, by cellMarkovMinPeriod, in the literal", f, src(l))
						}
					}
				case *ast.Ident:
					if x.Name == "raceAdaptiveMarkovMinPeriod" {
						if inFunc == "cellMarkovMinPeriod" {
							constUses++
						} else {
							t.Errorf("%s uses raceAdaptiveMarkovMinPeriod in %q: only cellMarkovMinPeriod may", f, inFunc)
						}
					}
				}
				return true
			})
		}
		if f == "main.go" {
			raw, _ := os.ReadFile(f)
			if strings.Contains(string(raw), "MarkovMinPeriod") {
				t.Error("main.go (the single-cell path) must not mention MarkovMinPeriod")
			}
		}
	}
	if literals != 1 {
		t.Errorf("%d Config literals set MarkovMinPeriod, want exactly 1 (matrix.go)", literals)
	}
	if constUses != 1 {
		t.Errorf("raceAdaptiveMarkovMinPeriod is used %d times inside cellMarkovMinPeriod, want 1", constUses)
	}
}

// The period is derived, not picked (PREREG.md, run 39560945814): the fleet's
// ceiling, 67 Markov walkers / period, must stay at least 15% above the
// promotion floor (8.35k requests a second: below it a healthy 64 KiB echo
// pushes a tick's bytes per request to the controller's 8192 refusal) and clearly
// below the 16.56k a second the same walkers sent closed-loop when the cell
// saturated. A change of the period that leaves this window is a change of the
// derivation, made here and in PREREG.md.
func TestRaceAdaptivePeriodSitsBetweenThePromotionFloorAndTheSaturatingRate(t *testing.T) {
	const (
		markovWalkers = 67 // 110 walkers less 22 adversarial, 11 h2c, 5 ws, 5 sse
		floor         = 8350.0
		saturating    = 16560.0
	)
	ceiling := markovWalkers / raceAdaptiveMarkovMinPeriod.Seconds()
	if ceiling < floor*1.15 {
		t.Errorf("fleet ceiling %.0f/s is under 15%% above the promotion floor %.0f/s", ceiling, floor)
	}
	if ceiling > saturating*0.7 {
		t.Errorf("fleet ceiling %.0f/s is not clearly below the %.0f/s that saturated the cell", ceiling, saturating)
	}
}
