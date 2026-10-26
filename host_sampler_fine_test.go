package main

import (
	"context"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The fine host sampler (ops/host-sampler/celeris-host-sampler-fine, the
// 1 s / 0.05 s series the bench tier starts beside its run) is a python
// script, and python is outside what `go test` covers. Its tests are a unittest
// file over a fixture /proc (ops/host-sampler/test_fine_sampler.py): the line
// format, the elision of idle CPUs, the starvation and gap events, rotation
// and the event-log cap, the deadline / stop-sentinel / vanished-runner exits,
// the take-over of an older instance by sentinel file, the config bounds and
// the lowered priority. This runs the file so a change to the script cannot
// pass `go test ./...` without them.
func TestFineHostSamplerUnitTests(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	// bounded: a loop that never ends (a deadline ignored) must fail here, not at the suite's timeout
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, py, "-I", "ops/host-sampler/test_fine_sampler.py").CombinedOutput()
	t.Logf("%s", out)
	if err != nil {
		t.Fatalf("ops/host-sampler/test_fine_sampler.py: %v", err)
	}
	m := regexp.MustCompile(`Ran (\d+) tests`).FindSubmatch(out)
	if m == nil || string(m[1]) == "0" {
		t.Fatalf("the unit tests ran nothing:\n%s", out)
	}
	if !strings.Contains(string(out), "\nOK") {
		t.Fatalf("the unit tests did not report OK:\n%s", out)
	}
}

// The script that the playbook stages is the one that is tested: the playbook
// copies it from ops/host-sampler/, executable bit included, and launches it
// with `python3 -I`, so the file must parse as python 3 without the
// environment (no PYTHONPATH, no site customisation).
func TestFineHostSamplerCompiles(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	out, err := exec.Command(py, "-I", "-c",
		"import sys; compile(open(sys.argv[1]).read(), sys.argv[1], 'exec')",
		"ops/host-sampler/celeris-host-sampler-fine").CombinedOutput()
	if err != nil {
		t.Fatalf("does not compile: %v\n%s", err, out)
	}
}

// The playbook wiring that the real-playbook test (ansible/tests/
// test_host_sampler_play.sh, run by the ansible job of test.yml) proves
// end to end, pinned as text so a hand edit that drops one piece fails
// `go test` without waiting for that job.
func TestHostSamplerPlaybookWiresTheFineSampler(t *testing.T) {
	src := mustRead(t, "ansible/host-sampler.yml")
	for _, c := range []struct{ what, needle string }{
		{"the cadence defaults to none (the race and weekend tiers)", `sampler_fine_interval: ""`},
		{"python without the environment's site code", `python3 -I "{{ sampler_rundir }}/celeris-host-sampler-fine"`},
		{"it ends with the runner that started it", `SAMPLER_EXIT_WITHOUT_RUNNER: "1"`},
		{"the cadence is validated in the playbook", `sampler_fine_interval | string is match('^([0-9]{1,2}([.][0-9]{1,3})?)?$')`},
		{"and bounded", `(sampler_fine_interval | float) >= 0.05 and (sampler_fine_interval | float) <= 10`},
		{"the full cadence is bounded too", `(sampler_interval | int) <= 3600`},
		{"the stop fetches the compressed generations (found by name)", `patterns: "fine.*.log.z"`},
		{"and the per-minute record of the whole run", `'fine-minute.log'`},
		{"and the event log", `'fine-events.log'`},
		{"and the stats", `'fine.stats'`},
		{"and the run file", `'fine-run.txt'`},
		{"and the generations it found", `(sampler_fine_gens.files | default([]) | map(attribute='path') | map('basename') | list)`},
		{"into the host's fine/ directory", `{{ sampler_fetch_dir }}/{{ inventory_hostname }}/fine/{{ item }}`},
		{"only when the run file carries the stop's tag (fixed string, whole field)", `head -n 1 "{{ sampler_fine_dir }}/fine-run.txt" | tr " " "\n" | head -n 1 | grep -qxF "tag={{ sampler_fetch_tag }}"`},
		{"the tag is checked at the confirmation too", `head -n 1 "{{ sampler_fine_dir }}/fine-run.txt" 2>/dev/null | tr " " "\n" | head -n 1 | grep -qxF "tag={{ sampler_tag }}"`},
		{"the shell sampler is launched low only when asked", `if [ "{{ sampler_low_priority | bool | lower }}" = "true" ]; then`},
		{"at nice 19 under SCHED_IDLE", `pfx="nice -n 19 chrt -i 0"`},
		{"and without chrt at nice 19", `pfx="nice -n 19"`},
		{"the low-priority flag defaults to false", `sampler_low_priority: false`},
		{"the tag is validated", `sampler_tag | string is match('^[A-Za-z0-9._-]{1,64}$')`},
		{"and the fetch tag", `sampler_fetch_tag | string is match('^([A-Za-z0-9._-]{1,64})?$')`},
		{"a missing file fails nothing", `fail_on_missing: false`},
		{"the fine sampler is launched detached", `setsid nohup python3 -I`},
		{"a host without python3 is not a failure", `failed_when: false`},
	} {
		if !strings.Contains(src, c.needle) {
			t.Errorf("host-sampler.yml: %s: missing %q", c.what, c.needle)
		}
	}
	// both the listing and the fetch of the fine series are guarded by the tag check
	if n := strings.Count(src, "        - sampler_fine_mine.rc | default(1) == 0\n"); n != 2 {
		t.Errorf("%d tasks are guarded by the tag check of the fine series, want 2 (the listing and the fetch)", n)
	}
	// the sentinel is touched by the stop and by a start without a fine cadence
	if n := strings.Count(src, `path: "{{ sampler_rundir }}/host-sampler-fine-stop"`); n != 2 {
		t.Errorf("the fine stop sentinel is touched in %d tasks, want 2 (stop, and a start without a fine cadence)", n)
	}
	retire := regexp.MustCompile(`(?s)retire a stale fine sampler.*?when:\n(.*?)\n\n`).FindStringSubmatch(src)
	if retire == nil || !strings.Contains(retire[1], "sampler_action == 'start'") || !strings.Contains(retire[1], "sampler_fine_interval | string | length == 0") {
		t.Errorf("the retire task is not guarded by `start` and `no fine cadence`:\n%v", retire)
	}
	// the 60 s sampler's launch is the same script with the same environment;
	// without sampler_low_priority the prefix is empty
	for _, want := range []string{
		`setsid nohup $pfx "{{ sampler_rundir }}/celeris-host-sampler" >/dev/null 2>&1 </dev/null &`,
		`SAMPLER_INTERVAL: "{{ sampler_interval }}"`,
		`pfx=""`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the 60 s sampler's launch changed: %q is gone", want)
		}
	}
}

// test.yml's ansible job runs the real-playbook test.
func TestAnsibleJobRunsTheHostSamplerPlaybookTest(t *testing.T) {
	wf := readWorkflow(t, "test.yml")
	if !regexp.MustCompile(`(?m)^\s*run: bash ansible/tests/test_host_sampler_play\.sh\s*$`).MatchString(wf) {
		t.Error("test.yml's ansible job does not run `bash ansible/tests/test_host_sampler_play.sh`")
	}
}

// The forensics play (the runner-died path) fetches the fine files that are
// small and answer the question: the episodes, the per-minute record of the
// whole run, the stats and the run file.
func TestForensicsFetchesTheFineEvidence(t *testing.T) {
	src := mustRead(t, "ansible/forensics.yml")
	i := strings.Index(src, "name: Fetch the fine host sampler events")
	if i < 0 {
		t.Fatal("forensics.yml has no `Fetch the fine host sampler events` task")
	}
	blk := src[i:]
	if j := strings.Index(blk[10:], "\n    - name:"); j > 0 {
		blk = blk[:j+10]
	}
	for _, want := range []string{
		`src: "{{ sampler_fine_dir }}/{{ item }}"`, `- fine-events.log`, `- fine-minute.log`, `- fine.stats`, `- fine-run.txt`,
		`fail_on_missing: false`, `failed_when: false`, `when: forensics.stdout is defined`,
	} {
		if !strings.Contains(blk, want) {
			t.Errorf("the fine fetch of forensics.yml lacks %q:\n%s", want, blk)
		}
	}
	if !strings.Contains(src, "sampler_fine_dir: /run/celeris/fine") {
		t.Error("forensics.yml does not define sampler_fine_dir")
	}
}

// The samplers run under SCHED_IDLE. A SCHED_IDLE task is preempted as soon as
// anything else is runnable, so a read that holds a lock the SUT shares can
// hold it for hundreds of ms (review r2, N1): pgrep opens /proc/<pid>/cmdline
// of EVERY process (mmap_lock of the SUT's mm); maps, smaps, environ, stack
// and wchan are the same family. Only comm (task_lock), schedstat, status of
// the sampler's own targets and the system-wide files may be read.
var perProcessMemoryReads = regexp.MustCompile(
	`\b(pgrep|pidof|pkill|killall)\b|(^|[;&|(\s])ps(\s|$)|/cmdline|\bcmdline\b|/maps\b|smaps|/environ|/stack\b|/wchan|/auxv|/mem\b|/pagemap`)

func TestSamplersReadNoPerProcessMemoryState(t *testing.T) {
	for _, path := range []string{"ops/host-sampler/celeris-host-sampler", "ops/host-sampler/celeris-host-sampler-fine"} {
		src := stripComments(mustRead(t, path))
		if m := perProcessMemoryReads.FindString(src); m != "" {
			t.Errorf("%s uses %q: under SCHED_IDLE a read that takes another process's mmap_lock (pgrep, cmdline, maps...) "+
				"can stall that process for hundreds of ms; scan /proc/*/comm instead", path, strings.TrimSpace(m))
		}
	}
	// the one function that finds a pid by name reads comm
	src := mustRead(t, "ops/host-sampler/celeris-host-sampler")
	i := strings.Index(src, "pid_of() {")
	if i < 0 || !strings.Contains(src[i:i+600], `"$_d/comm"`) {
		t.Error("pid_of does not scan /proc/*/comm")
	}
}

// Stale or wrong figures and class names that this PR has already had to correct:
// the compression assumption (2-5x, not 5.6x: a quiet host compresses 5x, high
// entropy counters 2.1-2.4x), and the fine sampler is SCHED_IDLE, not "nice 10".
func TestSamplerDocsDoNotRepeatCorrectedClaims(t *testing.T) {
	stale := regexp.MustCompile(`5\.6x|about 5x|~5x|4-5\.6x|\(about 5x|at nice 10|nice 10; see|9 descriptors|128 MiB`)
	for _, path := range []string{"README.md", ".github/workflows/benchmark-tier.yml", "ansible/host-sampler.yml",
		"ops/host-sampler/celeris-host-sampler-fine", "mage_sampler.go", ".github/workflows/test.yml"} {
		src := mustRead(t, path)
		for _, m := range stale.FindAllString(src, -1) {
			t.Errorf("%s still says %q", path, m)
		}
	}
	for _, path := range []string{"README.md", ".github/workflows/benchmark-tier.yml"} {
		if !strings.Contains(mustRead(t, path), "2-5x") {
			t.Errorf("%s does not state the compression range as 2-5x", path)
		}
	}
}

// The forensics play is the only collector when the runner dies, so it also
// takes the per-tick generations -- but only of a sampler nobody stopped
// (exit_reason=stop_requested means the stop step or a later start retired it
// after its own fetch), and only the newest 24, so the artifact is bounded.
func TestForensicsFetchesThePerTickSeriesOfAnUnstoppedSampler(t *testing.T) {
	src := mustRead(t, "ansible/forensics.yml")
	if n := strings.Count(src, "sampler_fine_stopped.rc | default(0) != 0"); n != 2 {
		t.Errorf("%d tasks are guarded by `the sampler was not stopped`, want 2 (the listing and the fetch)", n)
	}
	for _, want := range []string{
		`grep -q "exit_reason=stop_requested" "{{ sampler_fine_dir }}/fine.stats"`,
		`patterns: "fine.*.log.z"`,
		`sort(attribute='mtime') | reverse | list)[:24]`,
		`'fine.log'`,
		`sampler_fine_stopped.rc | default(0) != 0`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("forensics.yml lacks %q", want)
		}
	}
}
