package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The 2026-10-09 bootstrap failure (celeris-stress run 37934646717): the first
// cluster run after the 2026-10-06 outage failed on all three hosts, five
// attempts each, at "Install ansible-core into a uv venv", with
//
//	error: Python installation is missing a `_sysconfigdata_` file   (msa2-*)
//	Fatal Python error: Failed to import encodings module            (msr1)
//
// Cause: /tmp on the cluster hosts is aged by systemd-tmpfiles-clean
// (`q /tmp 1777 root root 10d`, stock tmp.conf), which deletes file by file
// whatever nothing has read for ten days. The uv-managed python of 2026-09-26
// had kept only what the last runs touched -- 184 bytecode files and 4 sources
// of ~4,500 -- because Python stats a source file but reads only its bytecode.
// uv treats an existing cpython-3.13.15-* directory as installed, so every
// `uv venv` died on the husk, and the venv was already gone: the task's own
// `rm -rf` runs before it. The old cache check (a stamp that was only stat'ed,
// plus running ansible-playbook) could neither see a hole in a file nothing
// imports nor keep the files alive.
//
// So: the exact python patch is pinned, a cached venv is trusted only after
// every file of it and of its python hashes to what was recorded (which also
// READS the files, resetting their age), and a miss wipes everything the venv
// stands on instead of trusting what is lying there.

// ansibleCoreTaskScript returns the shell script of the ansible-core install
// task, de-indented, with its Jinja variables replaced by vars.
func ansibleCoreTaskScript(t *testing.T, setup string, vars map[string]string) string {
	t.Helper()
	const name = "- name: Install ansible-core into a uv venv"
	i := strings.Index(setup, name)
	if i < 0 {
		t.Fatal("runner-setup.yml has no ansible-core install task")
	}
	task := setup[i:]
	const open = "ansible.builtin.shell: |\n"
	j := strings.Index(task, open)
	if j < 0 {
		t.Fatal("the ansible-core install task is not a shell block")
	}
	body := task[j+len(open):]
	k := strings.Index(body, "\n      args:")
	if k < 0 {
		t.Fatal("cannot find the end of the ansible-core install script")
	}
	var out []string
	for _, line := range strings.Split(body[:k], "\n") {
		out = append(out, strings.TrimPrefix(line, "        "))
	}
	script := strings.Join(out, "\n") + "\n"
	script = regexp.MustCompile(`\{\{\s*([a-z_]+)\s*\}\}`).ReplaceAllStringFunc(script, func(m string) string {
		key := strings.Trim(m, "{} ")
		v, ok := vars[key]
		if !ok {
			t.Fatalf("the ansible-core install script uses {{ %s }}, which this test does not define", key)
		}
		return v
	})
	if strings.Contains(script, "{{") {
		t.Fatalf("unrendered Jinja left in the script:\n%s", script)
	}
	return script
}

func playbookPin(t *testing.T, setup, key string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^\s*` + key + `:\s*"([^"]+)"`).FindStringSubmatch(setup)
	if m == nil {
		t.Fatalf("runner-setup.yml has no %s pin", key)
	}
	return m[1]
}

func TestBootstrapPythonIsPinnedToAPatchAndRebuiltFromNothing(t *testing.T) {
	setup := readPlaybook(t, "runner-setup.yml")

	// "3.13" lets uv take any 3.13.x it knows, or reuse one already on disk.
	if py := playbookPin(t, setup, "python_version"); !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(py) {
		t.Errorf("python_version %q is not an exact patch release: with the uv pin it is what decides "+
			"which python-build-standalone build the hosts run", py)
	}

	if b := playbookPin(t, setup, "python_build"); !regexp.MustCompile(`^\d{8}$`).MatchString(b) {
		t.Errorf("python_build %q is not a python-build-standalone release tag (YYYYMMDD)", b)
	}

	script := ansibleCoreTaskScript(t, setup, map[string]string{
		"ansible_venv_dir": "/V", "ansible_core_version": "C", "uv_python_install_dir": "/P",
		"uv_cache_dir": "/U", "uv_dir": "/D", "python_version": "3.13.15",
		"ansible_deps_exclude_newer": "X", "uv_version": "0.0.0", "python_build": "20260901",
	})
	for what, want := range map[string]string{
		"a miss wipes the venv, the whole managed python tree and the uv cache together": `rm -rf "$venv" "/P" "/U"`,
		"the python is installed as the exact pin without linking into ~/.local/bin":     "/D/uv python install --no-bin 3.13.15",
		"the managed python is probed before a venv is built on it":                      "/D/uv python find --managed-python 3.13.15",
		"every file is hashed against the manifest when the cache is trusted":            `sha256sum --check --quiet --strict "$manifest"`,
		"the stamp is read, not stat'ed":                                                 `cat "$stamp"`,
		"the manifest is refused when it names too few files":                            `-lt 1000`,
		"the venv is still created from the managed python only":                         "/D/uv venv --clear --managed-python --python 3.13.15 /V",
		"the installed standalone build is compared with its pin":                        `[ "$build" != "20260901" ]`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("ansible-core install: %s (want %q)", what, want)
		}
	}
	if strings.Contains(script, `[ -f "$stamp" ]`) || strings.Contains(script, `test -f "$stamp"`) {
		t.Error("the ansible-core stamp is only stat'ed: a stat does not reset the age systemd-tmpfiles-clean " +
			"measures, so the stamp is deleted ten days after it is written even on a host that ran every day")
	}

	// The same applies to the ansible.posix stamp in the same file.
	for _, bad := range []string{
		`test -f "{{ ansible_collections_dir }}/.celeris-installed-posix`,
		`[ -f "{{ ansible_collections_dir }}/.celeris-installed-posix`,
	} {
		if strings.Contains(setup, bad) {
			t.Errorf("the ansible.posix stamp is only stat'ed (%q): read it with cat so its age is reset", bad)
		}
	}
	if !strings.Contains(setup, `cat "{{ ansible_collections_dir }}/.celeris-installed-posix{{ ansible_posix_version }}"`) {
		t.Error("the cached ansible.posix check does not read its stamp")
	}
}

// Runs the rendered task script against a small fake cache. Only the decision
// matters here -- trust the cache, or discard it and start over -- so uv is a
// stub that records its arguments and fails; reaching it IS the rebuild.
func TestAnsibleCoreCacheIsTrustedOnlyWhenEveryFileHashes(t *testing.T) {
	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("needs GNU sha256sum, which the cluster hosts and the Linux CI runners have")
	}
	setup := readPlaybook(t, "runner-setup.yml")
	core := playbookPin(t, setup, "ansible_core_version")

	type fixture struct {
		root, venv, py, cache, uvlog, script string
	}
	build := func(t *testing.T) fixture {
		t.Helper()
		root := t.TempDir()
		f := fixture{
			root:  root,
			venv:  filepath.Join(root, "venv"),
			py:    filepath.Join(root, "uv-python"),
			cache: filepath.Join(root, "uv-cache"),
			uvlog: filepath.Join(root, "uv.log"),
		}
		write := func(path, content string, mode os.FileMode) {
			t.Helper()
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(content), mode); err != nil {
				t.Fatal(err)
			}
		}
		write(filepath.Join(f.venv, "bin", "ansible-playbook"),
			"#!/bin/sh\necho 'ansible-playbook [core "+core+"]'\n", 0o755)
		write(filepath.Join(f.py, "cpython-x", "lib", "python3.13", "os.py"), "# the stdlib\n", 0o644)
		write(filepath.Join(f.py, "cpython-x", "lib", "python3.13", "_sysconfigdata__x.py"), "build_time_vars = {}\n", 0o644)
		write(filepath.Join(f.cache, "archive-v0", "wheel"), "cached wheel\n", 0o644)
		write(filepath.Join(f.root, "uvbin", "uv"), "#!/bin/sh\necho \"$@\" >> "+f.uvlog+"\nexit 97\n", 0o755)

		var manifest strings.Builder
		for _, p := range []string{
			filepath.Join(f.venv, "bin", "ansible-playbook"),
			filepath.Join(f.py, "cpython-x", "lib", "python3.13", "os.py"),
			filepath.Join(f.py, "cpython-x", "lib", "python3.13", "_sysconfigdata__x.py"),
		} {
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(b)
			manifest.WriteString(hex.EncodeToString(sum[:]) + "  " + p + "\n")
		}
		write(filepath.Join(f.venv, ".celeris-manifest"), manifest.String(), 0o644)
		write(filepath.Join(f.venv, ".celeris-installed"), "ansible-core "+core+"\n", 0o644)

		f.script = ansibleCoreTaskScript(t, setup, map[string]string{
			"ansible_venv_dir": f.venv, "ansible_core_version": core,
			"uv_python_install_dir": f.py, "uv_cache_dir": f.cache, "uv_dir": filepath.Join(root, "uvbin"),
			"python_version": playbookPin(t, setup, "python_version"), "uv_version": playbookPin(t, setup, "uv_version"),
			"ansible_deps_exclude_newer": playbookPin(t, setup, "ansible_deps_exclude_newer"),
			"python_build":               playbookPin(t, setup, "python_build"),
		})
		return f
	}
	run := func(t *testing.T, f fixture) (string, int) {
		t.Helper()
		cmd := exec.Command("bash", "-c", f.script)
		out, err := cmd.CombinedOutput()
		if err == nil {
			return string(out), 0
		}
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("run script: %v", err)
		}
		return string(out), ee.ExitCode()
	}
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }

	t.Run("intact cache is trusted and uv is never run", func(t *testing.T) {
		f := build(t)
		out, rc := run(t, f)
		if rc != 0 || !strings.Contains(out, "already cached") {
			t.Fatalf("rc=%d out=%q: an intact cache was not trusted", rc, out)
		}
		if exists(f.uvlog) {
			t.Error("uv ran although the cache was intact")
		}
		if !exists(filepath.Join(f.cache, "archive-v0", "wheel")) {
			t.Error("the trusted path deleted the uv cache")
		}
	})

	// Each of these must reach uv, having first deleted everything the venv
	// stands on. rc 97 is the stub, so a script that stopped for another reason fails.
	for name, damage := range map[string]func(t *testing.T, f fixture){
		"a python source file is gone (the 2026-10-09 state)": func(t *testing.T, f fixture) {
			if err := os.Remove(filepath.Join(f.py, "cpython-x", "lib", "python3.13", "_sysconfigdata__x.py")); err != nil {
				t.Fatal(err)
			}
		},
		"a file's content changed": func(t *testing.T, f fixture) {
			if err := os.WriteFile(filepath.Join(f.py, "cpython-x", "lib", "python3.13", "os.py"), []byte("# other  \n"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"the stamp is empty (written by the previous format)": func(t *testing.T, f fixture) {
			if err := os.WriteFile(filepath.Join(f.venv, ".celeris-installed"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"the stamp is gone (aged out)": func(t *testing.T, f fixture) {
			if err := os.Remove(filepath.Join(f.venv, ".celeris-installed")); err != nil {
				t.Fatal(err)
			}
		},
		"the manifest is gone": func(t *testing.T, f fixture) {
			if err := os.Remove(filepath.Join(f.venv, ".celeris-manifest")); err != nil {
				t.Fatal(err)
			}
		},
		"ansible-playbook reports another core": func(t *testing.T, f fixture) {
			// Rewrite the script AND the manifest so only the probe can object.
			p := filepath.Join(f.venv, "bin", "ansible-playbook")
			body := []byte("#!/bin/sh\necho 'ansible-playbook [core 0.0.0]'\n")
			if err := os.WriteFile(p, body, 0o755); err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(body)
			m := filepath.Join(f.venv, ".celeris-manifest")
			old, _ := os.ReadFile(m)
			lines := strings.Split(strings.TrimSpace(string(old)), "\n")
			lines[0] = hex.EncodeToString(sum[:]) + "  " + p
			if err := os.WriteFile(m, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run("rebuilds from nothing when "+name, func(t *testing.T) {
			f := build(t)
			damage(t, f)
			out, rc := run(t, f)
			if rc != 97 {
				t.Fatalf("rc=%d (want 97, the uv stub); out=%q: the damaged cache was trusted, or the script died elsewhere", rc, out)
			}
			log, _ := os.ReadFile(f.uvlog)
			if !strings.HasPrefix(string(log), "python install --no-bin "+playbookPin(t, readPlaybook(t, "runner-setup.yml"), "python_version")) {
				t.Errorf("the first uv call was %q, want the exact-pin python install", log)
			}
			for _, p := range []string{f.venv, f.py, f.cache} {
				if exists(p) {
					t.Errorf("%s survived the miss: a damaged managed python or an aged uv cache would be reused", p)
				}
			}
		})
	}
}
