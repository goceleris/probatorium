package validation

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// procFixture builds a /proc tree: pid 100 with a leader pinned to CPU 0 (an
// epoll loop on the main thread, the shape of race run 38410499633) and three
// more threads, two free on CPUs 0-15 and one pinned to CPU 1.
func procFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	status := func(name, state, cpus string, vol, nonvol int) string {
		return "Name:\t" + name + "\nState:\t" + state + "\nCpus_allowed_list:\t" + cpus +
			"\nvoluntary_ctxt_switches:\t" + itoa(vol) + "\nnonvoluntary_ctxt_switches:\t" + itoa(nonvol) + "\n"
	}
	stat := func(tid int, comm string, utime, stime int) string {
		// pid (comm) S ppid pgrp session tty tpgid flags minflt cminflt majflt cmajflt utime stime ...
		return itoa(tid) + " (" + comm + ") S 1 1 1 0 -1 4194560 10 0 0 0 " + itoa(utime) + " " + itoa(stime) + " 0 0 20 0 24 0 1 1 2 3\n"
	}
	write("self/status", "Name:\tvalidator\nCpus_allowed_list:\t0-15\n")
	write("100/task/100/status", status("auth_session_ra", "S (sleeping)", "0", 32846, 780))
	write("100/task/100/stat", stat(100, "auth_session_ra", 500, 200))
	write("100/task/101/status", status("auth_session_ra", "R (running)", "0-15", 100, 90000))
	write("100/task/101/stat", stat(101, "auth_session_ra", 9000, 100))
	write("100/task/102/status", status("auth_session_ra", "S (sleeping)", "0-15", 5, 5))
	write("100/task/102/stat", stat(102, "auth_session_ra", 1, 1))
	write("100/task/103/status", status("auth_session_ra", "S (sleeping)", "1", 7000, 12))
	write("100/task/103/stat", stat(103, "auth (odd) name", 3, 4)) // a ")" inside comm must not shift the fields
	return root
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestThreadTable_LeaderMaskIsNotTheProcessMask(t *testing.T) {
	got := threadTable(procFixture(t), 100)
	for _, want := range []string{
		"thread tid=100 comm=auth_session_ra state=S cpus_allowed_list=0 vol=32846 nonvol=780 utime=500 stime=200 leader",
		"thread tid=101 comm=auth_session_ra state=R cpus_allowed_list=0-15",
		"thread tid=103 comm=auth_session_ra state=S cpus_allowed_list=1 vol=7000 nonvol=12 utime=3 stime=4",
		// The summary groups masks and states: the leader's mask of 0 is one
		// of three, and two threads run free on 0-15.
		"REFAPP_THREADS n=4 leader_cpus_allowed_list=0 masks=0:1,0-15:2,1:1 states=R:1,S:3",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("table lacks %q:\n%s", want, got)
		}
	}
}

func TestThreadTable_Unreadable(t *testing.T) {
	if got := threadTable(t.TempDir(), 100); !strings.Contains(got, "REFAPP_THREADS unavailable") {
		t.Errorf("a pid with no /proc entry must say so, got %q", got)
	}
}

func TestRefappSchedText_SideListenerAndMissingLegs(t *testing.T) {
	root := procFixture(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback listener")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/sched", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"gomaxprocs":16,"numcpu":16}` + "\n"))
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	got := refappSchedText(context.Background(), root, 100, ln.Addr().String())
	for _, want := range []string{"VALIDATOR_CPUS ", "cpus_allowed_list=0-15", "REFAPP_THREADS n=4", `REFAPP_SCHED {"gomaxprocs":16,"numcpu":16}`} {
		if !strings.Contains(got, want) {
			t.Errorf("text lacks %q:\n%s", want, got)
		}
	}
	// Every leg is best effort and says what it could not read.
	none := refappSchedText(context.Background(), t.TempDir(), 0, "")
	for _, want := range []string{"REFAPP_THREADS unavailable: no refapp pid", "REFAPP_SCHED unavailable: no side listener"} {
		if !strings.Contains(none, want) {
			t.Errorf("text lacks %q:\n%s", want, none)
		}
	}
	bad := refappSchedText(context.Background(), root, 100, "127.0.0.1:1")
	if !strings.Contains(bad, "REFAPP_SCHED unavailable:") {
		t.Errorf("a dead side listener must say so:\n%s", bad)
	}
}
