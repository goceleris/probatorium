package main

// L857F (celeris#857) knobs of the memcached adapter, read ONCE at mount time from the process environment:
//
//	PROBATORIUM_MC_MAXOPEN  the celeris memcached client's WithMaxOpen, a positive integer; unset or empty = 16 (what the adapter always used).
//	PROBATORIUM_MC_CLIENT   "celeris" (the in-tree driver/memcached, the default) or "gomemcache" (github.com/bradfitz/gomemcache with
//	                        MaxIdleConns 16, the same calls as servers/fasthttp, behind the SAME celeris engine and the same async routes).
//
// With both unset the adapter behaves exactly as it did before this file existed: memcached.NewClient(addr, WithMaxOpen(16), WithEngine(srv)),
// the same three celeris handlers on the same routes; the only observable difference is the `[m857]` startup line. A malformed value is a
// fatal start error (never a silent fall-back to the default arm), as is MAXOPEN together with CLIENT=gomemcache (MAXOPEN would be ignored).

import (
	"fmt"
	"log"
	"os"
	"strconv"
)

const (
	envMCMaxOpen       = "PROBATORIUM_MC_MAXOPEN"
	envMCClient        = "PROBATORIUM_MC_CLIENT"
	mcClientCeleris    = "celeris"
	mcClientGomemcache = "gomemcache"
	defaultMCMaxOpen   = 16
)

// mcKnobs is the resolved memcached configuration of this process.
type mcKnobs struct {
	client  string
	maxOpen int // the celeris WithMaxOpen value; 0 when the celeris client is not used
}

// parseMCKnobs resolves the knobs from a getenv function (testable without touching the process environment).
func parseMCKnobs(getenv func(string) string) (mcKnobs, error) {
	client := getenv(envMCClient)
	if client == "" {
		client = mcClientCeleris
	}
	if client != mcClientCeleris && client != mcClientGomemcache {
		return mcKnobs{}, fmt.Errorf("%s=%q: want %q or %q", envMCClient, client, mcClientCeleris, mcClientGomemcache)
	}
	raw := getenv(envMCMaxOpen)
	if client == mcClientGomemcache {
		if raw != "" {
			return mcKnobs{}, fmt.Errorf("%s=%q has no effect with %s=%s", envMCMaxOpen, raw, envMCClient, mcClientGomemcache)
		}
		return mcKnobs{client: client}, nil
	}
	n := defaultMCMaxOpen
	if raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 1 {
			return mcKnobs{}, fmt.Errorf("%s=%q: want a positive integer", envMCMaxOpen, raw)
		}
		n = v
	}
	return mcKnobs{client: client, maxOpen: n}, nil
}

// resolveMCKnobs reads the process environment, prints the one `[m857]` startup line the L857F validator compares with the registered arm
// (cap is the fasthttp adapter's semaphore, always 0 here), and exits on an invalid value.
func resolveMCKnobs() mcKnobs {
	k, err := parseMCKnobs(os.Getenv)
	if err != nil {
		log.Fatalf("[m857] invalid environment: %v", err)
	}
	log.Printf("[m857] client=%s maxopen=%d cap=0", k.client, k.maxOpen)
	return k
}
