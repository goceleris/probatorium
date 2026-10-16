package servers

// celeris#1036 ROUND-2 ARM64 ARMS (throwaway branch ab/arm64-r2, not for merge;
// evidence/cluster-reads-20261016/ab-prep/PLAN-arm64-r2.md). Four plan variants of celeris main
// fca46e6, three repetitions each, in one run on msr1:
//
//	A  = servers/celeris (re-pinned to main fca46e6): 12 loops, 8 pinned, 4 left to float over 0-11
//	B  = servers/revert973r2_celeris (ab/revert-973-r2 7a3f40f): #973 reverted, 12 loops pinned i % NumCPU
//	C1 = servers/c18loops_celeris (ab/c1-8loops 1af380f): 8 loops, all pinned to non-little CPUs
//	C2 = servers/c2masked_celeris (ab/c2-masked 037cba2): 12 loops, 8 pinned, 4 masked to the non-little CPUs
//
// Column suffixes sort as r1 (b c d; A is the plain column), r2 (a b c d), r3 (a b c d), so each engine runs
// A B C1 C2 A B C1 C2 A B C1 C2. -r2a and -r3a are the A binary again.
func init() {
	const (
		verB  = "v1.5.12-0.20261016183331-7a3f40f314b0"
		verC1 = "v1.5.12-0.20261016183331-1af380fffba5"
		verC2 = "v1.5.12-0.20261016183332-037cba2d081d"
	)
	arms := []struct{ suffix, framework, version, dir string }{
		{"b-rev973", "celeris-rev973r2", verB, "servers/revert973r2_celeris"},
		{"c-c1", "celeris-c1-8loops", verC1, "servers/c18loops_celeris"},
		{"d-c2", "celeris-c2-masked", verC2, "servers/c2masked_celeris"},
	}
	for _, n := range []string{
		"celeris-adaptive-h1-async",
		"celeris-epoll-h1-async",
		"celeris-iouring-h1-async",
	} {
		a, ok := Registry[n]
		if !ok || a.Framework != "celeris" {
			panic("arm64_r2_arms: no celeris column " + n)
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
