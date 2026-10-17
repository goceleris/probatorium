package servers

// celeris#1036 STALL INVESTIGATION, X2 ARMS, revised after the X1 read (throwaway branch ab/stall-x2, not for merge;
// evidence/cluster-reads-20261016/ab-prep/PLAN-stall-x2.md). One module, servers/x2knobs_celeris (celeris branch
// ab/stall-x2 37575415 = PR #1049 head 317fa77 + env knobs, all off when unset), and one column per arm; the arm
// is the column's run-time environment (servers.Adapter.SUTEnv), so every arm runs the same binary:
//
//	a-c1       no knob                          C1, 8 loops pinned on 0,1,6-11                 4 reps (C1 is bistable)
//	b-spin4    CELERIS_AB_SPIN_CPUS=4           C1 + a SCHED_IDLE spinner on CPU 4             1 rep (H1a has a low prior)
//	c-loop4    CELERIS_AB_EXTRA_LOOP_CPU=4      C1 + one more loop pinned on CPU 4 (9 loops)   4 reps, the decisive arm
//	e-nobp     CELERIS_AB_BUSYPOLL=0            C1 without SO_BUSY_POLL                         3 reps
//
// C3+spin8 is dropped. Sorted order per engine: r1 a b c e, r2 a c e, r3 a c e, r4 a c. The epoll post-4k control runs r1 only.
func init() {
	const ver = "v1.5.12-0.20261017151113-37575415d985"
	type arm struct {
		suffix, framework string
		env               map[string]string
		reps              []string
	}
	arms := []arm{
		{"a-c1", "celeris-x2-c1", nil, []string{"r1", "r2", "r3", "r4"}},
		{"b-spin4", "celeris-x2-spin4", map[string]string{"CELERIS_AB_SPIN_CPUS": "4"}, []string{"r1"}},
		{"c-loop4", "celeris-x2-loop4", map[string]string{"CELERIS_AB_EXTRA_LOOP_CPU": "4"}, []string{"r1", "r2", "r3", "r4"}},
		{"e-nobp", "celeris-x2-nobusypoll", map[string]string{"CELERIS_AB_BUSYPOLL": "0"}, []string{"r1", "r2", "r3"}},
	}
	for _, n := range []string{
		"celeris-adaptive-h1-async",
		"celeris-epoll-h1-async",
		"celeris-iouring-h1-async",
	} {
		a, ok := Registry[n]
		if !ok || a.Framework != "celeris" {
			panic("stall_x2_arms: no celeris column " + n)
		}
		for _, x := range arms {
			for _, r := range x.reps {
				v := a
				v.Name = n + "-" + r + x.suffix
				v.Framework = x.framework // keeps framework_version honest: main's pin is stamped on Framework=="celeris"
				v.FrameworkVersion = ver
				v.Bin = GoBinary{ModuleDir: "servers/x2knobs_celeris"}
				v.SUTEnv = x.env
				Registry[v.Name] = v
			}
		}
	}
}
