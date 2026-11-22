package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// publicSchema versions the published composition.json that the docs site and
// the README scripts read.
const publicSchema = "composition/1"

// Public file names inside a published cell, next to summary.json.
const (
	publicCompositionFile = "composition.json"
	publicTwinFile        = "twin.json"
)

// PublicComposition is the part of COMPOSITION.json that is published with the
// cell: no local paths, no run directories, no per-metric noise. The ratios are
// d1/d2 of the saturation rate, per anchor and scenario.
type PublicComposition struct {
	Schema         string                    `json:"schema"`
	RefusalsSHA256 string                    `json:"refusals_sha256"`
	Arch           string                    `json:"arch"`
	PublishDate    string                    `json:"publish_date"`
	Dispatches     map[string]PublicDispatch `json:"dispatches"`
	GapHours       float64                   `json:"gap_hours_between_dispatches"`
	Reboot         RebootRecord              `json:"reboot"`
	TieFloor       float64                   `json:"tie_floor"`
	TieMargin      map[string]float64        `json:"tie_margin_by_scenario"`
	MaxAnchorDrift float64                   `json:"max_anchor_drift"`
	DriftWarning   bool                      `json:"drift_warning"`
	Anchors        map[string]PublicAnchor   `json:"anchors"`
	Columns        map[string]string         `json:"columns"`
	Twin           TwinRecord                `json:"twin"`
}

// PublicDispatch is one dispatch's published provenance.
type PublicDispatch struct {
	RunID         string `json:"run_id"`
	HarnessCommit string `json:"harness_commit"`
	Kernel        string `json:"kernel"`
	StartedAt     string `json:"started_at"`
	FinishedAt    string `json:"finished_at"`
}

// PublicAnchor is one anchor's two measurements per scenario.
type PublicAnchor struct {
	MaxDrift    float64                   `json:"max_drift"`
	MedianDrift float64                   `json:"median_drift"`
	Scenarios   map[string]PublicAnchorSc `json:"scenarios"`
}

// PublicAnchorSc is one anchor scenario: d1 (candidate dispatch, the published
// value), d2 (frameworks dispatch) and the symmetric drift.
type PublicAnchorSc struct {
	D1    float64  `json:"d1"`
	D2    float64  `json:"d2"`
	Ratio *float64 `json:"ratio_d1_d2"`
	Drift float64  `json:"drift"`
}

// Public derives the published composition from the full one.
func (c *Composition) Public() PublicComposition {
	p := PublicComposition{
		Schema: publicSchema, RefusalsSHA256: c.RefusalsSHA256, Arch: c.Arch, PublishDate: c.Publish.Date,
		Dispatches: map[string]PublicDispatch{}, GapHours: c.GapHours, Reboot: c.Reboot,
		TieFloor: c.Anchors.TieMarginFloor, TieMargin: c.TieMarginByScenario, MaxAnchorDrift: c.MaxAnchorDrift,
		DriftWarning: c.Anchors.DriftWarning, Anchors: map[string]PublicAnchor{}, Columns: c.Columns, Twin: c.Twin,
	}
	for role, d := range c.Dispatches {
		p.Dispatches[role] = PublicDispatch{RunID: d.RunID, HarnessCommit: d.HarnessCommit, Kernel: d.Kernel, StartedAt: d.StartedAt, FinishedAt: d.FinishedAt}
	}
	for _, a := range c.Anchors.Per {
		pa := PublicAnchor{MaxDrift: a.MaxDrift, MedianDrift: a.MedianDrift, Scenarios: map[string]PublicAnchorSc{}}
		notOK := map[string]bool{}
		for _, k := range a.NotOK {
			notOK[k] = true
		}
		for _, mp := range a.Metrics[driftMetric] {
			if notOK[mp.Key] {
				continue
			}
			sc := PublicAnchorSc{D1: mp.D1, D2: mp.D2, Ratio: mp.Ratio}
			if mp.Ratio != nil {
				sc.Drift = symDrift(*mp.Ratio)
			}
			pa.Scenarios[mp.Key] = sc
		}
		p.Anchors[a.Name] = pa
	}
	return p
}

// writePublicExtras writes the files that must sit next to summary.json in the
// published cell: composition.json and, with --twin=separate, twin.json.
func writePublicExtras(res *Result, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, publicCompositionFile), res.Comp.Public()); err != nil {
		return err
	}
	if res.Twin != nil {
		return writeJSON(filepath.Join(dir, publicTwinFile), res.Twin.Comparison)
	}
	return nil
}

// Install copies <composed>/publish-extras/* into the published cell
// <docs>/results/<version>/<date>/<arch>/ after `mage Publish` wrote it. mage
// Publish (frozen at the F5 SHA) writes four files and knows nothing of
// composition.json, so this is a separate, explicit step. It refuses unless the
// cell's summary.json holds exactly the columns of the composition, so the extras
// cannot be attached to another cell.
func Install(composed, docs, version string) (string, error) {
	var pc PublicComposition
	b, err := os.ReadFile(filepath.Join(composed, "publish-extras", publicCompositionFile))
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(b, &pc); err != nil {
		return "", err
	}
	cell := filepath.Join(docs, "results", version, pc.PublishDate, pc.Arch)
	sb, err := os.ReadFile(filepath.Join(cell, "summary.json"))
	if err != nil {
		return "", fmt.Errorf("the published cell %s has no summary.json (run mage Publish first): %w", cell, err)
	}
	var s struct {
		Benchmarks []struct {
			Name string `json:"name"`
		} `json:"benchmarks"`
	}
	if err := json.Unmarshal(sb, &s); err != nil {
		return "", err
	}
	have := map[string]bool{}
	for _, x := range s.Benchmarks {
		have[x.Name] = true
	}
	var diff []string
	for n := range pc.Columns {
		if !have[n] {
			diff = append(diff, "missing in summary.json: "+n)
		}
	}
	for n := range have {
		if _, ok := pc.Columns[n]; !ok {
			diff = append(diff, "not in the composition: "+n)
		}
	}
	if len(diff) > 0 {
		sort.Strings(diff)
		return "", fmt.Errorf("the cell %s does not match the composition: %v", cell, diff)
	}
	es, err := os.ReadDir(filepath.Join(composed, "publish-extras"))
	if err != nil {
		return "", err
	}
	for _, e := range es {
		if err := copyFile(filepath.Join(composed, "publish-extras", e.Name()), filepath.Join(cell, e.Name())); err != nil {
			return "", err
		}
	}
	return cell, nil
}
