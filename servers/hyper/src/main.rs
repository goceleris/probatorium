// probatorium hyper adapter — wave 4a.
//
// The raw Rust baseline: hyper driven directly, with no router crate and
// no tower stack. axum and ntex both sit above hyper (axum and
// the tower ecosystem literally re-export it); this column is the floor
// their numbers are interpreted against — the cost of hyper's H1 codec
// plus a hand-rolled match on (method, path), nothing more.
//
// Same contract as the other Rust adapters (see servers/common/contract.go
// for canonical bytes):
//   GET  /            -> "Hello, World!"  text/plain
//   GET  /json        -> {"message":"Hello, World!"}  application/json
//   GET  /json-1k     -> deterministic 1026-byte JSON page
//   GET  /json-8k     -> deterministic 8286-byte JSON page
//   GET  /json-16k    -> deterministic 16463-byte JSON page
//   GET  /json-64k    -> deterministic 65618-byte JSON page
//   GET  /users/{id}  -> "User ID: <id>"  text/plain
//   POST /upload      -> read-and-discard body, reply "OK"  text/plain
//
// Driver-backed (/db, /cache, /mc, /session) and chain-* endpoints land
// in wave 6. This binary returns 404 for anything off-contract — the
// scenario applicability filter (servers/servers.go) skips Rust cells for
// those scenarios, so a 404 is never observed by loadgen.
//
// CLI:
//   -bind <host:port>  default 127.0.0.1:8080. Pass `:0` (or any
//                      `host:0`) to let the kernel allocate a port; the
//                      bound address is reported on stdout via the
//                      `ready addr=<addr>` line that the runner waits for
//                      before opening loadgen.
//   -engine <value>    default "h1". One of:
//                        h1  — plain HTTP/1.1 (http1::Builder, as before).
//                        h2c — HTTP/2 cleartext, PRIOR-KNOWLEDGE only:
//                              each accepted TCP conn is served through
//                              http2::Builder, so the client must open
//                              with the h2 preface (curl
//                              --http2-prior-knowledge). No TLS, no h1->h2
//                              upgrade. Mirrors stdhttp-h2's h2c-noupg
//                              mode. Unknown values exit non-zero.
//
// Lifecycle: SIGTERM (or SIGINT) stops accepting new connections and the
// hyper-util GracefulShutdown watcher drains in-flight connections, well
// within the runner's 5-second SIGKILL fallback in servers/start.go.

mod payload;

use std::convert::Infallible;
use std::io::Write as _;
use std::net::SocketAddr;
use std::process::ExitCode;

use bytes::Bytes;
use http_body_util::{BodyExt, Full};
use hyper::body::Incoming;
use hyper::header::{HeaderValue, CONTENT_TYPE};
use hyper::service::service_fn;
use hyper::{Method, Request, Response, StatusCode};
use hyper_util::rt::{TokioExecutor, TokioIo};
use hyper_util::server::graceful::GracefulShutdown;
use tokio::net::TcpListener;
use tokio::signal::unix::{signal, SignalKind};

// Engine names the wire protocol the listener speaks. h2c is
// prior-knowledge-only (no h1 fallback on that listener), mirroring the
// stdhttp adapter's h2c-noupg mode.
#[derive(Clone, Copy, PartialEq, Eq)]
enum Engine {
    H1,
    H2c,
}

#[tokio::main(flavor = "multi_thread")]
async fn main() -> ExitCode {
    let engine = match parse_engine_arg() {
        Ok(e) => e,
        Err(msg) => {
            eprintln!("{msg}");
            return ExitCode::FAILURE;
        }
    };
    let bind = parse_bind_arg().unwrap_or_else(|| "127.0.0.1:8080".to_string());
    let addr: SocketAddr = match bind.parse() {
        Ok(a) => a,
        Err(e) => {
            eprintln!("hyper: bad -bind {bind:?}: {e}");
            return ExitCode::FAILURE;
        }
    };

    let listener = match TcpListener::bind(addr).await {
        Ok(l) => l,
        Err(e) => {
            eprintln!("hyper: bind {addr}: {e}");
            return ExitCode::FAILURE;
        }
    };
    let local = match listener.local_addr() {
        Ok(a) => a,
        Err(e) => {
            eprintln!("hyper: local_addr: {e}");
            return ExitCode::FAILURE;
        }
    };

    // The runner's TCP probe waits for this exact line on stdout. Print
    // and flush before the accept loop so the probe never races the
    // listener.
    println!("ready addr={local}");
    let _ = std::io::stdout().flush();

    // hyper 1.x has no top-level serve() like axum: we own the accept
    // loop. Each accepted socket is served as one connection — http1 for
    // engine h1, http2 (prior-knowledge cleartext) for engine h2c.
    // GracefulShutdown tracks them so SIGTERM drains in-flight requests
    // instead of cutting them mid-response.
    let http1 = hyper::server::conn::http1::Builder::new();
    let http2 = hyper::server::conn::http2::Builder::new(TokioExecutor::new());
    let graceful = GracefulShutdown::new();
    // Pin the shutdown future so it can be polled by reference across loop
    // iterations inside select! without being moved.
    let shutdown = shutdown_signal();
    tokio::pin!(shutdown);

    loop {
        tokio::select! {
            accepted = listener.accept() => {
                let (stream, _peer) = match accepted {
                    Ok(pair) => pair,
                    // A single accept error (e.g. fd exhaustion under load)
                    // must not tear the whole server down — skip and keep
                    // serving, matching every other adapter's resilience.
                    Err(_) => continue,
                };
                let io = TokioIo::new(stream);
                // serve_connection returns different Connection types for
                // h1 vs h2; watch() each in its own arm so the graceful
                // watcher tracks both without a boxed trait object.
                match engine {
                    Engine::H1 => {
                        let conn = http1.serve_connection(io, service_fn(handle));
                        let fut = graceful.watch(conn);
                        tokio::spawn(async move {
                            // Per-connection errors (client resets, partial
                            // sends) are expected churn under load; drop them.
                            let _ = fut.await;
                        });
                    }
                    Engine::H2c => {
                        let conn = http2.serve_connection(io, service_fn(handle));
                        let fut = graceful.watch(conn);
                        tokio::spawn(async move {
                            let _ = fut.await;
                        });
                    }
                }
            }
            _ = &mut shutdown => {
                // Stop accepting; fall through to draining below.
                break;
            }
        }
    }

    // Drain in-flight connections. The runner's SIGKILL fallback fires at
    // 5s (servers/start.go), so a hard 3s cap leaves a 2s margin for the
    // OS to flush the listener and the process to exit cleanly.
    tokio::select! {
        _ = graceful.shutdown() => {}
        _ = tokio::time::sleep(std::time::Duration::from_secs(3)) => {}
    }

    ExitCode::SUCCESS
}

// parse_bind_arg walks argv looking for `-bind <value>` (Go-flag style,
// matches the convention used by every Go adapter so the runner can
// invoke this binary identically). Dep-free for parity with the other
// Rust adapters' main.rs.
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

// parse_engine_arg reads `-engine <value>` (default "h1"). Accepts "h1"
// and "h2c"; any other value is a hard error so a typo in the runner's
// invocation fails fast and visibly rather than silently serving h1.
fn parse_engine_arg() -> Result<Engine, String> {
    let mut value: Option<String> = None;
    let mut args = std::env::args().skip(1);
    while let Some(a) = args.next() {
        if a == "-engine" || a == "--engine" {
            value = args.next();
        } else if let Some(rest) = a.strip_prefix("-engine=") {
            value = Some(rest.to_string());
        } else if let Some(rest) = a.strip_prefix("--engine=") {
            value = Some(rest.to_string());
        }
    }
    match value.as_deref() {
        None | Some("h1") => Ok(Engine::H1),
        Some("h2c") => Ok(Engine::H2c),
        Some(other) => Err(format!("hyper: unknown -engine {other:?} (want h1|h2c)")),
    }
}

// shutdown_signal resolves on the first SIGTERM or SIGINT. Returning ends
// the accept loop, which then drains via GracefulShutdown.
async fn shutdown_signal() {
    let mut term = signal(SignalKind::terminate()).expect("install SIGTERM handler");
    let mut intr = signal(SignalKind::interrupt()).expect("install SIGINT handler");
    tokio::select! {
        _ = term.recv() => {}
        _ = intr.recv() => {}
    }
}

// handle is the single service: a hand-rolled router (no router crate)
// matching on (method, path). This is the whole point of the hyper
// baseline — the routing cost here is a string match, the floor the
// framework columns add their abstraction on top of.
async fn handle(req: Request<Incoming>) -> Result<Response<Full<Bytes>>, Infallible> {
    let method = req.method();
    let path = req.uri().path();

    let resp = match (method, path) {
        (&Method::GET, "/") => text_plain(Bytes::from_static(b"Hello, World!")),
        (&Method::GET, "/json") => {
            json_response(Bytes::from_static(br#"{"message":"Hello, World!"}"#))
        }
        (&Method::GET, "/json-1k") => json_response(Bytes::from_static(payload::json_1k())),
        (&Method::GET, "/json-8k") => json_response(Bytes::from_static(payload::json_8k())),
        (&Method::GET, "/json-16k") => json_response(Bytes::from_static(payload::json_16k())),
        (&Method::GET, "/json-64k") => json_response(Bytes::from_static(payload::json_64k())),
        (&Method::POST, "/upload") => {
            // Read-and-discard the request body so /upload exercises the
            // body parser without dominating the cell with allocator
            // pressure. The reply is the literal "OK" the contract demands.
            let _ = req.into_body().collect().await;
            text_plain(Bytes::from_static(b"OK"))
        }
        (&Method::GET, p) if p.starts_with("/users/") => {
            // /users/{id} — single dynamic segment, echoed back. Manual
            // suffix slice; the contract sends literal ids like /users/42
            // with no further path components.
            let id = &p["/users/".len()..];
            text_plain(Bytes::from(format!("User ID: {id}")))
        }
        _ => not_found(),
    };
    Ok(resp)
}

// ---- response helpers ----

fn text_plain(body: Bytes) -> Response<Full<Bytes>> {
    let mut resp = Response::new(Full::new(body));
    resp.headers_mut()
        .insert(CONTENT_TYPE, HeaderValue::from_static("text/plain"));
    resp
}

fn json_response(body: Bytes) -> Response<Full<Bytes>> {
    let mut resp = Response::new(Full::new(body));
    resp.headers_mut()
        .insert(CONTENT_TYPE, HeaderValue::from_static("application/json"));
    resp
}

fn not_found() -> Response<Full<Bytes>> {
    let mut resp = Response::new(Full::new(Bytes::new()));
    *resp.status_mut() = StatusCode::NOT_FOUND;
    resp
}
