package main

import (
	"runtime/debug"
	"testing"
)

func TestCelerisBuildLine(t *testing.T) {
	dep := func(v, sum string, rep *debug.Module) *debug.BuildInfo {
		return &debug.BuildInfo{Deps: []*debug.Module{{Path: "golang.org/x/sys", Version: "v0.1.0", Sum: "h1:x"}, {Path: celerisModulePath, Version: v, Sum: sum, Replace: rep}}}
	}
	for _, c := range []struct {
		name string
		bi   *debug.BuildInfo
		ok   bool
		want string
	}{
		{"pinned", dep("v1.5.12-0.20261116093428-9d5e134caa6c", "h1:V5NUgmTSmZHIw7/ieCiOB9vJQSUcUQ74mDVvMjF9meE=", nil), true, "[celeris-build] version=v1.5.12-0.20261116093428-9d5e134caa6c sum=h1:V5NUgmTSmZHIw7/ieCiOB9vJQSUcUQ74mDVvMjF9meE="},
		{"replaced", dep("v1.5.12-0.20261116093428-9d5e134caa6c", "h1:x", &debug.Module{Path: "../celeris"}), true, "[celeris-build] version=replaced sum=replaced"},
		{"no sum", dep("v1.5.12-0.20261116093428-9d5e134caa6c", "", nil), true, "[celeris-build] version=unknown sum=unknown"},
		{"no version", dep("", "h1:x", nil), true, "[celeris-build] version=unknown sum=unknown"},
		{"absent", &debug.BuildInfo{Deps: []*debug.Module{{Path: "golang.org/x/sys", Version: "v0.1.0", Sum: "h1:x"}}}, true, "[celeris-build] version=absent sum=absent"},
		{"no build info", nil, false, "[celeris-build] version=unknown sum=unknown"},
		{"nil with ok", nil, true, "[celeris-build] version=unknown sum=unknown"},
	} {
		if got := celerisBuildLine(c.bi, c.ok); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
