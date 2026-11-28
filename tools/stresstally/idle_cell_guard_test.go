package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The bare-metal idle cell (.github/workflows/idle-cell-baremetal.yml) must never touch a cluster run that is going. Its two
// guards run `guard -idle`; these are the listings they judge. The release run that matters is the benchmark tier that holds
// the group and the runner of msr1 (run 45969386818 of 2026-11-27): a dispatch of the idle cell made early, by mistake, must be
// refused by the guard, which is what keeps its `cluster` job (the only one that joins the group) from being created.
func TestIdleCellGuardRefusesWhileAnotherClusterRunIsGoing(t *testing.T) {
	const self = 7
	idleCell := ".github/workflows/idle-cell-baremetal.yml"
	cases := map[string]struct {
		runs []ghRun
		want int
	}{
		"the benchmark tier running (the release run)": {[]ghRun{guardRun(45969386818, ".github/workflows/benchmark-tier.yml", "in_progress", "workflow_dispatch", "Benchmark Tier")}, 1},
		"the benchmark tier queued behind another":     {[]ghRun{guardRun(45969386818, ".github/workflows/benchmark-tier.yml", "queued", "workflow_dispatch", "Benchmark Tier")}, 1},
		"another idle cell running":                    {[]ghRun{guardRun(8, idleCell, "in_progress", "workflow_dispatch", "Idle cell run [msr1] stages a,b,c")}, 1},
		"another idle cell queued":                     {[]ghRun{guardRun(8, idleCell, "queued", "workflow_dispatch", "Idle cell run [msr1] stages a,b,c")}, 1},
		"this run alone":                               {[]ghRun{guardRun(self, idleCell, "in_progress", "workflow_dispatch", "Idle cell dry-run [msr1] stages a,b")}, 0},
		"nothing open":                                 {nil, 0},
		"a finished benchmark tier":                    {[]ghRun{guardRun(45969386818, ".github/workflows/benchmark-tier.yml", "completed", "workflow_dispatch", "Benchmark Tier")}, 0},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			var b strings.Builder
			for _, r := range c.runs {
				j, _ := json.Marshal(r)
				b.Write(j)
				b.WriteString("\n")
			}
			if err := os.WriteFile(filepath.Join(dir, "runs-in_progress.jsonl"), []byte(b.String()), 0o644); err != nil {
				t.Fatal(err)
			}
			var out strings.Builder
			code := cmdGuard([]string{"-dir", dir, "-self", "7", "-idle"}, &out)
			if code != c.want {
				t.Fatalf("guard -idle exit %d, want %d\n%s", code, c.want, out.String())
			}
			if c.want == 1 && !strings.Contains(out.String(), "::error::refusing: the cluster is not idle") {
				t.Errorf("the refusal is not an error annotation:\n%s", out.String())
			}
		})
	}
}
