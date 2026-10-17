package report

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// placementFixture is what ansible/files/nic_irq_pin.sh + the playbook record for the msr1 standing
// placement (goceleris/celeris#1036): the vector found on CPU 4/8 under the mask 0-11 and pinned to CPU 1.
func placementFixture() *NICIRQPlacement {
	vec := NICIRQVector{
		IRQ:    167,
		Before: NICIRQAffinity{Affinity: "0-11", Effective: "8"},
		After:  NICIRQAffinity{Affinity: "1", Effective: "1"},
	}
	end := NICIRQVector{
		IRQ:    167,
		Before: NICIRQAffinity{Affinity: "1", Effective: "1"},
		After:  NICIRQAffinity{Affinity: "1", Effective: "1"},
	}
	return &NICIRQPlacement{
		Host: "msr1", Interface: "enp1s0", Vector: "enp1s0-8", CPU: 1,
		Changed: true, Verified: true, IRQs: []NICIRQVector{vec},
		AtEnd: &NICIRQPlacement{Interface: "enp1s0", Vector: "enp1s0-8", CPU: 1, Verified: true, IRQs: []NICIRQVector{end}},
	}
}

func minimalDoc(t *testing.T) Document {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "v5_minimal.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc Document
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc.SchemaVersion = SchemaVersion
	return doc
}

// TestNICIRQPlacementSchemaAndEnvJSON pins the 5.21 field: it validates against schema_v5.json, survives a
// round trip, and lands in env.json (EnvDoc.Environment) through SplitDocument.
func TestNICIRQPlacementSchemaAndEnvJSON(t *testing.T) {
	t.Parallel()
	sch := loadSchema(t)
	doc := minimalDoc(t)
	doc.Environment.NICIRQPlacement = placementFixture()

	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	validateAny(t, sch, raw)

	var back Document
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back.Environment.NICIRQPlacement, doc.Environment.NICIRQPlacement) {
		t.Errorf("round trip changed the placement:\n got %+v\nwant %+v", back.Environment.NICIRQPlacement, doc.Environment.NICIRQPlacement)
	}

	_, _, env := SplitDocument(&doc, SplitMeta{Version: "v1.6.0", Arch: "arm64", Date: "20261017", RunID: DefaultRunID})
	envRaw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	var probe struct {
		Environment struct {
			P *NICIRQPlacement `json:"nic_irq_placement"`
		} `json:"environment"`
	}
	if err := json.Unmarshal(envRaw, &probe); err != nil {
		t.Fatal(err)
	}
	p := probe.Environment.P
	if p == nil || p.Interface != "enp1s0" || p.Vector != "enp1s0-8" || p.CPU != 1 || len(p.IRQs) != 1 ||
		p.IRQs[0].IRQ != 167 || p.IRQs[0].Before.Effective != "8" || p.IRQs[0].After.Effective != "1" {
		t.Errorf("env.json environment.nic_irq_placement = %+v, want enp1s0 enp1s0-8 -> CPU 1, irq 167 8 -> 1", p)
	}
}

// TestNICIRQPlacementOmittedWhenNil: a host without a placement (every amd64 run) writes no key at all,
// so an old reader and a no-placement run are indistinguishable from before 5.21.
func TestNICIRQPlacementOmittedWhenNil(t *testing.T) {
	t.Parallel()
	doc := minimalDoc(t)
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "nic_irq_placement") {
		t.Error("a document without a placement must not carry the nic_irq_placement key")
	}
	validateAny(t, loadSchema(t), raw)
}

// TestNICIRQPlacementSchemaRejectsIncomplete: the schema requires the fields that make the record readable.
func TestNICIRQPlacementSchemaRejectsIncomplete(t *testing.T) {
	t.Parallel()
	sch := loadSchema(t)
	doc := minimalDoc(t)
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m["environment"].(map[string]any)["nic_irq_placement"] = map[string]any{"interface": "enp1s0"}
	if err := sch.Validate(m); err == nil {
		t.Error("a nic_irq_placement without host/vector/cpu/verified/irqs must fail the schema")
	}
}
