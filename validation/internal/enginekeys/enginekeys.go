// Package enginekeys is the root module's view of what the refapps publish
// from celeris's engine.EngineMetrics.
//
// The root module does not depend on celeris, and must not start to: a second
// celeris pin could drift from the one the refapps build against, and a guard
// reflecting over the wrong struct would pass while the cluster runs another.
// Only validation/refapp/internal/debugvars compiles against the pinned
// celeris, and that module is a separate module under an internal path, so
// nothing in the root module can import it either.
//
// So the boundary is crossed with a file. engine_metrics_keys.txt lists every
// EngineMetrics scalar field and the /debug/vars key it is published under. It
// is GENERATED and CHECKED by TestEngineKeysManifestMatchesEngineMetrics in the
// debugvars module, which walks the struct by reflection, so a field added in
// celeris fails there until this file is regenerated. The root module's guards
// read it through this package and hold every hop after the publisher to it:
// ParseDebugVars, properties.Snapshot, the per-cell series and the end-of-cell
// tally (probatorium#391). Each hop used to be its own hand-list, and twenty
// published keys reached none of them.
package enginekeys

import (
	_ "embed"
	"fmt"
	"slices"
	"strings"
)

//go:embed engine_metrics_keys.txt
var manifest string

// Floor is how many EngineMetrics scalar fields celeris carried when the
// manifest was introduced (celeris 9aa258c). A manifest shorter than this is
// truncated, not a celeris that shrank: removing a field fails the debugvars
// guard first, and the floor moves with it on purpose.
const Floor = 52

// Key is one published EngineMetrics field.
type Key struct {
	// Key is the /debug/vars key, e.g. "celeris.engine_transplant_stranded".
	Key string
	// Field is the engine.EngineMetrics field name, e.g. "TransplantStranded".
	Field string
	// Type is the field's Go type: "uint64", "int64", "int" or "float64".
	Type string
}

// Name is the key without its "celeris." prefix, which is how report's
// registries (ZeroWitnessMeaning, ErrorClasses, EngineCounters) and the
// series columns name it.
func (k Key) Name() string { return strings.TrimPrefix(k.Key, "celeris.") }

// IsFloat reports whether the field is a floating-point value, which
// checker's readInt64 would truncate.
func (k Key) IsFloat() bool { return k.Type == "float32" || k.Type == "float64" }

// All returns every published key, sorted by key. It fails on a malformed or
// truncated manifest rather than returning a short list, because every caller
// is a completeness guard and a short list is a guard that checks less while
// reporting success.
func All() ([]Key, error) {
	return parse(manifest)
}

// Sentinel is the value the completeness guards publish for keys[i]: far
// above any count a test fixture uses, spaced so no two keys share one, and
// non-integral for a float key so a parse that truncates it to an integer
// cannot pass for a read. Guards match by VALUE, not by name, so a key read
// into the wrong field -- or into two -- lands a wrong number somewhere
// instead of the right number everywhere, and no alias map is needed to
// excuse the historical names (ActiveConnections -> ActiveConns and the rest).
func Sentinel(i int, k Key) float64 {
	v := float64(1_000_003 + i*7_919)
	if k.IsFloat() {
		v += 0.5
	}
	return v
}

// SentinelDocument is a /debug/vars document carrying every key at its
// [Sentinel], plus celeris.engine so the engine block reads as present.
func SentinelDocument(keys []Key) map[string]any {
	doc := map[string]any{"celeris.engine": "io_uring"}
	for i, k := range keys {
		doc[k.Key] = Sentinel(i, k)
	}
	return doc
}

func parse(text string) ([]Key, error) {
	var out []Key
	seen := map[string]bool{}
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 3 {
			return nil, fmt.Errorf("engine_metrics_keys.txt:%d: want key<TAB>field<TAB>type, got %q", i+1, line)
		}
		k := Key{Key: parts[0], Field: parts[1], Type: parts[2]}
		if !strings.HasPrefix(k.Key, "celeris.") || k.Field == "" || k.Type == "" {
			return nil, fmt.Errorf("engine_metrics_keys.txt:%d: malformed entry %q", i+1, line)
		}
		if seen[k.Key] {
			return nil, fmt.Errorf("engine_metrics_keys.txt:%d: key %q listed twice", i+1, k.Key)
		}
		seen[k.Key] = true
		out = append(out, k)
	}
	if len(out) < Floor {
		return nil, fmt.Errorf("engine_metrics_keys.txt lists %d key(s) against a floor of %d: the manifest is truncated, not celeris", len(out), Floor)
	}
	if !slices.IsSortedFunc(out, func(a, b Key) int { return strings.Compare(a.Key, b.Key) }) {
		return nil, fmt.Errorf("engine_metrics_keys.txt is not sorted by key: it was edited by hand, regenerate it")
	}
	return out, nil
}
