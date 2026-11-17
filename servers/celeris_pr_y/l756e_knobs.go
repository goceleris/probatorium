package main

// The affinity knob and the GOMAXPROCS report of the L756E experiment (the #756 amd64 "free Ps x free CPUs" 2x2; THROWAWAY head ab/l756e-diag-20261114, not for merge).
//
// PROBATORIUM_AFFINITY=<lo>-<hi> (one decimal CPU range: the registry env value charset has no comma) restricts EVERY thread of this process to the CPUs lo..hi, once, at
// start, BEFORE celeris.New, so that the engine's own CPU plan (it reads the process mask) sees the restricted mask. Threads created afterwards inherit the mask of the thread
// that creates them, which is restricted too. It is the in-process equivalent of `taskset -c lo-hi <server>` (the harness cannot taskset: the SUT is launched by ansible). Arm C
// of the experiment: 14 loops on CPUs 0-13 and every other thread (GC workers, the netpoller's Ms, handler goroutines' Ms, sysmon) confined to the same 14 CPUs, so no CPU
// is free of a pinned loop thread while two Ps stay free (GOMAXPROCS=16 is set explicitly: the Go runtime re-derives an unset default from the affinity mask).
//
// GOMAXPROCS is the Go runtime's own env var and is NOT touched here; gomaxprocsLine only reports it.
//
// Two startup lines, always printed, after the [workers] line (formats are a contract with l756e-validate.py, pinned by TestL756ELineFormats):
//
//	[affinity] env=<raw|unset> range=<lo-hi|none> tids=<n> set=<n> verified=<n> allowed=<Cpus_allowed_list of the process>
//	[gomaxprocs] env=<raw|unset> runtime=<n> numcpu=<n>
//
// verified counts the threads whose own Cpus_allowed_list equals the range after the pass (a second pass re-applies the mask to a thread that appeared during the first). A set
// value that is malformed, over 1023, or the env on a non-Linux host is a fatal start error, never a silent default; a pass that leaves a thread unrestricted is fatal too.

import (
	"fmt"
	"log"
	"os"
	"regexp"
	"runtime"
	"strconv"
)

const affinityEnv = "PROBATORIUM_AFFINITY"

var affinityValueRE = regexp.MustCompile(`^(0|[1-9][0-9]{0,3})-(0|[1-9][0-9]{0,3})$`)

// parseAffinity parses "<lo>-<hi>" (decimal, lo <= hi <= 1023).
func parseAffinity(raw string) (lo, hi int, err error) {
	if raw == "" {
		return 0, 0, fmt.Errorf("%s is set but empty (unset it for the default)", affinityEnv)
	}
	m := affinityValueRE.FindStringSubmatch(raw)
	if m == nil {
		return 0, 0, fmt.Errorf("%s=%q: want one decimal CPU range <lo>-<hi>", affinityEnv, raw)
	}
	lo, _ = strconv.Atoi(m[1])
	hi, _ = strconv.Atoi(m[2])
	if lo > hi || hi > 1023 {
		return 0, 0, fmt.Errorf("%s=%q: want lo <= hi <= 1023", affinityEnv, raw)
	}
	return lo, hi, nil
}

// affinityMask is the sched_setaffinity mask (1024 bits) of CPUs lo..hi.
func affinityMask(lo, hi int) [16]uint64 {
	var m [16]uint64
	for c := lo; c <= hi; c++ {
		m[c/64] |= 1 << (uint(c) % 64)
	}
	return m
}

// affinityState is what the startup line reports.
type affinityState struct {
	env     string
	envSet  bool
	lo, hi  int
	tids    int
	set     int
	checked int
	allowed string
}

// affinityLine is the startup line (after the log package's own timestamp).
func (st affinityState) line() string {
	env, rng := "unset", "none"
	if st.envSet {
		env = st.env
		rng = fmt.Sprintf("%d-%d", st.lo, st.hi)
	}
	allowed := st.allowed
	if allowed == "" {
		allowed = "na"
	}
	return fmt.Sprintf("[affinity] env=%s range=%s tids=%d set=%d verified=%d allowed=%s", env, rng, st.tids, st.set, st.checked, allowed)
}

// mustApplyAffinityKnob applies PROBATORIUM_AFFINITY (when set) to every thread; fatal on any error.
func mustApplyAffinityKnob() affinityState {
	var st affinityState
	st.env, st.envSet = os.LookupEnv(affinityEnv)
	if st.envSet {
		lo, hi, err := parseAffinity(st.env)
		if err != nil {
			log.Fatalf("celeris: affinity knob: %v", err)
		}
		st.lo, st.hi = lo, hi
		if err := applyAffinityAllThreads(&st); err != nil {
			log.Fatalf("celeris: affinity knob: %v", err)
		}
	}
	st.allowed = processAllowedList()
	return st
}

// gomaxprocsLine reports the Go runtime's GOMAXPROCS and the CPU count it saw at start.
func gomaxprocsLine() string {
	env, ok := os.LookupEnv("GOMAXPROCS")
	if !ok {
		env = "unset"
	}
	return fmt.Sprintf("[gomaxprocs] env=%s runtime=%d numcpu=%d", env, runtime.GOMAXPROCS(0), runtime.NumCPU())
}
