package validation

import (
	"io"
	"strings"
	"testing"
)

// The scan keeps the text of a race report, header to closing rule, so
// the dossier names the sites and not only the count.
func TestSuperviseStderr_KeepsRaceReportText(t *testing.T) {
	report := strings.Join([]string{
		"ready addr=127.0.0.1:1",
		"2026/09/13 02:37:10 INFO io_uring engine listening addr=127.0.0.1:23159",
		"==================",
		"WARNING: DATA RACE",
		"Read at 0x00c0003843c0 by goroutine 101:",
		"  github.com/goceleris/celeris/engine/iouring.(*Engine).Metrics()",
		"      engine/iouring/engine.go:335 +0xc4",
		"",
		"Previous write at 0x00c0003843c0 by main goroutine:",
		"  github.com/goceleris/celeris/engine/iouring.(*Engine).Listen()",
		"      engine/iouring/engine.go:240 +0x1172",
		"==================",
		"2026/09/13 02:37:11 INFO still serving",
	}, "\n") + "\n"
	l := &livenessTally{}
	superviseStderr(io.NopCloser(strings.NewReader(report)), l, func(string) {}, func(error) {}, func() {})
	s := l.snapshot()
	if s.RaceReports != 1 {
		t.Fatalf("race_reports = %d, want 1", s.RaceReports)
	}
	if len(s.RaceReportSamples) != 1 {
		t.Fatalf("want one sample, got %d", len(s.RaceReportSamples))
	}
	got := s.RaceReportSamples[0]
	for _, want := range []string{"WARNING: DATA RACE", "engine.go:335", "engine.go:240", "Previous write"} {
		if !strings.Contains(got, want) {
			t.Errorf("sample lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "still serving") {
		t.Errorf("sample ran past the closing rule:\n%s", got)
	}
	if s.Crashed {
		t.Error("a race report is not a crash")
	}
}

// Every report is counted; only the first raceSampleMax are kept.
func TestSuperviseStderr_BoundsRaceSamples(t *testing.T) {
	var b strings.Builder
	b.WriteString("ready addr=127.0.0.1:1\n")
	for i := 0; i < raceSampleMax+2; i++ {
		b.WriteString("==================\nWARNING: DATA RACE\nRead at 0x1 by goroutine 1:\n  f()\n      x.go:1\n==================\n")
	}
	l := &livenessTally{}
	superviseStderr(io.NopCloser(strings.NewReader(b.String())), l, func(string) {}, func(error) {}, func() {})
	s := l.snapshot()
	if s.RaceReports != int64(raceSampleMax+2) {
		t.Fatalf("race_reports = %d, want %d", s.RaceReports, raceSampleMax+2)
	}
	if len(s.RaceReportSamples) != raceSampleMax {
		t.Fatalf("samples = %d, want %d", len(s.RaceReportSamples), raceSampleMax)
	}
}
