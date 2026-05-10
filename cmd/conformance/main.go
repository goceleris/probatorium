// Command conformance is the pre-bench server-contract validator.
//
// For every adapter in [servers.Registry] (or the subset selected via
// -servers), the harness:
//
//  1. Picks a free loopback port (chosen sequentially from -bind-base
//     starting at -bind-port-start; falls back to ":0" when that range
//     is exhausted).
//  2. Boots the adapter via [servers.StartAdapter].
//  3. TCP-probes the bind addr until ready.
//  4. Issues every entry in [common.Endpoints] and byte-compares the
//     response body against [common.Endpoint.ResponseBody]. Endpoints
//     whose ResponseBody is empty are dynamic — for /users/:id we
//     substitute a sample id and verify the echo; for /upload we send a
//     small body and verify the "OK" reply.
//  5. SIGTERM / SIGKILL the adapter.
//
// The harness MUST run at the start of every bench session; refuses to
// start the bench if any adapter fails. Output is one
// `<bind-base>/conformance/<server>.json` per adapter plus a stdout
// summary table. Exit code is non-zero iff any adapter fails any
// endpoint.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/goceleris/probatorium/servers"
	"github.com/goceleris/probatorium/servers/common"
)

// Config is the parsed flag set.
type Config struct {
	Servers  string
	BindBase string
	Out      string
	FailFast bool
	Timeout  time.Duration
}

// DefaultConfig returns the fresh-flag defaults.
func DefaultConfig() Config {
	return Config{
		BindBase: "127.0.0.1",
		Out:      "",
		Timeout:  10 * time.Second,
	}
}

// Bind registers every Config field on fs.
func (c *Config) Bind(fs *flag.FlagSet) {
	fs.StringVar(&c.Servers, "servers", c.Servers, "comma-separated list of adapter names to probe (default: all)")
	fs.StringVar(&c.BindBase, "bind-base", c.BindBase, "IP to bind probed adapters on (port chosen automatically)")
	fs.StringVar(&c.Out, "out", c.Out, "output directory for per-adapter JSON; empty = skip JSON output")
	fs.BoolVar(&c.FailFast, "fail-fast", c.FailFast, "abort on first conformance failure")
	fs.DurationVar(&c.Timeout, "timeout", c.Timeout, "per-adapter probe timeout (boot + every endpoint)")
}

// ParseArgs parses argv (without the program name).
func ParseArgs(args []string, out io.Writer) (Config, error) {
	cfg := DefaultConfig()
	fs := flag.NewFlagSet("probatorium-conformance", flag.ContinueOnError)
	fs.SetOutput(out)
	cfg.Bind(fs)
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// EndpointFailure records one mismatched response.
type EndpointFailure struct {
	Path     string `json:"path"`
	Method   string `json:"method"`
	Expected []byte `json:"expected"`
	Actual   []byte `json:"actual"`
	Status   int    `json:"status"`
	Reason   string `json:"reason,omitempty"`
}

// AdapterReport is the per-adapter conformance verdict.
type AdapterReport struct {
	Adapter         string            `json:"adapter"`
	OK              bool              `json:"ok"`
	BindAddr        string            `json:"bind_addr"`
	StartError      string            `json:"start_error,omitempty"`
	FailedEndpoints []EndpointFailure `json:"failed_endpoints,omitempty"`
}

func main() {
	cfg, err := ParseArgs(os.Args[1:], os.Stderr)
	if err != nil {
		os.Exit(2)
	}
	failed, err := run(cfg, os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "probatorium-conformance: %v\n", err)
		os.Exit(1)
	}
	if failed > 0 {
		os.Exit(3)
	}
}

// run drives the conformance pass. Returns the count of adapters that
// failed at least one endpoint.
func run(cfg Config, summary io.Writer) (int, error) {
	names := selectAdapters(cfg.Servers)
	if len(names) == 0 {
		return 0, fmt.Errorf("no adapters selected (Registry is empty or -servers filtered everything out)")
	}
	if cfg.Out != "" {
		if err := os.MkdirAll(cfg.Out, 0o755); err != nil {
			return 0, fmt.Errorf("mkdir %s: %w", cfg.Out, err)
		}
	}

	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	failed := 0
	reports := make([]AdapterReport, 0, len(names))
	for _, name := range names {
		rep := probeAdapter(rootCtx, cfg, name)
		reports = append(reports, rep)
		if !rep.OK {
			failed++
			if cfg.FailFast {
				break
			}
		}
		if cfg.Out != "" {
			if err := writeJSON(filepath.Join(cfg.Out, name+".json"), &rep); err != nil {
				return failed, err
			}
		}
	}
	writeSummary(summary, reports)
	return failed, nil
}

// selectAdapters resolves the -servers filter to a stable adapter-name
// slice. Empty filter → every registered adapter.
func selectAdapters(filter string) []string {
	all := servers.AdaptersSorted()
	if filter == "" {
		out := make([]string, 0, len(all))
		for _, a := range all {
			out = append(out, a.Name)
		}
		return out
	}
	want := map[string]bool{}
	for _, p := range strings.Split(filter, ",") {
		if p = strings.TrimSpace(p); p != "" {
			want[p] = true
		}
	}
	out := make([]string, 0, len(want))
	for _, a := range all {
		if want[a.Name] {
			out = append(out, a.Name)
		}
	}
	return out
}

// probeAdapter boots one adapter and walks every endpoint in the
// contract. Adapter-start errors short-circuit endpoint probing — the
// caller still gets a report with OK=false and a populated StartError.
func probeAdapter(ctx context.Context, cfg Config, name string) AdapterReport {
	rep := AdapterReport{Adapter: name}

	port, err := freePort(cfg.BindBase)
	if err != nil {
		rep.StartError = "free port: " + err.Error()
		return rep
	}
	bind := fmt.Sprintf("%s:%d", cfg.BindBase, port)
	rep.BindAddr = bind

	startCtx, startCancel := context.WithTimeout(ctx, cfg.Timeout)
	defer startCancel()
	stop, err := servers.StartAdapter(startCtx, name, bind)
	if err != nil {
		rep.StartError = "start: " + err.Error()
		return rep
	}
	defer func() { _ = stop() }()

	if err := waitForTCP(ctx, bind, cfg.Timeout); err != nil {
		rep.StartError = "ready-check: " + err.Error()
		return rep
	}

	rep.FailedEndpoints = ProbeContract(ctx, "http://"+bind, cfg.Timeout)
	rep.OK = len(rep.FailedEndpoints) == 0
	return rep
}

// ProbeContract issues every endpoint in [common.Endpoints] against
// targetURL and returns the set of failures. Exported so the test can
// drive the same logic against a stub server.
func ProbeContract(ctx context.Context, targetURL string, perRequestTimeout time.Duration) []EndpointFailure {
	client := &http.Client{Timeout: perRequestTimeout}
	failures := make([]EndpointFailure, 0)
	for _, ep := range common.Endpoints {
		fail := probeOne(ctx, client, targetURL, ep)
		if fail != nil {
			failures = append(failures, *fail)
		}
	}
	return failures
}

// probeOne dispatches one endpoint check. Static-body endpoints are
// byte-compared; the dynamic endpoints (/users/:id and /upload) have
// their own equality predicates.
func probeOne(ctx context.Context, client *http.Client, base string, ep common.Endpoint) *EndpointFailure {
	switch ep.Path {
	case "/users/:id":
		return probeUsers(ctx, client, base)
	case "/upload":
		return probeUpload(ctx, client, base)
	}
	url := base + ep.Path
	req, err := http.NewRequestWithContext(ctx, ep.Method, url, nil)
	if err != nil {
		return &EndpointFailure{Path: ep.Path, Method: ep.Method, Reason: "build request: " + err.Error()}
	}
	resp, err := client.Do(req)
	if err != nil {
		return &EndpointFailure{Path: ep.Path, Method: ep.Method, Reason: err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return &EndpointFailure{Path: ep.Path, Method: ep.Method, Status: resp.StatusCode, Reason: "read body: " + err.Error()}
	}
	if resp.StatusCode != http.StatusOK {
		return &EndpointFailure{
			Path: ep.Path, Method: ep.Method, Status: resp.StatusCode,
			Expected: ep.ResponseBody, Actual: body,
			Reason: fmt.Sprintf("status %d != 200", resp.StatusCode),
		}
	}
	if !bytes.Equal(body, ep.ResponseBody) {
		return &EndpointFailure{
			Path: ep.Path, Method: ep.Method, Status: resp.StatusCode,
			Expected: ep.ResponseBody, Actual: body,
			Reason: "body mismatch",
		}
	}
	return nil
}

// probeUsers issues GET /users/42 and verifies the echoed body matches
// the canonical "User ID: 42" template (see common.WritePath).
func probeUsers(ctx context.Context, client *http.Client, base string) *EndpointFailure {
	const sampleID = "42"
	expected := []byte("User ID: " + sampleID)
	url := base + "/users/" + sampleID
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return &EndpointFailure{Path: "/users/:id", Method: "GET", Reason: "build request: " + err.Error()}
	}
	resp, err := client.Do(req)
	if err != nil {
		return &EndpointFailure{Path: "/users/:id", Method: "GET", Reason: err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return &EndpointFailure{
			Path: "/users/:id", Method: "GET", Status: resp.StatusCode,
			Expected: expected, Actual: body,
			Reason: fmt.Sprintf("status %d != 200", resp.StatusCode),
		}
	}
	if !bytes.Equal(body, expected) {
		return &EndpointFailure{
			Path: "/users/:id", Method: "GET", Status: resp.StatusCode,
			Expected: expected, Actual: body,
			Reason: "echo mismatch",
		}
	}
	return nil
}

// probeUpload POSTs a small body to /upload and verifies the "OK" reply
// matches common.Endpoints[/upload].ResponseBody byte-for-byte.
func probeUpload(ctx context.Context, client *http.Client, base string) *EndpointFailure {
	expected := []byte("OK")
	body := bytes.NewReader([]byte("conformance-probe"))
	req, err := http.NewRequestWithContext(ctx, "POST", base+"/upload", body)
	if err != nil {
		return &EndpointFailure{Path: "/upload", Method: "POST", Reason: "build request: " + err.Error()}
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := client.Do(req)
	if err != nil {
		return &EndpointFailure{Path: "/upload", Method: "POST", Reason: err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return &EndpointFailure{
			Path: "/upload", Method: "POST", Status: resp.StatusCode,
			Expected: expected, Actual: got,
			Reason: fmt.Sprintf("status %d != 200", resp.StatusCode),
		}
	}
	if !bytes.Equal(got, expected) {
		return &EndpointFailure{
			Path: "/upload", Method: "POST", Status: resp.StatusCode,
			Expected: expected, Actual: got,
			Reason: "ack mismatch",
		}
	}
	return nil
}

// writeSummary prints a fixed-width pass/fail table to w.
func writeSummary(w io.Writer, reports []AdapterReport) {
	sort.Slice(reports, func(i, j int) bool { return reports[i].Adapter < reports[j].Adapter })
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintf(w, "%-40s %-6s %s\n", "adapter", "ok", "detail")
	_, _ = fmt.Fprintln(w, strings.Repeat("-", 80))
	for _, r := range reports {
		ok := "PASS"
		detail := r.BindAddr
		if !r.OK {
			ok = "FAIL"
			if r.StartError != "" {
				detail = r.StartError
			} else if len(r.FailedEndpoints) > 0 {
				paths := make([]string, 0, len(r.FailedEndpoints))
				for _, f := range r.FailedEndpoints {
					paths = append(paths, f.Method+" "+f.Path)
				}
				detail = strings.Join(paths, ", ")
			}
		}
		_, _ = fmt.Fprintf(w, "%-40s %-6s %s\n", r.Adapter, ok, detail)
	}
}

// freePort asks the kernel for a free port on the given bind base.
func freePort(bindBase string) (int, error) {
	l, err := net.Listen("tcp", bindBase+":0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// waitForTCP retries DialContext until the address answers or the
// deadline elapses.
func waitForTCP(ctx context.Context, addr string, timeout time.Duration) error {
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var d net.Dialer
	for {
		conn, err := d.DialContext(wctx, "tcp", addr)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case <-wctx.Done():
			return fmt.Errorf("tcp probe %s: %w", addr, wctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// writeJSON marshals v to path with 2-space indentation.
func writeJSON(p string, v any) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	buf, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, buf, 0o644)
}
