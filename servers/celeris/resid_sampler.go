package main

// The 1 Hz `[res]` exporter of the D757 dx arm (THROWAWAY ab/d757-<date>, not for merge). The residence stamps live in celeris itself (internal/diag/resid on the throwaway branch
// diag/757-residence-stamps, gate CELERIS_DIAG_RESID read by celeris at process start); this file only (1) prints the gate line `[res-env] env=<raw|unset> level=<0|1>` ALWAYS (so the validator can
// prove the gate of every celeris column, a0 included) and (2) when the gate is on, prints once per second the cumulative snapshot line `[res] ...` beside `[sched]`.
//
// THE EXPORTER MUST STOP BEFORE THE SUT'S "signal received" LINE, exactly like the sched sampler (the CRASH rule needs that line last before [respawn]; a `[res]` line at or after it voids the column):
// stop() is synchronous, nothing is written once it has returned. server.go calls it right after stopSched().
//
// Like sched_sampler.go this file names no celeris symbol and is byte-identical in the generated mirrors (servers/celeris_9f4d89b, servers/celeris_v158: the c2lad tests demand it). The snapshot
// function is a package variable: only servers/celeris/resid_snapshot.go (main module only, built against the stamps branch) sets it; in a mirror it stays nil and a gate that is on is a fatal start error.

import (
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

// residGateEnv is celeris's own gate (internal/diag/resid EnvGate); the registry carries it as "1" on the dx columns only.
const residGateEnv = "CELERIS_DIAG_RESID"

// residSnapshot returns the cumulative `[res]` line (set by resid_snapshot.go; nil where the binary has no stamps).
var residSnapshot func() string

// residGateOn mirrors celeris's gateOn: any value except "", 0, false, off, no turns the stamps on.
func residGateOn(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "false", "off", "no":
		return false
	}
	return true
}

// residEnvLine is the gate line the validator reads: env = the raw value or "unset", level 1 iff celeris turns the stamps on.
func residEnvLine(raw string, set bool) string {
	env, level := "unset", "0"
	if set {
		env = raw
	}
	if set && residGateOn(raw) {
		level = "1"
	}
	return "[res-env] env=" + env + " level=" + level
}

// startResidSampler calls emit(snap()) on every tick (once per second when ticks is nil) until stop() returns.
func startResidSampler(emit func(string), snap func() string, ticks <-chan time.Time) (stop func()) {
	var mu sync.Mutex
	stopped := false
	quit, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		if ticks == nil {
			tk := time.NewTicker(time.Second)
			defer tk.Stop()
			ticks = tk.C
		}
		for {
			select {
			case <-quit:
				return
			case <-ticks:
				mu.Lock()
				if stopped {
					mu.Unlock()
					return
				}
				emit(snap())
				mu.Unlock()
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			mu.Lock()
			stopped = true
			mu.Unlock()
			close(quit)
		})
		<-done
	}
}

// mustStartResidSampler prints the gate line and, with the gate on, starts the exporter.
func mustStartResidSampler() (stop func()) {
	raw, set := os.LookupEnv(residGateEnv)
	log.Print(residEnvLine(raw, set))
	if !set || !residGateOn(raw) {
		return func() {}
	}
	if residSnapshot == nil {
		log.Fatalf("celeris: %s=%q but this binary has no residence stamps (servers/celeris/resid_snapshot.go, built against the stamps branch)", residGateEnv, raw)
	}
	return startResidSampler(func(l string) { log.Print(l) }, residSnapshot, nil)
}
