//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// powerSrc is one place the host reports energy or power: a counter (energy in microjoules, read at the window's
// edges; the RAPL powercap energy_uj and a hwmon energy*_input are of this kind) or a gauge (a rate, read once a
// second and averaged: a hwmon power*_input, in microwatts, is scaled to watts by `scale`).
type powerSrc struct {
	name, kind, path, rangePath string
	scale                       float64
	start, end, max             uint64
	err                         string
	gauges                      []float64
}

type powerSet struct {
	srcs []*powerSrc
}

// parse reads "name=counter:<file>[:<range file>],name=gauge:<file>:<scale>". A malformed entry is kept as an
// unreadable source so that the INDEX says so, rather than being dropped.
func (p *powerSet) parse(spec string) {
	for _, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		name, rest, ok := strings.Cut(item, "=")
		parts := strings.Split(rest, ":")
		s := &powerSrc{name: name, scale: 1}
		switch {
		case !ok || len(parts) < 2:
			s.kind, s.err = "bad", "malformed -power entry "+strconv.Quote(item)
		case parts[0] == "counter":
			s.kind, s.path = "counter", parts[1]
			if len(parts) > 2 {
				s.rangePath = parts[2]
			}
		case parts[0] == "gauge":
			s.kind, s.path = "gauge", parts[1]
			if len(parts) > 2 {
				if v, err := strconv.ParseFloat(parts[2], 64); err == nil {
					s.scale = v
				}
			}
		default:
			s.kind, s.err = "bad", "unknown kind in "+strconv.Quote(item)
		}
		p.srcs = append(p.srcs, s)
	}
}

func readUint(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}

func (p *powerSet) hasGauges() bool {
	for _, s := range p.srcs {
		if s.kind == "gauge" && s.err == "" {
			return true
		}
	}
	return false
}

func (p *powerSet) begin() {
	for _, s := range p.srcs {
		if s.kind != "counter" || s.err != "" {
			continue
		}
		v, err := readUint(s.path)
		if err != nil {
			s.err = err.Error()
			continue
		}
		s.start = v
		if s.rangePath != "" {
			if m, err := readUint(s.rangePath); err == nil {
				s.max = m
			}
		}
	}
	p.sampleGauges()
}

func (p *powerSet) sampleGauges() {
	for _, s := range p.srcs {
		if s.kind != "gauge" || s.err != "" {
			continue
		}
		v, err := readUint(s.path)
		if err != nil {
			s.err = err.Error()
			continue
		}
		s.gauges = append(s.gauges, float64(v)*s.scale)
	}
}

// end prints one line per source. ENERGY (counters): joules and mean watts over the window; POWER (gauges): the
// mean of the once-a-second looks. With no source at all it says so: "none" is an answer, not a gap.
func (p *powerSet) end(elapsed time.Duration) {
	sec := elapsed.Seconds()
	if len(p.srcs) == 0 {
		fmt.Printf("ENERGY none reason=%q\n", "no power source was given (the host exposes none that this user can read)")
		return
	}
	for _, s := range p.srcs {
		switch {
		case s.kind == "counter" && s.err == "":
			v, err := readUint(s.path)
			if err != nil {
				s.err = err.Error()
				break
			}
			s.end = v
			d := s.end - s.start
			if s.end < s.start && s.max > 0 {
				d = s.end + s.max - s.start // the counter wrapped once
			}
			fmt.Printf("ENERGY src=%s kind=counter start_uj=%d end_uj=%d max_range_uj=%d window_s=%.3f joules=%.4f watts=%.4f\n",
				s.name, s.start, s.end, s.max, sec, float64(d)/1e6, float64(d)/1e6/sec)
		case s.kind == "gauge" && s.err == "" && len(s.gauges) > 0:
			mn, mx, sum := s.gauges[0], s.gauges[0], 0.0
			for _, g := range s.gauges {
				mn, mx, sum = min(mn, g), max(mx, g), sum+g
			}
			fmt.Printf("POWER src=%s kind=gauge n=%d mean_w=%.4f min_w=%.4f max_w=%.4f\n", s.name, len(s.gauges), sum/float64(len(s.gauges)), mn, mx)
		}
		if s.err != "" {
			fmt.Printf("ENERGY src=%s kind=%s error=%q\n", s.name, s.kind, s.err)
		}
	}
}

// perfRuns are the perf stat children of the window: this process's instructions and cycles, and the package
// energy. Each is started at the window's start and ends with its own `sleep <window>`; the tool writes their
// output, success or not, to the given files, and the reader says what was not counted.
type perfRuns struct {
	statCmd, statOut, energyCmd, energyOut string
	runs                                   []*perfRun
}

type perfRun struct {
	cmd *exec.Cmd
	out *os.File
}

func (f *perfRuns) start(prefix, outPath string, extra []string, window time.Duration) {
	if prefix == "" || outPath == "" {
		return
	}
	out, err := os.Create(outPath)
	if err != nil {
		fmt.Printf("PERF_ERROR cmd=%q err=%q\n", prefix, err)
		return
	}
	fields := strings.Fields(prefix)
	args := append(append(fields[1:len(fields):len(fields)], extra...), "--", "sleep", strconv.FormatFloat(window.Seconds(), 'f', -1, 64))
	c := exec.Command(fields[0], args...)
	c.Stderr, c.Stdout = out, out
	if err := c.Start(); err != nil {
		fmt.Fprintf(out, "PERF_START_ERROR %v\n", err)
		_ = out.Close()
		return
	}
	f.runs = append(f.runs, &perfRun{c, out})
}

func (f *perfRuns) begin(window time.Duration) {
	f.start(f.statCmd, f.statOut, []string{"-p", strconv.Itoa(os.Getpid())}, window)
	f.start(f.energyCmd, f.energyOut, nil, window)
}

// end waits for the perf children (they finish a moment after the window) and kills one that does not.
func (f *perfRuns) end() {
	for _, r := range f.runs {
		fin := make(chan error, 1)
		go func() { fin <- r.cmd.Wait() }()
		select {
		case err := <-fin:
			if err != nil {
				fmt.Fprintf(r.out, "PERF_EXIT %v\n", err)
			}
		case <-time.After(10 * time.Second):
			_ = r.cmd.Process.Kill()
			<-fin
			fmt.Fprintln(r.out, "PERF_EXIT killed after 10 s")
		}
		_ = r.out.Close()
	}
	f.runs = nil
}
