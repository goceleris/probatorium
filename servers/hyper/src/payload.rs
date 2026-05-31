// Deterministic 1 KiB / 64 KiB JSON payload generator.
//
// Verbatim duplicate of servers/axum/src/payload.rs,
// servers/actix-web/src/payload.rs and servers/ntex/src/payload.rs. Each
// Rust adapter ships its own copy so the source tarball
// ansible/tasks/build_native_competitor.yml pushes is self-contained —
// no path-dep crate to coordinate. The duplication is bounded (~80 LOC)
// and the sizes are asserted by unit tests.
//
// See servers/axum/src/payload.rs for the design rationale (why we
// hand-emit bytes rather than using serde_json on a derived struct).

use std::sync::OnceLock;

static JSON_1K: OnceLock<Vec<u8>> = OnceLock::new();
static JSON_64K: OnceLock<Vec<u8>> = OnceLock::new();

pub fn json_1k() -> &'static [u8] {
    JSON_1K.get_or_init(|| generate(1024)).as_slice()
}

pub fn json_64k() -> &'static [u8] {
    JSON_64K.get_or_init(|| generate(65536)).as_slice()
}

fn generate(target_size: usize) -> Vec<u8> {
    let header = br#"{"page":1,"per_page":50,"total":1000,"total_pages":20,"data":["#;
    let footer = b"]}";

    let mut buf: Vec<u8> = Vec::with_capacity(target_size + 256);
    buf.extend_from_slice(header);

    let mut i: u64 = 1;
    loop {
        if i > 1 {
            buf.push(b',');
        }
        append_item(&mut buf, i);
        if buf.len() + footer.len() >= target_size {
            break;
        }
        i += 1;
    }
    buf.extend_from_slice(footer);
    buf
}

fn append_item(buf: &mut Vec<u8>, n: u64) {
    buf.extend_from_slice(br#"{"id":"#);
    push_u64(buf, n);
    buf.extend_from_slice(br#","name":"User "#);
    push_u64(buf, n);
    buf.extend_from_slice(br#"","email":"user"#);
    push_u64(buf, n);
    buf.extend_from_slice(br#"@example.com","status":"active","created_at":"2024-01-15T09:30:00Z"}"#);
}

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

    #[test]
    fn json_1k_matches_go_size() {
        assert_eq!(json_1k().len(), 1026);
    }

    #[test]
    fn json_64k_matches_go_size() {
        assert_eq!(json_64k().len(), 65618);
    }
}
