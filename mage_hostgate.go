//go:build mage

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/goceleris/probatorium/hostgate"
)

// hostGatePlaybook is ansible/host-gate.yml: it runs hostgate/collect.sh on
// each host (read-only, as root) and writes <hostgate_local_dir>/<host>.txt.
const hostGatePlaybook = "host-gate.yml"

// HostGate is the PRE-TIER HOST GATE (probatorium#473, ask 3): before a tier
// starts, assert that every host it will use is clean, and fail fast when it
// is not.
//
// For each host it checks, read-only:
//
//   - no listener on a port the harness is about to bind (the SUT port 8080,
//     its debug sidecar 18089, the postgres/redis/memcached fixture ports);
//   - no process from a previous run: a harness binary name (cleanup.yml's
//     list), a process living in a /tmp/celeris-* directory (deleted or not),
//     or a binary deleted from /tmp, older than HOSTGATE_MAX_AGE_HOURS;
//   - idle IO pressure (/proc/pressure/io some avg60) below HOSTGATE_MAX_IO_PSI.
//
// A host that cannot be read fails the gate too. On violation it prints a
// GitHub ::error:: annotation carrying the process list and returns an error.
// IT NEVER KILLS ANYTHING: a process it names may be somebody's, and the owner
// is found first (the msr1 rvtest was an agent's smoke test).
//
// Run it BEFORE `mage Deploy` (and before HostSamplerStart), so the tier's own
// fixtures and sampler are not mistaken for leftovers.
//
// Env knobs:
//
//	HOSTGATE_TARGET=both       msa2-server | msr1 | both; falls back to
//	                           BENCH_TARGET then VALIDATE_TARGET. Gates the
//	                           target host(s) plus the loadgen, as clusterLimitForTarget.
//	HOSTGATE_HOSTS=            csv override of the host list
//	HOSTGATE_MAX_IO_PSI=25     percent; 0 disables
//	HOSTGATE_MAX_AGE_HOURS=1
//	HOSTGATE_PORTS=            csv override of the guarded ports
//	HOSTGATE_REQUIRE_PSI=      1: a kernel without PSI fails the gate
//	HOSTGATE_INPUT_DIR=        read <host>.txt files from here instead of running ansible
//	                           (tests feed fake host output this way)
func HostGate() error {
	pol, err := hostgate.PolicyFromEnv(os.Getenv)
	if err != nil {
		return err
	}
	target := firstNonEmpty(os.Getenv("HOSTGATE_TARGET"), os.Getenv("BENCH_TARGET"), os.Getenv("VALIDATE_TARGET"), defaultClusterTarget)
	hosts, err := gateHosts(target, os.Getenv("HOSTGATE_HOSTS"))
	if err != nil {
		return err
	}

	dir := os.Getenv("HOSTGATE_INPUT_DIR")
	if dir == "" {
		dir, err = os.MkdirTemp("", "hostgate-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		if err := collectHostGate(dir, hosts); err != nil {
			// Not fatal by itself: hosts with no output below fail as
			// no-data, which names them. Say what ansible said.
			fmt.Printf("host gate: ansible reported: %v\n", err)
		}
	}
	read := func(host string) ([]byte, error) {
		b, err := os.ReadFile(filepath.Join(dir, host+".txt"))
		if err != nil {
			return nil, fmt.Errorf("no gate output from %s (unreachable, or ansible failed before it ran): %w", host, err)
		}
		return b, nil
	}

	res := hostgate.CheckAll(hosts, read, pol)
	for _, h := range hosts {
		s, ok := res.Snapshots[h]
		if !ok {
			continue
		}
		io := "n/a"
		if s.IO.Present {
			io = fmt.Sprintf("%.2f", s.IO.SomeAvg60)
		}
		fmt.Printf("host gate: %s: %d process(es) older than the probe window, %d listening socket(s), io some avg60=%s\n",
			h, len(s.Procs), len(s.Listeners), io)
	}
	if res.OK() {
		fmt.Printf("host gate: %s clean (ports %v, processes under %s old, io pressure below %.0f%%)\n",
			strings.Join(hosts, ", "), pol.GuardedPorts, pol.MaxAge, pol.MaxIOSomeAvg60)
		return nil
	}

	fmt.Println(hostgate.WorkflowCommand(res))
	writeHostGateSummary(res)
	for _, v := range res.Violations {
		fmt.Printf("host gate: %s [%s] %s\n", v.Host, v.Rule, v.Summary)
		for _, l := range v.Lines {
			fmt.Printf("host gate:     %s\n", l)
		}
	}
	return fmt.Errorf("host gate failed on %s: the cluster is not clean enough to start a tier (nothing was stopped; see the annotation for the process list)",
		strings.Join(res.Hosts(), ", "))
}

// gateHosts resolves the hosts to gate: an explicit csv, else the target's
// hosts plus the loadgen ("both"/"all"/"" = every cluster host).
func gateHosts(target, override string) ([]string, error) {
	if strings.TrimSpace(override) != "" {
		var out []string
		for _, h := range strings.Split(override, ",") {
			if h = strings.TrimSpace(h); h != "" {
				out = append(out, h)
			}
		}
		return out, nil
	}
	switch target {
	case "", "both", "all":
		var out []string
		for h := range lanIPs {
			out = append(out, h)
		}
		sort.Strings(out)
		return out, nil
	}
	if lanIPForHost(target) == "" {
		return nil, fmt.Errorf("host gate: unknown target %q (want msa2-server, msr1 or both)", target)
	}
	out := []string{target}
	if target != loadgenHost {
		out = append(out, loadgenHost)
	}
	sort.Strings(out)
	return out, nil
}

func collectHostGate(dir string, hosts []string) error {
	if err := requireAnsible(); err != nil {
		return err
	}
	vars, err := json.Marshal(map[string]string{"hostgate_local_dir": dir})
	if err != nil {
		return err
	}
	args := []string{"-i", "inventory.yml", hostGatePlaybook, "--limit", strings.Join(hosts, ","), "--extra-vars", string(vars)}
	if os.Getenv("CLUSTER_USE_LAN") == "1" {
		args = append(args, "--extra-vars", "use_lan=true")
	}
	cmd := exec.Command("ansible-playbook", args...)
	cmd.Dir = ansibleDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func writeHostGateSummary(res hostgate.Result) {
	path := os.Getenv("GITHUB_STEP_SUMMARY")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "### Host gate FAILED on %s\n\nNothing was stopped or removed. Find each process's owner before killing it.\n\n", strings.Join(res.Hosts(), ", "))
	for _, v := range res.Violations {
		fmt.Fprintf(f, "- **%s** `%s`: %s\n", v.Host, v.Rule, v.Summary)
		for _, l := range v.Lines {
			fmt.Fprintf(f, "  - `%s`\n", l)
		}
	}
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
