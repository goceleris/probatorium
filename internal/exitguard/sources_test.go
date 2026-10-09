package exitguard

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Every harness main that starts a celeris server must end through this
// package. The helper only protects the processes that use it, so this is the
// guard that a ninth refapp, or a refactor of an existing one, cannot bring
// back the ignore-and-block pattern unnoticed.

var (
	importsCeleris = regexp.MustCompile(`"github\.com/goceleris/celeris"`)
	// A reference counts as well as a call: guard.Serve(srv.Start) hands Start over uncalled.
	startsServer = regexp.MustCompile(`\.(Start|StartWithListener|StartWithContext|StartWithListenerAndContext)\b`)
	usesGuard    = regexp.MustCompile(`"github\.com/goceleris/probatorium/internal/exitguard"`)
	installs     = regexp.MustCompile(`exitguard\.Install\(`)
	serves       = regexp.MustCompile(`\.Serve\(`)
	// The old handler: it caught the signal itself and dropped Shutdown's result.
	ignoredShutdown = regexp.MustCompile(`_\s*=\s*\w+\.Shutdown\(`)
	ownSignalNotify = regexp.MustCompile(`signal\.Notify\(`)
)

// violations returns one message per rule a harness main breaks. src is the
// file's text.
func violations(path, src string) []string {
	if !importsCeleris.MatchString(src) || !startsServer.MatchString(src) {
		return nil // not a file that starts a celeris server
	}
	var v []string
	if !usesGuard.MatchString(src) || !installs.MatchString(src) || !serves.MatchString(src) {
		v = append(v, path+": starts a celeris server without exitguard.Install + Guard.Serve")
	}
	if ignoredShutdown.MatchString(src) {
		v = append(v, path+": ignores a Shutdown result (`_ = x.Shutdown(...)`)")
	}
	if ownSignalNotify.MatchString(src) {
		v = append(v, path+": installs its own signal handler; exitguard owns SIGTERM/SIGINT")
	}
	return v
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && strings.HasPrefix(string(b), "module github.com/goceleris/probatorium\n") {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skip("probatorium root module not found above the test directory")
		}
		dir = parent
	}
}

// harnessMains lists every non-test .go file under the trees that hold
// celeris-backed harness servers, except the shared helper module.
func harnessMains(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	for _, tree := range []string{"validation/refapp", "servers/celeris"} {
		err := filepath.WalkDir(filepath.Join(root, tree), func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "internal" || d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
				files = append(files, p)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(files)
	return files
}

func TestEveryHarnessMainUsesExitguard(t *testing.T) {
	root := repoRoot(t)
	mains := map[string]bool{}
	for _, p := range harnessMains(t, root) {
		src, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(root, p)
		if !importsCeleris.Match(src) || !startsServer.Match(src) {
			continue
		}
		mains[rel] = true
		for _, v := range violations(rel, string(src)) {
			t.Error(v)
		}
	}
	// Anchor the scan: it must have found the nine mains it exists for, or
	// a renamed tree would make it vacuously green.
	want := []string{
		"servers/celeris/server.go",
		"validation/refapp/auth_jwt_csrf/main.go",
		"validation/refapp/auth_session_ratelimit/main.go",
		"validation/refapp/driver_memcached/main.go",
		"validation/refapp/driver_postgres/main.go",
		"validation/refapp/driver_redis/main.go",
		"validation/refapp/kitchen_sink/main.go",
		"validation/refapp/observability/main.go",
		"validation/refapp/static_swagger_proxy/main.go",
	}
	for _, w := range want {
		if !mains[w] {
			t.Errorf("scan did not pick up %s (renamed, or no longer starts a celeris server)", w)
		}
	}
}

// NEGATIVE CONTROL: the scan's rules do fire on the old pattern, verbatim.
func TestViolationsCatchTheOldPattern(t *testing.T) {
	const old = `package main

import (
	"context"
	"os/signal"
	"github.com/goceleris/celeris"
)

func main() {
	srv := celeris.New(celeris.Config{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()
	if err := srv.StartWithListener(ln); err != nil {
	}
}
`
	if v := violations("old.go", old); len(v) != 3 {
		t.Fatalf("old pattern produced %d violation(s) %q, want 3 (no guard, ignored Shutdown, own handler)", len(v), v)
	}
	const fixed = `package main

import (
	"github.com/goceleris/celeris"
	"github.com/goceleris/probatorium/internal/exitguard"
)

func main() {
	srv := celeris.New(celeris.Config{})
	guard := exitguard.Install(exitguard.Config{Name: "x"}, srv.Shutdown)
	if err := guard.Serve(func() error { return srv.StartWithListener(ln) }); err != nil {
	}
}
`
	if v := violations("fixed.go", fixed); len(v) != 0 {
		t.Fatalf("guarded main flagged: %q", v)
	}
}
