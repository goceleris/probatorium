// probatorium actix-web adapter — wave 4a.
//
// Same contract as servers/axum, served by actix-web's actor-style HTTP
// stack. Lifecycle and CLI are identical to the axum adapter so
// servers.StartAdapter can launch every Rust competitor with one
// invocation pattern.
//
// Endpoint set (see servers/common/contract.go for canonical bytes):
//   GET  /            -> "Hello, World!"  text/plain
//   GET  /json        -> {"message":"Hello, World!"}  application/json
//   GET  /json-1k     -> deterministic 1026-byte JSON page
//   GET  /json-64k    -> deterministic 65618-byte JSON page
//   GET  /users/{id}  -> "User ID: <id>"  text/plain
//   POST /upload      -> read-and-discard body, reply "OK"  text/plain
//
// Driver-backed (/db, /cache, /mc, /session) and chain-* endpoints
// land in wave 6.
//
// Lifecycle: actix-web's HttpServer installs its own SIGTERM/SIGINT
// handler by default, so we don't add a redundant tokio::signal
// handler — running both creates a stop() race where the user's
// handle.stop() interleaves with actix-web's internal stop and
// the worker pool can stay alive past the runner's 5-second SIGKILL.

mod payload;

use std::io::Write as _;
use std::net::SocketAddr;

use actix_web::{
    middleware,
    web::{self, Bytes},
    App, HttpResponse, HttpServer, Responder,
};

#[actix_web::main]
async fn main() -> std::io::Result<()> {
    let bind = parse_bind_arg().unwrap_or_else(|| "127.0.0.1:8080".to_string());
    let addr: SocketAddr = bind
        .parse()
        .unwrap_or_else(|e| panic!("actix-web: bad -bind {bind:?}: {e}"));

    // bind_auto_h2c is unstable on actix-web — for wave 4a we land H1
    // only, matching the runner's H1 cell coverage. H2 lands later.
    let server = HttpServer::new(|| {
        App::new()
            .wrap(middleware::DefaultHeaders::new())
            // actix-web's web::Bytes extractor caps the buffered body at
            // 256 KiB by default (actix_web::web::Bytes::MAX = 262144).
            // The contract's POST /upload drains the body — including the
            // 1 MiB post-1m scenario — so the server must accept up to
            // ~2 MiB. PayloadConfig::new sets the limit for the streaming
            // Payload type; the Bytes extractor's own cap is raised via
            // the same JSON-config path that JsonConfig uses, which
            // also raises JsonConfig's default 32 KiB. Without this,
            // post-1m returns 413 Payload Too Large and the cell is
            // classified not_applicable (zero-request cell: all bodies
            // rejected, all responses non-2xx). 2 MiB covers the bench
            // while still capping pathological clients.
            .app_data(web::PayloadConfig::new(2 * 1024 * 1024))
            .route("/", web::get().to(root))
            .route("/json", web::get().to(json_static))
            .route("/json-1k", web::get().to(json_1k))
            .route("/json-64k", web::get().to(json_64k))
            .route("/users/{id}", web::get().to(users_id))
            .route("/upload", web::post().to(upload))
    })
    // shutdown_timeout(3) gives in-flight requests up to 3 seconds to
    // finish before workers are forced down. The runner's SIGKILL
    // fallback fires at 5s (servers/start.go), so this leaves a 2s
    // margin for the OS to flush the listener and child processes to
    // exit cleanly.
    .shutdown_timeout(3)
    .bind(addr)?;

    // Resolve the actually-bound socket addr (handles port=0 cleanly)
    // and emit the ready line the runner waits for.
    let bound = server
        .addrs()
        .into_iter()
        .next()
        .unwrap_or(addr);
    println!("ready addr={bound}");
    let _ = std::io::stdout().flush();

    server.run().await
}

// parse_bind_arg — minimal Go-flag-style parser, kept dep-free for
// parity with servers/axum/src/main.rs.
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

// ---- handlers ----

async fn root() -> impl Responder {
    HttpResponse::Ok()
        .content_type("text/plain")
        .body("Hello, World!")
}

async fn json_static() -> impl Responder {
    HttpResponse::Ok()
        .content_type("application/json")
        .body(&b"{\"message\":\"Hello, World!\"}"[..])
}

async fn json_1k() -> impl Responder {
    HttpResponse::Ok()
        .content_type("application/json")
        .body(payload::json_1k())
}

async fn json_64k() -> impl Responder {
    HttpResponse::Ok()
        .content_type("application/json")
        .body(payload::json_64k())
}

async fn users_id(path: web::Path<String>) -> impl Responder {
    let id = path.into_inner();
    HttpResponse::Ok()
        .content_type("text/plain")
        .body(format!("User ID: {id}"))
}

// upload reads-and-discards the body. actix-web hands us the buffered
// bytes via Bytes; we drop them immediately rather than streaming
// because the contract reply is a fixed-size "OK".
async fn upload(_body: Bytes) -> impl Responder {
    HttpResponse::Ok()
        .content_type("text/plain")
        .body("OK")
}
