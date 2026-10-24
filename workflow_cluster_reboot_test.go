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
	"testing"
)

// The sanctioned host reboot (.github/workflows/cluster-reboot.yml, ansible/reboot.yml). A reboot is the one
// cluster action that cannot be undone from a keyboard: a host that does not come back needs a person at the
// rack, and a reboot in the middle of a run destroys it. These tests pin what makes it safe, as text and as
// behaviour, and each has a mutant that turns it red (see the pull request).

const (
	rebootWorkflow = ".github/workflows/cluster-reboot.yml"
	rebootPlaybook = "ansible/reboot.yml"
	rebootSnapshot = "ansible/files/reboot_snapshot.sh"
	rebootJudge    = "ansible/vars/reboot_judge.yml"
	rebootRequest  = "ansible/tasks/reboot_request.yml"
)

// section returns the job `name` of a workflow: from its two-space-indented key to the next one.
func jobSection(t *testing.T, src, name string) string {
	t.Helper()
	start := regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(name) + `:\n`).FindStringIndex(src)
	if start == nil {
		t.Fatalf("no job %q in the workflow", name)
	}
	rest := src[start[1]:]
	if next := regexp.MustCompile(`(?m)^  [A-Za-z0-9_-]+:\n`).FindStringIndex(rest); next != nil {
		rest = rest[:next[0]]
	}
	return rest
}

// The workflow runs only when a person dispatches it, and never from a schedule, a push, a pull request or a
// call from another workflow.
func TestClusterRebootOnlyRunsWhenDispatched(t *testing.T) {
	src := withoutComments(readWorkflow(t, "cluster-reboot.yml"))
	on := regexp.MustCompile(`(?ms)^on:\n(.*?)^\S`).FindStringSubmatch(src + "\nX")
	if on == nil {
		t.Fatal("no on: block")
	}
	var triggers []string
	for _, m := range regexp.MustCompile(`(?m)^  ([a-z_]+):`).FindAllStringSubmatch(on[1], -1) {
		triggers = append(triggers, m[1])
	}
	if !slices.Equal(triggers, []string{"workflow_dispatch"}) {
		t.Errorf("triggers = %v, want only workflow_dispatch: a reboot must never start by itself", triggers)
	}
	if regexp.MustCompile(`(?m)^\s*-?\s*cron:`).MatchString(src) || strings.Contains(src, "schedule:") {
		t.Error("the workflow has a schedule")
	}
}

// The inputs: msa2-client drives both targets and is not offered, the confirmation and the reason are required,
// and the playbook accepts exactly the hosts the workflow offers.
func TestClusterRebootInputs(t *testing.T) {
	src := withoutComments(readWorkflow(t, "cluster-reboot.yml"))
	host := regexp.MustCompile(`(?ms)^      host:\n(.*?)^      confirm:`).FindStringSubmatch(src)
	if host == nil {
		t.Fatal("no host input before confirm")
	}
	opts := regexp.MustCompile(`options: \[([^\]]*)\]`).FindStringSubmatch(host[1])
	if opts == nil || !strings.Contains(host[1], "type: choice") {
		t.Fatalf("the host input is not a choice: %s", host[1])
	}
	var offered []string
	for _, o := range strings.Split(opts[1], ",") {
		offered = append(offered, strings.TrimSpace(o))
	}
	if !slices.Equal(offered, []string{"msr1", "msa2-server"}) {
		t.Errorf("hosts offered = %v, want msr1 and msa2-server only (msa2-client drives both targets)", offered)
	}
	for _, in := range []string{"confirm", "reason"} {
		m := regexp.MustCompile(`(?ms)^      ` + in + `:\n(.*?)^      [a-z_]+:`).FindStringSubmatch(src)
		if m == nil || !strings.Contains(m[1], "required: true") {
			t.Errorf("input %s is not required", in)
		}
	}

	pb := mustRead(t, rebootRequest)
	allowed := regexp.MustCompile(`reboot_host in \[([^\]]*)\]`).FindStringSubmatch(pb)
	if allowed == nil {
		t.Fatal("the playbook does not check reboot_host against a list")
	}
	var accepted []string
	for _, o := range strings.Split(allowed[1], ",") {
		accepted = append(accepted, strings.Trim(strings.TrimSpace(o), "'"))
	}
	if !slices.Equal(accepted, offered) {
		t.Errorf("the playbook accepts %v, the workflow offers %v", accepted, offered)
	}
	if strings.Contains(withoutComments(pb+mustRead(t, rebootPlaybook)), "'msa2-client'") {
		t.Error("the playbook names msa2-client outside a comment")
	}
}

// The plan job refuses before any host is touched: a wrong confirmation, a ref other than the default branch.
func TestClusterRebootPlanRefuses(t *testing.T) {
	src := withoutComments(readWorkflow(t, "cluster-reboot.yml"))
	plan := jobSection(t, src, "plan")
	for _, want := range []string{
		`"$IN_CONFIRM" != "REBOOT-$IN_HOST"`,
		`"$REF" != "refs/heads/$DEFAULT_BRANCH"`,
		`msr1|msa2-server) ;;`,
		`-gt 200`,
	} {
		if !strings.Contains(plan, want) {
			t.Errorf("the plan job no longer checks %s", want)
		}
	}
	if !strings.Contains(mustRead(t, rebootRequest), "reboot_confirm == 'REBOOT-' ~ reboot_host") {
		t.Error("the playbook does not check the confirmation again (defence in depth)")
	}
	// the request is validated by the first two plays, not only the first: a failed play does not stop the next
	if n := strings.Count(stripComments(mustRead(t, rebootPlaybook)), "import_tasks: tasks/reboot_request.yml"); n != 2 {
		t.Errorf("%d plays validate the request, want 2 (localhost, then the preflight on every host)", n)
	}
}

// The serial group, and what keeps this run from cancelling a pending one: concurrency on the reboot JOB only
// (a workflow-level group would enter at dispatch, before the guard looked), never cancel-in-progress, behind a
// guard that runs outside the group in its idle mode, on a verdict of the same attempt, and no older than ten
// minutes.
func TestClusterRebootJoinsTheGroupWithoutCancellingAnything(t *testing.T) {
	src := withoutComments(readWorkflow(t, "cluster-reboot.yml"))
	if regexp.MustCompile(`(?m)^concurrency:`).MatchString(src) {
		t.Error("workflow-level concurrency: the run would enter matrix-tier-cluster at dispatch, before the guard looked")
	}
	if strings.Contains(src, "cancel-in-progress: true") {
		t.Error("cancel-in-progress: true")
	}
	if n := strings.Count(src, "group: matrix-tier-cluster"); n != 1 {
		t.Errorf("%d concurrency groups named matrix-tier-cluster, want exactly 1 (the reboot job)", n)
	}
	reboot := jobSection(t, src, "reboot")
	if !regexp.MustCompile(`(?m)^    concurrency:\n      group: matrix-tier-cluster\n      cancel-in-progress: false\n`).MatchString(reboot) {
		t.Error("the reboot job does not hold matrix-tier-cluster with cancel-in-progress: false")
	}
	for _, other := range []string{"plan", "cluster-guard", "verdict"} {
		if strings.Contains(jobSection(t, src, other), "concurrency:") {
			t.Errorf("job %s declares concurrency: only the reboot job may hold the group", other)
		}
	}
	if !regexp.MustCompile(`needs: \[plan, cluster-guard\]`).MatchString(reboot) {
		t.Error("the reboot job does not need the guard")
	}
	for _, want := range []string{
		"needs.cluster-guard.result == 'success'",
		"needs.cluster-guard.outputs.attempt == github.run_attempt",
		"github.ref == format('refs/heads/{0}', github.event.repository.default_branch)",
	} {
		if !strings.Contains(reboot, want) {
			t.Errorf("the reboot job's if: lacks %s", want)
		}
	}
	guard := jobSection(t, src, "cluster-guard")
	if !strings.Contains(guard, `guard -idle -dir "$GUARD_DIR" -self "$RUN_ID"`) {
		t.Error("the guard job does not run `stresstally guard -idle`: the ordinary verdict lets a reboot queue behind a holder")
	}
	if !strings.Contains(guard, "bash tools/stresstally/guard.sh") {
		t.Error("the guard job does not list the open runs first (guard.sh)")
	}
	if !strings.Contains(guard, "needs: plan") {
		t.Error("the guard job runs before the plan")
	}

	// the stale-verdict refusal, then the join, then the playbook
	iStale := mustIndex(t, reboot, "Refuse a stale idle verdict", "the stale-verdict step")
	iLimit := mustIndex(t, reboot, `"$age" -gt 600`, "the ten-minute limit")
	iJoin := mustIndex(t, reboot, "Join tailnet", "the tailnet join")
	iPlay := mustIndex(t, reboot, "ansible/reboot.yml", "the playbook run")
	iAudit := mustIndex(t, reboot, "eviction-audit.sh", "the eviction audit")
	if iStale >= iLimit || iLimit >= iJoin || iJoin >= iPlay || iAudit >= iJoin {
		t.Error("order must be: eviction audit, stale-verdict refusal, tailnet join, playbook")
	}
}

// The workflow is red unless a host was rebooted and verified: a skipped reboot job (a re-run on an old guard
// verdict) must not read as a reboot that happened.
func TestClusterRebootVerdictIsRedUnlessVerified(t *testing.T) {
	src := withoutComments(readWorkflow(t, "cluster-reboot.yml"))
	v := jobSection(t, src, "verdict")
	if !strings.Contains(v, `needs: [plan, cluster-guard, reboot]`) || !strings.Contains(v, `"$REBOOT" != success`) {
		t.Error("the verdict job does not fail unless the reboot job succeeded")
	}
	if !strings.Contains(v, "!cancelled()") {
		t.Error("the verdict job must run when the reboot job was skipped")
	}
}

// Inputs and secrets reach a script only through env:, every action is pinned by commit sha, the token reads,
// the cluster key is given to one step, and no job runs on the cluster's own runners (nothing on the host being
// rebooted may be needed to reboot it, and no runner is left to strand).
func TestClusterRebootWorkflowHygiene(t *testing.T) {
	src := withoutComments(readWorkflow(t, "cluster-reboot.yml"))
	lines := strings.Split(src, "\n")
	for i, l := range lines {
		if strings.Contains(l, "${{ inputs.") && !regexp.MustCompile(`^\s*(name|run-name|[A-Z][A-Z_]*):`).MatchString(l) {
			t.Errorf("line %d: an input outside env:, run-name or a name: %s", i+1, l)
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
		t.Error("a job names a cluster runner label: the reboot must not depend on, or leave, a runner on a cluster host")
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
	// the ansible the reboot runs is the one the cluster pins
	if !strings.Contains(src, "ansible_core_version") || !strings.Contains(src, "ansible/runner-setup.yml") {
		t.Error("the reboot job does not install the ansible-core pin of ansible/runner-setup.yml")
	}
	// the job outlives the reboot and the settling: timeout > 15 min + 3 min + setup
	reboot := jobSection(t, src, "reboot")
	m := regexp.MustCompile(`timeout-minutes: (\d+)`).FindStringSubmatch(reboot)
	pb := mustRead(t, rebootPlaybook)
	rt := regexp.MustCompile(`reboot_timeout_seconds: (\d+)`).FindStringSubmatch(pb)
	if m == nil || rt == nil {
		t.Fatal("no timeout-minutes on the reboot job or no reboot_timeout_seconds in the playbook")
	}
	job, _ := strconv.Atoi(m[1])
	boot, _ := strconv.Atoi(rt[1])
	if boot > 900 {
		t.Errorf("reboot_timeout_seconds = %d: the bound is 15 minutes", boot)
	}
	if job*60 < boot+36*5+300 {
		t.Errorf("the reboot job's %d-minute timeout does not cover the %d s reboot wait, 3 minutes of settling and the setup", job, boot)
	}
}

// The playbook: refuses before touching, stops everything when one host is unreachable, reboots exactly once
// with a message that carries no free text, and does nothing to a host but read it and reboot it.
func TestClusterRebootPlaybookSafety(t *testing.T) {
	raw := mustRead(t, rebootPlaybook)
	src := stripComments(raw)
	if n := strings.Count(src, "ansible.builtin.reboot:"); n != 1 {
		t.Errorf("%d reboot tasks, want exactly 1", n)
	}
	if n := strings.Count(src, "any_errors_fatal: true"); n < 3 {
		t.Errorf("%d plays with any_errors_fatal, want the request check, the preflight and the target: a peer that does not answer must stop the reboot", n)
	}
	if !strings.Contains(src, "hostvars[reboot_host].reboot_verified | default(false)") || !strings.Contains(src, "reboot_verified: true") {
		t.Error("no last play proves the target play ran to its end: a host pattern that matched nothing would read as success")
	}
	task := regexp.MustCompile(`(?s)ansible\.builtin\.reboot:\n(.*?)\n    - name:`).FindStringSubmatch(src)
	if task == nil {
		t.Fatal("cannot find the reboot task")
	}
	if strings.Contains(task[1], "reboot_reason") || !strings.Contains(task[1], `msg: "cluster-reboot workflow run {{ reboot_run_id }}"`) {
		t.Errorf("the reboot message must be fixed text and the validated run id: the module puts it inside double quotes in a root shell command, so free text there is a shell injection:\n%s", task[1])
	}
	if !strings.Contains(task[1], `reboot_timeout: "{{ reboot_timeout_seconds | int }}"`) {
		t.Error("the reboot wait is not bounded by reboot_timeout_seconds")
	}
	for _, banned := range []string{"ansible.builtin.shell:", "ansible.builtin.command:", "ansible.builtin.raw:", "ansible.builtin.service:", "ansible.builtin.systemd:", "ansible.builtin.apt:", "ansible.builtin.lineinfile:", "state: absent", "state: restarted", "state: stopped", "docker"} {
		if strings.Contains(src, banned) {
			t.Errorf("the playbook uses %s: it may only read (the snapshot script) and reboot", banned)
		}
	}
	if !strings.Contains(src, `hosts: "{{ reboot_host | default('none') }}"`) {
		t.Error("the target play does not run on reboot_host")
	}
	// the refusals and the verification gate the reboot / the success
	if !strings.Contains(src, "when: refusals_str | trim | length > 0") || !strings.Contains(src, "when: post_problems_str | trim | length > 0") {
		t.Error("the playbook does not act on the judgement (refusals_str before, post_problems_str after)")
	}
	iRefuse := mustIndex(t, src, "Target | refuse", "the refusal task")
	iReboot := mustIndex(t, src, "ansible.builtin.reboot:", "the reboot task")
	iJudge := mustIndex(t, src, "Post | judge", "the verification task")
	if iRefuse >= iReboot || iReboot >= iJudge {
		t.Error("order must be: refuse, reboot, judge")
	}
	if !strings.Contains(src, "ControlMaster=no") || !strings.Contains(src, "ControlPath=none") {
		t.Error("a multiplexed ssh socket may outlive the reboot: switch ControlMaster off for the play")
	}
}

// The judgement must keep every verification the maintainer asked for. These are the sentences the step summary
// and the failure print; the Jinja is exercised for real by test_reboot_judge.sh.
func TestClusterRebootJudgementCoversTheRequirements(t *testing.T) {
	src := mustRead(t, rebootJudge)
	for _, want := range []string{
		"boot_id did not change",
		"kernel changed",
		"new crash dump(s)",
		"prerequisite regressed",
		"is not owned by",
		"is not in /proc/interrupts",
		"is not configured on the host",
		"over the fabric",
		"bond lost a slave",
		"the counters were not reset",
		"no readable snapshot",
		"results_root",
		"resume-pending.json",
		"runner job(s) are executing",
		"no shutdown quiesce hook",
		"tailscaled is running but systemctl is-enabled says",
		"r8127 module is not available",
		"the reboot would boot another kernel",
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

// snapshotRecord is what the playbook and its judgement read of reboot_snapshot.sh. Decoding the script's output
// strictly into it fails on a field the script adds or loses without this side knowing.
type snapshotRecord struct {
	BootID                string `json:"boot_id"`
	BootTime              string `json:"boot_time"`
	Btime                 int64  `json:"btime"`
	UptimeS               int64  `json:"uptime_s"`
	Kernel                string `json:"kernel"`
	NewestInstalledKernel string `json:"newest_installed_kernel"`
	NICs                  []struct {
		Iface  string `json:"iface"`
		Driver string `json:"driver"`
	} `json:"nics"`
	NICVectors []struct {
		Iface     string `json:"iface"`
		IRQ       int    `json:"irq"`
		Name      string `json:"name"`
		Total     int64  `json:"total"`
		Affinity  string `json:"affinity"`
		Effective string `json:"effective"`
	} `json:"nic_vectors"`
	MaxVectorTotal         int64  `json:"max_vector_total"`
	PlacementVector        string `json:"placement_vector"`
	PlacementVectorPresent *bool  `json:"placement_vector_present"`
	Bonds                  []struct {
		Bond   string   `json:"bond"`
		Mode   string   `json:"mode"`
		Slaves []string `json:"slaves"`
	} `json:"bonds"`
	LanIP           string   `json:"lan_ip"`
	LanIPPresent    *bool    `json:"lan_ip_present"`
	PeerIP          string   `json:"peer_ip"`
	PeerPingOK      *bool    `json:"peer_ping_ok"`
	PeerProbe       string   `json:"peer_probe"`
	ManifestPresent bool     `json:"manifest_present"`
	ResultsEntries  int      `json:"results_entries"`
	ResultsList     []string `json:"results_list"`
	ResumePending   bool     `json:"resume_pending"`
	RunnerDirs      []string `json:"runner_dirs"`
	RunnerListeners int      `json:"runner_listeners"`
	RunnerWorkers   int      `json:"runner_workers"`
	Crash           []string `json:"crash"`
	R8127           bool     `json:"r8127"`
	ModinfoR8127    *bool    `json:"modinfo_r8127"`
	GrubPinned      bool     `json:"grub_pinned_to_running"`
	Prereq          struct {
		TailscaledActive    bool   `json:"tailscaled_active"`
		TailscaledEnabled   string `json:"tailscaled_enabled"`
		ApcupsdActive       bool   `json:"apcupsd_active"`
		ResumeCheckEnabled  bool   `json:"resume_check_enabled"`
		PowerEventInstalled bool   `json:"power_event_installed"`
		TmpfilesConf        bool   `json:"tmpfiles_conf"`
		QuiesceHook         bool   `json:"quiesce_hook"`
		RunCelerisOwner     string `json:"run_celeris_owner"`
	} `json:"prereq"`
	SystemState string   `json:"system_state"`
	FailedUnits []string `json:"failed_units"`
}

func linuxBash(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the snapshot script reads /proc and uses GNU stat; CI runs this on ubuntu")
	}
	return needBash(t)
}

// TestRebootSnapshotScript runs ansible/tests/test_reboot_snapshot.sh: vectors found by name and msi_irqs and
// summed over the CPUs, another device's vector not reported, the placement vector by exact name, the run state
// reported, and the tree identical after the run (it writes nothing).
func TestRebootSnapshotScript(t *testing.T) {
	bash := linuxBash(t)
	out, err := exec.Command(bash, filepath.Join("ansible", "tests", "test_reboot_snapshot.sh")).CombinedOutput()
	if err != nil {
		t.Fatalf("test_reboot_snapshot.sh failed: %v\n%s", err, out)
	}
	if !bytes.Contains(out, []byte(" 0 failed")) {
		t.Fatalf("test_reboot_snapshot.sh reported failures:\n%s", out)
	}
}

// TestRebootSnapshotRecordMatchesWhatIsRead decodes the script's JSON strictly, and ties the judgement's field
// reads to it: every a.<field> / b.<field> in ansible/vars/reboot_judge.yml must be a field of the record.
func TestRebootSnapshotRecordMatchesWhatIsRead(t *testing.T) {
	bash := linuxBash(t)
	root := t.TempDir()
	mk := func(p, body string) {
		t.Helper()
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("sys/class/net/enp1s0/device/msi_irqs/167", "")
	mk("proc/interrupts", "  CPU0 CPU1\n 167: 5 7 ITS-MSI 2 Edge enp1s0-8\n")
	mk("proc/irq/167/smp_affinity_list", "1\n")
	mk("proc/irq/167/effective_affinity_list", "1\n")
	mk("proc/sys/kernel/random/boot_id", "abc\n")
	mk("proc/sys/kernel/osrelease", "7.0.0-1\n")
	mk("proc/uptime", "10.5 1\n")
	mk("proc/stat", "btime 1700000000\n")
	cmd := exec.Command(bash, filepath.Join("ansible", "files", "reboot_snapshot.sh"), "/tmp/m", "/tmp/r", "enp1s0-8", "-", "-")
	cmd.Env = append(os.Environ(), "HOST_SNAPSHOT_ROOT="+root)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		t.Fatalf("snapshot failed: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	dec.DisallowUnknownFields()
	var rec snapshotRecord
	if err := dec.Decode(&rec); err != nil {
		t.Fatalf("the script's record does not decode into the fields the playbook reads: %v\n%s", err, stdout.String())
	}
	if rec.BootID != "abc" || rec.Kernel != "7.0.0-1" || len(rec.NICVectors) != 1 || rec.NICVectors[0].Total != 12 ||
		rec.PlacementVectorPresent == nil || !*rec.PlacementVectorPresent {
		t.Errorf("record = %+v", rec)
	}

	// every field the judgement reads is a field of the record
	fields := map[string]bool{}
	var walk func(m map[string]any)
	walk = func(m map[string]any) {
		for k, v := range m {
			fields[k] = true
			if sub, ok := v.(map[string]any); ok {
				walk(sub)
			}
		}
	}
	var generic map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &generic); err != nil {
		t.Fatal(err)
	}
	walk(generic)
	judge := stripComments(mustRead(t, rebootJudge))
	reads := regexp.MustCompile(`\b[ab]\.([a-z_0-9]+)`).FindAllStringSubmatch(judge, -1)
	if len(reads) < 20 {
		t.Fatalf("only %d field reads found in the judgement: the pattern no longer sees it", len(reads))
	}
	for _, m := range reads {
		if !fields[m[1]] {
			t.Errorf("the judgement reads %q, which the snapshot script does not write", m[1])
		}
	}
}

// The snapshot script is a pure read: no redirection to anything but /dev/null, and none of the verbs the
// monitoring scripts are also forbidden (workflow_forensics_test.go).
func TestRebootSnapshotCannotDisturbAHost(t *testing.T) {
	src := stripComments(mustRead(t, rebootSnapshot))
	if m := commandPosition.FindString(src); m != "" {
		t.Errorf("the snapshot invokes %q as a command", strings.TrimSpace(m))
	}
	if m := destructiveSystemctl.FindString(src); m != "" {
		t.Errorf("the snapshot changes unit state: %q", m)
	}
	for _, re := range otherDestructive {
		if m := re.FindString(src); m != "" {
			t.Errorf("the snapshot does something destructive: %q", m)
		}
	}
	// the one other redirection: the TCP connect to the peer's sshd on a host without ping (bash /dev/tcp opens a socket, it writes nothing);
	// the target is exactly the host and port the script passes as arguments, nothing else is allowed
	tcpProbe := `"/dev/tcp/$0/$1"'`
	for _, m := range regexp.MustCompile(`\d?>>?\s*(\S+)`).FindAllStringSubmatch(src, -1) {
		target := strings.TrimRight(m[1], `);"`)
		if target != "/dev/null" && !strings.HasPrefix(target, "&") && m[1] != tcpProbe {
			t.Errorf("the snapshot redirects output to %q", m[1])
		}
	}
	if m := regexp.MustCompile(`(^|[^A-Za-z0-9_./-])(mkdir|touch|tee|mv|cp|rm|ln|dd|chmod|chown|sysctl|modprobe|ethtool|sed -i)([^A-Za-z0-9_-]|$)`).FindString(src); m != "" {
		t.Errorf("the snapshot uses %q: it may only read", strings.TrimSpace(m))
	}
}

// TestRebootJudgePlay runs the real judgement under ansible (test_reboot_judge.sh). Opt-in like the other play
// tests: the ansible CI job installs the cluster's pin and runs the script directly.
func TestRebootJudgePlay(t *testing.T) {
	bash := linuxBash(t)
	if os.Getenv("PROBATORIUM_ANSIBLE_TESTS") != "1" {
		t.Skip("set PROBATORIUM_ANSIBLE_TESTS=1 to run the judgement under ansible (needs ansible-playbook); the ansible CI job runs it")
	}
	if _, err := exec.LookPath("ansible-playbook"); err != nil {
		t.Fatal("PROBATORIUM_ANSIBLE_TESTS=1 but ansible-playbook is not installed")
	}
	out, err := exec.Command(bash, filepath.Join("ansible", "tests", "test_reboot_judge.sh")).CombinedOutput()
	if err != nil {
		t.Fatalf("test_reboot_judge.sh failed: %v\n%s", err, out)
	}
}

// TestRebootPlay runs the real playbook end to end on local connections (test_reboot_play.sh), with only the
// restart and the host's snapshot replaced: what refuses before the reboot, that one failing peer stops the
// reboot of the target, that a host which came back wrong is not verified, the evidence left behind.
func TestRebootPlay(t *testing.T) {
	bash := linuxBash(t)
	if os.Getenv("PROBATORIUM_ANSIBLE_TESTS") != "1" {
		t.Skip("set PROBATORIUM_ANSIBLE_TESTS=1 to run the playbook (needs ansible-playbook); the ansible CI job runs it")
	}
	if _, err := exec.LookPath("ansible-playbook"); err != nil {
		t.Fatal("PROBATORIUM_ANSIBLE_TESTS=1 but ansible-playbook is not installed")
	}
	out, err := exec.Command(bash, filepath.Join("ansible", "tests", "test_reboot_play.sh")).CombinedOutput()
	if err != nil {
		t.Fatalf("test_reboot_play.sh failed: %v\n%s", err, out)
	}
}

// The ansible CI job runs the judgement and the playbook tests, so the Jinja and the control flow are exercised
// under the cluster's ansible pin.
func TestAnsibleJobRunsTheRebootTests(t *testing.T) {
	wf := mustRead(t, filepath.Join(".github", "workflows", "test.yml"))
	for _, s := range []string{"test_reboot_judge", "test_reboot_play"} {
		if !regexp.MustCompile(`(?m)^\s*run: bash ansible/tests/` + s + `\.sh\s*$`).MatchString(wf) {
			t.Errorf("test.yml's ansible job does not run `bash ansible/tests/%s.sh`", s)
		}
	}
}

// The step summary: a verified reboot, one that never came back (names the physical power-cycle), and one the
// play never reached. Needs jq, which the CI runners and the hosted runner of the workflow have.
func TestRebootSummary(t *testing.T) {
	bash := needBash(t)
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not installed")
	}
	script := filepath.Join(".github", "scripts", "reboot-summary.sh")
	out := t.TempDir()
	host := filepath.Join(out, "msr1")
	if err := os.MkdirAll(host, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(host, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	snap := func(boot string, total int64) string {
		return `{"boot_id":"` + boot + `","boot_time":"2026-10-21 10:00:00","btime":1,"uptime_s":5,"kernel":"7.0.0-14-generic","newest_installed_kernel":"7.0.0-14-generic","nics":[],` +
			`"nic_vectors":[{"iface":"enp49s0","irq":135,"name":"enp49s0-8","total":` + strconv.FormatInt(total, 10) + `,"affinity":"0-11","effective":"8"}],"max_vector_total":` + strconv.FormatInt(total, 10) + `,` +
			`"placement_vector":"enp1s0-8","placement_vector_present":true,"bonds":[{"bond":"bond0","mode":"802.3ad","slaves":["enp1s0=up","enp49s0=up"]}],"lan_ip":"x","lan_ip_present":true,"peer_ip":"y","peer_ping_ok":true,` +
			`"manifest_present":false,"results_entries":0,"results_list":[],"resume_pending":false,"runner_dirs":[],"runner_listeners":0,"runner_workers":0,"crash":[],"r8127":true,"modinfo_r8127":true,` +
			`"prereq":{"tailscaled_active":true,"tailscaled_enabled":"enabled","apcupsd_active":true,"resume_check_enabled":true,"power_event_installed":true,"tmpfiles_conf":true,"quiesce_hook":true,"run_celeris_owner":"mini"},"system_state":"running","failed_units":[]}`
	}
	run := func(rc, reason string) string {
		t.Helper()
		b, err := exec.Command(bash, script, out, "msr1", rc, reason, "123").CombinedOutput()
		if err != nil {
			t.Fatalf("summary: %v\n%s", err, b)
		}
		return string(b)
	}

	write("phase.txt", "verified\n")
	write("pre-snapshot.json", snap("aaa", 4050000000))
	write("post-snapshot.json", snap("bbb", 1200))
	write("result.json", `{"host":"msr1","elapsed_seconds":142,"problems":[]}`)
	got := run("0", "rule D `counters`")
	for _, want := range []string{"**result: VERIFIED**", "| boot_id | `aaa` | `bbb` |", "| after | enp49s0 | 135 | enp49s0-8 | 1200 | 0-11 | 8 |",
		"| before | enp49s0 | 135 | enp49s0-8 | 4050000000 | 0-11 | 8 |", "bench.yml", "Nothing is restored by hand", "COLD", "rule D 'counters'"} {
		if !strings.Contains(got, want) {
			t.Errorf("verified summary lacks %q:\n%s", want, got)
		}
	}
	// a snapshot from before the probe was recorded has no peer_probe: the row reads n/a, not null; a recorded probe prints as is, and a
	// false peer_ping_ok is not turned into n/a
	if !strings.Contains(got, "| peer_probe | n/a | n/a |") || strings.Contains(got, "null") {
		t.Errorf("an old snapshot without peer_probe must print n/a, never null:\n%s", got)
	}
	write("pre-snapshot.json", strings.Replace(snap("aaa", 4050000000), `"peer_ping_ok":true,`, `"peer_ping_ok":false,"peer_probe":"tcp22",`, 1))
	got = run("0", "x")
	for _, want := range []string{"| peer_probe | tcp22 | n/a |", "| peer_ping_ok | false | true |"} {
		if !strings.Contains(got, want) {
			t.Errorf("a recorded probe is not printed as recorded: lacks %q:\n%s", want, got)
		}
	}
	write("pre-snapshot.json", snap("aaa", 4050000000))

	write("phase.txt", "rebooting\n")
	if err := os.Remove(filepath.Join(host, "post-snapshot.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(host, "result.json")); err != nil {
		t.Fatal(err)
	}
	got = run("2", "x")
	for _, want := range []string{"**result: NOT VERIFIED**", "PHYSICAL power-cycle", "(no snapshot)"} {
		if !strings.Contains(got, want) {
			t.Errorf("a host that did not return: summary lacks %q:\n%s", want, got)
		}
	}

	// a reboot with rc 0 but a phase short of verified is not verified either
	write("phase.txt", "returned\n")
	if got = run("0", "x"); !strings.Contains(got, "NOT VERIFIED") {
		t.Errorf("rc 0 with phase returned must not read as verified:\n%s", got)
	}

	// a refusal's reasons are printed
	write("refusal.txt", "results_root /tmp/celeris-results holds 3 entr(ies)\n")
	if got = run("2", "x"); !strings.Contains(got, "### Refused: msr1") || !strings.Contains(got, "results_root /tmp/celeris-results holds 3 entr(ies)") {
		t.Errorf("a refusal's reasons are not in the summary:\n%s", got)
	}

	// the play never started
	if err := os.RemoveAll(host); err != nil {
		t.Fatal(err)
	}
	if got = run("4", "x"); !strings.Contains(got, "nothing was rebooted") {
		t.Errorf("a refused run does not say nothing was rebooted:\n%s", got)
	}
}

// Rebooting a cluster host is documented where an operator looks: the exact dispatch, the refusals, the
// verification, what the next run re-applies, and the risks.
func TestRebootIsDocumented(t *testing.T) {
	readme := mustRead(t, "README.md")
	i := strings.Index(readme, "### Rebooting a cluster host")
	if i < 0 {
		t.Fatal("README.md has no 'Rebooting a cluster host' section")
	}
	sec := readme[i:]
	if j := strings.Index(sec[4:], "\n## "); j >= 0 {
		sec = sec[:j+4]
	}
	for _, want := range []string{
		"gh workflow run cluster-reboot.yml",
		"confirm=REBOOT-",
		"msa2-client",
		"idle",
		"matrix-tier-cluster",
		"boot id",
		"nic_irq_placement",
		"cold",
		"physical power-cycle",
		"HOLD-CLUSTER",
		"ansible/reboot.yml",
		"allow_results_loss",
	} {
		if !strings.Contains(sec, want) {
			t.Errorf("the README section lacks %q", want)
		}
	}
	rb := mustRead(t, filepath.Join("ansible", "RUNNER_BOOTSTRAP.md"))
	if !strings.Contains(rb, "cluster-reboot.yml") {
		t.Error("RUNNER_BOOTSTRAP.md, which says a reboot clears the tool cache, does not point at the sanctioned reboot path")
	}
}
