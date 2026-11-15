//go:build linux

package main

// Linux half of the L756E affinity knob (see l756e_knobs.go): sched_setaffinity on every tid of /proc/self/task, then a verification pass over each tid's own
// Cpus_allowed_list.

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

func listTids() ([]int, error) {
	ents, err := os.ReadDir("/proc/self/task")
	if err != nil {
		return nil, err
	}
	tids := make([]int, 0, len(ents))
	for _, e := range ents {
		if n, err := strconv.Atoi(e.Name()); err == nil {
			tids = append(tids, n)
		}
	}
	return tids, nil
}

func setTidAffinity(tid int, mask *[16]uint64) error {
	_, _, errno := syscall.RawSyscall(syscall.SYS_SCHED_SETAFFINITY, uintptr(tid), unsafe.Sizeof(*mask), uintptr(unsafe.Pointer(mask)))
	if errno != 0 {
		return errno
	}
	return nil
}

// tidAllowedList is the Cpus_allowed_list line of /proc/self/task/<tid>/status ("" when unreadable).
func tidAllowedList(tid int) string {
	b, err := os.ReadFile("/proc/self/task/" + strconv.Itoa(tid) + "/status")
	if err != nil {
		return ""
	}
	for _, ln := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(ln, "Cpus_allowed_list:"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func processAllowedList() string { return tidAllowedList(os.Getpid()) }

// applyAffinityAllThreads restricts every thread to st.lo..st.hi: pass 1 over the tids seen, pass 2 over tids that appeared meanwhile; then it verifies every tid.
func applyAffinityAllThreads(st *affinityState) error {
	mask := affinityMask(st.lo, st.hi)
	want := fmt.Sprintf("%d-%d", st.lo, st.hi)
	if st.lo == st.hi {
		want = strconv.Itoa(st.lo)
	}
	done := map[int]bool{}
	for pass := 0; pass < 2; pass++ {
		tids, err := listTids()
		if err != nil {
			return fmt.Errorf("list /proc/self/task: %w", err)
		}
		for _, t := range tids {
			if done[t] {
				continue
			}
			if err := setTidAffinity(t, &mask); err != nil {
				return fmt.Errorf("sched_setaffinity(tid %d, %s): %w", t, want, err)
			}
			done[t] = true
		}
	}
	tids, err := listTids()
	if err != nil {
		return fmt.Errorf("list /proc/self/task: %w", err)
	}
	st.tids, st.set = len(tids), len(done)
	for _, t := range tids {
		if tidAllowedList(t) == want {
			st.checked++
		}
	}
	if st.checked != st.tids {
		return fmt.Errorf("after the pass %d of %d threads carry Cpus_allowed_list %s (a thread is outside the restriction)", st.checked, st.tids, want)
	}
	return nil
}
