package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// WriteOutput writes the merged results directory. out must not exist or be
// an empty directory. The layout is what `mage Publish` consumes with
// PUBLISH_RESULTS=<out>/results.json:
//
//	results.json, timeseries.json.gz   the merged document and its sidecar
//	bench-vars.json                    union of competitor_columns
//	raw/d1-<f>.json, raw/d2-<f>.json   both dispatches' raw payloads
//	<TS>-bench-<host>/<RR>-<col>/      both dispatches' column dirs (the
//	                                   runner rollup results.json only), so the
//	                                   integrity audit sees both
//	COMPOSITION.json                   provenance + anchor drift + tie_margin
//	publish-extras/composition.json    the published part of COMPOSITION.json (+ twin.json
//	                                   with --twin=separate): `mergedocs install` puts them
//	                                   next to summary.json in the docs cell
//	twin-comparison.json, twin-rows.json  with --twin=separate
func WriteOutput(res *Result, out string) error {
	if es, err := os.ReadDir(out); err == nil && len(es) > 0 {
		return fmt.Errorf("--out %s exists and is not empty", out)
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, "results.json"), res.ResultsJSON, 0o644); err != nil {
		return err
	}
	if res.TS != nil {
		gz, err := res.TS.MarshalGzip()
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(out, "timeseries.json.gz"), gz, 0o644); err != nil {
			return err
		}
	}
	if res.BenchVars != nil {
		b, err := json.MarshalIndent(map[string]any{"competitor_columns": res.BenchVars}, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(out, "bench-vars.json"), b, 0o644); err != nil {
			return err
		}
	}
	for i, d := range []*Dispatch{res.C, res.P} {
		prefix := fmt.Sprintf("d%d-", i+1)
		if len(d.RawFiles) > 0 {
			if err := os.MkdirAll(filepath.Join(out, "raw"), 0o755); err != nil {
				return err
			}
		}
		for _, f := range d.RawFiles {
			if err := copyFile(filepath.Join(d.Dir, "raw", f), filepath.Join(out, "raw", prefix+f)); err != nil {
				return err
			}
		}
		for _, bd := range d.BenchDirs {
			for _, cd := range bd.Columns {
				dst := filepath.Join(out, bd.Name, cd.Name)
				if err := os.MkdirAll(dst, 0o755); err != nil {
					return err
				}
				if cd.HasRollup { // a column without its rollup stays without it: the audit must flag it
					if err := copyFile(cd.Rollup, filepath.Join(dst, "results.json")); err != nil {
						return err
					}
				}
			}
		}
	}
	if res.Twin != nil {
		if err := writeJSON(filepath.Join(out, "twin-comparison.json"), res.Twin.Comparison); err != nil {
			return err
		}
		if err := writeJSON(filepath.Join(out, "twin-rows.json"), res.Twin.Rows); err != nil {
			return err
		}
	}
	if err := writePublicExtras(res, filepath.Join(out, "publish-extras")); err != nil {
		return err
	}
	return writeJSON(filepath.Join(out, "COMPOSITION.json"), res.Comp)
}

func writeJSON(p string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, append(b, '\n'), 0o644)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	o, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(o, in); err != nil {
		_ = o.Close()
		return err
	}
	return o.Close()
}
