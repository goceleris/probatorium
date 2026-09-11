package validation

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Response-conformance slice (I-RFC-1, I-RFC-2).
//
// Both predicates were registered against counters nothing ever set, and the
// waiver said they "need the response-scraping MITM". A proxy in front of the
// refapp is the wrong instrument here for a measurable reason: the Tier 1
// walkers drive ~219k requests/s in a soak cell, so a byte-level parser in
// that path would change the very load profile the rest of the run is
// measuring, and every historical comparison with it.
//
// What these predicates actually need is not volume but SHAPE. A
// Content-Length that disagrees with the body, a HEAD that carries one, a
// chunked response with no terminator — each is a property of a single
// response, and one malformed response is as diagnostic as a million. So this
// is a dedicated low-rate slice on a raw socket, in the same pattern as the
// WebSocket-torture and SSE-kill walkers: it reads the bytes celeris actually
// wrote, independently of what celeris's own counters claim, which is the
// independence the waiver was really asking for.
//
// Raw sockets matter beyond cost. Go's http.Client normalises away most of
// what is under test here: it discards HEAD bodies, special-cases 204/304,
// and rejects malformed headers at parse time rather than reporting them. A
// client that tolerates a violation cannot count it.

// rfcConformanceInterval paces one request/response exchange. Deliberately
// slow: the slice is a correctness probe, not load. At 250ms a 1h soak cell
// still examines ~14,000 responses, which is far more than enough to catch a
// framing rule that is broken rather than flaky.
const rfcConformanceInterval = 250 * time.Millisecond

// rfcDialTimeout and rfcIOTimeout bound one exchange so a wedged refapp
// stalls this slice rather than the run.
const (
	rfcDialTimeout = 5 * time.Second
	rfcIOTimeout   = 10 * time.Second
)

// rfcMaxBody caps how much of a response body the scraper will read. Bodies
// here are small by construction; the cap exists so a refapp that streams
// forever cannot make the slice allocate without bound.
const rfcMaxBody = 1 << 20

// rfcTally counts what the scraper observed on the wire. Every field maps to
// a properties.Snapshot counter the two predicates already read.
type rfcTally struct {
	exchanges       atomic.Int64
	dialFail        atomic.Int64
	readFail        atomic.Int64
	headProbes      atomic.Int64
	conditionals    atomic.Int64
	saw204          atomic.Int64
	saw304          atomic.Int64
	sawChunked      atomic.Int64
	badFraming      atomic.Int64
	headWithBody    atomic.Int64
	body204         atomic.Int64
	body304         atomic.Int64
	missingChunkEnd atomic.Int64
	crlfInHeader    atomic.Int64
	nulInHeader     atomic.Int64
}

// rfcSnapshot is the JSON shape for the cell document.
type rfcSnapshot struct {
	Exchanges       int64 `json:"rfc_exchanges"`
	DialFail        int64 `json:"rfc_dial_fail"`
	ReadFail        int64 `json:"rfc_read_fail"`
	HeadProbes      int64 `json:"rfc_head_probes"`
	Conditionals    int64 `json:"rfc_conditional_probes"`
	Saw204          int64 `json:"rfc_saw_204"`
	Saw304          int64 `json:"rfc_saw_304"`
	SawChunked      int64 `json:"rfc_saw_chunked"`
	BadFraming      int64 `json:"rfc_bad_framing"`
	HeadWithBody    int64 `json:"rfc_head_with_body"`
	Body204         int64 `json:"rfc_204_with_body"`
	Body304         int64 `json:"rfc_304_with_body"`
	MissingChunkEnd int64 `json:"rfc_missing_chunk_end"`
	CRLFInHeader    int64 `json:"rfc_crlf_in_header"`
	NULInHeader     int64 `json:"rfc_nul_in_header"`
}

// ResponseCounters is the subset of the scraper's counts that the two
// predicates judge. Kept separate from rfcSnapshot (which is the JSON shape
// for the cell document, and carries diagnostics like dial failures and how
// many 304s were even seen) so the property loop copies only what it judges
// and a new diagnostic counter cannot silently become a gate signal.
type ResponseCounters struct {
	// Exchanges is how many responses the scraper actually parsed. Zero
	// means the slice never ran (below the concurrency threshold, or the
	// refapp never answered), and the counters below are structurally zero
	// rather than clean -- which is the difference the gate must see.
	Exchanges       int64
	BadFraming      int64
	HeadWithBody    int64
	Body204         int64
	Body304         int64
	MissingChunkEnd int64
	CRLFInHeader    int64
	NULInHeader     int64
}

// counters returns what I-RFC-1 and I-RFC-2 read.
func (t *rfcTally) counters() ResponseCounters {
	return ResponseCounters{
		Exchanges:       t.exchanges.Load(),
		BadFraming:      t.badFraming.Load(),
		HeadWithBody:    t.headWithBody.Load(),
		Body204:         t.body204.Load(),
		Body304:         t.body304.Load(),
		MissingChunkEnd: t.missingChunkEnd.Load(),
		CRLFInHeader:    t.crlfInHeader.Load(),
		NULInHeader:     t.nulInHeader.Load(),
	}
}

func (t *rfcTally) snapshot() rfcSnapshot {
	return rfcSnapshot{
		Exchanges: t.exchanges.Load(), DialFail: t.dialFail.Load(), ReadFail: t.readFail.Load(),
		HeadProbes: t.headProbes.Load(), Conditionals: t.conditionals.Load(),
		Saw204: t.saw204.Load(), Saw304: t.saw304.Load(), SawChunked: t.sawChunked.Load(),
		BadFraming: t.badFraming.Load(), HeadWithBody: t.headWithBody.Load(),
		Body204: t.body204.Load(), Body304: t.body304.Load(),
		MissingChunkEnd: t.missingChunkEnd.Load(),
		CRLFInHeader:    t.crlfInHeader.Load(), NULInHeader: t.nulInHeader.Load(),
	}
}

// rfcProbe is one request shape the slice rotates through.
type rfcProbe struct {
	method string
	// conditional adds If-None-Match:* to elicit a 304 from refapps that
	// serve ETags. A refapp that does not is not in violation, so a 200 here
	// is simply scored as a 200 and the 304 rules do not apply.
	conditional bool
}

var rfcProbes = []rfcProbe{
	{method: "GET"},
	{method: "HEAD"},
	{method: "GET", conditional: true},
}

// runRFCConformanceWalker drives the slice until ctx is done. One exchange per
// tick on a fresh connection: keep-alive would be more efficient, but a fresh
// conn per exchange means a Content-Length that disagrees with the body is
// observable as a short or long read rather than as a desync that could be
// blamed on the following request.
func runRFCConformanceWalker(ctx context.Context, hostPort, path string,
	seed uint64, interval time.Duration, tally *rfcTally,
) {
	if interval <= 0 {
		interval = rfcConformanceInterval
	}
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			p := rfcProbes[rng.IntN(len(rfcProbes))]
			fireRFCProbe(ctx, hostPort, path, p, tally)
		}
	}
}

func fireRFCProbe(ctx context.Context, hostPort, path string, p rfcProbe, tally *rfcTally) {
	d := net.Dialer{Timeout: rfcDialTimeout}
	c, err := d.DialContext(ctx, "tcp", hostPort)
	if err != nil {
		tally.dialFail.Add(1)
		return
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(rfcIOTimeout))

	var req strings.Builder
	fmt.Fprintf(&req, "%s %s HTTP/1.1\r\nHost: %s\r\n", p.method, path, hostPort)
	if p.conditional {
		req.WriteString("If-None-Match: *\r\n")
		tally.conditionals.Add(1)
	}
	if p.method == "HEAD" {
		tally.headProbes.Add(1)
	}
	// Close the connection after this exchange so a chunked response must
	// terminate, and so a body that outruns its Content-Length shows up as
	// trailing bytes rather than as the next response.
	req.WriteString("Connection: close\r\n\r\n")
	if _, err := c.Write([]byte(req.String())); err != nil {
		tally.readFail.Add(1)
		return
	}
	scrapeRFCResponse(bufio.NewReader(c), p.method, tally)
}

// scrapeRFCResponse parses one HTTP/1 response off the wire and scores every
// framing rule that applies to it. It is deliberately its own parser rather
// than http.ReadResponse: the standard reader silently repairs several of the
// exact conditions under test (it discards HEAD bodies, ignores a body on
// 204/304, and errors out on malformed headers instead of counting them), so
// using it would make the predicates structurally unable to fire.
func scrapeRFCResponse(br *bufio.Reader, method string, tally *rfcTally) {
	status, ok := readRFCStatusLine(br)
	if !ok {
		tally.readFail.Add(1)
		return
	}
	tally.exchanges.Add(1)

	contentLength := int64(-1)
	chunked := false
	sawCL := false
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			tally.readFail.Add(1)
			return
		}
		raw := strings.TrimRight(line, "\r\n")
		if raw == "" {
			break // end of header block
		}
		scoreRFCHeaderLine(raw, tally)
		name, value, found := strings.Cut(raw, ":")
		if !found {
			continue // already scored as malformed by scoreRFCHeaderLine
		}
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "content-length":
			if n, cerr := strconv.ParseInt(strings.TrimSpace(value), 10, 64); cerr == nil {
				contentLength = n
				sawCL = true
			}
		case "transfer-encoding":
			if strings.Contains(strings.ToLower(value), "chunked") {
				chunked = true
			}
		}
	}

	// RFC 9112 §6.1: a message must not carry both. Doing so is the classic
	// request-smuggling primitive.
	if sawCL && chunked {
		tally.badFraming.Add(1)
	}
	if chunked {
		tally.sawChunked.Add(1)
	}
	switch status {
	case 204:
		tally.saw204.Add(1)
	case 304:
		tally.saw304.Add(1)
	}

	body, terminated := readRFCBody(br, chunked, contentLength)

	// RFC 9110 §9.3.2 / §15.3.5 / §15.4.5: these carry no body, ever.
	switch {
	case method == "HEAD" && body > 0:
		tally.headWithBody.Add(1)
	case status == 204 && body > 0:
		tally.body204.Add(1)
	case status == 304 && body > 0:
		tally.body304.Add(1)
	}
	// A declared length the body does not honour. HEAD is exempt: its
	// Content-Length describes the body a GET would have returned.
	if sawCL && !chunked && method != "HEAD" && status != 204 && status != 304 && body != contentLength {
		tally.badFraming.Add(1)
	}
	if chunked && !terminated {
		tally.missingChunkEnd.Add(1)
	}
}

// readRFCStatusLine returns the status code from "HTTP/1.x NNN reason".
func readRFCStatusLine(br *bufio.Reader) (int, bool) {
	line, err := br.ReadString('\n')
	if err != nil {
		return 0, false
	}
	raw := strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(raw, "HTTP/1.") {
		return 0, false
	}
	parts := strings.SplitN(raw, " ", 3)
	if len(parts) < 2 {
		return 0, false
	}
	code, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, false
	}
	return code, true
}

// scoreRFCHeaderLine judges one header line for I-RFC-2.
//
// A NUL is directly observable: RFC 9110 §5.5 forbids it in a field value and
// nothing legitimate produces one, so its presence on the wire is the
// violation itself.
//
// A CRLF injected into a field value is NOT directly observable — on the wire
// it is indistinguishable from the end of that header and the start of
// another, which is precisely what makes it a smuggling vector. What it
// leaves behind is a line that is not a well-formed field: either it has no
// colon, or it begins with whitespace (obs-fold, which RFC 9112 §5.2 requires
// a server not to generate). Those are the observable signature, and that is
// what is counted. A value-splitting injection that happens to produce a
// syntactically valid header is not detectable from the response alone by any
// method, including a proxy, and this comment exists so nobody later reads a
// zero here as proof of more than it is.
func scoreRFCHeaderLine(raw string, tally *rfcTally) {
	if strings.ContainsRune(raw, 0) {
		tally.nulInHeader.Add(1)
		return
	}
	if raw != "" && (raw[0] == ' ' || raw[0] == '\t') {
		tally.crlfInHeader.Add(1) // obs-fold: a server must not emit this
		return
	}
	if !strings.Contains(raw, ":") {
		tally.crlfInHeader.Add(1) // not a field at all — a split value looks like this
	}
}

// readRFCBody consumes the body and reports its length and, for chunked,
// whether the terminating zero-length chunk arrived. The connection was
// requested closed, so a non-chunked body without Content-Length ends at EOF.
func readRFCBody(br *bufio.Reader, chunked bool, contentLength int64) (int64, bool) {
	if chunked {
		var total int64
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return total, false // EOF before the terminator
			}
			sizeField, _, _ := strings.Cut(strings.TrimRight(line, "\r\n"), ";")
			n, cerr := strconv.ParseInt(strings.TrimSpace(sizeField), 16, 64)
			if cerr != nil {
				return total, false
			}
			if n == 0 {
				return total, true // terminator seen
			}
			if total+n > rfcMaxBody {
				return total, true // capped; not a framing verdict
			}
			buf := make([]byte, n)
			if _, err := readFullRFC(br, buf); err != nil {
				return total, false
			}
			total += n
			// Trailing CRLF after the chunk data.
			if _, err := br.ReadString('\n'); err != nil {
				return total, false
			}
		}
	}
	// Read to EOF (Connection: close) and report what actually arrived, so a
	// body shorter or longer than Content-Length is both visible.
	var sink bytes.Buffer
	limit := int64(rfcMaxBody)
	if contentLength >= 0 && contentLength < limit {
		// Allow one byte past the declared length so an over-long body is
		// observable rather than silently truncated to a match.
		limit = contentLength + 1
	}
	n, _ := sink.ReadFrom(&limitedReader{r: br, n: limit})
	return n, true
}

// limitedReader is io.LimitedReader with an int64 budget; the stdlib type is
// fine but this keeps the read loop's intent local and avoids importing io
// for one use.
type limitedReader struct {
	r *bufio.Reader
	n int64
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.n <= 0 {
		return 0, errRFCLimit
	}
	if int64(len(p)) > l.n {
		p = p[:l.n]
	}
	n, err := l.r.Read(p)
	l.n -= int64(n)
	return n, err
}

var errRFCLimit = fmt.Errorf("rfc scraper: body cap reached")

func readFullRFC(br *bufio.Reader, buf []byte) (int, error) {
	got := 0
	for got < len(buf) {
		n, err := br.Read(buf[got:])
		got += n
		if err != nil {
			return got, err
		}
	}
	return got, nil
}

func summariseRFCConformance(s rfcSnapshot) string {
	return fmt.Sprintf("rfc: %d exchanges, framing=%d head_body=%d 204_body=%d 304_body=%d chunk_end=%d crlf=%d nul=%d",
		s.Exchanges, s.BadFraming, s.HeadWithBody, s.Body204, s.Body304, s.MissingChunkEnd, s.CRLFInHeader, s.NULInHeader)
}
