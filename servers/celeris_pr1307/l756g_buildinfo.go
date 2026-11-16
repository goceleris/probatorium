package main

// L756G (THROWAWAY ab/l756g-diag-20261116, not for merge): the IDENTITY of the linked celeris, printed ONCE at process start.
//
// L756G is an A/B of two celeris pins whose default loop count is the same (12 loops on both PR #1306 and PR #1307): the engine's own
// "loops=N" line cannot tell the arms apart, and the registry's FrameworkVersion is a label the generator wrote. The binary's own build info is
// the one thing that says which celeris module (version and go.sum hash) was linked:
//
//	[celeris-build] version=<module version> sum=<h1:...>
//
// printed from init (before main, so before every other startup line); the validator (l756g-validate.py, tag BUILD) compares it with the arm's
// pin and with the go.sum hash of that pin. A binary built without module info, or with a replaced celeris, prints version=unknown / a replace marker
// and fails the check; it never prints a default that could pass.

import (
	"fmt"
	"log"
	"runtime/debug"
)

const celerisModulePath = "github.com/goceleris/celeris"

// celerisBuildLine formats the identity line from build info (a separate function so a test can feed it a synthetic BuildInfo).
func celerisBuildLine(bi *debug.BuildInfo, ok bool) string {
	if !ok || bi == nil {
		return "[celeris-build] version=unknown sum=unknown"
	}
	for _, d := range bi.Deps {
		if d.Path != celerisModulePath {
			continue
		}
		if d.Replace != nil {
			return "[celeris-build] version=replaced sum=replaced"
		}
		if d.Version == "" || d.Sum == "" {
			return "[celeris-build] version=unknown sum=unknown"
		}
		return fmt.Sprintf("[celeris-build] version=%s sum=%s", d.Version, d.Sum)
	}
	return "[celeris-build] version=absent sum=absent"
}

func init() {
	bi, ok := debug.ReadBuildInfo()
	log.Print(celerisBuildLine(bi, ok))
}
