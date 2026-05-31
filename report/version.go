package report

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Version comparison + docs-index parsing for new-release detection
// (probatorium#167). Lives in report/ (no build tag) so plain
// `go test ./report/` reaches it, exactly like the SLO gate — the
// mage-tagged DetectRelease target is a thin shell over these pure
// helpers.

// CompareSemver returns -1, 0, or +1 for a < b, a == b, a > b over
// vMAJOR.MINOR.PATCH tags. A leading "v" is optional and ignored. A
// trailing pre-release / build suffix (-rc1, +meta) is compared
// lexically after the numeric core, so v1.4.0 > v1.4.0-rc1 (a release
// outranks its own pre-releases), matching SemVer precedence enough for
// the docs index, which only ever carries release tags.
//
// Non-semver inputs (e.g. "dev", "") sort LOWEST: a "dev" pin compared
// against any real published tag yields -1, so DetectRelease never treats
// an un-pinned dev build as newer than a shipped release.
func CompareSemver(a, b string) int {
	na, oka := parseSemver(a)
	nb, okb := parseSemver(b)
	switch {
	case !oka && !okb:
		return strings.Compare(a, b)
	case !oka:
		return -1 // non-semver sorts lowest
	case !okb:
		return 1
	}
	for i := 0; i < 3; i++ {
		if na.core[i] != nb.core[i] {
			if na.core[i] < nb.core[i] {
				return -1
			}
			return 1
		}
	}
	// Equal numeric core: a release (no pre-release) outranks a
	// pre-release; otherwise compare the pre-release strings lexically.
	switch {
	case na.pre == "" && nb.pre == "":
		return 0
	case na.pre == "":
		return 1
	case nb.pre == "":
		return -1
	default:
		return strings.Compare(na.pre, nb.pre)
	}
}

type semver struct {
	core [3]int
	pre  string
}

// parseSemver parses "[v]MAJOR.MINOR.PATCH[-pre][+build]". Missing minor
// / patch default to 0 (so "v1" and "v1.4" parse). Returns ok=false when
// the major component isn't numeric.
func parseSemver(s string) (semver, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return semver{}, false
	}
	// Split off build metadata (+...) then pre-release (-...).
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	var pre string
	if i := strings.IndexByte(s, '-'); i >= 0 {
		pre = s[i+1:]
		s = s[:i]
	}
	parts := strings.SplitN(s, ".", 3)
	var out semver
	out.pre = pre
	for i := 0; i < 3; i++ {
		if i >= len(parts) || parts[i] == "" {
			out.core[i] = 0
			continue
		}
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			return semver{}, false
		}
		out.core[i] = n
	}
	return out, true
}

// NewestBenchmarkedVersion returns the highest version key present in the
// docs results/index.json body. The docs sync-benchmarks workflow is the
// single writer of that manifest, so it is the canonical record of what
// has already been benchmarked — DetectRelease compares the go.mod celeris
// pin against this to decide is_new_release.
//
// The index shape is tolerated loosely so a docs-side schema tweak
// doesn't break detection: the parser accepts any of
//
//	{"versions": ["v1.4.12", "v1.4.13"]}      // explicit list
//	{"versions": {"v1.4.12": {...}}}          // map keyed by version
//	{"results":  {"v1.4.12": {...}}}          // map under "results"
//	{"v1.4.12": {...}, "v1.4.13": {...}}      // bare top-level map
//
// and picks the max by CompareSemver. An empty/whitespace body returns
// ("", nil) so a cold-start index (absent on first ever publish) is
// handled by the caller as "treat as new release", not an error.
func NewestBenchmarkedVersion(indexJSON []byte) (string, error) {
	if len(strings.TrimSpace(string(indexJSON))) == 0 {
		return "", nil
	}
	keys, err := indexVersionKeys(indexJSON)
	if err != nil {
		return "", err
	}
	newest := ""
	for _, k := range keys {
		if k == "" {
			continue
		}
		if newest == "" || CompareSemver(k, newest) > 0 {
			newest = k
		}
	}
	return newest, nil
}

// indexVersionKeys extracts the set of version strings from any of the
// tolerated index layouts.
func indexVersionKeys(indexJSON []byte) ([]string, error) {
	// First try the structured shapes.
	var structured struct {
		Versions json.RawMessage `json:"versions"`
		Results  json.RawMessage `json:"results"`
	}
	if err := json.Unmarshal(indexJSON, &structured); err == nil {
		if keys, ok := rawVersionKeys(structured.Versions); ok {
			return keys, nil
		}
		if keys, ok := rawVersionKeys(structured.Results); ok {
			return keys, nil
		}
	}

	// Fall back to a bare top-level map keyed by version.
	var bare map[string]json.RawMessage
	if err := json.Unmarshal(indexJSON, &bare); err != nil {
		return nil, fmt.Errorf("parse docs index.json: %w", err)
	}
	out := make([]string, 0, len(bare))
	for k := range bare {
		out = append(out, k)
	}
	return out, nil
}

// rawVersionKeys interprets a raw JSON value as either a []string list of
// versions or a map keyed by version, returning the version keys. ok is
// false when the raw value is absent/null or neither shape.
func rawVersionKeys(raw json.RawMessage) ([]string, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return list, true
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err == nil {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		return out, true
	}
	return nil, false
}
