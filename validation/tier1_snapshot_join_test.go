package validation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/goceleris/probatorium/validation/remote"
)

// The periodic snapshot writer must be joined before driveTier1 returns,
// and every write must be atomic: whenever the caller reads the file it
// is complete JSON carrying the canonical field. Under a loaded -race run
// the unjoined writer left the file EMPTY for the reader that came next
// (CI, run 34727613829). Fifty short tiers with a 20 ms cadence make the
// window wide; every read must parse.
func TestDriveTier1_SnapshotIsCompleteWhenTheTierReturns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	dir := t.TempDir()
	for i := 0; i < 50; i++ {
		snapPath := dir + "/tier1_tally.json"
		_ = os.Remove(snapPath)
		cfg := tier1Config{
			Driver:                remote.NewLocal("/bin/sh"),
			RefappArgs:            []string{"-c", `echo "ready addr=` + srv.URL + `"; sleep 10`},
			BaseURL:               srv.URL,
			Matrix:                minimalMatrix(t),
			Seed:                  42,
			Concurrency:           1,
			ReadyTimeout:          2 * time.Second,
			RequestTimeout:        time.Second,
			SnapshotPath:          snapPath,
			TallyCallbackInterval: 20 * time.Millisecond,
		}
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		_, err := driveTier1(ctx, cfg)
		cancel()
		if err != nil {
			t.Fatalf("iteration %d: driveTier1: %v", i, err)
		}
		data, err := os.ReadFile(snapPath)
		if err != nil {
			t.Fatalf("iteration %d: snapshot not written: %v", i, err)
		}
		var doc map[string]any
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatalf("iteration %d: snapshot is not complete JSON (%d bytes): %v", i, len(data), err)
		}
		if _, ok := doc["requests_sent"]; !ok {
			t.Fatalf("iteration %d: snapshot missing requests_sent", i)
		}
		if _, err := os.Stat(snapPath + ".tmp"); err == nil {
			t.Fatalf("iteration %d: temp file left behind after the tier returned", i)
		}
	}
}
