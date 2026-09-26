// Package fieldguard holds one guard and its controls: every field of
// properties.Snapshot that code in the probatorium module reads must also be
// written somewhere in that module (probatorium#395). A field that is read and
// never written holds zero forever, so a predicate judging it compares zero
// with zero, cannot fail, and is still counted as an evaluation in every run
// report.
//
// The guard needs full type information, so that a use counts only when the
// value being selected from IS a properties.Snapshot, however it was reached:
// a parameter, an element of Context.History, a struct field, a slice or map
// element, a promoted field. That takes golang.org/x/tools/go/packages, and
// this is a module of its own so x/tools stays out of the root module: every
// adapter module under servers/ replaces the root module and inherits its
// requirements, so a requirement added there spreads to all twelve of them.
//
// The tests load the ROOT module (the directory three levels up) from source,
// with `go list`, as its own go.mod resolves it. This module's go.mod only
// decides how the test binary itself is built.
package fieldguard
