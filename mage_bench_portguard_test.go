//go:build mage

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/goceleris/probatorium/servers"
)

// THROWAWAY (ab/c1-harness-h7fix-20261105, not for merge). Review r5 P6: run_bench_cell.yml decides whether the listener on the bench port is OUR binary by matching its live command line
// against competitors/<bin>. As a bare substring "competitors/celeris" also matches a lingering competitors/celeris_v158 (and _9f4d89b, _v1511). The fix ends the token with a space. It is
// only safe if EVERY command line that contains "competitors/" has competitors/<bin> followed by a space. This test derives that from the real launch machinery, for every registry column:
//
//   - the launch line of the playbook (./competitors/<bin> -bind ADDR $ENGINE_ARG) is read from run_bench_cell.yml and every native-shape fixture is built from it;
//   - the language-runtime classes (bun, node, java, python) exec a runtime from a launcher: the exec lines of the roles are read and must not contain competitors/ (they reach
//     competitors-src or python-venvs through readlink -f of the symlink, or a hard-coded venv), so those listeners never reach the binary match at all (the playbook accepts them loosely);
//   - the real probe line is extracted from the playbook and run in bash on all (column x column) pairs: own binary accepted, any other binary rejected, in particular the
//     celeris / celeris_v158 / celeris_9f4d89b / celeris_v1511 family in both directions.
func TestPortOwnershipProbeAcceptsOnlyTheLaunchedBinaryForEveryColumn(t *testing.T) {
	y := readRepoFile(t, "ansible/tasks/run_bench_cell.yml")
	launch := regexp.MustCompile(`\./competitors/\{\{ cell_bin \}\} -bind 0\.0\.0\.0:\{\{ bench_port \| default\(8080\) \}\} \$ENGINE_ARG`)
	if !launch.MatchString(y) {
		t.Fatal("the launch line of the playbook changed shape: re-derive the fixtures (and the probe) from it")
	}
	if !strings.Contains(y, `ENGINE_ARG="{{ ('-engine ' + cell_engine) if cell_engine else '' }}"`) {
		t.Fatal("the -engine argument of the launch line changed shape")
	}
	probe := regexp.MustCompile(`(?m)^\s*if printf '%s' "\$cmd" \| grep -qF -- "\$want_bin( ?)"; then$`).FindStringSubmatch(y)
	if probe == nil {
		t.Fatal("the binary match of the probe is gone or changed shape")
	}
	if probe[1] != " " {
		t.Fatal(`the binary match is a bare substring: competitors/celeris would accept a leftover competitors/celeris_v158; end the path token with a space`)
	}
	// the runtime classes never put competitors/ into their live command line
	for _, f := range []string{"ansible/roles/java/tasks/build_competitor.yml", "ansible/roles/node/tasks/build_competitor.yml", "ansible/roles/bun/tasks/build_competitor.yml"} {
		src := readRepoFile(t, f)
		if !strings.Contains(src, `readlink -f "$0"`) {
			t.Errorf("%s: the launcher no longer resolves its symlink with readlink -f: its runtime argv would name competitors/", f)
		}
		ex := regexp.MustCompile(`(?m)^\s*exec .*$`).FindAllString(src, -1)
		if len(ex) == 0 {
			t.Errorf("%s: no exec line", f)
		}
		for _, l := range ex {
			if strings.Contains(l, "competitors/") || !strings.Contains(l, "$SCRIPT_DIR") && !strings.Contains(l, "$JAR") {
				t.Errorf("%s: exec line %q reaches the sources other than through the resolved SCRIPT_DIR/JAR", f, strings.TrimSpace(l))
			}
		}
	}
	py := readRepoFile(t, "ansible/roles/python/templates/launcher.sh.j2")
	for _, l := range regexp.MustCompile(`(?m)^\s*exec "\$\{VENV_DIR\}.*$`).FindAllString(py, -1) {
		if strings.Contains(l, "competitors/") {
			t.Errorf("python launcher exec line %q names competitors/", l)
		}
	}
	if !strings.Contains(py, `VENV_DIR="{{ competitor_venv_path }}"`) || !strings.Contains(readRepoFile(t, "ansible/roles/python/tasks/build_competitor.yml"), `competitor_venv_path: "{{ bench_root }}/python-venvs/{{ competitor_name }}"`) {
		t.Error("the python venv moved: re-check that its argv does not name competitors/")
	}

	// fixtures for every column, from the registry exactly as mage hands it to the playbook
	cols, err := resolveBenchColumns("all")
	if err != nil {
		t.Fatal(err)
	}
	type fx struct{ bin, cmd string }
	bins := map[string]fx{}
	runtime := map[string]bool{}
	for _, c := range cols {
		lang := "go"
		if nb, ok := servers.Registry[c.Slug].Bin.(servers.NativeBinary); ok {
			lang = nb.Lang
		}
		switch lang {
		case "bun", "node", "java", "python":
			runtime[c.Bin] = true
			continue
		}
		cmd := "/opt/bench/competitors/" + c.Bin + " -bind 0.0.0.0:8080 "
		if c.Engine != "" {
			cmd += "-engine " + c.Engine + " "
		}
		if _, ok := bins[c.Bin]; !ok || c.Engine == "" {
			bins[c.Bin] = fx{c.Bin, cmd}
		}
	}
	for _, must := range []string{"celeris", "celeris_v1511", "celeris_v158", "celeris_9f4d89b"} {
		if _, ok := bins[must]; !ok {
			t.Fatalf("no native-shape fixture for %s: the family the fix exists for is not covered", must)
		}
	}
	names := make([]string, 0, len(bins))
	for b := range bins {
		names = append(names, b)
	}
	sort.Strings(names)
	var in strings.Builder
	type pair struct {
		want, holder string
		ok           bool
	}
	var pairs []pair
	for _, w := range names {
		for _, h := range names {
			pairs = append(pairs, pair{w, h, w == h})
			in.WriteString(w + "\t" + bins[h].cmd + "\n")
		}
	}
	script := `while IFS=$'\t' read -r bin cmd; do want_bin="competitors/$bin"; if printf '%s' "$cmd" | grep -qF -- "$want_bin "; then echo Y; else echo N; fi; done`
	cmd := exec.Command(needBash(t), "-c", script)
	cmd.Stdin = strings.NewReader(in.String())
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Fields(string(out))
	if len(got) != len(pairs) {
		t.Fatalf("%d verdicts for %d pairs", len(got), len(pairs))
	}
	for i, p := range pairs {
		if (got[i] == "Y") != p.ok {
			t.Errorf("want %s, holder %s: accepted=%v, want %v", p.want, p.holder, got[i] == "Y", p.ok)
		}
	}
	// the probe line used above is the one in the playbook: run it verbatim for the family the fix exists for
	line := `printf '%s' "$cmd" | grep -qF -- "$want_bin "`
	if !strings.Contains(y, line) {
		t.Fatal("the probe line differs from the one executed above")
	}
	t.Logf("%d native-shape binaries, %d runtime-class binaries (never reach the match), %d pairs", len(names), len(runtime), len(pairs))
}

// probeScript extracts the WHOLE port-ownership probe from the playbook (the shell block of the "wait for OUR server to own the bench port" task), renders its Jinja inputs for one column and
// points /proc at a fixture tree, so the test runs the real binary match, engine match, env match and listener walk, not a copy of one line (review r6 F4: mutations of want_bin, of the
// cmdline separator and of the -engine token boundary stayed green when only the token line was run).
func probeScript(t *testing.T, y, bin, engine, wantEnv string) string {
	t.Helper()
	i := strings.Index(y, `| wait for OUR server to own the bench port"`)
	if i < 0 {
		t.Fatal("the probe task is gone")
	}
	blk := y[i:]
	const head = "ansible.builtin.shell: |\n"
	a := strings.Index(blk, head)
	b := strings.Index(blk[a:], "\n  args:")
	if a < 0 || b < 0 {
		t.Fatal("the probe shell block is not where it was")
	}
	var lines []string
	for _, l := range strings.Split(blk[a+len(head):a+b], "\n") {
		lines = append(lines, strings.TrimPrefix(l, "    "))
	}
	script := strings.Join(lines, "\n")
	script = regexp.MustCompile(`want_env="\{% for .*%\}"`).ReplaceAllString(script, `want_env="`+wantEnv+`"`)
	script = strings.NewReplacer(
		"{{ bench_port | default(8080) }}", "8080",
		"{{ cell_bin }}", bin,
		"{{ cell_engine | default('') }}", engine,
		"{{ cell.1 }}", "fixture-column",
		"{{ cell_dir }}", "$FAKEDIR",
		"/proc/", "$FAKEPROC/",
		"seq 1 40", "seq 1 2",
		"sleep 0.25", "sleep 0",
	).Replace(script)
	if strings.Contains(script, "{{") || strings.Contains(script, "{%") {
		t.Fatalf("the probe has Jinja this test does not render:\n%s", script)
	}
	return `ss() { echo "State Recv-Q Send-Q Local Peer Process"; for p in $FAKE_PIDS; do echo "LISTEN 0 128 0.0.0.0:8080 0.0.0.0:* users:((\"srv\",pid=$p,fd=3))"; done; }` + "\n" + script
}

func TestPortOwnershipProbeAsAWhole(t *testing.T) {
	y := readRepoFile(t, "ansible/tasks/run_bench_cell.yml")
	bash := needBash(t)
	type holder struct {
		pid     string
		cmd     string // space separated, becomes NUL separated
		environ string // space separated KEY=VALUE
	}
	cl := func(bin, extra string) string {
		return "/opt/bench/competitors/" + bin + " -bind 0.0.0.0:8080 " + extra
	}
	for _, tc := range []struct {
		name            string
		bin, engine, ev string // the column: binary, -engine value, wanted env ("K=V K=V ")
		holders         []holder
		want            int // exit code of the probe: 0 = ours
	}{
		{"own binary and engine", "celeris", "epoll-h1-async", "", []holder{{"11", cl("celeris", "-engine epoll-h1-async "), "A=1"}}, 0},
		{"a leftover celeris_v158 for celeris", "celeris", "epoll-h1-async", "", []holder{{"11", cl("celeris_v158", "-engine epoll-h1-async "), "A=1"}}, 1},
		{"a leftover celeris_9f4d89b for celeris", "celeris", "epoll-h1-async", "", []holder{{"11", cl("celeris_9f4d89b", "-engine epoll-h1-async "), "A=1"}}, 1},
		{"a leftover celeris for celeris_v158", "celeris_v158", "epoll-h1-async", "", []holder{{"11", cl("celeris", "-engine epoll-h1-async "), "A=1"}}, 1},
		{"engine h2c-noupg for a column of engine h2c", "stdhttp", "h2c", "", []holder{{"11", cl("stdhttp", "-engine h2c-noupg "), "A=1"}}, 1},
		{"engine h2c for a column of engine h2c", "stdhttp", "h2c", "", []holder{{"11", cl("stdhttp", "-engine h2c "), "A=1"}}, 0},
		{"another engine of the same binary", "celeris", "epoll-h1-async", "", []holder{{"11", cl("celeris", "-engine iouring-h1-async "), "A=1"}}, 1},
		{"bpon column, holder has the knob", "celeris", "epoll-h1-async", "PROBATORIUM_SOCKET_BUSY_POLL=50us ", []holder{{"11", cl("celeris", "-engine epoll-h1-async "), "A=1 PROBATORIUM_SOCKET_BUSY_POLL=50us"}}, 0},
		{"bpon column, holder lacks the knob", "celeris", "epoll-h1-async", "PROBATORIUM_SOCKET_BUSY_POLL=50us ", []holder{{"11", cl("celeris", "-engine epoll-h1-async "), "A=1"}}, 1},
		{"plain column, leftover busy-poll-on holder", "celeris", "epoll-h1-async", "", []holder{{"11", cl("celeris", "-engine epoll-h1-async "), "A=1 PROBATORIUM_SOCKET_BUSY_POLL=50us"}}, 1},
		{"plain column, leftover ARMED holder", "celeris", "epoll-h1-async", "", []holder{{"11", cl("celeris", "-engine epoll-h1-async "), "A=1 PROBATORIUM_PPROF=dump"}}, 1},
		{"armed column, holder is armed", "celeris", "epoll-h1-async", "PROBATORIUM_PPROF=dump ", []holder{{"11", cl("celeris", "-engine epoll-h1-async "), "A=1 PROBATORIUM_PPROF=dump"}}, 0},
		{"armed column, holder is not armed", "celeris", "epoll-h1-async", "PROBATORIUM_PPROF=dump ", []holder{{"11", cl("celeris", "-engine epoll-h1-async "), "A=1"}}, 1},
		{"a key that only ends in the arm key is not the key", "celeris", "epoll-h1-async", "", []holder{{"11", cl("celeris", "-engine epoll-h1-async "), "XPROBATORIUM_PPROF=dump"}}, 0},
		{"runtime-class listener (no competitors/ in argv) is accepted loosely", "hono", "", "", []holder{{"11", "/opt/bench/bun/bin/bun run /opt/bench/competitors-src/hono/dist/server -bind 0.0.0.0:8080 ", "A=1"}}, 0},
		{"no listener", "celeris", "epoll-h1-async", "", nil, 1},
		{"the wrong process first, ours second", "celeris", "epoll-h1-async", "", []holder{{"11", cl("celeris_v158", "-engine epoll-h1-async "), "A=1"}, {"12", cl("celeris", "-engine epoll-h1-async "), "A=1"}}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			var pids []string
			for _, h := range tc.holders {
				pids = append(pids, h.pid)
				d := filepath.Join(dir, "proc", h.pid)
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
				nul := func(s string) []byte { return []byte(strings.Join(strings.Fields(s), "\x00") + "\x00") }
				if err := os.WriteFile(filepath.Join(d, "cmdline"), nul(h.cmd), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(d, "environ"), nul(h.environ), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command(bash, "-c", probeScript(t, y, tc.bin, tc.engine, tc.ev))
			cmd.Env = append(os.Environ(), "FAKEPROC="+filepath.Join(dir, "proc"), "FAKEDIR="+dir, "FAKE_PIDS="+strings.Join(pids, " "))
			out, err := cmd.CombinedOutput()
			rc := 0
			if err != nil {
				ee, ok := err.(*exec.ExitError)
				if !ok {
					t.Fatal(err)
				}
				rc = ee.ExitCode()
			}
			if rc != tc.want {
				t.Errorf("probe exit %d, want %d\n%s", rc, tc.want, out)
			}
		})
	}
}
