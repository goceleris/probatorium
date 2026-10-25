//go:build linux

package main

// P2c arm "noc8" (throwaway probatorium branch ab/p2c-io512-discriminate-38, not for merge; evidence EV/lanes-20261025/P2C/): celeris main 713b40d with NO loop on CPU 8 and
// nothing else changed. Derived from the P2b "vnv" arm (servers/celeris_vnv), same mechanism, other set.
//
// Question it serves (STALL-FORENSICS-r1.md): in every io512 stall of main the victim loop sat on CPU 8, the TX-completion vector's CPU (about 90 % softirq even when clean).
// If a loop on CPU 8 is NECESSARY for the stall, a plan without it stalls less. Celeris plans its loops from the allowed-CPU mask of the thread that calls Listen
// (internal/platform PlanWorkerCPUs: "the mask of the thread that calls Listen, which is the process's mask unless something narrowed that one thread"), so this arm narrows
// exactly that thread, just before srv.Start(), to every CPU but 8: {0-7,9-11}. The planner (unmodified 713b40d) still avoids the littles 2-5 and is expected to plan 7 loops on
// 0,1,6,7,9,10,11 (main's 8 minus CPU 8; the preparation checked it against the real msr1 snapshot, see EV/lanes-20261025/P2C/p2c-logs/celeris-plan-lines.out). Workers is NOT set.
// This arm has ONE LOOP FEWER than main (7 against 8): the other loops carry 512/7 = 73 connections each instead of 64. That is part of "no loop on CPU 8" and is reported, not removed.
//
// Why only the Listen thread and why LockOSThread comes FIRST: the narrowing is per thread (sched_setaffinity(0)), and a new OS thread inherits the mask of the thread that
// clones it. The Go runtime never clones from a thread the user locked (runtime/proc.go newm: "We're on a locked M ... ask a known-good thread to create the thread for us",
// the template thread, which is created by the FIRST LockOSThread call, from a thread that is still unnarrowed). So: lock, then narrow, and every other thread of the process
// keeps the mask 0-11. The Listen thread is the only thread whose list stays 0-7,9-11; the loop threads pin themselves to one CPU each, so the validator expects, per column:
// single-CPU masks exactly 0,1,6,7,9,10,11 (once each), exactly one thread with the list 0-7,9-11, and every other thread 0-11.
//
// Fail closed: a narrowing that does not read back as requested, or a CPU that is not in the original mask, is log.Fatalf (the column then has no server and comes back VOID,
// never a silently different arm). The constant is a variable only so that the container test can substitute a set that fits an 8-CPU Docker VM (-ldflags -X); nothing in the
// run environment can change it.

import (
	"fmt"
	"log"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

var armListenCPUs = "0-7,9-11"

// armBeforeStart narrows the calling (about to Listen) thread, logs the proof line and starts the accepted-socket probe (probe_linux.go). It never returns on failure.
func armBeforeStart(bind string) {
	want, err := parseCPUList(armListenCPUs)
	if err != nil {
		log.Fatalf("[noc8-arm] bad CPU list %q: %v", armListenCPUs, err)
	}
	was, err := narrowCallingThread(want)
	if err != nil {
		log.Fatalf("[noc8-arm] cannot narrow the Listen thread to %s: %v", armListenCPUs, err)
	}
	log.Printf("[noc8-arm] pid=%d listen tid=%d narrowed from %s to %s (celeris plans from this mask; every other thread keeps %s)",
		os.Getpid(), syscall.Gettid(), formatCPUList(was), formatCPUList(want), formatCPUList(was))
	go runAcceptedSocketProbe("noc8", bind)
}

// narrowCallingThread locks the calling goroutine to its OS thread (FIRST: see the file comment) and sets that thread's mask to want, which must be a subset of the mask it
// had. It returns the mask the thread had and verifies the readback.
func narrowCallingThread(want []int) (was []int, err error) {
	runtime.LockOSThread()
	was, err = getAffinityList()
	if err != nil {
		return nil, err
	}
	for _, c := range want {
		if !slices.Contains(was, c) {
			return was, fmt.Errorf("CPU %d is not in the thread's mask %s", c, formatCPUList(was))
		}
	}
	if err := setAffinityList(want); err != nil {
		return was, err
	}
	got, err := getAffinityList()
	if err != nil {
		return was, err
	}
	if !slices.Equal(got, want) {
		return was, fmt.Errorf("readback %s != requested %s", formatCPUList(got), formatCPUList(want))
	}
	return was, nil
}

const maskBytes = 128 // 1024 CPUs, enough for any bench host

func getAffinityList() ([]int, error) {
	var buf [maskBytes]byte
	n, _, e := syscall.RawSyscall(syscall.SYS_SCHED_GETAFFINITY, 0, uintptr(len(buf)), uintptr(unsafe.Pointer(&buf[0])))
	if e != 0 {
		return nil, fmt.Errorf("sched_getaffinity: %v", e)
	}
	var out []int
	for i := 0; i < int(n)*8; i++ {
		if buf[i/8]&(1<<(uint(i)%8)) != 0 {
			out = append(out, i)
		}
	}
	return out, nil
}

func setAffinityList(cpus []int) error {
	var buf [maskBytes]byte
	for _, c := range cpus {
		if c < 0 || c >= maskBytes*8 {
			return fmt.Errorf("CPU %d out of range", c)
		}
		buf[c/8] |= 1 << (uint(c) % 8)
	}
	_, _, e := syscall.RawSyscall(syscall.SYS_SCHED_SETAFFINITY, 0, uintptr(len(buf)), uintptr(unsafe.Pointer(&buf[0])))
	if e != 0 {
		return fmt.Errorf("sched_setaffinity: %v", e)
	}
	return nil
}

// parseCPUList parses the kernel's list format ("0-5,7,9-11"), sorted, no duplicates.
func parseCPUList(s string) ([]int, error) {
	var out []int
	for _, part := range strings.Split(strings.TrimSpace(s), ",") {
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(lo)
		if err != nil {
			return nil, fmt.Errorf("%q: %v", part, err)
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil || b < a {
				return nil, fmt.Errorf("%q is not a range", part)
			}
		}
		for c := a; c <= b; c++ {
			out = append(out, c)
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// formatCPUList is the inverse (the kernel's Cpus_allowed_list spelling).
func formatCPUList(cpus []int) string {
	var parts []string
	for i := 0; i < len(cpus); {
		j := i
		for j+1 < len(cpus) && cpus[j+1] == cpus[j]+1 {
			j++
		}
		if j > i {
			parts = append(parts, fmt.Sprintf("%d-%d", cpus[i], cpus[j]))
		} else {
			parts = append(parts, strconv.Itoa(cpus[i]))
		}
		i = j + 1
	}
	return strings.Join(parts, ",")
}
