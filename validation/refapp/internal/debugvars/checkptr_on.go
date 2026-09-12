//go:build checkptr

package debugvars

// checkptrBuild is true only in a refapp compiled with -tags=checkptr, which
// the build pipeline pairs with -gcflags=...=-d=checkptr=1. The tag is the
// refapp's own statement that the checker is compiled in; the gcflags alone
// leave no trace a running process can report.
const checkptrBuild = true
