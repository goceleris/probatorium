package main

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// A cluster run is judged by conditions GitHub evaluates, not by code this
// repository runs, so these tests evaluate the workflows' own `if:`
// conditions under the contexts a run and its re-runs produce.
//
// GitHub re-runs failed jobs (and, the docs say, their dependents) reusing
// the results and outputs of every job that succeeded, and gives the new
// attempt a higher github.run_attempt. So on "Re-run failed jobs" after a
// cluster job failed, cluster-guard (which succeeded) is NOT re-run: its
// outputs are attempt 1's. The cluster job must not enter
// matrix-tier-cluster on that old verdict, and no host job may wait for a
// runner that attempt 1's teardown already removed.

// ghExpr evaluates the subset of the GitHub Actions expression language the
// stress workflows' conditions use: literals, context paths, ! && || == !=,
// parentheses, format() and the status functions. Values are string,
// float64, bool or nil, compared as GitHub compares them (strings without
// case, mixed types as numbers).
type ghExpr struct {
	toks []string
	pos  int
	ctx  map[string]any
}

var ghTokRe = regexp.MustCompile(`\s*('(?:[^']|'')*'|==|!=|&&|\|\||[!(),]|[0-9]+(?:\.[0-9]+)?|[A-Za-z_][A-Za-z0-9_.-]*)`)

// evalCondition evaluates a job's or step's if: value. Like GitHub, a
// condition that calls no status function is evaluated as success() && (it).
func evalCondition(t *testing.T, cond string, ctx map[string]any) bool {
	t.Helper()
	cond = strings.TrimSpace(cond)
	if strings.HasPrefix(cond, "${{") && strings.HasSuffix(cond, "}}") {
		cond = strings.TrimSpace(cond[3 : len(cond)-2])
	}
	if cond == "" {
		cond = "success()" // no if: at all
	}
	if !regexp.MustCompile(`\b(success|failure|cancelled|always)\(\)`).MatchString(cond) {
		cond = "success() && (" + cond + ")"
	}
	e := &ghExpr{ctx: ctx}
	rest := cond
	for strings.TrimSpace(rest) != "" {
		m := ghTokRe.FindStringSubmatchIndex(rest)
		if m == nil || m[0] != 0 {
			t.Fatalf("cannot tokenize %q at %q", cond, rest)
		}
		e.toks = append(e.toks, rest[m[2]:m[3]])
		rest = rest[m[1]:]
	}
	v := e.or(t)
	if e.pos != len(e.toks) {
		t.Fatalf("trailing tokens in %q: %v", cond, e.toks[e.pos:])
	}
	return truthy(v)
}

func (e *ghExpr) peek() string {
	if e.pos < len(e.toks) {
		return e.toks[e.pos]
	}
	return ""
}

func (e *ghExpr) next(t *testing.T) string {
	t.Helper()
	if e.pos >= len(e.toks) {
		t.Fatal("unexpected end of expression")
	}
	e.pos++
	return e.toks[e.pos-1]
}

func (e *ghExpr) or(t *testing.T) any {
	v := e.and(t)
	for e.peek() == "||" {
		e.next(t)
		r := e.and(t)
		v = truthy(v) || truthy(r)
	}
	return v
}

func (e *ghExpr) and(t *testing.T) any {
	v := e.unary(t)
	for e.peek() == "&&" {
		e.next(t)
		r := e.unary(t)
		v = truthy(v) && truthy(r)
	}
	return v
}

func (e *ghExpr) unary(t *testing.T) any {
	if e.peek() == "!" {
		e.next(t)
		return !truthy(e.unary(t))
	}
	v := e.primary(t)
	switch e.peek() {
	case "==":
		e.next(t)
		return ghEqual(v, e.primary(t))
	case "!=":
		e.next(t)
		return !ghEqual(v, e.primary(t))
	}
	return v
}

func (e *ghExpr) primary(t *testing.T) any {
	tok := e.next(t)
	switch {
	case tok == "(":
		v := e.or(t)
		if e.next(t) != ")" {
			t.Fatal("missing )")
		}
		return v
	case strings.HasPrefix(tok, "'"):
		return strings.ReplaceAll(tok[1:len(tok)-1], "''", "'")
	case tok == "true", tok == "false":
		return tok == "true"
	case tok == "null":
		return nil
	case tok[0] >= '0' && tok[0] <= '9':
		f, _ := strconv.ParseFloat(tok, 64)
		return f
	}
	if e.peek() != "(" {
		return e.ctx[tok] // a context path; absent is null, as on GitHub
	}
	e.next(t)
	var args []any
	for e.peek() != ")" {
		args = append(args, e.or(t))
		if e.peek() == "," {
			e.next(t)
		}
	}
	e.next(t)
	switch tok {
	case "format":
		s := ghString(args[0])
		for i, a := range args[1:] {
			s = strings.ReplaceAll(s, "{"+strconv.Itoa(i)+"}", ghString(a))
		}
		return s
	case "always":
		return true
	case "success", "failure", "cancelled":
		v, ok := e.ctx[tok+"()"]
		if !ok {
			return tok == "success"
		}
		return v
	}
	t.Fatalf("function %s() is not modelled", tok)
	return nil
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0 && !math.IsNaN(x)
	case string:
		return x != ""
	}
	return true
}

func ghString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return ""
}

func ghNumber(v any) float64 {
	switch x := v.(type) {
	case nil:
		return 0
	case bool:
		if x {
			return 1
		}
		return 0
	case float64:
		return x
	case string:
		if strings.TrimSpace(x) == "" {
			return 0
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return math.NaN()
		}
		return f
	}
	return math.NaN()
}

func ghEqual(a, b any) bool {
	as, aok := a.(string)
	bs, bok := b.(string)
	if aok && bok {
		return strings.EqualFold(as, bs)
	}
	if a == nil && b == nil {
		return true
	}
	return ghNumber(a) == ghNumber(b)
}

// The evaluator is only evidence if it evaluates what GitHub would.
func TestGhExprEvaluatesLikeGitHub(t *testing.T) {
	ctx := map[string]any{"github.run_attempt": "2", "needs.a-b.outputs.x": "2", "github.ref": "refs/heads/Main", "cancelled()": false}
	for cond, want := range map[string]bool{
		"github.run_attempt == needs.a-b.outputs.x":                    true,
		"github.run_attempt == '1'":                                    false,
		"github.run_attempt == 2":                                      true,
		"github.ref == 'refs/heads/main'":                              true, // case-insensitive
		"github.ref != format('refs/heads/{0}', 'main')":               false,
		"needs.missing.outputs.x == github.run_attempt":                false,
		"${{ !cancelled() && (github.ref == 'x' || 'a' == 'A') }}":     true,
		"always() && needs.missing.result == 'success'":                false,
		"needs.a-b.outputs.x == format('{0}', github.run_attempt)":     true,
		"${{ needs.a-b.outputs.x != '' && needs.a-b.outputs.x == 2 }}": true,
	} {
		if got := evalCondition(t, cond, ctx); got != want {
			t.Errorf("%s = %v, want %v", cond, got, want)
		}
	}
	if evalCondition(t, "github.ref == github.ref", map[string]any{"success()": false}) {
		t.Error("a condition without a status function must be ANDed with success()")
	}
}

// jobIf is a job's if: condition, or "" (GitHub's default, success()).
func jobIf(t *testing.T, src, job string) string {
	t.Helper()
	if m := regexp.MustCompile(`(?m)^    if: (.+)$`).FindStringSubmatch(jobBlock(t, src, job)); m != nil {
		return m[1]
	}
	return ""
}

// stepIf is the if: condition of the step with this name in a job, and
// whether the step exists.
func stepIf(t *testing.T, src, job, step string) (string, bool) {
	t.Helper()
	m := regexp.MustCompile(`(?s)\n      - name: ` + regexp.QuoteMeta(step) + `\n(.*?)(\n      - |\z)`).FindStringSubmatch(jobBlock(t, src, job))
	if m == nil {
		return "", false
	}
	if c := regexp.MustCompile(`(?m)^        if: (.+)$`).FindStringSubmatch(m[1]); c != nil {
		return c[1], true
	}
	return "", true
}

// dispatchCtx is a cluster dispatch from a standing branch in attempt
// runAttempt, with cluster-guard's and bootstrap's attempt outputs as given.
func dispatchCtx(runAttempt, guardAttempt, bootAttempt string) map[string]any {
	return map[string]any{
		"github.event_name":                      "workflow_dispatch",
		"github.ref":                             "refs/heads/stress/runs",
		"github.event.repository.default_branch": "main",
		"github.run_attempt":                     runAttempt,
		"needs.plan.result":                      "success",
		"needs.plan.outputs.target":              "cluster",
		"needs.cluster-guard.result":             "success",
		"needs.cluster-guard.outputs.attempt":    guardAttempt,
		"needs.bootstrap.result":                 "success",
		"needs.bootstrap.outputs.attempt":        bootAttempt,
	}
}

func TestStressClusterRerunNeverReentersTheGroup(t *testing.T) {
	caller := withoutComments(readStressWorkflow(t))
	called := withoutComments(readStressClusterWorkflow(t))
	// Each attempt output is the attempt its job ran in.
	for file, job := range map[string]string{stressWorkflow: "cluster-guard", stressClusterWorkflow: "bootstrap"} {
		src := caller
		if file == stressClusterWorkflow {
			src = called
		}
		if !regexp.MustCompile(`(?m)^    outputs:\n(?:      .*\n)*?      attempt: \$\{\{ github\.run_attempt \}\}\n`).MatchString(jobBlock(t, src, job) + "\n") {
			t.Errorf("%s: job %s does not output attempt: ${{ github.run_attempt }}", file, job)
		}
	}

	clusterIf, hostIf := jobIf(t, caller, "cluster"), jobIf(t, called, "host")
	for name, c := range map[string]struct {
		run, guard, boot string
		cluster, host    bool
	}{
		"first attempt":                             {"1", "1", "1", true, true},
		"re-run of failed jobs: guard reused":       {"2", "1", "1", false, false},
		"re-run of failed jobs: bootstrap reused":   {"2", "2", "1", true, false},
		"re-run that ran the guard and bootstrap":   {"2", "2", "2", true, true},
		"third attempt on the first attempt's view": {"3", "1", "1", false, false},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := dispatchCtx(c.run, c.guard, c.boot)
			if got := evalCondition(t, clusterIf, ctx); got != c.cluster {
				t.Errorf("the cluster job (which enters matrix-tier-cluster) runs = %v, want %v, for run_attempt %s on a guard from attempt %s:\n  if: %s",
					got, c.cluster, c.run, c.guard, clusterIf)
			}
			if got := evalCondition(t, hostIf, ctx); got != c.host {
				t.Errorf("a host job (which waits for a self-hosted runner) runs = %v, want %v, for run_attempt %s on a bootstrap from attempt %s:\n  if: %s",
					got, c.host, c.run, c.boot, hostIf)
			}
		})
	}
	// The guard refusing, or a pull request, still keeps the cluster job out.
	refused := dispatchCtx("1", "1", "1")
	refused["needs.cluster-guard.result"] = "failure"
	if evalCondition(t, clusterIf, refused) {
		t.Error("the cluster job runs after the guard refused")
	}
	pr := dispatchCtx("1", "1", "1")
	pr["github.event_name"] = "pull_request"
	if evalCondition(t, clusterIf, pr) {
		t.Error("the cluster job runs on a pull request")
	}

	// A re-run that skipped the cluster job says so and goes red, instead
	// of re-summarising attempt 1's logs as if they were new.
	const refuse = "Refuse a re-run that skipped the cluster"
	cond, ok := stepIf(t, caller, "summary", refuse)
	if !ok {
		t.Fatalf("the summary job has no step %q", refuse)
	}
	for name, c := range map[string]struct {
		target, cluster string
		want            bool
	}{
		"cluster skipped by a stale guard": {"cluster", "skipped", true},
		"cluster ran":                      {"cluster", "success", false},
		"cluster failed":                   {"cluster", "failure", false},
		"github target":                    {"github", "skipped", false},
	} {
		ctx := dispatchCtx("2", "1", "1")
		ctx["needs.plan.outputs.target"], ctx["needs.cluster.result"] = c.target, c.cluster
		if got := evalCondition(t, cond, ctx); got != c.want {
			t.Errorf("%s: step %q runs = %v, want %v (if: %s)", name, refuse, got, c.want, cond)
		}
	}
	if !strings.Contains(jobBlock(t, caller, "summary"), "      - name: "+refuse+"\n") ||
		strings.Index(jobBlock(t, caller, "summary"), refuse) > strings.Index(jobBlock(t, caller, "summary"), "Tally and judge every case") {
		t.Errorf("%q must come before the tally", refuse)
	}
}

// The host job's Go state (tools/stresstally/gostate.sh) is wiped by the
// job's last step, whatever happened before it, so the runner-dir wipes of
// teardown and of the next bootstrap (ansible file: state=absent, as the
// runner's user) never meet it.
func TestStressClusterHostJobWipesItsGoState(t *testing.T) {
	host := jobBlock(t, withoutComments(readStressClusterWorkflow(t)), "host")
	steps := regexp.MustCompile(`(?m)^      - (?:name|uses): `).FindAllStringIndex(host, -1)
	if len(steps) == 0 {
		t.Fatal("the host job has no steps")
	}
	last := host[steps[len(steps)-1][0]:]
	for _, want := range []string{"        if: ${{ always() }}\n", "        run: bash probatorium/tools/stresstally/gostate.sh wipe"} {
		if !strings.Contains(last+"\n", want) {
			t.Errorf("the host job's last step must be the always() Go state wipe; it lacks %q:\n%s", strings.TrimSpace(want), last)
		}
	}
}

// The host jobs each wait for ONE named runner; the bootstrap proves the
// planned hosts' runners are online before it succeeds, so a host job never
// queues for a runner that is not there.
func TestStressClusterBootstrapChecksThePlannedRunners(t *testing.T) {
	boot := jobBlock(t, withoutComments(readStressClusterWorkflow(t)), "bootstrap")
	up := strings.Index(boot, "uses: ./.github/actions/cluster-runner-up")
	check := strings.Index(boot, "run: bash tools/stresstally/runners-online.sh")
	if up < 0 || check < 0 || check < up {
		t.Fatalf("bootstrap must run tools/stresstally/runners-online.sh after cluster-runner-up:\n%s", boot)
	}
	for _, want := range []string{"GH_TOKEN: ${{ secrets.RUNNER_PAT }}", "MATRIX: ${{ inputs.matrix }}", "REPO: ${{ github.repository }}"} {
		if !strings.Contains(boot[up:], want) {
			t.Errorf("the runners check lacks %q", want)
		}
	}
}

// The guard's look and the run's entry into the group are as close as the
// job allows: the tool is built first, then the runs are listed, then judged.
func TestStressClusterGuardBuildsBeforeItLooks(t *testing.T) {
	guard := jobBlock(t, withoutComments(readStressWorkflow(t)), "cluster-guard")
	build := strings.Index(guard, `go build -o "$RUNNER_TEMP/stresstally" ./tools/stresstally`)
	list := strings.Index(guard, "bash tools/stresstally/guard.sh")
	judge := strings.Index(guard, `"$RUNNER_TEMP/stresstally" guard -dir "$GUARD_DIR" -self "$RUN_ID"`)
	if build < 0 || list < 0 || judge < 0 || !(build < list && list < judge) {
		t.Errorf("cluster-guard must build the tool, then list the runs, then judge them (positions %d %d %d):\n%s", build, list, judge, guard)
	}
}
