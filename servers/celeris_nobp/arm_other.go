//go:build !linux

package main

import "log"

// The bench SUT is Linux-only.
func armBeforeStart(string) { log.Fatalf("[nobp-arm] linux only") }
