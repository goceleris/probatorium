package debugvars

import "testing"

// A plain test build is not a -tags=validation build: the document must
// say so, and the corruption counter must be the stub's zero.
func TestValidationBuildKeysInDocument(t *testing.T) {
	doc := New().Document()
	if b, ok := doc["celeris.validation_build"].(bool); !ok || b {
		t.Errorf("celeris.validation_build must be false in a plain build, got %#v", doc["celeris.validation_build"])
	}
	if n, ok := doc["celeris.iouring_sqe_corruptions"].(int64); !ok || n != 0 {
		t.Errorf("celeris.iouring_sqe_corruptions must be int64 0 in a plain build, got %#v", doc["celeris.iouring_sqe_corruptions"])
	}
}
