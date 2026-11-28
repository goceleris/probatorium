package main

import (
	"bytes"
	"encoding/json"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The pin the fixtures stand for: celeris 713b40dfdd0d, the version the race
// tier's refapps are pinned to when the shaping was derived.
const fixtureVersion = "v1.5.12-0.20261021042539-713b40dfdd0d"

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func writeFile(t *testing.T, path, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

// celerisTree writes a celeris tree holding the controller and telemetry
// excerpts of the fixture, after mutate has changed them (file name -> edit).
func celerisTree(t *testing.T, mutate map[string]func(string) string) string {
	t.Helper()
	dir := t.TempDir()
	for _, f := range []string{"controller.go", "telemetry.go"} {
		s := readFile(t, filepath.Join("testdata", "celeris-713b40d", f+".txt"))
		if m := mutate[f]; m != nil {
			s = m(s)
		}
		writeFile(t, filepath.Join(dir, "internal", "adaptive", f), s)
	}
	return dir
}

func replaceOnce(t *testing.T, old, new string) func(string) string {
	return func(s string) string {
		t.Helper()
		if strings.Count(s, old) != 1 {
			t.Fatalf("the fixture has %d copies of %q, want 1", strings.Count(s, old), old)
		}
		return strings.Replace(s, old, new, 1)
	}
}

func repoRootForTest(t *testing.T) string {
	t.Helper()
	root, err := repoRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// The derivation at the pin reproduces the pinned numbers: the promotion floor
// of 8.35k requests a second and the Markov period of 6.5 ms.
func TestDerivationAtThePinReproducesTheFloorAndThePeriod(t *testing.T) {
	c, err := ReadController(celerisTree(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Threshold != 8192 {
		t.Fatalf("threshold %v, want 8192", c.Threshold)
	}
	r, err := Derive(c.Threshold, Run39560945814)
	if err != nil {
		t.Fatal(err)
	}
	// The three readings of PREREG.md (6.6k, 7.3k, 8.35k).
	want := map[string]float64{"A": 6609, "B": 7329, "C": 8348}
	for _, s := range r.Scenarios {
		if got := s.FloorRPS; got < want[s.Name]-1 || got > want[s.Name]+1 {
			t.Errorf("scenario %s: floor %.0f requests/s, want %.0f", s.Name, got, want[s.Name])
		}
	}
	if r.FloorRPS != 8350 {
		t.Errorf("promotion floor %.0f requests/s, want 8350", r.FloorRPS)
	}
	if r.Period != 6500*time.Microsecond {
		t.Errorf("period %v, want 6.5ms", r.Period)
	}
	if r.CeilingRPS < 10300 || r.CeilingRPS > 10320 {
		t.Errorf("ceiling %.0f requests/s, want 67 / 6.5ms = 10308", r.CeilingRPS)
	}
	if r.CeilingOver < FloorMargin {
		t.Errorf("ceiling is %.3fx the floor, want at least %.2fx", r.CeilingOver, FloorMargin)
	}
}

// Every assumption the derivation stands on fails loudly, naming its file.
func TestAChangedControllerFailsLoudly(t *testing.T) {
	cases := []struct {
		name   string
		file   string
		edit   func(string) string
		expect string
	}{
		{"threshold 16384", "controller.go", replaceOnce(t, "largePayloadBytes: 8192.0,", "largePayloadBytes: 16384.0,"), "threshold is 16384"},
		{"threshold 4096", "controller.go", replaceOnce(t, "largePayloadBytes: 8192.0,", "largePayloadBytes: 4096.0,"), "threshold is 4096"},
		{"refusal strictly above", "controller.go", replaceOnce(t, "snap.BytesPerReq >= c.largePayloadBytes", "snap.BytesPerReq > c.largePayloadBytes"), "AT OR ABOVE"},
		{"snap path not gated", "controller.go", replaceOnce(t, "case !largePayload && cpw >= c.highWatermark:", "case cpw >= c.highWatermark:"), "single-tick snap"},
		{"sustain path not gated", "controller.go", replaceOnce(t, "case !largePayload && cpw >= c.upThreshold:", "case cpw >= c.upThreshold:"), "sustained path"},
		{"tick is 2 s", "controller.go", replaceOnce(t, "evalInterval: 1 * time.Second", "evalInterval: 2 * time.Second"), "a tick is 1 s"},
		{"sustain 3 ticks", "controller.go", replaceOnce(t, "sustainTicks:      2,", "sustainTicks:      3,"), "2 consecutive ticks"},
		{"up threshold moved", "controller.go", replaceOnce(t, "upThreshold:       24.0,", "upThreshold:       32.0,"), "24 conns per worker"},
		{"bytes are only written bytes", "telemetry.go", replaceOnce(t, "m.BytesRead+m.BytesWritten, prev.BytesRead+prev.BytesWritten", "m.BytesWritten, prev.BytesWritten"), "read plus written"},
		{"requests counted another way", "telemetry.go", replaceOnce(t, "satSub(m.RequestCount, prev.RequestCount)", "satSub(m.AcceptCount, prev.AcceptCount)"), "request count"},
		{"bytes per request another ratio", "telemetry.go", replaceOnce(t, "float64(deltaBytes) / float64(deltaReqs)", "float64(deltaBytes) / elapsed"), "byte delta over the request delta"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ReadController(celerisTree(t, map[string]func(string) string{c.file: c.edit}))
			if err == nil {
				t.Fatal("a controller that differs from the derivation's assumptions was accepted")
			}
			if !IsShapeError(err) {
				t.Fatalf("want a shape error, got %v", err)
			}
			msg := err.Error()
			if !strings.Contains(msg, c.file) || !strings.Contains(msg, c.expect) {
				t.Fatalf("the error does not name %s and %q:\n%s", c.file, c.expect, msg)
			}
		})
	}
	t.Run("a missing source", func(t *testing.T) {
		if _, err := ReadController(t.TempDir()); err == nil {
			t.Fatal("an empty tree was accepted")
		}
	})
	t.Run("two thresholds", func(t *testing.T) {
		_, err := ReadController(celerisTree(t, map[string]func(string) string{
			"controller.go": func(s string) string { return s + "\n\tlargePayloadBytes: 8192.0,\n" },
		}))
		if err == nil || !strings.Contains(err.Error(), "exactly one") {
			t.Fatalf("want an error about a second initialiser, got %v", err)
		}
	})
}

// A measurement that leaves no period in the window is an error, not a
// rounded-off guess.
func TestDeriveFailsWhenNoPeriodFits(t *testing.T) {
	m := Run39560945814
	m.CellExtraBytesPerTick = 30e6 // a higher floor, 10.65k: the longest period is under the saturating bound
	if _, err := Derive(8192, m); err == nil || !strings.Contains(err.Error(), "no period") {
		t.Fatalf("want a no-period error, got %v", err)
	}
	if _, err := Derive(700, Run39560945814); err == nil || !strings.Contains(err.Error(), "no request rate can promote") {
		t.Fatalf("want an error for a threshold under the baseline ratio, got %v", err)
	}
}

// Another measurement moves the numbers, and ApplyMatrix, ApplyPins,
// ApplyPrereg and ApplyReader carry them into every file they own.
func TestApplyCarriesTheNumbersIntoEveryFile(t *testing.T) {
	root := repoRootForTest(t)
	m := Run39560945814
	m.CellExtraBytesPerTick = 30e6
	m.SaturatingRPS = 25000
	r, err := Derive(8192, m)
	if err != nil {
		t.Fatal(err)
	}
	if r.Period != 5000*time.Microsecond || r.FloorRPS != 10650 {
		t.Fatalf("test setup: period %v floor %.0f, want 5ms and 10650", r.Period, r.FloorRPS)
	}
	const newVersion = "v1.5.12-0.20261126122750-ff7df5231102"

	matrix, err := ApplyMatrix(readFile(t, filepath.Join(root, "cmd", "validator", "matrix.go")), r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(matrix, "const raceAdaptiveMarkovMinPeriod = 5000 * time.Microsecond\n") {
		t.Error("matrix.go: the period constant was not rewritten")
	}
	for _, want := range []string{">= 10.6k", "as the floor: 10.7k.", "Chosen: 5 ms. The ceiling is 67 / 5 ms = 13.4k a second"} {
		if !strings.Contains(matrix, want) {
			t.Errorf("matrix.go: the derivation comment lacks %q", want)
		}
	}
	if _, err := parser.ParseFile(token.NewFileSet(), "matrix.go", matrix, parser.ParseComments); err != nil {
		t.Errorf("matrix.go no longer parses: %v", err)
	}
	if n := strings.Count(matrix, "Chosen: 6.5 ms"); n != 0 {
		t.Errorf("matrix.go still has %d copies of the old period", n)
	}

	pins, err := ApplyPins(readFile(t, filepath.Join(root, "cmd", "validator", "race_shape_test.go")), r, newVersion)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`pinnedCelerisVersion    = "` + newVersion + `"`,
		"pinnedMarkovMinPeriod   = 5000 * time.Microsecond",
		"pinnedPromotionFloorRPS = 10650.0",
	} {
		if !strings.Contains(pins, want) {
			t.Errorf("race_shape_test.go: the pin block lacks %q", want)
		}
	}
	if _, err := parser.ParseFile(token.NewFileSet(), "race_shape_test.go", pins, parser.ParseComments); err != nil {
		t.Errorf("race_shape_test.go no longer parses: %v", err)
	}

	prereg, err := ApplyPrereg(readFile(t, filepath.Join("testdata", "prereg-fixture.md")), r, newVersion)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{newVersion, "**10.7k requests/s**", "Period: **5 ms**", "Text before.", "Between.", "Text after."} {
		if !strings.Contains(prereg, want) {
			t.Errorf("PREREG.md lacks %q:\n%s", want, prereg)
		}
	}
	if strings.Contains(prereg, "old floor text") || strings.Contains(prereg, "old period text") {
		t.Error("PREREG.md kept the text of a generated block")
	}

	reader, err := ApplyReader(readFile(t, filepath.Join("testdata", "reader-fixture.py.txt")), r, 6500)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reader, "FLOOR = 10650.0") || strings.Contains(reader, "6500") || strings.Count(reader, "5000") != 3 {
		t.Errorf("read-shaped-cell.py not rewritten:\n%s", reader)
	}

	// Rewriting twice changes nothing.
	again, err := ApplyMatrix(matrix, r)
	if err != nil || again != matrix {
		t.Errorf("ApplyMatrix is not idempotent (err %v)", err)
	}
	again, err = ApplyPins(pins, r, newVersion)
	if err != nil || again != pins {
		t.Errorf("ApplyPins is not idempotent (err %v)", err)
	}
	again, err = ApplyPrereg(prereg, r, newVersion)
	if err != nil || again != prereg {
		t.Errorf("ApplyPrereg is not idempotent (err %v)", err)
	}
}

// A file that lost a marker or its constant is an error, never skipped.
func TestApplyRefusesAFileWithoutItsMarkers(t *testing.T) {
	r, err := Derive(8192, Run39560945814)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyPrereg("# no markers\n", r, fixtureVersion); err == nil {
		t.Error("a PREREG without markers was accepted")
	}
	if _, err := ApplyPrereg(strings.Replace(readFile(t, filepath.Join("testdata", "prereg-fixture.md")), "raceshape:period end", "raceshape:period done", 1), r, fixtureVersion); err == nil {
		t.Error("a PREREG with a missing end marker was accepted")
	}
	if _, err := ApplyMatrix("package main\n", r); err == nil {
		t.Error("a matrix.go without the constant was accepted")
	}
	root := repoRootForTest(t)
	src := readFile(t, filepath.Join(root, "cmd", "validator", "matrix.go"))
	if _, err := ApplyMatrix(strings.Replace(src, "raceshape:derived end", "raceshape:derived done", 1), r); err == nil {
		t.Error("a matrix.go without the end marker was accepted")
	}
	if _, err := ApplyReader("FLOOR = 1.0\n", r, 6500); err == nil {
		t.Error("a reader without the period was accepted")
	}
}

// The repository agrees with itself: the period constant, the test pin block and
// the celeris version of every refapp module.
func TestTheRepositoryIsConsistentWithItsPin(t *testing.T) {
	repo, err := ReadRepository(repoRootForTest(t))
	if err != nil {
		t.Fatal(err)
	}
	if p := repo.Problems(); len(p) > 0 {
		t.Fatalf("the repository disagrees with its pin:\n  %s", strings.Join(p, "\n  "))
	}
	// And the pinned numbers are what the derivation gives at the pinned
	// version: the module's own source, not the excerpt.
	dir, version, err := downloadedAt(t, repo.PinVersion)
	if err != nil {
		t.Skipf("the pinned celeris module cannot be downloaded here: %v", err)
	}
	if version != repo.PinVersion {
		t.Fatalf("downloaded %s, want %s", version, repo.PinVersion)
	}
	c, err := ReadController(dir)
	if err != nil {
		t.Fatalf("the controller at the pin: %v", err)
	}
	r, err := Derive(c.Threshold, Run39560945814)
	if err != nil {
		t.Fatal(err)
	}
	if r.Period.Microseconds() != repo.PeriodUs || r.FloorRPS != repo.PinFloor {
		t.Fatalf("at %s the derivation gives a period of %v and a floor of %.0f; the repository has %d us and %.0f: run `go run ./tools/raceshape repin %s`",
			version, r.Period, r.FloorRPS, repo.PeriodUs, repo.PinFloor, version)
	}
}

// downloadedAt fetches celeris at version through the module system.
func downloadedAt(t *testing.T, version string) (dir, got string, err error) {
	t.Helper()
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "go.mod"), "module resolve\n\ngo 1.27\n")
	cmd := exec.Command("go", "mod", "download", "-json", "github.com/goceleris/celeris@"+version)
	cmd.Dir = tmp
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOTOOLCHAIN=local")
	b, runErr := cmd.Output()
	var res struct{ Version, Dir, Error string }
	_ = json.Unmarshal(b, &res)
	if runErr != nil || res.Error != "" || res.Dir == "" {
		return "", "", &exec.Error{Name: "go mod download", Err: runErr}
	}
	return res.Dir, res.Version, nil
}

// repinWorkspace is a minimal checkout: the real matrix.go and test pin block,
// and two refapp modules at the fixture's pin.
func repinWorkspace(t *testing.T) string {
	t.Helper()
	root := repoRootForTest(t)
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "go.mod"), "module github.com/goceleris/probatorium\n\ngo 1.27\n")
	for _, f := range []string{"matrix.go", "race_shape_test.go"} {
		writeFile(t, filepath.Join(ws, "cmd", "validator", f), readFile(t, filepath.Join(root, "cmd", "validator", f)))
	}
	// Start from the numbers the fixture's controller gives, whatever the
	// repository carries now.
	c, err := ReadController(celerisTree(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	r, err := Derive(c.Threshold, Run39560945814)
	if err != nil {
		t.Fatal(err)
	}
	mp, tp := filepath.Join(ws, "cmd", "validator", "matrix.go"), filepath.Join(ws, "cmd", "validator", "race_shape_test.go")
	m, err := ApplyMatrix(readFile(t, mp), r)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, mp, m)
	p, err := ApplyPins(readFile(t, tp), r, fixtureVersion)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, tp, p)
	for _, m := range []string{"auth_session_ratelimit", "kitchen_sink"} {
		writeFile(t, filepath.Join(ws, "validation", "refapp", m, "go.mod"),
			"module example/"+m+"\n\ngo 1.27\n\nrequire (\n\tgithub.com/goceleris/celeris "+fixtureVersion+"\n)\n")
	}
	return ws
}

func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	m := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		m[p] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func withFakes(t *testing.T, tree string, version string) *[]string {
	t.Helper()
	var calls []string
	oldResolve, oldGet := resolveCeleris, runGoGet
	resolveCeleris = func(rev string) (string, string, error) { return tree, version, nil }
	runGoGet = func(modDir, v string, noBuild bool, out io.Writer) error {
		calls = append(calls, filepath.Base(modDir)+"@"+v)
		return nil
	}
	t.Cleanup(func() { resolveCeleris, runGoGet = oldResolve, oldGet })
	return &calls
}

func runRepin(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	err := run(append([]string{"repin"}, args...), &buf)
	return buf.String(), err
}

// A controller that differs from the assumptions stops the re-pin before it
// changes anything.
func TestRepinChangesNothingWhenTheShapeDiffers(t *testing.T) {
	ws := repinWorkspace(t)
	tree := celerisTree(t, map[string]func(string) string{
		"controller.go": replaceOnce(t, "largePayloadBytes: 8192.0,", "largePayloadBytes: 16384.0,"),
	})
	calls := withFakes(t, tree, "v1.5.12-0.20261126122750-ff7df5231102")
	before := snapshotTree(t, ws)
	_, err := runRepin(t, "ff7df5231102", "-repo", ws)
	if err == nil || !IsShapeError(err) {
		t.Fatalf("want a shape error, got %v", err)
	}
	if !strings.Contains(err.Error(), "8192") || !strings.Contains(err.Error(), "16384") {
		t.Errorf("the error does not name the thresholds: %v", err)
	}
	if after := snapshotTree(t, ws); !equalTrees(before, after) {
		t.Error("the re-pin changed files although the controller differs from the derivation's assumptions")
	}
	if len(*calls) != 0 {
		t.Errorf("the re-pin ran go get: %v", *calls)
	}
}

func equalTrees(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// At the current pin the re-pin finds the numbers it already carries: no text
// file changes and no module is touched.
func TestRepinAtTheCurrentPinChangesNothing(t *testing.T) {
	ws := repinWorkspace(t)
	calls := withFakes(t, celerisTree(t, nil), fixtureVersion)
	before := snapshotTree(t, ws)
	out, err := runRepin(t, fixtureVersion, "-repo", ws)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if after := snapshotTree(t, ws); !equalTrees(before, after) {
		t.Errorf("the re-pin changed files at the pin it already carries:\n%s", out)
	}
	if len(*calls) != 0 {
		t.Errorf("the re-pin ran go get at the pin it already carries: %v", *calls)
	}
	if !strings.Contains(out, "promotion floor: 8350 requests/s") || !strings.Contains(out, "period: 6.5ms") {
		t.Errorf("the re-pin did not report 8350 requests/s and 6.5ms:\n%s", out)
	}
}

// Moving the pin to another commit is one invocation: the pin block follows,
// every refapp module is moved, and the numbers stay what the controller gives.
func TestRepinMovesThePinInOneStep(t *testing.T) {
	ws := repinWorkspace(t)
	const next = "v1.5.12-0.20261126122750-ff7df5231102"
	calls := withFakes(t, celerisTree(t, nil), next)
	prereg := filepath.Join(t.TempDir(), "PREREG.md")
	writeFile(t, prereg, readFile(t, filepath.Join("testdata", "prereg-fixture.md")))

	out, err := runRepin(t, "ff7df5231102", "-repo", ws, "-prereg", prereg, "-no-build")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	pins := readFile(t, filepath.Join(ws, "cmd", "validator", "race_shape_test.go"))
	if !strings.Contains(pins, `pinnedCelerisVersion    = "`+next+`"`) {
		t.Errorf("the pin block did not follow the pin:\n%s", pins)
	}
	want := []string{"auth_session_ratelimit@" + next, "kitchen_sink@" + next}
	if strings.Join(*calls, " ") != strings.Join(want, " ") {
		t.Errorf("go get ran for %v, want %v", *calls, want)
	}
	if p := readFile(t, prereg); !strings.Contains(p, next) || !strings.Contains(p, "**8.35k requests/s**") || !strings.Contains(p, "Period: **6.5 ms**") {
		t.Errorf("PREREG.md not rewritten:\n%s", p)
	}
}

// -dry-run prints and writes nothing.
func TestRepinDryRunWritesNothing(t *testing.T) {
	ws := repinWorkspace(t)
	calls := withFakes(t, celerisTree(t, nil), "v1.5.12-0.20261126122750-ff7df5231102")
	before := snapshotTree(t, ws)
	out, err := runRepin(t, "ff7df5231102", "-repo", ws, "-dry-run")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if after := snapshotTree(t, ws); !equalTrees(before, after) {
		t.Error("a dry run changed files")
	}
	if len(*calls) != 0 {
		t.Errorf("a dry run ran go get: %v", *calls)
	}
	if !strings.Contains(out, "would run in") || !strings.Contains(out, "dry run: nothing was changed") {
		t.Errorf("a dry run did not say what it would do:\n%s", out)
	}
}

// Refapp modules on different celeris versions are an error, not a guess.
func TestRepinRefusesModulesOnDifferentPins(t *testing.T) {
	ws := repinWorkspace(t)
	writeFile(t, filepath.Join(ws, "validation", "refapp", "kitchen_sink", "go.mod"),
		"module example/kitchen_sink\n\ngo 1.27\n\nrequire (\n\tgithub.com/goceleris/celeris v1.5.12-0.20261120120335-fe2b5be71ebc\n)\n")
	withFakes(t, celerisTree(t, nil), fixtureVersion)
	if _, err := runRepin(t, fixtureVersion, "-repo", ws); err == nil || !strings.Contains(err.Error(), "not on one celeris version") {
		t.Fatalf("want a disagreement error, got %v", err)
	}
}

// The floor is the largest of the three readings, whichever it is.
func TestFloorIsTheLargestReading(t *testing.T) {
	m := Run39560945814
	m.CellBytesPerReq = 6000 // reading B (the cell's own ratio) is now the largest
	m.SaturatingRPS = 100000
	r, err := Derive(8192, m)
	if err != nil {
		t.Fatal(err)
	}
	if b := r.scenario("B").FloorRPS; r.FloorRPS < b || r.FloorRPS >= b+FloorGranularity {
		t.Errorf("floor %.0f is not reading B (%.0f) rounded up to %d", r.FloorRPS, b, FloorGranularity)
	}
	m = Run39560945814
	m.BaselineBytesPerReq = 1000 // reading A and C move; C stays the largest
	if r, err = Derive(8192, m); err != nil {
		t.Fatal(err)
	}
	if c := r.scenario("C").FloorRPS; r.FloorRPS < c || r.FloorRPS >= c+FloorGranularity {
		t.Errorf("floor %.0f is not reading C (%.0f) rounded up to %d", r.FloorRPS, c, FloorGranularity)
	}
}
