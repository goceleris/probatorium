//go:build linux

// Command idlehold (probatorium tools/idlecell) is the bare-metal form of the tool of
// the celeris#1485 measurement (phase 1 and m1485): it measures the idle cost of a
// native engine that HOLDS N idle keep-alive connections. It uses only the exported
// API of the celeris module it is built against, so one source builds every arm
// (main, head, head-aw, main-skip, old). Added for the bare-metal cell: -power (RAPL
// and the like, read at the window's edges), -perf and -perf-energy (perf stat of this
// process and of the package, started at the window's start), engine none (the idle
// baseline of the host: nothing is served, the window is measured the same way), and
// the WINDOW_BEGIN line. Nothing here changes a host setting: files are read, perf
// counts.
//
// The server side only: a separate process (idleclient) opens the N connections
// from another container that shares this container's network namespace, each one
// sends one GET /ping with the header X-Hold: 1 and then stays idle. When the
// handler has counted N such requests, the tool settles, then measures the same
// window as idlecost does (voluntary switches of the loop threads found by their
// one-CPU affinity, process utime+stime), plus the on-CPU time of every thread
// from /proc/self/task/<tid>/schedstat (nanosecond resolution), split into the
// loop threads and the rest. It prints the IDLE line and the connections' socket
// count (/proc/net/sockstat, both ends of the loopback connections live in the
// same namespace), holds the connections for two more seconds, then shuts down.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/goceleris/celeris"
)

// m1485 additions: timeouts of the measured server and a /json handler.
var cfgIdle, cfgRead time.Duration

// pw and pf are the power sources and the perf runs of the window (see power.go).
var (
	pw powerSet
	pf perfRuns
)

func main() {
	engine := flag.String("engine", "epoll", "epoll, iouring, adaptive, std, or none (no server: the host's idle baseline over the same window)")
	workers := flag.Int("workers", 0, "celeris.Config.Workers (0: the default)")
	window := flag.Duration("window", 20*time.Second, "the idle measurement window")
	settle := flag.Duration("settle", 5*time.Second, "settle after the last connection is counted")
	conns := flag.Int("conns", 0, "idle keep-alive connections the client will open (0: none, the control)")
	addr := flag.String("addr", "127.0.0.1:18080", "listen address")
	flag.DurationVar(&cfgIdle, "idle-timeout", 0, "celeris.Config.IdleTimeout (0: the default)")
	flag.DurationVar(&cfgRead, "read-timeout", 0, "celeris.Config.ReadTimeout (0: the default)")
	power := flag.String("power", "", "power sources, comma separated: name=counter:<energy_uj file>[:<max_energy_range_uj file>] (energy in uJ, read at the window's edges) or name=gauge:<file>:<scale to W> (read once a second); unreadable ones are reported, never fatal")
	flag.StringVar(&pf.statCmd, "perf", "", "perf stat command prefix for this process over the window, e.g. 'taskset -c 4-7 perf stat -x, -e duration_time,instructions:u,instructions:k,cycles:u,cycles:k' (the tool appends -p <pid> -- sleep <window>); empty: not run")
	flag.StringVar(&pf.statOut, "perf-out", "", "file the perf stat output goes to")
	flag.StringVar(&pf.energyCmd, "perf-energy", "", "perf stat command prefix for the package energy, e.g. 'taskset -c 4-7 perf stat -x, -a -e duration_time,power/energy-pkg/' (the tool appends -- sleep <window>); empty: not run")
	flag.StringVar(&pf.energyOut, "perf-energy-out", "", "file the perf energy output goes to")
	flag.Parse()
	pw.parse(*power)
	if err := run(*engine, *workers, *window, *settle, *conns, *addr); err != nil {
		fmt.Fprintln(os.Stderr, "idlehold:", err)
		os.Exit(1)
	}
}

func engineType(name string) (celeris.EngineType, error) {
	switch name {
	case "epoll":
		return celeris.Epoll, nil
	case "iouring":
		return celeris.IOUring, nil
	case "adaptive":
		return celeris.Adaptive, nil
	case "std":
		return celeris.Std, nil
	}
	return 0, fmt.Errorf("unknown engine %q", name)
}

// schedNs returns, per thread, the on-CPU nanoseconds (first field of schedstat).
func schedNs() map[int]int64 {
	m := map[int]int64{}
	for _, tid := range readTasks() {
		b, err := os.ReadFile("/proc/self/task/" + strconv.Itoa(tid) + "/schedstat")
		if err != nil {
			continue
		}
		f := strings.Fields(string(b))
		if len(f) > 0 {
			n, _ := strconv.ParseInt(f[0], 10, 64)
			m[tid] = n
		}
	}
	return m
}

// schedFull returns per thread {on-CPU ns, run-queue wait ns, timeslices} from schedstat.
func schedFull() map[int][3]int64 {
	m := map[int][3]int64{}
	for _, tid := range readTasks() {
		b, err := os.ReadFile("/proc/self/task/" + strconv.Itoa(tid) + "/schedstat")
		if err != nil {
			continue
		}
		f := strings.Fields(string(b))
		if len(f) >= 3 {
			var v [3]int64
			for i := 0; i < 3; i++ {
				v[i], _ = strconv.ParseInt(f[i], 10, 64)
			}
			m[tid] = v
		}
	}
	return m
}

func tcpInuse() string {
	b, err := os.ReadFile("/proc/net/sockstat")
	if err != nil {
		return "?"
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "TCP:") {
			return strings.Join(strings.Fields(l)[1:3], "=")
		}
	}
	return "?"
}

func vmRSSMiB() float64 {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "VmRSS:"); ok {
			f := strings.Fields(v)
			n, _ := strconv.ParseFloat(f[0], 64)
			return n / 1024
		}
	}
	return 0
}

func run(engineName string, workers int, window, settle time.Duration, conns int, addr string) error {
	var held atomic.Int64
	var shutdown func(context.Context) error
	done := make(chan error, 1)
	if engineName == "none" {
		// the idle baseline: nothing is served; the same settle and window, the same power and perf reads.
		shutdown = func(context.Context) error { done <- nil; return nil }
	} else if engineName == "nethttp" {
		// phase 1b: a plain net/http server, no celeris code, no timeouts set.
		hs := &http.Server{Addr: addr, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Hold") == "1" {
				held.Add(1)
			}
			w.Write([]byte("ok"))
		})}
		shutdown = hs.Shutdown
		go func() { done <- hs.ListenAndServe() }()
	} else {
		eng, err := engineType(engineName)
		if err != nil {
			return err
		}
		s := celeris.New(celeris.Config{Engine: eng, Addr: addr, Workers: workers, IdleTimeout: cfgIdle, ReadTimeout: cfgRead})
		s.GET("/ping", func(c *celeris.Context) error {
			if c.Header("x-hold") == "1" {
				held.Add(1)
			}
			return c.String(http.StatusOK, "ok")
		})
		jsonDoc := struct {
			Message string `json:"message"`
			ID      int    `json:"id"`
			OK      bool   `json:"ok"`
		}{"Hello, World!", 42, true}
		s.GET("/json", func(c *celeris.Context) error {
			if c.Header("x-hold") == "1" {
				held.Add(1)
			}
			return c.JSON(http.StatusOK, jsonDoc)
		})
		shutdown = s.Shutdown
		go func() { done <- s.Start() }()
	}

	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	get := func() error {
		resp, err := client.Get("http://" + addr + "/ping")
		if err != nil {
			return err
		}
		return resp.Body.Close()
	}
	ready := engineName == "none"
	for dl := time.Now().Add(15 * time.Second); !ready && time.Now().Before(dl); time.Sleep(20 * time.Millisecond) {
		select {
		case err := <-done:
			return fmt.Errorf("the server stopped before it was ready: %v", err)
		default:
		}
		if err := get(); err == nil {
			ready = true
			break
		}
	}
	if !ready {
		return fmt.Errorf("the server was not ready in 15 s")
	}
	label := fmt.Sprintf("engine=%s workers=%d cpus=%d conns=%d", engineName, workers, cpuCount(), conns)
	fmt.Printf("READY %s unix_ms=%d\n", label, time.Now().UnixMilli())

	// Wait for the client's N connections to have each sent their request.
	if conns > 0 {
		dl := time.Now().Add(180 * time.Second)
		for held.Load() < int64(conns) {
			if time.Now().After(dl) {
				return fmt.Errorf("only %d of %d connections were counted in 180 s", held.Load(), conns)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	fmt.Printf("HELD %s counted=%d unix_ms=%d\n", label, held.Load(), time.Now().UnixMilli())

	time.Sleep(settle)
	// The perf children and the power reads start just before the window's first snapshot, so that their fork and exec
	// are not in the window's CPU (they begin counting a few ms early: perf's own duration_time is what the reader divides by).
	pw.begin()
	pf.begin(window)
	loops := loopThreads()
	allBefore, loopBefore := threadSwitches(loops)
	schedBefore := schedNs()
	cpuBefore := processCPUTicks()
	sockBefore := tcpInuse()
	t0 := time.Now()
	startMs := t0.UnixMilli()
	full0 := schedFull()
	fmt.Printf("WINDOW_BEGIN %s unix_ms=%d\n", label, startMs)
	// One look a second at the gauges (power sources that report a rate), if there are any: nothing else wakes in the window.
	if pw.hasGauges() {
		for next := t0.Add(time.Second); next.Before(t0.Add(window)) || next.Equal(t0.Add(window)); next = next.Add(time.Second) {
			time.Sleep(time.Until(next))
			pw.sampleGauges()
		}
	}
	if d := time.Until(t0.Add(window)); d > 0 {
		time.Sleep(d)
	}
	elapsed := time.Since(t0)
	pw.end(elapsed)
	pf.end()
	full1 := schedFull()
	{
		var lc, lq, ls, oc, oq, os_ float64
		for tid, a := range full1 {
			b := full0[tid]
			d := [3]float64{float64(a[0] - b[0]), float64(a[1] - b[1]), float64(a[2] - b[2])}
			if slices.Contains(loops, tid) {
				lc += d[0]
				lq += d[1]
				ls += d[2]
			} else {
				oc += d[0]
				oq += d[1]
				os_ += d[2]
			}
		}
		sec := elapsed.Seconds()
		fmt.Printf("SCHED2 %s loops: cpu_ms_per_s=%.2f runq_wait_ms_per_s=%.3f timeslices_per_s=%.1f us_per_slice=%.2f | others: cpu_ms_per_s=%.2f runq_wait_ms_per_s=%.3f timeslices_per_s=%.1f\n",
			label, lc/1e6/sec, lq/1e6/sec, ls/sec, lc/1e3/max(ls, 1), oc/1e6/sec, oq/1e6/sec, os_/sec)
	}
	endMs := time.Now().UnixMilli()
	allAfter, loopAfter := threadSwitches(loops)
	schedAfter := schedNs()
	cpuTicks := processCPUTicks() - cpuBefore
	sockAfter := tcpInuse()
	cpuPct := 100 * float64(cpuTicks) / clockTicks() / elapsed.Seconds()
	var perLoop []string
	var loopSum int64
	for i := range loops {
		d := loopAfter[i] - loopBefore[i]
		loopSum += d
		perLoop = append(perLoop, fmt.Sprintf("%.1f", float64(d)/elapsed.Seconds()))
	}
	otherPerSec := float64(allAfter-allBefore-loopSum) / elapsed.Seconds()
	var schedLoop, schedAll int64
	var perLoopCPU []string
	for tid, a := range schedAfter {
		d := a - schedBefore[tid]
		schedAll += d
		if i := slices.Index(loops, tid); i >= 0 {
			schedLoop += d
		}
	}
	for _, tid := range loops {
		perLoopCPU = append(perLoopCPU, fmt.Sprintf("%.3f", 100*float64(schedAfter[tid]-schedBefore[tid])/1e9/elapsed.Seconds()))
	}
	sp := func(ns int64) float64 { return 100 * float64(ns) / 1e9 / elapsed.Seconds() }
	fmt.Printf("IDLE %s window=%v loop_threads=%d process_cpu_pct=%.3f loop_wakeups_per_s=[%s] other_threads_voluntary_per_s=%.1f "+
		"sched_cpu_pct=%.4f sched_loop_cpu_pct=%.4f sched_other_cpu_pct=%.4f sched_per_loop_cpu_pct=[%s] rss_mib=%.0f tcp_before=%s tcp_after=%s win_start_ms=%d win_end_ms=%d loop_cpus=[%s]\n",
		label, elapsed.Round(time.Millisecond), len(loops), cpuPct, strings.Join(perLoop, " "), otherPerSec,
		sp(schedAll), sp(schedLoop), sp(schedAll-schedLoop), strings.Join(perLoopCPU, " "), vmRSSMiB(), sockBefore, sockAfter, startMs, endMs, loopCPUs(loops))

	time.Sleep(2 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second) // phase 1b: v1.5.11 io_uring needs about 53 s with idle connections; not waited for
	defer cancel()
	t1 := time.Now()
	fmt.Printf("SHUTDOWN_BEGIN %s unix_ms=%d\n", label, t1.UnixMilli())
	if err := shutdown(ctx); err != nil {
		fmt.Printf("SHUTDOWN_TIMEOUT %s after=%v err=%v\n", label, time.Since(t1).Round(10*time.Millisecond), err)
		return nil // the measurement is done; the process exit closes what is left
	}
	select {
	case err := <-done:
		fmt.Printf("SHUTDOWN %s shutdown=%v start_err=%v\n", label, time.Since(t1).Round(10*time.Microsecond), err)
	case <-time.After(3 * time.Second):
		fmt.Printf("START_NOT_RETURNED %s 3s after Shutdown returned\n", label) // v1.5.11 only; not waited for
		return nil
	}
	return nil
}

// readTasks lists the thread ids of this process.
func readTasks() []int {
	ents, err := os.ReadDir("/proc/self/task")
	if err != nil {
		return nil
	}
	var tids []int
	for _, e := range ents {
		if tid, err := strconv.Atoi(e.Name()); err == nil {
			tids = append(tids, tid)
		}
	}
	return tids
}

// loopThreads returns the threads pinned to one CPU: the loops'.
func loopThreads() []int {
	var out []int
	for _, tid := range readTasks() {
		b, err := os.ReadFile("/proc/self/task/" + strconv.Itoa(tid) + "/status")
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(line, "Cpus_allowed_list:"); ok {
				if v = strings.TrimSpace(v); !strings.ContainsAny(v, "-,") {
					out = append(out, tid)
				}
			}
		}
	}
	return out
}

// loopCPUs lists the CPU each loop thread is pinned to (Cpus_allowed_list), in the order of the other per-loop figures, so that
// the record shows where the loops ran relative to the taskset mask of the server and of the holder.
func loopCPUs(loops []int) string {
	var out []string
	for _, tid := range loops {
		b, err := os.ReadFile("/proc/self/task/" + strconv.Itoa(tid) + "/status")
		if err != nil {
			out = append(out, "?")
			continue
		}
		cpu := "?"
		for _, line := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(line, "Cpus_allowed_list:"); ok {
				cpu = strings.TrimSpace(v)
			}
		}
		out = append(out, cpu)
	}
	return strings.Join(out, " ")
}

// threadSwitches returns the voluntary context switches of every thread of the
// process in all, and of each of the given threads in loops, in order.
func threadSwitches(loops []int) (all int64, perLoop []int64) {
	for _, tid := range readTasks() {
		n := voluntary(tid)
		all += n
		if i := slices.Index(loops, tid); i >= 0 {
			for len(perLoop) <= i {
				perLoop = append(perLoop, 0)
			}
			perLoop[i] = n
		}
	}
	for len(perLoop) < len(loops) {
		perLoop = append(perLoop, 0)
	}
	return all, perLoop
}

func voluntary(tid int) int64 {
	b, err := os.ReadFile("/proc/self/task/" + strconv.Itoa(tid) + "/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "voluntary_ctxt_switches:"); ok {
			n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			return n
		}
	}
	return 0
}

// processCPUTicks is utime plus stime of the process, in clock ticks.
func processCPUTicks() int64 {
	b, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return 0
	}
	s := string(b)
	// The command name is in parentheses and may hold spaces: fields count from after the last ')'.
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0
	}
	f := strings.Fields(s[i+1:])
	// f[0] is the state (field 3); utime and stime are fields 14 and 15.
	if len(f) < 13 {
		return 0
	}
	u, _ := strconv.ParseInt(f[11], 10, 64)
	st, _ := strconv.ParseInt(f[12], 10, 64)
	return u + st
}

// clockTicks is the kernel's USER_HZ, 100 on every Linux the module supports.
func clockTicks() float64 { return 100 }

// cpuCount is the number of CPUs the process may run on, as the runtime read it
// at start (before any loop thread was pinned).
func cpuCount() int { return runtime.NumCPU() }
