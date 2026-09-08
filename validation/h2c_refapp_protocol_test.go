package validation

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/goceleris/probatorium/report"
)

// refappProtocolRoot is where the per-refapp modules live, relative to
// this package's directory (go test runs with cwd = package dir).
const refappProtocolRoot = "refapp"

// refappProtocols parses every refapp's main.go and returns
// slug -> the celeris.Protocol constant it passes to celeris.Config.
//
// Source parsing rather than an import because each refapp is its own
// Go module (its own go.mod pins its own celeris), so the root module
// cannot link them. The parse is deliberately strict: a refapp that
// omits Protocol, or configures the upgrade in a shape this scan does
// not understand, fails the test instead of being silently classified.
func refappProtocols(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(refappProtocolRoot)
	if err != nil {
		t.Fatalf("read %s: %v", refappProtocolRoot, err)
	}
	out := map[string]string{}
	for _, e := range entries {
		// internal/ holds the shared debugvars helper, not a refapp.
		if !e.IsDir() || e.Name() == "internal" {
			continue
		}
		path := filepath.Join(refappProtocolRoot, e.Name(), "main.go")
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		var proto string
		ast.Inspect(f, func(n ast.Node) bool {
			kv, ok := n.(*ast.KeyValueExpr)
			if !ok {
				return true
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok {
				return true
			}
			switch key.Name {
			case "Protocol":
				sel, ok := kv.Value.(*ast.SelectorExpr)
				if !ok {
					t.Fatalf("%s: Protocol must be a celeris.<Const>, got %T", path, kv.Value)
				}
				proto = sel.Sel.Name
			case "EnableH2Upgrade":
				// Forcing the flag on works for iouring and epoll but NOT
				// for the std engine, which wraps the handler in
				// x/net/http2/h2c only when Protocol is Auto or H2C and
				// ignores EnableH2Upgrade entirely. A refapp using that
				// shape would upgrade on 2 of the 3 matrix engines, so the
				// gate could not judge its cells uniformly.
				t.Fatalf("%s: sets EnableH2Upgrade; the std engine ignores it — use Protocol: celeris.Auto", path)
			}
			return true
		})
		if proto == "" {
			t.Fatalf("%s: no Protocol field in the celeris.Config literal; an unset Protocol defaults to Auto silently", path)
		}
		out[e.Name()] = proto
	}
	if len(out) == 0 {
		t.Fatalf("no refapps found under %s", refappProtocolRoot)
	}
	return out
}

// h2cUpgradeRefappsFromSource returns the sorted slugs of the refapps
// whose Protocol makes celeris answer an h1->h2c upgrade. Only Auto
// qualifies: celeris infers EnableH2Upgrade from Protocol (enabled for
// Auto, disabled for HTTP1 and H2C), and the std engine reads Protocol
// alone.
func h2cUpgradeRefappsFromSource(t *testing.T) []string {
	t.Helper()
	var out []string
	for slug, proto := range refappProtocols(t) {
		switch proto {
		case "Auto":
			out = append(out, slug)
		case "HTTP1":
			// Serves h1 only; declining the upgrade is correct here.
		default:
			t.Fatalf("%s: unhandled Protocol %q — teach this test (and report.DefaultH2CUpgradeRefapps) what it means for the h2c slice", slug, proto)
		}
	}
	slices.Sort(out)
	return out
}

// TestRefappsServeH2CUpgrade pins probatorium#279: at least one refapp
// must serve the HTTP/1.1 -> h2c upgrade, or the Tier 1 h2c-churn slice
// is testing nothing.
//
// All eight refapps used to pin Protocol HTTP1, from which celeris
// infers EnableH2Upgrade=false, so every one of the slice's three churn
// modes degenerated into a plain declined GET: the v1.5.11 nightly sent
// 172,656 upgrade preambles across 48 cells and recorded zero 101s.
// h2c_hang == 0 and h2c_crashed == 0 then read as health while judging a
// code path the server never entered — the path whose PauseAccept race
// is celeris#470's whole subject.
func TestRefappsServeH2CUpgrade(t *testing.T) {
	up := h2cUpgradeRefappsFromSource(t)
	if len(up) == 0 {
		t.Fatalf("no refapp serves Protocol celeris.Auto: every h2c churn walker gets a decline, so h2c_hang/h2c_crashed judge nothing (probatorium#279); protocols=%v",
			refappProtocols(t))
	}
	t.Logf("refapps serving the h1->h2c upgrade: %v", up)
}

// TestRefappH2CUpgradeSetMatchesGateDefault keeps the gate's expectation
// and the refapp configs from drifting apart. The gate fails a cell that
// sent upgrade preambles and got no 101, but only for the refapps in
// report.DefaultH2CUpgradeRefapps; if that list outlives the config it
// names, the gate either judges an HTTP1-only refapp (a false failure on
// every one of its cells) or stops judging the one refapp that is
// supposed to upgrade — which puts probatorium#279 straight back.
func TestRefappH2CUpgradeSetMatchesGateDefault(t *testing.T) {
	src := h2cUpgradeRefappsFromSource(t)
	want := slices.Sorted(slices.Values(report.DefaultH2CUpgradeRefapps))
	if !slices.Equal(src, want) {
		t.Fatalf("refapps serving Protocol Auto = %v, report.DefaultH2CUpgradeRefapps = %v; update whichever is stale",
			src, want)
	}
}
