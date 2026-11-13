package main

import "testing"

func TestParseMCKnobs(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	for _, tc := range []struct {
		name    string
		env     map[string]string
		want    mcKnobs
		wantErr bool
	}{
		{"both unset = today's adapter (celeris client, WithMaxOpen 16)", nil, mcKnobs{client: "celeris", maxOpen: 16}, false},
		{"both empty = unset", map[string]string{envMCMaxOpen: "", envMCClient: ""}, mcKnobs{client: "celeris", maxOpen: 16}, false},
		{"maxopen 128 (arm B)", map[string]string{envMCMaxOpen: "128"}, mcKnobs{client: "celeris", maxOpen: 128}, false},
		{"maxopen 1", map[string]string{envMCMaxOpen: "1"}, mcKnobs{client: "celeris", maxOpen: 1}, false},
		{"client celeris spelled out", map[string]string{envMCClient: "celeris"}, mcKnobs{client: "celeris", maxOpen: 16}, false},
		{"gomemcache (arm G): the celeris client is not used", map[string]string{envMCClient: "gomemcache"}, mcKnobs{client: "gomemcache", maxOpen: 0}, false},
		{"maxopen 0 is refused (0 would mean the library default)", map[string]string{envMCMaxOpen: "0"}, mcKnobs{}, true},
		{"maxopen negative", map[string]string{envMCMaxOpen: "-4"}, mcKnobs{}, true},
		{"maxopen not a number", map[string]string{envMCMaxOpen: "abc"}, mcKnobs{}, true},
		{"maxopen with a space", map[string]string{envMCMaxOpen: "16 "}, mcKnobs{}, true},
		{"unknown client", map[string]string{envMCClient: "memcache"}, mcKnobs{}, true},
		{"client in the wrong case", map[string]string{envMCClient: "Celeris"}, mcKnobs{}, true},
		{"maxopen together with gomemcache is refused (it would be ignored)", map[string]string{envMCClient: "gomemcache", envMCMaxOpen: "128"}, mcKnobs{}, true},
	} {
		got, err := parseMCKnobs(env(tc.env))
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", tc.name, err, tc.wantErr)
			continue
		}
		if err == nil && got != tc.want {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
	}
}
