package broken

import "testing"

// Does not compile on purpose: the tally must report a build failure.
func TestBroken(t *testing.T) { undefinedOnPurpose() }
