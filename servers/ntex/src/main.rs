// probatorium ntex adapter — wave 4a.
//
// Same contract as servers/axum and servers/actix-web, served by the
// ntex framework on its default tokio runtime. Lifecycle and CLI are
// identical so servers.StartAdapter can launch every Rust competitor
// with one invocation pattern.
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
// Lifecycle: ntex's #[ntex::main] entrypoint installs a default
// SIGTERM/SIGINT handler that triggers graceful workers shutdown. We
// override `shutdown_timeout` to 3 seconds so the runner's 5-second
// SIGKILL window in servers/start.go always wins; without that override
// the default 30-second grace would race the SIGKILL fallback.

mod payload;

use std::io::Write as _;
use std::net::TcpListener;

use ntex::time::Seconds;
use ntex::util::Bytes;
use ntex::web::{self, App, HttpResponse, HttpServer};

#[ntex::main]
async fn main() -> std::io::Result<()> {
    let bind = parse_bind_arg().unwrap_or_else(|| "127.0.0.1:8080".to_string());

    // Bind a std listener ourselves so we can resolve the actual addr
    // (handles port=0 cleanly) before handing it to ntex via .listen().
    // ntex's HttpServer doesn't expose an addrs() accessor like
    // actix-web's, so this is the canonical pattern.
    let listener = TcpListener::bind(&bind)
        .unwrap_or_else(|e| panic!("ntex: bind {bind:?}: {e}"));
    let bound = listener
        .local_addr()
        .unwrap_or_else(|e| panic!("ntex: local_addr: {e}"));

    println!("ready addr={bound}");
    let _ = std::io::stdout().flush();

    HttpServer::new(async || {
        App::new()
            // ntex's web::Bytes extractor caps the buffered body at
            // 256 KiB by default (ntex::web::Bytes::LIMIT = 262144),
            // the same default as actix-web. The contract's POST
            // /upload drains the body — including the 1 MiB post-1m
            // scenario — so the server must accept up to ~2 MiB.
            // web::PayloadConfig::new raises the per-payload limit;
            // without this, post-1m returns 400 Payload Overflow and
            // the cell is classified not_applicable (zero-request
            // cell: all bodies rejected, all responses non-2xx).
            .state(web::PayloadConfig::new(2 * 1024 * 1024))
            .route("/", web::get().to(root))
            .route("/json", web::get().to(json_static))
            .route("/json-1k", web::get().to(json_1k))
            .route("/json-64k", web::get().to(json_64k))
            .route("/users/{id}", web::get().to(users_id))
            .route("/upload", web::post().to(upload))
    })
    .listen(listener)?
    // Shutdown grace tighter than the runner's 5s SIGKILL fallback.
    .shutdown_timeout(Seconds(3))
    .run()
    .await
}

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

async fn root() -> HttpResponse {
    HttpResponse::Ok()
        .content_type("text/plain")
        .body("Hello, World!")
}

async fn json_static() -> HttpResponse {
    HttpResponse::Ok()
        .content_type("application/json")
        .body(&b"{\"message\":\"Hello, World!\"}"[..])
}

async fn json_1k() -> HttpResponse {
    HttpResponse::Ok()
        .content_type("application/json")
        .body(payload::json_1k())
}

async fn json_64k() -> HttpResponse {
    HttpResponse::Ok()
        .content_type("application/json")
        .body(payload::json_64k())
}

async fn users_id(path: web::types::Path<String>) -> HttpResponse {
    let id = path.into_inner();
    HttpResponse::Ok()
        .content_type("text/plain")
        .body(format!("User ID: {id}"))
}

async fn upload(_body: Bytes) -> HttpResponse {
    HttpResponse::Ok()
        .content_type("text/plain")
        .body("OK")
}
