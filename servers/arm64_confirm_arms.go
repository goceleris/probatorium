package servers

// celeris PR #1049 CONFIRMATION ARMS for arm64 / msr1 (throwaway branch ab/arm64-confirm, not for
// merge; evidence/cluster-reads-20261016/ab-prep/PLAN-arm64-confirm.md). Four arms, three
// repetitions each, in one run:
//
//	A  = servers/celeris           celeris main eb75c84 (default worker count: 12 loops, 8 pinned, 4 floating)
//	B  = servers/revert973r3_celeris   eb75c84 with #973 reverted (ab/revert-973-r3 659b3f8): 12 loops pinned i % NumCPU
//	C1 = servers/pr1049_celeris    PR #1049 head 655d8d6, default worker count: 8 loops, all pinned
//	C3 = servers/pr1049c3_celeris  ab/c3-7loops 79f6ce5 = PR #1049 head 655d8d6 + CELERIS_AB_EXCLUDE_CPU=8 set by
//	                                   the adapter: 7 loops pinned on 0,1,6,7,9,10,11, none on CPU 8 (diagnostic)
//	(the D arm of the confirmation run, servers/pr1049wrap_celeris, is dropped on this branch)
//
// Column suffixes sort as r1 (b c d; A is the plain column), r2 (a b c d), r3 (a b c d), so each
// engine runs B C1 C3 B C1 C3 B C1 C3 (the glob selects only the -r* columns). -r2a and -r3a are the A binary again.
func init() {
	const (
		verB  = "v1.5.12-0.20261017075218-659b3f8636a6"
		verC1 = "v1.5.12-0.20261017001135-655d8d68553c"
		verC3 = "v1.5.12-0.20261017100904-79f6ce537951"
	)
	arms := []struct{ suffix, framework, version, dir string }{
		{"b-rev973", "celeris-rev973r3", verB, "servers/revert973r3_celeris"},
		{"c-pr1049", "celeris-pr1049", verC1, "servers/pr1049_celeris"},
		{"d-c3", "celeris-pr1049-c3", verC3, "servers/pr1049c3_celeris"},
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
			// X1 (stall investigation): no arm A columns; the plain column <n> is not selected by the cells glob.
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
