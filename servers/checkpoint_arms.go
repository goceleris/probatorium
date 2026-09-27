package servers

// PERF CHECKPOINT ARMS: throwaway branch, not for merge (evidence/triage-20260926/PERF-CHECKPOINT.md).
// <name>-v158 = celeris v1.5.8 (servers/baseline_celeris); <name>-vmain = the SAME
// servers/celeris binary as <name>, an in-run A/A twin. Sorted order runs
// <name>, <name>-v158, <name>-vmain back to back for every engine.
func init() {
	for _, n := range Names() {
		a := Registry[n]
		if a.Category != "celeris" || a.Framework != "celeris" {
			continue
		}
		twin := a
		twin.Name = n + "-vmain"
		Registry[twin.Name] = twin
		base := a
		base.Name = n + "-v158"
		base.Framework = "celeris-v1.5.8" // keeps framework_version honest: mage_bench.go:1921 and
		base.FrameworkVersion = "v1.5.8"  // cmd/runner/main.go:1954 stamp main's pin on Framework=="celeris"
		base.Bin = GoBinary{ModuleDir: "servers/baseline_celeris"}
		Registry[base.Name] = base
	}
}

// celeris#674 A/B ARMS (throwaway branch perf/ab-674-7aeb2ec-vs-9f4d89b, not for merge;
// evidence/celeris-662/ab-674-cluster/PREREG.md). <name>-pr674 = PR #674 head 7aeb2ec
// (servers/pr674_celeris); <name> and <name>-vmain = main 9f4d89b (servers/celeris). Sorted order
// runs <name>, <name>-pr674, <name>-vmain back to back; the -v158 columns stay registered but the
// dispatch glob leaves them out.
func init() {
	const ver = "v1.5.12-0.20260927030609-7aeb2ec3340b"
	for _, n := range []string{
		"celeris-adaptive-h1-async",
		"celeris-epoll-h1-async",
		"celeris-epoll-h1-sync",
		"celeris-iouring-h1-async",
		"celeris-iouring-h1-sync",
	} {
		a, ok := Registry[n]
		if !ok || a.Framework != "celeris" {
			panic("checkpoint_arms: no celeris column " + n)
		}
		a.Name = n + "-pr674"
		a.Framework = "celeris-pr674" // keeps framework_version honest (mage_bench.go:1921, cmd/runner/main.go:1954)
		a.FrameworkVersion = ver
		a.Bin = GoBinary{ModuleDir: "servers/pr674_celeris"}
		Registry[a.Name] = a
	}
}
