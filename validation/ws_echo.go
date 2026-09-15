package validation

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// WebSocket large-echo slice (celeris#587).
//
// The WS torture walker sends frames of at most 125 bytes and expects a
// Close in reply, and the SSE walker reads "tick" events, so no walker in
// Tier 1 ever had more than a few hundred bytes queued on a detached
// connection. celeris's io_uring engine only takes the SEND_ZC path for a
// send of sendZCMinBytes (4096) or more, and only from the worker thread
// once the dispatch goroutine's inline unix.Write has short-written; the
// v1.5.7 send-state audit reasoned about the interaction of those two
// writers and every tier that runs the race detector never entered the
// branch. The 48/48 I-RACE verdict said nothing about it.
//
// This slice puts large frames on the wire and reads the echoes back
// byte-for-byte. Its oracle is the wire: an interleave between a raw
// unix.Write and a ring SEND corrupts or reorders the echo regardless of
// what the engine's own counters claim, and a lost echo is a lost send.
//
// Load shape (binding, from the adversarial review of celeris#587): NOT a
// burst that is then drained -- that serialises the two phases the audit
// is about, because every guarded write on the connection happens while
// the client is not reading and every ring completion arrives after the
// handler has gone idle. Instead each fire holds ONE connection for
// wsEchoStreamDuration with two goroutines: a sender streaming 64 KiB
// masked binary frames back-to-back (bounded by wsEchoMaxInFlight, 4 MiB,
// far under the engine's 64 MiB detached send cap), and a reader that is
// deliberately throttled (a small fixed SO_RCVBUF set before connect, which
// disables receive autotuning, plus a pause after every frame). The server
// socket then oscillates between full and not-full, so its ring sends are
// poll-armed and complete WHILE the handler is still echoing newer frames on
// the SAME connection -- the worker-thread-vs-dispatch-goroutine overlap the
// audit's guard exists for. Once the sender stops, the reader drains the
// remainder unpaced and the fire closes cleanly.
//
// Scoring is per echoed frame: FIN + binary + exactly wsEchoFrameBytes with
// the sequence-numbered pattern intact and in order is ws_echo_ok. The four
// failure classes the gate reads are:
//
//   - ws_echo_corrupt  — a frame whose header or payload is not the byte
//     image of what was sent. Split (sums to it) into
//     ws_echo_egress_interleave, where the bytes at the first mismatch are
//     recognisably ANOTHER expected echo (a server frame header, another
//     sequence's prefix, or the pattern continuing from a different
//     offset) -- the shape a raw write landing inside a ring send leaves
//     -- and ws_echo_other_corrupt for everything else (a flipped byte, a
//     truncated frame). The split is attribution, not tolerance.
//   - ws_echo_reorder  — an intact frame that carries a sequence other than
//     the next one expected. Counted once per out-of-order arrival.
//   - ws_echo_missing  — a fire whose connection ended (EOF, reset, server
//     Close, framing error) with echoes still outstanding.
//   - ws_echo_timeout  — a fire that reached wsEchoMaxHold with echoes still
//     outstanding: the server stopped delivering.
//
// ws_echo_missing and ws_echo_timeout are per FIRE (a connection that broke
// once is one event, however many frames were in flight); ws_echo_ok,
// ws_echo_corrupt and ws_echo_reorder are per FRAME.
//
// Attribution the oracle cannot make on its own: an echo of bytes that
// were already corrupted on the way IN (the celeris#484 class, a double-
// armed recv clobbering the read buffer) scores exactly like an egress
// interleave. The slice therefore runs on every engine that routes /ws, not
// only io_uring: an ingress-side defect shows on every engine that has it,
// an egress-guard interleave is io_uring-only, and a corruption that
// disappears with CELERIS_IOURING_SEND_ZC=off is the ZC class. The counters
// that would witness the ZC window directly (send_zc submits/notifs, the
// inline guard being blocked by a pending NOTIF) do not exist in the pinned
// celeris; this slice does not invent them.
//
// One walker, off-budget, exactly like the RFC conformance slice: one
// connection at a time against the hundreds a cell already drives changes
// no counter the gate reads, and taking a Markov slot would have changed
// the load profile of every cell to buy nothing.

// wsEchoFrameBytes is the payload size of every frame the slice sends,
// matching the bench tier's ws-large-echo scenario (scenarios/streaming.go)
// and far above celeris's sendZCMinBytes (4096). It needs the 64-bit
// (127) length form on the wire, which is the form the torture walker's
// wsFrame refuses.
const wsEchoFrameBytes = 64 * 1024

// wsEchoSeqBytes is the big-endian sequence number that leads each payload.
const wsEchoSeqBytes = 8

// wsEchoMaxInFlight bounds sent-minus-received frames per connection: 64 x
// 64 KiB = 4 MiB, several MiB so the server's autotuned send buffer cannot
// absorb it all, and well under celeris's 64 MiB detached send cap so the
// engine never closes the conn for backlog.
const wsEchoMaxInFlight = 64

// wsEchoRcvBuf is the SO_RCVBUF set on the client socket BEFORE connect. A
// fixed value disables the kernel's receive autotuning, so the receive
// window closes early and the server's sends back up onto its ring.
const wsEchoRcvBuf = 64 * 1024

// wsEchoInterval paces fires. Each fire holds its connection for about
// wsEchoStreamDuration plus the drain, so the walker is effectively one
// connection back to back; the interval only bounds the gap between them.
const wsEchoInterval = 250 * time.Millisecond

// wsEchoReadPace is the pause after every frame the reader takes WHILE the
// sender is still streaming (2-5 ms per the review). Not applied once the
// sender has stopped: the drain is as fast as the server is.
const wsEchoReadPace = 3 * time.Millisecond

// wsEchoStreamDuration is how long the sender streams per fire. A var so
// the unit tests can run a whole fire in a fraction of a second.
var wsEchoStreamDuration = 1 * time.Second

// wsEchoMaxHold bounds one fire end to end: dial, handshake, stream,
// drain, close. See wsMaxHold and TestStreamHoldsStayUnderTheConnCloseBound:
// a detached stream is not stamped in the refapp's last-byte table, so this
// must stay well under I-CONN-1's deadline. A var so the timeout class can
// be unit-tested in well under a second.
var wsEchoMaxHold = 2 * time.Second

// wsEchoCloseWait bounds the close handshake after a drained fire.
const wsEchoCloseWait = 500 * time.Millisecond

// wsEchoTally aggregates the slice's outcomes. Every field is exported
// through wsEchoSnapshot (see TestTier1SummaryExportsEveryTallyField).
type wsEchoTally struct {
	routeProbed  atomic.Bool
	routePresent atomic.Bool
	// fires counts fire attempts, upgraded the ones that got their 101.
	fires         atomic.Int64
	upgraded      atomic.Int64
	handshakeFail atomic.Int64
	// endpointAbsent counts a 404 on the upgrade GET; the slice is not
	// scheduled on a refapp whose route the pre-flight probe found absent,
	// so a nonzero value here means the route vanished mid-run.
	endpointAbsent atomic.Int64
	// Per-frame outcomes.
	sent             atomic.Int64
	ok               atomic.Int64
	corrupt          atomic.Int64
	egressInterleave atomic.Int64
	otherCorrupt     atomic.Int64
	reorder          atomic.Int64
	// Per-fire outcomes.
	missing atomic.Int64
	timeout atomic.Int64
	// frameErr counts fires whose server stream stopped parsing as RFC 6455
	// frames (a length beyond the cap, a header mid-payload). Diagnostic
	// detail for a missing fire, not a separate signal.
	frameErr atomic.Int64
	// closeOK counts drained fires whose Close was answered with a Close
	// or a FIN.
	closeOK atomic.Int64
	// cutAtDeadline counts fires the run context ended mid-stream. Not a
	// defect: the budget ran out.
	cutAtDeadline atomic.Int64
}

// wsEchoSnapshot is the value-typed projection emitted into the tally
// JSON. Prefix `ws_echo_` keeps the keys unambiguous next to ws_torture's.
type wsEchoSnapshot struct {
	RouteProbed      bool  `json:"ws_echo_route_probed"`
	RoutePresent     bool  `json:"ws_echo_route_present"`
	Fires            int64 `json:"ws_echo_fires"`
	Upgraded         int64 `json:"ws_echo_upgraded"`
	HandshakeFail    int64 `json:"ws_echo_handshake_fail"`
	EndpointAbsent   int64 `json:"ws_echo_endpoint_absent"`
	Sent             int64 `json:"ws_echo_sent"`
	OK               int64 `json:"ws_echo_ok"`
	Corrupt          int64 `json:"ws_echo_corrupt"`
	EgressInterleave int64 `json:"ws_echo_egress_interleave"`
	OtherCorrupt     int64 `json:"ws_echo_other_corrupt"`
	Reorder          int64 `json:"ws_echo_reorder"`
	Missing          int64 `json:"ws_echo_missing"`
	Timeout          int64 `json:"ws_echo_timeout"`
	FrameErr         int64 `json:"ws_echo_frame_err"`
	CloseOK          int64 `json:"ws_echo_close_ok"`
	CutAtDeadline    int64 `json:"ws_echo_cut_at_deadline"`
}

// recordRoute stores the pre-flight verdict for this cell (the same probe
// the torture slice uses; both walk the same path).
func (t *wsEchoTally) recordRoute(p routeProbe) {
	t.routeProbed.Store(p.Probed)
	t.routePresent.Store(p.Present)
}

func (t *wsEchoTally) snapshot() wsEchoSnapshot {
	return wsEchoSnapshot{
		RouteProbed:      t.routeProbed.Load(),
		RoutePresent:     t.routePresent.Load(),
		Fires:            t.fires.Load(),
		Upgraded:         t.upgraded.Load(),
		HandshakeFail:    t.handshakeFail.Load(),
		EndpointAbsent:   t.endpointAbsent.Load(),
		Sent:             t.sent.Load(),
		OK:               t.ok.Load(),
		Corrupt:          t.corrupt.Load(),
		EgressInterleave: t.egressInterleave.Load(),
		OtherCorrupt:     t.otherCorrupt.Load(),
		Reorder:          t.reorder.Load(),
		Missing:          t.missing.Load(),
		Timeout:          t.timeout.Load(),
		FrameErr:         t.frameErr.Load(),
		CloseOK:          t.closeOK.Load(),
		CutAtDeadline:    t.cutAtDeadline.Load(),
	}
}

// summariseWSEcho formats a snapshot for the run summary log line.
func summariseWSEcho(s wsEchoSnapshot) string {
	return fmt.Sprintf("ws_echo_fires=%d ws_echo_sent=%d ws_echo_ok=%d ws_echo_corrupt=%d ws_echo_reorder=%d ws_echo_missing=%d ws_echo_timeout=%d",
		s.Fires, s.Sent, s.OK, s.Corrupt, s.Reorder, s.Missing, s.Timeout)
}

// runWSLargeEchoWalker fires fireWSLargeEcho at hostPort + path per
// tickInterval until ctx is done. seed is the per-walker PCG seed; the
// frame masks are drawn from it, so the same seed puts the same bytes on
// the wire.
func runWSLargeEchoWalker(ctx context.Context, hostPort, path string,
	seed uint64, tickInterval time.Duration, tally *wsEchoTally,
) {
	rng := rand.New(rand.NewPCG(seed, ^seed^0xec40_1a26_5eed_0001))
	if tickInterval <= 0 {
		tickInterval = wsEchoInterval
	}
	if path == "" {
		path = wsTorturePath
	}
	tick := time.NewTicker(tickInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			fireWSLargeEcho(ctx, hostPort, path, rng, tally)
		}
	}
}

// wsEchoSetRcvBuf is the net.Dialer Control that pins SO_RCVBUF on the
// client socket before connect (see wsEchoRcvBuf).
func wsEchoSetRcvBuf(_, _ string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, wsEchoRcvBuf)
	}); err != nil {
		return err
	}
	return serr
}

// fireWSLargeEcho runs one fire: dial, upgrade, stream + verify, close.
// rng is the walker's; the fire is synchronous so the sender goroutine is
// its only user while it runs.
func fireWSLargeEcho(ctx context.Context, hostPort, path string, rng *rand.Rand, tally *wsEchoTally) {
	tally.fires.Add(1)
	d := net.Dialer{Timeout: wsEchoMaxHold, Control: wsEchoSetRcvBuf}
	conn, err := d.DialContext(ctx, "tcp", hostPort)
	if err != nil {
		// Dial failure is infra -- don't fold into outcomes.
		return
	}
	defer func() { _ = conn.Close() }()
	deadline := time.Now().Add(wsEchoMaxHold)
	_ = conn.SetDeadline(deadline)

	if _, err := conn.Write(wsUpgradeRequest(hostPort, path, wsProbeKey)); err != nil {
		tally.handshakeFail.Add(1)
		return
	}
	br := bufio.NewReaderSize(conn, wsEchoFrameBytes)
	status, err := wsReadUpgradeResponse(br)
	if err != nil {
		tally.handshakeFail.Add(1)
		return
	}
	if !strings.HasPrefix(status, "HTTP/1.1 101") {
		if strings.HasPrefix(status, "HTTP/1.1 404") {
			tally.endpointAbsent.Add(1)
			return
		}
		tally.handshakeFail.Add(1)
		return
	}
	tally.upgraded.Add(1)

	// sent is the highest sequence handed to the socket, received the number
	// of data frames read back (whatever they scored). Both atomic: the
	// sender publishes sent BEFORE the bytes leave, so an echo can never
	// arrive for a sequence the verifier does not yet know about.
	var sent, received atomic.Uint64
	outstanding := func() uint64 { return sent.Load() - received.Load() }
	stop := make(chan struct{})
	sendDone := make(chan struct{})
	// senderFinished is set by the sender BEFORE it pokes the reader and
	// closes sendDone, so a reader woken by the poke can tell "drained"
	// from "timed out"; sendDone closing after the poke is what lets the
	// main path set a fresh deadline for the close handshake without the
	// poke landing on top of it.
	var senderFinished atomic.Bool
	senderDone := senderFinished.Load
	go func() {
		defer close(sendDone)
		defer func() {
			// When the sender finishes with nothing outstanding the reader
			// may be parked in Read with nothing to wait for; the poke
			// turns that into an immediate deadline error the reader
			// recognises as "drained".
			senderFinished.Store(true)
			if outstanding() == 0 {
				_ = conn.SetReadDeadline(time.Now())
			}
		}()
		streamEnd := time.Now().Add(wsEchoStreamDuration)
		payload := make([]byte, wsEchoFrameBytes)
		var frame []byte
		var seq uint64
		for time.Now().Before(streamEnd) && ctx.Err() == nil {
			select {
			case <-stop:
				return
			default:
			}
			if outstanding() >= wsEchoMaxInFlight {
				time.Sleep(time.Millisecond)
				continue
			}
			seq++
			wsEchoFillPayload(payload, seq)
			var mask [4]byte
			binary.LittleEndian.PutUint32(mask[:], rng.Uint32())
			frame = wsAppendFrame(frame[:0], true, 0x2, payload, &mask)
			sent.Store(seq)
			tally.sent.Add(1)
			if _, err := conn.Write(frame); err != nil {
				return
			}
		}
	}()
	v := newWSEchoVerifier()
	drained := false
	connEnded := false // the peer closed, reset, or stopped speaking frames
readLoop:
	for {
		fin, opcode, payload, err := wsReadFrame(br, wsEchoFrameBytes)
		if err != nil {
			switch {
			case errors.Is(err, os.ErrDeadlineExceeded) && senderDone() && outstanding() == 0:
				drained = true
			case ctx.Err() != nil:
				tally.cutAtDeadline.Add(1)
				connEnded = true
			case errors.Is(err, os.ErrDeadlineExceeded):
				tally.timeout.Add(1)
				connEnded = true
			case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF),
				errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
				if outstanding() > 0 || !senderDone() {
					tally.missing.Add(1)
				}
				connEnded = true
			default:
				tally.frameErr.Add(1)
				if outstanding() > 0 || !senderDone() {
					tally.missing.Add(1)
				}
				connEnded = true
			}
			break readLoop
		}
		switch opcode {
		case 0x8: // server Close mid-stream
			if outstanding() > 0 || !senderDone() {
				tally.missing.Add(1)
			}
			connEnded = true
			break readLoop
		case 0x9: // Ping: answer, keep going.
			var mask [4]byte
			binary.LittleEndian.PutUint32(mask[:], rng.Uint32())
			_, _ = conn.Write(wsAppendFrame(nil, true, 0xA, payload, &mask))
			continue
		case 0xA: // Pong: nothing asked for it, nothing to score.
			continue
		}
		received.Add(1)
		switch v.score(fin, opcode, payload, sent.Load()) {
		case wsEchoOutcomeOK:
			tally.ok.Add(1)
		case wsEchoOutcomeReorder:
			tally.reorder.Add(1)
		case wsEchoOutcomeCorruptInterleave:
			tally.corrupt.Add(1)
			tally.egressInterleave.Add(1)
		case wsEchoOutcomeCorruptOther:
			tally.corrupt.Add(1)
			tally.otherCorrupt.Add(1)
		}
		if senderDone() && outstanding() == 0 {
			drained = true
			break readLoop
		}
		if !senderDone() {
			time.Sleep(wsEchoReadPace)
		}
	}
	// Stop the sender (it may be parked in Write behind a closed window)
	// and join it before touching the socket again.
	close(stop)
	_ = conn.SetWriteDeadline(time.Now())
	<-sendDone
	if connEnded {
		return
	}
	_ = conn.SetDeadline(time.Now().Add(wsEchoCloseWait))
	var mask [4]byte
	binary.LittleEndian.PutUint32(mask[:], rng.Uint32())
	if _, err := conn.Write(wsAppendFrame(nil, true, 0x8, []byte{0x03, 0xE8}, &mask)); err != nil {
		return
	}
	for drained {
		_, opcode, _, err := wsReadFrame(br, wsEchoFrameBytes)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) {
				tally.closeOK.Add(1)
			}
			return
		}
		if opcode == 0x8 {
			tally.closeOK.Add(1)
			return
		}
	}
}

// wsReadUpgradeResponse reads the HTTP response to an upgrade request:
// the status line, then (for a 101) the headers up to the blank line, so
// the reader is positioned on the first frame byte. A non-101 status is
// returned as-is with the headers unread.
func wsReadUpgradeResponse(br *bufio.Reader) (string, error) {
	status, err := br.ReadString('\n')
	if err != nil {
		return "", err
	}
	status = strings.TrimSpace(status)
	if !strings.HasPrefix(status, "HTTP/1.1 101") {
		return status, nil
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return status, err
		}
		if line == "\r\n" || line == "\n" {
			return status, nil
		}
	}
}

// wsAppendFrame appends one RFC 6455 frame to dst with the full length
// encoding: <= 125 inline, 126 + 16-bit, 127 + 64-bit. A nil mask builds a
// server-style unmasked frame; a non-nil mask sets the MASK bit and XORs
// the payload with it (RFC 6455 section 5.3). The torture walker's wsFrame
// stops at 125 bytes; this is the encoder the 64 KiB frames need.
func wsAppendFrame(dst []byte, fin bool, opcode byte, payload []byte, mask *[4]byte) []byte {
	n := len(payload)
	b0 := opcode & 0x0f
	if fin {
		b0 |= 0x80
	}
	var lenByte byte
	switch {
	case n <= 125:
		lenByte = byte(n)
	case n <= 0xffff:
		lenByte = 126
	default:
		lenByte = 127
	}
	if mask != nil {
		lenByte |= 0x80
	}
	dst = append(dst, b0, lenByte)
	switch {
	case n <= 125:
	case n <= 0xffff:
		dst = binary.BigEndian.AppendUint16(dst, uint16(n))
	default:
		dst = binary.BigEndian.AppendUint64(dst, uint64(n))
	}
	if mask == nil {
		return append(dst, payload...)
	}
	dst = append(dst, mask[:]...)
	for i, b := range payload {
		dst = append(dst, b^mask[i&3])
	}
	return dst
}

// wsReadFrame reads one frame from br and returns its FIN bit, opcode and
// unmasked payload. All three length forms are decoded; a MASK bit is
// honoured (a server must not set it, but the test-side echo server reads
// the client's masked frames through this same function). maxPayload caps
// the allocation: a declared length beyond it is a framing error, not a
// 64-bit malloc.
func wsReadFrame(br *bufio.Reader, maxPayload int) (fin bool, opcode byte, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(br, h[:]); err != nil {
		return false, 0, nil, err
	}
	fin = h[0]&0x80 != 0
	opcode = h[0] & 0x0f
	masked := h[1]&0x80 != 0
	n := uint64(h[1] & 0x7f)
	switch n {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(br, ext[:]); err != nil {
			return fin, opcode, nil, err
		}
		n = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(br, ext[:]); err != nil {
			return fin, opcode, nil, err
		}
		n = binary.BigEndian.Uint64(ext[:])
	}
	if n > uint64(maxPayload) {
		return fin, opcode, nil, fmt.Errorf("ws frame: declared payload %d exceeds cap %d", n, maxPayload)
	}
	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(br, mask[:]); err != nil {
			return fin, opcode, nil, err
		}
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(br, payload); err != nil {
		return fin, opcode, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i&3]
		}
	}
	return fin, opcode, payload, nil
}

// wsEchoFillPayload writes the payload for seq into dst (len
// wsEchoFrameBytes): the 8-byte big-endian sequence, then byte(seq)+byte(j)
// for j from 0 -- the pattern celeris's own inline-egress correctness test
// uses, so a corrupted echo is decodable by eye in a dossier.
func wsEchoFillPayload(dst []byte, seq uint64) {
	binary.BigEndian.PutUint64(dst[:wsEchoSeqBytes], seq)
	b := byte(seq)
	for j := range dst[wsEchoSeqBytes:] {
		dst[wsEchoSeqBytes+j] = b + byte(j)
	}
}

// wsEchoPayload allocates and fills the payload for seq (tests).
func wsEchoPayload(seq uint64) []byte {
	p := make([]byte, wsEchoFrameBytes)
	wsEchoFillPayload(p, seq)
	return p
}

// wsEchoFirstMismatch returns the offset of the first byte of payload that
// differs from the expected payload for seq, or -1 when it is the byte
// image of what was sent. The first wsEchoSeqBytes are the caller's (they
// decoded seq from them).
func wsEchoFirstMismatch(payload []byte, seq uint64) int {
	b := byte(seq)
	for j, got := range payload[wsEchoSeqBytes:] {
		if got != b+byte(j) {
			return wsEchoSeqBytes + j
		}
	}
	return -1
}

// wsEchoOutcome is the verdict on one echoed frame.
type wsEchoOutcome int

const (
	wsEchoOutcomeOK wsEchoOutcome = iota
	wsEchoOutcomeReorder
	wsEchoOutcomeCorruptInterleave
	wsEchoOutcomeCorruptOther
)

func (o wsEchoOutcome) String() string {
	switch o {
	case wsEchoOutcomeOK:
		return "ok"
	case wsEchoOutcomeReorder:
		return "reorder"
	case wsEchoOutcomeCorruptInterleave:
		return "corrupt-egress-interleave"
	case wsEchoOutcomeCorruptOther:
		return "corrupt-other"
	}
	return "unknown"
}

// wsEchoVerifier scores echoes against the sequence the sender put on the
// wire. Sequences are 1..sent; next is the one expected in order; early
// holds intact frames that arrived ahead of it.
type wsEchoVerifier struct {
	next  uint64
	early map[uint64]bool
}

func newWSEchoVerifier() *wsEchoVerifier {
	return &wsEchoVerifier{next: 1, early: map[uint64]bool{}}
}

// score classifies one data frame. sent is the highest sequence the sender
// has published so far.
//
// A damaged frame still CONSUMES its place in the order -- the sequence it
// carries when that is intact, the next expected one when the frame is too
// damaged to say -- so one corrupted echo is one corrupt event and the
// intact frames after it are still judged in order. Without that, every
// frame following a single corruption would score as a reorder, and one
// event would be reported once per frame for the rest of the fire.
func (v *wsEchoVerifier) score(fin bool, opcode byte, payload []byte, sent uint64) wsEchoOutcome {
	if !fin || opcode != 0x2 || len(payload) != wsEchoFrameBytes {
		// Framing-level damage: a partial frame, a wrong type, a wrong
		// length. Not the interleave shape (that keeps the declared length
		// and puts foreign bytes inside it), so no attribution scan.
		v.consume(v.next)
		return wsEchoOutcomeCorruptOther
	}
	seq := binary.BigEndian.Uint64(payload[:wsEchoSeqBytes])
	if seq < v.next || v.early[seq] {
		// Already echoed once: a duplicate takes no place in the order.
		return v.classifyCorrupt(payload, 0, sent)
	}
	if seq == 0 || seq > sent {
		// Never sent: the leading bytes are not ours, so start the
		// attribution scan at offset 0.
		v.consume(v.next)
		return v.classifyCorrupt(payload, 0, sent)
	}
	if m := wsEchoFirstMismatch(payload, seq); m >= 0 {
		v.consume(seq)
		return v.classifyCorrupt(payload, m, sent)
	}
	if seq != v.next {
		v.early[seq] = true
		return wsEchoOutcomeReorder
	}
	v.consume(seq)
	return wsEchoOutcomeOK
}

// consume marks seq as having taken its place in the order and advances
// next past every early arrival that is now in sequence.
func (v *wsEchoVerifier) consume(seq uint64) {
	if seq != v.next {
		v.early[seq] = true
		return
	}
	v.next++
	for v.early[v.next] {
		delete(v.early, v.next)
		v.next++
	}
}

// classifyCorrupt attributes a mismatch at offset at: the bytes there are
// either recognisably another expected echo (interleave) or not (other).
func (v *wsEchoVerifier) classifyCorrupt(payload []byte, at int, sent uint64) wsEchoOutcome {
	if wsEchoLooksLikeAnotherEcho(payload[at:], sent) {
		return wsEchoOutcomeCorruptInterleave
	}
	return wsEchoOutcomeCorruptOther
}

// wsEchoLooksLikeAnotherEcho reports whether b starts with bytes that
// belong to a DIFFERENT position of the expected echo stream:
//
//   - a server frame header for a full-size binary frame followed by a
//     sequence that was sent (a whole frame landed inside another);
//   - the start of another sent sequence's payload (its 8-byte sequence
//     and the first pattern bytes);
//   - the pattern continuing consecutively from some other offset -- the
//     pattern is a 256-cycle, so any run of consecutive bytes is a slice of
//     some expected echo, and a single flipped byte breaks the run.
//
// A byte flip, a zeroed page or a truncation matches none of these.
func wsEchoLooksLikeAnotherEcho(b []byte, sent uint64) bool {
	const probe = 16
	if len(b) < probe+wsEchoSeqBytes+2 {
		return false
	}
	if b[0] == 0x82 && b[1] == 0x7f && binary.BigEndian.Uint64(b[2:10]) == wsEchoFrameBytes {
		if s := binary.BigEndian.Uint64(b[10:18]); s >= 1 && s <= sent {
			return true
		}
	}
	if s := binary.BigEndian.Uint64(b[:wsEchoSeqBytes]); s >= 1 && s <= sent {
		ok := true
		for j := 0; j < probe; j++ {
			if b[wsEchoSeqBytes+j] != byte(s)+byte(j) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	for j := 1; j < probe; j++ {
		if b[j] != b[j-1]+1 {
			return false
		}
	}
	return true
}
