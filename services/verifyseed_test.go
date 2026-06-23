package services

import (
	"context"
	"testing"
)

// TestVerifySeedNoBackendsNoop pins the empty-address contract: a Go-only
// bench with no driver backends provisioned must verify cleanly (every
// address empty → every backend skipped), mirroring SeedExternal's no-op
// path. The populated-backend assertions live behind the integration
// harness that has live postgres/redis/memcached.
func TestVerifySeedNoBackendsNoop(t *testing.T) {
	t.Parallel()
	if err := VerifySeed(context.Background(), "", "", ""); err != nil {
		t.Errorf("VerifySeed with no addresses should be a no-op, got %v", err)
	}
}
