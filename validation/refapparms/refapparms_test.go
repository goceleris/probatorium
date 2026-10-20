package refapparms

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestParseCPUs(t *testing.T) {
	ok := []struct {
		in   string
		want []int
	}{
		{"2-3", []int{2, 3}},
		{"0,1", []int{0, 1}},
		{" 2-5 ", []int{2, 3, 4, 5}},
		{"0-3,8", []int{0, 1, 2, 3, 8}},
		{"4,0-1,1-2", []int{0, 1, 2, 4}},
		{"7,7,9", []int{7, 9}},
	}
	for _, tc := range ok {
		got, err := ParseCPUs(tc.in)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ParseCPUs(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
	// Negative controls: each of these must be refused, none may reach
	// taskset or a cell.
	bad := map[string]string{
		"":          "empty",
		"  ":        "empty",
		"1":         "at least 2",
		"3-3":       "at least 2",
		"1,1":       "at least 2",
		"3-2":       "descending",
		"a":         "not a CPU number",
		"1,,2":      "empty CPU number",
		"1-":        "empty CPU number",
		"-3":        "empty CPU number",
		"1-2-3":     "not a CPU number",
		"0-99999":   "out of range",
		"1;rm -rf":  "not a CPU number",
		"$(id),1":   "not a CPU number",
		"0x1,2":     "not a CPU number",
		"1, 2":      "not a CPU number",
		"0-1,\n2-3": "not a CPU number",
	}
	for in, want := range bad {
		if got, err := ParseCPUs(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseCPUs(%q) = %v, %v; want an error containing %q", in, got, err, want)
		}
	}
}

func TestValidateWorkloadHint(t *testing.T) {
	for _, v := range []string{"", "high", "low", " high "} {
		if err := ValidateWorkloadHint(v); err != nil {
			t.Errorf("%q refused: %v", v, err)
		}
	}
	for _, v := range []string{"HIGH", "medium", "1", "io_uring"} {
		if err := ValidateWorkloadHint(v); err == nil {
			t.Errorf("%q accepted", v)
		}
	}
}

// debugvars is a module of its own and cannot import this package, so it
// spells the variable and the values itself. This keeps the two equal.
func TestDebugvarsSpellsTheHintTheSameWay(t *testing.T) {
	b, err := os.ReadFile("../refapp/internal/debugvars/workload_hint.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"` + WorkloadHintEnv + `"`, `"high"`, `"low"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("debugvars/workload_hint.go does not contain %s", want)
		}
	}
}
