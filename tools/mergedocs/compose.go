package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/goceleris/probatorium/report"
)

// twinPrefix is servers.BaselinePrefix (a test pins the equality; the servers
// package is not imported so the composer does not link the adapter registry).
const twinPrefix = "baseline-celeris-v1511-"

// twinCategory is the category of a v1.5.11 twin column.
const twinCategory = "celeris-baseline"

// DefaultAnchors are the saturation-only columns measured in both dispatches.
var DefaultAnchors = []string{"gnet-h1", "fiber-h1", "stdhttp-h1"}

// Options are the composer's knobs. There is no option that relaxes a refusal.
type Options struct {
	Anchors []string
	Twin    string // "separate" (default), "strip" or "keep"
	Date    string // yyyymmdd, REQUIRED: the same date for both architectures
	// HarnessDiff proves two differing harness commits differ only by the celeris
	// pin: it returns the changed file names and the -U0 diff of go.mod/go.sum.
	// nil means no checkout was given, so differing commits are a refusal.
	HarnessDiff   func(a, b string) (names []string, diff string, err error)
	ExpectColumns []string
	Now           func() time.Time
}

var dateRE = regexp.MustCompile(`^[0-9]{8}$`)

func (o *Options) normalise() error {
	if o.Twin == "" {
		o.Twin = "separate"
	}
	switch o.Twin {
	case "separate", "strip", "keep":
	default:
		return fmt.Errorf("--twin=%q: want separate, strip or keep", o.Twin)
	}
	if !dateRE.MatchString(o.Date) {
		return fmt.Errorf("--date is required (yyyymmdd, got %q): publish both architectures of a release under the SAME date, because the README scripts read results/<version>/<date>/{x86_64,arm64}", o.Date)
	}
	if len(o.Anchors) == 0 {
		return fmt.Errorf("at least one drift anchor is required (default %s)", strings.Join(DefaultAnchors, ","))
	}
	seen := map[string]bool{}
	for _, a := range o.Anchors {
		if a == "" || seen[a] {
			return fmt.Errorf("--anchors: empty or repeated name %q", a)
		}
		seen[a] = true
	}
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
	return nil
}

func archTag(hostArchPair string) string {
	_, goarch, _ := strings.Cut(hostArchPair, "/")
	switch goarch {
	case "amd64", "x86_64":
		return "x86_64"
	case "arm64", "aarch64":
		return "arm64"
	}
	return goarch
}

func isTwin(b *report.ServerResult) bool {
	return strings.HasPrefix(b.Name, twinPrefix) || b.Category == twinCategory
}

// Result is a composition ready to be written.
type Result struct {
	Doc         *report.Document
	TS          *report.TimeseriesDoc // nil when neither input has a sidecar
	Comp        *Composition
	Twin        *TwinOutput // nil unless --twin=separate and twins exist
	BenchVars   map[string]json.RawMessage
	C, P        *Dispatch
	ResultsJSON []byte
}

// Compose checks the pair against REFUSALS.md and merges it.
func Compose(c, p *Dispatch, opt Options) (*Result, error) {
	if err := opt.normalise(); err != nil {
		return nil, err
	}
	var rf refusals
	checkPair(&rf, c, p, opt)
	if err := rf.err(); err != nil {
		return nil, err
	}

	// --- merge (every refusal passed: both Docs are non-nil) ---
	anchorSet := map[string]bool{}
	for _, a := range opt.Anchors {
		anchorSet[a] = true
	}
	byNameC := indexRows(c.Doc)
	byNameP := indexRows(p.Doc)

	columns := map[string]string{}
	var rows []report.ServerResult
	var twinRows []report.ServerResult
	var twinNames []string
	add := func(r report.ServerResult, role string) {
		if isTwin(&r) && opt.Twin != "keep" {
			twinRows = append(twinRows, r)
			twinNames = append(twinNames, r.Name)
			return
		}
		rows = append(rows, r)
		columns[r.Name] = role
	}
	for _, r := range c.Doc.Benchmarks {
		add(r, "candidate")
	}
	for _, r := range p.Doc.Benchmarks {
		if anchorSet[r.Name] {
			continue // the anchor row ships from the candidate dispatch
		}
		add(r, "peers")
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	sort.Strings(twinNames)

	var missing []string
	for _, n := range opt.ExpectColumns {
		if _, ok := columns[n]; !ok {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		rf.add("R21", "expected column(s) missing from the merged document: %s", strings.Join(missing, ", "))
		return nil, rf.err()
	}

	doc := *c.Doc
	doc.Benchmarks = rows
	bc := doc.BenchmarkConfig
	if p.Doc.BenchmarkConfig.StartedAt.Before(bc.StartedAt) {
		bc.StartedAt = p.Doc.BenchmarkConfig.StartedAt
	}
	if p.Doc.BenchmarkConfig.FinishedAt.After(bc.FinishedAt) {
		bc.FinishedAt = p.Doc.BenchmarkConfig.FinishedAt
	}
	doc.BenchmarkConfig = bc
	// load windows: concatenated and sorted; nil stays nil.
	if len(c.Doc.LoadWindows)+len(p.Doc.LoadWindows) > 0 {
		lw := append(append([]report.LoadWindow{}, c.Doc.LoadWindows...), p.Doc.LoadWindows...)
		sort.SliceStable(lw, func(i, j int) bool {
			if !lw[i].Start.Equal(lw[j].Start) {
				return lw[i].Start.Before(lw[j].Start)
			}
			if lw[i].Competitor != lw[j].Competitor {
				return lw[i].Competitor < lw[j].Competitor
			}
			return lw[i].Scenario < lw[j].Scenario
		})
		doc.LoadWindows = lw
	}
	resJSON, err := json.MarshalIndent(&doc, "", "  ")
	if err != nil {
		return nil, err
	}

	// --- timeseries ---
	var ts *report.TimeseriesDoc
	var twinSeries []report.ScenarioSeries
	if c.HasTS {
		out := *c.TS
		out.Scenarios = nil
		if p.TS.GeneratedAt.After(out.GeneratedAt) {
			out.GeneratedAt = p.TS.GeneratedAt
		}
		keep := func(s report.ScenarioSeries, role string) {
			if role == "peers" && anchorSet[s.Server] {
				return
			}
			if r := rowOf(byNameC, byNameP, s.Server, role); r != nil && isTwin(r) && opt.Twin != "keep" {
				twinSeries = append(twinSeries, s)
				return
			}
			out.Scenarios = append(out.Scenarios, s)
		}
		for _, s := range c.TS.Scenarios {
			keep(s, "candidate")
		}
		for _, s := range p.TS.Scenarios {
			keep(s, "peers")
		}
		sortSeries(out.Scenarios)
		sortSeries(twinSeries)
		ts = &out
		if err := checkMergedTS(ts, &doc); err != nil {
			rf.add("R16", "%v", err)
			return nil, rf.err()
		}
	}

	anchors := buildAnchors(opt.Anchors, byNameC, byNameP)
	date := opt.Date
	comp := &Composition{
		ComposerVersion: composerVersion,
		GeneratedAt:     opt.Now().UTC().Format(time.RFC3339),
		RefusalsSHA256:  refusalsSHA256(),
		HostArchPair:    doc.HostArchPair,
		Arch:            archTag(doc.HostArchPair),
		Publish: PublishRecord{
			Date: date, RunID: "run-1",
			Note: "ONE results.json, ONE date: publish once with BENCH_START_DATE=" + date + " and PUBLISH_RUN_ID=run-1",
		},
		Dispatches: map[string]DispatchRecord{
			"candidate": dispatchRecord(c),
			"peers":     dispatchRecord(p),
		},
		Anchors:  anchors,
		Columns:  columns,
		Twin:     TwinRecord{Mode: opt.Twin, Columns: twinNames},
		ColumnsN: len(rows),
	}
	comp.MaxAnchorDrift = anchors.MaxAnchorDrift
	comp.TieMargin = anchors.TieMargin
	comp.Warnings = append(comp.Warnings, anchors.Warnings...)
	if c.Doc.BenchmarkConfig.GitRef != p.Doc.BenchmarkConfig.GitRef || c.Doc.BenchmarkConfig.CelerisVer != p.Doc.BenchmarkConfig.CelerisVer {
		comp.Warnings = append(comp.Warnings, fmt.Sprintf("celeris pin differs between the dispatches (candidate %s, peers %s); the peers document carries no celeris column",
			c.Doc.BenchmarkConfig.CelerisVer, p.Doc.BenchmarkConfig.CelerisVer))
	}
	if !sameCommit(c.Meta.HarnessCommit, p.Meta.HarnessCommit) {
		comp.Warnings = append(comp.Warnings, fmt.Sprintf("harness commits differ (%s, %s) by the celeris pin only (proved with --harness-repo)", c.Meta.HarnessCommit, p.Meta.HarnessCommit))
	}
	if gap := p.Doc.BenchmarkConfig.StartedAt.Sub(c.Doc.BenchmarkConfig.FinishedAt); gap > 0 {
		comp.GapHours = gap.Hours()
	} else if gap2 := c.Doc.BenchmarkConfig.StartedAt.Sub(p.Doc.BenchmarkConfig.FinishedAt); gap2 > 0 {
		comp.GapHours = gap2.Hours()
	}
	sum := sha256.Sum256(resJSON)
	comp.Publish.ResultsSHA256 = hex.EncodeToString(sum[:])

	res := &Result{Doc: &doc, TS: ts, Comp: comp, C: c, P: p, ResultsJSON: resJSON}
	res.BenchVars = mergeBenchVars(c, p, anchorSet)
	if opt.Twin == "separate" && len(twinRows) > 0 {
		res.Twin = buildTwin(twinRows, twinSeries, c.Doc.Benchmarks)
		comp.Twin.Sidecars = []string{"twin-comparison.json", "twin-rows.json"}
	}
	return res, nil
}

const composerVersion = "mergedocs/1"

func indexRows(d *report.Document) map[string]*report.ServerResult {
	m := map[string]*report.ServerResult{}
	for i := range d.Benchmarks {
		m[d.Benchmarks[i].Name] = &d.Benchmarks[i]
	}
	return m
}

func rowOf(c, p map[string]*report.ServerResult, name, role string) *report.ServerResult {
	if role == "candidate" {
		return c[name]
	}
	return p[name]
}

func sortSeries(s []report.ScenarioSeries) {
	sort.SliceStable(s, func(i, j int) bool {
		if s[i].Scenario != s[j].Scenario {
			return s[i].Scenario < s[j].Scenario
		}
		return s[i].Server < s[j].Server
	})
}

func checkMergedTS(ts *report.TimeseriesDoc, doc *report.Document) error {
	gz, err := ts.MarshalGzip()
	if err != nil {
		return err
	}
	if err := report.CheckTimeseriesForArch(gz, archTag(doc.HostArchPair)); err != nil {
		return fmt.Errorf("merged sidecar: %w", err)
	}
	names := map[string]bool{}
	for _, b := range doc.Benchmarks {
		names[b.Name] = true
	}
	for _, s := range ts.Scenarios {
		if !names[s.Server] {
			return fmt.Errorf("merged sidecar holds a series for %q, which has no row in the merged document", s.Server)
		}
	}
	return nil
}

func mergeBenchVars(c, p *Dispatch, anchors map[string]bool) map[string]json.RawMessage {
	if c.BenchVars == nil && p.BenchVars == nil {
		return nil
	}
	out := map[string]json.RawMessage{}
	for k, v := range p.BenchVars {
		if !anchors[k] {
			out[k] = v
		}
	}
	for k, v := range c.BenchVars {
		out[k] = v
	}
	return out
}

// checkPair evaluates every refusal of REFUSALS.md and collects all of them.
func checkPair(rf *refusals, c, p *Dispatch, opt Options) {
	for _, d := range []*Dispatch{c, p} {
		if d.RoundTripErr != "" {
			rf.add("R20", "%s", d.RoundTripErr)
		}
		if d.Composed {
			rf.add("R22", "%s: %s already carries a COMPOSITION.json", d.Role, d.Dir)
		}
		for _, cf := range d.Meta.conflicts {
			rf.add("R19", "%s: %s", d.Role, cf)
		}
	}
	if c.Doc == nil || p.Doc == nil {
		return
	}
	cd, pd := c.Doc, p.Doc

	// R2, R1
	if rf.need("candidate", "schema_version", cd.SchemaVersion) && rf.need("peers", "schema_version", pd.SchemaVersion) && cd.SchemaVersion != pd.SchemaVersion {
		rf.add("R2", "schema_version %q (candidate) != %q (peers)", cd.SchemaVersion, pd.SchemaVersion)
	}
	if rf.need("candidate", "host_arch_pair", cd.HostArchPair) && rf.need("peers", "host_arch_pair", pd.HostArchPair) && cd.HostArchPair != pd.HostArchPair {
		rf.add("R1", "host_arch_pair %q (candidate) != %q (peers): one composition is one architecture", cd.HostArchPair, pd.HostArchPair)
	}
	for _, d := range []*Dispatch{c, p} {
		if d.HasTS && d.TS != nil {
			want := archTag(d.Doc.HostArchPair)
			if d.TS.Arch == "" {
				rf.add("R1", "%s: %s carries no arch label (an unlabelled sidecar cannot be told from a both-machine one)", d.Role, d.TSName)
			} else if d.TS.Arch != want {
				rf.add("R1", "%s: %s is labelled %q, the document is %q", d.Role, d.TSName, d.TS.Arch, want)
			}
		}
	}

	// R3..R8: metadata
	cm, pm := c.Meta, p.Meta
	cmp := func(id, what, a, b string, eq func(a, b string) bool) {
		okA := rf.need("candidate", what, a)
		okB := rf.need("peers", what, b)
		if okA && okB && !eq(a, b) {
			rf.add(id, "%s %q (candidate) != %q (peers)", what, a, b)
		}
	}
	same := func(a, b string) bool { return a == b }
	if okA, okB := rf.need("candidate", "harness commit", cm.HarnessCommit), rf.need("peers", "harness commit", pm.HarnessCommit); okA && okB && !sameCommit(cm.HarnessCommit, pm.HarnessCommit) {
		if _, why := harnessVerdict(opt, cm.HarnessCommit, pm.HarnessCommit); why != "" {
			rf.add("R3", "harness commit %q (candidate) != %q (peers): %s", cm.HarnessCommit, pm.HarnessCommit, why)
		}
	}
	cmp("R4", "loadgen_version", cd.BenchmarkConfig.LoadgenVer, pd.BenchmarkConfig.LoadgenVer, same)
	cmp("R6", "kernel", cm.Kernel, pm.Kernel, kernelEq)
	cmp("R7", "boot id", cm.BootID, pm.BootID, same)
	cmp("R8", "profile", cm.Profile, pm.Profile, same)
	rf.need("candidate", "run id", cm.RunID)
	rf.need("peers", "run id", pm.RunID)
	rf.need("candidate", "SUT host", cm.SUTHost)
	rf.need("peers", "SUT host", pm.SUTHost)
	if cm.SUTHost != "" && pm.SUTHost != "" && cm.SUTHost != pm.SUTHost {
		rf.add("R1", "SUT host %q (candidate) != %q (peers): one composition is one machine", cm.SUTHost, pm.SUTHost)
	}

	// R5: Go toolchain
	gc, okc := goToolchain(rf, "candidate", cd)
	gp, okp := goToolchain(rf, "peers", pd)
	if okc && okp && gc != gp {
		rf.add("R5", "Go toolchain %q (candidate) != %q (peers)", gc, gp)
	}

	// R9
	cb, pb := cd.BenchmarkConfig, pd.BenchmarkConfig
	if cb.Duration == 0 || pb.Duration == 0 {
		rf.add("R19", "benchmark_config.duration is missing on a side")
	}
	if cb.Duration != pb.Duration || cb.Warmup != pb.Warmup || cb.Runs != pb.Runs || cb.ScenariosFilter != pb.ScenariosFilter {
		rf.add("R9", "window differs: candidate duration=%s warmup=%s runs=%d scenarios_filter=%q, peers duration=%s warmup=%s runs=%d scenarios_filter=%q",
			cb.Duration, cb.Warmup, cb.Runs, cb.ScenariosFilter, pb.Duration, pb.Warmup, pb.Runs, pb.ScenariosFilter)
	}

	// R10
	if cb.GitRef != pb.GitRef || cb.CelerisVer != pb.CelerisVer {
		for _, b := range pd.Benchmarks {
			if b.Category == "celeris" || b.Framework == "celeris" {
				rf.add("R10", "celeris pin differs (candidate git_ref=%s celeris_version=%s; peers git_ref=%s celeris_version=%s) and the peers document carries the celeris column %q",
					cb.GitRef, cb.CelerisVer, pb.GitRef, pb.CelerisVer, b.Name)
				break
			}
		}
	}

	// R11
	if a, b := envConfig(cd.Environment), envConfig(pd.Environment); !reflect.DeepEqual(a, b) {
		aj, _ := json.Marshal(a)
		bj, _ := json.Marshal(b)
		rf.add("R11", "environment configuration differs: candidate %s | peers %s", aj, bj)
	}

	// R12
	if cm.RunID != "" && cm.RunID == pm.RunID {
		rf.add("R12", "both documents are run %s", cm.RunID)
	}
	if string(c.DocRaw) == string(p.DocRaw) {
		rf.add("R12", "the two results.json are byte-identical")
	}
	if cb.StartedAt.IsZero() || cb.FinishedAt.IsZero() || pb.StartedAt.IsZero() || pb.FinishedAt.IsZero() {
		rf.add("R19", "started_at/finished_at is missing on a side")
	} else if cb.StartedAt.Before(pb.FinishedAt) && pb.StartedAt.Before(cb.FinishedAt) {
		rf.add("R12", "the dispatch windows overlap (candidate %s..%s, peers %s..%s): one cluster cannot run two dispatches at once",
			cb.StartedAt.Format(time.RFC3339), cb.FinishedAt.Format(time.RFC3339), pb.StartedAt.Format(time.RFC3339), pb.FinishedAt.Format(time.RFC3339))
	}

	// R13, R14, R15
	anchorSet := map[string]bool{}
	for _, a := range opt.Anchors {
		anchorSet[a] = true
	}
	cnames, pnames := map[string]int{}, map[string]int{}
	for _, b := range cd.Benchmarks {
		cnames[b.Name]++
	}
	for _, b := range pd.Benchmarks {
		pnames[b.Name]++
	}
	for _, x := range []struct {
		role string
		m    map[string]int
	}{{"candidate", cnames}, {"peers", pnames}} {
		for n, k := range x.m {
			if k > 1 {
				rf.add("R14", "%s: column %q appears %d times", x.role, n, k)
			}
		}
	}
	var dups []string
	for n := range cnames {
		if pnames[n] > 0 && !anchorSet[n] {
			dups = append(dups, n)
		}
	}
	sort.Strings(dups)
	if len(dups) > 0 {
		rf.add("R13", "column(s) in both documents that are not declared anchors: %s", strings.Join(dups, ", "))
	}
	byC, byP := indexRows(cd), indexRows(pd)
	for _, a := range opt.Anchors {
		rc, rp := byC[a], byP[a]
		if rc == nil || rp == nil {
			rf.add("R15", "drift anchor %q is missing from the %s document", a, missingSide(rc == nil, rp == nil))
			continue
		}
		if diff := anchorIdentityDiff(rc, rp); diff != "" {
			rf.add("R15", "drift anchor %q is not the same artifact in both documents: %s", a, diff)
			continue
		}
		if !hasPairedSaturation(rc, rp) {
			rf.add("R15", "drift anchor %q has no saturation scenario measured ok in both documents", a)
		}
	}

	// R16
	if c.HasTS != p.HasTS {
		rf.add("R16", "only the %s document has a %s sidecar", map[bool]string{true: "candidate", false: "peers"}[c.HasTS], "timeseries.json.gz")
	}
	for _, d := range []*Dispatch{c, p} {
		if d.TSErr != "" {
			rf.add("R16", "%s", d.TSErr)
			continue
		}
		if !d.HasTS {
			continue
		}
		if err := report.CheckTimeseriesForArch(d.TSGz, archTag(d.Doc.HostArchPair)); err != nil && d.TS.Arch != "" {
			// label mismatch is R1; only the duplicate case is R16
			if strings.Contains(err.Error(), "appear more than once") {
				rf.add("R16", "%s: %v", d.Role, err)
			}
		}
		names := map[string]bool{}
		for _, b := range d.Doc.Benchmarks {
			names[b.Name] = true
		}
		orphans := map[string]bool{}
		for _, s := range d.TS.Scenarios {
			if !names[s.Server] {
				orphans[s.Server] = true
			}
		}
		if len(orphans) > 0 {
			var o []string
			for n := range orphans {
				o = append(o, n)
			}
			sort.Strings(o)
			rf.add("R16", "%s: series for server(s) with no row in the document: %s", d.Role, strings.Join(o, ", "))
		}
	}
	if c.HasTS && p.HasTS && c.TS != nil && p.TS != nil && c.TS.SchemaVersion != p.TS.SchemaVersion {
		rf.add("R16", "timeseries schema_version %q != %q", c.TS.SchemaVersion, p.TS.SchemaVersion)
	}

	// R17
	for _, d := range []*Dispatch{c, p} {
		if d.Doc.Validation != nil || d.Doc.Soak != nil {
			rf.add("R17", "%s: validation_results or soak_summary is present", d.Role)
		}
	}

	// R18
	cdirs := map[string]bool{}
	for _, bd := range c.BenchDirs {
		cdirs[bd.Name] = true
	}
	for _, bd := range p.BenchDirs {
		if cdirs[bd.Name] {
			rf.add("R18", "both run directories carry %q", bd.Name)
		}
	}
}

// sameCommit: equal, or one is an abbreviation (7+ hex) of the other.
func sameCommit(a, b string) bool {
	if a == b {
		return true
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	return len(a) >= 7 && strings.HasPrefix(b, a)
}

// harnessVerdict says whether two different harness commits differ only by the
// celeris pin (R3). reason is non-empty when that cannot be established.
func harnessVerdict(opt Options, a, b string) (pinOnly bool, reason string) {
	if opt.HarnessDiff == nil {
		return false, "no --harness-repo was given to prove that they differ only by the celeris pin"
	}
	names, diff, err := opt.HarnessDiff(a, b)
	if err != nil {
		return false, "the diff could not be read: " + err.Error()
	}
	if why := classifyHarnessDiff(names, diff); why != "" {
		return false, "the difference is not only the celeris pin: " + why
	}
	return true, ""
}

// classifyHarnessDiff returns "" when the commits differ only by the celeris
// pin: the changed files are go.mod or go.sum files (the pin lives in
// servers/celeris/go.mod, not in the root module) and every changed line of
// them names the module github.com/goceleris/celeris. Otherwise the reason.
func classifyHarnessDiff(names []string, diff string) string {
	if len(names) == 0 {
		return "no file differs (the commits have identical trees)"
	}
	for _, n := range names {
		if b := path.Base(n); b != "go.mod" && b != "go.sum" {
			return "file " + n + " differs"
		}
	}
	for _, l := range strings.Split(diff, "\n") {
		if l == "" || strings.HasPrefix(l, "+++") || strings.HasPrefix(l, "---") || (l[0] != '+' && l[0] != '-') {
			continue
		}
		if !strings.Contains(l, "github.com/goceleris/celeris ") && !strings.Contains(l, "github.com/goceleris/celeris/") {
			return "changed line outside the celeris pin: " + strings.TrimSpace(l)
		}
	}
	return ""
}

func missingSide(c, p bool) string {
	switch {
	case c && p:
		return "candidate and peers"
	case c:
		return "candidate"
	}
	return "peers"
}

func goToolchain(rf *refusals, role string, d *report.Document) (string, bool) {
	vs := map[string]bool{}
	for _, b := range d.Benchmarks {
		if b.Language == "go" {
			vs[b.LanguageVersion] = true
		}
	}
	if len(vs) == 0 {
		rf.add("R19", "%s: no Go row, so the Go toolchain cannot be established", role)
		return "", false
	}
	if len(vs) > 1 {
		var l []string
		for v := range vs {
			l = append(l, fmt.Sprintf("%q", v))
		}
		sort.Strings(l)
		rf.add("R5", "%s: the Go rows carry several language_version values: %s", role, strings.Join(l, ", "))
		return "", false
	}
	for v := range vs {
		if v == "" {
			rf.add("R19", "%s: the Go rows carry an empty language_version", role)
			return "", false
		}
		return v, true
	}
	return "", false
}

// envConfig is the configuration part of the environment: the measurements of
// the NIC placement readback are dropped.
func envConfig(e report.Environment) any {
	type nic struct {
		Host, Interface, Vector, Binding string
		CPU                              int
	}
	var n *nic
	if e.NICIRQPlacement != nil {
		x := e.NICIRQPlacement
		n = &nic{x.Host, x.Interface, x.Vector, x.Binding, x.CPU}
	}
	ae, _ := json.Marshal(e.ArchExclusion)
	return struct {
		Sysctls     []string          `json:"kernel_sysctls_applied"`
		LoadgenHost string            `json:"loadgen_host"`
		Fabric      string            `json:"fabric"`
		Line        int64             `json:"fabric_line_rate_bits_per_sec"`
		SUTEnv      map[string]string `json:"sut_env"`
		NIC         *nic              `json:"nic_irq_placement"`
		ArchExcl    string            `json:"arch_exclusion"`
	}{e.KernelSysctlsApplied, e.LoadgenHost, e.Fabric, e.FabricLineRateBitsPerSec, e.SUTEnv, n, string(ae)}
}

func anchorIdentityDiff(a, b *report.ServerResult) string {
	var d []string
	chk := func(n, x, y string) {
		if x != y {
			d = append(d, fmt.Sprintf("%s %q != %q", n, x, y))
		}
	}
	chk("category", a.Category, b.Category)
	chk("language", a.Language, b.Language)
	chk("framework", a.Framework, b.Framework)
	chk("framework_version", a.FrameworkVersion, b.FrameworkVersion)
	chk("engine", a.Engine, b.Engine)
	if !reflect.DeepEqual(a.CompileOptions, b.CompileOptions) {
		d = append(d, fmt.Sprintf("compile_options %v != %v", a.CompileOptions, b.CompileOptions))
	}
	return strings.Join(d, "; ")
}
