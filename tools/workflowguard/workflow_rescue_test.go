package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

const rescueScript = "ops/rescue/celeris-rescue-results"

// The teardown's purge is what turned the 20 h loss in probatorium#354 into a
// total one: cleanup.yml --extra-vars purge_results=true rm -rf's
// results_root, which on these hosts is tmpfs and holds the only document a
// resumed run can read. Both properties below are properties of hand-edited
// YAML and both fail silently — the teardown still succeeds, the artifact is
// still produced, and you find out the next time a soak is lost.

func TestResultsAreRescuedBeforeTheyArePurged(t *testing.T) {
	src := mustRead(t, downAction)

	rescue := mustIndex(t, src, "name: Rescue in-flight results off tmpfs", "results rescue step")
	upload := mustIndex(t, src, "name: Upload rescued results", "rescued-results upload step")
	reclaim := mustIndex(t, src, "name: Reclaim cluster host state", "cleanup.yml step")
	teardown := mustIndex(t, src, "name: Run ansible runner-teardown", "runner-teardown.yml step")

	if rescue > reclaim {
		t.Errorf("the results rescue runs AFTER cleanup.yml (offsets %d > %d); cleanup.yml purges "+
			"results_root, so there would be nothing left to resume from", rescue, reclaim)
	}
	if rescue > teardown {
		t.Errorf("the results rescue runs AFTER runner-teardown.yml (offsets %d > %d)", rescue, teardown)
	}
	if upload < rescue {
		t.Errorf("the upload step precedes the rescue it uploads (offsets %d < %d)", upload, rescue)
	}
	if upload > reclaim {
		t.Errorf("the rescued-results upload runs after reclamation (offsets %d > %d); keep it "+
			"adjacent to the rescue so a later step failing cannot cost the artifact", upload, reclaim)
	}
	// The run this exists for is the one where the matrix job died.
	for _, step := range []string{
		"name: Rescue in-flight results off tmpfs",
		"name: Upload rescued results",
	} {
		i := mustIndex(t, src, step, "rescue step")
		// The `if:` sits within a couple of lines of the name in this file's
		// style (an `id:` can come between); take a window around the header.
		if !strings.Contains(src[max(i-200, 0):min(i+200, len(src))], "if: always()") {
			t.Errorf("step %q is not guarded with `if: always()`; it would be skipped on exactly "+
				"the runs it exists for", step)
		}
	}
}

// A purge hardcoded to `true` destroys the resume material whether or not the
// rescue worked. It has to be the rescue's verdict.
func TestThePurgeIsConditionalOnTheRescue(t *testing.T) {
	src := mustRead(t, downAction)
	if strings.Contains(src, `--extra-vars "purge_results=true"`) {
		t.Error("the teardown still drives cleanup.yml with a hardcoded purge_results=true; a " +
			"rescue that failed must leave the partial run on the host (probatorium#376)")
	}
	reclaim := mustIndex(t, src, "name: Reclaim cluster host state", "cleanup.yml step")
	end := min(reclaim+1600, len(src))
	block := src[reclaim:end]
	if !strings.Contains(block, "steps.rescue.outputs.purge_results") {
		t.Errorf("the reclamation step does not read the rescue step's verdict; it cannot know "+
			"whether results_root has been copied anywhere. Block:\n%s", block)
	}
}

// walkTree fingerprints a directory: relative path -> sha256 of the contents.
// Used to prove the rescue is a COPY, byte for byte, and that nothing under
// results_root is touched — including by the retention prune.
func walkTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if info.IsDir() {
			out[rel+"/"] = "dir"
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		out[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

func sameTree(a, b map[string]string) bool {
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

// stageResultsRoot builds a results_root shaped like the one a 20 h soak
// leaves behind: one run directory with a current validate-results.json and
// per-cell subdirectories, plus one that never got far enough to have a
// document at all.
func stageResultsRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	run := filepath.Join(root, "20260913T040535-validate-msr1-soak")
	if err := os.MkdirAll(filepath.Join(run, "cell-00-kitchen_sink-iouring"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(p, s string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(run, "validate-results.json"),
		`{"schema_version":"5.13","validation":{"cells":[{"refapp":"kitchen_sink","engine":"iouring"}]}}`)
	write(filepath.Join(run, "streaming-coverage.txt"), "kitchen_sink 1 /ws routed\n")
	write(filepath.Join(run, "cell-00-kitchen_sink-iouring", "series.jsonl"), "{\"t\":1}\n")

	bare := filepath.Join(root, "20260913T040535-validate-msr1-nodoc")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(bare, "validator.stdout"), "matrix: 64 cells\n")
	return root
}

func runRescue(t *testing.T, resultsRoot, rescueRoot, label, keep string) (string, int) {
	t.Helper()
	cmd := exec.Command("sh", rescueScript, resultsRoot, rescueRoot, label, keep)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("run %s: %v\n%s", rescueScript, err, out)
		}
		code = ee.ExitCode()
	}
	return string(out), code
}

// The rescue must produce the resume material AND leave the thing it is
// rescuing exactly as it found it. A rescue that moved, chmod'd or truncated
// results_root would be a second way to lose the run.
func TestRescueCopiesTheResultsAndNamesTheResumePath(t *testing.T) {
	results := stageResultsRoot(t)
	before := walkTree(t, results)
	rescueRoot := filepath.Join(t.TempDir(), "celeris-rescue")

	out, code := runRescue(t, results, rescueRoot, "matrix=failure (lost runner)", "3")
	if code != 0 {
		t.Fatalf("rescue exited %d:\n%s", code, out)
	}
	if !sameTree(before, walkTree(t, results)) {
		t.Fatalf("results_root changed; the rescue must be a pure copy:\n%s", out)
	}

	resumeList := filepath.Join(rescueRoot, "last-resume-from.txt")
	b, err := os.ReadFile(resumeList)
	if err != nil {
		t.Fatalf("no %s: %v\n%s", resumeList, err, out)
	}
	lines := strings.Fields(strings.TrimSpace(string(b)))
	if len(lines) != 1 {
		t.Fatalf("want exactly one resume candidate (only one run dir has a document), got %v", lines)
	}
	// The path named must be a directory that a resume can actually read.
	doc := filepath.Join(lines[0], "validate-results.json")
	if _, err := os.Stat(doc); err != nil {
		t.Fatalf("the resume path %q does not hold a validate-results.json: %v", lines[0], err)
	}
	if !strings.HasSuffix(lines[0], "20260913T040535-validate-msr1-soak") {
		t.Errorf("resume candidate names the wrong run dir: %s", lines[0])
	}
	// Per-cell material comes along; a rescue of the document alone would
	// throw away the forensics of the very run that failed.
	if _, err := os.Stat(filepath.Join(lines[0], "cell-00-kitchen_sink-iouring", "series.jsonl")); err != nil {
		t.Errorf("per-cell material was not rescued: %v", err)
	}
	// The run directory with no document is RECORDED, not silently dropped.
	manifest := mustRead(t, filepath.Join(filepath.Dir(lines[0]), "rescue-manifest.tsv"))
	if !strings.Contains(manifest, "nodoc") || !strings.Contains(manifest, "\tno\t") {
		t.Errorf("the run dir with no document is missing from the manifest:\n%s", manifest)
	}
	// And the small bundle the teardown fetches into the run's artifact.
	if _, err := os.Stat(filepath.Join(rescueRoot, "last-rescue-documents.tar.gz")); err != nil {
		t.Errorf("no document bundle for the artifact: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rescueRoot, "last-rescue.json")); err != nil {
		t.Errorf("no last-rescue.json: %v", err)
	}
}

// Retention is the price of putting anything on these hosts' real storage --
// this cluster has already lost one NVMe to writes it did not need to be
// doing. It must bound the rescue directory and touch nothing else.
func TestRescueRetentionPrunesOnlyItsOwnDirectory(t *testing.T) {
	results := stageResultsRoot(t)
	before := walkTree(t, results)
	rescueRoot := filepath.Join(t.TempDir(), "celeris-rescue")
	if err := os.MkdirAll(rescueRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-72 * time.Hour)
	for i := range 5 {
		d := filepath.Join(rescueRoot, "2026090"+string(rune('1'+i))+"T000000Z")
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		when := base.Add(time.Duration(i) * time.Hour)
		if err := os.Chtimes(d, when, when); err != nil {
			t.Fatal(err)
		}
	}
	out, code := runRescue(t, results, rescueRoot, "retention", "3")
	if code != 0 {
		t.Fatalf("rescue exited %d:\n%s", code, out)
	}
	entries, err := os.ReadDir(rescueRoot)
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Strings(dirs)
	if len(dirs) != 3 {
		t.Errorf("keep=3 left %d rescue dirs: %v\n%s", len(dirs), dirs, out)
	}
	if !sameTree(before, walkTree(t, results)) {
		t.Fatal("the retention prune changed results_root")
	}
}

func TestRescueRefusesADangerousDestination(t *testing.T) {
	results := stageResultsRoot(t)
	for _, dest := range []string{"/", "/tmp", "/var/lib", "relative-path"} {
		out, code := runRescue(t, results, dest, "danger", "3")
		if code != 2 {
			t.Errorf("rescue_root=%q exited %d, want 2 (refused):\n%s", dest, code, out)
		}
	}
}

// Same hard constraint as the monitoring scripts, narrowed to what a rescue
// legitimately needs: it writes, so `rm` and `cp` are allowed, but nothing it
// does may be able to stop a process on a host that is still running one.
func TestRescueCannotDisturbAHost(t *testing.T) {
	src := stripComments(mustRead(t, rescueScript)) + "\n" + stripComments(mustRead(t, "ansible/rescue-results.yml"))
	forbidden := regexp.MustCompile(
		`(?m)(^|[;&|(]|\$\()[[:space:]]*(kill|pkill|killall|reboot|shutdown|halt|poweroff|docker|sysctl|service)([[:space:]]|$)`)
	if m := forbidden.FindString(src); m != "" {
		t.Errorf("the rescue invokes %q as a command; it runs on a host that may still be "+
			"measuring something", strings.TrimSpace(m))
	}
	if m := regexp.MustCompile(`systemctl[[:space:]]+(start|stop|restart|reload|kill|enable|disable)\b`).
		FindString(src); m != "" {
		t.Errorf("the rescue changes unit state: %q", m)
	}
	// The one destructive verb it does use must never be aimed at the results.
	for _, line := range strings.Split(src, "\n") {
		if !strings.Contains(line, "rm -") && !strings.Contains(line, "rmdir") {
			continue
		}
		if strings.Contains(line, "RESULTS_ROOT") || strings.Contains(line, "results_root") {
			t.Errorf("the rescue removes something under the results root: %q", strings.TrimSpace(line))
		}
	}
}
