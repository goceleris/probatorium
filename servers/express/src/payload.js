// Deterministic 1/8/16/64 KiB JSON payload generator.
//
// Byte-identical port of probatorium/servers/common/payload.go. The Go
// reference runs encoding/json over (paginatedResponse, paginatedItem),
// which emits compact JSON with field order matching struct declaration
// order: page, per_page, total, total_pages, data for the wrapper, and
// id, name, email, status, created_at per item.
//
// As in the bun adapters, the bytes are emitted by hand rather than via
// JSON.stringify on a JS object: byte-for-byte equivalence with the Go
// reference is a hard conformance requirement (cmd/conformance does a
// bytes-equal compare), and the two encoders carry no formal
// cross-language guarantee — so this module owns the bytes. The corpus is
// pure ASCII (no escape hazards) and is generated once at startup, so the
// manual path is both correct and trivially auditable.
//
// Termination rule from the Go reference: append items until the
// marshalled length is at least targetSize. Resulting sizes (the cluster
// conformance probe byte-compares these): 1k=1026, 8k=8286, 16k=16463,
// 64k=65618 bytes.

"use strict";

const HEADER =
  '{"page":1,"per_page":50,"total":1000,"total_pages":20,"data":[';
const FOOTER = "]}";

function item(n) {
  // Order MUST match the Go struct declaration exactly:
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
  // Build the string then encode once. The wrapper struct serialises to a
  // fixed-length footer, so length(buf) + length(FOOTER) is the exact
  // marshalled length — same termination predicate as the Go reference's
  // per-iteration json.Marshal.
  let buf = HEADER;
  let i = 1;
  while (true) {
    if (i > 1) buf += ",";
    buf += item(i);
    if (buf.length + FOOTER.length >= targetSize) break;
    i += 1;
  }
  buf += FOOTER;
  // Pure ASCII, so a latin1/utf-8 Buffer is byte-identical; Buffer.from
  // defaults to utf-8 which is correct here.
  return Buffer.from(buf, "utf-8");
}

let json1k;
let json8k;
let json16k;
let json64k;

function json1KPayload() {
  if (!json1k) json1k = generate(1024);
  return json1k;
}

function json8KPayload() {
  if (!json8k) json8k = generate(8192);
  return json8k;
}

function json16KPayload() {
  if (!json16k) json16k = generate(16384);
  return json16k;
}

function json64KPayload() {
  if (!json64k) json64k = generate(65536);
  return json64k;
}

module.exports = {
  json1KPayload,
  json8KPayload,
  json16KPayload,
  json64KPayload,
};
