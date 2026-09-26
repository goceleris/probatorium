package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The workflow's dispatch defaults and every dispatch docs/STRESS.md shows
// must pass the plan's validation: a documented command that the plan
// refuses would fail at the first step, on someone's first try.

const (
	workflowPath = "../../.github/workflows/celeris-stress.yml"
	docsPath     = "../../docs/STRESS.md"
)

// workflowDefaults reads each dispatch input's default from the workflow.
func workflowDefaults(t *testing.T) map[string]string {
	t.Helper()
	b, err := os.ReadFile(filepath.FromSlash(workflowPath))
	if err != nil {
		t.Fatal(err)
	}
	block := regexp.MustCompile(`(?s)workflow_dispatch:\s*\n\s*inputs:\n(.*?)\n  pull_request:`).FindStringSubmatch(string(b))
	if block == nil {
		t.Fatal("no workflow_dispatch inputs block")
	}
	out := map[string]string{}
	name := ""
	for _, l := range strings.Split(block[1], "\n") {
		if m := regexp.MustCompile(`^      ([a-z_]+):$`).FindStringSubmatch(l); m != nil {
			name = m[1]
		} else if m := regexp.MustCompile(`^        default:\s*(.*)$`).FindStringSubmatch(l); m != nil && name != "" {
			out[name] = strings.Trim(m[1], `"`)
		}
	}
	if len(out) != 10 {
		t.Fatalf("read %d defaults, want 10: %v", len(out), out)
	}
	return out
}

func inputsOf(m map[string]string) Inputs {
	return Inputs{CelerisRef: m["celeris_ref"], Packages: m["packages"], Run: m["run"], Count: m["count"], Shards: m["shards"],
		Arches: m["arches"], Memlock: m["memlock"], Race: m["race"], Timeout: m["timeout"], Extra: m["extra"]}
}

func TestWorkflowDefaultsAreAValidDispatch(t *testing.T) {
	if _, err := planDispatch(inputsOf(workflowDefaults(t))); err != nil {
		t.Errorf("the dispatch defaults are refused: %v", err)
	}
}

// shellWords splits a command line the way sh would for the quoting the
// docs use: '...', "..." and bare words.
func shellWords(s string) []string {
	var out []string
	var cur strings.Builder
	in, quote := false, rune(0)
	for _, r := range s {
		switch {
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
			cur.WriteRune(r)
		case r == '\'' || r == '"':
			quote, in = r, true
		case r == ' ' || r == '\t':
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if in {
		out = append(out, cur.String())
	}
	return out
}

func TestDocsExamplesAreValidDispatches(t *testing.T) {
	b, err := os.ReadFile(filepath.FromSlash(docsPath))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(b), "\\\n", " ")
	defaults := workflowDefaults(t)
	n := 0
	for _, line := range strings.Split(text, "\n") {
		i := strings.Index(line, "gh workflow run celeris-stress.yml")
		if i < 0 {
			continue
		}
		n++
		if c := strings.Index(line, " #"); c > i {
			line = line[:c]
		}
		in := map[string]string{}
		for k, v := range defaults {
			in[k] = v
		}
		words := shellWords(line[i:])
		for j := 0; j+1 < len(words); j++ {
			if words[j] != "-f" {
				continue
			}
			k, v, ok := strings.Cut(words[j+1], "=")
			if _, known := defaults[k]; !ok || !known {
				t.Errorf("example %d: -f %q is not a dispatch input", n, words[j+1])
				continue
			}
			if strings.HasPrefix(v, "$") { // a shell variable holding a resolved sha
				v = testSHA
			}
			in[k] = v
		}
		if _, err := planDispatch(inputsOf(in)); err != nil {
			t.Errorf("example %d is refused by the plan: %v\n%s", n, err, strings.TrimSpace(line[i:]))
		}
	}
	if n < 6 {
		t.Errorf("found %d gh workflow run examples, want at least 6", n)
	}
}
