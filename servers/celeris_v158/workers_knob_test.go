package main

import (
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/goceleris/celeris"
)

// workersLineRE is the contract with the P3a validator: the same expression is pinned there.
var workersLineRE = regexp.MustCompile(`^\[workers\] env=(\S+) config=(\d+|absent)$`)

func celerisHasWorkersField() bool {
	f, ok := reflect.TypeOf(celeris.Config{}).FieldByName(workersField)
	return ok && f.Type.Kind() == reflect.Int
}

// unsetWorkersEnv leaves PROBATORIUM_WORKERS unset for the test and restores the caller's value after it.
func unsetWorkersEnv(t *testing.T) {
	t.Helper()
	t.Setenv(workersEnv, "")
	os.Unsetenv(workersEnv)
}

func TestParseWorkers(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
		err  string // substring, "" = ok
	}{
		{"1", 1, ""},
		{"12", 12, ""},
		{"14", 14, ""},
		{"16", 16, ""},
		{"4096", 4096, ""},
		{"", 0, "set but empty"},
		{"0", 0, "positive decimal"},
		{"-1", 0, "positive decimal"},
		{"+14", 0, "positive decimal"},
		{"014", 0, "positive decimal"},
		{" 14", 0, "positive decimal"},
		{"14 ", 0, "positive decimal"},
		{"1.5", 0, "positive decimal"},
		{"0x10", 0, "positive decimal"},
		{"fourteen", 0, "positive decimal"},
		{"4097", 0, "over"},
		{"99999", 0, "positive decimal"},
	} {
		got, err := parseWorkers(tc.in)
		if tc.err == "" {
			if err != nil || got != tc.want {
				t.Errorf("parseWorkers(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("parseWorkers(%q) = %v, %v; want an error containing %q", tc.in, got, err, tc.err)
		}
	}
}

// Unset keeps the celeris default (k0 is the genuine default, never an explicit 16); set reaches
// Config.Workers; a bad value is an error; a celeris without the int field refuses the env.
func TestApplyWorkersKnob(t *testing.T) {
	has := celerisHasWorkersField()

	t.Run("unset", func(t *testing.T) {
		unsetWorkersEnv(t)
		cfg := celeris.Config{}
		st, err := applyWorkersKnob(&cfg)
		if err != nil {
			t.Fatal(err)
		}
		if st.envSet || st.cfgPresent != has {
			t.Errorf("state %+v, want envSet=false cfgPresent=%v", st, has)
		}
		if has && (st.cfg != 0 || reflect.ValueOf(cfg).FieldByName(workersField).Int() != 0) {
			t.Errorf("unset env changed Config.Workers: state %+v", st)
		}
		want := "[workers] env=unset config=0"
		if !has {
			want = "[workers] env=unset config=absent"
		}
		if got := st.line(); got != want {
			t.Errorf("line %q, want %q", got, want)
		}
	})

	t.Run("set", func(t *testing.T) {
		for _, n := range []int{12, 14, 16} {
			raw := map[int]string{12: "12", 14: "14", 16: "16"}[n]
			t.Setenv(workersEnv, raw)
			cfg := celeris.Config{}
			st, err := applyWorkersKnob(&cfg)
			if !has {
				if err == nil || !strings.Contains(err.Error(), "no int Config.Workers") {
					t.Fatalf("env %s on a celeris without the field: err %v, want the refusal", raw, err)
				}
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := int(reflect.ValueOf(cfg).FieldByName(workersField).Int()); got != n || st.cfg != n {
				t.Errorf("env %s: Config.Workers=%d state %+v", raw, got, st)
			}
			if got, want := st.line(), "[workers] env="+raw+" config="+raw; got != want {
				t.Errorf("line %q, want %q", got, want)
			}
		}
	})

	t.Run("bad", func(t *testing.T) {
		for _, raw := range []string{"", "0", "-3", "abc", "14.0", "4097"} {
			t.Setenv(workersEnv, raw)
			cfg := celeris.Config{}
			if _, err := applyWorkersKnob(&cfg); err == nil {
				t.Errorf("env %q: no error", raw)
			}
			if has && reflect.ValueOf(cfg).FieldByName(workersField).Int() != 0 {
				t.Errorf("env %q: a bad value still changed Config.Workers", raw)
			}
		}
	})

	t.Run("not a struct pointer", func(t *testing.T) {
		unsetWorkersEnv(t)
		if _, err := applyWorkersKnob(celeris.Config{}); err == nil {
			t.Error("a Config by value was accepted")
		}
		if _, err := applyWorkersKnob(nil); err == nil {
			t.Error("nil was accepted")
		}
	})

	t.Run("wrong field type", func(t *testing.T) {
		unsetWorkersEnv(t)
		var fake struct{ Workers string }
		if _, err := applyWorkersKnob(&fake); err == nil || !strings.Contains(err.Error(), "want int") {
			t.Errorf("a string Workers field: err %v", err)
		}
	})
}

func TestWorkersLineFormat(t *testing.T) {
	for _, st := range []workersState{
		{},
		{cfgPresent: true},
		{envSet: true, env: "14", cfgPresent: true, cfg: 14},
		{envSet: true, env: "12", cfgPresent: false},
	} {
		if !workersLineRE.MatchString(st.line()) {
			t.Errorf("line %q does not match %s", st.line(), workersLineRE)
		}
	}
}

// A bad value must END the process, not start a default SUT: log.Fatalf in a child process.
func TestMustApplyWorkersKnobIsFatal(t *testing.T) {
	if os.Getenv("P3A_WORKERS_FATAL_CHILD") == "1" {
		cfg := celeris.Config{}
		mustApplyWorkersKnob(&cfg)
		os.Exit(0) // reached only when the knob did not stop the process
	}
	for _, raw := range []string{"banana", "0", ""} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestMustApplyWorkersKnobIsFatal$")
		cmd.Env = append(os.Environ(), "P3A_WORKERS_FATAL_CHILD=1", workersEnv+"="+raw)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Errorf("env %q: the child exited 0, want a fatal start error; output %s", raw, out)
			continue
		}
		if !strings.Contains(string(out), "workers knob") {
			t.Errorf("env %q: output does not name the knob: %s", raw, out)
		}
	}
}

// Source order of server.go (the knob is applied between the cfg literal and celeris.New, and the
// [workers] line follows the [busypoll] line); a refactor that moves them breaks the arm silently.
func TestServerAppliesWorkersKnobBeforeNew(t *testing.T) {
	b, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	idx := func(sub string) int {
		i := strings.Index(s, sub)
		if i < 0 {
			t.Fatalf("server.go has no %q", sub)
		}
		return i
	}
	lit, bp, wk, nw, bpLine, wkLine := idx("cfg := celeris.Config{"), idx("applyBusyPollKnob(&cfg)"), idx("mustApplyWorkersKnob(&cfg)"), idx("celeris.New(cfg)"), idx("log.Print(bp.line())"), idx("log.Print(wk.line())")
	if !(lit < bp && bp < wk && wk < nw) {
		t.Errorf("order in server.go: cfg literal %d, busy-poll knob %d, workers knob %d, celeris.New %d", lit, bp, wk, nw)
	}
	if !(nw < bpLine && bpLine < wkLine) {
		t.Errorf("the [workers] line (%d) must follow the [busypoll] line (%d), both after celeris.New (%d)", wkLine, bpLine, nw)
	}
}
