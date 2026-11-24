package main

import (
	"sort"
)

// Composition is COMPOSITION.json: everything the study says is not stored in
// the documents, plus the drift record the tie rule reads.
type Composition struct {
	ComposerVersion string `json:"composer_version"`
	GeneratedAt     string `json:"generated_at"`
	RefusalsSHA256  string `json:"refusals_sha256"`
	HostArchPair    string `json:"host_arch_pair"`
	Arch            string `json:"arch"`

	Publish PublishRecord `json:"publish"`

	// TieMarginByScenario is what the README scripts and the docs read: celeris
	// counts as ahead of a framework from the OTHER dispatch only when its margin
	// exceeds the margin of that scenario (same-dispatch comparisons keep the
	// 5 % band). Columns says which dispatch each merged column came from; the
	// anchors ship from the candidate dispatch. MaxAnchorDrift is reporting only.
	MaxAnchorDrift      float64            `json:"max_anchor_drift"`
	TieMarginByScenario map[string]float64 `json:"tie_margin_by_scenario"`
	// Reboot says whether the SUT host rebooted between the two dispatches
	// (boot ids differ); accepted only under R7's conditions.
	Reboot RebootRecord `json:"reboot"`

	// HarnessDiff is set when the two dispatches ran different harness commits
	// (R3): the files that differ, none of which can change a measurement.
	HarnessDiff *HarnessDiff `json:"harness_diff,omitempty"`

	Dispatches map[string]DispatchRecord `json:"dispatches"`
	GapHours   float64                   `json:"gap_hours_between_dispatches"`
	Anchors    AnchorSection             `json:"anchors"`
	Columns    map[string]string         `json:"columns"`
	ColumnsN   int                       `json:"columns_merged"`
	Twin       TwinRecord                `json:"twin"`
	Warnings   []string                  `json:"warnings,omitempty"`
}

// RebootRecord is the reboot note of a composition.
type RebootRecord struct {
	BetweenDispatches bool   `json:"between_dispatches"`
	Note              string `json:"note,omitempty"`
}

// PublishRecord is the one cell the composition is published as.
type PublishRecord struct {
	Date          string `json:"publish_date"`
	RunID         string `json:"publish_run_id"`
	ResultsSHA256 string `json:"results_json_sha256"`
	Note          string `json:"note"`
}

// DispatchRecord is one input document's provenance.
type DispatchRecord struct {
	Role          string            `json:"role"`
	RunID         string            `json:"run_id"`
	Dir           string            `json:"dir"`
	ResultsSHA256 string            `json:"results_json_sha256"`
	HarnessCommit string            `json:"harness_commit"`
	Profile       string            `json:"profile"`
	LoadgenVer    string            `json:"loadgen_version"`
	GoToolchain   string            `json:"go_toolchain"`
	CelerisPin    string            `json:"celeris_version"`
	GitRef        string            `json:"git_ref"`
	SUTHost       string            `json:"sut_host"`
	Kernel        string            `json:"kernel"`
	BootID        string            `json:"boot_id"`
	BootCmdline   string            `json:"boot_cmdline,omitempty"`
	Sources       map[string]string `json:"meta_sources"`
	StartedAt     string            `json:"started_at"`
	FinishedAt    string            `json:"finished_at"`
	Duration      string            `json:"duration"`
	Warmup        string            `json:"warmup"`
	Runs          int               `json:"runs"`
	Competitors   []string          `json:"competitors"`
	Columns       []string          `json:"columns"`
}

// TwinRecord says what became of the v1.5.11 twin columns.
type TwinRecord struct {
	Mode     string   `json:"mode"`
	Columns  []string `json:"columns,omitempty"`
	Sidecars []string `json:"sidecars,omitempty"`
}

func dispatchRecord(d *Dispatch) DispatchRecord {
	var cols []string
	for _, b := range d.Doc.Benchmarks {
		cols = append(cols, b.Name)
	}
	sort.Strings(cols)
	comp := append([]string(nil), cols...)
	if len(d.BenchVars) > 0 {
		comp = nil
		for k := range d.BenchVars {
			comp = append(comp, k)
		}
		sort.Strings(comp)
	}
	toolchain := ""
	for _, b := range d.Doc.Benchmarks {
		if b.Language == "go" {
			toolchain = b.LanguageVersion
			break
		}
	}
	bc := d.Doc.BenchmarkConfig
	return DispatchRecord{
		Role: d.Role, RunID: d.Meta.RunID, Dir: d.Dir, ResultsSHA256: sha256Hex(d.DocRaw),
		HarnessCommit: d.Meta.HarnessCommit, Profile: d.Meta.Profile,
		LoadgenVer: bc.LoadgenVer, GoToolchain: toolchain, CelerisPin: bc.CelerisVer, GitRef: bc.GitRef,
		SUTHost: d.Meta.SUTHost, Kernel: d.Meta.Kernel, BootID: d.Meta.BootID, BootCmdline: d.Meta.BootCmdline, Sources: d.Meta.Sources,
		StartedAt: bc.StartedAt.UTC().Format("2006-01-02T15:04:05Z"), FinishedAt: bc.FinishedAt.UTC().Format("2006-01-02T15:04:05Z"),
		Duration: bc.Duration.String(), Warmup: bc.Warmup.String(), Runs: bc.Runs,
		Competitors: comp, Columns: cols,
	}
}
