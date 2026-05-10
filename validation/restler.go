package validation

import (
	"math/rand/v2"

	fuzz "github.com/google/gofuzz"
)

// RESTlerMutator is the per-value mutator the Tier-2 fuzzer uses to
// generate request payloads. It wraps [gofuzz.Fuzzer] with a PCG-seeded
// source so the same seed yields the same mutation stream — required
// for deterministic replay of an incident from Tier 2.
//
// Wave 6 lands the seed harness so callers can build mutators from a
// seed; the per-endpoint dependency-graph walker lands in wave 7 along
// with the validation-tagged celeris build.
type RESTlerMutator struct {
	fz *fuzz.Fuzzer
}

// NewRESTlerMutator constructs a mutator seeded with the given uint64.
// gofuzz wraps math/rand (v1); we tee a deterministic stream by
// pre-rolling the seed through math/rand/v2.PCG and feeding the
// 32-bit prefix into gofuzz's RandSource.
func NewRESTlerMutator(seed uint64) *RESTlerMutator {
	r := rand.New(rand.NewPCG(seed, ^seed))
	return &RESTlerMutator{
		fz: fuzz.New().RandSource(&pcgSource{r: r}).NilChance(0).NumElements(0, 4),
	}
}

// Fuzz mutates dst in place. dst must be a pointer to a value the
// gofuzz library can populate (basic types, structs, slices, maps).
func (m *RESTlerMutator) Fuzz(dst any) { m.fz.Fuzz(dst) }

// pcgSource adapts math/rand/v2's *rand.Rand to the v1 RandSource
// interface gofuzz consumes. The Seed method is a no-op — the source
// is constructed pre-seeded and any reseed would defeat the
// deterministic-replay guarantee.
type pcgSource struct{ r *rand.Rand }

func (s *pcgSource) Int63() int64 { return int64(s.r.Uint64() >> 1) }
func (s *pcgSource) Seed(int64)   {}
