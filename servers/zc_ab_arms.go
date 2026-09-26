package servers

// SEND_ZC A/B ARMS (celeris#585): THROWAWAY BRANCH perf/zc-ab-9f4d89b, NOT
// FOR MERGE. Recipe and decision rule:
// evidence/measurements-585-587-588/lane-20260926/585/zc_ab_verdict.py (docstring).
//
// For each io_uring celeris column <n> the registry gets three columns of
// ONE binary (servers/celeris) and ONE -engine, differing only in the
// runtime SEND_ZC policy (servers.Adapter.SUTEnv, merged over the dispatch
// env and checked in the listener's /proc/<pid>/environ by the port guard):
//
//	<n>             A1  CELERIS_IOURING_SEND_ZC=on
//	<n>-zc1-off     B   CELERIS_IOURING_SEND_ZC=off
//	<n>-zc2-on      A2  CELERIS_IOURING_SEND_ZC=on   (in-run A/A twin)
//
// servers.Names() sorts, so each engine runs A1, B, A2 back to back: B sits
// between two ON runs, linear drift cancels in B - mean(A1, A2), and
// |A1 - A2| is each arch's own noise floor, as in the perf checkpoint's ABA.
// "on", never "auto", so the ON arms are not hostage to the functional
// probe (celeris#585 procedure step 3).
func init() {
	for _, n := range []string{
		"celeris-iouring-auto+upg-async",
		"celeris-iouring-h1-async",
		"celeris-iouring-h1-sync",
	} {
		a, ok := Registry[n]
		if !ok {
			panic("zc_ab_arms: no registry column " + n)
		}
		a.SUTEnv = map[string]string{"CELERIS_IOURING_SEND_ZC": "on"}
		Registry[n] = a
		b := a
		b.Name = n + "-zc1-off"
		b.SUTEnv = map[string]string{"CELERIS_IOURING_SEND_ZC": "off"}
		Registry[b.Name] = b
		a2 := a
		a2.Name = n + "-zc2-on"
		a2.SUTEnv = map[string]string{"CELERIS_IOURING_SEND_ZC": "on"}
		Registry[a2.Name] = a2
	}
}
