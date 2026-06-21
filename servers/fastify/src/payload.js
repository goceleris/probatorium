// Deterministic 1/8/16/64 KiB JSON payload generator.
//
// Byte-identical port of probatorium/servers/common/payload.go. The Go
// reference runs encoding/json over (paginatedResponse, paginatedItem),
// which emits compact JSON with field order matching struct declaration
// order: page, per_page, total, total_pages, data for the wrapper, and
// id, name, email, status, created_at per item.
//
// We emit the bytes by hand rather than via JSON.stringify on a JS object.
// Byte-for-byte equivalence with the Go reference is a hard conformance
// requirement (cmd/conformance does a bytes-equal check); JSON.stringify
// and encoding/json agree on this pure-ASCII corpus, but owning the bytes
// removes any cross-runtime serialiser ambiguity. The corpus is generated
// once at startup and reused for every request, so the build cost never
// shows up in the bench.
//
// Termination rule from the Go reference: append items until the
// marshalled length is at least targetSize. Resulting sizes:
//   1 KiB target  -> 1026 bytes ending at item 9
//   8 KiB target  -> 8286 bytes ending at item 75
//   16 KiB target -> 16463 bytes ending at item 148
//   64 KiB target -> 65618 bytes ending at item 583

'use strict';

const HEADER = '{"page":1,"per_page":50,"total":1000,"total_pages":20,"data":[';
const FOOTER = ']}';

function item(n) {
  // Field order MUST match the Go struct declaration exactly:
  //   id, name, email, status, created_at.
  return (
    '{"id":' +
    n +
    ',"name":"User ' +
    n +
    '","email":"user' +
    n +
    '@example.com","status":"active","created_at":"2024-01-15T09:30:00Z"}'
  );
}

function generate(targetSize) {
  // Build the body as a string, encode once to a Buffer at the end. The
  // payload is pure ASCII so byte length equals string length, which makes
  // the Go termination predicate (marshalled length >= targetSize) exact
  // against buf.length + FOOTER.length here.
  let buf = HEADER;
  let i = 1;
  while (true) {
    if (i > 1) buf += ',';
    buf += item(i);
    if (buf.length + FOOTER.length >= targetSize) break;
    i += 1;
  }
  buf += FOOTER;
  return Buffer.from(buf, 'utf8');
}

// Generated once at module load and frozen into module-level constants so
// every request reuses the same immutable Buffer — no per-request alloc,
// mirroring the Go adapters that serve a pre-baked slice from
// servers/common.
const JSON_1K_PAYLOAD = generate(1024);
const JSON_8K_PAYLOAD = generate(8192);
const JSON_16K_PAYLOAD = generate(16384);
const JSON_64K_PAYLOAD = generate(65536);

module.exports = {
  JSON_1K_PAYLOAD,
  JSON_8K_PAYLOAD,
  JSON_16K_PAYLOAD,
  JSON_64K_PAYLOAD,
};
