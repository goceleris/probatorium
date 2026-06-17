// probatorium axum adapter — wave 4a.
//
// Serves the canonical contract endpoints declared in
// servers/common/contract.go:
//
//   GET  /            -> "Hello, World!"  text/plain
//   GET  /json        -> {"message":"Hello, World!"}  application/json
//   GET  /json-1k     -> deterministic 1026-byte JSON page
//   GET  /json-8k     -> deterministic 8286-byte JSON page
//   GET  /json-16k    -> deterministic 16463-byte JSON page
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
//   -engine <value>    default "h1". One of:
//                        h1  — plain HTTP/1.1, served strictly (hyper's
//                              http1::Builder per accepted conn — see
//                              serve_h1 for why not axum::serve).
//                        h2c — HTTP/2 cleartext, PRIOR-KNOWLEDGE only:
//                              each accepted TCP conn is driven straight
//                              through hyper's HTTP/2 server builder, so
//                              the client must open with the h2 preface
//                              (curl --http2-prior-knowledge). No TLS, no
//                              h1->h2 upgrade. Mirrors stdhttp-h2's
//                              h2c-noupg mode. Unknown values exit non-zero.
//
// Lifecycle: SIGTERM (or SIGINT) triggers graceful shutdown, which
// finishes in-flight requests within the runner's 5-second grace window,
// well below the spawn() SIGKILL fallback in servers/start.go.

mod payload;

use std::net::SocketAddr;
use std::process::ExitCode;

use axum::{
    body::Bytes,
    extract::Path,
    http::{header, HeaderValue, StatusCode},
    response::{IntoResponse, Response},
    routing::{get, post},
    Router,
};
use hyper::server::conn::{http1, http2};
use hyper_util::rt::{TokioExecutor, TokioIo};
use hyper_util::server::graceful::GracefulShutdown;
use hyper_util::service::TowerToHyperService;
use tokio::net::TcpListener;
use tokio::signal::unix::{signal, SignalKind};

// Engine names the wire protocol the listener speaks. Mirrors the
// stdhttp adapter's -engine vocabulary (minus the h1+h2 "hybrid" mode,
// which prior-knowledge h2c deliberately does not offer).
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

    let app = Router::new()
        .route("/", get(root))
        .route("/json", get(json_static))
        .route("/json-1k", get(json_1k))
        .route("/json-8k", get(json_8k))
        .route("/json-16k", get(json_16k))
        .route("/json-64k", get(json_64k))
        // axum 0.8+ uses `{id}` capture groups; older `:id` is rejected
        // at router-build time. See axum CHANGELOG 0.8 for the rationale.
        .route("/users/{id}", get(users_id))
        .route("/upload", post(upload));

    let addr: SocketAddr = match bind.parse() {
        Ok(a) => a,
        Err(e) => {
            eprintln!("axum: bad -bind {bind:?}: {e}");
            return ExitCode::FAILURE;
        }
    };
    let listener = match TcpListener::bind(addr).await {
        Ok(l) => l,
        Err(e) => {
            eprintln!("axum: bind {addr}: {e}");
            return ExitCode::FAILURE;
        }
    };
    let local = match listener.local_addr() {
        Ok(a) => a,
        Err(e) => {
            eprintln!("axum: local_addr: {e}");
            return ExitCode::FAILURE;
        }
    };

    // The runner's TCP probe waits for this exact line on stdout. Print
    // and flush before serving so the probe never races the listener.
    println!("ready addr={local}");
    use std::io::Write;
    let _ = std::io::stdout().flush();

    let result = match engine {
        Engine::H1 => serve_h1(listener, app).await,
        Engine::H2c => serve_h2c(listener, app).await,
    };
    if let Err(e) = result {
        eprintln!("axum: serve: {e}");
        return ExitCode::FAILURE;
    }
    ExitCode::SUCCESS
}

// serve_h1 serves strict HTTP/1.1. We drive hyper's http1::Builder
// directly rather than axum::serve: axum::serve uses hyper-util's
// auto::Builder, which sniffs the h2 connection preface and would serve
// h2 too whenever hyper's "http2" feature is on — and that feature IS on
// in this crate (the h2c path needs it), so Cargo feature-unification
// would otherwise make -engine h1 accept h2 as well. http1::Builder keeps
// h1 strictly h1, matching the pre-h2c behaviour and the stdhttp h1 mode.
async fn serve_h1(listener: TcpListener, app: Router) -> std::io::Result<()> {
    let svc = TowerToHyperService::new(app);
    serve_loop(listener, move |io, graceful| {
        let conn = http1::Builder::new().serve_connection(io, svc.clone());
        graceful.watch(conn)
    })
    .await
}

// serve_h2c drives every accepted TCP connection straight through hyper's
// HTTP/2 server builder — prior-knowledge h2c cleartext. There is no TLS
// and no HTTP/1.1 on this listener: a client that does not open with the
// h2 connection preface gets no h1 fallback (this is the strict
// h2c-noupg interpretation, matching stdhttp-h2). The axum Router is a
// tower Service<Request<Incoming>>; TowerToHyperService adapts it to the
// hyper Service the http2 builder expects, cloned per connection.
async fn serve_h2c(listener: TcpListener, app: Router) -> std::io::Result<()> {
    let svc = TowerToHyperService::new(app);
    let builder = http2::Builder::new(TokioExecutor::new());
    serve_loop(listener, move |io, graceful| {
        let conn = builder.serve_connection(io, svc.clone());
        graceful.watch(conn)
    })
    .await
}

// serve_loop owns the accept loop + graceful-shutdown lifecycle shared by
// both engines. `spawn_conn` builds the per-connection future (already
// wrapped by GracefulShutdown::watch) so the only difference between h1
// and h2c is which hyper builder serves the socket. The future is spawned
// detached; per-connection errors are expected churn under load.
async fn serve_loop<F, Fut>(listener: TcpListener, spawn_conn: F) -> std::io::Result<()>
where
    F: Fn(TokioIo<tokio::net::TcpStream>, &GracefulShutdown) -> Fut,
    Fut: std::future::Future + Send + 'static,
    Fut::Output: Send,
{
    let graceful = GracefulShutdown::new();
    let shutdown = shutdown_signal();
    tokio::pin!(shutdown);

    loop {
        tokio::select! {
            accepted = listener.accept() => {
                let (stream, _peer) = match accepted {
                    Ok(pair) => pair,
                    // A single accept error must not tear the server down —
                    // skip and keep serving, matching the hyper adapter.
                    Err(_) => continue,
                };
                let io = TokioIo::new(stream);
                let fut = spawn_conn(io, &graceful);
                tokio::spawn(async move {
                    // Per-connection errors (client resets, GOAWAY races,
                    // partial sends) are expected churn under load; drop them.
                    let _ = fut.await;
                });
            }
            _ = &mut shutdown => break,
        }
    }

    // Drain in-flight connections. The runner's SIGKILL fallback fires at
    // 5s (servers/start.go), so a hard 3s cap leaves margin to exit clean.
    tokio::select! {
        _ = graceful.shutdown() => {}
        _ = tokio::time::sleep(std::time::Duration::from_secs(3)) => {}
    }
    Ok(())
}

// parse_bind_arg walks argv looking for `-bind <value>` (Go-flag style,
// matches the convention used by every Go adapter so the runner can
// invoke this binary identically). We deliberately do not use `clap` —
// the dependency is overkill for two string flags.
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
        Some(other) => Err(format!(
            "axum: unknown -engine {other:?} (want h1|h2c)"
        )),
    }
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

async fn json_8k() -> Response {
    json_response(payload::json_8k())
}

async fn json_16k() -> Response {
    json_response(payload::json_16k())
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
