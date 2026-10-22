package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The sanctioned OS survey and upgrade (.github/workflows/cluster-os-upgrade.yml, ansible/os-upgrade.yml,
// ansible/files/os_upgrade.py). A kernel change is the one cluster action that can leave a host without a NIC and
// recoverable only physically (msr1 has an out-of-tree r8127 driver and panics at power-off). These tests pin what
// makes it safe, as text and as behaviour: the workflow's gates, the playbook's order of steps, the subcommands the
// playbook may run on a host, and that the python and ansible tests run in CI. The judgement (every refusal) is
// run for real by ansible/tests/test_os_upgrade_judge.py, the host-changing subcommands by test_os_upgrade.py, the
// whole playbook by test_os_upgrade_play.py.

const (
	osWorkflow = "cluster-os-upgrade.yml"
	osPlaybook = "ansible/os-upgrade.yml"
	osScript   = "ansible/files/os_upgrade.py"
	osJudge    = "ansible/vars/os_upgrade_judge.yml"
	osRequest  = "ansible/tasks/os_upgrade_request.yml"
)

func osUpgradeHosts() []string { return []string{"msa2-server", "msr1", "msa2-client"} }

func TestClusterOSUpgradeOnlyRunsWhenDispatched(t *testing.T) {
	src := withoutComments(readWorkflow(t, osWorkflow))
	on := regexp.MustCompile(`(?ms)^on:\n(.*?)^\S`).FindStringSubmatch(src + "\nX")
	if on == nil {
		t.Fatal("no on: block")
	}
	var triggers []string
	for _, m := range regexp.MustCompile(`(?m)^  ([a-z_]+):`).FindAllStringSubmatch(on[1], -1) {
		triggers = append(triggers, m[1])
	}
	if !slices.Equal(triggers, []string{"workflow_dispatch"}) {
		t.Errorf("triggers = %v, want only workflow_dispatch: an OS upgrade must never start by itself", triggers)
	}
	if regexp.MustCompile(`(?m)^\s*-?\s*cron:`).MatchString(src) || strings.Contains(src, "schedule:") {
		t.Error("the workflow has a schedule")
	}
}

// The inputs: a survey (no host) or an upgrade of ONE named host with a typed confirmation; the playbook accepts
// exactly the hosts the workflow offers.
func TestClusterOSUpgradeInputs(t *testing.T) {
	src := withoutComments(readWorkflow(t, osWorkflow))
	choice := func(name, next string) []string {
		m := regexp.MustCompile(`(?ms)^      ` + name + `:\n(.*?)^      ` + next + `:`).FindStringSubmatch(src)
		if m == nil {
			t.Fatalf("no input %s before %s", name, next)
		}
		if !strings.Contains(m[1], "type: choice") {
			t.Fatalf("input %s is not a choice", name)
		}
		o := regexp.MustCompile(`options: \[([^\]]*)\]`).FindStringSubmatch(m[1])
		if o == nil {
			t.Fatalf("input %s has no options", name)
		}
		var out []string
		for _, x := range strings.Split(o[1], ",") {
			out = append(out, strings.Trim(strings.TrimSpace(x), `"'`))
		}
		return out
	}
	if got := choice("mode", "host"); !slices.Equal(got, []string{"survey", "rehearse", "upgrade", "disarm", "repair", "freeze", "thaw"}) {
		t.Errorf("modes = %v", got)
	}
	offered := choice("host", "confirm")
	if !slices.Equal(offered, append([]string{"-"}, osUpgradeHosts()...)) {
		t.Errorf("hosts offered = %v, want '-' and the three hosts", offered)
	}
	for _, in := range []string{"mode", "reason"} {
		m := regexp.MustCompile(`(?ms)^      ` + in + `:\n(.*?)^      [a-z_0-9]+:`).FindStringSubmatch(src)
		if m == nil || !strings.Contains(m[1], "required: true") {
			t.Errorf("input %s is not required", in)
		}
	}
	if got := choice("hold_access_packages", "blacklist_r8169"); !slices.Equal(got, []string{"auto", "on", "off"}) {
		t.Errorf("hold_access_packages = %v", got)
	}
	if got := choice("repair_action", "retry_kernel"); !slices.Equal(got, []string{"configure", "abort-new-kernel", "release-holds"}) {
		t.Errorf("repair actions = %v", got)
	}
	for _, in := range []string{"allow_results_loss", "refresh_lists"} {
		m := regexp.MustCompile(`(?ms)^      ` + in + `:\n(.*?)(?:^      [a-z_0-9]+:|^permissions:)`).FindStringSubmatch(src)
		if m == nil || !strings.Contains(m[1], "default: false") {
			t.Errorf("input %s does not default to false", in)
		}
	}

	req := mustRead(t, osRequest)
	allowed := regexp.MustCompile(`os_host in \[([^\]]*)\]`).FindStringSubmatch(req)
	if allowed == nil {
		t.Fatal("the playbook does not check os_host against a list")
	}
	var accepted []string
	for _, o := range strings.Split(allowed[1], ",") {
		accepted = append(accepted, strings.Trim(strings.TrimSpace(o), "'"))
	}
	if !slices.Equal(accepted, osUpgradeHosts()) {
		t.Errorf("the playbook accepts %v, want %v", accepted, osUpgradeHosts())
	}
	for _, want := range []string{"os_confirm == 'UPGRADE-' ~ os_host", "os_confirm == 'DISARM-' ~ os_host", "os_confirm in ['REHEARSE-' ~ os_host, 'REHEARSE-' ~ os_host ~ '-CONSOLE']", "os_mode == 'survey' and os_host == '-'", "os_mode in ['survey', 'upgrade', 'rehearse', 'disarm', 'repair', 'freeze', 'thaw']", "os_confirm == 'FREEZE-' ~ os_host", "os_confirm == 'THAW-' ~ os_host", "os_confirm == 'REPAIR-' ~ os_host"} {
		if !strings.Contains(req, want) {
			t.Errorf("the playbook's request check lacks %s", want)
		}
	}
	if n := strings.Count(stripComments(mustRead(t, osPlaybook)), "import_tasks: tasks/os_upgrade_request.yml"); n != 2 {
		t.Errorf("%d plays validate the request, want 2 (localhost, then the preflight on every host)", n)
	}
}

func TestClusterOSUpgradePlanRefuses(t *testing.T) {
	plan := jobSection(t, withoutComments(readWorkflow(t, osWorkflow)), "plan")
	for _, want := range []string{
		`"$IN_CONFIRM" != "UPGRADE-$IN_HOST"`,
		`"$IN_CONFIRM" != "DISARM-$IN_HOST"`,
		`"$IN_CONFIRM" != "REHEARSE-$IN_HOST"`,
		`"$IN_CONFIRM" != "REPAIR-$IN_HOST"`,
		`"$IN_CONFIRM" != "FREEZE-$IN_HOST"`,
		`"$IN_CONFIRM" != "THAW-$IN_HOST"`,
		`OS_UPGRADE_ALLOWED_HOSTS`,
		`"$REF" != "refs/heads/$DEFAULT_BRANCH"`,
		`msa2-server|msr1|msa2-client) ;;`,
		`"$IN_HOST" != "-"`,
		`-gt 200`,
		`mode must be survey, rehearse, upgrade, disarm, repair, freeze or thaw`,
		`a survey takes no confirmation`,
		`reason must not contain braces`,
	} {
		if !strings.Contains(plan, want) {
			t.Errorf("the plan job no longer checks %s", want)
		}
	}
}

// Serial group, no cancelling, behind the idle guard, on a fresh verdict. Same rules as the reboot.
func TestClusterOSUpgradeJoinsTheGroupWithoutCancellingAnything(t *testing.T) {
	src := withoutComments(readWorkflow(t, osWorkflow))
	if regexp.MustCompile(`(?m)^concurrency:`).MatchString(src) {
		t.Error("workflow-level concurrency: the run would enter matrix-tier-cluster at dispatch, before the guard looked")
	}
	if strings.Contains(src, "cancel-in-progress: true") {
		t.Error("cancel-in-progress: true")
	}
	if n := strings.Count(src, "group: matrix-tier-cluster"); n != 1 {
		t.Errorf("%d concurrency groups named matrix-tier-cluster, want exactly 1 (the os-upgrade job)", n)
	}
	job := jobSection(t, src, "os-upgrade")
	if !regexp.MustCompile(`(?m)^    concurrency:\n      group: matrix-tier-cluster\n      cancel-in-progress: false\n`).MatchString(job) {
		t.Error("the os-upgrade job does not hold matrix-tier-cluster with cancel-in-progress: false")
	}
	for _, other := range []string{"plan", "cluster-guard", "verdict"} {
		if strings.Contains(jobSection(t, src, other), "concurrency:") {
			t.Errorf("job %s declares concurrency: only the os-upgrade job may hold the group", other)
		}
	}
	if !regexp.MustCompile(`needs: \[plan, cluster-guard\]`).MatchString(job) {
		t.Error("the os-upgrade job does not need the guard")
	}
	for _, want := range []string{
		"needs.cluster-guard.result == 'success'",
		"needs.cluster-guard.outputs.attempt == github.run_attempt",
		"github.ref == format('refs/heads/{0}', github.event.repository.default_branch)",
	} {
		if !strings.Contains(job, want) {
			t.Errorf("the os-upgrade job's if: lacks %s", want)
		}
	}
	guard := jobSection(t, src, "cluster-guard")
	if !strings.Contains(guard, `guard -idle -dir "$GUARD_DIR" -self "$RUN_ID"`) {
		t.Error("the guard job does not run `stresstally guard -idle`: the ordinary verdict lets a change queue behind a holder")
	}
	if !strings.Contains(guard, "bash tools/stresstally/guard.sh") {
		t.Error("the guard job does not list the open runs first (guard.sh)")
	}
	iAudit := mustIndex(t, job, "eviction-audit.sh", "the eviction audit")
	iStale := mustIndex(t, job, "Refuse a stale idle verdict", "the stale-verdict step")
	iLimit := mustIndex(t, job, `"$age" -gt 600`, "the ten-minute limit")
	iJoin := mustIndex(t, job, "Join tailnet", "the tailnet join")
	iPlay := mustIndex(t, job, "ansible/os-upgrade.yml", "the playbook run")
	if iAudit >= iStale || iStale >= iLimit || iLimit >= iJoin || iJoin >= iPlay {
		t.Error("order must be: eviction audit, stale-verdict refusal, tailnet join, playbook")
	}
	// the other workflows' guards must see this one as a holder
	g := mustRead(t, filepath.Join("tools", "stresstally", "guard.go"))
	if !strings.Contains(g, `".github/workflows/cluster-os-upgrade.yml"`) {
		t.Error("tools/stresstally/guard.go does not list cluster-os-upgrade.yml in clusterGroupWorkflows: a tier's guard would not see an OS upgrade holding the cluster")
	}
	if !strings.Contains(mustRead(t, filepath.Join("tools", "stresstally", "eviction-audit.sh")), "cluster-os-upgrade") {
		t.Error("eviction-audit.sh does not report a cancelled OS upgrade")
	}
}

func TestClusterOSUpgradeVerdictIsRedUnlessDone(t *testing.T) {
	v := jobSection(t, withoutComments(readWorkflow(t, osWorkflow)), "verdict")
	if !strings.Contains(v, `needs: [plan, cluster-guard, os-upgrade]`) || !strings.Contains(v, `"$RUN" != success`) {
		t.Error("the verdict job does not fail unless the os-upgrade job succeeded")
	}
	if !strings.Contains(v, "!cancelled()") {
		t.Error("the verdict job must run when the os-upgrade job was skipped")
	}
}

func TestClusterOSUpgradeWorkflowHygiene(t *testing.T) {
	src := withoutComments(readWorkflow(t, osWorkflow))
	lines := strings.Split(src, "\n")
	for i, l := range lines {
		if strings.Contains(l, "${{ inputs.") && !regexp.MustCompile(`^\s*(name|run-name|environment|[A-Z][A-Z_]*):`).MatchString(l) {
			t.Errorf("line %d: an input outside env:, run-name, environment or a name: %s", i+1, l)
		}
		if strings.Contains(l, "${{ secrets.") && !regexp.MustCompile(`^\s*(oauth-client-id|oauth-secret|[A-Z][A-Z_]*):`).MatchString(l) {
			t.Errorf("line %d: a secret outside env: or the tailscale step's with: %s", i+1, l)
		}
	}
	for i := 0; i < len(lines); i++ {
		m := regexp.MustCompile(`^(\s*)(?:- )?run:\s*(.*)$`).FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		if strings.Contains(m[2], "${{") {
			t.Errorf("line %d: an expression in a run: script", i+1)
		}
		if strings.TrimSpace(m[2]) != "|" {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			l := lines[j]
			if strings.TrimSpace(l) != "" && len(l)-len(strings.TrimLeft(l, " ")) <= len(m[1]) {
				break
			}
			if strings.Contains(l, "${{") {
				t.Errorf("line %d: an expression in a run: script: %s", j+1, l)
			}
		}
	}
	pinned := regexp.MustCompile(`^[\w.-]+/[\w.-]+@[0-9a-f]{40}$`)
	for _, u := range regexp.MustCompile(`(?m)^\s*(?:- )?uses:\s*(\S+)(.*)$`).FindAllStringSubmatch(src, -1) {
		if !pinned.MatchString(u[1]) || !regexp.MustCompile(`^\s+# v\d`).MatchString(u[2]) {
			t.Errorf("uses %s%s: pin by full commit sha with a # vX comment", u[1], u[2])
		}
	}
	if strings.Count(src, "secrets.CLUSTER_SSH_KEY") != 1 {
		t.Errorf("the cluster key must be given to exactly one step, found %d uses", strings.Count(src, "secrets.CLUSTER_SSH_KEY"))
	}
	if strings.Contains(src, "self-hosted") || strings.Contains(src, "celeris-cluster") {
		t.Error("a job names a cluster runner label: the upgrade must not depend on, or leave, a runner on a cluster host")
	}
	if strings.Contains(src, "CLUSTER_USE_LAN") {
		t.Error("CLUSTER_USE_LAN: the reconnect after the reboot must go over the tailnet, which proves tailscale came back")
	}
	if strings.Contains(src, "pull_request_target") {
		t.Error("pull_request_target")
	}
	perms := regexp.MustCompile(`(?m)^permissions:\n((?:[ ]+.*\n)+)`).FindStringSubmatch(src)
	if perms == nil || strings.TrimSpace(perms[1]) != "contents: read" {
		t.Errorf("top-level permissions must be exactly contents: read, got %q", perms)
	}
	if !strings.Contains(jobSection(t, src, "cluster-guard"), "actions: read") {
		t.Error("the guard cannot list runs without actions: read")
	}
	if !strings.Contains(src, "ansible_core_version") || !strings.Contains(src, "ansible/runner-setup.yml") {
		t.Error("the job does not install the ansible-core pin of ansible/runner-setup.yml")
	}
	// the survey step that talks to the internet runs only in survey mode and cannot fail the run
	if !strings.Contains(src, "if: ${{ env.OS_MODE == 'survey' }}") || !strings.Contains(src, "|| echo") {
		t.Error("the toolchain comparison is not survey-only and best-effort")
	}

	// the job outlives apt, a DKMS build, the reboot and the settling, and the ansible timeout fits inside the job
	job := jobSection(t, src, "os-upgrade")
	m := regexp.MustCompile(`timeout-minutes: (\d+)`).FindStringSubmatch(job)
	pb := mustRead(t, osPlaybook)
	rt := regexp.MustCompile(`reboot_timeout_seconds: (\d+)`).FindStringSubmatch(pb)
	retries := regexp.MustCompile(`upgrade_poll_retries: (\d+)`).FindStringSubmatch(pb)
	rounds := regexp.MustCompile(`upgrade_poll_rounds: (\d+)`).FindStringSubmatch(pb)
	at := regexp.MustCompile(`timeout -k 30 (\d+) ansible-playbook`).FindStringSubmatch(job)
	if m == nil || rt == nil || retries == nil || rounds == nil || at == nil {
		t.Fatal("no timeout-minutes on the job, reboot_timeout_seconds / upgrade_poll_retries / upgrade_poll_rounds in the playbook, or timeout on ansible-playbook")
	}
	jobMin, _ := strconv.Atoi(m[1])
	boot, _ := strconv.Atoi(rt[1])
	poll, _ := strconv.Atoi(retries[1])
	nround, _ := strconv.Atoi(rounds[1])
	play, _ := strconv.Atoi(at[1])
	if boot > 900 {
		t.Errorf("reboot_timeout_seconds = %d: the bound is 15 minutes", boot)
	}
	// every bounded step at its worst, from os_upgrade.py's own bounds: apt update 1500, three update-grub runs of 600, a DKMS
	// build 1800, an initramfs rebuild 900, two toolchain checks of 6 curls x 40 s, the settling loops, and the setup tasks
	const (
		aptUpdate, updateGrub, dkmsBuild, initramfs, toolchecks, settle, setup = 1500, 3 * 600, 1800, 900, 2 * 240, 36*5 + 24*5, 900
	)
	pollRounds := nround * (poll*(30+1) + 305) // each poll waits 30 s on the host; a round ends with up to 300 s of wait_for_connection
	need := aptUpdate + updateGrub + dkmsBuild + initramfs + toolchecks + settle + setup + pollRounds + boot + 30
	if play < need {
		t.Errorf("ansible-playbook is bounded at %d s but the bounded steps add up to %d s at their worst: the kill could land after the one-shot is armed", play, need)
	}
	if jobMin*60 < play+600 {
		t.Errorf("the job's %d-minute timeout does not cover the %d s playbook bound and the setup", jobMin, play)
	}
	script := mustRead(t, osScript)
	for _, want := range []string{"timeout=1800", "timeout=900", "timeout=1500", "timeout=600"} {
		if !strings.Contains(script, want) {
			t.Errorf("os_upgrade.py no longer has %q: the timeout arithmetic above is stale", want)
		}
	}
	// the host-changing modes run in an environment the maintainer can protect; a survey in one that carries no protection
	if !strings.Contains(job, "environment: ${{ inputs.mode == 'survey' && 'cluster-read' || 'cluster-host-change' }}") {
		t.Error("the os-upgrade job does not run host-changing modes in the cluster-host-change environment and surveys in cluster-read")
	}
	for _, other := range []string{"plan", "cluster-guard", "verdict"} {
		if strings.Contains(jobSection(t, src, other), "environment:") {
			t.Errorf("job %s names an environment: only the os-upgrade job may", other)
		}
	}
}

// The playbook: no shell/command task, every host command is a subcommand of os_upgrade.py and only the ones listed,
// they run in the order that makes a failed boot fall back to the old kernel, the new kernel becomes the default
// only after the verification, the reboot is the only one and carries no free text.
func TestClusterOSUpgradePlaybookSafety(t *testing.T) {
	raw := mustRead(t, osPlaybook)
	src := stripComments(raw)
	if n := strings.Count(src, "ansible.builtin.reboot:"); n != 1 {
		t.Errorf("%d reboot tasks, want exactly 1", n)
	}
	if n := strings.Count(src, "any_errors_fatal: true"); n < 3 {
		t.Errorf("%d plays with any_errors_fatal, want the request check, the preflight and the target: a peer that does not answer must stop the run", n)
	}
	if !strings.Contains(src, "hostvars[os_host].os_upgrade_verified | default(false)") || !strings.Contains(src, "os_upgrade_verified: true") {
		t.Error("no last play proves the target play ran to its end: a host pattern that matched nothing would read as success")
	}
	task := regexp.MustCompile(`(?s)ansible\.builtin\.reboot:\n(.*?)\n\s+- name:`).FindStringSubmatch(src)
	if task == nil {
		t.Fatal("cannot find the reboot task")
	}
	if strings.Contains(task[1], "os_reason") || !strings.Contains(task[1], `msg: "cluster-os-upgrade workflow run {{ os_run_id }}"`) {
		t.Errorf("the reboot message must be fixed text and the validated run id: the module puts it inside double quotes in a root shell command, so free text there is a shell injection:\n%s", task[1])
	}
	if !strings.Contains(task[1], `reboot_timeout: "{{ reboot_timeout_seconds | int }}"`) {
		t.Error("the reboot wait is not bounded by reboot_timeout_seconds")
	}
	for _, banned := range []string{"ansible.builtin.shell:", "ansible.builtin.command:", "ansible.builtin.raw:", "ansible.builtin.service:", "ansible.builtin.systemd:",
		"ansible.builtin.apt:", "ansible.builtin.package:", "ansible.builtin.lineinfile:", "ansible.builtin.replace:", "state: absent", "state: restarted", "state: stopped", "docker", "autoremove"} {
		if strings.Contains(src, banned) {
			t.Errorf("the playbook uses %s: a host is touched only through the subcommands of os_upgrade.py and the reboot", banned)
		}
	}
	if !strings.Contains(src, `hosts: "{{ os_host if ((os_mode | default('survey')) in ['upgrade', 'rehearse']) else 'none' }}"`) {
		t.Error("the target play does not run on os_host in upgrade mode only")
	}

	// the subcommands the playbook may run on a host
	allowed := []string{"apt-update", "survey", "simulate", "dump", "pin-grub", "upgrade-start", "upgrade-status", "upgrade-log",
		"blacklist-r8169", "dkms-check", "arm-oneshot", "make-permanent", "toolcheck", "upgrade-cleanup", "disarm", "repair-start", "arm-rehearsal", "record-rehearsal", "hold-access", "release-access", "freeze", "thaw"}
	all := src
	tasks, _ := filepath.Glob("ansible/tasks/os_upgrade_*.yml")
	for _, tf := range tasks {
		all += "\n" + stripComments(mustRead(t, tf))
	}
	for _, m := range regexp.MustCompile(`\{\{ osu \}\} ([a-z0-9-]+)`).FindAllStringSubmatch(all, -1) {
		if !slices.Contains(allowed, m[1]) {
			t.Errorf("the playbook runs the subcommand %q, which is not on the reviewed list %v", m[1], allowed)
		}
	}
	// the reboot_snapshot.sh record is read, never changed: it is a script with no write verb (its own tests)

	// the order
	idx := func(needle string) int { return mustIndex(t, src, needle, needle) }
	order := []string{
		"Target | refuse when vars/os_upgrade_judge.yml says so",
		"osu }} pin-grub",
		"Apt | update the package lists",
		"Apt | refuse when the plan is unacceptable",
		"osu }} upgrade-start",
		"tasks/os_upgrade_wait_apt.yml",
		"Apt | the full-upgrade must have finished with exit 0",
		"osu }} dkms-check --build",
		"Dkms | REFUSE TO REBOOT",
		"'arm-oneshot' if (os_mode == 'upgrade') else 'arm-rehearsal'",
		"ansible.builtin.reboot:",
		"Post | judge",
		"osu }} make-permanent",
	}
	last := -1
	for _, o := range order {
		i := idx(o)
		if i <= last {
			t.Errorf("order must be: %s; %q is out of place", strings.Join(order, " -> "), o)
		}
		last = i
	}
	for _, once := range []string{"osu }} pin-grub", "'arm-oneshot' if (os_mode == 'upgrade') else 'arm-rehearsal'", "osu }} make-permanent", "osu }} upgrade-start"} {
		if n := strings.Count(src, once); n != 1 {
			t.Errorf("%q appears %d times, want exactly 1", once, n)
		}
	}
	// the pin happens before the first package change (the upgrade is also before the dkms-check)
	if idx("osu }} pin-grub") > idx("Apt | update the package lists") {
		t.Error("the old kernel is not pinned before apt runs")
	}
	// mode=rehearse reuses the target play but skips every package step; make-permanent lives only in the upgrade block
	for _, n := range []string{"- name: Upgrade | the packages, DKMS and the checks of the new kernel\n      when: os_mode == 'upgrade'",
		"- name: Permanent | the new kernel becomes the default\n      when: os_mode == 'upgrade'", "- name: Rehearse | the kernel under test is the running one"} {
		if !strings.Contains(src, n) {
			t.Errorf("the playbook lost %q", n)
		}
	}
	// the blacklist is written on an r8127 host only, and the playbook wires the cleanup of a dead run's policy-rc.d
	if !strings.Contains(src, "when: (blacklist_r8169 | bool) and pre_target.r8127") {
		t.Error("blacklist_r8169 is no longer limited to a host with an r8127 NIC")
	}
	if strings.Count(src, "osu }} upgrade-cleanup --run-id") < 2 || !strings.Contains(src, "Apt | take our policy-rc.d away if the run that left it is over") {
		t.Error("the playbook does not clean up the policy-rc.d of a run that died")
	}
	// the two tasks that can leave the host armed must reach the rescue even when the host is UNREACHABLE (ansible does not rescue those)
	for _, n := range []string{"'arm-oneshot' if (os_mode == 'upgrade') else 'arm-rehearsal'", "ansible.builtin.reboot:"} {
		i := idx(n)
		j := strings.Index(src[i:], "- name:")
		if j < 0 {
			j = 600
		}
		if !strings.Contains(src[i:i+j+400], "ignore_unreachable: true") {
			t.Errorf("the task with %q does not carry ignore_unreachable: an unreachable host would skip the rescue", n)
		}
	}
	for _, n := range []string{"when: arm_raw is unreachable", "when: reboot_result is unreachable", "Target | record the phase (from here on a one-shot may be armed)"} {
		if !strings.Contains(src, n) {
			t.Errorf("the playbook lost %q", n)
		}
	}
	// a failure between the arming and the host's return disarms the one-shot: arm and reboot sit in a block whose rescue disarms
	iArm := idx("'arm-oneshot' if (os_mode == 'upgrade') else 'arm-rehearsal'")
	iRescue := idx("\n      rescue:")
	iDisarm := strings.Index(src[iRescue:], "osu }} disarm")
	if iArm >= iRescue || iDisarm < 0 || iRescue+iDisarm >= idx("Post | judge") || idx("ansible.builtin.reboot:") >= iRescue {
		t.Error("the arming and the reboot are not in a block whose rescue disarms the one-shot before the verification")
	}
	// R3-m4: the access path is held before the plan is simulated, and a refusal releases what the run held
	if idx("osu }} hold-access") >= idx("osu }} simulate") || idx("osu }} simulate") >= idx("osu }} upgrade-start") {
		t.Error("the access-path holds are not put before the simulation")
	}
	if !strings.Contains(src, "osu }} release-access --run-id") {
		t.Error("a refused plan does not release the holds the run put")
	}
	// the judgement gates each step
	for _, want := range []string{"when: refusals_str | trim | length > 0", "when: plan_refusals_str | trim | length > 0",
		"when: pre_reboot_problems_str | trim | length > 0", "when: post_problems_str | trim | length > 0"} {
		if !strings.Contains(src, want) {
			t.Errorf("the playbook does not act on the judgement: %q is missing", want)
		}
	}
	if !strings.Contains(src, "ControlMaster=no") || !strings.Contains(src, "ControlPath=none") {
		t.Error("a multiplexed ssh socket may outlive the reboot: switch ControlMaster off for the plays")
	}
	// the survey's preflight takes no confirmation and changes nothing but, when asked, the apt lists
	pre := regexp.MustCompile(`(?s)preflight and survey on every host.*?- name: OS upgrade \| the target`).FindString(src)
	for _, banned := range []string{"pin-grub", "upgrade-start", "arm-oneshot", "make-permanent", "ansible.builtin.reboot"} {
		if strings.Contains(pre, banned) {
			t.Errorf("the preflight/survey play contains %s", banned)
		}
	}
}

// The script: autoremove never runs, the read-only subcommands run in READ_ONLY mode, nothing else reaches a host.
func TestOSUpgradeScriptCannotAutoremoveAndGuardsItsReads(t *testing.T) {
	raw := mustRead(t, osScript)
	code := regexp.MustCompile(`(?s)""".*?"""`).ReplaceAllString(raw, "")
	code = stripComments(code)
	// the one place that removes packages is mode=repair's abort, and only the half-installed newer kernel's
	repairBody := regexp.MustCompile(`(?s)def cmd_repair_run\(a\):.*?\ndef cmd_upgrade_run`).FindString(code)
	if repairBody == "" || !strings.Contains(repairBody, "new_kernel_pkgs(running_kernel(), broken_only=True)") || strings.Count(code, `"remove", "--purge"`) != 2 || strings.Count(repairBody, `"remove", "--purge"`) != 2 || !strings.Contains(repairBody, `"apt-get", "-s"`) {
		t.Error("the only package removal is not the abort of mode=repair, limited to the half-installed newer kernels")
	}
	code = strings.Replace(code, repairBody, "", 1)
	for _, banned := range []string{"autoremove", `"purge"`, `"remove"`, "--purge", "rm -rf", `"reboot"`, `"shutdown"`, `"poweroff"`, "systemctl\", \"restart", "kexec"} {
		if strings.Contains(code, banned) {
			t.Errorf("os_upgrade.py contains %s", banned)
		}
	}
	if !strings.Contains(code, `a.cmd in ("survey", "simulate", "dump", "upgrade-status", "upgrade-log", "marker")`) {
		t.Error("the read-only subcommands are no longer run in READ_ONLY mode")
	}
	for _, want := range []string{"--force-confold", "--force-confdef", `"NEEDRESTART_MODE": "l"`, `"DEBIAN_FRONTEND": "noninteractive"`, "exit 101", "full-upgrade", "start_new_session=True"} {
		if !strings.Contains(code, want) {
			t.Errorf("os_upgrade.py lost %s", want)
		}
	}
	// grub-set-default is called in exactly these places: pinning the OLD kernel, re-asserting it when arming, and the permanent step
	if n := strings.Count(code, `run(["grub-set-default"`); n != 3 {
		t.Errorf("%d grub-set-default calls, want 3 (pin-grub, arm-oneshot, make-permanent)", n)
	}
	if n := strings.Count(code, `run(["grub-reboot"`); n != 2 {
		t.Errorf("%d grub-reboot calls, want 2 (arm-oneshot, arm-rehearsal)", n)
	}
}

// The judgement keeps every refusal the maintainer asked for. These are the sentences the step summary and the
// failure print; the Jinja is exercised for real by test_os_upgrade_judge.py.
func TestClusterOSUpgradeJudgementCoversTheRequirements(t *testing.T) {
	src := mustRead(t, osJudge)
	for _, want := range []string{
		"a bench or validation manifest", "runner job(s) are executing", "resume-pending.json", "results_root", "no shutdown quiesce hook",
		"r8127 module is not available for the running kernel", "tailscaled is running but", "not enough free disk on /:", "not enough free disk on /boot:",
		"dpkg or apt is busy", "dpkg --audit", "held kernel, boot or DKMS packages", "policy-rc.d", "no usable GRUB", "GRUB is not proven to have booted this host", "GRUB_SAVEDEFAULT=true",
		"grub cannot write its environment block", "no menu entry for the running kernel", "order: ",
		"would REMOVE protected package(s)", "apt-get -s full-upgrade failed",
		"no successful one-shot rehearsal", "was armed as a one-shot by run", "a one-shot is still pending (", "vmlinuz-", "initrd.img-", "has no menu entry for", "the fallback is not intact", "waits at the menu for ever", "linux-headers for",
		"are installed for the running kernel and not for", "dkms autoinstall -k", "the driver(s) of the NICs in use", "is NOT built for",
		"no PCI aliases", "Secure Boot is ", "lists the same PCI id as r8127", "boot_id did not change", "the one-shot entry for",
		"new crash dump(s)", "prerequisite regressed", "is not owned by", "is not in /proc/interrupts", "is not configured on the host",
		"over the fabric", "bond lost a slave", "the counters were not reset", "new failed unit(s)", "link speed", "is bound to", "the clock is not synchronized",
		"the system C compiler built and ran a program before", "verified before the upgrade", "one-shot entry pending", "an earlier OS upgrade left grub", "blacklist_r8169=true, but a NIC is bound to r8169", "no readable snapshot",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the judgement lost %q", want)
		}
	}
	for _, k := range []string{"tailscaled_active", "apcupsd_active", "resume_check_enabled", "power_event_installed", "tmpfiles_conf", "quiesce_hook"} {
		if !strings.Contains(src, "'"+k+"'") {
			t.Errorf("the judgement no longer compares prerequisite %s before and after", k)
		}
	}
}

func pythonOrSkip(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	return p
}

// TestOSUpgradeScript runs ansible/tests/test_os_upgrade.py: the grub menu parser, the survey (read-only: nothing
// written, no changing command run), and each host-changing subcommand against a fake host with stub commands.
// The suite is ~200 tests that each start dozens of stub processes, and the root package has a 5 minute budget under -race:
// it runs as OSU_SHARD=i/n shards, one python process each, at once (the tests share nothing; each has its own temp dir).
func TestOSUpgradeScript(t *testing.T) {
	py := pythonOrSkip(t)
	n := min(max(runtime.NumCPU(), 1), 4)
	type res struct {
		out []byte
		err error
	}
	results := make([]res, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(py, "-I", filepath.Join("ansible", "tests", "test_os_upgrade.py"))
			cmd.Env = append(os.Environ(), "OSU_SHARD="+strconv.Itoa(i)+"/"+strconv.Itoa(n))
			results[i].out, results[i].err = cmd.CombinedOutput()
		}()
	}
	wg.Wait()
	ran, failed := 0, false
	for i, r := range results {
		if r.err != nil {
			failed = true
			t.Errorf("test_os_upgrade.py shard %d/%d FAILED: %v\n%s", i, n, r.err, r.out)
			continue
		}
		m := regexp.MustCompile(`Ran (\d+) tests?`).FindSubmatch(r.out)
		if m == nil || !bytes.Contains(r.out, []byte("OK")) {
			failed = true
			t.Errorf("test_os_upgrade.py shard %d/%d did not report OK:\n%s", i, n, r.out)
			continue
		}
		k, _ := strconv.Atoi(string(m[1]))
		ran += k
	}
	if ran < 200 && !failed { // after a failed shard the count is only a consequence of it
		t.Errorf("the shards ran %d tests in all: the suite has more than 200 (a shard that silently ran nothing?)", ran)
	}
}

// TestOSUpgradeJudgePlay runs the real judgement under ansible (test_os_upgrade_judge.py). Opt-in like the other
// play tests: the ansible CI job installs the cluster's pin and runs the script directly.
func TestOSUpgradeJudgePlay(t *testing.T) {
	py := pythonOrSkip(t)
	if os.Getenv("PROBATORIUM_ANSIBLE_TESTS") != "1" {
		t.Skip("set PROBATORIUM_ANSIBLE_TESTS=1 to run the judgement under ansible (needs ansible-playbook); the ansible CI job runs it")
	}
	if _, err := exec.LookPath("ansible-playbook"); err != nil {
		t.Fatal("PROBATORIUM_ANSIBLE_TESTS=1 but ansible-playbook is not installed")
	}
	out, err := exec.Command(py, "-I", filepath.Join("ansible", "tests", "test_os_upgrade_judge.py")).CombinedOutput()
	if err != nil {
		t.Fatalf("test_os_upgrade_judge.py failed: %v\n%s", err, out)
	}
}

// TestOSUpgradePlay runs the real playbook end to end on local connections (test_os_upgrade_play.py), with only the
// restart, the host's commands and sudo replaced.
func TestOSUpgradePlay(t *testing.T) {
	py := linuxPython(t)
	if os.Getenv("PROBATORIUM_ANSIBLE_TESTS") != "1" {
		t.Skip("set PROBATORIUM_ANSIBLE_TESTS=1 to run the playbook (needs ansible-playbook); the ansible CI job runs it")
	}
	if _, err := exec.LookPath("ansible-playbook"); err != nil {
		t.Fatal("PROBATORIUM_ANSIBLE_TESTS=1 but ansible-playbook is not installed")
	}
	out, err := exec.Command(py, "-I", filepath.Join("ansible", "tests", "test_os_upgrade_play.py")).CombinedOutput()
	if err != nil {
		t.Fatalf("test_os_upgrade_play.py failed: %v\n%s", err, out)
	}
}

// TestOSUpgradeRealHosts replays the three real hosts' records (the first real survey, run 39814880117) through the summary's
// checklist and the judgement (test_os_upgrade_real_hosts.py): the row that read FAIL on every host because of two idle daemons is PASS.
func TestOSUpgradeRealHosts(t *testing.T) {
	py := pythonOrSkip(t)
	if os.Getenv("PROBATORIUM_ANSIBLE_TESTS") != "1" {
		t.Skip("set PROBATORIUM_ANSIBLE_TESTS=1 to replay the real hosts' records (needs jq and ansible-playbook); the ansible CI job runs it")
	}
	if _, err := exec.LookPath("ansible-playbook"); err != nil {
		t.Fatal("PROBATORIUM_ANSIBLE_TESTS=1 but ansible-playbook is not installed")
	}
	out, err := exec.Command(py, "-I", filepath.Join("ansible", "tests", "test_os_upgrade_real_hosts.py")).CombinedOutput()
	if err != nil {
		t.Fatalf("test_os_upgrade_real_hosts.py failed: %v\n%s", err, out)
	}
}

func linuxPython(t *testing.T) string {
	t.Helper()
	_ = linuxBash(t) // skips off Linux: the play reads /proc with GNU tools
	return pythonOrSkip(t)
}

// The ansible CI job runs the three python tests under the cluster's ansible pin.
func TestAnsibleJobRunsTheOSUpgradeTests(t *testing.T) {
	wf := mustRead(t, filepath.Join(".github", "workflows", "test.yml"))
	for _, s := range []string{"test_os_upgrade", "test_os_upgrade_judge", "test_os_upgrade_play", "test_os_upgrade_real_hosts"} {
		if !regexp.MustCompile(`(?m)^\s*run: python3 -I ansible/tests/` + s + `\.py\s*$`).MatchString(wf) {
			t.Errorf("test.yml's ansible job does not run `python3 -I ansible/tests/%s.py`", s)
		}
	}
}

// The toolchain comparison reads the pins of the roles themselves: every role has a row, and the pins are the
// repository's.
func TestOSUpgradeToolchainPins(t *testing.T) {
	bash := needBash(t)
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not installed")
	}
	out, err := exec.Command(bash, filepath.Join(".github", "scripts", "os-upgrade-toolchains.sh"), "--pins-only").Output()
	if err != nil {
		t.Fatalf("--pins-only: %v", err)
	}
	var rows []struct{ Name, Kind, Pin, Where string }
	if err := json.Unmarshal(out, &rows); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	pin := map[string]string{}
	for _, r := range rows {
		pin[r.Name] = r.Pin
		if r.Pin == "" || r.Where == "" || (r.Kind != "pinned" && r.Kind != "floating") {
			t.Errorf("incomplete row %+v", r)
		}
	}
	gomod := mustRead(t, "go.mod")
	goVer := regexp.MustCompile(`(?m)^go (\S+)$`).FindStringSubmatch(gomod)[1]
	if !strings.Contains(pin["go (the module and the CI)"], goVer) {
		t.Errorf("go pin %q does not carry go.mod's %s", pin["go (the module and the CI)"], goVer)
	}
	for name, file := range map[string]string{
		"dotnet sdk":         "ansible/roles/dotnet/defaults/main.yml",
		"drogon (libdrogon)": "ansible/roles/cpp/defaults/main.yml",
		"uv":                 "ansible/runner-setup.yml",
		"ansible-core":       "ansible/runner-setup.yml",
		"actions runner":     ".github/actions/cluster-runner-up/action.yml",
		"postgres image":     "ansible/roles/dbservices/tasks/main.yml",
	} {
		if !strings.Contains(mustRead(t, file), pin[name]) || pin[name] == "" {
			t.Errorf("row %q: pin %q is not in %s", name, pin[name], file)
		}
	}
	// every role that installs something has a row
	roles, _ := filepath.Glob("ansible/roles/*")
	want := map[string]string{"bun": "bun", "c": "h2o", "cpp": "drogon", "dbservices": "postgres", "dotnet": "dotnet", "java": "java", "node": "node", "python": "python", "rust": "rust", "zig": "zig"}
	for _, r := range roles {
		base := filepath.Base(r)
		key, ok := want[base]
		if !ok {
			t.Errorf("ansible/roles/%s has no entry in this test's table: add its toolchain to os-upgrade-toolchains.sh", base)
			continue
		}
		found := false
		for n := range pin {
			if strings.HasPrefix(n, key) {
				found = true
			}
		}
		if !found {
			t.Errorf("no toolchain row for role %s", base)
		}
	}
}

// The step summary of a survey with no record at all still renders (a run that failed before the hosts answered).
func TestOSUpgradeSummaryWithNothingRecorded(t *testing.T) {
	bash := needBash(t)
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not installed")
	}
	script := filepath.Join(".github", "scripts", "os-upgrade-summary.sh")
	dir := t.TempDir()
	b, err := exec.Command(bash, script, dir, "survey", "-", "2", "r `x`", "9").CombinedOutput()
	if err != nil {
		t.Fatalf("survey summary: %v\n%s", err, b)
	}
	if !strings.Contains(string(b), "SURVEY FAILED") || !strings.Contains(string(b), "no survey record") || !strings.Contains(string(b), "r 'x'") {
		t.Errorf("survey summary:\n%s", b)
	}
	b, err = exec.Command(bash, script, dir, "upgrade", "msr1", "2", "x", "9").CombinedOutput()
	if err != nil {
		t.Fatalf("upgrade summary: %v\n%s", err, b)
	}
	if !strings.Contains(string(b), "Nothing was rebooted") || !strings.Contains(string(b), "NOT COMPLETE") {
		t.Errorf("upgrade summary:\n%s", b)
	}
}

// The upgrade is documented where an operator looks: the exact dispatches, the order rule, the refusals, the
// one-shot, the recovery, the r8127 caveat and the risks.
func TestOSUpgradeIsDocumented(t *testing.T) {
	readme := mustRead(t, "README.md")
	i := strings.Index(readme, "### Upgrading a cluster host's OS")
	if i < 0 {
		t.Fatal("README.md has no \"Upgrading a cluster host's OS\" section")
	}
	sec := readme[i:]
	if j := strings.Index(sec[4:], "\n## "); j >= 0 {
		sec = sec[:j+4]
	}
	for _, want := range []string{
		"gh workflow run cluster-os-upgrade.yml",
		"mode=survey",
		"mode=upgrade",
		"confirm=UPGRADE-",
		"servers first",
		"msa2-client",
		"idle",
		"matrix-tier-cluster",
		"grub-reboot",
		"saved default",
		"physical power-cycle",
		"r8127",
		"r8169",
		"policy-rc.d",
		"autoremove",
		"HOLD-CLUSTER",
		"ansible/os-upgrade.yml",
		"allow_results_loss",
		"mode=disarm",
		"OS_UPGRADE_ALLOWED_HOSTS",
		"cluster-host-change",
		"abort-new-kernel",
		"Required reviewers",
		"still armed",
		"restores the choice of kernel only",
		"blacklist_r8169",
		"refresh_lists",
		"stub",
		"only when GRUB consumed its one-shot",
		"first_entry_kernel",
		"Rehearsal at a console",
		"mode=freeze",
		"mode=thaw",
		"FREEZE-<host>",
		"THAW-<host>",
		"zzz-celeris-freeze.conf",
		"freeze.drifted",
		"apt-daily-upgrade.timer",
		"unattended-upgrades.service",
	} {
		if !strings.Contains(sec, want) {
			t.Errorf("the README section lacks %q", want)
		}
	}
	if !strings.Contains(mustRead(t, filepath.Join("ansible", "RUNNER_BOOTSTRAP.md")), "cluster-os-upgrade.yml") {
		t.Error("RUNNER_BOOTSTRAP.md does not point at the sanctioned OS upgrade path")
	}
}

// Free text is data. ansible renders extra-vars from a file as Jinja templates, on a runner that holds the cluster
// key, in survey mode too (no typed confirmation there): reason and confirm must reach it as unsafe strings.
func TestFreeTextInputsAreWrittenAsUnsafeStrings(t *testing.T) {
	for _, c := range []struct{ file, confirm, reason string }{
		{osWorkflow, "os_confirm", "os_reason"},
		{"cluster-reboot.yml", "reboot_confirm", "reboot_reason"},
	} {
		src := withoutComments(readWorkflow(t, c.file))
		for _, key := range []string{c.confirm, c.reason} {
			want := `"` + key + `": {"__ansible_unsafe": os.environ[`
			if !strings.Contains(src, want) {
				t.Errorf("%s writes %s without __ansible_unsafe: a reason like {{ lookup('pipe', ...) }} would run on the runner", c.file, key)
			}
		}
		if !strings.Contains(src, `*'{'* | *'}'*)`) {
			t.Errorf("%s does not reject braces in the reason", c.file)
		}
	}
	// the other string inputs of the os-upgrade workflow are literals too, and not only the free text
	osSrc := withoutComments(readWorkflow(t, osWorkflow))
	for _, key := range []string{"os_mode", "os_host", "repair_action", "retry_kernel", "hold_access_packages"} {
		if !strings.Contains(osSrc, `"`+key+`": {"__ansible_unsafe": os.environ[`) {
			t.Errorf("%s writes %s without __ansible_unsafe: a template in a choice input would run on the runner", osWorkflow, key)
		}
	}
	if !strings.Contains(mustRead(t, osRequest), "os_mode == 'survey' and os_confirm == ''") {
		t.Error("the playbook accepts a confirmation in a survey")
	}
}

// planScript is the inline script of the plan job's first step, as the runner would execute it.
func planScript(t *testing.T) string {
	t.Helper()
	src := readWorkflow(t, osWorkflow)
	i := strings.Index(src, "Refuse a malformed request before any host is touched")
	if i < 0 {
		t.Fatal("no plan step")
	}
	j := strings.Index(src[i:], "run: |\n")
	if j < 0 {
		t.Fatal("the plan step has no run: |")
	}
	var out []string
	indent := -1
	for _, l := range strings.Split(src[i+j+len("run: |\n"):], "\n") {
		if strings.TrimSpace(l) == "" {
			out = append(out, "")
			continue
		}
		n := len(l) - len(strings.TrimLeft(l, " "))
		if indent < 0 {
			indent = n
		}
		if n < indent {
			break
		}
		out = append(out, l[indent:])
	}
	return strings.Join(out, "\n")
}

// The plan job's gate, RUN: who may dispatch what. A host-changing mode is impossible until the maintainer lists the host in
// the repository variable OS_UPGRADE_ALLOWED_HOSTS; free text cannot carry template syntax; the confirmations are exact.
func TestClusterOSUpgradePlanScriptBehaviour(t *testing.T) {
	bash := needBash(t)
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not installed")
	}
	script := planScript(t)
	// a gh that answers from a fixture: the environment's protection rules, or an error
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\ncat \"$GH_FIXTURE\"\nexit \"${GH_RC:-0}\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fixtures := map[string]string{
		"good":       `{"name":"cluster-host-change","protection_rules":[{"type":"required_reviewers","reviewers":[{"type":"User","reviewer":{"login":"maintainer"}}]}],"deployment_branch_policy":{"protected_branches":false,"custom_branch_policies":true}}`,
		"noreviewer": `{"name":"cluster-host-change","protection_rules":[],"deployment_branch_policy":{"protected_branches":false,"custom_branch_policies":true}}`,
		"emptyrevs":  `{"name":"cluster-host-change","protection_rules":[{"type":"required_reviewers","reviewers":[]}],"deployment_branch_policy":{"custom_branch_policies":true}}`,
		"nobranches": `{"name":"cluster-host-change","protection_rules":[{"type":"required_reviewers","reviewers":[{"type":"User"}]}],"deployment_branch_policy":null}`,
		"waitonly":   `{"name":"cluster-host-change","protection_rules":[{"type":"wait_timer","wait_timer":5}],"deployment_branch_policy":{"custom_branch_policies":true}}`,
		"notfound":   `gh: Not Found (HTTP 404)`,
		"forbidden":  `gh: Resource not accessible by integration (HTTP 403)`,
		"notjson":    `<html>`,
	}
	for k, v := range fixtures {
		if err := os.WriteFile(filepath.Join(bin, k+".json"), []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	type env = map[string]string
	base := func(over env) env {
		e := env{"IN_MODE": "survey", "IN_HOST": "-", "IN_CONFIRM": "", "IN_REASON": "platform survey", "IN_REPAIR_ACTION": "configure",
			"IN_RETRY_KERNEL": "", "IN_HOLD": "auto",
			"ALLOWED_HOSTS": "", "REF": "refs/heads/main", "DEFAULT_BRANCH": "main", "REPO": "goceleris/probatorium",
			"GH_FIXTURE": filepath.Join(bin, "good.json"), "GH_RC": "0"}
		for k, v := range over {
			e[k] = v
		}
		return e
	}
	up := func(host string, over env) env {
		e := env{"IN_MODE": "upgrade", "IN_HOST": host, "IN_CONFIRM": "UPGRADE-" + host, "ALLOWED_HOSTS": host}
		for k, v := range over {
			e[k] = v
		}
		return base(e)
	}
	envFix := func(name string, rc string) env {
		return env{"GH_FIXTURE": filepath.Join(bin, name+".json"), "GH_RC": rc}
	}
	rep := func(over env) env {
		e := env{"IN_MODE": "repair", "IN_HOST": "msr1", "IN_CONFIRM": "REPAIR-msr1", "ALLOWED_HOSTS": "msr1"}
		for k, v := range over {
			e[k] = v
		}
		return base(e)
	}
	fz := func(mode string, over env) env {
		e := env{"IN_MODE": mode, "IN_HOST": "msr1", "IN_CONFIRM": strings.ToUpper(mode) + "-msr1", "ALLOWED_HOSTS": "msr1"}
		for k, v := range over {
			e[k] = v
		}
		return base(e)
	}
	for _, c := range []struct {
		name string
		env  env
		ok   bool
		msg  string
	}{
		{"a survey needs no switch and no environment", base(env{"GH_FIXTURE": filepath.Join(bin, "notfound.json"), "GH_RC": "1"}), true, ""},
		{"a survey with a host", base(env{"IN_HOST": "msr1"}), false, "leave host as '-'"},
		{"a survey with a template as the repair action", base(env{"IN_REPAIR_ACTION": "{{ lookup('pipe','id') }}"}), false, "repair_action must be"},
		{"a survey with an unknown repair action", base(env{"IN_REPAIR_ACTION": "rm-rf"}), false, "repair_action must be"},
		{"a survey with a template as the hold", base(env{"IN_HOLD": "{{ lookup('pipe','id') }}"}), false, "hold_access_packages must be"},
		{"a survey with an unknown hold", base(env{"IN_HOLD": "maybe"}), false, "hold_access_packages must be"},
		{"a survey with a template as the retry kernel", base(env{"IN_RETRY_KERNEL": "{{ x }}"}), false, "retry_kernel must be"},
		{"a survey with a retry kernel", base(env{"IN_RETRY_KERNEL": "latest"}), false, "retry_kernel must be"},
		{"a survey with a template as the host", base(env{"IN_HOST": "{{ lookup('pipe','id') }}"}), false, "host must be"},
		{"a survey with a template as the mode", base(env{"IN_MODE": "{{ lookup('pipe','id') }}"}), false, "mode must be"},
		{"a survey with a confirmation", base(env{"IN_CONFIRM": "UPGRADE-msr1"}), false, "takes no confirmation"},
		{"an upgrade with the variable empty", up("msr1", env{"ALLOWED_HOSTS": ""}), false, "OS_UPGRADE_ALLOWED_HOSTS"},
		{"an upgrade of a host that is not listed", up("msr1", env{"ALLOWED_HOSTS": "msa2-server"}), false, "is not in the repository variable"},
		{"a host that merely starts like a listed one", up("msr1", env{"ALLOWED_HOSTS": "msr10"}), false, "is not in the repository variable"},
		{"an upgrade of a listed host", up("msr1", env{"ALLOWED_HOSTS": "msa2-server,msr1"}), true, ""},
		{"a list separated by spaces", up("msa2-server", env{"ALLOWED_HOSTS": "msr1 msa2-server"}), true, ""},
		{"an upgrade with the wrong confirmation", up("msr1", env{"IN_CONFIRM": "UPGRADE-msa2-server"}), false, "confirm must be exactly UPGRADE-msr1"},
		{"an upgrade confirmed in lower case", up("msr1", env{"IN_CONFIRM": "upgrade-msr1"}), false, "confirm must be exactly UPGRADE-msr1"},
		{"an upgrade confirmed in mixed case", up("msr1", env{"IN_CONFIRM": "Upgrade-msr1"}), false, "confirm must be exactly UPGRADE-msr1"},
		{"an upgrade confirmed with a trailing space", up("msr1", env{"IN_CONFIRM": "UPGRADE-msr1 "}), false, "confirm must be exactly UPGRADE-msr1"},
		{"an upgrade confirmed as a disarm", up("msr1", env{"IN_CONFIRM": "DISARM-msr1"}), false, "confirm must be exactly UPGRADE-msr1"},
		{"an upgrade of no host", up("-", env{"IN_HOST": "-", "IN_CONFIRM": "UPGRADE--"}), false, "names ONE host"},
		{"an upgrade of all", up("all", nil), false, "host must be"},
		{"a disarm is gated too", base(env{"IN_MODE": "disarm", "IN_HOST": "msr1", "IN_CONFIRM": "DISARM-msr1"}), false, "OS_UPGRADE_ALLOWED_HOSTS"},
		{"a listed disarm", base(env{"IN_MODE": "disarm", "IN_HOST": "msr1", "IN_CONFIRM": "DISARM-msr1", "ALLOWED_HOSTS": "msr1"}), true, ""},
		{"a disarm confirmed in lower case", base(env{"IN_MODE": "disarm", "IN_HOST": "msr1", "IN_CONFIRM": "disarm-msr1", "ALLOWED_HOSTS": "msr1"}), false, "confirm must be exactly DISARM-msr1"},
		{"a rehearsal with its confirmation", base(env{"IN_MODE": "rehearse", "IN_HOST": "msr1", "IN_CONFIRM": "REHEARSE-msr1", "ALLOWED_HOSTS": "msr1"}), true, ""},
		{"a rehearsal with the console confirmation", base(env{"IN_MODE": "rehearse", "IN_HOST": "msr1", "IN_CONFIRM": "REHEARSE-msr1-CONSOLE", "ALLOWED_HOSTS": "msr1"}), true, "CONSOLE ATTESTED: the dispatcher states that a person is at the rack of msr1 with a console"},
		{"a rehearsal without it says no console is attested", base(env{"IN_MODE": "rehearse", "IN_HOST": "msr1", "IN_CONFIRM": "REHEARSE-msr1", "ALLOWED_HOSTS": "msr1"}), true, "no console attested"},
		{"a rehearsal with the console confirmation for another host", base(env{"IN_MODE": "rehearse", "IN_HOST": "msr1", "IN_CONFIRM": "REHEARSE-msa2-server-CONSOLE", "ALLOWED_HOSTS": "msr1"}), false, "confirm must be exactly REHEARSE-msr1"},
		{"a console confirmation in lower case", base(env{"IN_MODE": "rehearse", "IN_HOST": "msr1", "IN_CONFIRM": "rehearse-msr1-console", "ALLOWED_HOSTS": "msr1"}), false, "confirm must be exactly REHEARSE-msr1"},
		{"an upgrade confirmed as a console rehearsal", up("msr1", env{"IN_CONFIRM": "REHEARSE-msr1-CONSOLE"}), false, "confirm must be exactly UPGRADE-msr1"},
		{"a rehearsal is gated too", base(env{"IN_MODE": "rehearse", "IN_HOST": "msr1", "IN_CONFIRM": "REHEARSE-msr1"}), false, "OS_UPGRADE_ALLOWED_HOSTS"},
		{"a rehearsal confirmed as an upgrade", base(env{"IN_MODE": "rehearse", "IN_HOST": "msr1", "IN_CONFIRM": "UPGRADE-msr1", "ALLOWED_HOSTS": "msr1"}), false, "confirm must be exactly REHEARSE-msr1"},
		{"a rehearsal confirmed in lower case", base(env{"IN_MODE": "rehearse", "IN_HOST": "msr1", "IN_CONFIRM": "rehearse-msr1", "ALLOWED_HOSTS": "msr1"}), false, "confirm must be exactly REHEARSE-msr1"},
		{"a rehearsal needs the environment too", base(env{"IN_MODE": "rehearse", "IN_HOST": "msr1", "IN_CONFIRM": "REHEARSE-msr1", "ALLOWED_HOSTS": "msr1", "GH_FIXTURE": filepath.Join(bin, "noreviewer.json")}), false, "no required reviewer"},
		{"an upgrade retrying a kernel", up("msr1", env{"IN_RETRY_KERNEL": "7.0.0-40-generic"}), true, ""},
		{"a retry kernel that is not a kernel", up("msr1", env{"IN_RETRY_KERNEL": "latest"}), false, "retry_kernel must be"},
		{"a retry kernel with a second line", up("msr1", env{"IN_RETRY_KERNEL": "7.0.0-40-generic\nrm -rf"}), false, "retry_kernel must be"},
		{"a retry kernel with a template", up("msr1", env{"IN_RETRY_KERNEL": "{{ x }}"}), false, "retry_kernel must be"},
		{"hold on", up("msr1", env{"IN_HOLD": "on"}), true, ""},
		{"hold off", up("msr1", env{"IN_HOLD": "off"}), true, ""},
		{"hold with an unknown value", up("msr1", env{"IN_HOLD": "maybe"}), false, "hold_access_packages must be"},
		{"a repair with its confirmation", rep(nil), true, ""},
		{"a repair is gated too", rep(env{"ALLOWED_HOSTS": ""}), false, "OS_UPGRADE_ALLOWED_HOSTS"},
		{"a repair of a host that is not listed", rep(env{"ALLOWED_HOSTS": "msa2-server"}), false, "is not in the repository variable"},
		{"a repair with the upgrade confirmation", rep(env{"IN_CONFIRM": "UPGRADE-msr1"}), false, "confirm must be exactly REPAIR-msr1"},
		{"a repair confirmed in lower case", rep(env{"IN_CONFIRM": "repair-msr1"}), false, "confirm must be exactly REPAIR-msr1"},
		{"a repair with an unknown action", rep(env{"IN_REPAIR_ACTION": "rm-rf"}), false, "repair_action must be"},
		{"an unknown mode", base(env{"IN_MODE": "dist-upgrade"}), false, "mode must be"},
		{"template syntax in the reason", base(env{"IN_REASON": "x {{ lookup('pipe','id') }}"}), false, "must not contain braces"},
		{"a brace in the reason", up("msr1", env{"IN_REASON": "x {% y"}), false, "must not contain braces"},
		{"an empty reason", base(env{"IN_REASON": "   "}), false, "reason is empty"},
		{"a long reason", base(env{"IN_REASON": strings.Repeat("a", 201)}), false, "longer than 200"},
		{"a reason with a newline", base(env{"IN_REASON": "a\nb"}), false, "control characters"},
		{"a branch ref", up("msr1", env{"REF": "refs/heads/feat/x"}), false, "dispatch from main only"},
		// the approval must exist: the environment, a required reviewer, a deployment branch policy
		{"the environment does not exist", up("msr1", envFix("notfound", "1")), false, "cannot read the environment cluster-host-change"},
		{"the token may not read environments", up("msr1", envFix("forbidden", "1")), false, "may not read environments"},
		{"an environment without a reviewer", up("msr1", envFix("noreviewer", "0")), false, "no required reviewer"},
		{"an environment with an empty reviewer list", up("msr1", envFix("emptyrevs", "0")), false, "no required reviewer"},
		{"an environment with only a wait timer", up("msr1", envFix("waitonly", "0")), false, "no required reviewer"},
		{"an environment without a branch policy", up("msr1", envFix("nobranches", "0")), false, "no deployment branch policy"},
		{"an answer that is not json", up("msr1", envFix("notjson", "0")), false, "no required reviewer"},
		{"a disarm needs the environment too", base(env{"IN_MODE": "disarm", "IN_HOST": "msr1", "IN_CONFIRM": "DISARM-msr1", "ALLOWED_HOSTS": "msr1", "GH_FIXTURE": filepath.Join(bin, "noreviewer.json")}), false, "no required reviewer"},
		{"a repair needs the environment too", rep(envFix("noreviewer", "0")), false, "no required reviewer"},
		// freeze and thaw: the same gate, the same approval, an exact confirmation each
		{"a freeze is gated too", fz("freeze", env{"ALLOWED_HOSTS": ""}), false, "OS_UPGRADE_ALLOWED_HOSTS"},
		{"a freeze of a host that is not listed", fz("freeze", env{"ALLOWED_HOSTS": "msa2-server"}), false, "is not in the repository variable"},
		{"a listed freeze", fz("freeze", nil), true, ""},
		{"a freeze needs the environment too", fz("freeze", envFix("noreviewer", "0")), false, "no required reviewer"},
		{"a freeze needs the branch policy too", fz("freeze", envFix("nobranches", "0")), false, "no deployment branch policy"},
		{"a freeze confirmed as an upgrade", fz("freeze", env{"IN_CONFIRM": "UPGRADE-msr1"}), false, "confirm must be exactly FREEZE-msr1"},
		{"a freeze confirmed as a thaw", fz("freeze", env{"IN_CONFIRM": "THAW-msr1"}), false, "confirm must be exactly FREEZE-msr1"},
		{"a freeze confirmed in lower case", fz("freeze", env{"IN_CONFIRM": "freeze-msr1"}), false, "confirm must be exactly FREEZE-msr1"},
		{"a freeze confirmed for another host", fz("freeze", env{"IN_CONFIRM": "FREEZE-msa2-server"}), false, "confirm must be exactly FREEZE-msr1"},
		{"a freeze of no host", fz("freeze", env{"IN_HOST": "-", "IN_CONFIRM": "FREEZE--"}), false, "names ONE host"},
		{"a freeze of all", fz("freeze", env{"IN_HOST": "all"}), false, "host must be"},
		{"a thaw is gated too", fz("thaw", env{"ALLOWED_HOSTS": ""}), false, "OS_UPGRADE_ALLOWED_HOSTS"},
		{"a listed thaw", fz("thaw", nil), true, ""},
		{"a thaw needs the environment too", fz("thaw", envFix("noreviewer", "0")), false, "no required reviewer"},
		{"a thaw confirmed as a freeze", fz("thaw", env{"IN_CONFIRM": "FREEZE-msr1"}), false, "confirm must be exactly THAW-msr1"},
		{"a thaw confirmed in lower case", fz("thaw", env{"IN_CONFIRM": "thaw-msr1"}), false, "confirm must be exactly THAW-msr1"},
		{"an upgrade confirmed as a freeze", up("msr1", env{"IN_CONFIRM": "FREEZE-msr1"}), false, "confirm must be exactly UPGRADE-msr1"},
		{"a freeze with template syntax in the reason", fz("freeze", env{"IN_REASON": "x {{ y }}"}), false, "must not contain braces"},
	} {
		cmd := exec.Command(bash, "-c", script)
		cmd.Env = []string{"PATH=" + bin + ":" + os.Getenv("PATH")}
		for k, v := range c.env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		out, err := cmd.CombinedOutput()
		if c.ok && err != nil {
			t.Errorf("%s: refused: %s", c.name, out)
		}
		if c.ok && c.msg != "" && !strings.Contains(string(out), c.msg) {
			t.Errorf("%s: accepted, but the log lacks %q: %s", c.name, c.msg, out)
		}
		if !c.ok {
			if err == nil {
				t.Errorf("%s: accepted", c.name)
			} else if !strings.Contains(string(out), c.msg) {
				t.Errorf("%s: refused for another reason than %q: %s", c.name, c.msg, out)
			}
			// R4-m3: a refusal never hands out the string that attests a console at the rack
			if consoleOverride.Match(out) {
				t.Errorf("%s: the refusal spells out the console override: %s", c.name, out)
			}
		}
	}
}

// R4-m3: the environment's approver sees the run name and the job name, not the inputs. A rehearsal says in both whether the dispatcher
// attests a person at the rack with a console.
func TestClusterOSUpgradeTheApproverSeesTheConsoleAttestation(t *testing.T) {
	src := readWorkflow(t, osWorkflow)
	rn := regexp.MustCompile(`(?m)^run-name: (.*)$`).FindStringSubmatch(src)
	jn := regexp.MustCompile(`(?m)^    name: (os-upgrade .*)$`).FindStringSubmatch(src)
	if rn == nil || jn == nil {
		t.Fatalf("run-name %v, os-upgrade job name %v", rn, jn)
	}
	for what, line := range map[string]string{"run-name": rn[1], "job name": jn[1]} {
		for _, want := range []string{"inputs.mode == 'rehearse'", "endsWith(inputs.confirm, '-CONSOLE')", "CONSOLE-ATTESTED", "(no console attested)"} {
			if !strings.Contains(line, want) {
				t.Errorf("the %s lacks %q: %s", what, want, line)
			}
		}
		if strings.Contains(line, "inputs.reason") {
			t.Errorf("the %s carries free text: %s", what, line)
		}
	}
	plan := jobSection(t, withoutComments(src), "plan")
	for _, want := range []string{`"$IN_CONFIRM" = "REHEARSE-$IN_HOST-CONSOLE"`, "CONSOLE ATTESTED", "no console attested", "GITHUB_STEP_SUMMARY"} {
		if !strings.Contains(plan, want) {
			t.Errorf("the plan job does not say whether a console is attested: lacks %q", want)
		}
	}
	if sum := mustRead(t, filepath.Join(".github", "scripts", "os-upgrade-summary.sh")); !strings.Contains(sum, "CONSOLE ATTESTED") || !strings.Contains(src, `"$RUN_ID" "$IN_CONFIRM"`) {
		t.Error("the step summary does not get the confirmation or does not say whether a console is attested")
	}
}

var consoleOverride = regexp.MustCompile(`REHEARSE-[^\s'"]*CONSOLE`)

// R4-m3: no refusal, error or input description spells out the confirmation that attests a console at the rack (an automated dispatcher that
// read it could re-send it). The README describes how to type it, the checks compare against it.
func TestClusterOSUpgradeNeverSpellsOutTheConsoleOverride(t *testing.T) {
	wf := readWorkflow(t, osWorkflow)
	for _, l := range strings.Split(wf, "\n") {
		if (strings.Contains(l, "description:") || strings.Contains(l, "::error::") || strings.Contains(l, "::notice") || strings.Contains(l, "echo ")) && consoleOverride.MatchString(l) {
			t.Errorf("the workflow spells out the override: %s", strings.TrimSpace(l))
		}
	}
	req := mustRead(t, osRequest)
	if i, j := strings.Index(req, "fail_msg:"), strings.Index(req, "quiet:"); i < 0 || j < i || consoleOverride.MatchString(req[i:j]) {
		t.Error("the request's failure message spells out the override")
	}
	for _, l := range strings.Split(mustRead(t, osJudge), "\n") {
		if i := strings.Index(l, "p.append("); i >= 0 && strings.Contains(l[i:], "CONSOLE") {
			t.Errorf("a refusal of the judgement spells out the override: %s", strings.TrimSpace(l[i:]))
		}
	}
	if consoleOverride.MatchString(stripComments(mustRead(t, osPlaybook))) {
		t.Error("the playbook's texts spell out the override")
	}
	if consoleOverride.MatchString(mustRead(t, filepath.Join(".github", "scripts", "os-upgrade-summary.sh"))) {
		t.Error("the summary spells out the override")
	}
	if !strings.Contains(mustRead(t, "README.md"), "that person's dispatch types `REHEARSE-<host>-CONSOLE` in `confirm`") {
		t.Error("the README no longer says how a person at the rack attests a console")
	}
}

// Review m1: the literal scan above is bypassed by a concatenation ('REHEARSE-' ~ os_host ~ '-CONSOLE'), a shell variable, or a print in the tool.
// The token "-CONSOLE" is therefore allowed only on the lines that COMPARE a confirmation against it (and on the approver's display), never in a
// message, in any of the files that print anything, and never in os_upgrade.py at all.
func TestClusterOSUpgradeTheConsoleTokenIsOnlyEverCompared(t *testing.T) {
	allowed := map[string][]*regexp.Regexp{
		filepath.Join(".github", "workflows", osWorkflow): {
			regexp.MustCompile(`^run-name: `), regexp.MustCompile(`^\s+name: os-upgrade `),
			regexp.MustCompile(`^\s+if \[ "\$IN_MODE" = rehearse \] && \[ "\$IN_CONFIRM" != "REHEARSE-\$IN_HOST" \] && \[ "\$IN_CONFIRM" != "REHEARSE-\$IN_HOST-CONSOLE" \]; then$`),
			regexp.MustCompile(`^\s+if \[ "\$IN_CONFIRM" = "REHEARSE-\$IN_HOST-CONSOLE" \]; then$`),
		},
		osRequest:  {regexp.MustCompile(`^      - \(os_mode == 'survey' and os_confirm == ''\) or `)},
		osJudge:    {regexp.MustCompile(`^  \{% set console_attested = \(os_confirm \| default\(''\)\) == \('REHEARSE-' ~ os_host ~ '-CONSOLE'\) %\}$`)},
		osPlaybook: {regexp.MustCompile(`^      confirm_kind: "\{\{ 'console-attested' if \(os_mode == 'rehearse' and \(os_confirm \| default\(''\)\) == \('REHEARSE-' ~ os_host ~ '-CONSOLE'\)\) else 'standard' \}\}"$`)},
		osScript:   {},
		filepath.Join(".github", "scripts", "os-upgrade-summary.sh"): {regexp.MustCompile(`^\t\*-CONSOLE\) echo "- confirmation: \*\*CONSOLE ATTESTED\*\*`)},
	}
	for file, ok := range allowed {
		src := mustRead(t, file)
		if file == osPlaybook || file == osScript {
			src = stripComments(src)
		}
		for i, l := range strings.Split(src, "\n") {
			if !strings.Contains(l, "-CONSOLE") {
				continue
			}
			legit := false
			for _, re := range ok {
				if re.MatchString(l) {
					legit = true
				}
			}
			if !legit {
				t.Errorf("%s:%d carries the console token outside a comparison (a message, a concatenation, a print): %s", file, i+1, strings.TrimSpace(l))
			}
		}
	}
	for _, f := range []string{filepath.Join("ansible", "tasks", "os_upgrade_phase.yml"), filepath.Join("ansible", "tasks", "os_upgrade_wait_apt.yml")} {
		if strings.Contains(mustRead(t, f), "CONSOLE") {
			t.Errorf("%s mentions the console token", f)
		}
	}
}

// The two scripts the playbook ships to a host are executable in the repository (reboot_snapshot.sh had lost its bit; ansible.builtin.script
// makes its copy executable, so only a person running it by hand noticed).
func TestOSUpgradeScriptsAreExecutable(t *testing.T) {
	for _, f := range []string{osScript, filepath.Join("ansible", "files", "reboot_snapshot.sh")} {
		info, err := os.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&0o111 == 0 {
			t.Errorf("%s is not executable (%v)", f, info.Mode())
		}
	}
}

// The summary's checklist and the script's list of protected packages are built from the judge's own numbers and text: a threshold
// or a package that moved in one place only would show PASS where the upgrade refuses.
func TestOSUpgradeConstantsAgree(t *testing.T) {
	judge := mustRead(t, osJudge)
	sum := mustRead(t, filepath.Join(".github", "scripts", "os-upgrade-summary.sh"))
	for _, c := range [][2]string{{"root_min", "os_min_root_free_kib"}, {"boot_min", "os_min_boot_free_kib"}, {"efi_min", "os_min_efi_free_kib"}} {
		a := regexp.MustCompile(`(?m)^` + c[0] + `=(\d+)$`).FindStringSubmatch(sum)
		b := regexp.MustCompile(`(?m)^` + c[1] + `: (\d+)$`).FindStringSubmatch(judge)
		if a == nil || b == nil || a[1] != b[1] {
			t.Errorf("%s in the summary (%v) and %s in the judge (%v) differ", c[0], a, c[1], b)
		}
	}
	// the package-manager programs that count as a package run: the tool's list, the judge's and the summary's are one list
	words := func(s string) string { f := strings.Fields(s); slices.Sort(f); return strings.Join(f, " ") }
	bs := regexp.MustCompile(`(?m)^PKG_RUN_NAMES = frozenset\("([^"]+)"\.split\(\)\)$`).FindStringSubmatch(mustRead(t, osScript))
	bj := regexp.MustCompile(`(?m)^os_busy_process_names: '([^']+)'$`).FindStringSubmatch(judge)
	bm := regexp.MustCompile(`(?m)^busy_names="([^"]+)"$`).FindStringSubmatch(sum)
	if bs == nil || bj == nil || bm == nil || words(bs[1]) != words(bj[1]) || words(bs[1]) != words(bm[1]) {
		t.Errorf("the busy program lists differ: script %v, judge %v, summary %v", bs, bj, bm)
	}
	for _, idle := range []string{"unattended-upgr", "packagekitd", "unattended-upgrade-shutdown"} {
		if strings.Contains(" "+words(bs[1])+" ", " "+idle+" ") {
			t.Errorf("%s is an idle daemon, not a package run: it must not be in the list", idle)
		}
	}
	rj := regexp.MustCompile(`(?m)^os_protected_regex: '(.*)'$`).FindStringSubmatch(judge)
	rs := regexp.MustCompile(`(?m)^PROTECTED_RE = re\.compile\(r"(.*)"\)$`).FindStringSubmatch(mustRead(t, osScript))
	if rj == nil || rs == nil || rj[1] != rs[1] {
		t.Errorf("os_protected_regex (%v) and PROTECTED_RE (%v) differ", rj, rs)
	}
}
