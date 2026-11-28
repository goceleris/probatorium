// Command raceshape re-derives the race tier's load shaping at a celeris commit
// and moves the celeris pin to it, in one step (probatorium#534, celeris#871).
//
//	go run ./tools/raceshape repin <celeris commit or version> [flags]
//	go run ./tools/raceshape derive <celeris commit, version or source directory>
//	go run ./tools/raceshape check
//
// repin resolves the commit to its module version, reads the adaptive
// controller's source at that commit, derives the promotion floor and the
// Markov period from it (derive.go), and rewrites, in one pass:
//
//   - cmd/validator/matrix.go: raceAdaptiveMarkovMinPeriod and the derivation
//     comment between its raceshape:derived markers;
//   - cmd/validator/race_shape_test.go: the pin block (celeris version,
//     period, floor) the tests compare against;
//   - the celeris requirement of every validation/refapp module (go.mod and
//     go.sum, through `go get`), then `go build ./...` in each; with -servers
//     also servers/celeris;
//   - -prereg FILE: the floor and period blocks of PREREG.md;
//   - -reader FILE: FLOOR and the period default of read-shaped-cell.py.
//
// It fails, and changes nothing, when the controller at that commit is not what
// the derivation assumes (the 8192 B/request refusal and the telemetry that
// feeds it, shape.go), when a marker or constant it rewrites is missing, or when
// no period fits the window. A failure after it has written files (go get, or
// go build of the refapps against the new commit) says the tree is partly moved
// and lists the git checkout that restores it; the refapps may need a source
// change that lands in the same commit as the re-pin. -dry-run prints what
// would change and changes nothing. It never removes a file, never commits and
// never pushes.
//
// check verifies that the repository is consistent with its pin: the period
// constant, the pin block and the refapp modules' celeris version agree.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// resolveCeleris, runGoGet and runGoBuild are the steps that need the network
// or a toolchain run; tests replace them.
var (
	resolveCeleris = ResolveCeleris
	runGoGet       = goGet
	runGoBuild     = goBuild
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "raceshape: FAIL:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: raceshape repin <celeris commit or version> [-dry-run] [-servers] [-no-build] [-prereg FILE] [-reader FILE] [-repo DIR] | derive <commit|version|dir> | check [-repo DIR]")
	}
	switch args[0] {
	case "repin":
		return cmdRepin(args[1:], out)
	case "derive":
		return cmdDerive(args[1:], out)
	case "check":
		return cmdCheck(args[1:], out)
	}
	return fmt.Errorf("unknown command %q", args[0])
}

// sourceOf returns the celeris module directory and version for arg: an
// existing directory is used as is (version empty), anything else is resolved
// through the module system (a commit, a branch or a pseudo-version).
func sourceOf(arg string) (dir, version string, err error) {
	if st, e := os.Stat(arg); e == nil && st.IsDir() {
		return arg, "", nil
	}
	return resolveCeleris(arg)
}

// ResolveCeleris downloads github.com/goceleris/celeris at rev through the
// module system and returns its directory and module version. The scratch
// module lives in a fresh temporary directory that is left for the system.
func ResolveCeleris(rev string) (dir, version string, err error) {
	tmp, err := os.MkdirTemp("", "raceshape-resolve-")
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(filepath.Join(tmp, "go.mod"), []byte("module resolve\n\ngo 1.27\n"), 0o644); err != nil {
		return "", "", err
	}
	cmd := exec.Command("go", "mod", "download", "-json", "github.com/goceleris/celeris@"+rev)
	cmd.Dir = tmp
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOTOOLCHAIN=local")
	b, runErr := cmd.Output()
	var res struct{ Version, Dir, Error string }
	if len(b) > 0 {
		_ = json.Unmarshal(b, &res)
	}
	if runErr != nil || res.Error != "" || res.Dir == "" {
		msg := res.Error
		if msg == "" && runErr != nil {
			msg = runErr.Error()
			var ee *exec.ExitError
			if errors.As(runErr, &ee) {
				msg += ": " + strings.TrimSpace(string(ee.Stderr))
			}
		}
		return "", "", fmt.Errorf("cannot resolve celeris %q: %s", rev, msg)
	}
	return res.Dir, res.Version, nil
}

func cmdDerive(args []string, out io.Writer) error {
	if len(args) != 1 {
		return errors.New("usage: raceshape derive <celeris commit, version or source directory>")
	}
	dir, version, err := sourceOf(args[0])
	if err != nil {
		return err
	}
	c, err := ReadController(dir)
	if err != nil {
		return err
	}
	r, err := Derive(c.Threshold, Run39560945814)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "celeris %s (%s)\n%s", version, dir, summary(r))
	return nil
}

func summary(r Result) string {
	var b strings.Builder
	for _, s := range r.Scenarios {
		_, _ = fmt.Fprintf(&b, "scenario %s: b0=%.0f B/request X=%s MB/tick -> R > %.0f requests/s\n", s.Name, s.BytesPerReq, fmtMB(s.BytesTick), s.FloorRPS)
	}
	_, _ = fmt.Fprintf(&b, "promotion floor: %.0f requests/s (%s)\n", r.FloorRPS, fmtK(r.FloorRPS))
	_, _ = fmt.Fprintf(&b, "period: %v (window %v to %v); ceiling %d / %v = %.0f requests/s, %.2fx the floor, %.0f%% of the saturating %.0f\n",
		r.Period, r.MinPeriod.Round(10e3), r.MaxPeriod.Round(10e3), r.Measured.MarkovWalkers, r.Period, r.CeilingRPS, r.CeilingOver, r.Saturating*100, r.Measured.SaturatingRPS)
	return b.String()
}

// repoRoot finds the probatorium module root from dir upwards.
func repoRoot(dir string) (string, error) {
	d, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		b, err := os.ReadFile(filepath.Join(d, "go.mod"))
		if err == nil && strings.HasPrefix(string(b), "module github.com/goceleris/probatorium\n") {
			return d, nil
		}
		p := filepath.Dir(d)
		if p == d {
			return "", fmt.Errorf("no probatorium module root above %s", dir)
		}
		d = p
	}
}

var celerisReqRe = regexp.MustCompile(`(?m)^\s*(?:require\s+)?(github\.com/goceleris/celeris(?:/[A-Za-z0-9_./-]+)?)\s+(v[0-9][^\s]*)\s*$`)

// refappModules lists the module directories whose celeris pin the race tier
// builds from, relative paths under root.
func refappModules(root string) ([]string, error) {
	var mods []string
	base := filepath.Join(root, "validation", "refapp")
	err := filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == "go.mod" {
			mods = append(mods, filepath.Dir(p))
		}
		return nil
	})
	sort.Strings(mods)
	return mods, err
}

// celerisPins returns, for one module, the celeris module paths it requires
// and their versions.
func celerisPins(modDir string) (map[string]string, error) {
	b, err := os.ReadFile(filepath.Join(modDir, "go.mod"))
	if err != nil {
		return nil, err
	}
	pins := map[string]string{}
	for _, m := range celerisReqRe.FindAllStringSubmatch(string(b), -1) {
		pins[m[1]] = m[2]
	}
	return pins, nil
}

// currentPin is the one celeris version every refapp module is pinned to; the
// modules disagreeing is an error.
func currentPin(root string) (string, error) {
	mods, err := refappModules(root)
	if err != nil {
		return "", err
	}
	pin := ""
	n := 0
	for _, m := range mods {
		pins, err := celerisPins(m)
		if err != nil {
			return "", err
		}
		for path, v := range pins {
			n++
			if pin == "" {
				pin = v
			}
			if v != pin {
				rel, _ := filepath.Rel(root, m)
				return "", fmt.Errorf("the refapp modules are not on one celeris version: %s requires %s at %s, others at %s", rel, path, v, pin)
			}
		}
	}
	if n == 0 {
		return "", errors.New("no refapp module requires celeris")
	}
	return pin, nil
}

type edit struct {
	path     string
	old, new string
}

func cmdRepin(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("repin", flag.ContinueOnError)
	dry := fs.Bool("dry-run", false, "print what would change; change nothing")
	servers := fs.Bool("servers", false, "also move servers/celeris")
	noBuild := fs.Bool("no-build", false, "skip go build in the moved modules")
	prereg := fs.String("prereg", "", "PREREG.md to update")
	reader := fs.String("reader", "", "read-shaped-cell.py to update")
	repo := fs.String("repo", ".", "a directory in the probatorium checkout")
	// The commit comes first, flags after: move it out of the way for flag.
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: raceshape repin <celeris commit or version> [flags]")
	}
	rev := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments %v", fs.Args())
	}
	root, err := repoRoot(*repo)
	if err != nil {
		return err
	}
	oldPin, err := currentPin(root)
	if err != nil {
		return err
	}
	dir, version, err := resolveCeleris(rev)
	if err != nil {
		return err
	}
	c, err := ReadController(dir)
	if err != nil {
		return err
	}
	r, err := Derive(c.Threshold, Run39560945814)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "celeris %s -> %s (pin was %s)\n%s", rev, version, oldPin, summary(r))

	var edits []edit
	rewrite := func(path string, f func(string) (string, error)) error {
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		n, err := f(string(b))
		if err != nil {
			return err
		}
		if n != string(b) {
			edits = append(edits, edit{path, string(b), n})
		}
		return nil
	}
	matrix := filepath.Join(root, "cmd", "validator", "matrix.go")
	mb, err := os.ReadFile(matrix)
	if err != nil {
		return err
	}
	oldUs, err := currentPeriodUs(string(mb))
	if err != nil {
		return err
	}
	if err := rewrite(matrix, func(s string) (string, error) { return ApplyMatrix(s, r) }); err != nil {
		return err
	}
	if err := rewrite(filepath.Join(root, "cmd", "validator", "race_shape_test.go"), func(s string) (string, error) { return ApplyPins(s, r, version) }); err != nil {
		return err
	}
	if *prereg != "" {
		if err := rewrite(*prereg, func(s string) (string, error) { return ApplyPrereg(s, r, version) }); err != nil {
			return err
		}
	}
	if *reader != "" {
		if err := rewrite(*reader, func(s string) (string, error) { return ApplyReader(s, r, oldUs) }); err != nil {
			return err
		}
	}
	// Everything that can fail on its own has run; a failure above changed nothing.
	mods, err := refappModules(root)
	if err != nil {
		return err
	}
	if *servers {
		mods = append(mods, filepath.Join(root, "servers", "celeris"))
	}
	for _, e := range edits {
		_, _ = fmt.Fprintf(out, "%s %s\n", map[bool]string{true: "would change", false: "changing"}[*dry], e.path)
		printDiff(out, e.old, e.new)
	}
	if len(edits) == 0 {
		_, _ = fmt.Fprintln(out, "the text files already carry these numbers")
	}
	for _, m := range mods {
		pins, err := celerisPins(m)
		if err != nil {
			return err
		}
		var paths []string
		for p, v := range pins {
			if v != version {
				paths = append(paths, p+"@"+version)
			}
		}
		sort.Strings(paths)
		rel, _ := filepath.Rel(root, m)
		if len(paths) == 0 {
			_, _ = fmt.Fprintf(out, "%s: already at %s\n", rel, version)
			continue
		}
		_, _ = fmt.Fprintf(out, "%s %s: go get %s\n", map[bool]string{true: "would run in", false: "running in"}[*dry], rel, strings.Join(paths, " "))
	}
	if *dry {
		_, _ = fmt.Fprintln(out, "dry run: nothing was changed")
		return nil
	}
	for _, e := range edits {
		if err := os.WriteFile(e.path, []byte(e.new), 0o644); err != nil {
			return err
		}
	}
	// Everything that can fail on its own has run, and the text files are
	// written. From here a failure leaves a partly moved tree, so it says which
	// files to restore. Every module is moved first and then every module is
	// built, so one build failure does not hide the others.
	restore := []string{}
	for _, e := range edits {
		restore = append(restore, relTo(root, e.path))
	}
	var moved []string
	for _, m := range mods {
		pins, err := celerisPins(m)
		if err != nil {
			return err
		}
		need := false
		for _, v := range pins {
			need = need || v != version
		}
		if !need {
			continue
		}
		restore = append(restore, relTo(root, filepath.Join(m, "go.mod")), relTo(root, filepath.Join(m, "go.sum")))
		if err := runGoGet(m, version); err != nil {
			return partlyMoved(root, restore, []string{err.Error()})
		}
		moved = append(moved, m)
		_, _ = fmt.Fprintf(out, "moved %s\n", relTo(root, m))
	}
	if !*noBuild {
		var fails []string
		for _, m := range moved {
			if err := runGoBuild(m); err != nil {
				fails = append(fails, err.Error())
				continue
			}
			_, _ = fmt.Fprintf(out, "builds %s\n", relTo(root, m))
		}
		if len(fails) > 0 {
			return partlyMoved(root, restore, fails)
		}
	}
	_, _ = fmt.Fprintf(out, "done: re-pinned to %s. Next: go test ./cmd/validator ./tools/raceshape ./report, review the diff, commit.\n", version)
	return nil
}

func relTo(root, p string) string {
	if r, err := filepath.Rel(root, p); err == nil {
		return r
	}
	return p
}

// partlyMoved is the error for a failure after files were written: the build of
// the refapps against the new commit, or a go get. The text files and the
// modules that were moved stay as they are, for inspection.
func partlyMoved(root string, restore, fails []string) error {
	return fmt.Errorf("the re-pin went as far as writing files and then failed, so the tree is PARTLY MOVED:\n  - %s\nRestore it with: git -C %s checkout -- %s\n(a build failure means the refapps need a source change that lands in the same commit as the re-pin)",
		strings.Join(fails, "\n  - "), root, strings.Join(restore, " "))
}

func goEnv(modDir string, args ...string) error {
	cmd := exec.Command("go", args...)
	cmd.Dir = modDir
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOTOOLCHAIN=local")
	if b, err := cmd.CombinedOutput(); err != nil {
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		if len(lines) > 20 {
			lines = append(lines[:20], "...")
		}
		return fmt.Errorf("go %s in %s: %w\n    %s", strings.Join(args, " "), modDir, err, strings.Join(lines, "\n    "))
	}
	return nil
}

// goGet moves every celeris requirement of the module that is not at version.
func goGet(modDir, version string) error {
	pins, err := celerisPins(modDir)
	if err != nil {
		return err
	}
	var paths []string
	for p, v := range pins {
		if v != version {
			paths = append(paths, p+"@"+version)
		}
	}
	sort.Strings(paths)
	return goEnv(modDir, append([]string{"get"}, paths...)...)
}

func goBuild(modDir string) error { return goEnv(modDir, "build", "./...") }

// printDiff prints the lines that differ, a line at a time.
func printDiff(out io.Writer, oldS, newS string) {
	count := func(s string) map[string]int {
		m := map[string]int{}
		for _, l := range strings.Split(s, "\n") {
			m[l]++
		}
		return m
	}
	o, n := count(oldS), count(newS)
	for _, l := range strings.Split(oldS, "\n") {
		if n[l] == 0 {
			_, _ = fmt.Fprintf(out, "  - %s\n", l)
		}
	}
	for _, l := range strings.Split(newS, "\n") {
		if o[l] == 0 {
			_, _ = fmt.Fprintf(out, "  + %s\n", l)
		}
	}
}

// Repository is what check reads: the values that must agree.
type Repository struct {
	PeriodUs   int64
	PinPeriod  int64
	PinFloor   float64
	PinVersion string
	Modules    string // the celeris version every refapp module requires
}

var (
	pinVersionRe = regexp.MustCompile(`pinnedCelerisVersion\s*=\s*"([^"]+)"`)
	pinPeriodRe  = regexp.MustCompile(`pinnedMarkovMinPeriod\s*=\s*(\d+) \* time\.Microsecond`)
	pinFloorRe   = regexp.MustCompile(`pinnedPromotionFloorRPS\s*=\s*([0-9.]+)`)
)

// ReadRepository reads the values of the checkout at root.
func ReadRepository(root string) (Repository, error) {
	var r Repository
	mb, err := os.ReadFile(filepath.Join(root, "cmd", "validator", "matrix.go"))
	if err != nil {
		return r, err
	}
	if r.PeriodUs, err = currentPeriodUs(string(mb)); err != nil {
		return r, err
	}
	tb, err := os.ReadFile(filepath.Join(root, "cmd", "validator", "race_shape_test.go"))
	if err != nil {
		return r, err
	}
	t := string(tb)
	vm, pm, fm := pinVersionRe.FindStringSubmatch(t), pinPeriodRe.FindStringSubmatch(t), pinFloorRe.FindStringSubmatch(t)
	if vm == nil || pm == nil || fm == nil {
		return r, errors.New("race_shape_test.go: the pin block is missing a constant")
	}
	r.PinVersion = vm[1]
	if _, err := fmt.Sscan(pm[1], &r.PinPeriod); err != nil {
		return r, err
	}
	if _, err := fmt.Sscan(fm[1], &r.PinFloor); err != nil {
		return r, err
	}
	r.Modules, err = currentPin(root)
	return r, err
}

// Problems lists every disagreement in the repository.
func (r Repository) Problems() []string {
	var p []string
	if r.PeriodUs != r.PinPeriod {
		p = append(p, fmt.Sprintf("matrix.go has a period of %d us, the test pin block says %d us", r.PeriodUs, r.PinPeriod))
	}
	if r.Modules != r.PinVersion {
		p = append(p, fmt.Sprintf("the refapp modules require celeris %s, the pin block was derived at %s: run `go run ./tools/raceshape repin <commit>`", r.Modules, r.PinVersion))
	}
	return p
}

func cmdCheck(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	repo := fs.String("repo", ".", "a directory in the probatorium checkout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, err := repoRoot(*repo)
	if err != nil {
		return err
	}
	r, err := ReadRepository(root)
	if err != nil {
		return err
	}
	if p := r.Problems(); len(p) > 0 {
		return errors.New(strings.Join(p, "; "))
	}
	_, _ = fmt.Fprintf(out, "consistent: celeris %s, period %d us, floor %.0f requests/s\n", r.PinVersion, r.PeriodUs, r.PinFloor)
	return nil
}
