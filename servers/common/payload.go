package common

import (
	"encoding/json"
	"fmt"
)

// Pre-computed JSON payloads for response-size benchmarks. Built once at
// package init so every adapter (and every test) observes byte-identical
// responses on /json-1k and /json-64k. The exact byte length is what
// loadgen records as ThroughputBPS, so any drift here would corrupt the
// throughput axis of the report.
var (
	json1KPayload  []byte
	json64KPayload []byte
)

func init() {
	json1KPayload = generateJSONPayload(1024)
	json64KPayload = generateJSONPayload(65536)
}

// paginatedItem is one row in the paginated response. Field shape and
// order match a realistic CRUD API so frameworks that special-case
// lower-cased keys, RFC 3339 timestamps, etc. do not get a free win.
type paginatedItem struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
}

// paginatedResponse wraps a page of items with pagination metadata.
type paginatedResponse struct {
	Page       int              `json:"page"`
	PerPage    int              `json:"per_page"`
	Total      int              `json:"total"`
	TotalPages int              `json:"total_pages"`
	Data       []*paginatedItem `json:"data"`
}

// generateJSONPayload builds a paginated response of at least targetSize
// bytes by appending items until the marshalled length crosses the
// threshold. The result is deterministic given a fixed targetSize.
func generateJSONPayload(targetSize int) []byte {
	resp := &paginatedResponse{
		Page:       1,
		PerPage:    50,
		Total:      1000,
		TotalPages: 20,
	}
	for i := 1; ; i++ {
		resp.Data = append(resp.Data, &paginatedItem{
			ID:        i,
			Name:      fmt.Sprintf("User %d", i),
			Email:     fmt.Sprintf("user%d@example.com", i),
			Status:    "active",
			CreatedAt: "2024-01-15T09:30:00Z",
		})
		data, _ := json.Marshal(resp)
		if len(data) >= targetSize {
			return data
		}
	}
}

// JSON1KPayload returns the pre-computed ~1 KiB JSON payload. The slice
// is shared — callers must not mutate it.
func JSON1KPayload() []byte { return json1KPayload }

// JSON64KPayload returns the pre-computed ~64 KiB JSON payload. The
// slice is shared — callers must not mutate it.
func JSON64KPayload() []byte { return json64KPayload }
