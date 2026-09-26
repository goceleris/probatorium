package report

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var fcT0 = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

func fcTS(d time.Duration) string { return fcT0.Add(d).Format(time.RFC3339Nano) }

// fcCell builds a synthetic artifact for one cell in which /ws was held for
// 8 s at fcT0 and the capture did everything right, then lets a test break
// one piece.
type fcCell struct {
	dir  string
	cell ValidationCellResult
}

func newFCCell(t *testing.T) *fcCell {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cell-00-auth_session_ratelimit-iouring")
	tail := "[fault] hold path=/ws hold=8s start=" + fcTS(0) + "\n[fault] release path=/ws end=" + fcTS(8*time.Second) + " waiters=14\n"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "refapp_stderr_tail.txt"), []byte(tail), 0o644); err != nil {
		t.Fatal(err)
	}
	d := filepath.Join(dir, "incidents", "20260926-120003-I-WS-HANDSHAKE")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	inc, _ := json.Marshal(map[string]any{"predicate": "I-WS-HANDSHAKE", "observed_at": fcTS(3200 * time.Millisecond), "record_only": true})
	stacks := "goroutine 7 [sleep]:\ntime.Sleep(0x1dcd65000)\ngithub.com/goceleris/probatorium/validation/refapp/internal/debugvars.(*FaultHold).run(0xc000123000, ...)\n\n" +
		"goroutine 88 [sync.Mutex.Lock, 3 seconds]:\nsync.(*Mutex).Lock(...)\ngithub.com/goceleris/probatorium/validation/refapp/internal/debugvars.(*FaultHold).wait(...)\n"
	for name, body := range map[string]string{
		"incident.json":          string(inc),
		"goroutine-stacks.txt":   stacks,
		"refapp_stderr_tail.txt": tail,
		"core.skipped":           "gcore skipped\n",
	} {
		if err := os.WriteFile(filepath.Join(d, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &fcCell{dir: dir, cell: ValidationCellResult{
		Refapp: "auth_session_ratelimit", Engine: "iouring", Arch: "arm64",
		Tier1: &Tier1Summary{
			WSTorture: map[string]int64{"ws_sent": 900, "ws_handshake_fail": 5, "ws_handshake_fail_timeout": 5},
			H2CChurn:  map[string]int64{"h2c_sent": 400, "h2c_hang": 0},
			WSSlowReads: []SlowFire{{
				TS: fcTS(2100 * time.Millisecond), DialMs: 0, WriteMs: 0, ReadMs: 2000,
				Outcome: "handshake-fail-timeout", Err: "read tcp 127.0.0.1:51234->127.0.0.1:8080: i/o timeout",
				LocalAddr: "127.0.0.1:51234", RemoteAddr: "127.0.0.1:8080",
			}},
		},
	}}
}

func (c *fcCell) dossier() string {
	return filepath.Join(c.dir, "incidents", "20260926-120003-I-WS-HANDSHAKE")
}

func (c *fcCell) write(t *testing.T, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(c.dossier(), name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func wantFail(t *testing.T, r FaultControlResult, part string) {
	t.Helper()
	if r.Pass() {
		t.Fatalf("passed, want a failure mentioning %q; findings: %v", part, r.Findings)
	}
	for _, f := range r.Failures {
		if strings.Contains(f, part) {
			return
		}
	}
	t.Fatalf("failures %v do not mention %q", r.Failures, part)
}

// The positive case: every piece present, and the findings spell out the
// root cause a reader would derive from the artifact.
func TestFaultControlWSInjectionPasses(t *testing.T) {
	c := newFCCell(t)
	r := CheckFaultControlCell(c.dir, c.cell, []string{"/ws"})
	if !r.Pass() {
		t.Fatalf("failures: %v", r.Failures)
	}
	all := strings.Join(r.Findings, "\n")
	for _, want := range []string{"+2.1s after its start", "read 2000 ms", "127.0.0.1:51234", "observed at +3.2s", "1 request goroutine(s) blocked", "held lock on /ws"} {
		if !strings.Contains(all, want) {
			t.Errorf("findings lack %q:\n%s", want, all)
		}
	}
	if len(r.Injections) != 1 || r.Injections[0].Waiters != 14 || r.Injections[0].Hold != 8*time.Second {
		t.Errorf("injections %+v", r.Injections)
	}
}

// Each capture defect the control exists to catch fails the cell.
func TestFaultControlCatchesEachCaptureDefect(t *testing.T) {
	for _, tc := range []struct {
		name    string
		breakIt func(*testing.T, *fcCell)
		part    string
	}{
		{"counter never moved", func(_ *testing.T, c *fcCell) { c.cell.Tier1.WSTorture["ws_handshake_fail_timeout"] = 0 }, "gated counter"},
		{"no record in the ring", func(_ *testing.T, c *fcCell) { c.cell.Tier1.WSSlowReads = nil }, "no \"handshake-fail-timeout\" record"},
		{"record outside the hold", func(_ *testing.T, c *fcCell) { c.cell.Tier1.WSSlowReads[0].TS = fcTS(30 * time.Second) }, "no \"handshake-fail-timeout\" record"},
		{"record without addresses", func(_ *testing.T, c *fcCell) { c.cell.Tier1.WSSlowReads[0].LocalAddr = "" }, "no socket addresses"},
		{"wrong leg expired", func(_ *testing.T, c *fcCell) { c.cell.Tier1.WSSlowReads[0].ReadMs = 5 }, "READ leg"},
		{"validator stalled", func(_ *testing.T, c *fcCell) { c.cell.Tier1.WSSlowReads[0].ValidatorSkewMs = 1800 }, "validator itself stalled"},
		{"dump taken after the release", func(t *testing.T, c *fcCell) {
			inc, _ := json.Marshal(map[string]any{"observed_at": fcTS(11 * time.Second)})
			c.write(t, "incident.json", string(inc))
		}, "outside the hold"},
		{"dump does not name the holder", func(t *testing.T, c *fcCell) {
			c.write(t, "goroutine-stacks.txt", "goroutine 1 [running]:\nmain.main()\n")
		}, "holder / 0 waiter"},
		{"no text dump", func(t *testing.T, c *fcCell) { _ = os.Remove(filepath.Join(c.dossier(), "goroutine-stacks.txt")) }, "no goroutine-stacks.txt"},
		{"gcore paused the refapp", func(t *testing.T, c *fcCell) { _ = os.Remove(filepath.Join(c.dossier(), "core.skipped")) }, "core.skipped"},
		{"no dossier", func(t *testing.T, c *fcCell) { _ = os.RemoveAll(filepath.Join(c.dir, "incidents")) }, "no incidents/*-I-WS-HANDSHAKE dossier"},
		{"fault never fired", func(t *testing.T, c *fcCell) {
			_ = os.WriteFile(filepath.Join(c.dir, "refapp_stderr_tail.txt"), []byte("nothing\n"), 0o644)
		}, "never logged the hold"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newFCCell(t)
			tc.breakIt(t, c)
			wantFail(t, CheckFaultControlCell(c.dir, c.cell, []string{"/ws"}), tc.part)
		})
	}
}

// The false-positive control: a path the run did not inject must show none
// of its events, and a clean un-injected cell passes.
func TestFaultControlUninjectedPathMustBeClean(t *testing.T) {
	clean := ValidationCellResult{Refapp: "kitchen_sink", Engine: "epoll", Arch: "amd64", Tier1: &Tier1Summary{
		H2CChurn: map[string]int64{"h2c_sent": 500}, WSTorture: map[string]int64{"ws_sent": 0}}}
	dir := t.TempDir()
	if r := CheckFaultControlCell(dir, clean, nil); !r.Pass() {
		t.Fatalf("clean un-injected cell failed: %v", r.Failures)
	}
	dirty := clean
	dirty.Tier1 = &Tier1Summary{H2CChurn: map[string]int64{"h2c_sent": 500, "h2c_hang": 1}}
	wantFail(t, CheckFaultControlCell(dir, dirty, nil), "(not injected): h2c_churn.h2c_hang=1")

	// And in an injected cell, the OTHER path is held to zero too.
	c := newFCCell(t)
	c.cell.Tier1.H2CChurn["h2c_hang"] = 2
	wantFail(t, CheckFaultControlCell(c.dir, c.cell, []string{"/ws"}), "/ (not injected)")
}

func TestParseFaultLogPairsHoldAndRelease(t *testing.T) {
	inj := ParseFaultLog([]string{
		"noise",
		"[fault] hold path=/ws hold=8s start=" + fcTS(0),
		"[fault] hold path=/ hold=40s start=" + fcTS(30*time.Second),
		"[fault] release path=/ws end=" + fcTS(8*time.Second) + " waiters=3",
	})
	if len(inj) != 2 || inj[0].Path != "/ws" || inj[0].Waiters != 3 || !inj[0].End.Equal(fcT0.Add(8*time.Second)) ||
		inj[1].Path != "/" || inj[1].Hold != 40*time.Second || inj[1].Waiters != -1 || !inj[1].End.IsZero() {
		t.Fatalf("parsed %+v", inj)
	}
}
