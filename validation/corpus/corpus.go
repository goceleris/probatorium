// Package corpus persists the validation tier's seed corpus — the
// list of (seed, tag) pairs that survive triage, the failure-shrinking
// pipeline, and the always-on Tier-3 replay schedule.
//
// On-disk format is gob-encoded record stream wrapped in an LZ4 frame:
//
//	┌─ lz4 frame ───────────────────────────────────────────────────┐
//	│  gob: Header{Magic, Version, GoVersion, WrittenAt}           │
//	│  gob: Seed                                                   │
//	│  gob: Seed                                                   │
//	│  ...                                                         │
//	│  gob: trailer sentinel (Seed{Value:0, Tag:"<EOF>"})          │
//	└──────────────────────────────────────────────────────────────┘
//
// LZ4 + gob is a deliberate combo: gob is byte-stable across Go
// versions for the trivial type ([Seed] is two fields), and LZ4 frame
// compression keeps the 100-1000-element corpora cheap to ship in a
// release artifact without pulling in a heavy CBOR / Parquet stack.
package corpus

import (
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"time"

	"github.com/pierrec/lz4/v4"
)

// Magic identifies a probatorium seed corpus file. The trailing newline
// is intentional — it keeps `file(1)` from misclassifying the LZ4 frame
// as random binary when the magic happens to align with a frame header.
const Magic = "celeris-probatorium-corpus\n"

// Version is the on-disk schema version. Bump when fields move; readers
// reject unknown versions with a clear error rather than silently
// decoding garbage.
const Version uint32 = 1

// Header is the leading record in every corpus file.
type Header struct {
	Magic     string    // must equal [Magic]
	Version   uint32    // must equal [Version]
	GoVersion string    // runtime.Version() at write time, informational
	WrittenAt time.Time // wall time the corpus was authored
}

// Seed is one record. The Value is the master seed handed to the
// validator's deterministic-expand function (math/rand/v2.PCG seeded
// from Value); Tag is a free-form short string used by humans when
// triaging incident reports.
type Seed struct {
	Value uint64
	Tag   string
}

// trailerTag is the sentinel tag that marks end-of-stream in the gob
// record sequence. Decoders stop reading when they see it; writers
// always emit it as the last record.
const trailerTag = "<EOF>"

// Write encodes seeds to w as a gob+lz4 stream. The function always
// writes a leading [Header] and a trailing sentinel record so a
// partial-decode reader can tell a truncated file from a clean EOF.
func Write(w io.Writer, seeds []Seed) error {
	lz := lz4.NewWriter(w)
	defer lz.Close()
	enc := gob.NewEncoder(lz)
	hdr := Header{
		Magic:     Magic,
		Version:   Version,
		GoVersion: runtime.Version(),
		WrittenAt: time.Now().UTC(),
	}
	if err := enc.Encode(&hdr); err != nil {
		return fmt.Errorf("corpus: write header: %w", err)
	}
	for i, s := range seeds {
		if s.Tag == trailerTag {
			return fmt.Errorf("corpus: seed[%d] uses reserved tag %q", i, trailerTag)
		}
		if err := enc.Encode(&s); err != nil {
			return fmt.Errorf("corpus: write seed[%d]: %w", i, err)
		}
	}
	if err := enc.Encode(&Seed{Tag: trailerTag}); err != nil {
		return fmt.Errorf("corpus: write trailer: %w", err)
	}
	return nil
}

// WriteFile is a convenience wrapper: writes to a temp file in the
// same directory and renames into place so a crash mid-write does not
// corrupt the canonical corpus file.
func WriteFile(path string, seeds []Seed) error {
	tmp, err := os.CreateTemp(filepath_dir(path), ".corpus-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := Write(tmp, seeds); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Read decodes a gob+lz4 corpus stream from r. Returns the header and
// the seed records (excluding the trailing sentinel). A truncated
// stream (no trailer record) surfaces as ErrTruncated so callers can
// distinguish a half-written file from a clean read.
func Read(r io.Reader) (Header, []Seed, error) {
	lz := lz4.NewReader(r)
	dec := gob.NewDecoder(lz)
	var hdr Header
	if err := dec.Decode(&hdr); err != nil {
		return Header{}, nil, fmt.Errorf("corpus: read header: %w", err)
	}
	if hdr.Magic != Magic {
		return hdr, nil, fmt.Errorf("corpus: bad magic %q", hdr.Magic)
	}
	if hdr.Version != Version {
		return hdr, nil, fmt.Errorf("corpus: unsupported version %d (want %d)", hdr.Version, Version)
	}
	var (
		out      []Seed
		sawTrail bool
	)
	for {
		var s Seed
		if err := dec.Decode(&s); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return hdr, out, fmt.Errorf("corpus: read seed: %w", err)
		}
		if s.Tag == trailerTag {
			sawTrail = true
			break
		}
		out = append(out, s)
	}
	if !sawTrail {
		return hdr, out, ErrTruncated
	}
	return hdr, out, nil
}

// ReadFile is a convenience wrapper that opens path and calls [Read].
func ReadFile(path string) (Header, []Seed, error) {
	f, err := os.Open(path)
	if err != nil {
		return Header{}, nil, err
	}
	defer f.Close()
	return Read(f)
}

// ErrTruncated is returned when [Read] reaches stream EOF without
// observing the trailing sentinel record. Callers can choose to
// salvage the partial decode (out is still populated up to the
// truncation point) or treat it as a fatal corpus corruption.
var ErrTruncated = errors.New("corpus: stream truncated (no trailer)")

// filepath_dir returns filepath.Dir(p); local helper because pulling
// in path/filepath here only for one call would push the import set
// over the threshold the package needs to stay flat.
func filepath_dir(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			return p[:i]
		}
	}
	return "."
}
