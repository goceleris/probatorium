package servers

// GOVAB (THROWAWAY ab/gov-ab-20261122, not for merge; PREREG-GOVAB): the five registry columns of the CPU power A/B on msa2-server. Each is a clone of the registry column
// celeris-<engine> (module servers/celeris, same engine, capabilities, category, SUT env) that runs ONE scenario with its own registry rated plan (cmd/runner resolveRatedPlan):
//
//	gabw-<engine>  ws-echo        fractions 0.25 and 0.9   (#755: the light-load ratio; 0.9 = the saturation-side reference)
//	gabl-<engine>  ws-large-echo  fractions 0.5  and 0.9   (#756)
//
// Framework is NOT "celeris" (the report builders stamp the main pin on Framework==celeris rows): it is celeris-govab with FrameworkVersion = the pin of servers/celeris/go.mod
// (gov_ab_arms_test.go asserts the two are equal, so a re-pin fails the test instead of mislabelling a column).
func init() {
	const pin = "v1.5.12-0.20261120120335-fe2b5be71ebc"
	for _, c := range []struct{ name, base, scenario string }{
		{"gabw-std-h1", "celeris-std-h1", "ws-echo"},
		{"gabw-adaptive-h1-async", "celeris-adaptive-h1-async", "ws-echo"},
		{"gabw-epoll-h1-async", "celeris-epoll-h1-async", "ws-echo"},
		{"gabl-std-h1", "celeris-std-h1", "ws-large-echo"},
		{"gabl-adaptive-h1-async", "celeris-adaptive-h1-async", "ws-large-echo"},
	} {
		base, ok := Registry[c.base]
		if !ok || base.Framework != "celeris" {
			panic("gov_ab_arms: no celeris base column " + c.base)
		}
		frac := []float64{0.25, 0.9}
		if c.scenario == "ws-large-echo" {
			frac = []float64{0.5, 0.9}
		}
		v := base
		v.Name = c.name
		v.Framework = "celeris-govab"
		v.FrameworkVersion = pin
		v.RatedPlans = map[string]RatedPlan{c.scenario: {Fractions: frac}}
		Registry[v.Name] = v
	}
}
