package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The cluster-runner-down teardown reported SUCCESS for bench 37226284351 while
// every host was UNREACHABLE and three runners stayed online. These drive the
// two scripts that now decide that, with a fake `ansible`, `gh` and `sleep` on
// the path, so the retry/budget arithmetic and the verdict are checked without
// a cluster. They need bash and awk (Linux or macOS).

const (
	reachScript   = ".github/scripts/cluster-reachability.sh"
	verdictScript = ".github/scripts/cluster-teardown-verdict.sh"
)

// fakeAnsible answers `--list-hosts` and `-m ping -o` from a per-attempt plan:
// FAKE_PING_PLAN is ';'-separated, one entry per ping call, each "ok" or
// "down:host,host". The last entry repeats.
const fakeAnsible = `#!/usr/bin/env bash
state=$FAKE_STATE
case " $* " in
*" --list-hosts "*)
  printf '  hosts (3):\n    msa2-server\n    msr1\n    msa2-client\n'; exit 0;;
esac
n=$(( $(cat "$state/pings" 2>/dev/null || echo 0) + 1 )); echo $n > "$state/pings"
IFS=';' read -r -a plan <<<"$FAKE_PING_PLAN"
i=$(( n - 1 )); [ $i -ge ${#plan[@]} ] && i=$(( ${#plan[@]} - 1 ))
entry=${plan[$i]}
down=""; [ "${entry%%:*}" = down ] && down=${entry#down:}
for h in msa2-server msr1 msa2-client; do
  case ",$down," in
  *",$h,"*) printf '%s | UNREACHABLE! => {"changed": false, "msg": "Failed to connect to the host via ssh: ssh: connect to host %s port 22: Connection timed out", "unreachable": true}\n' $h $h;;
  *) printf '%s | SUCCESS => {"changed": false, "ping": "pong"}\n' $h;;
  esac
done
[ -n "$down" ] && exit 4
exit 0
`

const fakeSleep = `#!/usr/bin/env bash
echo "$1" >> "$FAKE_STATE/sleeps"
`

// fakeGH prints the runner names for each call from FAKE_GH_PLAN
// (';'-separated, csv per call, '-' = nobody, last repeats); FAKE_GH_FAIL makes it fail.
const fakeGH = `#!/usr/bin/env bash
state=$FAKE_STATE
n=$(( $(cat "$state/ghcalls" 2>/dev/null || echo 0) + 1 )); echo $n > "$state/ghcalls"
if [ -n "${FAKE_GH_FAIL:-}" ]; then echo "HTTP 502: bad gateway" >&2; exit 1; fi
IFS=';' read -r -a plan <<<"${FAKE_GH_PLAN:-}"
i=$(( n - 1 )); [ $i -ge ${#plan[@]} ] && i=$(( ${#plan[@]} - 1 ))
[ ${#plan[@]} -eq 0 ] && exit 0
[ "${plan[$i]}" = "-" ] && exit 0
echo "${plan[$i]}" | tr ',' '\n' | sed '/^$/d'
`

type scriptRun struct {
	out    string
	rc     int
	sleeps []string
	pings  int
	gh     int
	gho    string // $GITHUB_OUTPUT
	sum    string // $GITHUB_STEP_SUMMARY
}

func runScript(t *testing.T, script string, args []string, env map[string]string) scriptRun {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("bash scripts")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	dir := t.TempDir()
	for name, body := range map[string]string{"ansible": fakeAnsible, "sleep": fakeSleep, "gh": fakeGH} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	state := filepath.Join(dir, "state")
	if err := os.Mkdir(state, 0o755); err != nil {
		t.Fatal(err)
	}
	gho, sum := filepath.Join(dir, "output"), filepath.Join(dir, "summary")
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Env = append(os.Environ(),
		"FAKE_STATE="+state, "ANSIBLE_BIN="+filepath.Join(dir, "ansible"), "SLEEP_BIN="+filepath.Join(dir, "sleep"),
		"GH_BIN="+filepath.Join(dir, "gh"), "GITHUB_OUTPUT="+gho, "GITHUB_STEP_SUMMARY="+sum, "REPO=goceleris/probatorium",
		"REMOVAL_TOKEN_MINTED=true") // a test overrides it; the later entry wins
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	rc := 0
	if err := cmd.Run(); err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatal(err)
		}
		rc = ee.ExitCode()
	}
	r := scriptRun{out: out.String(), rc: rc}
	if b, err := os.ReadFile(filepath.Join(state, "sleeps")); err == nil {
		r.sleeps = strings.Fields(string(b))
	}
	r.pings = readInt(filepath.Join(state, "pings"))
	r.gh = readInt(filepath.Join(state, "ghcalls"))
	if b, err := os.ReadFile(gho); err == nil {
		r.gho = string(b)
	}
	if b, err := os.ReadFile(sum); err == nil {
		r.sum = string(b)
	}
	return r
}

func readInt(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n := 0
	for _, c := range strings.TrimSpace(string(b)) {
		n = n*10 + int(c-'0')
	}
	return n
}

// A healthy cluster must not be slowed down: one ping, no sleep.
func TestReachabilityCostsOnePingWhenEveryHostAnswers(t *testing.T) {
	r := runScript(t, reachScript, []string{"wait"}, map[string]string{"FAKE_PING_PLAN": "ok"})
	if r.rc != 0 || r.pings != 1 || len(r.sleeps) != 0 {
		t.Fatalf("rc=%d pings=%d sleeps=%v\n%s", r.rc, r.pings, r.sleeps, r.out)
	}
	if !strings.Contains(r.gho, "unreachable=\n") || !strings.Contains(r.gho, "attempts=1") {
		t.Errorf("outputs = %q", r.gho)
	}
}

func TestReachabilityWaitsOutAShortOutageWithBackoff(t *testing.T) {
	r := runScript(t, reachScript, []string{"wait"}, map[string]string{
		"FAKE_PING_PLAN": "down:msa2-server,msr1,msa2-client;down:msr1;ok", "PROBE_DELAYS": "5 10 20", "PROBE_BUDGET_SECONDS": "180"})
	if r.rc != 0 || r.pings != 3 || strings.Join(r.sleeps, ",") != "5,10" {
		t.Fatalf("rc=%d pings=%d sleeps=%v\n%s", r.rc, r.pings, r.sleeps, r.out)
	}
	if !strings.Contains(r.gho, "unreachable=\n") || !strings.Contains(r.gho, "attempts=3") {
		t.Errorf("a recovered cluster must report no unreachable host: %q", r.gho)
	}
	if strings.Contains(r.out, "::error") {
		t.Errorf("the wait step must not fail by itself:\n%s", r.out)
	}
}

// The retry is BOUNDED: it gives up when the next sleep would pass the budget,
// names the hosts, and does not fail the step (the verdict step does).
func TestReachabilityGivesUpAtTheBudgetAndNamesTheHosts(t *testing.T) {
	r := runScript(t, reachScript, []string{"wait"}, map[string]string{
		"FAKE_PING_PLAN": "down:msr1,msa2-client", "PROBE_DELAYS": "5 10 20 30", "PROBE_BUDGET_SECONDS": "60"})
	if r.rc != 0 {
		t.Fatalf("wait must never fail the step: rc=%d\n%s", r.rc, r.out)
	}
	// 5+10+20 = 35s slept; the 4th delay (30) would reach 65 > 60.
	if strings.Join(r.sleeps, ",") != "5,10,20" || r.pings != 4 {
		t.Errorf("sleeps=%v pings=%d, want 5,10,20 and 4 pings", r.sleeps, r.pings)
	}
	if !strings.Contains(r.gho, "unreachable=msr1,msa2-client") || !strings.Contains(r.gho, "waited=35") {
		t.Errorf("outputs = %q", r.gho)
	}
	if !strings.Contains(r.out, "::warning title=Cluster hosts unreachable::no answer from msr1,msa2-client") {
		t.Errorf("no warning naming the hosts:\n%s", r.out)
	}
}

func TestReachabilityTreatsASilentProbeAsEveryHostDown(t *testing.T) {
	// ansible killed by the attempt timeout prints nothing at all.
	r := runScript(t, reachScript, []string{"wait"}, map[string]string{
		"FAKE_PING_PLAN": "down:msa2-server,msr1,msa2-client", "PROBE_DELAYS": "100", "PROBE_BUDGET_SECONDS": "10"})
	if !strings.Contains(r.gho, "unreachable=msa2-server,msr1,msa2-client") {
		t.Errorf("outputs = %q\n%s", r.gho, r.out)
	}
}

func TestReachabilityProbeModeFailsWhenAHostIsSilent(t *testing.T) {
	if r := runScript(t, reachScript, []string{"probe"}, map[string]string{"FAKE_PING_PLAN": "down:msr1"}); r.rc != 1 || r.pings != 1 || len(r.sleeps) != 0 {
		t.Errorf("probe with a dead host: rc=%d pings=%d sleeps=%v\n%s", r.rc, r.pings, r.sleeps, r.out)
	}
	if r := runScript(t, reachScript, []string{"probe"}, map[string]string{"FAKE_PING_PLAN": "ok"}); r.rc != 0 {
		t.Errorf("probe with a healthy cluster: rc=%d\n%s", r.rc, r.out)
	}
}

// ---- the verdict ----

// Lines as the Actions log carries them, from the real teardown job of run
// 37226284351 (evidence/msr1-rvtest-20261009/run-37226284351/teardown-job.log).
const teardownLogAllDown = `teardown cluster runners	UNKNOWN STEP	2026-10-06T01:16:51.6913007Z fatal: [msa2-server]: UNREACHABLE! => {"changed": false, "msg": "Task failed: Failed to connect to the host via ssh: ssh: connect to host msa2-server port 22: Connection timed out", "unreachable": true}
teardown cluster runners	UNKNOWN STEP	2026-10-06T01:16:51.6914571Z ...ignoring
teardown cluster runners	UNKNOWN STEP	2026-10-06T01:16:51.6922121Z fatal: [msr1]: UNREACHABLE! => {"changed": false, "msg": "Task failed: Failed to connect to the host via ssh: ssh: connect to host msr1 port 22: Connection timed out", "unreachable": true}
teardown cluster runners	UNKNOWN STEP	2026-10-06T01:16:51.6923366Z ...ignoring
teardown cluster runners	UNKNOWN STEP	2026-10-06T01:16:51.6929669Z fatal: [msa2-client]: UNREACHABLE! => {"changed": false, "msg": "Task failed: Failed to connect to the host via ssh: ssh: connect to host msa2-client port 22: Connection timed out", "unreachable": true}
teardown cluster runners	UNKNOWN STEP	2026-10-06T01:16:51.6930921Z ...ignoring
teardown cluster runners	UNKNOWN STEP	2026-10-06T01:17:42.8380146Z PLAY RECAP *********************************************************************
teardown cluster runners	UNKNOWN STEP	2026-10-06T01:17:42.8381692Z msa2-server                : ok=6    changed=0    unreachable=0    failed=0    skipped=6    rescued=0    ignored=6
`

func logDir(t *testing.T, files map[string]string) string {
	t.Helper()
	d := t.TempDir()
	for n, b := range files {
		if err := os.WriteFile(filepath.Join(d, n), []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func TestVerdictIsCleanAndCheapOnAHealthyTeardown(t *testing.T) {
	d := logDir(t, map[string]string{"runner-teardown.log": "msr1 : ok=6 changed=3 unreachable=0 failed=0 skipped=0 rescued=0 ignored=0\n"})
	r := runScript(t, verdictScript, nil, map[string]string{"LOG_DIR": d, "FAKE_GH_PLAN": ""})
	if r.rc != 0 || r.gh != 1 || len(r.sleeps) != 0 {
		t.Fatalf("rc=%d gh calls=%d sleeps=%v\n%s", r.rc, r.gh, r.sleeps, r.out)
	}
}

// The 10-06 teardown: every host UNREACHABLE, recap says ignored, job was green.
func TestVerdictFailsWhenEveryHostWasUnreachableAndNamesThem(t *testing.T) {
	d := logDir(t, map[string]string{"runner-teardown.log": teardownLogAllDown, "cleanup.log": teardownLogAllDown})
	r := runScript(t, verdictScript, nil, map[string]string{
		"LOG_DIR": d, "UNREACHABLE_AT_START": "msa2-server,msr1,msa2-client", "REMOVAL_TOKEN_MINTED": "false",
		"FAKE_GH_PLAN": "celeris-msr1,celeris-msa2-server,celeris-msa2-client", "ONLINE_RETRIES": "2"})
	if r.rc != 1 {
		t.Fatalf("a teardown that reached no host must fail, rc=%d\n%s", r.rc, r.out)
	}
	for _, want := range []string{"::error title=Cluster teardown incomplete::", "msa2-server", "msr1", "msa2-client", "removal token", "ONLINE", "celeris-msr1"} {
		if !strings.Contains(r.out, want) {
			t.Errorf("annotation lacks %q:\n%s", want, r.out)
		}
	}
	if !strings.Contains(r.sum, "INCOMPLETE") {
		t.Errorf("step summary: %q", r.sum)
	}
}

func TestVerdictNamesOnlyTheHostThatDropped(t *testing.T) {
	log := strings.Join(strings.Split(teardownLogAllDown, "\n")[2:4], "\n") + "\n" // msr1 only
	d := logDir(t, map[string]string{"cleanup.log": log})
	r := runScript(t, verdictScript, nil, map[string]string{"LOG_DIR": d})
	if r.rc != 1 || !strings.Contains(r.out, "UNREACHABLE host(s): msr1.") || strings.Contains(r.out, "msa2-server") {
		t.Fatalf("rc=%d\n%s", r.rc, r.out)
	}
}

// Fail closed: a step that never ran leaves the output empty, and that is not "minted".
func TestVerdictTreatsAnUnsetRemovalTokenFlagAsNotMinted(t *testing.T) {
	d := logDir(t, map[string]string{"x.log": ""})
	for _, v := range []string{"", "false", "maybe"} {
		r := runScript(t, verdictScript, nil, map[string]string{"LOG_DIR": d, "REMOVAL_TOKEN_MINTED": v})
		if r.rc != 1 || !strings.Contains(r.out, "removal token") {
			t.Errorf("REMOVAL_TOKEN_MINTED=%q: rc=%d\n%s", v, r.rc, r.out)
		}
	}
}

func TestVerdictFailsWhenTheCancelPathCouldNotStopASUT(t *testing.T) {
	d := logDir(t, map[string]string{"stop-sut.log": "fatal: [msa2-server]: FAILED! => {\"msg\": \"reap: stopped 1 process(es); STILL ALIVE after SIGKILL: 4242\"}\n"})
	r := runScript(t, verdictScript, nil, map[string]string{"LOG_DIR": d})
	if r.rc != 1 || !strings.Contains(r.out, "could not be stopped on: msa2-server") {
		t.Fatalf("rc=%d\n%s", r.rc, r.out)
	}
}

func TestVerdictFailsOnAHostThatNeverAnsweredEvenWithCleanLogs(t *testing.T) {
	d := logDir(t, map[string]string{"runner-teardown.log": "nothing logged\n"})
	r := runScript(t, verdictScript, nil, map[string]string{"LOG_DIR": d, "UNREACHABLE_AT_START": "msa2-client"})
	if r.rc != 1 || !strings.Contains(r.out, "msa2-client") {
		t.Fatalf("rc=%d\n%s", r.rc, r.out)
	}
}

// Online runners are what 37226284351 actually left behind. They are checked
// without any host answering, and given time to disappear.
func TestVerdictFailsOnRunnersThatStayOnlineAndForgivesOnesThatLeave(t *testing.T) {
	d := logDir(t, map[string]string{"x.log": ""})
	r := runScript(t, verdictScript, nil, map[string]string{"LOG_DIR": d, "FAKE_GH_PLAN": "celeris-msr1", "ONLINE_RETRIES": "3", "ONLINE_DELAY": "7"})
	if r.rc != 1 || r.gh != 3 || strings.Join(r.sleeps, ",") != "7,7" || !strings.Contains(r.out, "celeris-msr1") {
		t.Fatalf("persistent runner: rc=%d gh=%d sleeps=%v\n%s", r.rc, r.gh, r.sleeps, r.out)
	}
	r = runScript(t, verdictScript, nil, map[string]string{"LOG_DIR": d, "FAKE_GH_PLAN": "celeris-msr1,celeris-msa2-server;celeris-msr1;-", "ONLINE_RETRIES": "4", "ONLINE_DELAY": "7"})
	if r.rc != 0 || r.gh != 3 || len(r.sleeps) != 2 {
		t.Fatalf("runners that de-register during the wait must pass: rc=%d gh=%d sleeps=%v\n%s", r.rc, r.gh, r.sleeps, r.out)
	}
}

func TestVerdictWarnsButDoesNotFailWhenGitHubCannotBeAsked(t *testing.T) {
	d := logDir(t, map[string]string{"x.log": ""})
	r := runScript(t, verdictScript, nil, map[string]string{"LOG_DIR": d, "FAKE_GH_FAIL": "1"})
	if r.rc != 0 || !strings.Contains(r.out, "::warning title=Teardown verdict::") {
		t.Fatalf("rc=%d\n%s", r.rc, r.out)
	}
	// ...but it still fails when something else is wrong.
	r = runScript(t, verdictScript, nil, map[string]string{"LOG_DIR": d, "FAKE_GH_FAIL": "1", "UNREACHABLE_AT_START": "msr1"})
	if r.rc != 1 {
		t.Fatalf("rc=%d\n%s", r.rc, r.out)
	}
}
