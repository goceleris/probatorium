package enginekeys

import (
	"strings"
	"testing"
)

// The embedded manifest is what every root-module completeness guard walks,
// so a manifest that parses short would make all of them check less while
// still passing. All refuses one.
func TestTheEmbeddedManifestParsesAtOrAboveTheFloor(t *testing.T) {
	keys, err := All()
	if err != nil {
		t.Fatal(err)
	}
	var floats int
	for _, k := range keys {
		switch k.Type {
		case "uint64", "int64", "int":
		case "float64":
			floats++
		default:
			t.Errorf("%s has Go type %q; the root-module guards only know integer and float scalars", k.Key, k.Type)
		}
		if k.Name() == k.Key {
			t.Errorf("%s: Name() did not strip the celeris. prefix", k.Key)
		}
	}
	t.Logf("manifest lists %d published EngineMetrics key(s), %d of them float", len(keys), floats)
	if len(keys) == 0 {
		t.Fatal("the manifest parsed to nothing")
	}
}

// Negative controls for the parser itself: each malformed manifest must be
// refused, not silently shortened.
func TestAMalformedManifestIsRefused(t *testing.T) {
	var good strings.Builder
	for i := range Floor {
		good.WriteString("celeris.engine_k")
		good.WriteString(string(rune('a' + i/26)))
		good.WriteString(string(rune('a' + i%26)))
		good.WriteString("\tField\tuint64\n")
	}
	if _, err := parse(good.String()); err != nil {
		t.Fatalf("a well-formed manifest of exactly the floor was refused: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(good.String()), "\n")
	for name, text := range map[string]string{
		"truncated":     strings.Join(lines[:Floor-1], "\n"),
		"duplicate key": good.String() + lines[0] + "\n",
		"unsorted":      lines[1] + "\n" + lines[0] + "\n" + strings.Join(lines[2:], "\n"),
		"two fields":    good.String() + "celeris.engine_zz\tField\n",
		"no prefix":     good.String() + "engine_zz\tField\tuint64\n",
		"empty":         "",
	} {
		if _, err := parse(text); err == nil {
			t.Errorf("%s manifest was accepted", name)
		}
	}
}
