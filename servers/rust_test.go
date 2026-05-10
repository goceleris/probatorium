package servers_test

// Smoke test for the wave 4a Rust adapters (axum, actix-web, ntex). The
// test SKIPs gracefully when no Rust binary has been pre-built — most
// dev machines don't have a rust toolchain installed, and the cluster
// build is ansible-driven, not part of `go test`. When a binary IS
// present at PROBATORIUM_BENCH_ROOT/competitors/<name> (or one of the
// other paths servers.StartAdapter resolves), we boot it, probe the
// canonical contract endpoints, and verify byte-identical responses.
//
// This test exists so the cluster runner is not the first place a
// regression in src/payload.rs or src/main.rs surfaces. Running it on
// the dev Mac after `cargo build --profile release-fat` against
// ./target/release-fat/probatorium-axum-server (symlinked into ./axum)
// gives the same conformance verdict as cmd/conformance would on the
// cluster.
//
// The test never attempts to invoke cargo itself — that would couple
// `go test` to the rust toolchain, which the always-latest deploy
// policy explicitly forbids on the dev side.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goceleris/probatorium/servers"
	"github.com/goceleris/probatorium/servers/common"
)

func TestRustAdapters_ContractConformance(t *testing.T) {
	for _, name := range []string{"axum", "actix-web", "ntex"} {
		name := name
		t.Run(name, func(t *testing.T) {
			runRustAdapterConformance(t, name)
		})
	}
}

// runRustAdapterConformance boots one Rust adapter and walks every
// endpoint in common.Endpoints. SKIPs when the binary is absent.
func runRustAdapterConformance(t *testing.T, name string) {
	t.Helper()

	if !rustBinaryExists(name) {
		t.Skipf("rust adapter binary for %q not found in any staging path; skipping (build with `cargo build --profile release-fat` to enable)", name)
	}

	port, err := freePort()
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	bind := fmt.Sprintf("127.0.0.1:%d", port)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	stop, err := servers.StartAdapter(ctx, name, bind)
	if err != nil {
		t.Fatalf("start %q: %v", name, err)
	}
	defer func() { _ = stop() }()

	if err := waitForTCP(ctx, bind, 10*time.Second); err != nil {
		t.Fatalf("ready-check %q: %v", name, err)
	}

	probeContract(t, "http://"+bind)
}

// rustBinaryExists reports whether servers.StartAdapter can resolve a
// staged binary for name. We replicate the lookup logic here rather
// than calling a private helper so the test is decoupled from start.go.
//
// "Exists" here means a non-directory file with the executable bit
// set — `go test` runs from the package directory, so `./<name>` would
// otherwise match the source subdir (e.g. servers/axum/) and trick the
// test into trying to exec the directory.
func rustBinaryExists(name string) bool {
	root := os.Getenv(servers.BenchRootEnv)
	if root == "" {
		root = servers.DefaultBenchRoot
	}
	for _, p := range []string{
		filepath.Join(root, "competitors", name),
		filepath.Join(root, name),
		"./" + name,
	} {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if fi.IsDir() {
			continue
		}
		// Executable bit on owner / group / world. cargo + the ansible
		// symlink both produce 0755, so any of the three suffices.
		if fi.Mode().Perm()&0o111 == 0 {
			continue
		}
		return true
	}
	return false
}

// probeContract walks every entry in common.Endpoints and asserts the
// response body matches byte-for-byte. The dynamic endpoints
// (/users/:id and /upload) have their own equality predicates that
// match cmd/conformance's behaviour.
//
// expectedBody resolves the "what bytes should this endpoint return"
// question separately from common.Endpoints[i].ResponseBody — at this
// time the contract.go init order leaves the /json-1k and /json-64k
// entries empty (the payload.go init that populates the package
// generators runs after contract.go's wire-in init, since file-init
// order is lexical and "contract" sorts before "payload"). The
// canonical bytes are still available via the JSON*KPayload accessors,
// so we route around the empty-slice case here.
func probeContract(t *testing.T, base string) {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}

	for _, ep := range common.Endpoints {
		ep := ep
		switch ep.Path {
		case "/users/:id":
			probeUsers(t, client, base)
			continue
		case "/upload":
			probeUpload(t, client, base)
			continue
		}
		req, err := http.NewRequest(ep.Method, base+ep.Path, nil)
		if err != nil {
			t.Errorf("%s %s: build request: %v", ep.Method, ep.Path, err)
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Errorf("%s %s: %v", ep.Method, ep.Path, err)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s %s: status %d != 200; body=%q", ep.Method, ep.Path, resp.StatusCode, body)
			continue
		}
		want := expectedBody(ep)
		if !bytes.Equal(body, want) {
			t.Errorf("%s %s: body mismatch (got %d bytes, want %d bytes)",
				ep.Method, ep.Path, len(body), len(want))
		}
	}
}

// expectedBody resolves the canonical bytes for a contract endpoint,
// routing around the contract.go init-order quirk (see probeContract).
func expectedBody(ep common.Endpoint) []byte {
	switch ep.Path {
	case "/json-1k":
		return common.JSON1KPayload()
	case "/json-64k":
		return common.JSON64KPayload()
	}
	return ep.ResponseBody
}

func probeUsers(t *testing.T, client *http.Client, base string) {
	t.Helper()
	const sampleID = "42"
	expected := []byte("User ID: " + sampleID)
	resp, err := client.Get(base + "/users/" + sampleID)
	if err != nil {
		t.Errorf("GET /users/%s: %v", sampleID, err)
		return
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /users/%s: status %d; body=%q", sampleID, resp.StatusCode, body)
		return
	}
	if !bytes.Equal(body, expected) {
		t.Errorf("GET /users/%s: got %q want %q", sampleID, body, expected)
	}
}

func probeUpload(t *testing.T, client *http.Client, base string) {
	t.Helper()
	expected := []byte("OK")
	resp, err := client.Post(base+"/upload", "application/octet-stream",
		bytes.NewReader([]byte("conformance-probe")))
	if err != nil {
		t.Errorf("POST /upload: %v", err)
		return
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("POST /upload: status %d; body=%q", resp.StatusCode, body)
		return
	}
	if !bytes.Equal(body, expected) {
		t.Errorf("POST /upload: got %q want %q", body, expected)
	}
}

// freePort asks the kernel for a free loopback port.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// waitForTCP retries DialContext until the address answers or the
// deadline elapses. Mirrors cmd/conformance's helper.
func waitForTCP(ctx context.Context, addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var d net.Dialer
	for {
		dialCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		conn, err := d.DialContext(dialCtx, "tcp", addr)
		cancel()
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("tcp probe %s: %w", addr, err)
		}
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), err)
		case <-time.After(50 * time.Millisecond):
		}
	}
}
