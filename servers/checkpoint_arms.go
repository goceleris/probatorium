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

func init() { // bisect arms for celeris-iouring-h1-sync only (PERF-CHECKPOINT §3.6 step 4)
	for _, bl := range []struct{ suffix, dir, ver string }{
		{"-v159", "servers/bl159_celeris", "v1.5.9"},
		{"-v1510", "servers/bl1510_celeris", "v1.5.10"},
		{"-v1511", "servers/bl1511_celeris", "v1.5.11"},
		{"-vc40d0cb", "servers/blc40d0cb_celeris", "v1.5.12-0.20260915174751-c40d0cb9d4b7"},
	} {
		a := Registry["celeris-iouring-h1-sync"]
		a.Name = "celeris-iouring-h1-sync" + bl.suffix
		a.Framework, a.FrameworkVersion = "celeris-"+bl.ver, bl.ver
		a.Bin = GoBinary{ModuleDir: bl.dir}
		Registry[a.Name] = a
	}
}
