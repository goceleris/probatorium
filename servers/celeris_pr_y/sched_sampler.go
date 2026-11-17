package main

// The 1 Hz in-process scheduler sampler of the bench SUT (P3a; THROWAWAY head ab/p3a-rc1-20261110,
// not for merge). Descriptive scrape only: it feeds no verdict.
//
// Armed by PROBATORIUM_SCHED_SAMPLE=1 (unset or 0 = off, anything else is a fatal start error).
// Once per second it writes ONE line to the SUT's log (so into server.log):
//
//	[sched] <epoch s> lat_n=<N|na> lat=<lo>:<n>,<lo>:<n>,...|-|na pin_n=<k|na> pin_run_ns=<d> pin_wait_ns=<d> pin_slices=<d> oth_n=<m> oth_run_ns=<d> oth_wait_ns=<d> oth_slices=<d>
//
//   - lat_n / lat: the DELTA since the previous sample of the runtime/metrics histogram
//     /sched/latencies:seconds (time goroutines spent runnable before running): the total count
//     and the non-zero buckets as <lower bound in seconds, %g>:<count> (the first bucket's lower
//     bound prints as -Inf); "-" = no change in the second; "na" = the metric is unavailable.
//   - pin_* / oth_*: the DELTA of /proc/self/task/<tid>/schedstat (run ns, wait ns, timeslices)
//     summed over the threads whose Cpus_allowed_list is a single CPU (the pinned loop threads of
//     the epoll/io_uring engines: pin) and over all other threads (oth); *_n is the number of
//     threads of the class seen in this sample; "na" = /proc/self/task is not readable (not
//     Linux). Per-thread deltas, so a thread that appears in the interval counts from zero and a
//     thread that exited during it is lost (its last partial interval is not counted).
//
// THE SAMPLER MUST STOP BEFORE THE SUT'S "signal received" LINE. The validators' CRASH rule needs
// that line to be the last non-blank line before "[respawn]"; a sampler line after it voids the
// column. stop() is therefore SYNCHRONOUS: it tells the goroutine to stop and WAITS until it has
// left, and the goroutine re-checks the stop signal immediately before every write, so nothing is
// written once stop() has returned, whatever ticks are still pending. It logs nothing itself.
// server.go calls it as the first statement after the signal arrives (a test pins the order).
//
// Like busypoll_knob.go this file compiles against every celeris (it names no celeris symbol) and
// is byte-identical in the mirror modules.

import (
	"fmt"
	"log"
	"math"
	"os"
	"runtime/metrics"
	"strconv"
	"strings"
	"sync"
	"time"
)

// schedSampleEnv arms the sampler. The registry carries it as "1" on every celeris and std-h1 column.
const schedSampleEnv = "PROBATORIUM_SCHED_SAMPLE"

const (
	schedLatMetric = "/sched/latencies:seconds"
	schedNA        = "na"
)

// parseSchedSample parses the env value: "1" arms, "0" is an explicit off; anything else is an error.
func parseSchedSample(raw string) (bool, error) {
	switch raw {
	case "1":
		return true, nil
	case "0":
		return false, nil
	}
	return false, fmt.Errorf("%s=%q: want 1 (on) or 0 (off); unset it for the default (off)", schedSampleEnv, raw)
}

// threadSched is one thread's cumulative /proc schedstat and its affinity class.
type threadSched struct {
	run, wait, slices uint64
	pinned            bool
}

// schedSampler holds the previous sample (the deltas are taken against it).
type schedSampler struct {
	emit    func(string)
	hist    func() (counts []uint64, buckets []float64, ok bool)
	threads func() (map[int]threadSched, bool)
	now     func() time.Time

	prevCounts  []uint64
	prevThreads map[int]threadSched
}

// baseline records the starting point; the first tick then reports the first interval.
func (s *schedSampler) baseline() {
	if c, _, ok := s.hist(); ok {
		s.prevCounts = append([]uint64(nil), c...)
	}
	if th, ok := s.threads(); ok {
		s.prevThreads = th
	}
}

// sample builds the line of one tick and advances the previous sample.
func (s *schedSampler) sample() string {
	var b strings.Builder
	fmt.Fprintf(&b, "[sched] %d", s.now().Unix())

	if c, bk, ok := s.hist(); ok && len(bk) == len(c)+1 {
		var n uint64
		var parts []string
		if len(s.prevCounts) != len(c) { // first sample or a bucket layout change: report from zero
			s.prevCounts = make([]uint64, len(c))
		}
		for i := range c {
			d := c[i] - s.prevCounts[i]
			if c[i] < s.prevCounts[i] {
				d = 0
			}
			if d > 0 {
				n += d
				parts = append(parts, fmt.Sprintf("%s:%d", fmtBound(bk[i]), d))
			}
		}
		s.prevCounts = append(s.prevCounts[:0], c...)
		lat := "-"
		if len(parts) > 0 {
			lat = strings.Join(parts, ",")
		}
		fmt.Fprintf(&b, " lat_n=%d lat=%s", n, lat)
	} else {
		fmt.Fprintf(&b, " lat_n=%s lat=%s", schedNA, schedNA)
	}

	if cur, ok := s.threads(); ok {
		var pin, oth struct {
			n                 int
			run, wait, slices uint64
		}
		for tid, t := range cur {
			d := t
			if p, seen := s.prevThreads[tid]; seen {
				d.run, d.wait, d.slices = sub0(t.run, p.run), sub0(t.wait, p.wait), sub0(t.slices, p.slices)
			}
			acc := &oth
			if t.pinned {
				acc = &pin
			}
			acc.n++
			acc.run += d.run
			acc.wait += d.wait
			acc.slices += d.slices
		}
		s.prevThreads = cur
		fmt.Fprintf(&b, " pin_n=%d pin_run_ns=%d pin_wait_ns=%d pin_slices=%d oth_n=%d oth_run_ns=%d oth_wait_ns=%d oth_slices=%d",
			pin.n, pin.run, pin.wait, pin.slices, oth.n, oth.run, oth.wait, oth.slices)
	} else {
		fmt.Fprintf(&b, " pin_n=%[1]s pin_run_ns=%[1]s pin_wait_ns=%[1]s pin_slices=%[1]s oth_n=%[1]s oth_run_ns=%[1]s oth_wait_ns=%[1]s oth_slices=%[1]s", schedNA)
	}
	return b.String()
}

func sub0(a, b uint64) uint64 {
	if a < b {
		return 0
	}
	return a - b
}

func fmtBound(f float64) string {
	switch {
	case math.IsInf(f, -1):
		return "-Inf"
	case math.IsInf(f, 1):
		return "+Inf"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// runtimeSchedHist reads the runtime/metrics histogram.
func runtimeSchedHist() ([]uint64, []float64, bool) {
	s := []metrics.Sample{{Name: schedLatMetric}}
	metrics.Read(s)
	if s[0].Value.Kind() != metrics.KindFloat64Histogram {
		return nil, nil, false
	}
	h := s[0].Value.Float64Histogram()
	return h.Counts, h.Buckets, true
}

// procThreads reads /proc/self/task/*: schedstat and the affinity class of every thread.
func procThreads() (map[int]threadSched, bool) {
	ents, err := os.ReadDir("/proc/self/task")
	if err != nil {
		return nil, false
	}
	out := make(map[int]threadSched, len(ents))
	for _, e := range ents {
		tid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile("/proc/self/task/" + e.Name() + "/schedstat")
		if err != nil {
			continue // the thread exited between the listing and the read
		}
		f := strings.Fields(string(raw))
		if len(f) < 3 {
			continue
		}
		run, e1 := strconv.ParseUint(f[0], 10, 64)
		wait, e2 := strconv.ParseUint(f[1], 10, 64)
		sl, e3 := strconv.ParseUint(f[2], 10, 64)
		if e1 != nil || e2 != nil || e3 != nil {
			continue
		}
		out[tid] = threadSched{run: run, wait: wait, slices: sl, pinned: threadPinned(e.Name())}
	}
	return out, true
}

// threadPinned: the thread's Cpus_allowed_list is a single CPU.
func threadPinned(tid string) bool {
	raw, err := os.ReadFile("/proc/self/task/" + tid + "/status")
	if err != nil {
		return false
	}
	for _, ln := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(ln, "Cpus_allowed_list:"); ok {
			v = strings.TrimSpace(v)
			return v != "" && !strings.ContainsAny(v, "-,")
		}
	}
	return false
}

// startSchedSampler starts the goroutine. ticks is the tick source (a test injects its own; nil =
// a 1 s ticker, stopped with the goroutine). The returned stop is synchronous and idempotent.
func startSchedSampler(s *schedSampler, ticks <-chan time.Time) (stop func()) {
	release := func() {}
	if ticks == nil {
		tk := time.NewTicker(time.Second)
		ticks, release = tk.C, tk.Stop
	}
	return runSchedSampler(s, ticks, release)
}

func runSchedSampler(s *schedSampler, ticks <-chan time.Time, release func()) (stop func()) {
	s.baseline()
	done := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		defer release()
		defer func() {
			if r := recover(); r != nil {
				// a sampler bug must never take the SUT down; say so once (before the stop, never after).
				select {
				case <-done:
				default:
					s.emit(fmt.Sprintf("[sched] sampler stopped: panic: %v", r))
				}
			}
		}()
		for {
			select {
			case <-done:
				return
			case <-ticks:
				select { // a tick that raced the stop must not write
				case <-done:
					return
				default:
				}
				s.emit(s.sample())
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(done) })
		<-exited
	}
}

// mustStartSchedSampler is the server's entry: PROBATORIUM_SCHED_SAMPLE unset or 0 = a no-op stop;
// 1 = the sampler writing to the log; anything else is a fatal start error.
func mustStartSchedSampler() (stop func()) {
	raw, set := os.LookupEnv(schedSampleEnv)
	if !set {
		return func() {}
	}
	on, err := parseSchedSample(raw)
	if err != nil {
		log.Fatalf("celeris: sched sampler: %v", err)
	}
	if !on {
		return func() {}
	}
	return startSchedSampler(&schedSampler{
		emit:    func(l string) { log.Print(l) },
		hist:    runtimeSchedHist,
		threads: procThreads,
		now:     time.Now,
	}, nil)
}
