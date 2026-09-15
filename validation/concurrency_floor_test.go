package validation

import (
	"testing"
	"time"
)

// The adaptive cells' walker floor wins over both the duration-tiered
// default and the operator's VALIDATE_CONCURRENCY, because it encodes the
// adaptive controller's promotion threshold rather than a preference; it
// never lowers the count.
func TestResolveConcurrency_FloorWinsOverDefaultAndOverride(t *testing.T) {
	cases := []struct {
		d     time.Duration
		env   string
		floor int
		want  int
	}{
		{150 * time.Second, "", 0, 1},
		{150 * time.Second, "30", 0, 30},
		{time.Hour, "", 0, 50},
		{150 * time.Second, "30", 60, 60},
		{time.Hour, "50", 60, 60},
		{time.Hour, "80", 60, 80},
		{10 * time.Minute, "", 0, 10},
		{10 * time.Minute, "bogus", 0, 10},
	}
	for _, c := range cases {
		if got := resolveConcurrency(c.d, c.env, c.floor); got != c.want {
			t.Errorf("resolveConcurrency(%v, %q, %d) = %d, want %d", c.d, c.env, c.floor, got, c.want)
		}
	}
}
