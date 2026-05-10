//go:build mage

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Cluster orchestration targets. Mirrors celeris's mage_cluster.go but
// trims the "Cluster" prefix — every probatorium target is
// cluster-distributed by definition.
//
// Pristine semantics: every apt package installed by the deploy
// playbook is recorded in /tmp/celeris-bench-manifest.json; cleanup.yml
// uninstalls in reverse order. All transient state lives under /tmp on
// each node so a reboot wipes it. See ansible/README.md.

// Status prints quick health for each cluster node: ansible
// reachability, manifest state, and a short summary of any installed
// apt packages or staged binaries. Read-only — never mutates a node.
//
// Composed of two passes:
//
//  1. `ansible all -m shell -a uptime` — reachability check via the
//     same SSH path orchestration uses (Tailscale by default, LAN
//     when CLUSTER_USE_LAN=1).
//  2. manifestRead per host — pulls /tmp/celeris-bench-manifest.json
//     and prints apt_packages / binaries counts. A missing manifest
//     prints "no manifest" (a freshly provisioned host).
func Status() error {
	if err := requireAnsible(); err != nil {
		return err
	}
	args := []string{
		"-i", "inventory.yml", "all",
		"-m", "shell",
		"-a", "uptime -p",
	}
	cmd := exec.Command("ansible", args...)
	cmd.Dir = ansibleDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ansible reachability: %w", err)
	}

	hosts := []string{"msa2-server", "msa2-client", "msr1"}
	fmt.Printf("\n=== Manifest state ===\n")
	for _, h := range hosts {
		present, m, err := manifestRead(h)
		if err != nil {
			fmt.Printf("  %-12s  unreachable: %v\n", h, err)
			continue
		}
		if !present {
			fmt.Printf("  %-12s  no manifest (pristine)\n", h)
			continue
		}
		if m.IsEmpty() {
			fmt.Printf("  %-12s  manifest present, no installs (Go-only deploy)\n", h)
			continue
		}
		fmt.Printf("  %-12s  apt=%d toolchains=%d sysctls=%d\n",
			h, len(m.InstalledPackages), len(m.InstalledToolchains), len(m.PriorSysctl))
	}
	return nil
}

// Deploy cross-compiles every Go-side artifact and stages them on the
// cluster via deploy.yml. Steps:
//
//  1. Cross-compile probatorium's own four binaries (runner, loadgen,
//     observer, validator) for {linux/amd64, linux/arm64}.
//  2. For each Go competitor under servers/<name>/, cross-compile the
//     same two arches. DEPLOY_COMPETITORS controls the set:
//     "all" (default), "go-only" (skip non-Go), or a comma-separated
//     list of competitor names.
//  3. ansible-playbook deploy.yml, passing every staged binary path
//     as an extra-var. The playbook copies binaries into
//     {{ bench_root }} on each host and writes the manifest.
//
// Env knobs:
//
//	CLUSTER_USE_LAN=1          — connect via the LAN IP map (LACP
//	                             fabric) instead of Tailscale overlay.
//	DEPLOY_COMPETITORS=all|... — competitor selection (see above).
//
// Idempotent: re-running with the same competitor set is a no-op for
// the manifest (deploy.yml re-asserts each file).
func Deploy() error {
	if err := requireAnsible(); err != nil {
		return err
	}

	stagingDir, err := os.MkdirTemp("", "probatorium-deploy-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stagingDir)

	type bin struct {
		label  string
		module string
		pkg    string
		out    string
		arch   string
	}

	var jobs []bin
	// Core binaries land at {{ bench_root }}/<name> on the cluster.
	// The deploy playbook keys off the per-binary, per-arch extra-vars
	// listed below (e.g. runner_binary_amd64), so the set here MUST
	// stay in lockstep with `ansible/deploy.yml`'s push-* tasks.
	//
	// loadgen is amd64-only because msa2-client (the loadgen host)
	// is amd64. Multi-host loadgen federation (msr1 sidecar) is in
	// the v1.5 backlog; when it lands, add "arm64" here.
	coreBins := []struct {
		name  string
		archs []string
	}{
		{"runner", []string{"amd64", "arm64"}},
		{"loadgen", []string{"amd64"}},
		{"observer", []string{"amd64", "arm64"}},
		{"validator", []string{"amd64", "arm64"}},
		{"conformance", []string{"amd64", "arm64"}},
		{"validator-checker", []string{"amd64", "arm64"}},
		{"validator-replay", []string{"amd64", "arm64"}},
	}
	for _, b := range coreBins {
		for _, arch := range b.archs {
			jobs = append(jobs, bin{
				label:  b.name + " linux/" + arch,
				module: ".",
				pkg:    "./cmd/" + b.name,
				out:    filepath.Join(stagingDir, b.name+"-"+arch),
				arch:   arch,
			})
		}
	}

	// Go competitors live under servers/<name>/ as their own modules
	// (matches mage_helpers.go celerisVersion logic — competitor
	// modules track their own deps). Filter via DEPLOY_COMPETITORS.
	competitorsArg := envOrDefault("DEPLOY_COMPETITORS", "all")
	goComps, err := selectGoCompetitors(competitorsArg)
	if err != nil {
		return err
	}
	for _, c := range goComps {
		for _, arch := range []string{"amd64", "arm64"} {
			jobs = append(jobs, bin{
				label:  "competitor " + c + " linux/" + arch,
				module: filepath.Join("servers", c),
				pkg:    ".",
				out:    filepath.Join(stagingDir, "competitor-"+c+"-"+arch),
				arch:   arch,
			})
		}
	}

	for _, j := range jobs {
		fmt.Printf("Cross-compiling %s...\n", j.label)
		if err := crossCompileGoBinary(j.module, j.pkg, j.out, j.arch); err != nil {
			return fmt.Errorf("cross-compile %s: %w", j.label, err)
		}
	}

	// The deploy playbook reads two surfaces:
	//
	//   1. Flat per-binary/per-arch vars for core binaries, e.g.
	//        runner_binary_amd64=<host path>
	//        runner_binary_arm64=<host path>
	//      Hyphens in binary names map to underscores (validator-checker
	//      -> validator_checker_binary_amd64) per Ansible/Jinja2
	//      identifier rules.
	//
	//   2. A `competitor_binaries` dict for Go competitors:
	//        {"gin": {"amd64": "...", "arm64": "..."}, ...}
	//      plus `competitor_set` echoed through so the playbook can
	//      filter (go-only | all | csv).
	//
	// We pass everything through a single JSON vars-file (`@vars.json`)
	// rather than many `key=value` --extra-vars. Ansible's `key=value`
	// form is string-only — passing a dict literal like
	// `competitor_binaries={"gin":...}` is silently coerced to a string
	// and `dict | list` on that string returns its characters, which is
	// exactly the failure mode we hit before this rewrite.
	coreSet := make(map[string]bool, len(coreBins))
	for _, b := range coreBins {
		coreSet[b.name] = true
	}
	vars := map[string]any{
		"competitor_set": competitorsArg,
	}
	competitorBinaries := make(map[string]map[string]string)
	for _, j := range jobs {
		base := filepath.Base(j.out)
		logical := strings.TrimSuffix(base, "-"+j.arch)
		if coreSet[logical] {
			// runner / loadgen / observer / validator / conformance /
			// validator-checker / validator-replay
			varName := strings.ReplaceAll(logical, "-", "_") + "_binary_" + j.arch
			vars[varName] = j.out
			continue
		}
		// Competitor: strip the "competitor-" prefix to recover the slug.
		slug := strings.TrimPrefix(logical, "competitor-")
		if competitorBinaries[slug] == nil {
			competitorBinaries[slug] = make(map[string]string)
		}
		competitorBinaries[slug][j.arch] = j.out
	}
	if len(competitorBinaries) > 0 {
		vars["competitor_binaries"] = competitorBinaries
	}
	if os.Getenv("CLUSTER_USE_LAN") == "1" {
		vars["use_lan"] = true
		// Pin each host's ansible_host to its LAN IP for this run.
		for h, ip := range lanIPs {
			vars["ansible_host_"+strings.ReplaceAll(h, "-", "_")] = ip
		}
	}

	varsJSON, err := json.MarshalIndent(vars, "", "  ")
	if err != nil {
		return err
	}
	varsFile := filepath.Join(stagingDir, "deploy-vars.json")
	if err := os.WriteFile(varsFile, varsJSON, 0o600); err != nil {
		return err
	}

	args := []string{
		"-i", "inventory.yml",
		deployPlaybook,
		"--extra-vars", "@" + varsFile,
	}

	cmd := exec.Command("ansible-playbook", args...)
	cmd.Dir = ansibleDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("deploy: %w", err)
	}
	fmt.Printf("\n=== Deploy complete (%d binaries cross-compiled, %d competitors) ===\n",
		len(jobs), len(competitorBinaries))
	return nil
}

// Cleanup tears down every cluster artifact: uninstalls apt packages
// recorded in the manifest, removes /tmp/celeris-bench/ and
// /tmp/celeris-results/, and deletes the manifest itself. Use after
// any failed/interrupted bench or validate run.
//
// Env knobs:
//
//	CLEANUP_HOSTS=all|<csv>    — limit cleanup to a subset (default:
//	                             all cluster hosts).
//
// Safe to run on a freshly provisioned host (no manifest → no-op).
func Cleanup() error {
	if err := requireAnsible(); err != nil {
		return err
	}
	hosts := envOrDefault("CLEANUP_HOSTS", "all")
	args := []string{
		"-i", "inventory.yml",
		cleanupPlaybook,
		"--limit", hosts,
	}
	cmd := exec.Command("ansible-playbook", args...)
	cmd.Dir = ansibleDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// selectGoCompetitors returns the list of Go-based competitor
// directories under servers/ that match the DEPLOY_COMPETITORS spec.
// Recognised values:
//
//	"all"       — every directory under servers/ that has a go.mod
//	"go-only"   — same as "all" for Go competitors (the spec exists
//	              for symmetry with Rust/Bun/Python competitors that
//	              the ansible roles handle separately).
//	"<csv>"     — comma-separated list of names; each must exist
//	              under servers/ with a go.mod.
//
// Empty input behaves like "all" (matches the Deploy default).
func selectGoCompetitors(spec string) ([]string, error) {
	all, err := scanGoCompetitors()
	if err != nil {
		return nil, err
	}
	switch spec {
	case "", "all", "go-only":
		return all, nil
	}
	want := make(map[string]bool)
	for _, name := range strings.Split(spec, ",") {
		if name = strings.TrimSpace(name); name != "" {
			want[name] = true
		}
	}
	have := make(map[string]bool, len(all))
	for _, n := range all {
		have[n] = true
	}
	var out []string
	for name := range want {
		if !have[name] {
			return nil, fmt.Errorf("DEPLOY_COMPETITORS: %q not found under servers/ (or missing go.mod)", name)
		}
		out = append(out, name)
	}
	return out, nil
}

// scanGoCompetitors lists every immediate child of servers/ that has
// a go.mod (i.e. is a Go competitor module). Non-Go competitors
// (rust/, bun/, python/) live under their own ansible roles and are
// staged from source on the cluster nodes themselves.
func scanGoCompetitors() ([]string, error) {
	entries, err := os.ReadDir("servers")
	if err != nil {
		// servers/ might not exist in a freshly cloned repo — treat
		// as zero competitors rather than an error so Deploy still
		// stages probatorium's own binaries.
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// "common" is the shared scaffolding directory — not a
		// competitor itself.
		if e.Name() == "common" {
			continue
		}
		if _, err := os.Stat(filepath.Join("servers", e.Name(), "go.mod")); err == nil {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// runHostsParallel fans out fn over hosts and returns the first
// non-nil error per host as a single combined error. Used by targets
// that orchestrate multiple hosts independently (each host gets its
// own ansible-playbook subprocess).
func runHostsParallel(hosts []string, fn func(host string) error) error {
	type res struct {
		host string
		err  error
	}
	results := make(chan res, len(hosts))
	var wg sync.WaitGroup
	for _, h := range hosts {
		wg.Add(1)
		go func(host string) {
			defer wg.Done()
			results <- res{host: host, err: fn(host)}
		}(h)
	}
	wg.Wait()
	close(results)
	var failed []string
	for r := range results {
		if r.err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", r.host, r.err))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("parallel run failed on %d host(s): %s",
			len(failed), strings.Join(failed, "; "))
	}
	return nil
}
