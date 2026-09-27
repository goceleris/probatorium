package report

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// archDoc gzips a TimeseriesDoc labelled arch holding one series per
// "scenario/server" key (a key given twice yields two series for it).
func archDoc(t *testing.T, arch string, cells ...string) []byte {
	t.Helper()
	d := &TimeseriesDoc{
		GeneratedAt:   time.Date(2026, 8, 29, 4, 0, 0, 0, time.UTC),
		SchemaVersion: TimeseriesSchemaVersion,
		Arch:          arch,
	}
	for _, c := range cells {
		scn, srv, _ := strings.Cut(c, "/")
		d.Scenarios = append(d.Scenarios, BuildScenarioSeries(scn, srv, "", nil))
	}
	b, err := d.MarshalGzip()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// TestCheckTimeseriesForArch pins the publish-time refusal for
// probatorium#422: a sidecar goes into an arch dir only when it can be that
// one machine's series. The two refusal reasons are independent, so each
// has a case where it is the ONLY thing wrong.
func TestCheckTimeseriesForArch(t *testing.T) {
	cases := []struct {
		name    string
		tsGz    []byte
		arch    string
		wantErr string // "" = accepted
	}{
		{"unlabelled, one series per cell (in-process runner, pre-#422 single arch)",
			archDoc(t, "", "get-json/celeris", "post-4k/celeris", "get-json/gin-h1"), "x86_64", ""},
		{"labelled with this arch",
			archDoc(t, "arm64", "get-json/celeris", "get-json/gin-h1"), "arm64", ""},
		{"labelled with the other arch, cells unique",
			archDoc(t, "x86_64", "get-json/celeris"), "arm64", `labelled arch "x86_64", refusing to publish it under "arm64"`},
		{"unlabelled, a cell twice (the v1.5.8/20260829 shape)",
			archDoc(t, "", "get-json/celeris", "get-json/celeris", "get-json/gin-h1"), "x86_64",
			"holds 3 series for 2 (scenario, server) cells: 1 cells appear more than once (first: get-json/celeris)"},
		{"labelled with this arch, a cell twice",
			archDoc(t, "arm64", "get-json/gin-h1", "get-json/gin-h1"), "arm64", "(first: get-json/gin-h1)"},
		{"same server in two scenarios is not a duplicate",
			archDoc(t, "", "get-json/celeris", "post-4k/celeris"), "x86_64", ""},
		{"not gzip", []byte("{}"), "x86_64", "decode timeseries.json.gz"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := CheckTimeseriesForArch(c.tsGz, c.arch)
			switch {
			case c.wantErr == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case c.wantErr != "" && err == nil:
				t.Fatalf("accepted, want an error containing %q", c.wantErr)
			case c.wantErr != "" && !strings.Contains(err.Error(), c.wantErr):
				t.Fatalf("error %q does not contain %q", err, c.wantErr)
			}
		})
	}
}

// TestWriteTreeRefusesAMixedTimeseries: a sidecar CheckTimeseriesForArch
// refuses fails WriteTree before any file of the cell exists, so a refused
// publish leaves no half-written arch dir behind for the docs sync to find.
func TestWriteTreeRefusesAMixedTimeseries(t *testing.T) {
	doc := splitSampleDocument()
	meta := splitTestMeta() // x86_64

	for name, tsGz := range map[string][]byte{
		"a cell twice":   archDoc(t, "", "get-json/celeris", "get-json/celeris"),
		"the other arch": archDoc(t, "arm64", "get-json/celeris"),
		"undecodable":    []byte("not gzip"),
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			_, err := WriteTree(root, doc, tsGz, meta)
			if err == nil {
				t.Fatal("WriteTree accepted the sidecar")
			}
			if !strings.Contains(err.Error(), "refusing timeseries.json.gz for "+CellRelDir(meta)) {
				t.Errorf("error %q does not name the refused cell", err)
			}
			if _, statErr := os.Stat(filepath.Join(root, CellRelDir(meta))); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("refused WriteTree left the cell dir behind (stat: %v)", statErr)
			}
		})
	}

	// The same doc labelled with the cell's own arch goes through verbatim.
	root := t.TempDir()
	ok := archDoc(t, meta.Arch, "get-json/celeris")
	cell, err := WriteTree(root, doc, ok, meta)
	if err != nil {
		t.Fatalf("WriteTree refused a sidecar labelled %s: %v", meta.Arch, err)
	}
	got, err := os.ReadFile(filepath.Join(cell, TimeseriesFile))
	if err != nil || string(got) != string(ok) {
		t.Errorf("accepted sidecar not copied verbatim (err %v)", err)
	}
}

// TestTimeseriesArchRoundTrips: the label survives the gzip round trip, and
// an unlabelled doc marshals without the key, so a pre-#422 reader sees
// exactly the bytes it always did.
func TestTimeseriesArchRoundTrips(t *testing.T) {
	var d TimeseriesDoc
	if err := d.UnmarshalGzip(archDoc(t, "arm64", "get-json/celeris")); err != nil {
		t.Fatal(err)
	}
	if d.Arch != "arm64" {
		t.Errorf("Arch = %q after round trip, want arm64", d.Arch)
	}
	raw, err := gunzipForTest(archDoc(t, "", "get-json/celeris"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"arch"`) {
		t.Errorf("unlabelled doc marshals an arch key: %s", raw)
	}
}

// gunzipForTest returns the raw JSON inside a gzip sidecar, so a test can
// look at the bytes a reader gets rather than at a re-marshalled struct.
func gunzipForTest(b []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer func() { _ = zr.Close() }()
	return io.ReadAll(zr)
}
