//go:build linux

package main

// isLinux reports whether the current build target is Linux. Used to
// decide whether the "auto" engine value picks IOUring or Std.
func isLinux() bool { return true }
