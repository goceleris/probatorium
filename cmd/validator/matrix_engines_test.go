package main

import (
	"runtime"
	"slices"
	"testing"
)

// "auto" is the production set the release gate judges. Before celeris#580
// it was iouring+epoll+std, so the default engine (adaptive) never ran a
// single cell of any nightly, soak, checkptr or race tier.
func TestExpandEngines_AutoIsTheFourEngineProductionSetOnLinux(t *testing.T) {
	got := expandEngines("auto")
	want := []string{"std"}
	if runtime.GOOS == "linux" {
		want = []string{"iouring", "epoll", "std", "adaptive"}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("expandEngines(auto) on %s = %v, want %v", runtime.GOOS, got, want)
	}
}

// adaptive composes epoll and io_uring; celeris rejects it off Linux, so an
// explicit list must filter it like its sub-engines instead of scheduling a
// cell that can only die.
func TestExpandEngines_AdaptiveIsLinuxOnly(t *testing.T) {
	got := expandEngines("adaptive,std")
	want := []string{"std"}
	if runtime.GOOS == "linux" {
		want = []string{"adaptive", "std"}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("expandEngines(adaptive,std) on %s = %v, want %v", runtime.GOOS, got, want)
	}
}
