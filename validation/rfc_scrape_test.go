package validation

import (
	"bufio"
	"strings"
	"testing"
)

// scrape runs the real parser over a crafted wire response and returns what
// it counted. Deliberately calls scrapeRFCResponse itself rather than
// re-implementing the rules: a test that mirrors the logic it checks passes
// whatever the production code does.
func scrape(t *testing.T, method, wire string) rfcSnapshot {
	t.Helper()
	var tally rfcTally
	scrapeRFCResponse(bufio.NewReader(strings.NewReader(wire)), method, &tally)
	return tally.snapshot()
}

const cleanGET = "HTTP/1.1 200 OK\r\n" +
	"Content-Type: text/plain\r\n" +
	"Content-Length: 5\r\n" +
	"\r\n" +
	"hello"

// TestRFCScraperPassesAWellFormedResponse is the control every other case in
// this file is measured against. A scraper that flags a correct response
// would make I-RFC-1 and I-RFC-2 fire on every cell, which is worse than
// leaving them uninstrumented.
func TestRFCScraperPassesAWellFormedResponse(t *testing.T) {
	got := scrape(t, "GET", cleanGET)
	if got.Exchanges != 1 {
		t.Fatalf("exchanges = %d, want 1", got.Exchanges)
	}
	assertNoViolations(t, got)
}

func TestRFCScraperCatchesFramingViolations(t *testing.T) {
	cases := []struct {
		name   string
		method string
		wire   string
		want   func(rfcSnapshot) int64
		field  string
	}{
		{
			name:   "HEAD carrying a body (RFC 9110 §9.3.2)",
			method: "HEAD",
			wire:   "HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhello",
			want:   func(s rfcSnapshot) int64 { return s.HeadWithBody },
			field:  "head_with_body",
		},
		{
			name:   "204 carrying a body (RFC 9110 §15.3.5)",
			method: "GET",
			wire:   "HTTP/1.1 204 No Content\r\n\r\nnope",
			want:   func(s rfcSnapshot) int64 { return s.Body204 },
			field:  "204_with_body",
		},
		{
			name:   "304 carrying a body (RFC 9110 §15.4.5)",
			method: "GET",
			wire:   "HTTP/1.1 304 Not Modified\r\n\r\nstale",
			want:   func(s rfcSnapshot) int64 { return s.Body304 },
			field:  "304_with_body",
		},
		{
			name:   "Content-Length longer than the body",
			method: "GET",
			wire:   "HTTP/1.1 200 OK\r\nContent-Length: 99\r\n\r\nshort",
			want:   func(s rfcSnapshot) int64 { return s.BadFraming },
			field:  "bad_framing",
		},
		{
			name:   "Content-Length shorter than the body",
			method: "GET",
			wire:   "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nmuch longer than two",
			want:   func(s rfcSnapshot) int64 { return s.BadFraming },
			field:  "bad_framing",
		},
		{
			name:   "both Content-Length and chunked (RFC 9112 §6.1, smuggling primitive)",
			method: "GET",
			wire:   "HTTP/1.1 200 OK\r\nContent-Length: 5\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n0\r\n\r\n",
			want:   func(s rfcSnapshot) int64 { return s.BadFraming },
			field:  "bad_framing",
		},
		{
			name:   "chunked stream with no terminating zero chunk",
			method: "GET",
			wire:   "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n",
			want:   func(s rfcSnapshot) int64 { return s.MissingChunkEnd },
			field:  "missing_chunk_end",
		},
		{
			name:   "NUL byte in a header value (RFC 9110 §5.5)",
			method: "GET",
			wire:   "HTTP/1.1 200 OK\r\nX-Bad: va\x00lue\r\nContent-Length: 0\r\n\r\n",
			want:   func(s rfcSnapshot) int64 { return s.NULInHeader },
			field:  "nul_in_header",
		},
		{
			name:   "obs-fold continuation line (RFC 9112 §5.2 forbids generating it)",
			method: "GET",
			wire:   "HTTP/1.1 200 OK\r\nX-Split: first\r\n  smuggled\r\nContent-Length: 0\r\n\r\n",
			want:   func(s rfcSnapshot) int64 { return s.CRLFInHeader },
			field:  "crlf_in_header",
		},
		{
			name:   "header line with no colon, the signature of a split value",
			method: "GET",
			wire:   "HTTP/1.1 200 OK\r\nX-Ok: fine\r\nGET /smuggled HTTP/1.1\r\nContent-Length: 0\r\n\r\n",
			want:   func(s rfcSnapshot) int64 { return s.CRLFInHeader },
			field:  "crlf_in_header",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := scrape(t, tc.method, tc.wire)
			if n := tc.want(got); n == 0 {
				t.Fatalf("%s = 0, want > 0 — the scraper did not detect this violation\nwire:\n%q", tc.field, tc.wire)
			}
			// The same rule must stay silent on a correct response, so a
			// counter that simply always fires cannot pass this suite.
			if n := tc.want(scrape(t, "GET", cleanGET)); n != 0 {
				t.Fatalf("%s = %d on a well-formed response, want 0", tc.field, n)
			}
		})
	}
}

// TestRFCScraperExemptsHEADContentLength guards a rule that is easy to get
// backwards: a HEAD response SHOULD carry the Content-Length a GET would
// have produced, and must not be scored as a length mismatch for doing so.
func TestRFCScraperExemptsHEADContentLength(t *testing.T) {
	got := scrape(t, "HEAD", "HTTP/1.1 200 OK\r\nContent-Length: 1234\r\n\r\n")
	if got.BadFraming != 0 {
		t.Fatalf("bad_framing = %d on a correct HEAD response; Content-Length without a body is required, not a violation", got.BadFraming)
	}
	if got.HeadWithBody != 0 {
		t.Fatalf("head_with_body = %d on a HEAD response that sent no body", got.HeadWithBody)
	}
}

// TestRFCScraperAcceptsCleanChunked: chunked framing done correctly must not
// register, including the trailing CRLF handling that is easy to miscount.
func TestRFCScraperAcceptsCleanChunked(t *testing.T) {
	got := scrape(t, "GET",
		"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n"+
			"5\r\nhello\r\n6\r\n world\r\n0\r\n\r\n")
	if got.SawChunked != 1 {
		t.Fatalf("saw_chunked = %d, want 1", got.SawChunked)
	}
	assertNoViolations(t, got)
}

// TestRFCScraperAcceptsBodylessSuccess covers the common 204 and 304 case
// done right, since those are the responses the scraper is strictest about.
func TestRFCScraperAcceptsBodylessSuccess(t *testing.T) {
	for _, wire := range []string{
		"HTTP/1.1 204 No Content\r\n\r\n",
		"HTTP/1.1 304 Not Modified\r\nETag: \"abc\"\r\n\r\n",
	} {
		assertNoViolations(t, scrape(t, "GET", wire))
	}
}

func assertNoViolations(t *testing.T, s rfcSnapshot) {
	t.Helper()
	for _, v := range []struct {
		name string
		n    int64
	}{
		{"bad_framing", s.BadFraming},
		{"head_with_body", s.HeadWithBody},
		{"204_with_body", s.Body204},
		{"304_with_body", s.Body304},
		{"missing_chunk_end", s.MissingChunkEnd},
		{"crlf_in_header", s.CRLFInHeader},
		{"nul_in_header", s.NULInHeader},
	} {
		if v.n != 0 {
			t.Errorf("%s = %d on a conforming response, want 0", v.name, v.n)
		}
	}
}

// TestSummariseRFCConformance pins the operator-facing summary line. Matches
// the shape the WS-torture, SSE-kill and h2c-churn slices each use for their
// own summary: the line is what a human reads when scanning a cell log, so
// the counters that decide the verdict must all be in it.
func TestSummariseRFCConformance(t *testing.T) {
	got := summariseRFCConformance(rfcSnapshot{
		Exchanges:       1400,
		BadFraming:      3,
		HeadWithBody:    2,
		Body204:         1,
		Body304:         4,
		MissingChunkEnd: 5,
		CRLFInHeader:    6,
		NULInHeader:     7,
	})
	for _, want := range []string{
		"1400 exchanges", "framing=3", "head_body=2", "204_body=1",
		"304_body=4", "chunk_end=5", "crlf=6", "nul=7",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q: got %q", want, got)
		}
	}
}
