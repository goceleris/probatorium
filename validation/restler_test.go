package validation

import "testing"

func TestRESTlerMutator_Deterministic(t *testing.T) {
	type req struct {
		A int
		B string
		C []int
	}
	var x, y req
	NewRESTlerMutator(0x42).Fuzz(&x)
	NewRESTlerMutator(0x42).Fuzz(&y)
	if x.A != y.A || x.B != y.B || len(x.C) != len(y.C) {
		t.Fatalf("non-deterministic mutator: %+v vs %+v", x, y)
	}
	for i := range x.C {
		if x.C[i] != y.C[i] {
			t.Fatalf("non-deterministic at C[%d]: %d vs %d", i, x.C[i], y.C[i])
		}
	}
}

func TestRESTlerMutator_DifferentSeedsDiverge(t *testing.T) {
	type req struct {
		A int
		B string
	}
	var a, b req
	NewRESTlerMutator(1).Fuzz(&a)
	NewRESTlerMutator(2).Fuzz(&b)
	if a == b {
		t.Fatal("two distinct seeds produced identical fuzz output")
	}
}
