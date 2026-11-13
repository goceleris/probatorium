package main

// L857F (celeris#857) arm R16: PROBATORIUM_MC_CAP=<n> bounds the memcached operations IN FLIGHT to n with a counting semaphore (a buffered channel)
// acquired around each gomemcache Get / GetMulti / Set, the symmetric counterpart of the celeris adapter's hard WithMaxOpen cap. Unset or empty = no
// cap: mcSem stays nil, mcAcquire / mcRelease do nothing, and the request path is the one this adapter always had. A malformed value (not a positive
// integer) is a fatal start error, never a silent fall-back. The adapter prints one `[m857]` line at mount (the L857F validator compares it with the
// registered arm); client is always gomemcache here and maxopen is the celeris knob (0).

import (
	"fmt"
	"log"
	"os"
	"strconv"
)

const envMCCap = "PROBATORIUM_MC_CAP"

// mcSem is the semaphore; nil when no cap is configured. Written once at mount, before the server serves.
var mcSem chan struct{}

// parseMCCap resolves the cap from a getenv function: 0 = unset / empty (no cap), else a positive integer.
func parseMCCap(getenv func(string) string) (int, error) {
	raw := getenv(envMCCap)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s=%q: want a positive integer", envMCCap, raw)
	}
	return n, nil
}

// resolveMCCap reads the process environment, installs the semaphore and prints the `[m857]` startup line.
func resolveMCCap() {
	n, err := parseMCCap(os.Getenv)
	if err != nil {
		log.Fatalf("[m857] invalid environment: %v", err)
	}
	if n > 0 {
		mcSem = make(chan struct{}, n)
	}
	log.Printf("[m857] client=gomemcache maxopen=0 cap=%d", n)
}

func mcAcquire() {
	if mcSem != nil {
		mcSem <- struct{}{}
	}
}

func mcRelease() {
	if mcSem != nil {
		<-mcSem
	}
}
