package servers

// PRE-FREEZE SCREEN ARMS: throwaway branch ab/scrz-diag-20261122, not for merge
// (evidence/lanes-20261122/SCRZ/PREREG-SCRZ.md.DRAFT). celeris main fe2b5be7 (servers/celeris) against
// the celeris v1.5.11 release (servers/celeris_v1511), on the eight loop-engine columns. For each
// engine column <E> four columns of the SAME run, in this slot order:
//
//	<E>                A1  celeris main (servers/celeris)
//	<E>-v1511-b1       B1  celeris v1.5.11 (servers/celeris_v1511)
//	<E>-v1511-b2       B2  the SAME v1.5.11 binary again
//	<E>-vmain          A2  the SAME servers/celeris binary as <E>
//
// Names() sorts and these suffixes sort in the order above ("-v1511-b1" < "-v1511-b2" < "-vmain"), so
// every engine runs A1 B1 B2 A2 back to back (the mechanism of checkpoint_arms.go of the
// perf-checkpoint-20261003 branch). The B arms are built from the MAIN celeris-<E> adapter (not from the
// opt-in baseline-celeris-v1511-<E> one, which is Baseline=true and invisible when baseline=false), with
// the baseline module and the baseline framework label; Capabilities and Engine are main's, as in
// servers/baseline.go. celeris-std-h1 is not carried (the v1.5.11 twin set has no std-h1).
func init() {
	engines := []string{
		"epoll-h1-sync",
		"epoll-h1-async",
		"epoll-auto+upg-async",
		"iouring-h1-sync",
		"iouring-h1-async",
		"iouring-auto+upg-async",
		"adaptive-h1-async",
		"adaptive-auto+upg-async",
	}
	for _, e := range engines {
		n := "celeris-" + e
		a, ok := Registry[n]
		if !ok || a.Category != "celeris" || a.Framework != "celeris" || a.Baseline {
			panic("screen arms: " + n + " is not a registered main celeris engine column")
		}
		x := a
		x.Name = n + "-vmain"
		Registry[x.Name] = x
		for _, sfx := range []string{"-v1511-b1", "-v1511-b2"} {
			b := a
			b.Name = n + sfx
			b.Framework = "celeris-baseline"
			b.FrameworkVersion = BaselineCelerisVersion
			b.Bin = GoBinary{ModuleDir: BaselineModule}
			Registry[b.Name] = b
		}
	}
}
