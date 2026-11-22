package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/probatorium/report"
)

// refusalCases is the mutation table of REFUSALS.md: for every ID, one or more
// mutations of an otherwise valid pair, each of which must produce EXACTLY that
// ID (no other item may fire, and none may be silent).
var refusalCases = map[string]map[string]func(f *fixture){
	"R1": {
		"arch differs (C4)": func(f *fixture) {
			f.P.HostArchPair = "linux/arm64"
			f.PTS.Arch = "arm64"
		},
		"timeseries label differs from the document": func(f *fixture) { f.PTS.Arch = "arm64" },
		"timeseries unlabelled":                      func(f *fixture) { f.CTS.Arch = "" },
		"SUT host differs":                           func(f *fixture) { f.PBench = "20261123T150000-bench-msr1" },
	},
	"R2": {"schema differs": func(f *fixture) { f.P.SchemaVersion = "5.23" }},
	"R3": {"harness commit differs": func(f *fixture) { f.PMeta["harness_commit"] = "def456" }},
	"R4": {"loadgen differs": func(f *fixture) { f.P.BenchmarkConfig.LoadgenVer = "v1.4.16" }},
	"R5": {
		"toolchain differs between documents": func(f *fixture) {
			for i := range f.P.Benchmarks {
				f.P.Benchmarks[i].LanguageVersion = "go1.27.4"
			}
		},
		"two toolchains inside one document": func(f *fixture) { f.C.Benchmarks[0].LanguageVersion = "go1.27.4" },
	},
	"R6": {"kernel differs": func(f *fixture) { f.PMeta["kernel"] = "7.0.0-40-generic" }},
	"R7": {"boot id differs": func(f *fixture) { f.PMeta["boot_id"] = "boot-2" }},
	"R8": {"profile differs": func(f *fixture) { f.PMeta["profile"] = "headline" }},
	"R9": {
		"duration":         func(f *fixture) { f.P.BenchmarkConfig.Duration = 60 * time.Second },
		"warmup":           func(f *fixture) { f.P.BenchmarkConfig.Warmup = 10 * time.Second },
		"runs":             func(f *fixture) { f.P.BenchmarkConfig.Runs = 3 },
		"scenarios filter": func(f *fixture) { f.P.BenchmarkConfig.ScenariosFilter = "s1" },
	},
	"R10": {"celeris pin differs and the peers carry a celeris column": func(f *fixture) {
		f.P.BenchmarkConfig.CelerisVer = "v1.5.13-y"
		f.P.Benchmarks = append(f.P.Benchmarks, row("celeris-p", "celeris", "celeris", "go", map[string]float64{"s1": 1, "s2": 2}))
		sort1(f.P)
	}},
	"R11": {
		"fabric":        func(f *fixture) { f.P.Environment.Fabric = "loopback" },
		"sut env":       func(f *fixture) { f.P.Environment.SUTEnv = map[string]string{"X": "1"} },
		"nic placement": func(f *fixture) { f.P.Environment.NICIRQPlacement = &report.NICIRQPlacement{Host: "msr1", CPU: 1} },
	},
	"R12": {
		"same run id": func(f *fixture) { f.PMeta["run_id"] = f.CMeta["run_id"] },
		"windows overlap": func(f *fixture) {
			f.P.BenchmarkConfig.StartedAt = t0.Add(10 * time.Minute)
			f.P.BenchmarkConfig.FinishedAt = t0.Add(40 * time.Minute)
		},
	},
	"R13": {"non-anchor column in both (C2/C2b)": func(f *fixture) {
		f.C.Benchmarks = append(f.C.Benchmarks, row("gin-h1", "framework", "gin", "go", map[string]float64{"s1": 1, "s2": 2}))
		sort1(f.C)
	}},
	"R14": {"duplicate inside one document": func(f *fixture) {
		f.P.Benchmarks = append(f.P.Benchmarks, f.P.Benchmarks[0])
	}},
	"R15": {
		"anchor missing from peers": func(f *fixture) {
			var keep []report.ServerResult
			for _, b := range f.P.Benchmarks {
				if b.Name != "stdhttp-h1" {
					keep = append(keep, b)
				}
			}
			f.P.Benchmarks = keep
			var ts []report.ScenarioSeries
			for _, s := range f.PTS.Scenarios {
				if s.Server != "stdhttp-h1" {
					ts = append(ts, s)
				}
			}
			f.PTS.Scenarios = ts
		},
		"anchor is another artifact": func(f *fixture) {
			for i := range f.P.Benchmarks {
				if f.P.Benchmarks[i].Name == "gnet-h1" {
					f.P.Benchmarks[i].FrameworkVersion = "v9.9.9"
				}
			}
		},
		"anchor has no paired ok cell": func(f *fixture) {
			for i := range f.P.Benchmarks {
				if f.P.Benchmarks[i].Name == "fiber-h1" {
					f.P.Benchmarks[i].CellStatuses = map[string]string{"s1": "dnf", "s2": "dnf"}
				}
			}
		},
	},
	"R16": {
		"only one document has a sidecar": func(f *fixture) { f.PTS = nil },
		"duplicate series (C2)":           func(f *fixture) { f.PTS.Scenarios = append(f.PTS.Scenarios, f.PTS.Scenarios[0]) },
		"orphan series":                   func(f *fixture) { f.PTS.Scenarios = append(f.PTS.Scenarios, f.CTS.Scenarios[0]) },
		"sidecar does not decode": func(f *fixture) {
			f.Extra = func(cr, pr string) {
				_ = os.WriteFile(filepath.Join(pr, "timeseries.json.gz"), []byte("not gzip"), 0o644)
			}
		},
	},
	"R17": {
		"validation present": func(f *fixture) { f.C.Validation = &report.ValidationResults{} },
		"soak present":       func(f *fixture) { f.P.Soak = &report.SoakSummary{} },
	},
	"R18": {"same bench dir name": func(f *fixture) { f.PBench = f.CBench }},
	"R19": {
		"profile missing": func(f *fixture) { delete(f.PMeta, "profile") },
		"boot id missing": func(f *fixture) { delete(f.CMeta, "boot_id") },
		"run id missing":  func(f *fixture) { delete(f.PMeta, "run_id"); f.Extra = nil },
		"loadgen missing": func(f *fixture) { f.C.BenchmarkConfig.LoadgenVer = ""; f.P.BenchmarkConfig.LoadgenVer = "" },
		"empty == empty must not pass": func(f *fixture) {
			delete(f.CMeta, "harness_commit")
			delete(f.PMeta, "harness_commit")
		},
		"started_at missing": func(f *fixture) { f.C.BenchmarkConfig.StartedAt = time.Time{} },
	},
	"R20": {"input does not round-trip": func(f *fixture) {
		f.Extra = func(cr, pr string) {
			p := filepath.Join(pr, "results.json")
			b, _ := os.ReadFile(p)
			_ = os.WriteFile(p, append(b, '\n'), 0o644)
		}
	}},
	"R21": {"expected column missing": func(f *fixture) { f.Opt.ExpectColumns = []string{"gin-h1", "no-such-column"} }},
	"R22": {"composed input": func(f *fixture) {
		f.Extra = func(cr, pr string) { _ = os.WriteFile(filepath.Join(cr, "COMPOSITION.json"), []byte("{}"), 0o644) }
	}},
}

func TestRefusalCases(t *testing.T) {
	for id, cases := range refusalCases {
		for name, mut := range cases {
			t.Run(id+"/"+name, func(t *testing.T) {
				f := newFixture()
				mut(f)
				res, err := f.compose(t)
				var re *RefusalError
				if !errors.As(err, &re) {
					t.Fatalf("want REFUSE %s, got err=%v res=%v", id, err, res != nil)
				}
				if got := re.IDs(); !reflect.DeepEqual(got, []string{id}) {
					t.Fatalf("want exactly [%s], got %v:\n%v", id, got, re)
				}
			})
		}
	}
}

// Every ID of REFUSALS.md has at least one case, and every case names an ID of
// REFUSALS.md: the list cannot gain or lose an item without a test noticing.
func TestRefusalsFileCoverage(t *testing.T) {
	re := regexp.MustCompile(`(?m)^## (R[0-9]+) `)
	var ids []string
	for _, m := range re.FindAllStringSubmatch(string(refusalsMD), -1) {
		ids = append(ids, m[1])
	}
	if len(ids) == 0 {
		t.Fatal("no IDs parsed from REFUSALS.md")
	}
	var cases []string
	for id := range refusalCases {
		cases = append(cases, id)
	}
	sort.Slice(ids, func(i, j int) bool { return idNum(ids[i]) < idNum(ids[j]) })
	sort.Slice(cases, func(i, j int) bool { return idNum(cases[i]) < idNum(cases[j]) })
	if !reflect.DeepEqual(ids, cases) {
		t.Fatalf("REFUSALS.md IDs %v != tested IDs %v", ids, cases)
	}
	for i, id := range ids {
		if idNum(id) != i+1 {
			t.Fatalf("IDs must be R1..Rn without gaps, got %v", ids)
		}
	}
	// the source never emits an ID the file does not declare
	src, _ := filepath.Glob("*.go")
	emit := regexp.MustCompile(`\.add\("(R[0-9]+)"`)
	declared := map[string]bool{}
	for _, id := range ids {
		declared[id] = true
	}
	for _, p := range src {
		b, _ := os.ReadFile(p)
		for _, m := range emit.FindAllStringSubmatch(string(b), -1) {
			if !declared[m[1]] {
				t.Errorf("%s emits %s, which REFUSALS.md does not declare", p, m[1])
			}
		}
	}
}

// All violated items are reported, not only the first.
func TestAllRefusalsReported(t *testing.T) {
	f := newFixture()
	f.PMeta["boot_id"] = "boot-2"
	f.PMeta["profile"] = "headline"
	f.P.SchemaVersion = "5.23"
	_, err := f.compose(t)
	var re *RefusalError
	if !errors.As(err, &re) || !reflect.DeepEqual(re.IDs(), []string{"R2", "R7", "R8"}) {
		t.Fatalf("%v", err)
	}
}

// A refusal writes nothing.
func TestRefusalWritesNothing(t *testing.T) {
	f := newFixture()
	f.PMeta["boot_id"] = "boot-2"
	c, p, cr, pr := f.load(t)
	out := filepath.Join(t.TempDir(), "out")
	rc := run([]string{"--candidate", cr, "--peers", pr, "--out", out, "--meta-candidate", filepath.Join(filepath.Dir(cr), "meta.txt"), "--meta-peers", filepath.Join(filepath.Dir(pr), "meta.txt")}, os.Stdout, os.Stderr)
	_ = c
	_ = p
	if rc != 2 {
		t.Fatalf("exit %d", rc)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("a refused composition must not create the output directory")
	}
}

// The same document twice is a refusal of R12 (it also trips R13 by construction).
func TestIdenticalDocumentsRefused(t *testing.T) {
	f := newFixture()
	*f.P = *f.C
	f.PTS = f.CTS
	f.PBench = "20261123T150000-bench-msa2-server"
	_, err := f.compose(t)
	var re *RefusalError
	if !errors.As(err, &re) {
		t.Fatal(err)
	}
	found := false
	for _, it := range re.Items {
		if it.ID == "R12" && strings.Contains(it.Msg, "byte-identical") {
			found = true
		}
	}
	if !found {
		t.Fatal(re)
	}
}
