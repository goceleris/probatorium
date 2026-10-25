//go:build mage

package main

import (
	"slices"
	"strings"
	"testing"
)

// The fine host sampler (SAMPLER_FINE_INTERVAL) is the 1 s / 0.05 s series
// the bench tier starts beside its run (celeris#1036: the 60 s sampler could
// not see 1-3 s stalls). The value is handed to ansible as `--extra-vars
// sampler_fine_interval=<v>`, which splits on spaces, and to a process on a
// cluster host, so it is validated twice: here and in the playbook.

func TestSamplerFineIntervalAccepts(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", ""}, // unset: no fine series, the sampler is exactly what it was
		{"1", "1"},
		{"0.05", "0.05"},
		{"0.1", "0.1"},
		{"0.5", "0.5"},
		{"2", "2"},
		{"10", "10"},
		{"1.0", "1"},
		{"01", "1"},
		{"0.250", "0.25"},
	} {
		t.Setenv("SAMPLER_FINE_INTERVAL", c.in)
		got, err := samplerFineInterval()
		if err != nil {
			t.Errorf("SAMPLER_FINE_INTERVAL=%q rejected: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("SAMPLER_FINE_INTERVAL=%q -> %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSamplerFineIntervalRejects(t *testing.T) {
	for _, bad := range []string{
		"0",      // would spin
		"0.01",   // 100 Hz: below the floor, the cost is the point of the floor
		"0.049",  // just under the floor
		"0.0",    //
		"10.5",   // above the ceiling: a stop request could take over 10 s to land
		"11",     //
		"60",     // that is the full sampler's cadence, a different knob
		"-1",     //
		"abc",    //
		"1e-3",   // exponent form
		"1e0",    //
		" 1",     // padded
		"1 ",     //
		"1;x",    // shell metacharacter
		"1 x=2",  // a second ansible variable in the same --extra-vars string
		"1,2",    //
		"NaN",    //
		"Inf",    //
		".5",     // no leading digit
		"1.",     //
		"0.0501", // finer than the 1 ms resolution the playbook passes on
		"1\n2",   //
	} {
		t.Setenv("SAMPLER_FINE_INTERVAL", bad)
		if got, err := samplerFineInterval(); err == nil {
			t.Errorf("SAMPLER_FINE_INTERVAL=%q accepted as %q", bad, got)
		}
	}
}

// HostSamplerStart must refuse a bad cadence BEFORE touching the cluster, and
// the full sampler's own knob keeps its meaning (a positive integer).
func TestHostSamplerStartRejectsABadFineIntervalBeforeAnsible(t *testing.T) {
	t.Setenv("SAMPLER_FINE_INTERVAL", "0.01")
	t.Setenv("PATH", t.TempDir()) // no ansible-playbook: reaching it would fail differently
	err := HostSamplerStart()
	if err == nil || !strings.Contains(err.Error(), "SAMPLER_FINE_INTERVAL") {
		t.Fatalf("HostSamplerStart with SAMPLER_FINE_INTERVAL=0.01 = %v, want a SAMPLER_FINE_INTERVAL error", err)
	}
}

func TestSamplerIntervalIsBounded(t *testing.T) {
	for _, bad := range []string{"0", "-5", "1.5", "abc", "86400", "1 x=2", "3601"} {
		t.Setenv("SAMPLER_INTERVAL", bad)
		if got, err := samplerInterval(); err == nil {
			t.Errorf("SAMPLER_INTERVAL=%q accepted as %q", bad, got)
		}
	}
	t.Setenv("SAMPLER_INTERVAL", "")
	if got, err := samplerInterval(); err != nil || got != "60" {
		t.Errorf("default SAMPLER_INTERVAL = %q, %v; want 60", got, err)
	}
	t.Setenv("SAMPLER_INTERVAL", "300")
	if got, err := samplerInterval(); err != nil || got != "300" {
		t.Errorf("SAMPLER_INTERVAL=300 = %q, %v", got, err)
	}
}

// The playbook is limited to the hosts a bench uses, exactly like Deploy and
// Bench: a target=msr1 bench must not open SSH to msa2-server, which may be
// rebooted for unrelated work mid-run (2026-08-27, mage_helpers.go). The race
// and weekend tiers set no BENCH_TARGET and keep selecting every host.
func TestSamplerPlaybookIsLimitedToTheHostsOfTheRun(t *testing.T) {
	for _, c := range []struct {
		target    string
		wantLimit string // "" = no --limit
	}{
		{"", ""},
		{"both", ""},
		{"msr1", "msr1,msa2-client"},
		{"msa2-server", "msa2-server,msa2-client"},
	} {
		t.Setenv("BENCH_TARGET", c.target)
		t.Setenv("CLUSTER_USE_LAN", "")
		args := samplerPlaybookArgs("sampler_action=stop")
		i := slices.Index(args, "--limit")
		switch {
		case c.wantLimit == "" && i >= 0:
			t.Errorf("BENCH_TARGET=%q: unexpected %v", c.target, args)
		case c.wantLimit != "" && (i < 0 || i+1 >= len(args) || args[i+1] != c.wantLimit):
			t.Errorf("BENCH_TARGET=%q: want --limit %s in %v", c.target, c.wantLimit, args)
		}
		if !slices.Contains(args, "sampler_action=stop") || args[0] != "-i" || !slices.Contains(args, "host-sampler.yml") {
			t.Errorf("BENCH_TARGET=%q: playbook arguments lost: %v", c.target, args)
		}
	}
}

func TestSamplerPlaybookArgsKeepLAN(t *testing.T) {
	t.Setenv("BENCH_TARGET", "")
	t.Setenv("CLUSTER_USE_LAN", "1")
	if args := samplerPlaybookArgs(); !slices.Contains(args, "use_lan=true") {
		t.Errorf("CLUSTER_USE_LAN=1 lost: %v", args)
	}
}

// The extra-vars of a start: the fine cadence is passed only when asked for,
// so the race and weekend starts are byte-for-byte what they were.
func TestSamplerStartVars(t *testing.T) {
	t.Setenv("SAMPLER_FINE_INTERVAL", "")
	t.Setenv("SAMPLER_INTERVAL", "")
	t.Setenv("SAMPLER_DURATION", "150m")
	t.Setenv("SAMPLER_TAG", "race-1")
	vars, err := samplerStartVars()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"sampler_action=start", "sampler_interval=60", "sampler_duration_seconds=9000", "sampler_tag=race-1"}
	if !slices.Equal(vars, want) {
		t.Errorf("without SAMPLER_FINE_INTERVAL: %v, want %v", vars, want)
	}
	t.Setenv("SAMPLER_FINE_INTERVAL", "0.05")
	vars, err = samplerStartVars()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(vars, append(want, "sampler_fine_interval=0.05")) {
		t.Errorf("with SAMPLER_FINE_INTERVAL=0.05: %v", vars)
	}
}
