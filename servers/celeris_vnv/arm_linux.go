//go:build linux

package main

// P2b arm "vnv" (throwaway probatorium branch ab/p2b-io512-3arm-38, not for merge; evidence EV/lanes-20261025/P2B/): the old PINPLAN "V-NV" loop placement, forced
// from the harness on UNMODIFIED celeris main 713b40d (the pin of servers/celeris, byte for byte the same go.mod/go.sum apart from the module path).
//
// V-NV = 6 loops, one pinned to each of the CPUs 0,1,7,9,10,11; no loop on 6 and 8 (the CPUs that service the enp49s0 receive vectors); the little CPUs 2-5 avoided; the Go
// runtime's other threads free over 0-11. PINPLAN4-6 forced it with CELERIS_AB_PLAN_CPUS, a knob of a throwaway celeris branch (never intended to ship) that does not exist at
// 713b40d. Here nothing of celeris is touched: celeris plans its loops from the allowed-CPU mask of the thread that calls Listen (internal/platform PlanWorkerCPUs: "the mask
// of the thread that calls Listen, which is the process's mask unless something narrowed that one thread"), so this arm narrows exactly that thread, just before srv.Start(),
// to the CPUs {0-5,7,9-11} = every CPU but 6 and 8. The littles 2-5 stay in the set ON PURPOSE: the planner still sees the heterogeneous capacities, still avoids them, and
// plans 6 loops on the remaining big CPUs 0,1,7,9,10,11 with the standing reason text (requested=12, workers=6, pinned=6, allowed=0-5,7,9-11) and without the rule-D fields.
// Workers is NOT set (an explicit Workers changes requested= and the Resolve()-derived sizing).
//
// Why only the Listen thread and why LockOSThread comes FIRST: the narrowing is per thread (sched_setaffinity(0)), and a new OS thread inherits the mask of the thread that
// clones it. The Go runtime never clones from a thread the user locked (runtime/proc.go newm: "We're on a locked M ... ask a known-good thread to create the thread for us",
// the template thread, which is created by the FIRST LockOSThread call, from a thread that is still unnarrowed). So: lock, then narrow, and every other thread of the process
// (GC workers, netpoll, the 6 loop threads before they pin themselves) keeps the mask 0-11. The Listen thread is the only thread whose list stays 0-5,7,9-11; the loop threads
// pin themselves to one CPU each (celeris restores nothing on them), so the validator expects, per column: single-CPU masks exactly 0,1,7,9,10,11, exactly one thread with the
// list 0-5,7,9-11, and every other thread 0-11.
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

var armListenCPUs = "0-5,7,9-11"

// armBeforeStart narrows the calling (about to Listen) thread and logs the proof line. It never returns on failure.
func armBeforeStart() {
	want, err := parseCPUList(armListenCPUs)
	if err != nil {
		log.Fatalf("[vnv-arm] bad CPU list %q: %v", armListenCPUs, err)
	}
	was, err := narrowCallingThread(want)
	if err != nil {
		log.Fatalf("[vnv-arm] cannot narrow the Listen thread to %s: %v", armListenCPUs, err)
	}
	log.Printf("[vnv-arm] pid=%d listen tid=%d narrowed from %s to %s (celeris plans from this mask; every other thread keeps %s)",
		os.Getpid(), syscall.Gettid(), formatCPUList(was), formatCPUList(want), formatCPUList(was))
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
