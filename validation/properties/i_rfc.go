package properties

import "fmt"

// IRFC1 asserts response framing matches RFC 9110 / 9112: Content-Length
// equals body bytes, HEAD / 204 / 304 carry no body, chunked responses
// have exactly one terminator. These signals are fed by the validator's
// MITM response scraper — counters increment when the scraper observes
// a violation on the wire, regardless of what celeris's own counters
// say. Hard-fail on the first non-zero hit.
var IRFC1 = Spec{
	ID:          "I-RFC-1",
	Description: "Content-Length matches body bytes; no body for HEAD/204/304; single chunked terminator",
	Tier:        "core",
	Predicate: func(snap *Snapshot, _ Context) (bool, string) {
		if snap.ResponsesBadFraming > 0 {
			return false, fmt.Sprintf(
				"I-RFC-1 violated: %d responses with bad framing (CL mismatch / double TE / missing chunked terminator)",
				snap.ResponsesBadFraming)
		}
		if snap.ResponsesHeadWithBody > 0 {
			return false, fmt.Sprintf(
				"I-RFC-1 violated: %d HEAD responses carried a body (RFC 9110 §9.3.2)",
				snap.ResponsesHeadWithBody)
		}
		if snap.Responses204WithBody > 0 {
			return false, fmt.Sprintf(
				"I-RFC-1 violated: %d 204 responses carried a body (RFC 9110 §15.3.5)",
				snap.Responses204WithBody)
		}
		if snap.Responses304WithBody > 0 {
			return false, fmt.Sprintf(
				"I-RFC-1 violated: %d 304 responses carried a body (RFC 9110 §15.4.5)",
				snap.Responses304WithBody)
		}
		if snap.ResponsesMissingChunkEnd > 0 {
			return false, fmt.Sprintf(
				"I-RFC-1 violated: %d chunked responses without a terminating zero-length chunk",
				snap.ResponsesMissingChunkEnd)
		}
		return true, ""
	},
}

// IRFC2 asserts no CRLF or NUL byte appears in a response header value.
// CRLF injection is a header-smuggling vector; NUL is forbidden by
// RFC 9110 §5.5 in field-values. celeris must reject the header at
// serialization time. The scraper counts violations it observes on the
// wire.
var IRFC2 = Spec{
	ID:          "I-RFC-2",
	Description: "no CRLF / NUL in header values",
	Tier:        "core",
	Predicate: func(snap *Snapshot, _ Context) (bool, string) {
		if snap.ResponsesCRLFInHeader > 0 {
			return false, fmt.Sprintf(
				"I-RFC-2 violated: %d response headers carried CRLF (header-smuggling vector)",
				snap.ResponsesCRLFInHeader)
		}
		if snap.ResponsesNULInHeader > 0 {
			return false, fmt.Sprintf(
				"I-RFC-2 violated: %d response headers carried NUL (RFC 9110 §5.5)",
				snap.ResponsesNULInHeader)
		}
		return true, ""
	},
}
