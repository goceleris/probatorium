package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestParseArgs_Defaults(t *testing.T) {
	cfg, err := ParseArgs(nil, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MetricsURL == "" {
		t.Error("MetricsURL empty default")
	}
	if cfg.Out == "" {
		t.Error("Out empty default")
	}
	if cfg.Interval != time.Second {
		t.Errorf("Interval: got %s, want 1s", cfg.Interval)
	}
	if cfg.PID != 0 {
		t.Errorf("PID: got %d, want 0 (metrics-only mode)", cfg.PID)
	}
}

func TestParseArgs_FlagOverrides(t *testing.T) {
	args := []string{
		"-pid", "1234",
		"-metrics-url", "http://example.test/m",
		"-out", "/tmp/probtest-obs.sqlite",
		"-interval", "100ms",
	}
	cfg, err := ParseArgs(args, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PID != 1234 {
		t.Errorf("PID=%d", cfg.PID)
	}
	if cfg.MetricsURL != "http://example.test/m" {
		t.Errorf("MetricsURL=%q", cfg.MetricsURL)
	}
	if cfg.Out != "/tmp/probtest-obs.sqlite" {
		t.Errorf("Out=%q", cfg.Out)
	}
	if cfg.Interval != 100*time.Millisecond {
		t.Errorf("Interval=%s", cfg.Interval)
	}
}

// fakeExpvar returns an httptest server emitting an expvar-shape doc.
func fakeExpvar(t *testing.T, doc map[string]any) *httptest.Server {
	t.Helper()
	body, _ := json.Marshal(doc)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchMetrics_FlatCelerisCounters(t *testing.T) {
	srv := fakeExpvar(t, map[string]any{
		"goroutines":                  123.0,
		"celeris.accepted_conn_total": 4567.0,
		"celeris.closed_conn_total":   4500.0,
		"celeris.panic_count":         0.0,
	})
	httpc := &http.Client{Timeout: time.Second}
	v := fetchMetrics(context.Background(), httpc, srv.URL, 0)
	if v.Goroutines != 123 || v.AcceptedConn != 4567 || v.ClosedConn != 4500 {
		t.Errorf("metrics: %+v", v)
	}
	if v.Panics != 0 {
		t.Errorf("Panics: got %d, want 0", v.Panics)
	}
}

func TestFetchMetrics_MemstatsHeapInuse(t *testing.T) {
	srv := fakeExpvar(t, map[string]any{
		"memstats": map[string]any{
			"HeapInuse": 4096.0,
			"NumGC":     5.0,
			"PauseNs":   []any{1.0, 2.0, 3.0, 4.0, 250000.0},
		},
	})
	v := fetchMetrics(context.Background(), &http.Client{Timeout: time.Second}, srv.URL, 0)
	if v.HeapInuse != 4096 {
		t.Errorf("HeapInuse: got %d, want 4096", v.HeapInuse)
	}
	// PauseNs[(5-1)%5] = PauseNs[4] = 250000
	if v.GCPauseP99 != 250000 {
		t.Errorf("GCPauseP99: got %d, want 250000", v.GCPauseP99)
	}
}

func TestFetchMetrics_BadURLReturnsZero(t *testing.T) {
	httpc := &http.Client{Timeout: 100 * time.Millisecond}
	v := fetchMetrics(context.Background(), httpc, "http://127.0.0.1:1/", 0)
	if v.Goroutines != 0 || v.HeapInuse != 0 {
		t.Errorf("expected zero on connection error, got %+v", v)
	}
}

func TestFetchMetrics_NonJSONReturnsZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()
	v := fetchMetrics(context.Background(), &http.Client{Timeout: time.Second}, srv.URL, 0)
	if v.Goroutines != 0 || v.HeapInuse != 0 {
		t.Errorf("expected zero on malformed body, got %+v", v)
	}
}

func TestFetchMetrics_MissingFieldsDefaultZero(t *testing.T) {
	srv := fakeExpvar(t, map[string]any{}) // empty doc, every field absent
	v := fetchMetrics(context.Background(), &http.Client{Timeout: time.Second}, srv.URL, 0)
	if v != absentMetrics() {
		t.Errorf("empty doc must yield zero runtime values and absent (-1) engine counters, got %+v", v)
	}
}

// celeris#585: the bench server publishes engine.EngineMetrics whole under
// "celeris.engine_metrics"; the observer must lift the egress counters out
// of it by Go field name.
func TestFetchMetrics_BenchEngineMetricsBlock(t *testing.T) {
	srv := fakeExpvar(t, map[string]any{
		"pid":        123.0,
		"goroutines": 9.0,
		"celeris.engine_metrics": map[string]any{
			"ZCSendsSubmitted": 16.0, "ZCNotifs": 15.0,
			"InlineBytes": 2158592.0, "RingBytes": 31400960.0, "BytesWritten": 33559552.0,
			"RequestCount": 7.0,
		},
	})
	v := fetchMetrics(context.Background(), &http.Client{Timeout: time.Second}, srv.URL, 123)
	want := [len(engineKeys)]int64{16, 15, 2158592, 31400960, 33559552}
	if v.Engine != want || v.Goroutines != 9 {
		t.Errorf("engine=%v goroutines=%d, want %v / 9", v.Engine, v.Goroutines, want)
	}
}

// The validation refapps publish the same counters as flat debugvars keys.
func TestFetchMetrics_RefappFlatEngineKeys(t *testing.T) {
	srv := fakeExpvar(t, map[string]any{
		"celeris.engine_zc_sends_submitted": 4.0,
		"celeris.engine_zc_notifs":          4.0,
		"celeris.engine_inline_bytes":       10.0,
		"celeris.engine_ring_bytes":         20.0,
		"celeris.engine_bytes_written":      30.0,
	})
	v := fetchMetrics(context.Background(), &http.Client{Timeout: time.Second}, srv.URL, 0)
	if want := ([len(engineKeys)]int64{4, 4, 10, 20, 30}); v.Engine != want {
		t.Errorf("engine=%v want %v", v.Engine, want)
	}
}

// 0 is a reading, -1 is absence: the OFF arm of a SEND_ZC A/B reads exactly
// 0 submits and must not be confused with a celeris that has no counter.
func TestFetchMetrics_ZeroIsAReadingAbsenceIsNot(t *testing.T) {
	srv := fakeExpvar(t, map[string]any{
		"celeris.engine_metrics": map[string]any{"ZCSendsSubmitted": 0.0, "InlineBytes": 5.0},
	})
	v := fetchMetrics(context.Background(), &http.Client{Timeout: time.Second}, srv.URL, 0)
	if want := ([len(engineKeys)]int64{0, -1, 5, -1, -1}); v.Engine != want {
		t.Errorf("engine=%v want %v", v.Engine, want)
	}
}

// A document served by another process is refused whole: a leftover SUT
// holding the sidecar port, or a respawn after the observer pinned its PID.
func TestFetchMetrics_ForeignPIDIsAbsent(t *testing.T) {
	srv := fakeExpvar(t, map[string]any{
		"pid": 111.0, "goroutines": 5.0,
		"celeris.engine_metrics": map[string]any{"ZCSendsSubmitted": 3.0},
	})
	c := &http.Client{Timeout: time.Second}
	if v := fetchMetrics(context.Background(), c, srv.URL, 222); v != absentMetrics() {
		t.Errorf("pid 111 document read for pid 222: %+v", v)
	}
	if v := fetchMetrics(context.Background(), c, srv.URL, 111); v.Engine[0] != 3 || v.Goroutines != 5 {
		t.Errorf("own-pid document refused: %+v", v)
	}
	if v := fetchMetrics(context.Background(), c, srv.URL, 0); v.Engine[0] != 3 {
		t.Errorf("no pid to check (metrics-only mode) must read the document: %+v", v)
	}
}

// The pre-#585 bench server answered the scrape with a 404: that is
// absence, never a reading.
func TestFetchMetrics_Non200IsAbsent(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	if v := fetchMetrics(context.Background(), &http.Client{Timeout: time.Second}, srv.URL, 0); v != absentMetrics() {
		t.Errorf("404 must be absent, got %+v", v)
	}
}

func TestSample_NoMetricsURLLeavesEngineCountersAbsent(t *testing.T) {
	obs := sample(context.Background(), &http.Client{Timeout: time.Second}, Config{}, "h", time.Unix(1, 0))
	if obs.EngineZCSendsSubmitted != -1 || obs.EngineZCNotifs != -1 || obs.EngineInlineBytes != -1 ||
		obs.EngineRingBytes != -1 || obs.EngineBytesWritten != -1 {
		t.Errorf("no metrics URL must leave every engine counter at -1, got %+v", obs)
	}
}

func TestReadInt64_TypeCoercion(t *testing.T) {
	m := map[string]any{
		"float":  42.0,
		"int":    int(7),
		"int64":  int64(99),
		"string": "nope",
		"nil":    nil,
	}
	if readInt64(m, "float") != 42 {
		t.Error("float64 coercion")
	}
	if readInt64(m, "int") != 7 {
		t.Error("int coercion")
	}
	if readInt64(m, "int64") != 99 {
		t.Error("int64 passthrough")
	}
	if readInt64(m, "string") != 0 {
		t.Error("non-numeric must default zero")
	}
	if readInt64(m, "nil") != 0 {
		t.Error("nil must default zero")
	}
	if readInt64(m, "absent") != 0 {
		t.Error("missing key must default zero")
	}
}

func TestReadInt64Slice_BoundsCheck(t *testing.T) {
	s := []any{1.0, 2.0, 3.0}
	if readInt64Slice(s, 1) != 2 {
		t.Error("in-bounds index")
	}
	if readInt64Slice(s, -1) != 0 {
		t.Error("negative index must default zero")
	}
	if readInt64Slice(s, 99) != 0 {
		t.Error("out-of-range index must default zero")
	}
	if readInt64Slice([]any{"bad"}, 0) != 0 {
		t.Error("non-numeric element must default zero")
	}
}

func TestCountFDs_MissingPIDReturnsZero(t *testing.T) {
	// PID 0 is never live; /proc/0 doesn't exist on any kernel.
	if got := countFDs(0); got != 0 {
		t.Errorf("countFDs(0): got %d, want 0", got)
	}
}

func TestReadRSS_MissingPIDReturnsZero(t *testing.T) {
	if got := readRSS(0); got != 0 {
		t.Errorf("readRSS(0): got %d, want 0", got)
	}
}

func TestSample_PIDZeroSkipsProcSampling(t *testing.T) {
	// With PID=0 we expect the proc-derived fields to be zero
	// regardless of platform — pure metrics-only mode.
	srv := fakeExpvar(t, map[string]any{"goroutines": 42.0})
	cfg := Config{PID: 0, MetricsURL: srv.URL}
	obs := sample(context.Background(), &http.Client{Timeout: time.Second}, cfg, "testhost", time.Unix(1700000000, 0))
	if obs.FDCount != 0 || obs.RSSBytes != 0 {
		t.Errorf("PID=0 must zero /proc fields, got fd=%d rss=%d", obs.FDCount, obs.RSSBytes)
	}
	if obs.GoroutineCount != 42 {
		t.Errorf("metrics still flow with PID=0, got %d", obs.GoroutineCount)
	}
	if obs.TS != 1700000000 || obs.Host != "testhost" || obs.PID != 0 {
		t.Errorf("envelope fields: %+v", obs)
	}
}
