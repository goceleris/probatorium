package corpus

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"
)

func TestRoundtrip(t *testing.T) {
	seeds := []Seed{
		{Value: 1, Tag: "alpha"},
		{Value: 2, Tag: "beta"},
		{Value: 0xffffffffffffffff, Tag: "max-u64"},
	}
	var buf bytes.Buffer
	if err := Write(&buf, seeds); err != nil {
		t.Fatalf("write: %v", err)
	}
	hdr, got, err := Read(&buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if hdr.Magic != Magic || hdr.Version != Version {
		t.Fatalf("bad header: %+v", hdr)
	}
	if len(got) != len(seeds) {
		t.Fatalf("len mismatch: %d vs %d", len(got), len(seeds))
	}
	for i := range got {
		if got[i] != seeds[i] {
			t.Errorf("seed[%d] = %+v; want %+v", i, got[i], seeds[i])
		}
	}
}

func TestWriteFile_ReadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corpus.bin")
	if err := WriteFile(path, InitialSeeds); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, got, err := ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != len(InitialSeeds) {
		t.Fatalf("roundtrip len mismatch: %d vs %d", len(got), len(InitialSeeds))
	}
	for i := range got {
		if got[i] != InitialSeeds[i] {
			t.Errorf("seed[%d] mismatch", i)
		}
	}
}

func TestInitialSeeds_ExactlyOneHundred(t *testing.T) {
	if got, want := len(InitialSeeds), 100; got != want {
		t.Fatalf("InitialSeeds has %d entries; want %d", got, want)
	}
}

func TestInitialSeeds_NoDuplicates(t *testing.T) {
	seen := map[uint64]string{}
	for _, s := range InitialSeeds {
		if tag, ok := seen[s.Value]; ok {
			t.Errorf("duplicate Value 0x%x (tags %q, %q)", s.Value, tag, s.Tag)
		}
		seen[s.Value] = s.Tag
	}
}

func TestInitialSeeds_NoReservedTag(t *testing.T) {
	for _, s := range InitialSeeds {
		if s.Tag == trailerTag {
			t.Errorf("seed 0x%x uses reserved trailer tag", s.Value)
		}
		if s.Tag == "" {
			t.Errorf("seed 0x%x has empty tag", s.Value)
		}
	}
}

func TestRead_RejectsBadMagic(t *testing.T) {
	// Write a corpus, then corrupt the magic bytes inside the gob
	// stream. We do it via the public encode path so the encoding
	// matches; we just rewrite the Header struct's Magic field.
	var buf bytes.Buffer
	if err := Write(&buf, []Seed{{Value: 1, Tag: "x"}}); err != nil {
		t.Fatal(err)
	}
	// Replace every printable "celeris" byte sequence with garbage to
	// trip the magic check. (This is not surgical, but it's enough to
	// invalidate the decoded Magic field.)
	corrupted := bytes.ReplaceAll(buf.Bytes(), []byte("celeris-probatorium-corpus"), []byte("nope"))
	_, _, err := Read(bytes.NewReader(corrupted))
	if err == nil {
		t.Fatal("expected magic check failure")
	}
}

func TestRead_DetectsTruncation(t *testing.T) {
	// Build a corpus where the gob stream contains many records — that
	// guarantees there is at least one truncatable record in the back
	// half. With a single-record corpus the gob trailer can ride in the
	// same LZ4 frame as the header, and chopping yields a frame error
	// rather than a clean missing-trailer signal.
	seeds := make([]Seed, 32)
	for i := range seeds {
		seeds[i] = Seed{Value: uint64(i + 1), Tag: "x"}
	}
	var buf bytes.Buffer
	if err := Write(&buf, seeds); err != nil {
		t.Fatal(err)
	}
	full := buf.Bytes()
	// Lop off the trailing 16 bytes — enough to clip the final frame's
	// end-of-stream marker and the trailer record itself.
	short := full[:len(full)-16]
	_, _, err := Read(bytes.NewReader(short))
	if err == nil {
		t.Fatal("expected truncation to surface as an error")
	}
	// Either ErrTruncated (clean EOF without trailer) or a wrapped
	// EOF from gob/lz4 is acceptable signal of damage.
	if !errors.Is(err, ErrTruncated) && !bytes.Contains([]byte(err.Error()), []byte("EOF")) && !bytes.Contains([]byte(err.Error()), []byte("lz4")) {
		t.Logf("note: truncation surfaced as %v", err)
	}
}

func TestWrite_RejectsReservedTag(t *testing.T) {
	var buf bytes.Buffer
	err := Write(&buf, []Seed{{Value: 1, Tag: "<EOF>"}})
	if err == nil {
		t.Fatal("expected rejection")
	}
}
