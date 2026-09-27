//go:build !linux

package remote

// awaitExit reports false: the reaper learns of the exit from the reap
// itself. Go does not wait with WNOWAIT outside Linux and FreeBSD either; on
// macOS, waitid returns for a stopped process even with WEXITED alone
// (golang/go#19314).
func awaitExit(int) bool { return false }
