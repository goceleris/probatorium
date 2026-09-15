// splicedemo models the one thing a candidate binary does that the harness's
// output capture has to survive: it writes to stdout and stderr at the same
// time, and it does not finish a line in a single write.
//
// The Go runtime behaves exactly this way when it dies -- "fatal error: " and
// the reason are separate writes to stderr, with whatever the program was
// printing to stdout still in flight -- so the gaps inside a line are widened
// here on purpose rather than left to the scheduler to produce now and then.
// Each stream serialises its own writes, as a real process does
// (the runtime holds printlock, fmt writes a line at a time): the only
// unsynchronised interleaving is between the two streams, which is the one
// the harness owns.
//
// Modes:
//
//	splice <workers> <lines-per-worker> <gap-us>
//	    Both streams emit tagged fixed-shape lines from many goroutines.
//	    Every output line must survive whole; "o" filler is stdout's and
//	    "e" filler is stderr's, so any splice is visible in the line itself.
//
//	edges <L-line-bytes> <X-line-bytes> <noise-lines> <gap-us>
//	    stdout writes two very long lines in small chunks -- one of "L", one
//	    of "X" -- while stderr chatters complete lines into the gaps, then
//	    stdout writes a final line with NO trailing newline and the program
//	    exits, the shape a process killed mid-write leaves behind.
package main

import (
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: splicedemo splice|edges ...")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "splice":
		splice(atoiArg(2, 8), atoiArg(3, 15), atoiArg(4, 500))
	case "edges":
		edges(atoiArg(2, 131072), atoiArg(3, 302144), atoiArg(4, 120), atoiArg(5, 300))
	default:
		fmt.Fprintf(os.Stderr, "unknown mode %q\n", os.Args[1])
		os.Exit(2)
	}
}

func atoiArg(i, def int) int {
	if len(os.Args) <= i {
		return def
	}
	n, err := strconv.Atoi(os.Args[i])
	if err != nil || n < 0 {
		return def
	}
	return n
}

// writeSplit writes payload in pieces, with a gap between each and before the
// closing newline, holding mu so this stream's own lines stay whole. The Go
// runtime dies this way: "fatal error: " and the reason reach stderr as
// separate writes, and the newline is a third.
//
// One split point per line is not enough to model it. Two streams that each
// write a payload and then a newline settle into anti-phase -- each stream's
// newline terminates the other's payload -- and the merged output comes out
// looking clean by luck. Several writes per line take that luck away.
func writeSplit(f *os.File, mu *sync.Mutex, payload string, pieces int, gap func() time.Duration) {
	mu.Lock()
	defer mu.Unlock()
	per := max(1, len(payload)/pieces)
	for i := 0; i < len(payload); i += per {
		_, _ = f.WriteString(payload[i:min(i+per, len(payload))])
		time.Sleep(gap())
	}
	_, _ = f.WriteString("\n")
}

func splice(workers, perWorker, gapUS int) {
	base := time.Duration(gapUS) * time.Microsecond
	var wg sync.WaitGroup
	// Each stream jitters its gaps from its own generator: equal fixed gaps
	// would let the two lock into a rhythm instead of writing into each
	// other's unfinished lines.
	stream := func(f *os.File, tag string, filler string, seed uint64) {
		var mu sync.Mutex
		for w := range workers {
			wg.Add(1)
			go func() {
				rng := rand.New(rand.NewPCG(seed, uint64(w)))
				defer wg.Done()
				gap := func() time.Duration { return base + time.Duration(rng.Int64N(int64(base))) }
				for i := range perWorker {
					writeSplit(f, &mu, fmt.Sprintf("%s-%02d-%04d-%s", tag, w, i, strings.Repeat(filler, 24)), 4, gap)
				}
			}()
		}
	}
	stream(os.Stdout, "OUT", "o", 0x5eed0117)
	stream(os.Stderr, "ERR", "e", 0xc0ffee42)
	wg.Wait()
}

func edges(lBytes, xBytes, noiseLines, gapUS int) {
	gap := time.Duration(gapUS) * time.Microsecond
	const chunk = 4096
	longLine := func(filler string, total int) {
		for written := 0; written < total; {
			n := min(chunk, total-written)
			_, _ = os.Stdout.WriteString(strings.Repeat(filler, n))
			written += n
			time.Sleep(gap)
		}
		_, _ = os.Stdout.WriteString("\n")
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		longLine("L", lBytes)
		longLine("X", xBytes)
	}()
	go func() {
		defer wg.Done()
		for i := range noiseLines {
			_, _ = fmt.Fprintf(os.Stderr, "ERR-noise-%04d\n", i)
			time.Sleep(gap)
		}
	}()
	wg.Wait()
	// No trailing newline: the last thing a process writes before it dies is
	// often the most interesting line in the capture.
	_, _ = os.Stdout.WriteString("tail-without-newline")
}
