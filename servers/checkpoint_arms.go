package servers

// PERF CHECKPOINT ARMS: throwaway branch perf/checkpoint-dccb839-vs-9f4d89b, not for merge
// (evidence/perf-checkpoint-20260927/PREREG.md). For every celeris engine column <name>:
//
//	<name>               A1  celeris main dccb839 (servers/celeris)
//	<name>-v9f4d89b-b1   B1  celeris 9f4d89b, the previous checkpoint's main (servers/baseline_celeris)
//	<name>-v9f4d89b-b2   B2  the SAME baseline binary again
//	<name>-vmain         A2  the SAME servers/celeris binary as <name>
//
// Names() sorts, so every engine runs A1 B1 B2 A2 back to back: both pairs are centred on the same
// instant, so a linear drift cancels in mean(A1,A2) - mean(B1,B2), and A1/A2 and B1/B2 give the
// in-run floors. Two B columns because one B column can sit ~0.5 pp off on its own (bisect run
// 36291510757).
func init() {
	const bVer = "v1.5.12-0.20260920100717-9f4d89b171db"
	for _, n := range Names() {
		a := Registry[n]
		if a.Category != "celeris" || a.Framework != "celeris" {
			continue
		}
		a2 := a
		a2.Name = n + "-vmain"
		Registry[a2.Name] = a2
		for _, sfx := range []string{"-v9f4d89b-b1", "-v9f4d89b-b2"} {
			b := a
			b.Name = n + sfx
			b.Framework = "celeris-9f4d89b" // keeps framework_version honest: mage_bench.go:1935 and
			b.FrameworkVersion = bVer       // cmd/runner/main.go:1954 stamp main's pin on Framework=="celeris"
			b.Bin = GoBinary{ModuleDir: "servers/baseline_celeris"}
			Registry[b.Name] = b
		}
	}
}
