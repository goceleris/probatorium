// split.go factors the canonical v5.x summary [Document] into the
// four-file on-disk tree the docs site consumes, and merges the pieces
// back. The split is lossless: [MergeSplit] of a [SplitDocument]'s
// outputs reconstructs the original Document byte-for-byte under JSON.
//
// Why split at all: a full Document is ~650 KB, dominated by the
// per-server base64 HDR histograms. The dashboard's landing page only
// needs the scalar headline numbers, so the heavy HDR blobs move to a
// gzipped sidecar fetched lazily. The four pieces are:
//
//   - summary.json          — a Document with every ServerResult's
//     HdrHistogramB64 stripped. Uncompressed; the dashboard's primary
//     fetch (~40 KB). Resource series stay here (small, co-located with
//     the summary they annotate) — only HDR is split out.
//   - histograms.json.gz    — the stripped HDR blobs ([HistogramDoc]),
//     gzipped. The heavy payload, fetched only for a latency view.
//   - timeseries.json.gz    — the existing request-rate sidecar
//     ([TimeseriesDoc]). Not produced here; copied verbatim by the
//     publisher because it is already written next to results.json.
//   - env.json              — provenance + run environment ([EnvDoc]),
//     self-describing so the tree is debuggable in isolation.
//
// The tree lives at <root>/<version>/<yyyymmdd>/<arch>/. report/ stays
// a leaf node: this file imports only the stdlib (gzip already in use
// in timeseries.go).
package report

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// HistogramSchemaVersion identifies the histograms.json.gz sidecar. It
// versions independently of [SchemaVersion] (the summary schema) and
// [TimeseriesSchemaVersion].
const HistogramSchemaVersion = "histograms/1"

// EnvSchemaVersion identifies the env.json sidecar.
const EnvSchemaVersion = "env/1"

// Tree file names, fixed by the docs ingest contract.
const (
	SummaryFile    = "summary.json"
	HistogramsFile = "histograms.json.gz"
	TimeseriesFile = "timeseries.json.gz"
	EnvFile        = "env.json"
)

// HistogramDoc is the heavy sidecar: the per-(server, scenario) merged
// HDR histograms lifted out of every [ServerResult.HdrHistogramB64].
// Histograms maps server name → scenario name → V2-compressed base64
// HDR, exactly as it lived on the Document.
type HistogramDoc struct {
	SchemaVersion string                       `json:"schema_version"`
	GeneratedAt   time.Time                    `json:"generated_at"`
	HostArchPair  string                       `json:"host_arch_pair"`
	Histograms    map[string]map[string]string `json:"histograms"`
}

// EnvDoc is the small, self-describing provenance sidecar. It mirrors
// the manifest's per-run metadata plus the Document's [Environment] and
// [BenchmarkConfig] so the on-disk cell can be debugged without the
// index.
type EnvDoc struct {
	SchemaVersion   string          `json:"schema_version"`
	Version         string          `json:"version"`
	Arch            string          `json:"arch"`
	Date            string          `json:"date"`
	RunID           string          `json:"run_id"`
	GitSHA          string          `json:"git_sha,omitempty"`
	GitRef          string          `json:"git_ref,omitempty"`
	CelerisVersion  string          `json:"celeris_version,omitempty"`
	LoadgenVersion  string          `json:"loadgen_version,omitempty"`
	GeneratedAt     time.Time       `json:"generated_at"`
	Environment     Environment     `json:"environment"`
	BenchmarkConfig BenchmarkConfig `json:"benchmark_config"`
}

// SplitMeta carries the per-run provenance the tree needs that the
// Document itself does not encode (the on-disk version/date/arch/run_id
// keying and the git coordinates). The publisher fills it.
type SplitMeta struct {
	Version        string
	Arch           string
	Date           string
	RunID          string
	GitSHA         string
	GitRef         string
	CelerisVersion string
	LoadgenVersion string
	GeneratedAt    time.Time
}

// SplitDocument factors doc into its summary + histogram + env pieces.
//
// summary is a deep copy of doc with every ServerResult.HdrHistogramB64
// emptied — doc itself is never mutated. hist collects exactly those
// stripped blobs; a server/scenario with no HDR contributes nothing, so
// a histogram-free run yields an empty (but non-nil) Histograms map.
// env folds meta together with doc's Environment + BenchmarkConfig.
//
// The split is reversible: MergeSplit(summary, hist) == doc under JSON.
func SplitDocument(doc *Document, meta SplitMeta) (summary *Document, hist *HistogramDoc, env *EnvDoc) {
	summary = cloneDocument(doc)

	hist = &HistogramDoc{
		SchemaVersion: HistogramSchemaVersion,
		GeneratedAt:   meta.GeneratedAt,
		HostArchPair:  doc.HostArchPair,
		Histograms:    map[string]map[string]string{},
	}
	for i := range summary.Benchmarks {
		sr := &summary.Benchmarks[i]
		if len(sr.HdrHistogramB64) > 0 {
			byScenario := make(map[string]string, len(sr.HdrHistogramB64))
			for scn, b64 := range sr.HdrHistogramB64 {
				byScenario[scn] = b64
			}
			hist.Histograms[sr.Name] = byScenario
		}
		// Strip from the summary regardless: the field stays present
		// (an empty map, matching BuildDocument's initialisation) so the
		// summary round-trips to the same JSON shape minus the blobs.
		sr.HdrHistogramB64 = map[string]string{}
	}

	env = &EnvDoc{
		SchemaVersion:   EnvSchemaVersion,
		Version:         meta.Version,
		Arch:            meta.Arch,
		Date:            meta.Date,
		RunID:           meta.RunID,
		GitSHA:          meta.GitSHA,
		GitRef:          meta.GitRef,
		CelerisVersion:  meta.CelerisVersion,
		LoadgenVersion:  meta.LoadgenVersion,
		GeneratedAt:     meta.GeneratedAt,
		Environment:     doc.Environment,
		BenchmarkConfig: doc.BenchmarkConfig,
	}
	return summary, hist, env
}

// MergeSplit reattaches hist's histograms onto a copy of summary,
// reconstructing the original Document. summary is not mutated. A nil
// hist (or one missing a server's entry) leaves that server's
// HdrHistogramB64 as the empty map the summary already carries.
func MergeSplit(summary *Document, hist *HistogramDoc) *Document {
	out := cloneDocument(summary)
	if hist == nil {
		return out
	}
	for i := range out.Benchmarks {
		sr := &out.Benchmarks[i]
		byScenario, ok := hist.Histograms[sr.Name]
		if !ok {
			continue
		}
		merged := make(map[string]string, len(byScenario))
		for scn, b64 := range byScenario {
			merged[scn] = b64
		}
		sr.HdrHistogramB64 = merged
	}
	return out
}

// WriteTree writes the four-file cell under
// root/<version>/<yyyymmdd>/<arch>/. summary.json and env.json are
// uncompressed; histograms.json.gz is gzipped. timeseries is written
// only when tsGz is non-nil — the publisher passes the bytes of the
// existing timeseries.json.gz sidecar verbatim so the request-rate
// series is preserved without re-deriving it. Returns the cell dir.
func WriteTree(root string, doc *Document, tsGz []byte, meta SplitMeta) (string, error) {
	summary, hist, env := SplitDocument(doc, meta)

	cellDir := filepath.Join(root, meta.Version, meta.Date, meta.Arch)
	if err := os.MkdirAll(cellDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir tree cell: %w", err)
	}

	if err := writeJSONFile(filepath.Join(cellDir, SummaryFile), summary); err != nil {
		return "", err
	}
	if err := writeJSONFile(filepath.Join(cellDir, EnvFile), env); err != nil {
		return "", err
	}
	histGz, err := marshalGzipJSON(hist)
	if err != nil {
		return "", fmt.Errorf("gzip histograms: %w", err)
	}
	if err := os.WriteFile(filepath.Join(cellDir, HistogramsFile), histGz, 0o644); err != nil {
		return "", fmt.Errorf("write histograms: %w", err)
	}
	if tsGz != nil {
		if err := os.WriteFile(filepath.Join(cellDir, TimeseriesFile), tsGz, 0o644); err != nil {
			return "", fmt.Errorf("write timeseries: %w", err)
		}
	}
	return cellDir, nil
}

// cloneDocument deep-copies doc via a JSON round-trip. Correctness over
// speed: the splitter runs once per publish, not on a hot path, and a
// round-trip is the only copy guaranteed to track the schema as it
// grows without a hand-maintained field-by-field clone drifting stale.
func cloneDocument(doc *Document) *Document {
	raw, err := json.Marshal(doc)
	if err != nil {
		// Document is composed entirely of JSON-encodable types; a
		// marshal failure here is a programmer error, not a runtime one.
		panic(fmt.Sprintf("report: clone Document: %v", err))
	}
	var out Document
	if err := json.Unmarshal(raw, &out); err != nil {
		panic(fmt.Sprintf("report: clone Document: %v", err))
	}
	return &out
}

// writeJSONFile marshals v as indented JSON to path (0644).
func writeJSONFile(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", filepath.Base(path), err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	return nil
}

// marshalGzipJSON compact-marshals v then gzip-compresses it, mirroring
// TimeseriesDoc.MarshalGzip so every gz sidecar uses one encoding.
func marshalGzipJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		_ = zw.Close()
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
