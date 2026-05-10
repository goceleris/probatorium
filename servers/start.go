package servers

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// BenchRootEnv is the env var the runner consults to find the staging
// directory the ansible deploy.yml populated. The default
// ("/tmp/celeris-bench") matches inventory.yml's bench_root, so a runner
// invoked over ssh by the bench playbook needs no extra wiring.
const BenchRootEnv = "PROBATORIUM_BENCH_ROOT"

// DefaultBenchRoot is the staging directory cleanup.yml / deploy.yml
// agree on. Mirrors inventory.yml's bench_root.
const DefaultBenchRoot = "/tmp/celeris-bench"

// AdapterStop is the handle returned by [StartAdapter]. Calling it sends
// SIGTERM to the spawned adapter, then SIGKILL after a grace period if
// the process has not exited. Stop is idempotent — calling it twice is a
// no-op.
type AdapterStop func() error

// StartAdapter boots the named adapter, bound to bindAddr, and returns a
// stop function the caller invokes after the cell completes.
//
// Resolution order for the executable:
//
//  1. PROBATORIUM_BENCH_ROOT/competitors/<name> — primary path the
//     ansible deploy.yml stages binaries to (bench_root in inventory.yml).
//  2. PROBATORIUM_BENCH_ROOT/<name>             — direct staging
//     for adapter binaries that are part of the core deploy set rather
//     than the competitor pool (rare, but kept for symmetry with the
//     runner / loadgen / observer staging in the same playbook).
//  3. ./<name>                                  — local-dev fallback,
//     resolved against the runner's working directory. Convenient for
//     `go run ./cmd/runner` against `go build -o ./<name>` artefacts.
//
// The first existing path wins; otherwise StartAdapter returns an error.
//
// For adapters whose Adapter.Engine field is non-empty (every celeris
// cell-column and the stdhttp h1/h2c/hybrid trio), the engine name is
// passed via "-engine <Engine>". This matches the per-server entry-point
// convention in servers/<framework>/server.go.
//
// NativeBinary adapters delegate to the spec's RunCmd (see
// [NativeBinary]); BuildSteps are NOT executed here — they belong in the
// ansible build phase.
func StartAdapter(ctx context.Context, name, bindAddr string) (AdapterStop, error) {
	a, ok := Registry[name]
	if !ok {
		return nil, fmt.Errorf("probatorium/servers: unknown adapter %q", name)
	}
	switch spec := a.Bin.(type) {
	case GoBinary:
		return startGoAdapter(ctx, a, spec, bindAddr)
	case NativeBinary:
		return startNativeAdapter(ctx, a, spec, bindAddr)
	default:
		return nil, fmt.Errorf("probatorium/servers: %s: unsupported BuildSpec %T", name, a.Bin)
	}
}

// startGoAdapter resolves the staged binary, exec's it with -bind / -engine,
// and returns a stop that signals the process group on shutdown.
func startGoAdapter(ctx context.Context, a Adapter, _ GoBinary, bindAddr string) (AdapterStop, error) {
	exe, err := resolveAdapterBinary(a.Name)
	if err != nil {
		return nil, err
	}
	args := []string{"-bind", bindAddr}
	if a.Engine != "" {
		args = append(args, "-engine", a.Engine)
	}
	return spawn(ctx, exe, args, nil)
}

// startNativeAdapter exec's the spec's RunCmd. Substitutions:
//
//	{bind}   → bindAddr
//	{engine} → a.Engine (empty string for non-celeris adapters)
//	{name}   → a.Name
//
// If the executable token (parts[0]) — after substitution — equals the
// adapter name, it is resolved through [resolveAdapterBinary] so the
// PROBATORIUM_BENCH_ROOT env var override and the local-dev fallback
// apply identically to GoBinary and NativeBinary adapters. RunCmd
// authors who want to bypass that resolution (absolute path, custom
// staging) can write the literal path into RunCmd directly.
func startNativeAdapter(ctx context.Context, a Adapter, spec NativeBinary, bindAddr string) (AdapterStop, error) {
	if spec.RunCmd == "" {
		return nil, fmt.Errorf("probatorium/servers: %s: NativeBinary.RunCmd is empty", a.Name)
	}
	expanded := expandTemplate(spec.RunCmd, map[string]string{
		"bind":   bindAddr,
		"engine": a.Engine,
		"name":   a.Name,
	})
	parts := strings.Fields(expanded)
	if len(parts) == 0 {
		return nil, fmt.Errorf("probatorium/servers: %s: empty RunCmd after expansion", a.Name)
	}
	exe := parts[0]
	if exe == a.Name {
		resolved, err := resolveAdapterBinary(a.Name)
		if err != nil {
			return nil, err
		}
		exe = resolved
	}
	return spawn(ctx, exe, parts[1:], nil)
}

// resolveAdapterBinary walks the staging-path candidate list and returns
// the first one that exists.
func resolveAdapterBinary(name string) (string, error) {
	root := os.Getenv(BenchRootEnv)
	if root == "" {
		root = DefaultBenchRoot
	}
	candidates := []string{
		filepath.Join(root, "competitors", name),
		filepath.Join(root, name),
		"./" + name,
	}
	var tried []string
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
		tried = append(tried, p)
	}
	return "", fmt.Errorf("probatorium/servers: adapter %q binary not found (tried %s)",
		name, strings.Join(tried, ", "))
}

// spawn starts cmd as its own process group, ties its lifetime to ctx,
// and returns a stop that issues SIGTERM → 5s grace → SIGKILL.
//
// The whole process group is signalled (negative PID) so child processes
// the adapter spawned (e.g. a JVM bootstrap script forking the real
// server) follow the parent down rather than turning into orphans.
func spawn(ctx context.Context, exe string, args []string, extraEnv []string) (AdapterStop, error) {
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = newProcAttr()
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("probatorium/servers: exec %s: %w", exe, err)
	}

	stopped := make(chan error, 1)
	go func() { stopped <- cmd.Wait() }()

	var done bool
	stop := func() error {
		if done {
			return nil
		}
		done = true
		if cmd.Process == nil {
			return nil
		}
		// Negative PID → process group; ignore ESRCH so a process that
		// already exited on its own does not surface as a stop error.
		_ = signalGroup(cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-stopped:
			return nil
		case <-time.After(5 * time.Second):
			_ = signalGroup(cmd.Process.Pid, syscall.SIGKILL)
			<-stopped
			return nil
		}
	}
	return stop, nil
}

// expandTemplate replaces {key} tokens in s using the given map. Unknown
// keys are left as-is so a template typo surfaces in the error path
// rather than silently producing a runnable-looking command line.
func expandTemplate(s string, vars map[string]string) string {
	for k, v := range vars {
		s = strings.ReplaceAll(s, "{"+k+"}", v)
	}
	return s
}

// ListByLanguage returns every Adapter whose Language field equals lang,
// sorted by Name. Used by mage Deploy's `DEPLOY_COMPETITORS=go-only`
// filter and by the conformance harness when restricted to one
// language family.
func ListByLanguage(lang string) []Adapter {
	out := make([]Adapter, 0, len(Registry))
	for _, a := range Registry {
		if a.Language == lang {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ListByCategory returns every Adapter whose Category field equals cat,
// sorted by Name.
func ListByCategory(cat string) []Adapter {
	out := make([]Adapter, 0, len(Registry))
	for _, a := range Registry {
		if a.Category == cat {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// AdaptersSorted returns every registered Adapter, sorted by Name.
// Convenience for callers (cmd/conformance) that walk the full set.
func AdaptersSorted() []Adapter {
	out := make([]Adapter, 0, len(Registry))
	for _, a := range Registry {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
