//go:build mage

package main

import (
	"testing"

	"github.com/goceleris/probatorium/servers"
)

// TestFeatureSetForH2CEngineNoUpgrade pins the chi-h2 / gin-h2 / echo-h2 /
// hertz-h2 / iris-h2 / stdhttp-h2 fix: their "h2c" engine uses Go's
// h2c.NewHandler (or http.Protocols.SetUnencryptedHTTP2), which only
// accepts h2c PRIOR-KNOWLEDGE — not the h1→h2c upgrade handshake the
// loadgen's -h2c-upgrade mode looks for. Flagging H2CUpgrade=true for
// these engines produced a DNF cell on every auto-mix-111 run, not a
// real signal (regression: v3.7 chi-h2/auto-mix-111 DNF'd with "h2c
// upgrade: server returned status 200 (expected 101)"). The fix:
// "h2c" without a "+upg" suffix is HTTP2C prior-knowledge only, no
// upgrade; only "auto" / "hybrid" engines (the celeris variants) get
// H2CUpgrade=true.
func TestFeatureSetForH2CEngineNoUpgrade(t *testing.T) {
	// Every (server, engine) pair we know is "h2c"-style Go net/http.
	for _, name := range []string{"chi-h2", "gin-h2", "echo-h2", "hertz-h2", "iris-h2", "stdhttp-h2"} {
		adv, ok := servers.Registry[name]
		if !ok {
			t.Skipf("registry missing %q; not a regression, just an older snapshot", name)
		}
		fs := featureSetFor(adv, false)
		if !fs.HTTP2C {
			t.Errorf("%s: HTTP2C should be true (h2c cleartext), got %+v", name, fs)
		}
		if fs.H2CUpgrade {
			t.Errorf("%s: H2CUpgrade should be FALSE (h2c.NewHandler doesn't do upgrade), got %+v", name, fs)
		}
	}
}

// TestFeatureSetForCelerisAutoUpg pins the only engine that actually
// implements the h1→h2c upgrade handshake: celeris-iouring-auto+upg-async.
// All three (HTTP2C, H2CUpgrade, Auto) must be true.
func TestFeatureSetForCelerisAutoUpg(t *testing.T) {
	adv, ok := servers.Registry["celeris-iouring-auto+upg-async"]
	if !ok {
		t.Skip("registry missing celeris-iouring-auto+upg-async; not a regression")
	}
	fs := featureSetFor(adv, false)
	if !fs.HTTP2C {
		t.Errorf("celeris-iouring-auto+upg-async: HTTP2C should be true, got %+v", fs)
	}
	if !fs.H2CUpgrade {
		t.Errorf("celeris-iouring-auto+upg-async: H2CUpgrade should be true (the only engine that does), got %+v", fs)
	}
	if !fs.Auto {
		t.Errorf("celeris-iouring-auto+upg-async: Auto should be true, got %+v", fs)
	}
}
