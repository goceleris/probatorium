// Bun-adapter integration tests (wave 4b).
//
// These boot the staged hono / elysia launchers under the running Go
// toolchain and byte-compare every response body against the
// canonical contract in servers/common. The launchers themselves are
// written by ansible/roles/bun/tasks/build_competitor.yml on the
// cluster; for local dev runs the test SKIPS unless Bun is on PATH
// AND `bun install` + `bun build` have been run inside the source
// dirs (which the test does on demand the first time it's run).
//
// The test is deliberately conservative: it does NOT install bun for
// you, does NOT cache the build between test invocations, and does
// NOT touch competitors/<name> staging — it builds in-tree and exec's
// the launcher right out of the source dir. The point is to catch
// regressions where the TypeScript payload generator drifts from the
// Go reference, not to replicate the cluster build pipeline.

package servers_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/probatorium/servers/common"
)

// bunAdapters is the static list of (name, srcRel) pairs the test
// walks. srcRel is resolved relative to the repo root the test
// discovers via go.mod ascent.
var bunAdapters = []struct {
	name   string
	srcRel string
}{
	{"hono", "servers/hono"},
	{"elysia", "servers/elysia"},
}

func TestBunAdapters_Conformance(t *testing.T) {
	bunPath, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("bun not on PATH — wave 4b adapter conformance covered by cluster build only")
	}
	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Fatalf("findRepoRoot: %v", err)
	}

	for _, ad := range bunAdapters {
		ad := ad
		t.Run(ad.name, func(t *testing.T) {
			srcDir := filepath.Join(repoRoot, ad.srcRel)
			if _, err := os.Stat(filepath.Join(srcDir, "package.json")); err != nil {
				t.Skipf("source dir %s missing package.json: %v", srcDir, err)
			}
			distBin := filepath.Join(srcDir, "dist", "server")
			if _, err := os.Stat(distBin); err != nil {
				if err := bunInstallAndBuild(t, bunPath, srcDir); err != nil {
					t.Skipf("bun install/build failed (likely sandbox or no network): %v", err)
				}
			}
			runConformanceCell(t, bunPath, distBin, ad.name)
		})
	}
}

// bunInstallAndBuild is a best-effort helper for local dev. The
// cluster path uses ansible — this only runs when a developer runs
// `go test` against an unbuilt tree. Failures are non-fatal: the
// caller skips rather than failing.
func bunInstallAndBuild(t *testing.T, bunPath, srcDir string) error {
	t.Helper()
	t.Logf("bun install in %s", srcDir)
	cmd := exec.Command(bunPath, "install", "--production")
	cmd.Dir = srcDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("bun install: %w (output: %s)", err, out)
	}
	t.Logf("bun build in %s", srcDir)
	cmd = exec.Command(bunPath, "build",
		"--target=bun", "--minify", "--sourcemap=none",
		"./src/server.ts", "--outfile=./dist/server")
	cmd.Dir = srcDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("bun build: %w (output: %s)", err, out)
	}
	return nil
}

// runConformanceCell exec's `bun run <distBin> -- -bind 127.0.0.1:0`,
// parses the resolved port from the "ready addr=" stdout line, then
// hits every endpoint in common.Endpoints and byte-compares the
// response.
func runConformanceCell(t *testing.T, bunPath, distBin, framework string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bunPath, "run", distBin, "--", "-bind", "127.0.0.1:0")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "NODE_ENV=production")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start bun: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	})

	port, err := readReadyPort(stdout, 10*time.Second)
	if err != nil {
		t.Fatalf("read ready line: %v", err)
	}

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	if err := waitTCPReady(base, 5*time.Second); err != nil {
		t.Fatalf("waitTCPReady: %v", err)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	for _, ep := range common.Endpoints {
		ep := ep
		t.Run(strings.TrimPrefix(ep.Path, "/"), func(t *testing.T) {
			path := ep.Path
			expected := ep.ResponseBody
			if path == "/users/:id" {
				path = "/users/42"
				expected = []byte("User ID: 42")
			}
			var resp *http.Response
			var err error
			if ep.Method == "GET" {
				resp, err = client.Get(base + path)
			} else {
				resp, err = client.Post(base+path, "application/octet-stream",
					strings.NewReader("ping"))
			}
			if err != nil {
				t.Fatalf("%s %s: %v", ep.Method, path, err)
			}
			defer func() { _ = resp.Body.Close() }()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != 200 {
				t.Fatalf("%s %s: status %d, body=%q", ep.Method, path, resp.StatusCode, body)
			}
			if !bytes.Equal(body, expected) {
				t.Fatalf("%s %s: body mismatch\nwant %d bytes, got %d bytes\nfirst 80 want: %q\nfirst 80 got:  %q",
					ep.Method, path, len(expected), len(body),
					trim80(expected), trim80(body))
			}
		})
	}
	_ = framework
}

func trim80(b []byte) string {
	if len(b) <= 80 {
		return string(b)
	}
	return string(b[:80])
}

// readReadyPort scans the adapter's stdout for the canonical
// "ready addr=<host>:<port>" line and returns the port. The line is
// the cross-language ready signal — every probatorium adapter prints
// it once after Listen returns.
func readReadyPort(r io.Reader, timeout time.Duration) (int, error) {
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		buf := make([]byte, 0, 4096)
		tmp := make([]byte, 256)
		for {
			n, err := r.Read(tmp)
			if n > 0 {
				buf = append(buf, tmp[:n]...)
				if i := bytes.IndexByte(buf, '\n'); i >= 0 {
					line := string(buf[:i])
					if strings.Contains(line, "ready addr=") {
						ch <- result{line: line}
						return
					}
					buf = buf[i+1:]
				}
			}
			if err != nil {
				ch <- result{err: err}
				return
			}
		}
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			return 0, res.err
		}
		idx := strings.Index(res.line, "ready addr=")
		addr := res.line[idx+len("ready addr="):]
		_, portStr, ok := strings.Cut(addr, ":")
		if !ok {
			return 0, fmt.Errorf("invalid ready line: %q", res.line)
		}
		var port int
		if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
			return 0, fmt.Errorf("parse port from %q: %w", res.line, err)
		}
		return port, nil
	case <-time.After(timeout):
		return 0, errors.New("timeout waiting for ready addr= line")
	}
}

// waitTCPReady dials the listener until it accepts a connection,
// covering the gap between bun printing "ready addr=" and Bun.serve
// finishing the listen() syscall under the hood.
func waitTCPReady(base string, timeout time.Duration) error {
	hostPort := strings.TrimPrefix(base, "http://")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", hostPort, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("listener at %s not ready within %s", hostPort, timeout)
}

// findRepoRoot walks up from the test's working directory until it
// hits the file go.mod. Test binaries run with the package dir as
// cwd, which is /servers/ here.
func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			// Sanity check: probatorium's go.mod is the repo root.
			data, _ := os.ReadFile(filepath.Join(dir, "go.mod"))
			if bytes.Contains(data, []byte("module github.com/goceleris/probatorium")) {
				return dir, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("repo root not found (no probatorium go.mod up the tree)")
		}
		dir = parent
	}
}
