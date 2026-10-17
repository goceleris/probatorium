package servers

// celeris PR #1049 CONFIRMATION ARMS for arm64 / msr1 (throwaway branch ab/arm64-confirm, not for
// merge; evidence/cluster-reads-20261016/ab-prep/PLAN-arm64-confirm.md). Four arms, three
// repetitions each, in one run:
//
//	A  = servers/celeris           celeris main eb75c84 (default worker count: 12 loops, 8 pinned, 4 floating)
//	B  = servers/revert973r3_celeris   eb75c84 with #973 reverted (ab/revert-973-r3 659b3f8): 12 loops pinned i % NumCPU
//	C1 = servers/pr1049_celeris    PR #1049 head 655d8d6, default worker count: 8 loops, all pinned
//	D  = servers/pr1049wrap_celeris    PR #1049 head 655d8d6 with Config.Workers 12 set in the adapter: 12 loops
//	                                   pinned round-robin over the 8 non-little CPUs (the wrap path)
//
// Column suffixes sort as r1 (b c d; A is the plain column), r2 (a b c d), r3 (a b c d), so each
// engine runs A B C1 D A B C1 D A B C1 D. -r2a and -r3a are the A binary again.
func init() {
	const (
		verB  = "v1.5.12-0.20261017075218-659b3f8636a6"
		verC1 = "v1.5.12-0.20261017001135-655d8d68553c"
	)
	arms := []struct{ suffix, framework, version, dir string }{
		{"b-rev973", "celeris-rev973r3", verB, "servers/revert973r3_celeris"},
		{"c-pr1049", "celeris-pr1049", verC1, "servers/pr1049_celeris"},
		{"d-wrap", "celeris-pr1049-wrap12", verC1, "servers/pr1049wrap_celeris"},
	}
	for _, n := range []string{
		"celeris-adaptive-h1-async",
		"celeris-epoll-h1-async",
		"celeris-iouring-h1-async",
	} {
		a, ok := Registry[n]
		if !ok || a.Framework != "celeris" {
			panic("arm64_confirm_arms: no celeris column " + n)
		}
		for _, r := range []string{"r1", "r2", "r3"} {
			if r != "r1" {
				twin := a // arm A again: same binary as the plain column
				twin.Name = n + "-" + r + "a-tip"
				Registry[twin.Name] = twin
			}
			for _, x := range arms {
				v := a
				v.Name = n + "-" + r + x.suffix
				v.Framework = x.framework // keeps framework_version honest: main's pin is stamped on Framework=="celeris"
				v.FrameworkVersion = x.version
				v.Bin = GoBinary{ModuleDir: x.dir}
				Registry[v.Name] = v
			}
		}
	}
}
