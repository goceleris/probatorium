package main

import (
	"bytes"
	"errors"
	"strconv"
)

// HTTP/1.1 framing over gnet's inbound ring buffer.
//
// gnet hands OnTraffic a [gnet.Conn] whose buffered bytes are inspected with
// Peek (no advance) and consumed with Discard. There is no net/http on this
// path: the bufio.Reader / http.ReadRequest machinery wants an io.Reader and
// would defeat gnet's zero-copy buffer, so we parse the request line + headers
// inline and only materialise the two fields the contract actually reads — the
// method+target (for routing) and the body length (for /upload draining).
//
// Each OnTraffic call drains as many complete pipelined requests as the buffer
// holds; a partial request is left in the buffer untouched until the next
// readiness event delivers the remainder.

var (
	crlfcrlf = []byte("\r\n\r\n")
	crlf     = []byte("\r\n")

	errPartial   = errors.New("incomplete request")
	errMalformed = errors.New("malformed request")
)

// request is the parsed view of one HTTP/1.1 message needed by the contract
// dispatch. target retains the raw request-target (path + optional query);
// callers split the query off themselves. keepAlive reflects the effective
// HTTP/1.1 connection disposition after honouring Connection: close.
type request struct {
	method    []byte
	target    []byte
	keepAlive bool
}

// parseRequest decodes one HTTP/1.1 request from buf. On success it returns the
// parsed request and the total number of header+body bytes consumed (the amount
// to Discard). errPartial means buf does not yet hold a full request — the
// caller stops and waits for more bytes. errMalformed means the framing is
// unrecoverable and the connection must be closed.
func parseRequest(buf []byte) (request, int, error) {
	headerEnd := bytes.Index(buf, crlfcrlf)
	if headerEnd < 0 {
		return request{}, 0, errPartial
	}
	headerBlock := buf[:headerEnd]
	bodyStart := headerEnd + len(crlfcrlf)

	lineEnd := bytes.Index(headerBlock, crlf)
	if lineEnd < 0 {
		lineEnd = len(headerBlock)
	}
	reqLine := headerBlock[:lineEnd]

	sp1 := bytes.IndexByte(reqLine, ' ')
	if sp1 <= 0 {
		return request{}, 0, errMalformed
	}
	sp2 := bytes.IndexByte(reqLine[sp1+1:], ' ')
	if sp2 < 0 {
		return request{}, 0, errMalformed
	}
	method := reqLine[:sp1]
	target := reqLine[sp1+1 : sp1+1+sp2]
	proto := reqLine[sp1+1+sp2+1:]

	// HTTP/1.1 defaults to keep-alive; HTTP/1.0 defaults to close. The
	// Connection header then overrides whichever default applies.
	keepAlive := bytes.Equal(proto, []byte("HTTP/1.1"))

	contentLength := 0
	rest := headerBlock[lineEnd:]
	for len(rest) > 0 {
		if bytes.HasPrefix(rest, crlf) {
			rest = rest[len(crlf):]
		}
		if len(rest) == 0 {
			break
		}
		nl := bytes.Index(rest, crlf)
		var line []byte
		if nl < 0 {
			line = rest
			rest = nil
		} else {
			line = rest[:nl]
			rest = rest[nl:]
		}
		colon := bytes.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		name := bytes.TrimSpace(line[:colon])
		value := bytes.TrimSpace(line[colon+1:])
		switch {
		case asciiEqualFold(name, "content-length"):
			n, err := strconv.Atoi(string(value))
			if err != nil || n < 0 {
				return request{}, 0, errMalformed
			}
			contentLength = n
		case asciiEqualFold(name, "connection"):
			if asciiEqualFold(value, "close") {
				keepAlive = false
			} else if asciiEqualFold(value, "keep-alive") {
				keepAlive = true
			}
		}
	}

	total := bodyStart + contentLength
	if len(buf) < total {
		return request{}, 0, errPartial
	}

	return request{method: method, target: target, keepAlive: keepAlive}, total, nil
}

// asciiEqualFold reports whether b equals lower (an already-lowercase ASCII
// literal) ignoring ASCII case. Avoids the bytes.ToLower allocation on the
// header hot path.
func asciiEqualFold(b []byte, lower string) bool {
	if len(b) != len(lower) {
		return false
	}
	for i := range b {
		c := b[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != lower[i] {
			return false
		}
	}
	return true
}
