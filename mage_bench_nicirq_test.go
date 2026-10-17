//go:build mage

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/probatorium/report"
)

// msr1Placement is the record ansible/tasks/nic_irq_placement.yml writes next to the msr1 run
// (goceleris/celeris#1036), in the shape ansible/files/nic_irq_pin.sh prints.
const msr1Placement = `{
  "host": "msr1", "interface": "enp1s0", "vector": "enp1s0-8", "cpu": 1, "changed": true, "verified": true,
  "irqs": [{"irq": 167, "before": {"affinity": "0-11", "effective": "8"}, "after": {"affinity": "1", "effective": "1"}}],
  "at_end": {"interface": "enp1s0", "vector": "enp1s0-8", "cpu": 1, "changed": false, "verified": true,
             "irqs": [{"irq": 167, "before": {"affinity": "1", "effective": "1"}, "after": {"affinity": "1", "effective": "1"}}]}
}`

// TestMergeRecordsNICIRQPlacement: a bench run that applied the standing NIC interrupt placement carries it into
// environment.nic_irq_placement of that host's document (so into env.json), found by glob because the controller
// stamps each host's run dir with its own time; the other arch's document, which applied none, carries no key.
func TestMergeRecordsNICIRQPlacement(t *testing.T) {
	resultsDir := t.TempDir()
	writeTwoArchRun(t, resultsDir)
	if err := aggregatePerCellResults(resultsDir, 0); err != nil {
		t.Fatal(err)
	}
	// msr1's dir carries a different timestamp than msa2-server's, as it does on the cluster.
	if err := os.Rename(filepath.Join(resultsDir, "20260829T040000-bench-msr1"), filepath.Join(resultsDir, "20260829T040007-bench-msr1")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resultsDir, "20260829T040007-bench-msr1", nicIRQPlacementFile), []byte(msr1Placement), 0o644); err != nil {
		t.Fatal(err)
	}
	bp := benchParams{CelerisVer: "v0.0.0-test", Duration: "5s", Warmup: "1s", Runs: "1", StartedAt: time.Date(2026, 8, 29, 4, 0, 0, 0, time.UTC)}

	read := func(host, name string) (report.Document, string) {
		t.Helper()
		out, err := mergeBenchResultsFor(resultsDir, "both", bp, host, name)
		if err != nil {
			t.Fatalf("merge %s: %v", host, err)
		}
		raw, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		var doc report.Document
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		return doc, string(raw)
	}

	arm, _ := read("msr1", "results-arm64.json")
	p := arm.Environment.NICIRQPlacement
	if p == nil {
		t.Fatal("msr1 document: environment.nic_irq_placement is missing")
	}
	if p.Interface != "enp1s0" || p.Vector != "enp1s0-8" || p.CPU != 1 || !p.Verified || len(p.IRQs) != 1 ||
		p.IRQs[0].IRQ != 167 || p.IRQs[0].Before.Affinity != "0-11" || p.IRQs[0].Before.Effective != "8" ||
		p.IRQs[0].After.Affinity != "1" || p.IRQs[0].After.Effective != "1" {
		t.Errorf("msr1 placement = %+v", p)
	}
	if p.AtEnd == nil || !p.AtEnd.Verified {
		t.Errorf("msr1 placement lost its end-of-run readback: %+v", p.AtEnd)
	}

	amd, raw := read("msa2-server", "results-amd64.json")
	if amd.Environment.NICIRQPlacement != nil || strings.Contains(raw, "nic_irq_placement") {
		t.Errorf("msa2-server document must carry no placement, got %+v", amd.Environment.NICIRQPlacement)
	}
}

// TestReadNICIRQPlacementRefusesWhatItCannotTrust: the artifact must not claim a placement the run did not have,
// so a record that is unparseable or was not verified fails the merge instead of being dropped or published.
func TestReadNICIRQPlacementRefusesWhatItCannotTrust(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want string
	}{
		"not json":   {body: "{oops", want: "parse"},
		"unverified": {body: `{"host":"msr1","interface":"enp1s0","vector":"enp1s0-8","cpu":1,"verified":false,"irqs":[{"irq":167}]}`, want: "not verified"},
		"no irqs":    {body: `{"host":"msr1","interface":"enp1s0","vector":"enp1s0-8","cpu":1,"verified":true,"irqs":[]}`, want: "not verified"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			run := filepath.Join(dir, "20261017T101010-bench-msr1")
			if err := os.MkdirAll(run, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(run, nicIRQPlacementFile), []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			p, err := readNICIRQPlacement(dir, "msr1")
			if err == nil || !strings.Contains(err.Error(), tc.want) || p != nil {
				t.Fatalf("readNICIRQPlacement = %+v, %v; want an error containing %q", p, err, tc.want)
			}
		})
	}

	// No file at all is not an error: the host applied no placement.
	if p, err := readNICIRQPlacement(t.TempDir(), "msa2-server"); p != nil || err != nil {
		t.Errorf("no record: got %+v, %v; want nil, nil", p, err)
	}
	// Two runs for the host in one results dir: their cells cannot share one placement.
	dir := t.TempDir()
	for _, ts := range []string{"20261017T090000", "20261017T110000"} {
		run := filepath.Join(dir, ts+"-bench-msr1")
		if err := os.MkdirAll(run, 0o755); err != nil {
			t.Fatal(err)
		}
		body := `{"host":"msr1","interface":"enp1s0","vector":"enp1s0-8","cpu":1,"verified":true,"irqs":[{"irq":167}]}`
		if err := os.WriteFile(filepath.Join(run, nicIRQPlacementFile), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if p, err := readNICIRQPlacement(dir, "msr1"); err == nil || p != nil || !strings.Contains(err.Error(), "2 NIC IRQ placement records") {
		t.Errorf("two records: got %+v, %v; want an error", p, err)
	}
}
