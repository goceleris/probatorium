//go:build linux

package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestCPUListRoundTrip(t *testing.T) {
	for in, want := range map[string][]int{"0-5,7,9-11": {0, 1, 2, 3, 4, 5, 7, 9, 10, 11}, "3": {3}, "0-1,7": {0, 1, 7}, "2,2,1": {1, 2}} {
		got, err := parseCPUList(in)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("parseCPUList(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if s := formatCPUList([]int{0, 1, 2, 3, 4, 5, 7, 9, 10, 11}); s != "0-5,7,9-11" {
		t.Errorf("formatCPUList = %q", s)
	}
	for _, bad := range []string{"", "a", "5-3", "1,,2"} {
		if _, err := parseCPUList(bad); err == nil {
			t.Errorf("parseCPUList(%q) accepted", bad)
		}
	}
}

// threadLists returns Cpus_allowed_list of every task of this process.
func threadLists(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	dirs, _ := filepath.Glob("/proc/self/task/*")
	for _, d := range dirs {
		b, err := os.ReadFile(filepath.Join(d, "status"))
		if err != nil {
			continue // a thread that exited meanwhile
		}
		for _, l := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(l, "Cpus_allowed_list:"); ok {
				out[filepath.Base(d)] = strings.TrimSpace(v)
			}
		}
	}
	return out
}

// The mechanism the arm relies on, on whatever Linux this runs on (needs >= 4 CPUs): after LockOSThread + narrowing, (1) the thread's mask reads back as the subset,
// (2) threads that already existed keep the full mask, (3) threads the runtime creates AFTERWARDS (even while this locked, narrowed goroutine is the one asking for them)
// get the full mask too, because the runtime clones them from its template thread; so exactly ONE thread of the process has the narrowed list.
func TestNarrowingTouchesOnlyTheListenThread(t *testing.T) {
	was, err := getAffinityList()
	if err != nil || len(was) < 4 {
		t.Skipf("needs >= 4 CPUs (have %v, %v)", was, err)
	}
	// every CPU but the second one and the last one: the analogue of "all but 6 and 8".
	want := slices.DeleteFunc(slices.Clone(was), func(c int) bool { return c == was[1] || c == was[len(was)-1] })
	done := make(chan struct{})
	go func() { // a fresh goroutine, locked and narrowed, like main() before Start()
		defer close(done)
		got, err := narrowCallingThread(want)
		if err != nil {
			t.Errorf("narrow: %v", err)
			return
		}
		if !slices.Equal(got, was) {
			t.Errorf("was %v, want %v", got, was)
		}
		now, _ := getAffinityList()
		if !slices.Equal(now, want) {
			t.Errorf("thread mask %v, want %v", now, want)
		}
		// Force the runtime to create new threads FROM this narrowed, locked thread: goroutines that block in syscalls hold their M, and starting them wakes new Ms.
		var wg sync.WaitGroup
		r, w, _ := os.Pipe()
		for i := 0; i < 24; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var b [1]byte
				_, _ = syscall.Read(int(r.Fd()), b[:]) // blocks in the kernel, pins an M
			}()
		}
		time.Sleep(300 * time.Millisecond)
		lists := threadLists(t)
		full, narrowed, other := 0, 0, map[string]int{}
		wantS, fullS := formatCPUList(want), formatCPUList(was)
		for _, l := range lists {
			switch l {
			case fullS:
				full++
			case wantS:
				narrowed++
			default:
				other[l]++
			}
		}
		if narrowed != 1 || full < 4 || len(other) != 0 {
			t.Errorf("threads: %d with the narrowed list %s (want exactly 1), %d with the full list %s (want >= 4), others %v; all: %v", narrowed, wantS, full, fullS, other, lists)
		}
		_ = w.Close()
		wg.Wait()
		// the goroutine ends while still locked: the runtime then discards its (narrowed) thread.
	}()
	<-done
}

func TestNarrowRefusesACPUOutsideTheMask(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := narrowCallingThread([]int{0, 100000}); err == nil {
			t.Error("a CPU outside the mask was accepted")
		}
		// the thread was locked and (not narrowed): leave it locked so the runtime discards it.
	}()
	<-done
}
