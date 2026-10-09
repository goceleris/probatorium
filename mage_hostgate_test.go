//go:build mage

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// HostGate is driven here with FAKE host output: HOSTGATE_INPUT_DIR names a
// directory holding <host>.txt, exactly what ansible/tasks/host_gate.yml
// writes, and the ansible run is skipped. The parser and the thresholds are
// tested in hostgate/; this pins the wiring around them.

const (
	fakeClean  = "hostgate\tv1\nhost\tmsa2-client\nepoch\t1791547200\npsi\tio\tsome avg10=0.00 avg60=0.00 avg300=0.00 total=0\nend\tok\n"
	fakeOrphan = "hostgate\tv1\nhost\tmsa2-server\nepoch\t1791547200\npsi\tio\tsome avg10=0.00 avg60=0.00 avg300=0.00 total=0\n" +
		"ss\tLISTEN 0      65535                            *:8080             *:*    users:((\"gin\",pid=3941235,fd=4))\n" +
		"proc\t3941235\t3941234\tmini\t302219\tgin\t/tmp/celeris-bench/competitors/gin (deleted)\t/tmp/celeris-bench (deleted)\t./competitors/gin -bind 0.0.0.0:8080\nend\tok\n"
)

func writeFake(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name+".txt"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func gateEnv(t *testing.T, dir, target string) string {
	t.Helper()
	summary := filepath.Join(t.TempDir(), "summary.md")
	t.Setenv("HOSTGATE_INPUT_DIR", dir)
	t.Setenv("HOSTGATE_TARGET", target)
	t.Setenv("BENCH_TARGET", "")
	t.Setenv("VALIDATE_TARGET", "")
	t.Setenv("HOSTGATE_HOSTS", "")
	t.Setenv("GITHUB_STEP_SUMMARY", summary)
	for _, k := range []string{"HOSTGATE_MAX_IO_PSI", "HOSTGATE_MAX_AGE_HOURS", "HOSTGATE_PORTS", "HOSTGATE_REQUIRE_PSI"} {
		t.Setenv(k, "")
	}
	return summary
}

func TestHostGatePassesACleanCluster(t *testing.T) {
	clean := func(h string) string { return strings.Replace(fakeClean, "msa2-client", h, 1) }
	dir := writeFake(t, map[string]string{"msa2-server": clean("msa2-server"), "msr1": clean("msr1"), "msa2-client": clean("msa2-client")})
	gateEnv(t, dir, "both")
	if err := HostGate(); err != nil {
		t.Fatalf("clean cluster failed the gate: %v", err)
	}
}

func TestHostGateFailsAndNamesTheOrphan(t *testing.T) {
	dir := writeFake(t, map[string]string{"msa2-server": fakeOrphan, "msa2-client": fakeClean})
	summary := gateEnv(t, dir, "msa2-server") // msa2-server + the loadgen
	err := HostGate()
	if err == nil {
		t.Fatal("a host with an orphan SUT on :8080 passed the gate")
	}
	if !strings.Contains(err.Error(), "msa2-server") || strings.Contains(err.Error(), "msa2-client") {
		t.Errorf("the error must name exactly the failing host: %v", err)
	}
	b, rerr := os.ReadFile(summary)
	if rerr != nil || !strings.Contains(string(b), "3941235") || !strings.Contains(string(b), "gin") {
		t.Errorf("the step summary must carry the process list: %q (%v)", b, rerr)
	}
}

// The target decides which hosts are gated: an amd64-only bench must not be
// held up by an unrelated arm64 host (the 2026-08-28 lesson in
// clusterLimitForTarget), and a host with no output is a failure, not a pass.
func TestHostGateScopesToTheTargetAndFailsClosedOnMissingOutput(t *testing.T) {
	dir := writeFake(t, map[string]string{"msa2-server": strings.Replace(fakeClean, "msa2-client", "msa2-server", 1), "msa2-client": fakeClean})
	gateEnv(t, dir, "msa2-server")
	if err := HostGate(); err != nil {
		t.Fatalf("msr1 has no output but is not in scope: %v", err)
	}
	gateEnv(t, dir, "both")
	err := HostGate()
	if err == nil || !strings.Contains(err.Error(), "msr1") {
		t.Fatalf("msr1 is in scope with no output and must fail the gate: %v", err)
	}
}

func TestHostGateRejectsAMalformedPolicyAndAnUnknownTarget(t *testing.T) {
	dir := writeFake(t, map[string]string{"msa2-client": fakeClean})
	gateEnv(t, dir, "both")
	t.Setenv("HOSTGATE_MAX_IO_PSI", "lots")
	if err := HostGate(); err == nil || !strings.Contains(err.Error(), "HOSTGATE_MAX_IO_PSI") {
		t.Errorf("malformed threshold: %v", err)
	}
	t.Setenv("HOSTGATE_MAX_IO_PSI", "")
	t.Setenv("HOSTGATE_TARGET", "msr9")
	if err := HostGate(); err == nil || !strings.Contains(err.Error(), "msr9") {
		t.Errorf("unknown target: %v", err)
	}
}

func TestGateHostsFollowTheTarget(t *testing.T) {
	for target, want := range map[string]string{
		"both":        "msa2-client,msa2-server,msr1",
		"all":         "msa2-client,msa2-server,msr1",
		"":            "msa2-client,msa2-server,msr1",
		"msr1":        "msa2-client,msr1",
		"msa2-server": "msa2-client,msa2-server",
	} {
		got, err := gateHosts(target, "")
		if err != nil || strings.Join(got, ",") != want {
			t.Errorf("gateHosts(%q) = %v, %v; want %s", target, got, err, want)
		}
	}
	got, err := gateHosts("both", "msr1, msa2-client")
	if err != nil || strings.Join(got, ",") != "msr1,msa2-client" {
		t.Errorf("HOSTGATE_HOSTS override = %v, %v", got, err)
	}
}
