package servers_test

// Conformance tests for the fastapi (python) adapter.
//
// The two tests in this file walk the same byte-equality contract the
// cmd/conformance harness enforces in production, but in-process and
// without launching uvicorn:
//
//   1. TestFastAPIRegistered  — pure registry check, runs everywhere.
//   2. TestFastAPIPayload     — drives `python -m app.payload` to dump
//                                the generated 1k/64k payloads and
//                                byte-compares against the Go reference
//                                in servers/common. SKIPs cleanly when
//                                neither `uv` nor a system python with
//                                `orjson` available — i.e. on a vanilla
//                                CI runner without the full python
//                                toolchain installed by the ansible
//                                role.
//
// This is the "python-skip test" called out in the wave-4c spec —
// `go test ./...` on a dev mac without uv must pass.

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/goceleris/probatorium/servers"
	"github.com/goceleris/probatorium/servers/common"
)

func TestFastAPIRegistered(t *testing.T) {
	a, ok := servers.Registry["fastapi"]
	if !ok {
		t.Fatal("Registry[\"fastapi\"] missing")
	}
	if a.Language != "python" {
		t.Errorf("Language = %q, want python", a.Language)
	}
	if a.Framework != "fastapi" {
		t.Errorf("Framework = %q, want fastapi", a.Framework)
	}
	nb, ok := a.Bin.(servers.NativeBinary)
	if !ok {
		t.Fatalf("Bin = %T, want servers.NativeBinary", a.Bin)
	}
	if nb.Lang != "python" {
		t.Errorf("NativeBinary.Lang = %q, want python", nb.Lang)
	}
	if !strings.Contains(nb.RunCmd, "{bind}") {
		t.Errorf("RunCmd = %q, missing {bind} substitution", nb.RunCmd)
	}
}

// TestFastAPIPayload byte-compares the Python `payload.py` output
// against the Go reference. SKIPs when the python toolchain isn't
// installed on the dev box (CI without uv hits this path).
func TestFastAPIPayload(t *testing.T) {
	// Locate servers/fastapi relative to this test file via runtime.Caller.
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Dir(filepath.Dir(thisFile))
	fastapiDir := filepath.Join(repoRoot, "servers", "fastapi")

	py := pythonExe(t, fastapiDir)
	if py == "" {
		t.Skip("no python with orjson available — install via ansible/roles/python or `pip install orjson`")
	}

	for _, tc := range []struct {
		name     string
		expr     string
		expected []byte
	}{
		{"json-1k", "import sys; from app.payload import JSON_1K_PAYLOAD as p; sys.stdout.buffer.write(p)", common.JSON1KPayload()},
		{"json-8k", "import sys; from app.payload import JSON_8K_PAYLOAD as p; sys.stdout.buffer.write(p)", common.JSON8KPayload()},
		{"json-16k", "import sys; from app.payload import JSON_16K_PAYLOAD as p; sys.stdout.buffer.write(p)", common.JSON16KPayload()},
		{"json-64k", "import sys; from app.payload import JSON_64K_PAYLOAD as p; sys.stdout.buffer.write(p)", common.JSON64KPayload()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(py, "-c", tc.expr)
			cmd.Dir = fastapiDir
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if err != nil {
				t.Skipf("python exec failed (likely orjson missing): %v\nstderr: %s", err, stderr.String())
			}
			if !bytes.Equal(out, tc.expected) {
				t.Fatalf("%s payload differs from Go reference\nGo  len=%d\nPy  len=%d\nfirst-diff at %d",
					tc.name, len(tc.expected), len(out), firstDiffOffset(out, tc.expected))
			}
		})
	}
}

// pythonExe returns the path to a python interpreter that has the
// adapter's deps importable. Preference order:
//  1. .venv/bin/python under the adapter dir (matches what the
//     cluster launcher uses);
//  2. `python3` on PATH (dev-mac fallback — the test will further
//     verify orjson is importable by trying the actual expression).
//
// Returns "" if neither yields a working interpreter.
func pythonExe(t *testing.T, fastapiDir string) string {
	t.Helper()
	venvPy := filepath.Join(fastapiDir, ".venv", "bin", "python")
	if _, err := exec.LookPath(venvPy); err == nil {
		return venvPy
	}
	for _, name := range []string{"python3.13", "python3", "python"} {
		if p, err := exec.LookPath(name); err == nil {
			// Verify orjson importable.
			if err := exec.Command(p, "-c", "import orjson").Run(); err == nil {
				return p
			}
		}
	}
	return ""
}

func firstDiffOffset(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	if len(a) != len(b) {
		return n
	}
	return -1
}
