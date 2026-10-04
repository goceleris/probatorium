package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// celeris trees in the layouts a stress row meets: before celeris#443's move,
// after it (the layout of the move's PR: everything under internal/), and the
// proposal's (driver/X/internal/protocol). Each directory listed holds one Go
// file; engine/ in the moved layouts is left behind as a directory without
// one, as a stale checkout might.
var celerisLayouts = map[string][]string{
	"before": {".", "engine", "engine/iouring", "engine/epoll", "engine/internal/errclass", "adaptive", "probe",
		"protocol/h2/frame", "driver/postgres/protocol", "driver/redis/protocol", "internal/sockopts", "middleware/websocket"},
	"after": {".", "internal/engine", "internal/engine/iouring", "internal/engine/epoll", "internal/engine/internal/errclass",
		"internal/adaptive", "internal/probe", "internal/protocol/h2/frame", "internal/driver/postgres/protocol",
		"internal/driver/redis/protocol", "internal/sockopts", "middleware/websocket", "engine/"},
	"proposal": {".", "internal/engine", "internal/engine/iouring", "internal/engine/epoll", "internal/engine/internal/errclass",
		"internal/adaptive", "internal/probe",
		"internal/protocol/h2/frame", "driver/postgres/internal/protocol", "driver/redis/internal/protocol",
		"internal/sockopts", "middleware/websocket"},
}

// celerisTree writes one layout under dir: a directory ending in / gets no Go
// file.
func celerisTree(t *testing.T, dir string, pkgs []string) {
	t.Helper()
	for _, p := range pkgs {
		if strings.HasSuffix(p, "/") {
			if err := os.MkdirAll(filepath.Join(dir, p), 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		writeFile(t, filepath.Join(dir, p, "x.go"), "package x\n")
	}
}

// resolvePkgs runs pkgpaths.sh on dir and returns what it resolved and its
// per-pattern lines.
func resolvePkgs(t *testing.T, dir string, pats ...string) ([]string, string) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	cmd := exec.Command("bash", append([]string{script(t, "pkgpaths.sh"), dir}, pats...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("pkgpaths.sh: %v\n%s", err, stderr.String())
	}
	return strings.Fields(stdout.String()), stderr.String()
}

// One row works on both sides of celeris#443's move: each pattern runs where
// the commit under test keeps the package, the old path, the new one or the
// proposal's, whichever the row names; what is a package as named is never
// rewritten; . and ./... never are; a pattern no layout has is passed on as
// named, for go test to report as it always did.
func TestPkgPathsResolvesEachLayout(t *testing.T) {
	pats := []string{".", "./engine/iouring", "./adaptive", "./engine/epoll", "./driver/postgres/protocol",
		"./internal/engine/iouring", "./driver/redis/internal/protocol", "./internal/driver/redis/protocol",
		"./engine/...", "./engine", "./engine/internal/errclass", "./protocol/h2/frame", "./probe",
		"./internal/sockopts", "./middleware/websocket", "./...", "./nosuch"}
	want := map[string][]string{
		"before": {".", "./engine/iouring", "./adaptive", "./engine/epoll", "./driver/postgres/protocol",
			"./engine/iouring", "./driver/redis/protocol", "./driver/redis/protocol",
			"./engine/...", "./engine", "./engine/internal/errclass", "./protocol/h2/frame", "./probe",
			"./internal/sockopts", "./middleware/websocket", "./...", "./nosuch"},
		"after": {".", "./internal/engine/iouring", "./internal/adaptive", "./internal/engine/epoll", "./internal/driver/postgres/protocol",
			"./internal/engine/iouring", "./internal/driver/redis/protocol", "./internal/driver/redis/protocol",
			"./internal/engine/...", "./internal/engine", "./internal/engine/internal/errclass", "./internal/protocol/h2/frame", "./internal/probe",
			"./internal/sockopts", "./middleware/websocket", "./...", "./nosuch"},
		"proposal": {".", "./internal/engine/iouring", "./internal/adaptive", "./internal/engine/epoll", "./driver/postgres/internal/protocol",
			"./internal/engine/iouring", "./driver/redis/internal/protocol", "./driver/redis/internal/protocol",
			"./internal/engine/...", "./internal/engine", "./internal/engine/internal/errclass", "./internal/protocol/h2/frame", "./internal/probe",
			"./internal/sockopts", "./middleware/websocket", "./...", "./nosuch"},
	}
	for layout, pkgs := range celerisLayouts {
		t.Run(layout, func(t *testing.T) {
			dir := t.TempDir()
			celerisTree(t, dir, pkgs)
			got, lines := resolvePkgs(t, dir, pats...)
			if !slices.Equal(got, want[layout]) {
				for i := range max(len(got), len(want[layout])) {
					g, w := "", ""
					if i < len(got) {
						g = got[i]
					}
					if i < len(want[layout]) {
						w = want[layout][i]
					}
					if g != w {
						t.Errorf("%s: resolved %q, want %q", pats[min(i, len(pats)-1)], g, w)
					}
				}
				t.Logf("pkgpaths.sh:\n%s", lines)
			}
			if !strings.Contains(lines, "./nosuch -> ./nosuch (no candidate is a package at this commit") {
				t.Errorf("an unresolved pattern is not reported as such:\n%s", lines)
			}
		})
	}
}

// A timing whose arms straddle the move (base before it, fix after it) builds
// and runs each arm's package where that arm's commit keeps it, and each
// observation's header keeps the pattern as asked (the summary checks it
// against the plan) beside what ran.
func TestTimingHostJobResolvesEachArmsLayout(t *testing.T) {
	h := newTimingHost(t, "A", "B")
	celerisTree(t, filepath.Join(h.dir, "celeris-A"), celerisLayouts["before"])
	celerisTree(t, filepath.Join(h.dir, "celeris-B"), celerisLayouts["after"])
	h.env["STRESS_SEQUENCE"] = "A:1:11 B:1:11"
	out, code := runBash(t, h.dir, h.env, `bash "$1"`, script(t, "cluster-host.sh"))
	if code != 0 {
		t.Fatalf("cluster-host.sh: exit %d\n%s", code, out)
	}
	want := map[string]string{"celeris-A": "./engine/iouring", "celeris-B": "./internal/engine/iouring"}
	n := 0
	for _, c := range h.goCalls(t) {
		dir, args, _ := strings.Cut(c, " ")
		if !strings.HasPrefix(args, "test ") {
			continue
		}
		n++
		f := strings.Fields(args)
		if w := want[filepath.Base(dir)]; f[len(f)-1] != w {
			t.Errorf("go %s in %s: package %s, want %s", args, filepath.Base(dir), f[len(f)-1], w)
		}
	}
	if n != 4 {
		t.Errorf("%d go test calls, want 4 (a prebuild and an observation per arm):\n%v", n, h.goCalls(t))
	}
	facts, err := os.ReadFile(h.env["STRESS_FACTS"])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(facts), "prebuild B: package ./engine/iouring -> ./internal/engine/iouring") ||
		strings.Contains(string(facts), "prebuild A: package") {
		t.Errorf("the host facts do not record arm B's package, and only B's:\n%s", facts)
	}
	for arm, ran := range map[string]string{"A": "./engine/iouring", "B": "./internal/engine/iouring"} {
		log, err := os.ReadFile(filepath.Join(h.env["STRESS_LOG_DIR"], arm+"__"+h.env["STRESS_ARCH"]+"__1.log"))
		if err != nil {
			t.Fatal(err)
		}
		sl := parseShard(bytes.NewReader(log))
		if sl.header["packages"] != "./engine/iouring" || sl.header["packages_resolved"] != ran {
			t.Errorf("arm %s: header packages %q, packages_resolved %q; want ./engine/iouring and %s\n%s",
				arm, sl.header["packages"], sl.header["packages_resolved"], ran, log)
		}
		if !strings.HasSuffix(strings.TrimSpace(sl.header["cmd"]), " "+ran) {
			t.Errorf("arm %s: cmd %q does not run %s", arm, sl.header["cmd"], ran)
		}
	}
}
