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

// TestFeatureSetForStdhttpHybridNoUpgrade pins stdhttp-hybrid's h2c
// capability claim. The Go stdlib "hybrid" mode (SetHTTP1 + SetUnencryptedHTTP2)
// accepts plain HTTP/1.1 OR prior-knowledge h2c, but it does NOT speak
// the h1→h2c upgrade handshake (101 Switching Protocols) — the
// Go stdlib's http.Server with both protocols enabled does not perform
// the h2c upgrade; that requires a custom http.Handler that wraps
// http2.Transport. Flagging H2CUpgrade=true for stdhttp-hybrid would
// schedule the auto-mix-111 scenario against it, which then DNF's with
// "h2c upgrade: server returned status 200 (expected 101)" — the same
// false-capability bug as the v3.7 chi-h2 regression. H2CUpgrade must
// be false for stdhttp-hybrid.
//
// HTTP2C must remain true (stdhttp-hybrid accepts prior-knowledge h2c).
// HTTP1 must remain true (stdhttp-hybrid accepts plain HTTP/1.1).
func TestFeatureSetForStdhttpHybridNoUpgrade(t *testing.T) {
	adv, ok := servers.Registry["stdhttp-hybrid"]
	if !ok {
		t.Skip("registry missing stdhttp-hybrid; not a regression")
	}
	fs := featureSetFor(adv, false)
	if !fs.HTTP1 {
		t.Errorf("stdhttp-hybrid: HTTP1 should be true (hybrid accepts plain H1), got %+v", fs)
	}
	if !fs.HTTP2C {
		t.Errorf("stdhttp-hybrid: HTTP2C should be true (hybrid accepts h2c prior-knowledge), got %+v", fs)
	}
	if fs.H2CUpgrade {
		t.Errorf("stdhttp-hybrid: H2CUpgrade should be FALSE (Go stdlib doesn't do h1->h2c upgrade), got %+v", fs)
	}
}

// TestFeatureSetForStdhttpH2Noupg pins stdhttp-h2's h2c-noupg engine
// classification. The stdhttp server's -engine h2c mode sets
// SetUnencryptedHTTP2(true) ONLY (no SetHTTP1(true)) — so stdhttp-h2
// is HTTP/2 prior-knowledge only and refuses HTTP/1.1 entirely. To
// match that, the registry entry's Engine field is "h2c-noupg", which
// makes featureSetFor set HTTP1=false, HTTP2C=true. This is what keeps
// the H1 scenarios (get-json, post-4k, chain-*, driver-*, etc.) from
// being scheduled against stdhttp-h2 — without this they'd all DNF
// with "zero-request cell" because the server rejects H1 at protocol
// negotiation. (v3.8 smoke test caught this: 21 of 23 cells on
// stdhttp-h2 were not_applicable, all from the same root cause.)
func TestFeatureSetForStdhttpH2Noupg(t *testing.T) {
	adv, ok := servers.Registry["stdhttp-h2"]
	if !ok {
		t.Skip("registry missing stdhttp-h2; not a regression")
	}
	if adv.Engine != "h2c-noupg" {
		t.Errorf("stdhttp-h2: Engine field must be %q (it's h2c-prior-knowledge only), got %q",
			"h2c-noupg", adv.Engine)
	}
	fs := featureSetFor(adv, false)
	if fs.HTTP1 {
		t.Errorf("stdhttp-h2: HTTP1 should be false (h2c-prior-knowledge only), got %+v", fs)
	}
	if !fs.HTTP2C {
		t.Errorf("stdhttp-h2: HTTP2C should be true (h2c prior-knowledge), got %+v", fs)
	}
	if fs.H2CUpgrade {
		t.Errorf("stdhttp-h2: H2CUpgrade should be FALSE (prior-knowledge, no upgrade), got %+v", fs)
	}
}
