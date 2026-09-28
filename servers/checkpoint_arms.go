package servers

// PERF CHECKPOINT REPLICATION ARMS: throwaway branch perf/checkpoint-698bed6-vs-9f4d89b, not for
// merge (evidence/perf-checkpoint-20260927/replication/PREREG.md). It replicates run 36340530922
// and adds the reversed slot order. For each of the six engine columns <E> below, eight columns:
//
//	<E>                   A1  celeris main 698bed6 (servers/celeris)
//	<E>-v9f4d89b-b1       B1  celeris 9f4d89b (servers/baseline_celeris)
//	<E>-v9f4d89b-b2       B2  the SAME baseline binary again
//	<E>-vmain             A2  the SAME servers/celeris binary as <E>
//	<E>-x5-v9f4d89b-b3    B3  the baseline binary again
//	<E>-x6-vmain-a3       A3  the servers/celeris binary again
//	<E>-x7-vmain-a4       A4  the servers/celeris binary again
//	<E>-x8-v9f4d89b-b4    B4  the baseline binary again
//
// Names() sorts, and these suffixes sort in the order above ("-v9" < "-vm" < "-x5" < ... < "-x8"),
// so every engine runs A1 B1 B2 A2 B3 A3 A4 B4 back to back. Slots 1-4 are run 36340530922's
// design with its column names. There B held the middle slots, so d = mean(A) - mean(B) is the
// quadratic slot contrast and a mid-block advantage reads as "A worse". Slots 5-8 reverse it: A
// holds the middle slots. Averaging the two orders cancels a hump shared by both halves. Over
// all eight slots, both the A and B positions sum to 18 and their squares to 102, so linear and
// quadratic drifts across the block cancel too.
//
// The other three engines (epoll-h1-sync, iouring-auto+upg-async, iouring-h1-async) get no extra
// columns, and the dispatch glob excludes their own columns: 9 engines x 8 columns would take
// about 9.4 h of cluster time, and the budget is about 6.5 h.
func init() {
	const bVer = "v1.5.12-0.20260920100717-9f4d89b171db"
	engines := []string{
		"celeris-adaptive-auto+upg-async", // flagged in run 36340530922 (arm64 RPS, marginal)
		"celeris-adaptive-h1-async",       // its h1-async sibling
		"celeris-epoll-auto+upg-async",    // flagged in run 36340530922 (arm64 RPS, marginal)
		"celeris-epoll-h1-async",          // its h1-async sibling
		"celeris-iouring-h1-sync",         // the repeat amd64 CPU/req tilt
		"celeris-std-h1",                  // the control
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
