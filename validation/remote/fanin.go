package remote

import (
	"bufio"
	"errors"
	"io"
	"sync"
)

// fanInMaxLine bounds how much of a single line the fan-in buffers before it
// emits what it holds. Three numbers pin it down:
//
//   - it has to clear any line a candidate really produces. The longest lines
//     the harness sees are single stack frames and race-report rules, all far
//     under 4 KiB, so a real crash line is never broken apart.
//   - it has to stay under the 1 MiB token budget the consumers hand their
//     scanners (validation.superviseStderr and waitForReady both call
//     sc.Buffer(..., 1024*1024)). A line over that budget stops the scan with
//     ErrTooLong and loses everything after it -- a bigger loss than the one
//     this file exists to prevent.
//   - it is the fan-in's memory cost: one buffer per source, two sources per
//     running candidate, and the harness runs a couple of candidates at once.
const fanInMaxLine = 256 * 1024

var newline = []byte{'\n'}

// fanInLines copies src into dst one whole line at a time, holding mu across
// each write so no other source's bytes can land inside a line this one
// produced.
//
// A byte-oriented copy cannot promise that. io.Copy forwards whatever a read
// returned, so a line cut at a read boundary gets the other source's next
// bytes stapled to it: that is how the runtime's "fatal error: " met stdout's
// "before" and became the crash signature "fatal error: before", which names
// nothing and matched no detector (probatorium#382). The splice is worse than
// a dropped line because the result reads like real output.
//
// Interleaving BETWEEN lines is expected and unchanged -- the two sources are
// merged precisely so a panic on stdout lands in the same capture as one on
// stderr. What is guaranteed is that each line comes out whole.
//
// Both properties the byte-oriented version was chosen for survive:
//
//   - No serialisation. mu is taken only for the write of a finished line,
//     never across a read, so a silent or slow source never blocks the other.
//     (io.MultiReader is the wrong shape here for the same reason it always
//     was: it does not read the second source until the first reaches EOF,
//     which is forever for a long-running candidate.)
//   - No loss. Every byte read is written. A line longer than fanInMaxLine is
//     SPLIT across consecutive output lines, never truncated -- dropping the
//     tail of a crash line is the same loss in a different disguise. And the
//     last line of a process that died mid-write carries no newline; it is
//     emitted at EOF with the terminator supplied, because that line is
//     usually the interesting one.
//
// Splitting an over-long line does give up something: another source's line
// can appear between the pieces. That is the bounded-memory price of never
// holding an unterminated line open forever, and it keeps the bytes, which
// truncation would not.
func fanInLines(dst io.Writer, mu *sync.Mutex, src io.Reader) {
	br := bufio.NewReaderSize(src, fanInMaxLine)
	for {
		line, err := br.ReadSlice('\n')
		if len(line) > 0 {
			if werr := writeWholeLine(dst, mu, line); werr != nil {
				// The consumer is gone; so is the reason to keep reading.
				// io.Copy stopped on a write error too.
				return
			}
		}
		if err == nil {
			continue
		}
		// ErrBufferFull means the line is longer than the buffer: what came
		// back is its first fanInMaxLine bytes, already emitted above, and
		// the rest of it arrives on the next read.
		if !errors.Is(err, bufio.ErrBufferFull) {
			return
		}
	}
}

// writeWholeLine writes one line and, when the source did not supply one, its
// terminator. Both writes happen under mu so the pair cannot be split by
// another source. line aliases the reader's buffer, which is safe because the
// write completes before the next read.
func writeWholeLine(dst io.Writer, mu *sync.Mutex, line []byte) error {
	mu.Lock()
	defer mu.Unlock()
	if _, err := dst.Write(line); err != nil {
		return err
	}
	if line[len(line)-1] == '\n' {
		return nil
	}
	_, err := dst.Write(newline)
	return err
}
