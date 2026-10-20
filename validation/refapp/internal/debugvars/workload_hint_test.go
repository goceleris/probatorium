package debugvars

import (
	"testing"

	"github.com/goceleris/celeris"
)

func env(v string) func(string) string {
	return func(k string) string {
		if k == WorkloadHintEnv {
			return v
		}
		return ""
	}
}

func TestWorkloadHintFromEnv(t *testing.T) {
	for _, tc := range []struct {
		in     string
		want   celeris.WorkloadHint
		wantOK bool
		bad    bool
	}{
		{"", celeris.WorkloadUnspecified, false, false},
		{"high", celeris.WorkloadHighConcurrency, true, false},
		{" low ", celeris.WorkloadLowConcurrency, true, false},
		{"HIGH", celeris.WorkloadUnspecified, false, true},
		{"io_uring", celeris.WorkloadUnspecified, false, true},
	} {
		got, ok, err := workloadHint(env(tc.in))
		if (err != nil) != tc.bad || ok != tc.wantOK || got != tc.want {
			t.Errorf("workloadHint(%q) = %v, %v, %v; want %v, %v, bad=%v", tc.in, got, ok, err, tc.want, tc.wantOK, tc.bad)
		}
	}
}

// Defaults are today's behaviour: an unset variable leaves the config alone,
// and a refapp's own choice wins over the variable.
func TestApplyWorkloadHint(t *testing.T) {
	var cfg celeris.Config
	applyWorkloadHint(&cfg, env(""))
	if cfg.WorkloadHint != celeris.WorkloadUnspecified {
		t.Errorf("unset variable changed the hint to %v", cfg.WorkloadHint)
	}
	applyWorkloadHint(&cfg, env("high"))
	if cfg.WorkloadHint != celeris.WorkloadHighConcurrency {
		t.Errorf("high: hint = %v", cfg.WorkloadHint)
	}
	own := celeris.Config{WorkloadHint: celeris.WorkloadLowConcurrency}
	applyWorkloadHint(&own, env("high"))
	if own.WorkloadHint != celeris.WorkloadLowConcurrency {
		t.Errorf("the refapp's own hint was overridden: %v", own.WorkloadHint)
	}
}
