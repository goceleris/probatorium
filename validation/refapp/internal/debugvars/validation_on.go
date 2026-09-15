//go:build validation

package debugvars

// validationBuild is true only in a refapp compiled with -tags=validation,
// which compiles celeris's own assertion counters in (celeris/validation):
// the io_uring SQE monotonicity check behind I-ENG-IOURING among them. In
// a plain build validation.Snapshot() is all zeros by construction, so the
// tag is what turns that zero into a verdict.
const validationBuild = true
