package fault

import (
	"strings"
	"testing"
	"time"
)

func TestGenerate_Deterministic(t *testing.T) {
	cfg := GenerateConfig{
		Seed:              0xc0ffee,
		RunDuration:       2 * time.Hour,
		CelerisPID:        12345,
		CelerisListenFD:   7,
		CelerisListenPort: 8080,
		LoadgenIface:      "eth0",
	}
	a := Generate(cfg)
	b := Generate(cfg)
	if len(a) != len(b) {
		t.Fatalf("non-deterministic lengths: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Offset != b[i].Offset || a[i].Duration != b[i].Duration {
			t.Errorf("entry %d differs: %+v vs %+v", i, a[i], b[i])
		}
		if a[i].Fault.String() != b[i].Fault.String() {
			t.Errorf("entry %d fault mismatch: %q vs %q", i,
				a[i].Fault.String(), b[i].Fault.String())
		}
	}
}

func TestGenerate_SeedSensitive(t *testing.T) {
	base := GenerateConfig{
		Seed:              1,
		RunDuration:       2 * time.Hour,
		CelerisPID:        1,
		CelerisListenPort: 1,
	}
	other := base
	other.Seed = 2
	a := Generate(base)
	b := Generate(other)
	if len(a) == 0 || len(b) == 0 {
		t.Fatalf("empty schedules: %d %d", len(a), len(b))
	}
	same := true
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i].Fault.String() != b[i].Fault.String() || a[i].Offset != b[i].Offset {
			same = false
			break
		}
	}
	if same {
		t.Fatal("two distinct seeds produced identical schedule")
	}
}

func TestGenerate_BoundedAndSorted(t *testing.T) {
	cfg := GenerateConfig{
		Seed:              42,
		RunDuration:       time.Hour,
		CelerisPID:        99,
		CelerisListenPort: 9090,
	}
	s := Generate(cfg)
	if len(s) == 0 {
		t.Fatal("empty schedule")
	}
	for i := 1; i < len(s); i++ {
		if s[i-1].Offset > s[i].Offset {
			t.Errorf("schedule not sorted at %d", i)
		}
	}
	for _, sf := range s {
		end := sf.Offset + sf.Duration
		if end > cfg.RunDuration {
			t.Errorf("fault %s extends past run duration: end=%s", sf.Fault, end)
		}
	}
}

func TestGenerate_SkipsPIDDependentWithoutPID(t *testing.T) {
	cfg := GenerateConfig{
		Seed:              1,
		RunDuration:       time.Hour,
		CelerisListenPort: 8080,
		LoadgenIface:      "eth0",
	}
	s := Generate(cfg)
	for _, sf := range s {
		name := sf.Fault.String()
		if strings.Contains(name, "signal-pause") || strings.Contains(name, "fd-pressure") {
			t.Errorf("PID-dependent fault scheduled without PID: %s", name)
		}
	}
}

func TestSchedule_StringNonEmpty(t *testing.T) {
	s := Schedule{{
		Offset:   10 * time.Second,
		Duration: 30 * time.Second,
		Fault:    &TCNetem{Iface: "eth0", DelayMs: 10},
	}}
	got := s.String()
	if !strings.Contains(got, "tc-netem") {
		t.Fatalf("unexpected string: %q", got)
	}
}

func TestSchedule_StringEmpty(t *testing.T) {
	s := Schedule{}
	if got := s.String(); !strings.Contains(got, "empty") {
		t.Fatalf("expected empty marker: %q", got)
	}
}

func TestFaults_StringsStable(t *testing.T) {
	for _, f := range []Fault{
		&TCNetem{Host: HostClient, Iface: "eth0", DelayMs: 10, JitterMs: 5},
		&SignalPause{Host: HostServer, PID: 100},
		&FDPressure{Host: HostServer, PID: 100, SoftLimit: 200, RestoreLimit: 1048576},
		&IPTablesDrop{Host: HostServer, Chain: "INPUT", Port: 8080},
		&ListenFDClose{Host: HostServer, PID: 100, FD: 5},
	} {
		s := f.String()
		if s == "" {
			t.Errorf("%T.String() returned empty", f)
		}
	}
}
