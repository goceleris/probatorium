package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/goceleris/probatorium/report"
)

// Meta is what the documents do not carry: it comes from a meta file, the
// artifact layout (run id, SUT host) and cluster-forensics (kernel, boot id).
type Meta struct {
	RunID         string            `json:"run_id"`
	HarnessCommit string            `json:"harness_commit"`
	Profile       string            `json:"profile"`
	SUTHost       string            `json:"sut_host"`
	Kernel        string            `json:"kernel"`
	BootID        string            `json:"boot_id"`
	Sources       map[string]string `json:"sources"`
	conflicts     []string
}

// benchDir is one `<TS>-bench-<host>` directory of a run dir and the column
// directories (`<RR>-<column>`) in it.
type benchDir struct {
	Name    string
	Columns []columnDir
}

type columnDir struct {
	Name      string // "00-celeris-epoll-h1-async"
	HasRollup bool   // the runner's own results.json is present
	Rollup    string // absolute path of that file, when present
}

// Dispatch is one input document with everything the composer reads.
type Dispatch struct {
	Role   string // "candidate" or "peers"
	Dir    string // directory holding results.json
	Doc    *report.Document
	DocRaw []byte
	// RoundTripErr is non-empty when results.json does not survive a decode
	// into report.Document and an encode back to the same bytes (R20).
	RoundTripErr string

	HasTS  bool
	TS     *report.TimeseriesDoc
	TSGz   []byte
	TSErr  string // decode failure (R16)
	TSName string

	BenchVars map[string]json.RawMessage // competitor_columns
	RawFiles  []string                   // base names under raw/
	BenchDirs []benchDir
	Composed  bool // COMPOSITION.json present (R22)
	Meta      Meta
}

// resolveRunDir finds the directory that holds results.json. dir itself, or
// the single match of the artifact layout benchmark-tier-results-*/*-bench-*/.
func resolveRunDir(dir string) (string, error) {
	if st, err := os.Stat(filepath.Join(dir, "results.json")); err == nil && !st.IsDir() {
		return dir, nil
	}
	var hits []string
	for _, pat := range []string{
		filepath.Join(dir, "benchmark-tier-results-*", "*-bench-*", "results.json"),
		filepath.Join(dir, "*-bench-*", "results.json"),
	} {
		m, _ := filepath.Glob(pat)
		hits = append(hits, m...)
	}
	sort.Strings(hits)
	switch len(hits) {
	case 0:
		return "", fmt.Errorf("no results.json under %s", dir)
	case 1:
		return filepath.Dir(hits[0]), nil
	}
	return "", fmt.Errorf("%d results.json under %s: pass the run directory itself", len(hits), dir)
}

// LoadDispatch reads one run directory. metaPath may be empty.
func LoadDispatch(role, dir, metaPath string) (*Dispatch, error) {
	run, err := resolveRunDir(dir)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", role, err)
	}
	d := &Dispatch{Role: role, Dir: run}
	d.DocRaw, err = os.ReadFile(filepath.Join(run, "results.json"))
	if err != nil {
		return nil, err
	}
	var doc report.Document
	if err := json.Unmarshal(d.DocRaw, &doc); err != nil {
		d.RoundTripErr = fmt.Sprintf("%s: results.json does not decode into report.Document: %v", role, err)
	} else {
		d.Doc = &doc
		out, merr := json.MarshalIndent(&doc, "", "  ")
		if merr != nil || !bytes.Equal(out, d.DocRaw) {
			d.RoundTripErr = fmt.Sprintf("%s: results.json does not re-encode to the same bytes through report.Document (%d vs %d bytes): the composer checkout and the run disagree on the schema", role, len(out), len(d.DocRaw))
		}
	}

	d.TSName = "timeseries.json.gz"
	if b, err := os.ReadFile(filepath.Join(run, d.TSName)); err == nil {
		d.HasTS, d.TSGz = true, b
		var ts report.TimeseriesDoc
		if err := ts.UnmarshalGzip(b); err != nil {
			d.TSErr = fmt.Sprintf("%s: %s does not decode: %v", role, d.TSName, err)
		} else {
			d.TS = &ts
		}
	}

	if b, err := os.ReadFile(filepath.Join(run, "bench-vars.json")); err == nil {
		var bv struct {
			CompetitorColumns map[string]json.RawMessage `json:"competitor_columns"`
		}
		if err := json.Unmarshal(b, &bv); err == nil {
			d.BenchVars = bv.CompetitorColumns
		}
	}
	if es, err := os.ReadDir(filepath.Join(run, "raw")); err == nil {
		for _, e := range es {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
				d.RawFiles = append(d.RawFiles, e.Name())
			}
		}
	}
	if _, err := os.Stat(filepath.Join(run, "COMPOSITION.json")); err == nil {
		d.Composed = true
	}
	es, err := os.ReadDir(run)
	if err != nil {
		return nil, err
	}
	for _, e := range es {
		if !e.IsDir() || !strings.Contains(e.Name(), "-bench-") {
			continue
		}
		bd := benchDir{Name: e.Name()}
		cs, err := os.ReadDir(filepath.Join(run, e.Name()))
		if err != nil {
			return nil, err
		}
		for _, c := range cs {
			if !c.IsDir() || !isColumnDirName(c.Name()) {
				continue
			}
			cd := columnDir{Name: c.Name()}
			p := filepath.Join(run, e.Name(), c.Name(), "results.json")
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				cd.HasRollup, cd.Rollup = true, p
			}
			bd.Columns = append(bd.Columns, cd)
		}
		d.BenchDirs = append(d.BenchDirs, bd)
	}
	d.Meta = loadMeta(run, metaPath, d)
	return d, nil
}

var columnDirRE = regexp.MustCompile(`^[0-9]+-.+`)

func isColumnDirName(n string) bool { return columnDirRE.MatchString(n) }

var runIDRE = regexp.MustCompile(`(?:benchmark-tier-results|rundir)-([0-9]+)`)

// loadMeta merges, in increasing precedence: the artifact layout and
// cluster-forensics, then the meta file. A disagreement between the file and
// the artifact is recorded (R19), not silently resolved.
func loadMeta(runDir, metaPath string, d *Dispatch) Meta {
	m := Meta{Sources: map[string]string{}}
	abs, _ := filepath.Abs(runDir)
	if s := runIDRE.FindStringSubmatch(abs); s != nil {
		m.RunID, m.Sources["run_id"] = s[1], "artifact path"
	}
	for _, bd := range d.BenchDirs {
		if i := strings.LastIndex(bd.Name, "-bench-"); i >= 0 {
			m.SUTHost, m.Sources["sut_host"] = bd.Name[i+len("-bench-"):], "bench dir name "+bd.Name
			break
		}
	}
	var fk, fb string
	if m.RunID != "" && m.SUTHost != "" {
		fk, fb = forensicsFor(abs, m.RunID, m.SUTHost)
		if fk != "" {
			m.Kernel, m.Sources["kernel"] = fk, "cluster-forensics host-forensics.txt"
		}
		if fb != "" {
			m.BootID, m.Sources["boot_id"] = fb, "cluster-forensics host-forensics.txt"
		}
	}
	if metaPath == "" {
		return m
	}
	f, err := parseMetaFile(metaPath)
	if err != nil {
		m.conflicts = append(m.conflicts, fmt.Sprintf("meta file %s: %v", metaPath, err))
		return m
	}
	src := "meta file " + filepath.Base(metaPath)
	set := func(key string, dst *string, v string, cmp func(a, b string) bool) {
		if v == "" {
			return
		}
		if *dst != "" && !cmp(*dst, v) {
			m.conflicts = append(m.conflicts, fmt.Sprintf("%s: meta file says %q, %s says %q", key, v, m.Sources[key], *dst))
			return
		}
		if *dst == "" || len(v) > len(*dst) {
			*dst = v
		}
		if m.Sources[key] == "" || strings.HasPrefix(m.Sources[key], "meta") {
			m.Sources[key] = src
		} else {
			m.Sources[key] += " + " + src
		}
	}
	eq := func(a, b string) bool { return a == b }
	set("run_id", &m.RunID, f["run_id"], eq)
	set("harness_commit", &m.HarnessCommit, f["harness_commit"], eq)
	set("profile", &m.Profile, f["profile"], eq)
	set("sut_host", &m.SUTHost, f["sut_host"], eq)
	set("kernel", &m.Kernel, f["kernel"], kernelEq)
	set("boot_id", &m.BootID, f["boot_id"], eq)
	return m
}

// kernelEq: params.txt carries "7.0.0-38", uname "7.0.0-38-generic".
func kernelEq(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"-") || strings.HasPrefix(b, a+"-")
}

// parseMetaFile reads a JSON object or a key=value file and normalises the
// keys of the three producers: the composer's own, `gh run view --json
// headSha,databaseId` and the read wrapper's params.txt.
func parseMetaFile(p string) (map[string]string, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	raw := map[string]string{}
	if t := bytes.TrimSpace(b); len(t) > 0 && t[0] == '{' {
		var j map[string]any
		if err := json.Unmarshal(t, &j); err != nil {
			return nil, err
		}
		for k, v := range j {
			switch x := v.(type) {
			case string:
				raw[k] = x
			case float64:
				raw[k] = fmt.Sprintf("%.0f", x)
			}
		}
	} else {
		for _, l := range strings.Split(string(b), "\n") {
			if k, v, ok := strings.Cut(strings.TrimSpace(l), "="); ok {
				raw[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
	}
	first := func(keys ...string) string {
		for _, k := range keys {
			if raw[k] != "" {
				return raw[k]
			}
		}
		return ""
	}
	return map[string]string{
		"run_id":         first("run_id", "databaseId"),
		"harness_commit": first("harness_commit", "headSha", "head"),
		"profile":        first("profile"),
		"sut_host":       first("sut_host", "host"),
		"kernel":         first("kernel"),
		"boot_id":        first("boot_id"),
	}, nil
}

// forensicsFor looks for cluster-forensics-<run>-*/<host>/host-forensics.txt
// beside the run directory (the layout of a downloaded run's artifacts).
func forensicsFor(absRunDir, runID, host string) (kernel, bootID string) {
	dir := absRunDir
	for i := 0; i < 4; i++ {
		m, _ := filepath.Glob(filepath.Join(dir, "cluster-forensics-"+runID+"-*", host, "host-forensics.txt"))
		if len(m) > 0 {
			b, err := os.ReadFile(m[0])
			if err != nil {
				return "", ""
			}
			return parseForensics(string(b))
		}
		dir = filepath.Dir(dir)
	}
	return "", ""
}

func parseForensics(s string) (kernel, bootID string) {
	lines := strings.Split(s, "\n")
	section := func(prefix string) string {
		for i, l := range lines {
			if strings.HasPrefix(l, "===== "+prefix) {
				for _, n := range lines[i+1:] {
					if t := strings.TrimSpace(n); t != "" {
						return t
					}
				}
			}
		}
		return ""
	}
	if f := strings.Fields(section("uname")); len(f) >= 3 && f[0] == "Linux" {
		kernel = f[2]
	}
	bootID = section("boot id")
	return
}

// gitHarnessDiff returns the HarnessDiff of a probatorium checkout: the changed
// file names between two commits and the -U0 diff of go.mod and go.sum.
func gitHarnessDiff(repo string) func(a, b string) ([]string, string, error) {
	git := func(args ...string) (string, error) {
		out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return string(out), nil
	}
	return func(a, b string) ([]string, string, error) {
		names, err := git("diff", "--name-only", a, b)
		if err != nil {
			return nil, "", err
		}
		var mods []string
		for _, n := range strings.Fields(names) {
			if b := path.Base(n); b == "go.mod" || b == "go.sum" {
				mods = append(mods, n)
			}
		}
		diff := ""
		if len(mods) > 0 {
			diff, err = git(append([]string{"diff", "-U0", a, b, "--"}, mods...)...)
			if err != nil {
				return nil, "", err
			}
		}
		return strings.Fields(names), diff, nil
	}
}
