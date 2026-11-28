package validation

import "context"

// readErrorAfterCancel reports whether a walker's read error is left uncounted
// because the walker's own context was already cancelled (probatorium#595).
// STUB: nothing is wired yet.
func readErrorAfterCancel(ctx context.Context, err error) bool { return false }
