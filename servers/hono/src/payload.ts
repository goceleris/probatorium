// Deterministic 1 KiB / 64 KiB JSON payload generator.
//
// Byte-identical port of probatorium/servers/common/payload.go. The Go
// reference uses encoding/json on (paginatedResponse, paginatedItem),
// which emits compact JSON with field order matching struct declaration
// order: page, per_page, total, total_pages, data for the wrapper, and
// id, name, email, status, created_at per item.
//
// We emit the bytes by hand here rather than via JSON.stringify on a
// JS object. Reasons:
//
//   1. Byte-for-byte equivalence with the Go reference is a hard
//      conformance requirement (cmd/conformance does bytes::Equal).
//      JSON.stringify and encoding/json agree on most things but offer
//      no formal cross-language guarantee, so we own the bytes.
//   2. The corpus is tiny and pure-ASCII (no escape hazards), so the
//      manual path is correct and trivially auditable.
//   3. Generated once at startup and reused for every request, so the
//      build cost is irrelevant.
//
// Termination rule from the Go reference: append items until the
// marshalled length is at least targetSize. Resulting sizes:
//   1 KiB target  -> 1026 bytes ending at item 9
//   64 KiB target -> 65618 bytes ending at item 583

const HEADER = '{"page":1,"per_page":50,"total":1000,"total_pages":20,"data":[';
const FOOTER = "]}";

let json1k: Uint8Array | undefined;
let json64k: Uint8Array | undefined;

export function json1KPayload(): Uint8Array {
  if (!json1k) json1k = generate(1024);
  return json1k;
}

export function json64KPayload(): Uint8Array {
  if (!json64k) json64k = generate(65536);
  return json64k;
}

function generate(targetSize: number): Uint8Array {
  // Emit straight into a string then encode once at the end. Bun's
  // string concatenation is rope-backed so per-iteration append is
  // cheap; we'd lose nothing by switching to a Bun.ArrayBufferSink, but
  // this runs exactly twice at process startup.
  let buf = HEADER;
  let i = 1;
  // Match the Go termination rule: stop once buf + footer length is at
  // least targetSize. The Go code does a full Marshal per iteration
  // which is equivalent — the wrapper struct serialises to a fixed
  // footer length, so length(buf) + length(footer) is exact.
  while (true) {
    if (i > 1) buf += ",";
    buf += item(i);
    if (buf.length + FOOTER.length >= targetSize) break;
    i += 1;
  }
  buf += FOOTER;
  // Pure-ASCII so byte length == char length; TextEncoder is correct
  // and idiomatic regardless.
  return new TextEncoder().encode(buf);
}

function item(n: number): string {
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
