package main

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
)

// fmtK writes a rate in thousands of requests a second to 3 significant digits:
// 8350 -> "8.35k", 10310 -> "10.3k".
func fmtK(rps float64) string {
	return strconv3(rps/1000) + "k"
}

func strconv3(f float64) string {
	s := fmt.Sprintf("%.3g", f)
	if strings.ContainsAny(s, "e") {
		s = fmt.Sprintf("%.0f", f)
	}
	return s
}

// fmtMs writes a duration in milliseconds: 6.5ms -> "6.5".
func fmtMs(d time.Duration) string {
	return fmt.Sprintf("%g", float64(d)/float64(time.Millisecond))
}

func fmtMB(b float64) string { return fmt.Sprintf("%.1f", b/1e6) }

// Timer lateness of 0.1 to 0.4 ms only lowers a start-to-start period's rate
// (a start is booked from the actual start of the last).
const (
	latenessLo = 100 * time.Microsecond
	latenessHi = 400 * time.Microsecond
)

// expectedRange is the fleet rate to expect once timer lateness is counted, and
// the same as a share above the floor.
func (r Result) expectedRange() (lo, hi float64) {
	w := float64(r.Measured.MarkovWalkers)
	return w / (r.Period + latenessHi).Seconds(), w / (r.Period + latenessLo).Seconds()
}

func (r Result) scenario(name string) Scenario {
	for _, s := range r.Scenarios {
		if s.Name == name {
			return s
		}
	}
	return Scenario{}
}

// matrixBlock is the derivation's comment in cmd/validator/matrix.go, between
// its raceshape:derived markers (no trailing newline).
func matrixBlock(r Result) string {
	lo, hi := r.expectedRange()
	m := r.Measured
	a, b, c := r.scenario("A"), r.scenario("B"), r.scenario("C")
	lines := []string{
		"//   - Floor: the controller refuses to promote on a tick whose bytes per request",
		fmt.Sprintf("//     (engine read+written bytes over engine requests) is >= %.0f, and a healthy", r.Threshold),
		fmt.Sprintf("//     64 KiB echo adds up to ~%.0f MB a tick without counting a request. With the", m.EchoBytesPerTick/1e6),
		fmt.Sprintf("//     %.0f B/request of a pure epoll cell: >= %s requests a second; with the", m.BaselineBytesPerReq, fmtK(a.FloorRPS)),
		fmt.Sprintf("//     %.0f B/request this cell ran at: >= %s; with that cell's ~%.0f MB a second", m.CellBytesPerReq, fmtK(b.FloorRPS), m.CellExtraBytesPerTick/1e6),
		fmt.Sprintf("//     of bytes that do not scale with requests stacked on top: >= %s. Taken", fmtK(c.FloorRPS)),
		fmt.Sprintf("//     as the floor: %s.", fmtK(r.FloorRPS)),
		fmt.Sprintf("//   - Chosen: %s ms. The ceiling is %d / %s ms = %s a second; a start is", fmtMs(r.Period), m.MarkovWalkers, fmtMs(r.Period), fmtK(r.CeilingRPS)),
		"//     booked from the actual start of the last, so timer lateness (0.1 to 0.4 ms)",
		fmt.Sprintf("//     only lowers it, to ~%s to %s: %d to %d%% above the floor, and %d%% of the", trimK(lo), fmtK(hi), pct(lo/r.FloorRPS-1), pct(hi/r.FloorRPS-1), pct(hi/m.SaturatingRPS)),
		"//     rate that saturated. Predicted, not measured: the acceptance read decides.",
		"//", // gofmt wants a blank line between a list and the next paragraph
	}
	return strings.Join(lines, "\n")
}

// trimK is fmtK without the unit, for a range written "9.71 to 10.1k".
func trimK(rps float64) string { return strings.TrimSuffix(fmtK(rps), "k") }

func pct(f float64) int { return int(math.Floor(f * 100)) }

var matrixConstRe = regexp.MustCompile(`(?m)^const raceAdaptiveMarkovMinPeriod = (\d+) \* time\.Microsecond$`)

// ApplyMatrix rewrites the period constant and the derivation comment of
// cmd/validator/matrix.go.
func ApplyMatrix(src string, r Result) (string, error) {
	if len(matrixConstRe.FindAllStringIndex(src, -1)) != 1 {
		return "", fmt.Errorf("matrix.go: expected exactly one `const raceAdaptiveMarkovMinPeriod = N * time.Microsecond` line")
	}
	out := matrixConstRe.ReplaceAllString(src, fmt.Sprintf("const raceAdaptiveMarkovMinPeriod = %d * time.Microsecond", r.Period.Microseconds()))
	return replaceBlock(out, "derived", matrixBlock(r), "matrix.go")
}

// pinBlock is the Go constants the tests pin, between their raceshape:pin
// markers in cmd/validator/race_shape_test.go.
func pinBlock(r Result, celerisVersion string) string {
	return strings.Join([]string{
		"// Written by `go run ./tools/raceshape repin`; do not edit by hand.",
		"const (",
		fmt.Sprintf("\tpinnedCelerisVersion    = %q", celerisVersion),
		fmt.Sprintf("\tpinnedMarkovMinPeriod   = %d * time.Microsecond", r.Period.Microseconds()),
		fmt.Sprintf("\tpinnedPromotionFloorRPS = %.1f", r.FloorRPS),
		")",
		"", // gofmt wants a blank line before the directive-like marker comment
	}, "\n")
}

// ApplyPins rewrites the pin block of cmd/validator/race_shape_test.go.
func ApplyPins(src string, r Result, celerisVersion string) (string, error) {
	return replaceBlock(src, "pin", pinBlock(r, celerisVersion), "race_shape_test.go")
}

// preregFloor is the promotion-floor table of PREREG.md, between its
// raceshape:floor markers.
func preregFloor(r Result, celerisVersion string) string {
	m := r.Measured
	a, b, c := r.scenario("A"), r.scenario("B"), r.scenario("C")
	row := func(s Scenario, text string) string {
		return fmt.Sprintf("| %s: %s | %.0f | %s | %s/s |", s.Name, text, s.BytesPerReq, fmtMB(s.BytesTick), fmtK(s.FloorRPS))
	}
	return strings.Join([]string{
		fmt.Sprintf("Derived by `go run ./tools/raceshape repin` from celeris `internal/adaptive/controller.go` and `telemetry.go` at the pinned %s: the controller refuses a tick whose bytes per request is >= %.0f (single-tick snap and two-tick sustain alike); a tick is 1 s; the tick's bytes are the engine's read+written bytes and its requests are the engine's request count, both differenced between ticks. With R the engine request rate and X the bytes per tick that do not scale with R, a tick is promotable when R > X / (%.0f - b0).", celerisVersion, r.Threshold, r.Threshold),
		"",
		"| scenario | b0 (B/request) | X (MB/tick) | R > |",
		"|---|---|---|---|",
		row(a, "pure-engine baseline, healthy echo"),
		row(b, "this cell's own ratio, healthy echo"),
		row(c, fmt.Sprintf("pure-engine baseline, healthy echo, plus this cell's ~%.0f MB/s of bytes that do not scale with requests", m.CellExtraBytesPerTick/1e6)),
		"",
		fmt.Sprintf("The floor taken is the largest reading rounded up to %d requests/s: **%s requests/s**.", FloorGranularity, fmtK(r.FloorRPS)),
	}, "\n")
}

// preregPeriod is the period paragraph of PREREG.md, between its
// raceshape:period markers.
func preregPeriod(r Result) string {
	lo, hi := r.expectedRange()
	m := r.Measured
	return strings.Join([]string{
		fmt.Sprintf("Rule (`tools/raceshape`): the longest period on a %v grid whose ceiling is at least %.0f%% above the floor, and whose ceiling is at most %.0f%% of the %s requests/s that saturated the cell (a period between %v and %v).", PeriodGrid, (FloorMargin-1)*100, SaturatingFraction*100, fmtK(m.SaturatingRPS), r.MinPeriod.Round(10*time.Microsecond), r.MaxPeriod.Round(10*time.Microsecond)),
		"",
		fmt.Sprintf("Period: **%s ms** (`raceAdaptiveMarkovMinPeriod`). Fleet ceiling (starts, re-logins included) = %d Markov walkers / %s ms = **%s/s**, %d%% of the %s/s that saturated. A start is booked from the actual start of the last, so timer lateness (0.1 to 0.4 ms) only lowers the rate: ~%s to %s/s, %d%% to %d%% above the %s floor. Predicted, not measured.",
			fmtMs(r.Period), m.MarkovWalkers, fmtMs(r.Period), fmtK(r.CeilingRPS), pct(r.Saturating), fmtK(m.SaturatingRPS),
			trimK(lo), fmtK(hi), pct(lo/r.FloorRPS-1), pct(hi/r.FloorRPS-1), fmtK(r.FloorRPS)),
	}, "\n")
}

// ApplyPrereg rewrites the two generated blocks of PREREG.md.
func ApplyPrereg(src string, r Result, celerisVersion string) (string, error) {
	out, err := replaceBlock(src, "floor", preregFloor(r, celerisVersion), "PREREG.md")
	if err != nil {
		return "", err
	}
	return replaceBlock(out, "period", preregPeriod(r), "PREREG.md")
}

var readerFloorRe = regexp.MustCompile(`(?m)^FLOOR = [0-9.]+`)

// ApplyReader rewrites the reader's promotion floor and its period default
// (read-shaped-cell.py), given the period the file carries now.
func ApplyReader(src string, r Result, oldPeriodUs int64) (string, error) {
	if len(readerFloorRe.FindAllStringIndex(src, -1)) != 1 {
		return "", fmt.Errorf("read-shaped-cell.py: expected exactly one `FLOOR = N` line")
	}
	out := readerFloorRe.ReplaceAllString(src, fmt.Sprintf("FLOOR = %.1f", r.FloorRPS))
	old := fmt.Sprint(oldPeriodUs)
	if !strings.Contains(out, old) {
		return "", fmt.Errorf("read-shaped-cell.py: the period %s us is not in the file", old)
	}
	return strings.ReplaceAll(out, old, fmt.Sprint(r.Period.Microseconds())), nil
}

// replaceBlock replaces what stands between the lines holding "raceshape:NAME
// begin" and "raceshape:NAME end" with body, keeping both marker lines. A
// missing or repeated marker is an error: a block that is not there is not
// silently skipped.
func replaceBlock(src, name, body, file string) (string, error) {
	begin, end := "raceshape:"+name+" begin", "raceshape:"+name+" end"
	if strings.Count(src, begin) != 1 || strings.Count(src, end) != 1 {
		return "", fmt.Errorf("%s: expected exactly one %q and one %q marker", file, begin, end)
	}
	lines := strings.Split(src, "\n")
	bi, ei := -1, -1
	for i, l := range lines {
		if strings.Contains(l, begin) {
			bi = i
		}
		if strings.Contains(l, end) {
			ei = i
		}
	}
	if bi < 0 || ei < 0 || ei <= bi {
		return "", fmt.Errorf("%s: the %q marker must come before the %q marker, each on its own line", file, begin, end)
	}
	out := append([]string{}, lines[:bi+1]...)
	out = append(out, strings.Split(body, "\n")...)
	out = append(out, lines[ei:]...)
	return strings.Join(out, "\n"), nil
}

// currentPeriodUs reads the period constant out of matrix.go.
func currentPeriodUs(matrixSrc string) (int64, error) {
	m := matrixConstRe.FindStringSubmatch(matrixSrc)
	if m == nil {
		return 0, fmt.Errorf("matrix.go: no `const raceAdaptiveMarkovMinPeriod = N * time.Microsecond` line")
	}
	var us int64
	if _, err := fmt.Sscan(m[1], &us); err != nil {
		return 0, err
	}
	return us, nil
}
