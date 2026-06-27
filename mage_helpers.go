//go:build mage

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Internal helpers shared by mage_*.go targets. None of these are
// exported — mage discovers exported funcs as targets, so anything in
// this file stays out of the user-visible surface.

// Cluster orchestration constants. The ansible directory holds every
// playbook; bench state lives under /tmp on each node so reboot wipes
// it (matches inventory.yml `bench_root` / `results_root`).
const (
	ansibleDir       = "ansible"
	deployPlaybook   = "deploy.yml"
	cleanupPlaybook  = "cleanup.yml"
	benchPlaybook    = "bench.yml"
	validatePlaybook = "validate.yml"
	fuzzPlaybook     = "fuzz.yml"
	manifestRemote   = "/tmp/celeris-bench-manifest.json"
	// sshUser mirrors `ansible_user` in inventory.yml. The cluster nodes
	// only have a `mini` account, so any direct SSH must specify it
	// explicitly — Tailscale SSH otherwise tries to map the dev Mac's
	// current login (`fuming`) onto the target and fails with
	// "tailscale: failed to look up local user".
	sshUser = "mini"
)

// defaultClusterTarget is the default BENCH_TARGET / VALIDATE_TARGET when the
// user doesn't set one.
//
// arm64 benchmarking is TEMPORARILY DISABLED (2026-06): the sole arm64 cluster
// node (msr1 — a CIX Sky1 / CD8180 board) hard-hangs the entire host under
// sustained NIC load due to an immature SoC/firmware defect (celeris#312), NOT a
// celeris bug — both celeris and gnet trigger it and no OS-level workaround
// exists. Until a stable arm64 host replaces it, the benchmark + validation of
// record is amd64-only. Re-enable arm64 by setting BENCH_TARGET=both /
// VALIDATE_TARGET=both (or =msr1) once a reliable arm64 node is in the fabric.
const defaultClusterTarget = "msa2-server" // was "both"

// lanIPs maps inventory hostname to the LAN-pinned DHCP reservation
// used when CLUSTER_USE_LAN=1 forces traffic over the 20G LACP fabric
// instead of Tailscale's overlay. Update here AND in
// ansible/inventory.yml when a reservation changes.
var lanIPs = map[string]string{
	"msa2-server": "192.168.50.65",
	"msa2-client": "192.168.50.195",
	"msr1":        "192.168.50.199",
}

// lanIPForHost returns the LAN IP for a known cluster host, or "" if
// the host isn't in the static map.
func lanIPForHost(host string) string {
	return lanIPs[host]
}

// envOrDefault returns os.Getenv(k) if non-empty, otherwise d. Used
// for every bench/validate knob — keep the defaults next to the call
// site so each target's docstring reads as self-contained.
func envOrDefault(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// requireAnsible returns a friendly error if ansible-playbook is not
// on PATH. Every cluster-driven target calls this first so the dev
// gets a one-line install hint instead of an opaque exec error.
func requireAnsible() error {
	if _, err := exec.LookPath("ansible-playbook"); err != nil {
		return fmt.Errorf("ansible not installed — run `brew install ansible` (see ansible/README.md)")
	}
	return nil
}

// crossCompileGoBinary builds a Go package for linux/<arch> from
// moduleDir. Output is written to outputPath (resolved to absolute
// before the build so a non-cwd moduleDir doesn't drop the binary in
// the wrong place). -trimpath + `-s -w` keep the binary reproducible
// and small; CGO_ENABLED=0 keeps it statically linked so the cluster
// nodes don't need a matching libc.
func crossCompileGoBinary(moduleDir, pkgRel, outputPath, arch string) error {
	absOut, err := filepath.Abs(outputPath)
	if err != nil {
		return err
	}
	cmd := exec.Command("go", "build",
		"-trimpath",
		"-ldflags=-s -w",
		"-o", absOut,
		pkgRel,
	)
	cmd.Dir = moduleDir
	cmd.Env = append(os.Environ(),
		"GOOS=linux",
		"GOARCH="+arch,
		"CGO_ENABLED=0",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// Manifest mirrors the JSON written to /tmp/celeris-bench-manifest.json
// on each cluster node by the deploy playbook. The playbook is the
// source of truth for the on-disk shape — the field names here match
// `ansible/deploy.yml`'s `Persist deploy manifest` task verbatim.
//
//   - installed_packages: apt packages the playbook installed (so
//     cleanup can apt-purge them).
//   - prior_sysctl:       map of sysctl key → previous value, used to
//     restore kernel knobs the bench mutated.
//   - installed_toolchains: per-language toolchain dirs the playbook
//     created under bench_root (rust, bun, python, etc.). Each entry
//     is { lang, path, apt_pkgs }. Stored as a generic any so this
//     struct doesn't have to track the full schema as it evolves.
//   - fetched_versions:   pure-informational map of "always-latest"
//     resolutions (rustc, bun, python3.13, uv, ...) at deploy time.
type Manifest struct {
	InstalledPackages   []string          `json:"installed_packages,omitempty"`
	PriorSysctl         map[string]string `json:"prior_sysctl,omitempty"`
	InstalledToolchains []any             `json:"installed_toolchains,omitempty"`
	// FetchedVersions values are heterogeneous — each role writes
	// whatever the host's resolver returned. rustc / bun / uv stash
	// a version string; dbservices stashes a []string of image
	// digests. Decode loosely with `any` so Status doesn't choke
	// on either shape.
	FetchedVersions map[string]any `json:"fetched_versions,omitempty"`
}

// IsEmpty reports whether the manifest carries no installs / no
// sysctl mutations — i.e. the host is pristine from the bench's
// perspective even if the manifest file itself is present. A Go-only
// deploy on a fresh node produces an empty manifest because no
// toolchains/packages/sysctls are needed.
func (m Manifest) IsEmpty() bool {
	return len(m.InstalledPackages) == 0 &&
		len(m.PriorSysctl) == 0 &&
		len(m.InstalledToolchains) == 0 &&
		len(m.FetchedVersions) == 0
}

// manifestRead ssh's into host and dumps the bench manifest. Returns
// (false, zero, nil) when the manifest file is absent — that's how a
// freshly provisioned host looks. Returns (true, manifest, nil) when
// the file is present (which a Go-only deploy still produces, just
// with every field empty — call m.IsEmpty() to distinguish that
// case). Any SSH / JSON failure is surfaced as (false, zero, err).
//
// Routing:
//   - Always logs in as `mini` (matches inventory.yml ansible_user) —
//     bare-hostname SSH would inherit the current Mac user and fail
//     Tailscale SSH's local-user lookup.
//   - When CLUSTER_USE_LAN=1, targets the LAN IP directly (LACP fabric)
//     so Status() works even if Tailscale is offline / re-auth-pending.
func manifestRead(host string) (bool, Manifest, error) {
	var m Manifest
	target := sshUser + "@" + host
	if os.Getenv("CLUSTER_USE_LAN") == "1" {
		if ip := lanIPForHost(host); ip != "" {
			target = sshUser + "@" + ip
		}
	}
	// Use a sentinel marker so we can disambiguate "file absent" from
	// "file present but empty" — `cat … || true` collapses both to an
	// empty body without it.
	const marker = "__MANIFEST_ABSENT__"
	script := fmt.Sprintf("if [ -f %s ]; then cat %s; else printf '%%s' %s; fi",
		manifestRemote, manifestRemote, marker)
	cmd := exec.Command("ssh",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-o", "StrictHostKeyChecking=accept-new",
		target,
		script,
	)
	out, err := cmd.Output()
	if err != nil {
		return false, m, fmt.Errorf("ssh %s: %w", target, err)
	}
	raw := strings.TrimSpace(string(out))
	if raw == marker || raw == "" {
		return false, m, nil
	}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return false, m, fmt.Errorf("parse manifest from %s: %w", host, err)
	}
	return true, m, nil
}

// celerisVersion auto-detects the celeris release tag pinned in the
// caller's go.mod (fallback: env CELERIS_VERSION, then "dev"). Used
// to label result directories and the docs publish payload so a
// bench run is always traceable to a specific celeris version.
//
// Looks at the require block — `github.com/goceleris/celeris vX.Y.Z`
// — and returns the trimmed version. Returns ("dev", nil) if the
// dependency isn't present (e.g. running probatorium standalone).
func celerisVersion() (string, error) {
	if v := os.Getenv("CELERIS_VERSION"); v != "" {
		return v, nil
	}
	// Root go.mod first (probatorium standalone could pin celeris directly).
	if v := requireVersionFromFile("go.mod", "github.com/goceleris/celeris"); v != "" {
		return v, nil
	}
	// Fall back to the celeris ADAPTER's go.mod: probatorium's root module does
	// NOT require celeris (only the servers/celeris SUT does), so without this
	// the version degrades to "dev" even on a tagged release — which is exactly
	// what mislabeled the v1.5.5 publish (run dir "...-bench-dev"). The adapter
	// pin is the version actually benched, so use it for the run-dir name and
	// the publish version. PUBLISH_VERSION / CELERIS_VERSION still override.
	if v := requireVersionFromFile("servers/celeris/go.mod", "github.com/goceleris/celeris"); v != "" {
		return v, nil
	}
	return "dev", nil
}

// requireVersionFromFile returns the version pinned for modPath in the named
// go.mod, or "" when the file or require is absent. It matches the module path
// exactly (so "…/celeris" never matches "…/celeris/middleware/metrics") and
// only accepts a require line (version starts with "v"), so a `replace …=>…`
// directive for the same module is ignored.
func requireVersionFromFile(file, modPath string) string {
	data, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) >= 2 && fields[0] == modPath && strings.HasPrefix(fields[1], "v") {
			return fields[1]
		}
	}
	return ""
}

// goModRequireVersion returns the version pinned for modPath in the
// caller's go.mod, or "" when absent. Used to populate the v5.1
// BenchmarkConfig.loadgen_version from the require block without the
// env-override / "dev" fallback celerisVersion applies — an absent
// dependency should leave the field empty rather than misleading.
func goModRequireVersion(modPath string) string {
	data, err := os.ReadFile("go.mod")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) >= 2 && fields[0] == modPath {
			return fields[1]
		}
	}
	return ""
}
