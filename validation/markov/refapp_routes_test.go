package markov

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestMarkovPaths_MatchRefappRoutes is the canary that catches the
// class of bug discovered after the 25977080887 nightly: a refapp's
// Markov yaml references endpoint paths that the refapp doesn't
// actually register, so the Tier 1 walker silently 404s — generating
// "traffic" that exercises nothing real.
//
// Strategy:
//
//  1. Parse each validation/markov/<slug>.yaml + the matching
//     validation/refapp/<slug>/*.go source tree.
//  2. Extract every route path the refapp registers (srv.GET/POST/...
//     plus apiGroup.* / keyGroup.* / admin.* etc.). Group-mounted
//     paths get their prefix prepended via a tiny static-analysis
//     of `srv.Group("/api", ...)`.
//  3. Extract the `request:` path and the top-level `login:` path
//     for every state in the matrix.
//  4. Assert every Markov path is reachable via at least one
//     registered route — with parameter wildcards (`:id`, `*filepath`)
//     interpreted leniently.
//
// Refapps with backing-service dependencies (driver_postgres /
// driver_redis / driver_memcached) are checked the same way — the
// routes register at startup regardless of whether the driver is
// reachable; only request handling differs.
//
// The check is purely lexical (no boot) so it runs in ms.
func TestMarkovPaths_MatchRefappRoutes(t *testing.T) {
	yamls, err := filepath.Glob("*.yaml")
	if err != nil {
		t.Fatalf("glob yamls: %v", err)
	}
	for _, y := range yamls {
		slug := strings.TrimSuffix(filepath.Base(y), ".yaml")
		refappDir := filepath.Join("..", "refapp", slug)
		t.Run(slug, func(t *testing.T) {
			if _, err := os.Stat(refappDir); err != nil {
				t.Skipf("no refapp dir at %s — yaml lives without a matching refapp", refappDir)
			}
			m, err := LoadMatrixFile(y)
			if err != nil {
				t.Fatalf("load matrix: %v", err)
			}
			registered, err := extractRegisteredRoutes(refappDir)
			if err != nil {
				t.Fatalf("extract routes: %v", err)
			}
			if len(registered) == 0 {
				t.Fatalf("no routes extracted from %s — extractor pattern needs an update", refappDir)
			}

			check := func(label, method, path string) {
				if path == "" {
					return
				}
				if !pathMatchesAny(method, path, registered) {
					t.Errorf("%s: %s %s does not match any route registered in %s\nknown routes:\n  %s",
						label, method, path, refappDir,
						strings.Join(routesToString(registered), "\n  "))
				}
			}
			if m.Login.Path != "" {
				check("login directive", m.Login.Method, m.Login.Path)
			}
			for _, state := range sortedKeys(m.Requests) {
				r := m.Requests[state]
				check("state "+state, r.Method, r.Path)
			}
		})
	}
}

// extractRegisteredRoutes scans every .go file under refappDir and
// returns the set of (method, path) routes the refapp registers. It
// understands the four canonical patterns used in the refapps:
//
//	srv.GET("/path", ...)           → (GET, /path)
//	apiGroup.GET("/sub", ...)       → (GET, $apiGroup_prefix/sub)
//	srv.Use(healthcheck.New())      → (GET, /healthz) + (GET, /livez)
//	srv.Use(redirect.RemoveTrailingSlashRedirect()) → no routes
//
// Group prefixes are resolved by a sibling pass that matches
//
//	<ident> := srv.Group("/prefix",
//
// declarations and substitutes them into <ident>.METHOD calls.
func extractRegisteredRoutes(dir string) ([]route, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var sources []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		sources = append(sources, string(b))
	}
	src := strings.Join(sources, "\n")

	// Pass 1 — resolve group prefixes.
	groupPrefix := map[string]string{}
	groupRe := regexp.MustCompile(`(\w+)\s*:?=\s*srv\.Group\(\s*"([^"]+)"`)
	for _, mt := range groupRe.FindAllStringSubmatch(src, -1) {
		groupPrefix[mt[1]] = mt[2]
	}

	// Pass 2 — extract every method registration.
	out := []route{}
	verbs := `(GET|POST|PUT|DELETE|PATCH|HEAD|OPTIONS)`
	callRe := regexp.MustCompile(`(\w+)\.` + verbs + `\(\s*"([^"]+)"`)
	for _, mt := range callRe.FindAllStringSubmatch(src, -1) {
		recv, method, path := mt[1], mt[2], mt[3]
		if recv != "srv" {
			if pref, ok := groupPrefix[recv]; ok {
				path = pref + path
			} else {
				// Not a known dispatcher — skip (e.g. internal helpers).
				continue
			}
		}
		out = append(out, route{Method: method, Path: path})
	}

	// Pass 3 — known middleware that registers routes silently.
	if strings.Contains(src, "healthcheck.New(") {
		out = append(out,
			route{Method: "GET", Path: "/healthz"},
			route{Method: "GET", Path: "/livez"},
		)
	}
	if strings.Contains(src, "healthcheck.NewWithOptions(") {
		out = append(out,
			route{Method: "GET", Path: "/healthz"},
			route{Method: "GET", Path: "/livez"},
		)
	}

	return out, nil
}

type route struct{ Method, Path string }

// pathMatchesAny returns true if (method, path) is reachable via any
// registered route. Wildcards `:id` and `*filepath` are treated as
// matching any single path segment / any tail respectively.
func pathMatchesAny(method, path string, routes []route) bool {
	for _, r := range routes {
		if !strings.EqualFold(r.Method, method) {
			continue
		}
		if pathSegmentsMatch(r.Path, path) {
			return true
		}
	}
	return false
}

// pathSegmentsMatch compares a route pattern (may contain `:param`
// and `*catchAll`) against a concrete request path.
func pathSegmentsMatch(pattern, concrete string) bool {
	if pattern == concrete {
		return true
	}
	pp := strings.Split(strings.TrimPrefix(pattern, "/"), "/")
	cp := strings.Split(strings.TrimPrefix(concrete, "/"), "/")
	for i, p := range pp {
		if strings.HasPrefix(p, "*") {
			// Catch-all consumes the rest.
			return true
		}
		if i >= len(cp) {
			return false
		}
		if strings.HasPrefix(p, ":") {
			continue // any non-empty segment matches a :param
		}
		if p != cp[i] {
			return false
		}
	}
	return len(pp) == len(cp)
}

func sortedKeys(m map[string]Request) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func routesToString(rs []route) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Method+" "+r.Path)
	}
	sort.Strings(out)
	return out
}
