package main

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// busyPollLineRE is the contract with arm-validate.py / x86-validate.py (BUSYPOLL): the same
// expression, character for character, is in both validators.
var busyPollLineRE = regexp.MustCompile(`^\[busypoll\] env=(\S+) config_us=(\d+|absent) resolved_us=(\d+|absent) resolved=(\S+)$`)

func celerisHasBusyPollField() bool {
	_, ok := reflect.TypeOf(celeris.Config{}).FieldByName(busyPollField)
	return ok
}

func TestParseBusyPoll(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
		err  string // substring, "" = ok
	}{
		{"50us", 50 * time.Microsecond, ""},
		{"50µs", 50 * time.Microsecond, ""},
		{"1us", time.Microsecond, ""},
		{"1ms", time.Millisecond, ""},
		{"0", 0, ""},
		{"0s", 0, ""},
		{"2147483647us", 2147483647 * time.Microsecond, ""},
		{"", 0, "set but empty"},
		{"fifty", 0, "invalid duration"},
		{"50", 0, "missing unit"},
		{"-5us", 0, "negative"},
		{"999ns", 0, "under one microsecond"},
		{"1ns", 0, "under one microsecond"},
		{"2147483648us", 0, "over"},
	} {
		got, err := parseBusyPoll(tc.in)
		if tc.err == "" {
			if err != nil || got != tc.want {
				t.Errorf("parseBusyPoll(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("parseBusyPoll(%q) = %v, %v; want an error containing %q", tc.in, got, err, tc.err)
		}
	}
}

// The knob on the pinned celeris. With the field (celeris >= v1.6.0) the env reaches
// Config.SocketBusyPoll and Server.SocketBusyPoll reports it; without it (the 713b40d pin of the
// P2c-P2h lineage, v1.5.11) the env is a fatal error and the line says "absent", never 0.
func TestApplyBusyPollKnob(t *testing.T) {
	has := celerisHasBusyPollField()
	build := func(t *testing.T) (busyPollState, error) {
		t.Helper()
		cfg := celeris.Config{Engine: celeris.IOUring}
		st, err := applyBusyPollKnob(&cfg)
		if err != nil {
			return st, err
		}
		st.resolveBusyPoll(celeris.New(cfg))
		return st, nil
	}

	t.Run("unset", func(t *testing.T) {
		st, err := build(t)
		if err != nil {
			t.Fatal(err)
		}
		m := busyPollLineRE.FindStringSubmatch(st.line())
		if m == nil || m[1] != "unset" {
			t.Fatalf("line %q does not match the contract with env=unset", st.line())
		}
		if has {
			if st.cfgUS != 0 || !st.resolvedPresent || st.resolvedUS != 0 || m[2] != "0" || m[3] != "0" {
				t.Errorf("unset env on a celeris with the field must read config 0 / resolved 0, got %q", st.line())
			}
		} else if m[2] != busyPollAbsent || m[3] != busyPollAbsent || m[4] != busyPollAbsent {
			t.Errorf("unset env on a celeris WITHOUT the field must read absent (its engines busy-poll unconditionally; 0 would be a lie), got %q", st.line())
		}
	})

	t.Run("50us", func(t *testing.T) {
		t.Setenv(busyPollEnv, "50us")
		st, err := build(t)
		if !has {
			if err == nil || !strings.Contains(err.Error(), "no Config."+busyPollField) {
				t.Fatalf("env set on a celeris without the field must fail loudly, got %v", err)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if !st.cfgPresent || st.cfgUS != 50 || !st.resolvedPresent || st.resolvedUS != 50 || st.resolved != 50*time.Microsecond {
			t.Errorf("50us must reach Config and Server.SocketBusyPoll, got %+v", st)
		}
		m := busyPollLineRE.FindStringSubmatch(st.line())
		if m == nil || m[1] != "50us" || m[2] != "50" || m[3] != "50" {
			t.Errorf("line %q", st.line())
		}
	})

	t.Run("explicit-zero", func(t *testing.T) {
		t.Setenv(busyPollEnv, "0")
		st, err := build(t)
		if !has {
			if err == nil {
				t.Fatal("env set on a celeris without the field must fail loudly")
			}
			return
		}
		if err != nil || st.cfgUS != 0 || st.resolvedUS != 0 {
			t.Errorf("explicit 0 = off, got %+v, %v", st, err)
		}
	})

	for _, bad := range []string{"", "fifty", "-1us", "500ns"} {
		t.Run("bad-"+bad, func(t *testing.T) {
			t.Setenv(busyPollEnv, bad)
			if _, err := build(t); err == nil {
				t.Errorf("env %q must be a fatal start error, got none", bad)
			}
		})
	}
}

func TestApplyBusyPollKnobRefusesAWrongConfigShape(t *testing.T) {
	if _, err := applyBusyPollKnob(celeris.Config{}); err == nil {
		t.Error("a Config passed by value must be refused (the field could not be set)")
	}
	type other struct{ SocketBusyPoll int }
	t.Setenv(busyPollEnv, "50us")
	if _, err := applyBusyPollKnob(&other{}); err == nil || !strings.Contains(err.Error(), "want time.Duration") {
		t.Errorf("a field of another type must be refused, got %v", err)
	}
}

func TestBusyPollLineFormat(t *testing.T) {
	for _, st := range []busyPollState{
		{},
		{envSet: true, env: "50us", cfgPresent: true, cfgUS: 50, resolvedPresent: true, resolvedUS: 50, resolved: 50 * time.Microsecond},
		{cfgPresent: true, resolvedPresent: true},
		{envSet: true, env: "0", cfgPresent: true, resolvedPresent: true},
	} {
		if !busyPollLineRE.MatchString(st.line()) {
			t.Errorf("line %q does not match the contract", st.line())
		}
	}
	if got := (busyPollState{}).line(); got != "[busypoll] env=unset config_us=absent resolved_us=absent resolved=absent" {
		t.Errorf("absent line = %q", got)
	}
	if got := (busyPollState{envSet: true, env: "50us", cfgPresent: true, cfgUS: 50, resolvedPresent: true, resolvedUS: 50, resolved: 50 * time.Microsecond}).line(); got != "[busypoll] env=50us config_us=50 resolved_us=50 resolved=50µs" {
		t.Errorf("on line = %q", got)
	}
}
