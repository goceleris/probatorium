// probatorium axum adapter — wave 4a.
//
// Serves the canonical contract endpoints declared in
// servers/common/contract.go:
//
//   GET  /            -> "Hello, World!"  text/plain
//   GET  /json        -> {"message":"Hello, World!"}  application/json
//   GET  /json-1k     -> deterministic 1026-byte JSON page
//   GET  /json-64k    -> deterministic 65618-byte JSON page
//   GET  /users/:id   -> "User ID: <id>"  text/plain
//   POST /upload      -> read-and-discard body, reply "OK"  text/plain
//
// Driver-backed (/db, /cache, /mc, /session) and chain-* endpoints land
// in wave 6. This binary intentionally returns 404 for them — the
// scenario applicability filter (servers/servers.go) skips Rust cells
// for those scenarios, so 404 is never observed by loadgen.
//
// CLI:
//   -bind <host:port>  default 127.0.0.1:8080. Pass `:0` (or any
//                      `host:0`) to let the kernel allocate a port; the
//                      bound address is reported on stdout via the
//                      `ready addr=<addr>` line that the runner waits
//                      for before opening loadgen.
//
// Lifecycle: SIGTERM (or SIGINT) triggers axum's graceful shutdown,
// which finishes in-flight requests within the runner's 5-second grace
// window, well below the spawn() SIGKILL fallback in
// servers/start.go.

mod payload;

use std::net::SocketAddr;

use axum::{
    body::Bytes,
    extract::Path,
    http::{header, HeaderValue, StatusCode},
    response::{IntoResponse, Response},
    routing::{get, post},
    Router,
};
use tokio::net::TcpListener;
use tokio::signal::unix::{signal, SignalKind};

#[tokio::main(flavor = "multi_thread")]
async fn main() -> std::io::Result<()> {
    let bind = parse_bind_arg().unwrap_or_else(|| "127.0.0.1:8080".to_string());

    let app = Router::new()
        .route("/", get(root))
        .route("/json", get(json_static))
        .route("/json-1k", get(json_1k))
        .route("/json-64k", get(json_64k))
        // axum 0.8+ uses `{id}` capture groups; older `:id` is rejected
        // at router-build time. See axum CHANGELOG 0.8 for the rationale.
        .route("/users/{id}", get(users_id))
        .route("/upload", post(upload));

    let addr: SocketAddr = bind
        .parse()
        .unwrap_or_else(|e| panic!("axum: bad -bind {bind:?}: {e}"));
    let listener = TcpListener::bind(addr).await?;
    let local = listener.local_addr()?;

    // The runner's TCP probe waits for this exact line on stdout. Print
    // and flush before serve() so the probe never races the listener.
    println!("ready addr={local}");
    use std::io::Write;
    let _ = std::io::stdout().flush();

    axum::serve(listener, app)
        .with_graceful_shutdown(shutdown_signal())
        .await
}

// parse_bind_arg walks argv looking for `-bind <value>` (Go-flag style,
// matches the convention used by every Go adapter so the runner can
// invoke this binary identically). We deliberately do not use `clap` —
// the dependency is overkill for a single string flag.
fn parse_bind_arg() -> Option<String> {
    let mut args = std::env::args().skip(1);
    while let Some(a) = args.next() {
        if a == "-bind" || a == "--bind" {
            return args.next();
        }
        if let Some(rest) = a.strip_prefix("-bind=") {
            return Some(rest.to_string());
        }
        if let Some(rest) = a.strip_prefix("--bind=") {
            return Some(rest.to_string());
        }
    }
    None
}

// shutdown_signal awaits SIGTERM or SIGINT. Returning from this future
// triggers axum's graceful shutdown.
async fn shutdown_signal() {
    let mut term = signal(SignalKind::terminate()).expect("install SIGTERM handler");
    let mut intr = signal(SignalKind::interrupt()).expect("install SIGINT handler");
    tokio::select! {
        _ = term.recv() => {}
        _ = intr.recv() => {}
    }
}

// ---- handlers ----

async fn root() -> Response {
    text_plain("Hello, World!")
}

async fn json_static() -> Response {
    json_response(br#"{"message":"Hello, World!"}"#)
}

async fn json_1k() -> Response {
    json_response(payload::json_1k())
}

async fn json_64k() -> Response {
    json_response(payload::json_64k())
}

async fn users_id(Path(id): Path<String>) -> Response {
    let body = format!("User ID: {id}");
    text_plain_owned(body)
}

// upload reads-and-discards the request body so /upload exercises the
// body parser without dominating the cell with allocator pressure. The
// reply is the literal "OK" the contract demands.
async fn upload(_body: Bytes) -> Response {
    text_plain("OK")
}

// ---- response helpers ----

fn text_plain(s: &'static str) -> Response {
    let mut resp = (StatusCode::OK, s).into_response();
    resp.headers_mut().insert(
        header::CONTENT_TYPE,
        HeaderValue::from_static("text/plain"),
    );
    resp
}

fn text_plain_owned(s: String) -> Response {
    let mut resp = (StatusCode::OK, s).into_response();
    resp.headers_mut().insert(
        header::CONTENT_TYPE,
        HeaderValue::from_static("text/plain"),
    );
    resp
}

fn json_response(body: &'static [u8]) -> Response {
    let mut resp = (StatusCode::OK, body).into_response();
    resp.headers_mut().insert(
        header::CONTENT_TYPE,
        HeaderValue::from_static("application/json"),
    );
    resp
}
