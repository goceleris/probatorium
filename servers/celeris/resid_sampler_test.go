package main

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestResidGateMirrorsCeleris(t *testing.T) {
	for v, want := range map[string]bool{"": false, "0": false, "false": false, "OFF": false, " no ": false, "1": true, "on": true, "yes": true, "2": true} {
		if got := residGateOn(v); got != want {
			t.Errorf("residGateOn(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestResidEnvLine(t *testing.T) {
	for _, c := range []struct {
		raw  string
		set  bool
		want string
	}{{"", false, "[res-env] env=unset level=0"}, {"1", true, "[res-env] env=1 level=1"}, {"0", true, "[res-env] env=0 level=0"}, {"on", true, "[res-env] env=on level=1"}} {
		if got := residEnvLine(c.raw, c.set); got != c.want {
			t.Errorf("residEnvLine(%q, %v) = %q, want %q", c.raw, c.set, got, c.want)
		}
	}
}

// stop() is synchronous: nothing is emitted once it has returned, whatever ticks are still pending.
func TestResidSamplerStopsBeforeTheSignalLine(t *testing.T) {
	var mu sync.Mutex
	var out []string
	emit := func(l string) { mu.Lock(); out = append(out, l); mu.Unlock() }
	ticks := make(chan time.Time, 16)
	stop := startResidSampler(emit, func() string { return "[res] 1" }, ticks)
	ticks <- time.Now()
	ticks <- time.Now()
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(out)
		mu.Unlock()
		if n == 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	stop()
	for i := 0; i < 8; i++ {
		ticks <- time.Now()
	}
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(out) != 2 || !strings.HasPrefix(out[0], "[res] ") {
		t.Errorf("emitted %v, want exactly 2 lines before stop() and none after", out)
	}
	stop() // idempotent
}
