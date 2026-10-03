package servers

// PERF CHECKPOINT ARMS: throwaway branch perf/checkpoint-<A>-vs-9f4d89b, not for merge
// (evidence/perf-checkpoint-20261003/PREREG.md). celeris main A (servers/celeris) against 9f4d89b
// (servers/baseline_celeris), in both slot orders, on all nine celeris engine columns. For each
// engine column <E>, eight columns (the replication's suffixes, run 36376407767):
//
//	<E>                   A1  celeris main A (servers/celeris)
//	<E>-v9f4d89b-b1       B1  celeris 9f4d89b (servers/baseline_celeris)
//	<E>-v9f4d89b-b2       B2  the SAME baseline binary again
//	<E>-vmain             A2  the SAME servers/celeris binary as <E>
//	<E>-x5-v9f4d89b-b3    B3  the baseline binary again
//	<E>-x6-vmain-a3       A3  the servers/celeris binary again
//	<E>-x7-vmain-a4       A4  the servers/celeris binary again
//	<E>-x8-v9f4d89b-b4    B4  the baseline binary again
//
// Names() sorts, and these suffixes sort in the order above ("-v9" < "-vm" < "-x5" < ... < "-x8"),
// so every engine runs A1 B1 B2 A2 B3 A3 A4 B4 back to back. Slots 5-8 mirror slots 1-4, so the
// order pair cancels every slot term of degree 0 to 2 (and every within-half position term); the
// odd cubic term is the first one it leaves (PREREG section 0).
func init() {
	const bVer = "v1.5.12-0.20260920100717-9f4d89b171db"
	engines := []string{
		"celeris-adaptive-auto+upg-async",
		"celeris-adaptive-h1-async",
		"celeris-epoll-auto+upg-async",
		"celeris-epoll-h1-async",
		"celeris-epoll-h1-sync",
		"celeris-iouring-auto+upg-async",
		"celeris-iouring-h1-async",
		"celeris-iouring-h1-sync",
		"celeris-std-h1", // the control (it shares changed root code; PREREG section 5.2)
	}
	aSfx := []string{"-vmain", "-x6-vmain-a3", "-x7-vmain-a4"}
	bSfx := []string{"-v9f4d89b-b1", "-v9f4d89b-b2", "-x5-v9f4d89b-b3", "-x8-v9f4d89b-b4"}
	for _, n := range engines {
		a, ok := Registry[n]
		if !ok || a.Category != "celeris" || a.Framework != "celeris" {
			panic("checkpoint arms: " + n + " is not a registered celeris engine column")
		}
		for _, sfx := range aSfx {
			x := a
			x.Name = n + sfx
			Registry[x.Name] = x
		}
		for _, sfx := range bSfx {
			b := a
			b.Name = n + sfx
			b.Framework = "celeris-9f4d89b" // keeps framework_version honest: mage_bench.go and
			b.FrameworkVersion = bVer       // cmd/runner stamp main's pin on Framework=="celeris"
			b.Bin = GoBinary{ModuleDir: "servers/baseline_celeris"}
			Registry[b.Name] = b
		}
	}
}
