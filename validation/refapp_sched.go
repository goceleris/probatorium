package validation

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// The refapp's CPU and scheduling state, as text, for the ws_echo timeout
// diagnosis (probatorium#481).
//
// Why per thread. /proc/<pid>/status reports the CPU mask of the thread-group
// LEADER, not of the process. celeris pins each engine loop's OS thread to one
// CPU (platform.PinToCPU: LockOSThread + sched_setaffinity on that thread),
// and the process's main thread can be one of them, so a refapp whose leader
// reads "Cpus_allowed_list: 0" can still have every other thread free to run
// on every CPU. The race tier's incident dossier 38410499633 read exactly that
// and was taken for a one-CPU confinement; its proc-stack.txt shows the
// leader parked in epoll_pwait, which is a pinned epoll loop. The table below
// has every thread's own mask and state, so the next dossier can say which.

// refappSchedText describes the refapp at one instant: the validator's own
// CPU view, the refapp's thread table read from procRoot (/proc), and, when
// debugAddr is set, the refapp's own answer from its side listener
// (/debug/sched: GOMAXPROCS, NumCPU and the engine counters). Every leg is
// best effort and says what it could not read.
func refappSchedText(ctx context.Context, procRoot string, pid int, debugAddr string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "VALIDATOR_CPUS num_cpu=%d gomaxprocs=%d cpus_allowed_list=%s\n",
		runtime.NumCPU(), runtime.GOMAXPROCS(0), procStatusField(filepath.Join(procRoot, "self", "status"), "Cpus_allowed_list"))
	if pid <= 0 {
		b.WriteString("REFAPP_THREADS unavailable: no refapp pid\n")
	} else {
		b.WriteString(threadTable(procRoot, pid))
	}
	if debugAddr == "" {
		b.WriteString("REFAPP_SCHED unavailable: no side listener\n")
	} else if body, err := fetchSched(ctx, debugAddr); err != nil {
		fmt.Fprintf(&b, "REFAPP_SCHED unavailable: %v\n", err)
	} else {
		fmt.Fprintf(&b, "REFAPP_SCHED %s\n", strings.TrimSpace(body))
	}
	return b.String()
}

// procStatusField returns the value of one "Key:\tvalue" line of a
// /proc/<...>/status file, or "unreadable".
func procStatusField(path, key string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "unreadable"
	}
	return statusField(string(b), key)
}

func statusField(status, key string) string {
	for _, line := range strings.Split(status, "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && k == key {
			return strings.TrimSpace(v)
		}
	}
	return "absent"
}

// threadTable lists every thread of pid: its own CPU mask, state and
// scheduling counters, then one summary line (REFAPP_THREADS) that groups the
// masks and the states. A mask shared by many threads is the process mask; a
// mask of one CPU on a few threads is a pinned loop.
func threadTable(procRoot string, pid int) string {
	taskDir := filepath.Join(procRoot, strconv.Itoa(pid), "task")
	entries, err := os.ReadDir(taskDir)
	if err != nil {
		return fmt.Sprintf("REFAPP_THREADS unavailable: %v\n", err)
	}
	type thread struct {
		tid                       int
		comm, state, cpus         string
		vol, nonvol, utime, stime string
	}
	var ts []thread
	for _, e := range entries {
		tid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(taskDir, e.Name(), "status"))
		if err != nil {
			continue // the thread exited between the listing and the read
		}
		st := string(raw)
		t := thread{tid: tid, comm: statusField(st, "Name"), cpus: statusField(st, "Cpus_allowed_list"),
			vol: statusField(st, "voluntary_ctxt_switches"), nonvol: statusField(st, "nonvoluntary_ctxt_switches")}
		t.state, _, _ = strings.Cut(statusField(st, "State"), " ")
		t.utime, t.stime = "na", "na"
		if sb, err := os.ReadFile(filepath.Join(taskDir, e.Name(), "stat")); err == nil {
			// Fields after the ")" that closes the command name: state is the
			// first, utime and stime the 12th and 13th.
			if i := strings.LastIndexByte(string(sb), ')'); i >= 0 {
				if f := strings.Fields(string(sb)[i+1:]); len(f) > 13 {
					t.utime, t.stime = f[11], f[12]
				}
			}
		}
		ts = append(ts, t)
	}
	if len(ts) == 0 {
		return "REFAPP_THREADS unavailable: no thread readable\n"
	}
	sort.Slice(ts, func(i, j int) bool { return ts[i].tid < ts[j].tid })
	var b strings.Builder
	masks, states := map[string]int{}, map[string]int{}
	leader := "absent"
	for _, t := range ts {
		masks[t.cpus]++
		states[t.state]++
		role := ""
		if t.tid == pid {
			role, leader = " leader", t.cpus
		}
		fmt.Fprintf(&b, "thread tid=%d comm=%s state=%s cpus_allowed_list=%s vol=%s nonvol=%s utime=%s stime=%s%s\n",
			t.tid, t.comm, t.state, t.cpus, t.vol, t.nonvol, t.utime, t.stime, role)
	}
	fmt.Fprintf(&b, "REFAPP_THREADS n=%d leader_cpus_allowed_list=%s masks=%s states=%s\n",
		len(ts), leader, counts(masks, ";"), counts(states, ","))
	return b.String()
}

// counts renders a tally as "k:n<sep>k:n" sorted by key. The masks use ";"
// because a mask is itself a comma list ("0-3,8").
func counts(m map[string]int, sep string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + ":" + strconv.Itoa(m[k])
	}
	return strings.Join(parts, sep)
}

// fetchSched GETs the refapp's /debug/sched from its side listener.
func fetchSched(ctx context.Context, addr string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/debug/sched", nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return string(b), err
}
