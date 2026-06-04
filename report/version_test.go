package report

import "testing"

func TestCompareSemver(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v1.4.12", "v1.4.13", -1},
		{"v1.4.13", "v1.4.12", 1},
		{"v1.4.12", "v1.4.12", 0},
		{"1.4.12", "v1.4.12", 0},     // leading v optional
		{"v1.5.0", "v1.4.99", 1},     // minor dominates patch
		{"v2.0.0", "v1.99.99", 1},    // major dominates
		{"v1.4", "v1.4.0", 0},        // missing patch defaults to 0
		{"v1", "v1.0.0", 0},          // missing minor+patch default to 0
		{"v1.4.0", "v1.4.0-rc1", 1},  // release outranks its pre-release
		{"v1.4.0-rc1", "v1.4.0", -1}, // and vice versa
		{"v1.4.0+meta", "v1.4.0", 0}, // build metadata ignored
		{"dev", "v1.4.12", -1},       // non-semver sorts lowest
		{"v1.4.12", "dev", 1},
		{"", "v1.0.0", -1}, // empty sorts lowest
	}
	for _, c := range cases {
		if got := CompareSemver(c.a, c.b); got != c.want {
			t.Errorf("CompareSemver(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestNewestBenchmarkedVersion(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "versions list",
			body: `{"versions":["v1.4.10","v1.4.13","v1.4.12"]}`,
			want: "v1.4.13",
		},
		{
			name: "versions map",
			body: `{"versions":{"v1.4.10":{},"v1.4.13":{},"v1.4.2":{}}}`,
			want: "v1.4.13",
		},
		{
			name: "results map",
			body: `{"results":{"v1.4.11":{},"v1.5.0":{}}}`,
			want: "v1.5.0",
		},
		{
			name: "bare top-level map",
			body: `{"v1.4.12":{"x":1},"v1.4.13":{"y":2}}`,
			want: "v1.4.13",
		},
		{
			name: "empty body cold start",
			body: ``,
			want: "",
		},
		{
			name: "whitespace body cold start",
			body: "  \n ",
			want: "",
		},
	}
	for _, c := range cases {
		got, err := NewestBenchmarkedVersion([]byte(c.body))
		if err != nil {
			t.Errorf("%s: unexpected error: %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: NewestBenchmarkedVersion = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestNewestBenchmarkedVersionInvalid(t *testing.T) {
	if _, err := NewestBenchmarkedVersion([]byte("not json")); err == nil {
		t.Errorf("expected error for non-JSON body")
	}
}
