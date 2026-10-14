package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The validator starts every refapp as the leader of a process group of its
// own (validation/remote/local.go, probatorium#415). When the validator
// overruns its `async:` window, ansible's async wrapper SIGKILLs the
// validator's process group, and that kill no longer reaches the refapps.
// validate.yml's always: block is then the only thing that stops them.
//
// Its pattern used to be "[/]refapps/". That matches none of the race or
// checkptr tier's refapps, which run from refapps-race/ and
// refapps-checkptr/, so after a timed-out run of either tier they stayed up
// until the host rebooted. The directories are read from -matrix-bin-dir
// itself, so a tier directory added there and not here fails this test.
func TestValidateCleanupStopsEveryRefappDir(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("ansible", "validate.yml"))
	if err != nil {
		t.Fatalf("read validate.yml: %v", err)
	}
	src := string(b)
	dirs := validateRefappDirs(t, src)
	t.Logf("validate.yml runs refapps from %v", dirs)
	block := shellBlockOfTask(t, src, "Stop any leftover validator / refapp processes")

	var term, kill []*regexp.Regexp
	for _, m := range regexp.MustCompile(`(?m)^\s*(pkill|pgrep)((?:\s+-[A-Z]+)?)\s+-f\s+"([^"]+)"`).FindAllStringSubmatch(block, -1) {
		re, err := regexp.CompilePOSIX(m[3])
		if err != nil {
			t.Fatalf("pattern %q in the cleanup task is not an extended regex: %v", m[3], err)
		}
		// pkill -f and pgrep -f match the full argv of every process,
		// including the `sh -c <block>` that runs them. A pattern that
		// matches the block's own text kills the cleanup mid-way.
		if re.MatchString(block) {
			t.Errorf("%s -f %q matches the text of its own shell block, so it would kill the shell running the cleanup", m[1], m[3])
		}
		switch {
		case m[1] == "pkill" && strings.TrimSpace(m[2]) == "":
			term = append(term, re)
		case m[1] == "pkill" && strings.TrimSpace(m[2]) == "-KILL":
			kill = append(kill, re)
		}
	}
	if len(term) == 0 || len(kill) == 0 {
		t.Errorf("the cleanup task has %d SIGTERM and %d SIGKILL `pkill -f` patterns; it needs both, in that order:\n%s", len(term), len(kill), block)
	} else if strings.Index(block, "pkill -KILL") < strings.Index(block, "pkill -f") {
		t.Errorf("the cleanup sends SIGKILL before SIGTERM:\n%s", block)
	}
	anyMatch := func(res []*regexp.Regexp, s string) bool {
		for _, re := range res {
			if re.MatchString(s) {
				return true
			}
		}
		return false
	}
	for _, d := range dirs {
		argv := "/tmp/celeris-bench/" + d + "/kitchen_sink -bind 127.0.0.1:0 -engine iouring"
		if !anyMatch(term, argv) {
			t.Errorf("no SIGTERM pattern in the cleanup matches a refapp run from %s/ (argv %q)", d, argv)
		}
		if !anyMatch(kill, argv) {
			t.Errorf("no SIGKILL pattern in the cleanup matches a refapp run from %s/ (argv %q)", d, argv)
		}
	}
}

// validateRefappDirs returns every directory under bench_root that
// validate.yml runs refapps from: the one -celeris-bin names, and each one
// the -matrix-bin-dir expression can pick.
func validateRefappDirs(t *testing.T, src string) []string {
	t.Helper()
	seen := map[string]bool{}
	var dirs []string
	add := func(d string) {
		if !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	for _, m := range regexp.MustCompile(`-celeris-bin=\{\{ bench_root \}\}/([\w-]+)/`).FindAllStringSubmatch(src, -1) {
		add(m[1])
	}
	matrix := regexp.MustCompile(`-matrix-bin-dir=\{\{ bench_root \}\}/\{\{(.*?)\}\}`).FindAllStringSubmatch(src, -1)
	if len(matrix) == 0 {
		t.Fatal("validate.yml has no -matrix-bin-dir={{ bench_root }}/{{ ... }}: this test no longer reads the directories the matrix tiers run refapps from")
	}
	// The expression is a Jinja conditional, `'a' if x else ('b' if y else
	// 'c')`: its results are the literals before an `if` and after an
	// `else`. The literals it compares against ('' and '1') are neither.
	choice := regexp.MustCompile(`'([\w-]+)'\s+if\s|else\s+\(?\s*'([\w-]+)'`)
	n := 0
	for _, m := range matrix {
		for _, q := range choice.FindAllStringSubmatch(m[1], -1) {
			add(q[1] + q[2])
			n++
		}
	}
	if n < 2 {
		t.Fatalf("read %d directory names from the -matrix-bin-dir expression %q; it chooses between the plain, race and checkptr directories, so the parse is broken", n, matrix[0][1])
	}
	return dirs
}

// shellBlockOfTask returns the literal `ansible.builtin.shell: |` script of
// the task named name, with its indentation removed: the text `sh -c` runs.
func shellBlockOfTask(t *testing.T, src, name string) string {
	t.Helper()
	lines := strings.Split(src, "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == "- name: "+name {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("validate.yml has no task %q", name)
	}
	for i := start + 1; i < len(lines); i++ {
		l := lines[i]
		if strings.HasPrefix(strings.TrimSpace(l), "- name:") {
			break
		}
		if strings.TrimSpace(l) != "ansible.builtin.shell: |" {
			continue
		}
		keyIndent := len(l) - len(strings.TrimLeft(l, " "))
		var body []string
		indent := -1
		for _, b := range lines[i+1:] {
			if strings.TrimSpace(b) == "" {
				body = append(body, "")
				continue
			}
			ind := len(b) - len(strings.TrimLeft(b, " "))
			if ind <= keyIndent {
				break
			}
			if indent < 0 {
				indent = ind
			}
			body = append(body, b[min(indent, ind):])
		}
		if len(body) == 0 {
			t.Fatalf("task %q has an empty shell block", name)
		}
		return strings.Join(body, "\n")
	}
	t.Fatalf("task %q has no `ansible.builtin.shell: |` block", name)
	return ""
}
