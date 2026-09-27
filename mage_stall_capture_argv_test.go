//go:build mage

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runValidatePlaybook's real argv, recorded by a stand-in ansible-playbook
// on PATH: PROBATORIUM_STALL_CAPTURE set on the mage process reaches the
// playbook as the extra-var ansible/validate.yml re-exports to the
// validator (workflow_stall_capture_test.go checks the playbook side), and
// an unset knob adds nothing, so a routine run stays off.
func TestValidatePlaybookArgvCarriesTheStallCapture(t *testing.T) {
	for _, tc := range []struct {
		name, knob string
		want       bool
	}{
		{"set", "1", true},
		{"unset", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "ansible"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
				t.Fatal(err)
			}
			argv := filepath.Join(dir, "argv.txt")
			script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done >> '" + argv + "'\n"
			if err := os.WriteFile(filepath.Join(dir, "bin", "ansible-playbook"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", filepath.Join(dir, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("PROBATORIUM_STALL_CAPTURE", tc.knob)
			t.Setenv("PROBATORIUM_REFAPP_FAULT", "")
			t.Setenv("PROBATORIUM_VALIDATE_DRIVER", "")
			t.Setenv("VALIDATE_PARALLEL", "")
			t.Chdir(dir)
			if err := runValidatePlaybook("1m", "msr1", "v0.0.0-test", false); err != nil {
				t.Fatalf("runValidatePlaybook: %v", err)
			}
			b, err := os.ReadFile(argv)
			if err != nil {
				t.Fatalf("the stand-in ansible-playbook never ran: %v", err)
			}
			args := strings.Split(strings.TrimSpace(string(b)), "\n")
			got := false
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "--extra-vars" && strings.Contains(args[i+1], "probatorium_stall_capture") {
					got = true
					if args[i+1] != `{"probatorium_stall_capture":"1"}` {
						t.Errorf("stall-capture extra-var = %q, want {\"probatorium_stall_capture\":\"1\"}", args[i+1])
					}
				}
			}
			if got != tc.want {
				t.Fatalf("PROBATORIUM_STALL_CAPTURE=%q: extra-var present=%v, want %v; argv:\n%s", tc.knob, got, tc.want, b)
			}
		})
	}
}
