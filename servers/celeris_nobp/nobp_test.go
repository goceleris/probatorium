package main

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const (
	celerisPin      = "github.com/goceleris/celeris v1.5.12-0.20261021042539-713b40dfdd0d"
	patchedFile     = "_celeris/internal/sockopts/sockopts_linux.go"
	upstreamLine    = "\tif opts.SOBusyPoll > 0 {\n"
	patchedLinePfx  = "\tif false && opts.SOBusyPoll > 0 { // P2c nobp"
	celerisModPath  = "github.com/goceleris/celeris"
	celerisModVer   = "v1.5.12-0.20261021042539-713b40dfdd0d"
	celerisMainDir  = "../celeris"
	celerisCopyRoot = "_celeris"
)

// dirhash1 is golang.org/x/mod/sumdb/dirhash.Hash1 over the files of root, named as in a module zip (modpath@version/relpath). override replaces the bytes of one relative path.
func dirhash1(t *testing.T, root string, override map[string][]byte) string {
	t.Helper()
	var names []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		names = append(names, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, rel := range names {
		b, ok := override[rel]
		if !ok {
			var err error
			if b, err = os.ReadFile(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
				t.Fatal(err)
			}
		}
		fmt.Fprintf(h, "%x  %s\n", sha256.Sum256(b), celerisModPath+"@"+celerisModVer+"/"+rel)
	}
	return "h1:" + base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The copy under _celeris is celeris 713b40d plus exactly one changed line: with that line reverted, the dirhash of the whole tree equals the h1 that servers/celeris/go.sum
// records for the pinned module. (The h1 is the Go module proxy's checksum of the zip, so this is a content proof, not a name check.)
func TestCelerisCopyIsThePinPlusOneLine(t *testing.T) {
	goSum := mustRead(t, filepath.Join(celerisMainDir, "go.sum"))
	want := ""
	for _, l := range strings.Split(goSum, "\n") {
		f := strings.Fields(l)
		if len(f) == 3 && f[0] == celerisModPath && f[1] == celerisModVer {
			want = f[2]
		}
	}
	if want == "" {
		t.Fatal("servers/celeris/go.sum has no h1 for the pinned celeris")
	}
	src := mustRead(t, filepath.FromSlash(strings.TrimPrefix(patchedFile, "")))
	if strings.Count(src, upstreamLine) != 0 || strings.Count(src, patchedLinePfx) != 1 {
		t.Fatalf("%s must carry the patched line exactly once and no unpatched line", patchedFile)
	}
	// revert the single line: the patched line in full, up to and including its newline
	i := strings.Index(src, patchedLinePfx)
	j := i + strings.Index(src[i:], "\n") + 1
	reverted := src[:i] + upstreamLine + src[j:]
	rel := strings.TrimPrefix(patchedFile, celerisCopyRoot+"/")
	got := dirhash1(t, celerisCopyRoot, map[string][]byte{rel: []byte(reverted)})
	if got != want {
		t.Fatalf("_celeris with the line reverted hashes to %s, servers/celeris/go.sum has %s for %s: the copy is not the pin plus one line", got, want, celerisModVer)
	}
	// and the patch really is a one-line change
	if strings.Count(src, "\n") != strings.Count(reverted, "\n") {
		t.Errorf("patched file has %d lines, upstream %d", strings.Count(src, "\n"), strings.Count(reverted, "\n"))
	}
}

// This module is servers/celeris plus: the module path, the replace directive, the arm files, ONE added line in server.go, and go.sum without the two celeris lines (a
// replaced module needs none). Everything else is byte for byte main's.
func TestNobpModuleIsMainPlusTheArm(t *testing.T) {
	same := []string{"chain_handlers.go", "debugvars.go", "debugvars_test.go", "driver_handlers.go", "streaming_handlers.go"}
	for _, f := range same {
		if mustRead(t, f) != mustRead(t, filepath.Join(celerisMainDir, f)) {
			t.Errorf("%s differs from servers/celeris/%s", f, f)
		}
	}
	mainSrv, mySrv := mustRead(t, filepath.Join(celerisMainDir, "server.go")), mustRead(t, "server.go")
	if strings.Count(mySrv, "\tarmBeforeStart(*bind)\n") != 1 || strings.Replace(mySrv, "\tarmBeforeStart(*bind)\n", "", 1) != mainSrv {
		t.Error("server.go must be servers/celeris/server.go plus exactly one line: armBeforeStart(*bind)")
	}
	mainMod := strings.Replace(mustRead(t, filepath.Join(celerisMainDir, "go.mod")), "servers/celeris\n", "servers/celeris_nobp\n", 1)
	myMod := mustRead(t, "go.mod")
	if myMod != mainMod+"replace github.com/goceleris/celeris => ./_celeris\n" {
		t.Errorf("go.mod is not main's go.mod with the module path renamed plus the replace line:\n%s", myMod)
	}
	if !strings.Contains(myMod, celerisPin) {
		t.Errorf("go.mod does not require %s", celerisPin)
	}
	// go.sum: main's minus the two celeris lines.
	var keep []string
	for _, l := range strings.Split(mustRead(t, filepath.Join(celerisMainDir, "go.sum")), "\n") {
		if !strings.HasPrefix(l, celerisModPath+" ") {
			keep = append(keep, l)
		}
	}
	if mustRead(t, "go.sum") != strings.Join(keep, "\n") {
		t.Error("go.sum is not servers/celeris/go.sum without the celeris lines")
	}
}

// Only the one file of the copy differs from the pin, and the sibling options of the patched function are still there.
func TestPatchedFunctionStillAppliesTheOtherOptions(t *testing.T) {
	src := mustRead(t, patchedFile)
	for _, must := range []string{"TCP_NODELAY", "TCP_QUICKACK", "unix.SO_RCVBUF", "unix.SO_SNDBUF", "unix.SO_BUSY_POLL", "69, 1", "70, 8"} {
		if !strings.Contains(src, must) {
			t.Errorf("%s no longer contains %q: the patch must change the guard only", patchedFile, must)
		}
	}
}
