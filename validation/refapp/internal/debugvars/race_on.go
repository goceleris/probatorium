//go:build race

package debugvars

// raceBuild is true only in a refapp compiled with -race: the Go toolchain
// sets the race build tag itself, so this is the process's own statement
// that the detector is linked in and its reports can exist. The property
// loop declares I-RACE on it.
const raceBuild = true
