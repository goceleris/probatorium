package validation

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// captureForensicsLive snapshots the refapp's /proc state + dumps
// goroutine / heap / block / mutex profiles via the refapp's
// /debug/pprof endpoint into outDir.
//
// All capture is best-effort: a missing /proc/<pid>/<file> (process
// already exited, permissions denied on macOS, ...) writes a
// `.missing` marker rather than aborting the dossier. The bench
// pipeline already ran for hours before this fires; losing one
// forensic file shouldn't lose every other file.
//
// On Linux, gcore (if available) is run last because it's the
// expensive one — by then the inexpensive files are already on disk.
//
// outDir is created by the caller (handleIncident); this function
// writes siblings under it.
func captureForensicsLive(ctx context.Context, outDir string, pid int, listenAddr string) error {
	// /proc snapshots — read once, write atomically. These reads
	// are cheap (kilobyte-scale) so happen in series rather than
	// fan-out — sequential reads keep the file order recoverable
	// from postmortem timestamps. Skipped entirely when pid <= 0
	// (Tier 1 hard-fail before refapp ready, or refapp died); the
	// pprof leg below still runs if listenAddr is non-empty.
	if pid > 0 {
		for _, name := range []string{"maps", "status", "smaps", "stack", "cmdline", "limits"} {
			src := fmt.Sprintf("/proc/%d/%s", pid, name)
			dst := filepath.Join(outDir, "proc-"+name+".txt")
			if err := snapshotFile(src, dst); err != nil {
				// Don't propagate; write a marker so postmortem knows
				// which file failed.
				_ = writePlainText(dst+".missing",
					fmt.Sprintf("snapshot failed: %v\n", err))
			}
		}

		// /proc/<pid>/fd is a directory; capture the entry list (with
		// readlink targets where possible).
		if err := snapshotFDDir(pid, filepath.Join(outDir, "proc-fd.txt")); err != nil {
			_ = writePlainText(filepath.Join(outDir, "proc-fd.txt.missing"),
				fmt.Sprintf("fd snapshot failed: %v\n", err))
		}
	}

	// pprof profiles — only fire if the refapp exposes
	// /debug/pprof (celeris validation build does; competitor
	// adapters don't). Listen-addr empty disables the pprof leg.
	if listenAddr != "" {
		pprofProfiles := []struct {
			path string
			out  string
		}{
			{"/debug/pprof/heap", "heap.pprof"},
			{"/debug/pprof/goroutine", "goroutine.pprof"},
			{"/debug/pprof/block", "block.pprof"},
			{"/debug/pprof/mutex", "mutex.pprof"},
			{"/debug/pprof/threadcreate", "threadcreate.pprof"},
		}
		hc := &http.Client{Timeout: 5 * time.Second}
		for _, p := range pprofProfiles {
			if err := curlPprof(ctx, hc, "http://"+listenAddr+p.path,
				filepath.Join(outDir, p.out)); err != nil {
				_ = writePlainText(filepath.Join(outDir, p.out+".missing"),
					fmt.Sprintf("pprof fetch failed: %v\n", err))
			}
		}
	}

	// Try gcore — last because it's the only one that can take
	// seconds AND it temporarily pauses the target process. We do
	// this AFTER the pprof + /proc reads so an interrupt of the
	// gcore step doesn't lose the cheaper artefacts. Requires a
	// live PID — no-op for the pprof-only path.
	if pid > 0 {
		if hasBinary("gcore") {
			if err := runGCore(ctx, pid, filepath.Join(outDir, "core")); err != nil {
				_ = writePlainText(filepath.Join(outDir, "core.missing"),
					fmt.Sprintf("gcore failed: %v\n", err))
			}
		} else {
			_ = writePlainText(filepath.Join(outDir, "core.missing"),
				"gcore not available on this host\n")
		}
	}

	// dmesg tail — kernel ring buffer for OOM kills / segfaults
	// the kernel may have logged. Last 5 minutes is the sweet spot.
	if hasBinary("dmesg") {
		if err := dmesgSince(ctx, 5*time.Minute, filepath.Join(outDir, "dmesg.txt")); err != nil {
			_ = writePlainText(filepath.Join(outDir, "dmesg.txt.missing"),
				fmt.Sprintf("dmesg failed: %v\n", err))
		}
	}

	// Write a manifest of everything we attempted, so postmortem
	// readers know whether a missing file is "we tried and failed"
	// vs "we never tried."
	return writePlainText(filepath.Join(outDir, "forensics_status.txt"),
		fmt.Sprintf("pid=%d listen=%q gcore=%v dmesg=%v\n",
			pid, listenAddr, hasBinary("gcore"), hasBinary("dmesg")))
}

// snapshotFile copies /proc/<pid>/<name> into dst. Errors out if the
// source doesn't exist or isn't readable.
func snapshotFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}

// snapshotFDDir lists /proc/<pid>/fd entries with readlink targets.
// Format: one line per FD: "<fdnum> -> <target>".
func snapshotFDDir(pid int, dst string) error {
	dir := fmt.Sprintf("/proc/%d/fd", pid)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	for _, e := range entries {
		full := filepath.Join(dir, e.Name())
		target, err := os.Readlink(full)
		if err != nil {
			fmt.Fprintf(&buf, "%s -> (readlink failed: %v)\n", e.Name(), err)
			continue
		}
		fmt.Fprintf(&buf, "%s -> %s\n", e.Name(), target)
	}
	return os.WriteFile(dst, buf.Bytes(), 0o644)
}

// curlPprof GETs url and writes the response body to dst. The
// pprof endpoints serve binary protobuf — we don't decode here, just
// land the bytes for later `go tool pprof` analysis.
func curlPprof(ctx context.Context, hc *http.Client, url, dst string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.Copy(f, resp.Body)
	return err
}

// hasBinary reports whether cmd is on $PATH (exec.LookPath returns
// nil error). Used to gate gcore / dmesg / readelf-style optional
// dependencies. Safe to call from forensics-critical paths.
func hasBinary(cmd string) bool {
	_, err := exec.LookPath(cmd)
	return err == nil
}

// runGCore invokes `gcore -o <dst-prefix> <pid>`. gcore writes the
// core file at `<dst-prefix>.<pid>`; we rename it to a stable name
// so the dossier reader doesn't need to know the runtime PID.
func runGCore(ctx context.Context, pid int, dstPrefix string) error {
	cmd := exec.CommandContext(ctx, "gcore", "-o", dstPrefix, fmt.Sprintf("%d", pid))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	// gcore writes <dst-prefix>.<pid>; flatten to <dst-prefix>
	// for portability.
	wrote := fmt.Sprintf("%s.%d", dstPrefix, pid)
	if _, err := os.Stat(wrote); err == nil {
		return os.Rename(wrote, dstPrefix)
	}
	return nil
}

// dmesgSince captures the kernel ring buffer for the past `window`
// duration. Requires root on Linux 5.x+; non-root falls back to the
// `dmesg` binary's behaviour (usually nothing useful, but we write
// what we get).
func dmesgSince(ctx context.Context, window time.Duration, dst string) error {
	// --since= accepts ISO timestamps OR durations on util-linux 2.37+.
	// Older versions: best-effort tail of full dmesg.
	cmd := exec.CommandContext(ctx, "dmesg", "--since", "-"+window.String(), "--time-format", "iso")
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Fall back to plain dmesg (no time filter); tail the
		// last 100 lines to keep the file size sane.
		cmd = exec.CommandContext(ctx, "dmesg")
		out, err = cmd.CombinedOutput()
		if err != nil {
			return err
		}
		lines := strings.Split(string(out), "\n")
		if len(lines) > 100 {
			lines = lines[len(lines)-100:]
		}
		out = []byte(strings.Join(lines, "\n"))
	}
	return os.WriteFile(dst, out, 0o644)
}

// writePlainText writes text to path with 0644 perms, creating the
// parent dir if needed.
func writePlainText(path, text string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(text), 0o644)
}
