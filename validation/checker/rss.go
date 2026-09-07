package checker

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// ReadRSS returns the resident set size of pid in bytes from
// /proc/<pid>/status (VmRSS), or 0 when it cannot be read: pid <= 0,
// a non-linux host, a remote (ssh-driven) refapp, or a process that has
// already exited. Zero means "not sampled" to I-MEM-4, which then skips.
func ReadRSS(pid int) int64 {
	if pid <= 0 {
		return 0
	}
	f, err := os.Open(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()
	rss, _ := ParseVmRSS(f)
	return rss
}

// ParseVmRSS extracts the VmRSS line ("VmRSS:\t  123456 kB") from a
// /proc/<pid>/status document and returns it in bytes. Returns an error
// when the line is absent or malformed.
func ParseVmRSS(r io.Reader) (int64, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "VmRSS:"))
		if len(fields) < 1 {
			return 0, fmt.Errorf("malformed VmRSS line %q", line)
		}
		n, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("malformed VmRSS line %q: %w", line, err)
		}
		unit := "kB"
		if len(fields) > 1 {
			unit = fields[1]
		}
		switch strings.ToLower(unit) {
		case "kb":
			return n * 1024, nil
		case "mb":
			return n * 1024 * 1024, nil
		case "b", "bytes":
			return n, nil
		default:
			return 0, fmt.Errorf("unknown VmRSS unit %q", unit)
		}
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("no VmRSS line")
}
