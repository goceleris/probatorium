//go:build !linux

package main

// isLinux reports whether the current build target is Linux. On
// non-Linux platforms this returns false so "auto" engine selection
// picks Std (the only engine that builds + runs everywhere).
func isLinux() bool { return false }
