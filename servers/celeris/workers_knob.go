package main

// The Workers knob of the bench SUT (P3a, the #756 / #757 loop-count comparison; THROWAWAY head
// ab/p3a-rc1-20261110, not for merge).
//
// celeris.Config.Workers is the number of epoll/io_uring loops (0 = the platform default: one loop
// per usable CPU, on a homogeneous host GOMAXPROCS). P3a runs the SAME binary three times, once per
// arm, and only the registry column's SUTEnv differs:
//
//	k0  PROBATORIUM_WORKERS unset     (the genuine rc1 default; NOT "explicit 16")
//	k2  PROBATORIUM_WORKERS=14
//	k4  PROBATORIUM_WORKERS=12
//
// As in busypoll_knob.go the Config field is reached by reflection, for the same reasons: the file
// compiles against every celeris this module (or a mirror of it, servers/celeris_v158 and
// servers/celeris_9f4d89b, byte copies) may pin; an API drift (the field renamed or retyped) cannot
// silently turn a k2/k4 column into a default one, because the env set on a celeris without an
// int Config.Workers is a fatal start error. A set value that is not a positive integer is fatal
// too, never a silent default. k0 sets no env, so it is the true default.
//
// The startup line is printed ALWAYS (also on k0), after the [busypoll] line:
//
//	[workers] env=<raw|unset> config=<int|absent>
//
// config is Config.Workers after the knob ran (0 when the env is unset and the celeris default is
// kept). The resolved loop count is NOT known here: the engine's own
// "epoll engine listening ... loops=N" line is the resolved-value check.

import (
	"fmt"
	"log"
	"os"
	"reflect"
	"regexp"
	"strconv"
)

// workersEnv names the knob. Unset = the celeris default. Set but empty is an error.
const workersEnv = "PROBATORIUM_WORKERS"

const (
	workersField  = "Workers" // celeris.Config field
	workersAbsent = "absent"
	workersMax    = 4096 // sanity bound; the largest host of the bench has 16 CPUs
)

// workersValueRE: a plain positive decimal integer (no sign, no space, no leading zero).
var workersValueRE = regexp.MustCompile(`^[1-9][0-9]{0,3}$`)

// parseWorkers parses the env value.
func parseWorkers(raw string) (int, error) {
	if raw == "" {
		return 0, fmt.Errorf("%s is set but empty (unset it for the default)", workersEnv)
	}
	if !workersValueRE.MatchString(raw) {
		return 0, fmt.Errorf("%s=%q: want a positive decimal integer", workersEnv, raw)
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s=%q: %w", workersEnv, raw, err)
	}
	if n > workersMax {
		return 0, fmt.Errorf("%s=%q: over %d", workersEnv, raw, workersMax)
	}
	return n, nil
}

// workersState is what the startup line reports.
type workersState struct {
	env        string // the raw env value, "" when unset
	envSet     bool
	cfgPresent bool // celeris.Config has an int field Workers
	cfg        int  // its value after applyWorkersKnob
}

// applyWorkersKnob reads PROBATORIUM_WORKERS and, when it is set, stores it in the Config's Workers
// field (cfg is a *celeris.Config, an any here so that this file needs no import of celeris and is
// identical in every mirror). It returns an error on a bad value, on the env set while the Config
// has no int Workers field, and on a Workers field of another type.
func applyWorkersKnob(cfg any) (workersState, error) {
	var st workersState
	st.env, st.envSet = os.LookupEnv(workersEnv)
	v := reflect.ValueOf(cfg)
	if v.Kind() != reflect.Pointer || v.Elem().Kind() != reflect.Struct {
		return st, fmt.Errorf("applyWorkersKnob: want a pointer to the Config struct, got %T", cfg)
	}
	f := v.Elem().FieldByName(workersField)
	if f.IsValid() {
		if f.Kind() != reflect.Int {
			return st, fmt.Errorf("Config.%s has type %s, want int: the knob must be reviewed against this celeris", workersField, f.Type())
		}
		st.cfgPresent = true
	}
	if st.envSet {
		n, err := parseWorkers(st.env)
		if err != nil {
			return st, err
		}
		if !st.cfgPresent {
			return st, fmt.Errorf("%s=%q is set but this celeris has no int Config.%s: refusing to run a column whose arm cannot be honoured", workersEnv, st.env, workersField)
		}
		f.SetInt(int64(n))
	}
	if st.cfgPresent {
		st.cfg = int(f.Int())
	}
	return st, nil
}

// mustApplyWorkersKnob is applyWorkersKnob with the fatal start error of the server: a bad value,
// or the env on a celeris that cannot honour it, never starts the SUT.
func mustApplyWorkersKnob(cfg any) workersState {
	st, err := applyWorkersKnob(cfg)
	if err != nil {
		log.Fatalf("celeris: workers knob: %v", err)
	}
	return st
}

// line is the startup log line (after the log package's own timestamp). The format is a contract
// with the P3a validator and is pinned by TestWorkersLineFormat:
//
//	[workers] env=<raw|unset> config=<int|absent>
func (st workersState) line() string {
	env := "unset"
	if st.envSet {
		env = st.env
	}
	cfg := workersAbsent
	if st.cfgPresent {
		cfg = strconv.Itoa(st.cfg)
	}
	return fmt.Sprintf("[workers] env=%s config=%s", env, cfg)
}
