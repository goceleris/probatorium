// probatorium actix-web adapter — wave 4a (rust-actix).
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
// Driver-backed (/db, /cache, /mc, /session) and chain-* endpoints are
// OUT OF SCOPE: the scenario applicability filter (servers/servers.go)
// skips Rust cells for those scenarios via the Static-only capability
// manifest, so the unhandled paths are never observed by loadgen.
//
// CLI:
//   -bind <host:port>  default 127.0.0.1:8080. Pass `:0` (or any
//                      `host:0`) to let the kernel allocate a port; the
//                      bound address is reported on stdout via the
//                      `ready addr=<addr>` line the runner waits for
//                      before opening loadgen. We bind a std TcpListener
//                      ourselves and hand it to actix via `.listen()` /
//                      `.listen_auto_h2c()` so `local_addr()` resolves
//                      the kernel-assigned port for the ready line.
//   -engine <value>    default "h1". One of:
//                        h1  — plain HTTP/1.x (HttpServer::listen).
//                        h2c — HTTP/2 cleartext. actix-web only exposes
//                              AUTO h2c through HttpServer
//                              (listen_auto_h2c): one listener that
//                              serves H1 AND prior-knowledge h2c, sniffing
//                              the h2 preface per connection. There is no
//                              HttpServer API for strict h2c-only
//                              (preface-or-reject); the strict path lives
//                              in the lower-level actix-http service
//                              builder, which would mean abandoning the
//                              HttpServer ergonomics this adapter is built
//                              on. So -engine h2c here is h2c-WITH-h1
//                              fallback, not h2c-noupg. Unknown values
//                              exit non-zero.
//
// Lifecycle: actix-web's built-in signal handling performs the graceful
// shutdown on SIGTERM / SIGINT — it stops accepting, drains in-flight
// requests, then exits within shutdown_timeout (set to 3s, well below the
// runner's 5-second SIGKILL fallback in servers/start.go). We deliberately
// do NOT call disable_signals(): the custom-handler + ServerHandle::stop()
// path has a documented hang (actix-net#419), and the built-in handler
// already does exactly what the contract requires.

mod payload;

use std::net::TcpListener;
use std::process::ExitCode;

use actix_web::http::header::ContentType;
use actix_web::{web, App, HttpResponse, HttpServer};

// Engine names the wire protocol the listener speaks. Mirrors the other
// Rust adapters' -engine vocabulary. NOTE: H2c here is actix's AUTO h2c
// (h1 + prior-knowledge h2c on one socket), not the strict h2c-noupg the
// hyper/ntex/axum adapters offer — see the -engine doc block above.
#[derive(Clone, Copy, PartialEq, Eq)]
enum Engine {
    H1,
    H2c,
}

// Static bodies are computed once at startup and shared by every worker.
// actix clones the App factory per worker thread; capturing &'static
// slices (payload::*) and a &'static str keeps the handlers allocation-
// free on the hot path.
const HELLO: &str = "Hello, World!";
const JSON_HELLO: &[u8] = br#"{"message":"Hello, World!"}"#;

#[actix_web::main]
async fn main() -> ExitCode {
    let engine = match parse_engine_arg() {
        Ok(e) => e,
        Err(msg) => {
            eprintln!("{msg}");
            return ExitCode::FAILURE;
        }
    };
    let bind = parse_bind_arg().unwrap_or_else(|| "127.0.0.1:8080".to_string());

    // Bind a std listener ourselves so local_addr() resolves the kernel-
    // assigned port for the ready line when -bind ends in :0. set_nonblocking
    // is required: actix-server drives the listener inside its async accept
    // loop and a blocking socket would stall the worker.
    let listener = match TcpListener::bind(&bind) {
        Ok(l) => l,
        Err(e) => {
            eprintln!("actix: bind {bind:?}: {e}");
            return ExitCode::FAILURE;
        }
    };
    if let Err(e) = listener.set_nonblocking(true) {
        eprintln!("actix: set_nonblocking: {e}");
        return ExitCode::FAILURE;
    }
    let local = match listener.local_addr() {
        Ok(a) => a,
        Err(e) => {
            eprintln!("actix: local_addr: {e}");
            return ExitCode::FAILURE;
        }
    };

    let factory = || {
        App::new()
            // actix's web::Bytes extractor caps the buffered body at 256 KiB
            // by default (web::PayloadConfig default limit). The contract's
            // POST /upload drains the body — including the 1 MiB post-1m
            // scenario (scenarios/static.go: post1MBody = 1024*1024) — so the
            // server must accept up to ~2 MiB. Without raising this limit
            // post-1m returns 400 Payload Overflow and the cell is classified
            // not_applicable (the exact "post-1m body limit" capability gap
            // mage_bench.go calls out for ntex). PayloadConfig applies to the
            // built-in Bytes/String extractors and is registered via app_data.
            .app_data(web::PayloadConfig::new(2 * 1024 * 1024))
            .route("/", web::get().to(root))
            .route("/json", web::get().to(json_static))
            .route("/json-1k", web::get().to(json_1k))
            .route("/json-8k", web::get().to(json_8k))
            .route("/json-16k", web::get().to(json_16k))
            .route("/json-64k", web::get().to(json_64k))
            // actix uses `{id}` path-capture syntax; the contract's `:id`
            // template is translated here once at registration.
            .route("/users/{id}", web::get().to(users_id))
            .route("/upload", web::post().to(upload))
    };

    // shutdown_timeout caps the graceful drain; the runner SIGKILLs at 5s
    // (servers/start.go), so 3s leaves margin to exit clean.
    let server = HttpServer::new(factory).shutdown_timeout(3);

    let server = match engine {
        Engine::H1 => server.listen(listener),
        Engine::H2c => server.listen_auto_h2c(listener),
    };
    let server = match server {
        Ok(s) => s,
        Err(e) => {
            eprintln!("actix: listen {local}: {e}");
            return ExitCode::FAILURE;
        }
    };

    // The runner's TCP probe waits for this exact line on stdout. Print and
    // flush before running so the probe never races the listener.
    println!("ready addr={local}");
    use std::io::Write;
    let _ = std::io::stdout().flush();

    // run() returns a Server future; awaiting it serves until actix's
    // built-in SIGTERM/SIGINT handler triggers graceful shutdown.
    match server.run().await {
        Ok(()) => ExitCode::SUCCESS,
        Err(e) => {
            eprintln!("actix: serve: {e}");
            ExitCode::FAILURE
        }
    }
}

// parse_bind_arg walks argv for `-bind <value>` (Go-flag style, the
// convention every adapter shares so the runner invokes them identically).
// No clap — two string flags do not justify the dependency.
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
// invocation fails fast rather than silently serving h1.
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
        Some(other) => Err(format!("actix: unknown -engine {other:?} (want h1|h2c)")),
    }
}

// ---- handlers ----

async fn root() -> HttpResponse {
    HttpResponse::Ok().content_type(ContentType::plaintext()).body(HELLO)
}

async fn json_static() -> HttpResponse {
    json_response(JSON_HELLO)
}

async fn json_1k() -> HttpResponse {
    json_response(payload::json_1k())
}

async fn json_8k() -> HttpResponse {
    json_response(payload::json_8k())
}

async fn json_16k() -> HttpResponse {
    json_response(payload::json_16k())
}

async fn json_64k() -> HttpResponse {
    json_response(payload::json_64k())
}

async fn users_id(id: web::Path<String>) -> HttpResponse {
    HttpResponse::Ok()
        .content_type(ContentType::plaintext())
        .body(format!("User ID: {id}"))
}

// upload reads-and-discards the request body via the Bytes extractor (which
// collects the full payload stream) so /upload exercises the body parser
// without dominating the cell with allocator pressure. The reply is the
// literal "OK" the contract demands.
async fn upload(_body: web::Bytes) -> HttpResponse {
    HttpResponse::Ok().content_type(ContentType::plaintext()).body("OK")
}

// ---- response helpers ----

// json_response writes a &'static [u8] verbatim with application/json. The
// body bytes are shared across every request/worker (no copy of the corpus).
fn json_response(body: &'static [u8]) -> HttpResponse {
    HttpResponse::Ok().content_type(ContentType::json()).body(body)
}
