package servers

// celeris#973 REVERT A/B ARMS (throwaway branch ab/revert973-vs-main530858f, not for merge;
// evidence/cluster-reads-20261016/ab-prep/PLAN.md). Answers whether celeris#973 (3185649, the CPU
// pin policy) caused the arm64 loop-engine regression of celeris#1036.
//
//	<name>             arm A  = celeris main 530858f  (servers/celeris, re-pinned on this branch)
//	<name>-revert973   arm B  = main 530858f with #973 reverted, celeris branch ab/revert-973 at
//	                            fdc0449 (servers/revert973_celeris)
//	<name>-vmain       arm A' = the SAME servers/celeris binary as A, an in-run A/A twin
//
// Sorted order runs <name>, <name>-revert973, <name>-vmain back to back per engine: A, B, A'.
func init() {
	const ver = "v1.5.12-0.20261016113634-fdc044919a71"
	for _, n := range []string{
		"celeris-adaptive-h1-async",
		"celeris-epoll-h1-async",
		"celeris-iouring-h1-async",
	} {
		a, ok := Registry[n]
		if !ok || a.Framework != "celeris" {
			panic("revert973_arms: no celeris column " + n)
		}
		twin := a
		twin.Name = n + "-vmain"
		Registry[twin.Name] = twin

		b := a
		b.Name = n + "-revert973"
		b.Framework = "celeris-revert973" // keeps framework_version honest: main's pin is stamped on Framework=="celeris"
		b.FrameworkVersion = ver
		b.Bin = GoBinary{ModuleDir: "servers/revert973_celeris"}
		Registry[b.Name] = b
	}
}
