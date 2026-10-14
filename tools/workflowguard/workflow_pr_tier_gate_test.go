package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The PR-tier workflow states its cluster-claim condition TWICE: once as the
// guard job's `if:`, and once inside the `concurrency.group` expression that
// decides whether the run claims the shared `matrix-tier-cluster` group or an
// inert per-run group. The two must agree.
//
// They have already drifted once. probatorium#288: a run whose guard would
// skip still claimed the shared group, and because GitHub keeps only one
// pending run per group, that no-op displaced a queued nightly or soak. The
// fix covered the wrong-label case and left the fork case behind, so a fork
// pull request carrying the label would still claim the cluster and then
// skip.
//
// A string test is a weak instrument in general, but the failure mode here is
// specifically textual: two copies of one condition, edited independently.
func TestPRTierClusterClaimMatchesTheGuard(t *testing.T) {
	b, err := os.ReadFile(".github/workflows/matrix-pr-tier.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	src := string(b)

	guard := extractGuardIf(t, src)
	claim := extractConcurrencyGroup(t, src)

	// Every clause that decides whether the cluster is reached must appear in
	// BOTH. Listed as the sub-expressions they are written as, not as prose,
	// so a rewrite that changes the meaning cannot satisfy the test by
	// keeping a comment.
	for _, clause := range []string{
		"github.event_name == 'workflow_dispatch'",
		"github.event.pull_request.head.repo.full_name == github.repository",
		"github.event.action == 'labeled'",
		"github.event.label.name == 'cluster-ok'",
		"github.event.action == 'synchronize'",
		"contains(github.event.pull_request.labels.*.name, 'cluster-ok')",
	} {
		if !strings.Contains(guard, clause) {
			t.Errorf("guard `if:` is missing the clause %q\ngot: %s", clause, guard)
		}
		if !strings.Contains(claim, clause) {
			t.Errorf("concurrency group is missing the clause %q\ngot: %s", clause, claim)
		}
	}

	// The fork check is the one that was missing from the concurrency
	// expression, and its absence is silent: the run claims the group,
	// the guard skips, and a queued nightly is displaced with no error
	// anywhere. Name it explicitly so a future edit that drops it fails
	// with a message that says what it broke.
	if !strings.Contains(claim, "head.repo.full_name == github.repository") {
		t.Error("the concurrency group must refuse to claim matrix-tier-cluster for a fork pull request; " +
			"without it a labelled fork PR claims the shared group and then skips, displacing a queued nightly (probatorium#288)")
	}
}

// A push to an already-approved pull request must re-validate. Requiring the
// label to be re-applied by hand is what left this workflow with zero
// successful runs between 2026-05-30 and 2026-09-14 (probatorium#360).
func TestPRTierRevalidatesOnPush(t *testing.T) {
	b, err := os.ReadFile(".github/workflows/matrix-pr-tier.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	src := string(b)

	types := regexp.MustCompile(`(?m)^\s*types:\s*\[([^\]]*)\]`).FindStringSubmatch(src)
	if types == nil {
		t.Fatal("no pull_request types list found")
	}
	for _, want := range []string{"labeled", "synchronize"} {
		if !strings.Contains(types[1], want) {
			t.Errorf("pull_request types %q must include %q", strings.TrimSpace(types[1]), want)
		}
	}
}

func extractGuardIf(t *testing.T, src string) string {
	t.Helper()
	i := strings.Index(src, "name: cluster-ok label gate")
	if i < 0 {
		t.Fatal("guard job not found")
	}
	rest := src[i:]
	j := strings.Index(rest, "if: >-")
	if j < 0 {
		t.Fatal("guard `if:` not found")
	}
	rest = rest[j:]
	k := strings.Index(rest, "runs-on:")
	if k < 0 {
		t.Fatal("end of guard `if:` not found")
	}
	return rest[:k]
}

func extractConcurrencyGroup(t *testing.T, src string) string {
	t.Helper()
	i := strings.Index(src, "\nconcurrency:")
	if i < 0 {
		t.Fatal("concurrency block not found")
	}
	rest := src[i:]
	j := strings.Index(rest, "cancel-in-progress:")
	if j < 0 {
		t.Fatal("end of concurrency block not found")
	}
	return rest[:j]
}
