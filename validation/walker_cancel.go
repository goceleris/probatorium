package validation

import (
	"context"
	"errors"
	"os"
)

// readErrorAfterCancel reports whether a walker's read error is left
// uncounted because the walker's own context was already cancelled
// (probatorium#595).
//
// The runner ends a cell on a hard incident by cancelling the run and then
// killing the refapp, and a walker blocked in a read then sees EOF or a reset
// for a connection the harness itself ended. The h2c and websocket walkers
// fire in lockstep, so one such instant used to be counted as several
// h2c_hang / ws_handshake_fail at once. The context is read AFTER the error,
// so an error that comes first is still counted.
//
// A read that ran out its deadline is not excused: it sat the whole budget
// without a byte, which a cancel does not explain.
func readErrorAfterCancel(ctx context.Context, err error) bool {
	return err != nil && ctx.Err() != nil && !errors.Is(err, os.ErrDeadlineExceeded)
}
