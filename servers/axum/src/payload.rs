// Deterministic 1 KiB / 64 KiB JSON payload generator.
//
// Byte-identical port of probatorium/servers/common/payload.go. The Go
// reference uses encoding/json on the (paginatedResponse, paginatedItem)
// struct pair, which emits compact JSON with field order matching struct
// declaration order: id, name, email, status, created_at for each item;
// page, per_page, total, total_pages, data for the wrapper.
//
// We emit the bytes by hand here rather than via serde_json + a derived
// Serialize impl. Reasons:
//
//   1. Byte-for-byte equivalence with the Go reference is a hard
//      conformance requirement (cmd/conformance does bytes::Equal). Any
//      formatting drift between serde_json and encoding/json would fail
//      the harness — even though both libraries claim "compact JSON",
//      there is no formal cross-language guarantee on numeric or string
//      escaping for the exact corpus we emit.
//   2. The payload corpus is tiny and entirely ASCII (no escapes
//      needed beyond what manual formatting handles), so the manual path
//      is both correct and trivially auditable.
//   3. Generated once at startup and reused for every request, so the
//      build cost is irrelevant.
//
// The Go termination rule is "append items until the marshalled length
// crosses targetSize" — we mirror that. Resulting sizes:
//   1 KiB target  → 1026 bytes ending at item 9
//   64 KiB target → 65618 bytes ending at item 583

use std::sync::OnceLock;

static JSON_1K: OnceLock<Vec<u8>> = OnceLock::new();
static JSON_8K: OnceLock<Vec<u8>> = OnceLock::new();
static JSON_16K: OnceLock<Vec<u8>> = OnceLock::new();
static JSON_64K: OnceLock<Vec<u8>> = OnceLock::new();

pub fn json_1k() -> &'static [u8] {
    JSON_1K.get_or_init(|| generate(1024)).as_slice()
}

pub fn json_8k() -> &'static [u8] {
    JSON_8K.get_or_init(|| generate(8192)).as_slice()
}

pub fn json_16k() -> &'static [u8] {
    JSON_16K.get_or_init(|| generate(16384)).as_slice()
}

pub fn json_64k() -> &'static [u8] {
    JSON_64K.get_or_init(|| generate(65536)).as_slice()
}

// generate builds a paginated-response payload of at least target_size
// bytes using the same termination rule as the Go reference.
fn generate(target_size: usize) -> Vec<u8> {
    // Header is a constant prefix — identical for every payload size.
    let header = br#"{"page":1,"per_page":50,"total":1000,"total_pages":20,"data":["#;
    // Footer closes the data array and the wrapper object.
    let footer = b"]}";

    let mut buf: Vec<u8> = Vec::with_capacity(target_size + 256);
    buf.extend_from_slice(header);

    let mut i: u64 = 1;
    loop {
        if i > 1 {
            buf.push(b',');
        }
        append_item(&mut buf, i);
        // Tentative size = current buf + footer length. The Go code
        // marshals the whole thing on every iteration, but we can avoid
        // the recompute since the footer is fixed-length.
        if buf.len() + footer.len() >= target_size {
            break;
        }
        i += 1;
    }
    buf.extend_from_slice(footer);
    buf
}

// append_item writes one paginatedItem in the exact byte form
// encoding/json produces for the Go struct:
//   {"id":<n>,"name":"User <n>","email":"user<n>@example.com",
//    "status":"active","created_at":"2024-01-15T09:30:00Z"}
fn append_item(buf: &mut Vec<u8>, n: u64) {
    buf.extend_from_slice(br#"{"id":"#);
    push_u64(buf, n);
    buf.extend_from_slice(br#","name":"User "#);
    push_u64(buf, n);
    buf.extend_from_slice(br#"","email":"user"#);
    push_u64(buf, n);
    buf.extend_from_slice(br#"@example.com","status":"active","created_at":"2024-01-15T09:30:00Z"}"#);
}

// push_u64 appends the decimal representation of n. Faster and
// allocation-free vs format!("{}", n) — the only "hot" path here on
// startup, so worth being tidy about.
fn push_u64(buf: &mut Vec<u8>, mut n: u64) {
    if n == 0 {
        buf.push(b'0');
        return;
    }
    let mut tmp = [0u8; 20];
    let mut idx = tmp.len();
    while n > 0 {
        idx -= 1;
        tmp[idx] = b'0' + (n % 10) as u8;
        n /= 10;
    }
    buf.extend_from_slice(&tmp[idx..]);
}

#[cfg(test)]
mod tests {
    use super::*;

    // The Go reference produces these exact lengths. If a future change
    // alters the byte layout, this test breaks before the conformance
    // harness has a chance to surface it on the cluster.
    #[test]
    fn json_1k_matches_go_size() {
        assert_eq!(json_1k().len(), 1026);
    }

    #[test]
    fn json_8k_matches_go_size() {
        assert_eq!(json_8k().len(), 8286);
    }

    #[test]
    fn json_16k_matches_go_size() {
        assert_eq!(json_16k().len(), 16463);
    }

    #[test]
    fn json_64k_matches_go_size() {
        assert_eq!(json_64k().len(), 65618);
    }

    #[test]
    fn json_1k_starts_with_header() {
        let p = json_1k();
        assert!(p.starts_with(br#"{"page":1,"per_page":50,"total":1000,"total_pages":20,"data":["#));
        assert!(p.ends_with(b"}"));
    }
}
