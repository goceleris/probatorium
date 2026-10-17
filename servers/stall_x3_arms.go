package servers

// celeris#1036 STALL INVESTIGATION, X3 HOST PROBES (throwaway branch ab/stall-x3-host, not for merge, NEEDS THE MAINTAINER'S APPROVAL:
// the probes change msr1 runtime state and restore it; evidence/cluster-reads-20261016/ab-prep/PLAN-stall-x3-host.md).
// Same binary as X2's C1 (servers/x2knobs_celeris, no knob); the arm is the probe named by AB_HOST_PROBE in the column env, which
// run_bench_cell.yml applies on the SUT before the runner and restores after it.
//
//	a-c1        no probe
//	f-napi0     napi_defer_hard_irqs=0, gro_flush_timeout=0 on enp1s0 and enp49s0
//	g-napi0one  the same on enp1s0 only
//	h-irq4to1   the enp1s0-8 IRQ moved from CPU 4 to CPU 1 (a loop CPU)
//	i-qdisc     enp1s0 root qdisc pfifo_fast instead of mq/fq_codel
func init() {
	const ver = "v1.5.12-0.20261017151113-37575415d985"
	arms := []struct{ suffix, probe string }{
		{"a-c1", ""}, {"f-napi0", "napi0"}, {"g-napi0one", "napi0one"}, {"h-irq4to1", "irq4to1"}, {"i-qdisc", "qdisc"},
	}
	for _, n := range []string{"celeris-adaptive-h1-async", "celeris-iouring-h1-async"} {
		a, ok := Registry[n]
		if !ok || a.Framework != "celeris" {
			panic("stall_x3_arms: no celeris column " + n)
		}
		for _, r := range []string{"r1", "r2", "r3"} {
			for _, x := range arms {
				v := a
				v.Name = n + "-" + r + x.suffix
				v.Framework = "celeris-x3-" + x.suffix
				v.FrameworkVersion = ver
				v.Bin = GoBinary{ModuleDir: "servers/x2knobs_celeris"}
				if x.probe != "" {
					v.SUTEnv = map[string]string{"AB_HOST_PROBE": x.probe}
				}
				Registry[v.Name] = v
			}
		}
	}
}
