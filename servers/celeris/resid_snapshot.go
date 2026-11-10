package main

import (
	"time"

	"github.com/goceleris/celeris"
)

// The ONLY file that names a celeris symbol of the stamps: it needs the celeris of the throwaway branch diag/757-residence-stamps (DiagResidSnapshot, root diag_resid.go), so the go.mod pin of
// servers/celeris must point at it for the d757 dispatch. Main module only (the mirrors keep residSnapshot nil). Line has no log prefix: the exporter's log.Print adds the standard one.
func init() {
	residSnapshot = func() string { return celeris.DiagResidSnapshot().Line(time.Now().Unix()) }
}
