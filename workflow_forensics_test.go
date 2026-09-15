package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The post-hoc forensics capture (probatorium#354) has two properties that
// cannot be checked anywhere else, because the only place the real thing runs
// is a teardown job that follows a cluster run:
//
//  1. it must happen BEFORE the teardown reclaims anything, since both
//     reclamation steps delete exactly what it collects;
//  2. nothing it drives may be able to disturb a host that is still running
//     something.
//
// Both are properties of text that people edit by hand, and both fail
// silently: a capture moved below the cleanup still passes every lint and
// still produces an artifact — just an empty one, discovered the next time a
// 20 h soak is lost. So they are pinned here.

const (
	downAction      = ".github/actions/cluster-runner-down/action.yml"
	weekendWorkflow = ".github/workflows/matrix-weekend-tier.yml"
)

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// mustIndex returns the offset of needle, failing the test when it is absent.
func mustIndex(t *testing.T, src, needle, what string) int {
	t.Helper()
	i := strings.Index(src, needle)
	if i < 0 {
		t.Fatalf("%s: expected to find %s (%q)", what, what, needle)
	}
	return i
}

// TestForensicsIsCollectedBeforeAnythingIsReclaimed is the ordering guard.
//
// cleanup.yml is driven with purge_results=true, which rm -rf's results_root;
// runner-teardown.yml rm -rf's /tmp/actions-runner-<host>/ and with it
// runner.stdout, runner.stderr and the _diag logs. Those ARE the evidence. A
// capture that runs after them collects nothing and says nothing about it.
func TestForensicsIsCollectedBeforeAnythingIsReclaimed(t *testing.T) {
	src := mustRead(t, downAction)

	collect := mustIndex(t, src, "name: Collect post-hoc host forensics", "forensics capture step")
	upload := mustIndex(t, src, "name: Upload cluster forensics", "forensics upload step")
	reclaim := mustIndex(t, src, "name: Reclaim cluster host state", "cleanup.yml step")
	teardown := mustIndex(t, src, "name: Run ansible runner-teardown", "runner-teardown.yml step")

	if collect > reclaim {
		t.Errorf("the forensics capture runs AFTER cleanup.yml (offsets %d > %d); "+
			"cleanup.yml purges results_root, so the capture would collect nothing",
			collect, reclaim)
	}
	if collect > teardown {
		t.Errorf("the forensics capture runs AFTER runner-teardown.yml (offsets %d > %d); "+
			"runner-teardown wipes the runner dir, taking runner.stdout/stderr and _diag with it",
			collect, teardown)
	}
	if upload < collect {
		t.Errorf("the upload step precedes the capture it uploads (offsets %d < %d)", upload, collect)
	}
	if upload > reclaim {
		t.Errorf("the forensics upload runs after reclamation (offsets %d > %d); keep it adjacent "+
			"to the capture so a later step failing cannot cost the artifact", upload, reclaim)
	}
}

// TestForensicsStepsSurviveAFailedMatrixJob: the whole point is the run where
// the matrix job died, so neither step may be conditional on success.
func TestForensicsStepsSurviveAFailedMatrixJob(t *testing.T) {
	src := mustRead(t, downAction)
	for _, step := range []string{
		"name: Collect post-hoc host forensics",
		"name: Upload cluster forensics",
	} {
		i := mustIndex(t, src, step, "forensics step")
		// The `if:` sits on the line directly above the name in this file's
		// style; take a window around the step header and require always().
		start := i - 200
		if start < 0 {
			start = 0
		}
		end := i + 200
		if end > len(src) {
			end = len(src)
		}
		if !strings.Contains(src[start:end], "if: always()") {
			t.Errorf("step %q is not guarded with `if: always()`; it would be skipped on "+
				"exactly the runs it exists for", step)
		}
	}
}

// commandPosition matches a token used as a COMMAND: at the start of a line
// or right after a shell separator, and followed by whitespace or end of
// line. Both halves are needed, and each was put there by a false positive
// the first draft produced:
//
//   - without the leading anchor it matched `grep -iE 'oom-kill'` and
//     `last -x -F reboot shutdown`, which are the capture doing its job;
//   - without the trailing one it matched the glob in `case "$n" in
//     lo | veth* | docker* | br-*)`, which names an interface to skip.
//
// It will not catch `docker)` as a case pattern either, which is the price of
// a text test; the thing it is actually guarding against is somebody adding
// `pkill -f validator` to stop something, and that it catches.
var commandPosition = regexp.MustCompile(
	`(?m)(^|[;&|(]|\$\()[[:space:]]*(kill|pkill|killall|reboot|shutdown|halt|poweroff|docker|sysctl|service)([[:space:]]|$)`)

// destructiveSystemctl matches only the systemctl verbs that CHANGE
// something. `systemctl list-units` is a read and the capture uses it.
var destructiveSystemctl = regexp.MustCompile(
	`systemctl[[:space:]]+(start|stop|restart|reload|kill|enable|disable|mask|unmask|isolate)\b`)

// other verbs that would make monitoring capable of destroying state.
var otherDestructive = []*regexp.Regexp{
	regexp.MustCompile(`\brm[[:space:]]+-[a-zA-Z]*r`),                    // rm -r / rm -rf, at all
	regexp.MustCompile(`\bdmesg[[:space:]]+(-c\b|--clear|--read-clear)`), // consumes the ring buffer
	regexp.MustCompile(`state:[[:space:]]*absent`),                       // the ansible equivalent of rm
}

// stripComments removes whole-line comments, which is where every one of
// these words legitimately appears in these files — they explain at length
// what is deliberately NOT being done.
func stripComments(src string) string {
	var out []string
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// TestMonitoringCannotDisturbACluster encodes the hard constraint: everything
// the sampler and the forensics capture do to a cluster host must be a read
// (or a write to their own files). A single `pkill`, `docker rm`, `systemctl
// restart` or `rm -rf` added to any of these would make monitoring capable of
// killing the very run it is monitoring — a 24 h soak, mid-measurement.
//
// This is a coarse instrument, and it is the right one: the risk is not a
// subtle one, it is somebody reaching for the obvious way to stop a
// background process.
func TestMonitoringCannotDisturbACluster(t *testing.T) {
	for _, path := range []string{
		"ops/host-sampler/celeris-host-sampler",
		"ops/forensics/cluster-forensics",
		"ansible/host-sampler.yml",
		"ansible/forensics.yml",
	} {
		src := stripComments(mustRead(t, path))
		if m := commandPosition.FindString(src); m != "" {
			t.Errorf("%s: invokes %q as a command; monitoring must be read-only "+
				"(probatorium#354). Stop a process with its sentinel file, never a signal.",
				path, strings.TrimSpace(m))
		}
		if m := destructiveSystemctl.FindString(src); m != "" {
			t.Errorf("%s: %q changes unit state; only read-only systemctl queries are allowed", path, m)
		}
		for _, re := range otherDestructive {
			if m := re.FindString(src); m != "" {
				t.Errorf("%s: %q can destroy state on a cluster host", path, m)
			}
		}
	}
}

// TestWeekendSoakBracketsTheRunWithTheSampler: the sampler has to start
// before the thing it is meant to observe and stop before the results are
// uploaded, or the series it produces covers the wrong window.
func TestWeekendSoakBracketsTheRunWithTheSampler(t *testing.T) {
	src := mustRead(t, weekendWorkflow)

	start := mustIndex(t, src, "run: mage HostSamplerStart", "sampler start step")
	soak := mustIndex(t, src, "run: mage Soak", "soak step")
	stop := mustIndex(t, src, "run: mage HostSamplerStop", "sampler stop step")
	upload := mustIndex(t, src, "name: Upload results", "results upload step")

	if start > soak {
		t.Errorf("the sampler starts after `mage Soak` (offsets %d > %d); the window it "+
			"records would miss the run", start, soak)
	}
	if stop < soak {
		t.Errorf("the sampler is stopped before `mage Soak` (offsets %d < %d)", stop, soak)
	}
	if stop > upload {
		t.Errorf("the sampler is stopped after the results upload (offsets %d > %d); its "+
			"series would not be in the artifact", stop, upload)
	}
	// The soak is the deliverable; instrumentation must never be able to fail
	// it. Both steps are marked continue-on-error for that reason.
	for _, step := range []string{"run: mage HostSamplerStart", "run: mage HostSamplerStop"} {
		i := mustIndex(t, src, step, "sampler step")
		from := i - 300
		if from < 0 {
			from = 0
		}
		if !strings.Contains(src[from:i], "continue-on-error: true") {
			t.Errorf("step %q is not marked continue-on-error; a sampler that cannot start "+
				"must not fail a 24 h soak", step)
		}
	}
}
