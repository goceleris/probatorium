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
)

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

// shellQuote wraps s in single quotes safely for /bin/sh, so a value
// containing spaces or shell metacharacters survives a literal `eval`
// or `export` line. Embedded single quotes are split-and-re-escaped.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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

// buildSourceTarball runs `git archive HEAD | gzip > dst` to capture
// the current branch state. Tracked files only — no /vendor, no
// .git, no local artifacts. Identical to what dependabot or the
// GitHub PR diff observes.
func buildSourceTarball(dst string) error {
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	cmd := exec.Command("git", "archive", "--format=tar.gz", "HEAD")
	cmd.Stdout = out
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// findLoadgenSibling walks up from cwd looking for a directory at any
// ancestor level that contains a "loadgen/cmd/loadgen/main.go" file.
// Returns the absolute path to the loadgen repo root if found.
// Used by Deploy to locate the goceleris/loadgen sibling clone for
// cross-compile (sibling layout is the dev default; CI fetches via
// temp module instead).
func findLoadgenSibling() (string, bool) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", false
	}
	dir := cwd
	for {
		candidate := filepath.Join(dir, "loadgen", "cmd", "loadgen", "main.go")
		if _, err := os.Stat(candidate); err == nil {
			return filepath.Join(dir, "loadgen"), true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// Manifest mirrors the JSON written to /tmp/celeris-bench-manifest.json
// on each cluster node by the deploy playbook. It records every apt
// package the playbook installed plus any staged binary path, so the
// cleanup playbook can uninstall/remove them in reverse order.
//
// Fields stay loose (string-keyed map for extras) — the playbook is
// the source of truth for shape; this struct only needs to round-trip
// the parts Status() prints.
type Manifest struct {
	Host        string            `json:"host"`
	Timestamp   string            `json:"timestamp"`
	AptPackages []string          `json:"apt_packages,omitempty"`
	Binaries    []string          `json:"binaries,omitempty"`
	Extras      map[string]string `json:"extras,omitempty"`
}

// manifestRead ssh's into host and dumps the bench manifest. Returns
// (zero, nil) if the file does not exist (a freshly provisioned host
// won't have one), and (zero, err) for any other failure. Uses the
// same SSH path ansible would (no -o overrides — relies on
// ~/.ssh/config + ControlMaster).
func manifestRead(host string) (Manifest, error) {
	var m Manifest
	cmd := exec.Command("ssh",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		host,
		"cat "+manifestRemote+" 2>/dev/null || true",
	)
	out, err := cmd.Output()
	if err != nil {
		return m, fmt.Errorf("ssh %s: %w", host, err)
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return m, nil
	}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return m, fmt.Errorf("parse manifest from %s: %w", host, err)
	}
	return m, nil
}

// forwardEnvAsExports renders every os.Environ() entry whose key
// matches one of prefixes as a `export k='v'` line. Used to forward
// dev-machine env knobs (BENCH_*, VALIDATE_*, FUZZ_*) into a remote
// shell via an extra-vars exports block.
//
// A prefix matches if the key is exactly the prefix, has the prefix
// followed by an underscore, or — when the prefix itself ends in `_`
// — has the prefix as a literal substring at the start. Values are
// shell-quoted so spaces and metacharacters survive eval.
func forwardEnvAsExports(prefixes []string) string {
	var sb strings.Builder
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		k := kv[:eq]
		for _, p := range prefixes {
			if k == p || strings.HasPrefix(k, p+"_") || (strings.HasSuffix(p, "_") && strings.HasPrefix(k, p)) {
				fmt.Fprintf(&sb, "export %s=%s\n", k, shellQuote(kv[eq+1:]))
				break
			}
		}
	}
	return sb.String()
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
	data, err := os.ReadFile("go.mod")
	if err != nil {
		return "dev", nil
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "github.com/goceleris/celeris ") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				return fields[1], nil
			}
		}
	}
	return "dev", nil
}
