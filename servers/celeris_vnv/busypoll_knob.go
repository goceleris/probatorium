package main

// The busy-poll knob of the bench SUT (v1.6.0 attribution campaign, harness change H2).
//
// Since celeris#1227/#1233 SO_BUSY_POLL is opt-in: Config.SocketBusyPoll, default 0 = off,
// and the drivers follow it. The campaign benches the SAME module twice, once with the default
// ("main", busy poll off) and once with busy poll on ("bpon"), so the second arm is a registry
// column of this module whose SUTEnv carries PROBATORIUM_SOCKET_BUSY_POLL=50us, not a second
// patched copy. 50us restores the pre-v1.6.0 behaviour (Config{SocketBusyPoll: 50*time.Microsecond}).
//
// This file is deliberately the ONLY place that names the field, and it reaches it by reflection,
// for three reasons:
//
//   - it compiles against every celeris this module (or a mirror of it: servers/celeris_vnv,
//     servers/celeris_v1511, the H6 sha copies) may pin, including the 713b40d pin of the P2c-P2h
//     lineage and v1.5.11, whose Config has no SocketBusyPoll and whose engines hard-code 50us;
//   - an API drift (the field renamed or retyped) cannot silently turn a bpon column into a
//     default one: the env set on a celeris without the field is a fatal start error;
//   - it removes the "omit this file in the copies" hazard.
//
// "Unset" does NOT mean "off" on a celeris without the field (its engines busy-poll
// unconditionally), so the startup line never prints 0 for it: it prints the token "absent".
// The validators (arm-validate.py, x86-validate.py) read that line as the direct manipulation
// check: main must read resolved_us=0, bpon resolved_us=50, a v1.5.11 column resolved_us=absent,
// and "absent" on a main or bpon column voids it.

import (
	"fmt"
	"os"
	"reflect"
	"strconv"
	"time"
)

// busyPollEnv names the knob. Empty/unset = the celeris default. The registry carries it as
// "50us" (the SUT env charset has no micro sign).
const busyPollEnv = "PROBATORIUM_SOCKET_BUSY_POLL"

const (
	busyPollField  = "SocketBusyPoll" // celeris.Config field and celeris.Server method
	busyPollMaxUS  = 2147483647       // the kernel option is an int of microseconds
	busyPollAbsent = "absent"
)

// parseBusyPoll parses the env value. 0 is allowed (an explicit "off"); a negative value, a
// value under one microsecond and one over the kernel's int range are errors, as celeris itself
// refuses them at New/Start, so the failure is ours and names the variable.
func parseBusyPoll(raw string) (time.Duration, error) {
	if raw == "" {
		return 0, fmt.Errorf("%s is set but empty (unset it for the default)", busyPollEnv)
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s=%q: %w", busyPollEnv, raw, err)
	}
	switch {
	case d < 0:
		return 0, fmt.Errorf("%s=%q: negative", busyPollEnv, raw)
	case d > 0 && d < time.Microsecond:
		return 0, fmt.Errorf("%s=%q: under one microsecond (the kernel would read 0 and leave busy poll off)", busyPollEnv, raw)
	case d/time.Microsecond > busyPollMaxUS:
		return 0, fmt.Errorf("%s=%q: over %d microseconds", busyPollEnv, raw, busyPollMaxUS)
	}
	return d, nil
}

// busyPollState is what the startup line reports.
type busyPollState struct {
	env             string // the raw env value, "" when unset
	envSet          bool
	cfgPresent      bool  // celeris.Config has the field
	cfgUS           int64 // its value after applyBusyPollKnob
	resolvedPresent bool  // celeris.Server has the method
	resolvedUS      int64
	resolved        time.Duration
}

// applyBusyPollKnob reads PROBATORIUM_SOCKET_BUSY_POLL and, when it is set, stores it in the
// Config's SocketBusyPoll field (cfg is a *celeris.Config; it is any here so that this file
// needs no import of celeris, which keeps it identical in every mirror). It returns an error on
// a bad value, on the env set while the Config has no such field, and on a field of another type.
func applyBusyPollKnob(cfg any) (busyPollState, error) {
	var st busyPollState
	st.env, st.envSet = os.LookupEnv(busyPollEnv)
	v := reflect.ValueOf(cfg)
	if v.Kind() != reflect.Pointer || v.Elem().Kind() != reflect.Struct {
		return st, fmt.Errorf("applyBusyPollKnob: want a pointer to the Config struct, got %T", cfg)
	}
	f := v.Elem().FieldByName(busyPollField)
	if f.IsValid() {
		if f.Type() != reflect.TypeOf(time.Duration(0)) {
			return st, fmt.Errorf("Config.%s has type %s, want time.Duration: the knob must be reviewed against this celeris", busyPollField, f.Type())
		}
		st.cfgPresent = true
	}
	if st.envSet {
		d, err := parseBusyPoll(st.env)
		if err != nil {
			return st, err
		}
		if !st.cfgPresent {
			return st, fmt.Errorf("%s=%q is set but this celeris has no Config.%s (it predates v1.6.0; its engines busy-poll unconditionally): refusing to run a column whose arm cannot be honoured", busyPollEnv, st.env, busyPollField)
		}
		f.SetInt(int64(d))
	}
	if st.cfgPresent {
		st.cfgUS = f.Int() / int64(time.Microsecond)
	}
	return st, nil
}

// resolveBusyPoll fills in what the built server reports (Server.SocketBusyPoll, available from
// celeris v1.6.0): the value the engines will really put on accepted sockets, which is 0 for the
// std engine whatever the Config says. srv is a *celeris.Server.
func (st *busyPollState) resolveBusyPoll(srv any) {
	m := reflect.ValueOf(srv).MethodByName(busyPollField)
	if !m.IsValid() || m.Type().NumIn() != 0 || m.Type().NumOut() != 1 || m.Type().Out(0) != reflect.TypeOf(time.Duration(0)) {
		return
	}
	d := m.Call(nil)[0].Interface().(time.Duration)
	st.resolvedPresent = true
	st.resolved = d
	st.resolvedUS = int64(d / time.Microsecond)
}

// line is the startup log line (after the log package's own timestamp). The format is a
// contract with arm-validate.py / x86-validate.py (BUSYPOLL regex) and is pinned by
// TestBusyPollLineFormat:
//
//	[busypoll] env=<raw|unset> config_us=<int|absent> resolved_us=<int|absent> resolved=<Duration|absent>
//
// The validators key on the integers; the Duration string is for the human reader (it prints
// "50µs", with a micro sign, which is why nothing parses it).
func (st busyPollState) line() string {
	env := "unset"
	if st.envSet {
		env = st.env
	}
	cfg, res, resStr := busyPollAbsent, busyPollAbsent, busyPollAbsent
	if st.cfgPresent {
		cfg = strconv.FormatInt(st.cfgUS, 10)
	}
	if st.resolvedPresent {
		res = strconv.FormatInt(st.resolvedUS, 10)
		resStr = st.resolved.String()
	}
	return fmt.Sprintf("[busypoll] env=%s config_us=%s resolved_us=%s resolved=%s", env, cfg, res, resStr)
}
