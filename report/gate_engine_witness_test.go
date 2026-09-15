package report

import (
	"strings"
	"testing"
)

// Every must-stay-zero engine counter fails its cell, and the violation
// prints the defect rather than the counter's name. A reader acting on a
// gate report should not have to know the codebase to know what fired.
func TestGate_AnyNonzeroEngineWitnessFailsTheCell(t *testing.T) {
	base := GateOptions{RequireTier3: true, RequireProperties: true}

	for key, meaning := range ZeroWitnessMeaning {
		c := cleanCell("kitchen_sink", "iouring", "amd64")
		c.Tier1.EngineZeroWitness = map[string]int64{key: 1}

		v := Gate([]ValidationCellResult{c}, nil, base)
		if len(v) != 1 {
			t.Errorf("%s: want exactly one violation, got %v", key, v)
			continue
		}
		if v[0].Field != "tier_1."+key {
			t.Errorf("%s: violation field = %q", key, v[0].Field)
		}
		if v[0].Why != meaning {
			t.Errorf("%s: Why must be the declared meaning, got %q", key, v[0].Why)
		}
	}
}

// A clean cell records every witness at zero, and zeros must not fail it.
// The seeded map is the whole set the cell was watched against, so reading
// "present" as "fired" would redden every healthy cell in the matrix.
func TestGate_AllZeroEngineWitnessesPass(t *testing.T) {
	c := cleanCell("kitchen_sink", "iouring", "amd64")
	c.Tier1.EngineZeroWitness = map[string]int64{}
	for k := range ZeroWitnessMeaning {
		c.Tier1.EngineZeroWitness[k] = 0
	}
	if v := Gate([]ValidationCellResult{c}, nil, GateOptions{RequireTier3: true, RequireProperties: true}); len(v) != 0 {
		t.Fatalf("a clean witness set must not fail the cell, got %v", v)
	}
}

// The engine's own request counter against the walker's requests_sent,
// measured on the other side of the socket. celeris#626's real numbers are
// the fixture: epoll counted 3,089 while the walker sent 3,306,726.
func TestGate_AnEngineThatStoppedCountingRequestsFailsTheCell(t *testing.T) {
	opts := GateOptions{RequireTier3: true, RequireProperties: true}

	broken := cleanCell("driver_memcached", "epoll", "amd64")
	broken.Tier1.EngineRequestsTotal = 3_089
	broken.Tier1.RequestsSent = 3_306_726
	v := Gate([]ValidationCellResult{broken}, nil, opts)
	if len(v) != 1 {
		t.Fatalf("want one violation, got %v", v)
	}
	if v[0].Field != "tier_1.engine_requests_total" {
		t.Errorf("violation field = %q", v[0].Field)
	}
	for _, want := range []string{"3089", "3306726", "celeris#626"} {
		if !strings.Contains(strings.ReplaceAll(v[0].Why, ",", ""), want) {
			t.Errorf("Why must carry %q, got %q", want, v[0].Why)
		}
	}

	// A healthy cell tracks the walker closely. std reported 4,082,639
	// against 4,098,976 sent in the same run; that must pass.
	ok := cleanCell("auth_jwt_csrf", "std", "amd64")
	ok.Tier1.EngineRequestsTotal = 4_082_639
	ok.Tier1.RequestsSent = 4_098_976
	if v := Gate([]ValidationCellResult{ok}, nil, opts); len(v) != 0 {
		t.Fatalf("a healthy counter must pass, got %v", v)
	}

	// And a refapp that answers one walker operation with several
	// requests runs ABOVE the walker's count. static_swagger_proxy
	// reported 18,880,600 against 15,801,942 sent. The check must not
	// mistake that for a defect.
	above := cleanCell("static_swagger_proxy", "epoll", "amd64")
	above.Tier1.EngineRequestsTotal = 18_880_600
	above.Tier1.RequestsSent = 15_801_942
	if v := Gate([]ValidationCellResult{above}, nil, opts); len(v) != 0 {
		t.Fatalf("an engine counting above the walker must pass, got %v", v)
	}
}

// A cell that published no engine counter at all must not be failed for it:
// absent is not the same as broken, and an older refapp build reports zero
// for everything.
func TestGate_AMissingEngineRequestCounterIsNotAFailure(t *testing.T) {
	c := cleanCell("kitchen_sink", "iouring", "amd64")
	c.Tier1.EngineRequestsTotal = 0
	c.Tier1.RequestsSent = 3_306_726
	if v := Gate([]ValidationCellResult{c}, nil, GateOptions{RequireTier3: true, RequireProperties: true}); len(v) != 0 {
		t.Fatalf("an unpublished counter must not fail the cell, got %v", v)
	}
}
