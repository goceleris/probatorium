package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// probatorium#387: the cluster bootstrap fetched a 215 MiB actions-runner
// tarball per host on every single run, because the `force: false` guard
// that was supposed to skip an existing download pointed at a path inside
// the directory the preceding "Wipe stale runner dir" task had just
// deleted. The file could never be there, so the guard was dead code.
//
// On 2026-09-15 that download took 13m05s and cancelled the nightly's
// 15-minute bootstrap job; the historical norm for the whole bootstrap is
// under three minutes. The wipe is correct and stays -- its own comment
// explains why (a stale .runner pins a registration GitHub has deleted) --
// but that argument is about the runner CONFIGURATION, not about bytes
// GitHub returns byte-identical for a version-addressed filename.
//
// So the cache has to live outside runner_root, and teardown has to leave
// it alone. Both are one-line edits away from silently reverting to a
// re-download nobody would notice until a slow day cancelled another run.
func TestRunnerTarballIsCachedOutsideTheWipedDir(t *testing.T) {
	setup := readPlaybook(t, "runner-setup.yml")

	cacheVar := regexp.MustCompile(`(?m)^\s*runner_cache_dir:\s*"([^"]+)"`)
	m := cacheVar.FindStringSubmatch(setup)
	if m == nil {
		t.Fatal("runner-setup.yml declares no runner_cache_dir: the tarball has " +
			"nowhere to survive the wipe, so every run re-downloads 215 MiB per host")
	}
	cacheDir := m[1]

	rootVar := regexp.MustCompile(`(?m)^\s*runner_root:\s*"([^"]+)"`)
	rm := rootVar.FindStringSubmatch(setup)
	if rm == nil {
		t.Fatal("runner-setup.yml declares no runner_root")
	}
	root := rm[1]

	// runner_root interpolates the hostname, so compare on the literal
	// prefix before the first template expression.
	rootPrefix := root
	if i := strings.Index(rootPrefix, "{{"); i >= 0 {
		rootPrefix = rootPrefix[:i]
	}
	if strings.HasPrefix(cacheDir, rootPrefix) {
		t.Errorf("runner_cache_dir %q sits under runner_root %q: the wipe deletes it "+
			"and force:false becomes dead code again", cacheDir, root)
	}
	if !strings.HasPrefix(cacheDir, "/tmp/") {
		t.Errorf("runner_cache_dir %q is outside /tmp: the playbook's "+
			"pristine-by-design rule is that nothing lands elsewhere", cacheDir)
	}

	// The download itself must target the cache, not the runner dir.
	dl := regexp.MustCompile(`(?s)get_url:.*?dest:\s*"([^"]+)"`)
	dests := dl.FindAllStringSubmatch(setup, -1)
	if len(dests) == 0 {
		t.Fatal("runner-setup.yml has no get_url task")
	}
	for _, d := range dests {
		if !strings.Contains(d[1], "runner_cache_dir") {
			t.Errorf("a get_url writes to %q instead of the cache: that download "+
				"repeats on every run", d[1])
		}
	}

	// And teardown must not take the cache with it.
	teardown := readPlaybook(t, "runner-teardown.yml")
	if strings.Contains(teardown, "runner_cache_dir") {
		t.Error("runner-teardown.yml references runner_cache_dir: teardown removes " +
			"paths, and removing the cache restores the per-run download")
	}
}

// A cold cache still has 215 MiB to move, three times over. Fifteen minutes
// was never sized for that -- it left the race tier of 2026-09-15 with 2m40s
// of margin and gave the nightly none at all.
func TestClusterBootstrapHasHeadroomForAColdCache(t *testing.T) {
	const floor = 20

	entries, err := os.ReadDir(filepath.Join(".github", "workflows"))
	if err != nil {
		t.Fatalf("read workflows: %v", err)
	}

	// The bootstrap job is identified by its display name, then the first
	// timeout-minutes at job indentation after it.
	jobTimeout := regexp.MustCompile(
		`(?s)name: bootstrap cluster runners\n.*?\n    timeout-minutes: (\d+)\n`)

	checked := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml") {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(".github", "workflows", e.Name()))
		if rerr != nil {
			t.Errorf("%s: %v", e.Name(), rerr)
			continue
		}
		m := jobTimeout.FindStringSubmatch(string(b))
		if m == nil {
			continue // not a cluster workflow
		}
		checked++
		got, cerr := strconv.Atoi(m[1])
		if cerr != nil {
			t.Errorf("%s: unparseable timeout-minutes %q", e.Name(), m[1])
			continue
		}
		if got < floor {
			t.Errorf("%s: bootstrap timeout-minutes=%d, want >= %d -- a cold cache "+
				"moves 215 MiB per host and 15 cancelled nightly run 34971158913",
				e.Name(), got, floor)
		}
	}

	// Guarding nothing would pass silently.
	if checked == 0 {
		t.Error("found no bootstrap job in any workflow: this guard is vacuous")
	}
	t.Logf("checked %d cluster workflow(s)", checked)
}

func readPlaybook(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("ansible", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}
