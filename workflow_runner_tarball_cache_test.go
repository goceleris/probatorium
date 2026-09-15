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

// probatorium#392: #388 cached the runner tarball, but the same playbook
// fetched four more tools on every run into the directory it wipes -- uv, a
// python build, ansible-core and ansible.posix -- behind skip-if-present
// guards that therefore never skipped, with no retries, and with ansible-core
// and ansible.posix unpinned. A Galaxy timeout on one host failed nightly
// 34981852841 although the other two hosts had already registered.
//
// Pinning comes first: caching an unpinned install silently freezes whichever
// version happened to install first, with nothing recording which. Keying
// each cache directory by its pin is what makes a bump install fresh.
//
// Six cluster workflows reach the venv and the collections through hardcoded
// runner_root paths, not through anything the playbook exports, so the
// playbook keeps those two paths alive as symlinks into the cache. A path
// written in one file and consumed in six breaks silently, so the contract is
// asserted here from both ends.
func TestBootstrapToolchainIsPinnedAndCachedOutsideTheWipedDir(t *testing.T) {
	setup := readPlaybook(t, "runner-setup.yml")

	for _, p := range []string{"uv_version", "python_version", "ansible_core_version", "ansible_posix_version"} {
		re := regexp.MustCompile(`(?m)^\s*` + p + `:\s*"\d+\.\d+(\.\d+)?"\s*$`)
		if !re.MatchString(setup) {
			t.Errorf("%s is not pinned to a concrete version: an unpinned tool that is also "+
				"cached freezes whichever version installed first, with nothing recording which", p)
		}
	}

	for what, want := range map[string]string{
		"uv installer":              "astral.sh/uv/{{ uv_version }}/install.sh",
		"venv python":               "--python {{ python_version }}",
		"ansible-core":              "ansible-core=={{ ansible_core_version }}",
		"ansible.posix from Galaxy": "ansible.posix:=={{ ansible_posix_version }}",
		"ansible.posix from GitHub": "ansible.posix.git,{{ ansible_posix_commit }}",
	} {
		if !strings.Contains(setup, want) {
			t.Errorf("%s does not install its pin (want %q in runner-setup.yml)", what, want)
		}
	}

	// No tool may keep its install target or its skip guard under runner_root.
	for _, f := range []string{
		"{{ runner_root }}/uv/",
		"--collections-path {{ runner_root }}",
		`creates: "{{ runner_root }}/ansible-venv`,
		`creates: "{{ runner_root }}/ansible-collections`,
	} {
		if strings.Contains(setup, f) {
			t.Errorf("runner-setup.yml still has %q: that target is inside the directory the "+
				"bootstrap wipes, so its skip guard can never skip and the fetch repeats every run", f)
		}
	}

	for _, want := range []string{
		`tool_cache_dir: "{{ runner_cache_dir }}/`,
		`uv_dir: "{{ tool_cache_dir }}/`,
		`ansible_venv_dir: "{{ tool_cache_dir }}/`,
		`ansible_collections_dir: "{{ tool_cache_dir }}/`,
	} {
		if !strings.Contains(setup, want) {
			t.Errorf("want %q: every cached tool must live under runner_cache_dir, which "+
				"TestRunnerTarballIsCachedOutsideTheWipedDir already keeps teardown away from", want)
		}
	}

	// Without --managed-python uv prefers a matching python already on PATH
	// over downloading into UV_PYTHON_INSTALL_DIR, so the cached venv would
	// depend on a python outside the cache. An end-to-end run caught it.
	if !strings.Contains(setup, "uv venv --clear --managed-python") {
		t.Error("the ansible venv is not created with --managed-python: uv may build it on a " +
			"python found on PATH, outside the cache, and the cached venv breaks when that python goes")
	}

	// A cache that skips on presence alone turns one bad install into every
	// later run's problem, so ansible.posix is checked against its manifest.
	if !strings.Contains(setup, "MANIFEST.json") {
		t.Error("ansible.posix has no integrity check against its MANIFEST.json: a partial " +
			"or mismatched cached collection would be skipped as present, forever")
	}

	// The six-workflow contract, from both ends.
	links := map[string]string{
		"ansible-venv":        `src: "{{ ansible_venv_dir }}", dest: "{{ runner_root }}/ansible-venv"`,
		"ansible-collections": `src: "{{ ansible_collections_dir }}", dest: "{{ runner_root }}/ansible-collections"`,
	}
	consumers := map[string]int{}
	entries, err := os.ReadDir(filepath.Join(".github", "workflows"))
	if err != nil {
		t.Fatalf("read workflows: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml") {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(".github", "workflows", e.Name()))
		if rerr != nil {
			t.Errorf("%s: %v", e.Name(), rerr)
			continue
		}
		for path := range links {
			if strings.Contains(string(b), "${RUNNER_ROOT}/"+path) {
				consumers[path]++
			}
		}
	}
	for path, want := range links {
		if consumers[path] == 0 {
			t.Errorf("no workflow reads ${RUNNER_ROOT}/%s any more: this contract check is "+
				"guarding nothing, so update or delete it", path)
			continue
		}
		if !strings.Contains(setup, want) || !strings.Contains(setup, "state: link") {
			t.Errorf("%d workflow(s) read ${RUNNER_ROOT}/%s but runner-setup.yml does not link it "+
				"into the cache (want %q): every one of those tiers loses its ansible", consumers[path], path, want)
		}
	}
	t.Logf("toolchain contract: %d workflow(s) read ansible-venv, %d read ansible-collections",
		consumers["ansible-venv"], consumers["ansible-collections"])
}

// probatorium#393 review: a cache that skips on presence is a liability, and the
// first cut still skipped on presence in three places. Ansible's `creates:`
// checks with glob.glob, which counts a DANGLING bin/python symlink as present;
// uv writes a wheel's entry-point scripts before its package data, metadata and
// RECORD, so bin/ansible-playbook can exist over a half-finished install; and
// ansible-galaxy writes MANIFEST.json before any collection file, so a manifest
// naming the pin proves nothing about the files after it. A job killed by the
// bootstrap timeout leaves exactly those states, and the cache now outlives the
// run that left them.
//
// So each cached tool is trusted only when a stamp written after a verified
// install exists AND a check that exercises the tool agrees: the venv is probed
// by running ansible-playbook, and ansible.posix by verifying every file's hash.
func TestBootstrapToolchainCacheIsTrustedOnlyWhenVerified(t *testing.T) {
	setup := readPlaybook(t, "runner-setup.yml")

	for name, re := range map[string]*regexp.Regexp{
		"ansible_deps_exclude_newer":  regexp.MustCompile(`(?m)^\s*ansible_deps_exclude_newer:\s*"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z"\s*$`),
		"ansible_posix_commit":        regexp.MustCompile(`(?m)^\s*ansible_posix_commit:\s*"[0-9a-f]{40}"\s*$`),
		"galaxy_attempt_timeout":      regexp.MustCompile(`(?m)^\s*galaxy_attempt_timeout:\s*\d+\s*$`),
		"galaxy_git_fallback_timeout": regexp.MustCompile(`(?m)^\s*galaxy_git_fallback_timeout:\s*\d+\s*$`),
	} {
		if !re.MatchString(setup) {
			t.Errorf("%s is not set to a concrete value", name)
		}
	}

	for what, want := range map[string]string{
		"ansible-core dependencies frozen at a cutoff":              "--exclude-newer {{ ansible_deps_exclude_newer }}",
		"the cutoff keys the venv directory":                        `-core{{ ansible_core_version }}-deps{{ ansible_deps_exclude_newer`,
		"venv trusted only with a completion stamp":                 "{{ ansible_venv_dir }}/.celeris-installed",
		"venv trusted only if ansible-playbook runs":                "[core {{ ansible_core_version }}]",
		"ansible.posix verified file by file":                       "collection verify --offline",
		"ansible.posix trusted only with a stamp":                   ".celeris-installed-posix{{ ansible_posix_version }}",
		"each Galaxy attempt is bounded":                            "timeout {{ galaxy_attempt_timeout }}",
		"the GitHub fallback is bounded":                            "timeout {{ galaxy_git_fallback_timeout }}",
		"ansible-galaxy keeps its temp and cache in the tool cache": `ANSIBLE_HOME: "{{ ansible_home_dir }}"`,
		"ANSIBLE_HOME lives in the tool cache":                      `ansible_home_dir: "{{ tool_cache_dir }}/`,
	} {
		if !strings.Contains(setup, want) {
			t.Errorf("%s: want %q in runner-setup.yml", what, want)
		}
	}

	// A presence guard on the venv is exactly what the review caught.
	if strings.Contains(setup, `creates: "{{ ansible_venv_dir }}`) {
		t.Error("the venv is still guarded by creates:, which glob.glob satisfies with a dangling " +
			"bin/python symlink and with a half-installed bin/ansible-playbook")
	}

	// force: true on the link task switches off ansible's only check that the
	// link target exists, so a missing cache dir would become a dangling link
	// and a WARNING instead of a failure.
	const linkTask = "- name: Point the legacy runner_root tool paths at the cache"
	i := strings.Index(setup, linkTask)
	if i < 0 {
		t.Fatal("runner-setup.yml has no link task to check")
	}
	task := setup[i+len(linkTask):]
	if j := strings.Index(task, "\n    - name:"); j >= 0 {
		task = task[:j]
	}
	// Match force as a YAML key, not as text: the task's own comment explains
	// why there is no force, and a substring check tripped on that sentence.
	// Any truthy spelling counts; ansible accepts yes/on/true in any case.
	forceKey := regexp.MustCompile(`(?mi)^\s*force:\s*(true|yes|on)\s*$`)
	if forceKey.MatchString(task) {
		t.Error("the link task sets force: true, which turns off the check that its target exists")
	}
}

// ansible-galaxy collection verify --offline checks every file against the
// hashes recorded in FILES.json, but it does not hash MANIFEST.json itself.
// The round-3 end-to-end run measured the consequence: a cached manifest whose
// version had been altered to 0.0.0 passed the cached check, and the play
// reported success over it. So the cached check must compare the manifest's
// version to the pin as well as run verify.
func TestCachedAnsiblePosixCheckComparesTheManifestVersion(t *testing.T) {
	setup := readPlaybook(t, "runner-setup.yml")
	const name = "- name: Check the cached ansible.posix is complete and the pinned version"
	i := strings.Index(setup, name)
	if i < 0 {
		t.Fatal("runner-setup.yml has no cached ansible.posix check")
	}
	task := setup[i+len(name):]
	if j := strings.Index(task, "\n        - name:"); j >= 0 {
		task = task[:j]
	}
	for what, want := range map[string]string{
		"the completion stamp":                 ".celeris-installed-posix{{ ansible_posix_version }}",
		"a file-by-file hash check":            "collection verify --offline",
		"the manifest version against the pin": `["collection_info"]["version"] != sys.argv[2]`,
	} {
		if !strings.Contains(task, want) {
			t.Errorf("the cached ansible.posix check lacks %s (want %q): verify --offline does not hash "+
				"MANIFEST.json, so a manifest naming the wrong version passes it", what, want)
		}
	}
}
