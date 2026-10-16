package servers

// celeris#1037 p99 A/B ARMS (throwaway branch ab/p99-1037-amd64, not for merge;
// evidence/cluster-reads-20261016/ab-prep/PLAN-amd64.md). Same-run interleave on msa2-server of
// celeris dbbaaee (the bench 36441790519 build), ed3b7fb (the 2026-10-05 run) and the main tip
// 94b2db3, with the tip repeated as an in-run A/A twin. Every module has the same go.mod apart from
// the celeris require, so toolchain and dependencies are identical.
//
//	<name>              tip   (servers/celeris, re-pinned to 94b2db3)
//	<name>-r1a-dbbaaee  old   (servers/dbbaaee_celeris)
//	<name>-r1b-ed3b7fb  mid   (servers/ed3b7fb_celeris)
//	<name>-r1c-tip2     tip again, the A/A twin (servers/celeris, same binary as <name>)
//	<name>-r2a-... r2c  the same three again
//
// Sorted order per engine: tip, old, mid, tip2, old, mid, tip3.
func init() {
	const (
		verOld = "v1.5.12-0.20260928095652-dbbaaeebc86f"
		verMid = "v1.5.12-0.20261003135342-ed3b7fbcff33"
	)
	for _, n := range []string{
		"celeris-adaptive-h1-async",
		"celeris-epoll-h1-async",
		"celeris-iouring-h1-sync",
	} {
		a, ok := Registry[n]
		if !ok || a.Framework != "celeris" {
			panic("p99_1037_arms: no celeris column " + n)
		}
		for _, r := range []string{"r1", "r2"} {
			old := a
			old.Name = n + "-" + r + "a-dbbaaee"
			old.Framework = "celeris-dbbaaee" // keeps framework_version honest: main's pin is stamped on Framework=="celeris"
			old.FrameworkVersion = verOld
			old.Bin = GoBinary{ModuleDir: "servers/dbbaaee_celeris"}
			Registry[old.Name] = old

			mid := a
			mid.Name = n + "-" + r + "b-ed3b7fb"
			mid.Framework = "celeris-ed3b7fb"
			mid.FrameworkVersion = verMid
			mid.Bin = GoBinary{ModuleDir: "servers/ed3b7fb_celeris"}
			Registry[mid.Name] = mid

			twin := a
			twin.Name = n + "-" + r + "c-tip"
			Registry[twin.Name] = twin
		}
	}
}
