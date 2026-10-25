//go:build !linux

package main

import "log"

// The bench SUT is Linux-only: this arm exists to move the Listen thread's CPU mask.
func armBeforeStart(string) { log.Fatalf("[noc8-arm] linux only") }
