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
		{"the stop fetches the series", `fine.log.1`},
		{"and the event log", `- fine-events.log`},
		{"and the stats", `- fine.stats`},
		{"into the host's fine/ directory", `{{ sampler_fetch_dir }}/{{ inventory_hostname }}/fine/{{ item }}`},
		{"a missing file fails nothing", `fail_on_missing: false`},
		{"the fine sampler is launched detached", `setsid nohup python3 -I`},
		{"a host without python3 is not a failure", `failed_when: false`},
	} {
		if !strings.Contains(src, c.needle) {
			t.Errorf("host-sampler.yml: %s: missing %q", c.what, c.needle)
		}
	}
	// the sentinel is touched by the stop and by a start without a fine cadence
	if n := strings.Count(src, `path: "{{ sampler_rundir }}/host-sampler-fine-stop"`); n != 2 {
		t.Errorf("the fine stop sentinel is touched in %d tasks, want 2 (stop, and a start without a fine cadence)", n)
	}
	retire := regexp.MustCompile(`(?s)retire a stale fine sampler.*?when:\n(.*?)\n\n`).FindStringSubmatch(src)
	if retire == nil || !strings.Contains(retire[1], "sampler_action == 'start'") || !strings.Contains(retire[1], "sampler_fine_interval | string | length == 0") {
		t.Errorf("the retire task is not guarded by `start` and `no fine cadence`:\n%v", retire)
	}
	// the 60 s sampler's launch is untouched: same script, same environment
	for _, want := range []string{
		`setsid nohup "{{ sampler_rundir }}/celeris-host-sampler" >/dev/null 2>&1 </dev/null &`,
		`SAMPLER_INTERVAL: "{{ sampler_interval }}"`,
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
