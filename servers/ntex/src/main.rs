// probatorium ntex adapter — wave 4a.
//
// Same contract as servers/axum, served by the
// ntex framework on its default tokio runtime. Lifecycle and CLI are
// identical so servers.StartAdapter can launch every Rust competitor
// with one invocation pattern.
//
// Endpoint set (see servers/common/contract.go for canonical bytes):
//   GET  /            -> "Hello, World!"  text/plain
//   GET  /json        -> {"message":"Hello, World!"}  application/json
//   GET  /json-1k     -> deterministic 1026-byte JSON page
//   GET  /json-8k     -> deterministic 8286-byte JSON page
//   GET  /json-16k    -> deterministic 16463-byte JSON page
//   GET  /json-64k    -> deterministic 65618-byte JSON page
//   GET  /users/{id}  -> "User ID: <id>"  text/plain
//   POST /upload      -> read-and-discard body, reply "OK"  text/plain
//
// Driver-backed (/db, /cache, /mc, /session) and chain-* endpoints
// land in wave 6.
//
// CLI:
//   -bind <host:port>  default 127.0.0.1:8080. The bound addr is reported
//                      on stdout via `ready addr=<addr>`.
//   -engine <value>    default "h1". One of:
//                        h1  — plain HTTP/1.1 (web::HttpServer, as before).
//                        h2c — HTTP/2 cleartext, PRIOR-KNOWLEDGE only.
//                              ntex DOES support this without TLS: we drop
//                              to the low-level ntex::server builder and
//                              serve each accepted Io with
//                              ntex::http::HttpService::h2(app), which runs
//                              the H2 dispatcher directly on the plaintext
//                              socket (no TLS filter, no h1->h2 upgrade).
//                              Mirrors stdhttp-h2's h2c-noupg mode. Unknown
//                              values exit non-zero.
//
// Lifecycle: ntex's #[ntex::main] entrypoint installs a default
// SIGTERM/SIGINT handler that triggers graceful workers shutdown. We
// override `shutdown_timeout` to 3 seconds so the runner's 5-second
// SIGKILL window in servers/start.go always wins; without that override
// the default 30-second grace would race the SIGKILL fallback. Both the
// h1 and h2c paths share this lifecycle — they differ only in the
// per-connection HTTP service factory.

mod payload;

use std::io::Write as _;
use std::net::TcpListener;
use std::process::ExitCode;

use ntex::http::HttpService;
use ntex::time::Seconds;
use ntex::util::Bytes;
use ntex::web::{self, App, HttpResponse, HttpServer};

// Engine names the wire protocol the listener speaks. h2c is
// prior-knowledge-only (no h1 fallback on that listener), mirroring the
// stdhttp adapter's h2c-noupg mode.
#[derive(Clone, Copy, PartialEq, Eq)]
enum Engine {
    H1,
    H2c,
}

// make_app expands to the `App::new()...` builder expression. Both the h1
// and h2c serve paths need an identically-configured App, but the App
// builder's concrete type is unnameable (a deep chain of generic route
// wrappers), so a macro shares the expression without naming the type. It
// returns a value implementing IntoServiceFactory<_, Request, SharedCfg>,
// which is what both HttpServer::new and HttpService::h2 accept.
macro_rules! make_app {
    () => {
        App::new()
            // ntex's web::Bytes extractor caps the buffered body at
            // 256 KiB by default (ntex::web::Bytes::LIMIT = 262144).
            // The contract's POST /upload drains the body — including
            // the 1 MiB post-1m scenario — so the server must accept
            // up to ~2 MiB. web::types::PayloadConfig::new raises the
            // per-payload limit; without this, post-1m returns 400
            // Payload Overflow and the cell is classified not_applicable
            // (zero-request cell: all bodies rejected, all responses
            // non-2xx). PayloadConfig lives in ntex::web::types (not
            // ntex::web::PayloadConfig — that re-export doesn't exist
            // on ntex).
            .state(web::types::PayloadConfig::new(2 * 1024 * 1024))
            .route("/", web::get().to(root))
            .route("/json", web::get().to(json_static))
            .route("/json-1k", web::get().to(json_1k))
            .route("/json-8k", web::get().to(json_8k))
            .route("/json-16k", web::get().to(json_16k))
            .route("/json-64k", web::get().to(json_64k))
            .route("/users/{id}", web::get().to(users_id))
            .route("/upload", web::post().to(upload))
    };
}

#[ntex::main]
async fn main() -> ExitCode {
    let engine = match parse_engine_arg() {
        Ok(e) => e,
        Err(msg) => {
            eprintln!("{msg}");
            return ExitCode::FAILURE;
        }
    };
    let bind = parse_bind_arg().unwrap_or_else(|| "127.0.0.1:8080".to_string());

    // Bind a std listener ourselves so we can resolve the actual addr
    // (handles port=0 cleanly) before handing it to ntex. ntex's
    // HttpServer doesn't expose an addrs() accessor, so this is the
    // canonical pattern for both serve paths.
    let listener = match TcpListener::bind(&bind) {
        Ok(l) => l,
        Err(e) => {
            eprintln!("ntex: bind {bind:?}: {e}");
            return ExitCode::FAILURE;
        }
    };
    let bound = match listener.local_addr() {
        Ok(a) => a,
        Err(e) => {
            eprintln!("ntex: local_addr: {e}");
            return ExitCode::FAILURE;
        }
    };

    println!("ready addr={bound}");
    let _ = std::io::stdout().flush();

    let result = match engine {
        Engine::H1 => serve_h1(listener).await,
        Engine::H2c => serve_h2c(listener).await,
    };
    if let Err(e) = result {
        eprintln!("ntex: serve: {e}");
        return ExitCode::FAILURE;
    }
    ExitCode::SUCCESS
}

// serve_h1 keeps the original web::HttpServer path: HTTP/1.1 (ntex's
// HttpService::new default serves h1 on a cleartext socket).
async fn serve_h1(listener: TcpListener) -> std::io::Result<()> {
    HttpServer::new(async || make_app!())
        .listen(listener)?
        // Shutdown grace tighter than the runner's 5s SIGKILL fallback.
        .shutdown_timeout(Seconds(3))
        .run()
        .await
}

// serve_h2c serves HTTP/2 prior-knowledge cleartext. web::HttpServer has
// no protocol selector, so we drop to the low-level ntex::server builder
// and supply our own per-connection service factory:
// HttpService::h2(app) runs ntex's H2 dispatcher directly on the
// plaintext Io (the base filter is the raw socket — no TLS, no h1->h2
// upgrade). The server builder's lifecycle is identical to
// web::HttpServer's (web::HttpServer::run just delegates to it), so the
// default SIGTERM graceful shutdown and the 3s drain cap carry over.
async fn serve_h2c(listener: TcpListener) -> std::io::Result<()> {
    ntex::server::build()
        .listen("ntex-h2c", listener, async move |_cfg| {
            HttpService::h2(make_app!())
        })?
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
        Some(other) => Err(format!("ntex: unknown -engine {other:?} (want h1|h2c)")),
    }
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

async fn json_8k() -> HttpResponse {
    HttpResponse::Ok()
        .content_type("application/json")
        .body(payload::json_8k())
}

async fn json_16k() -> HttpResponse {
    HttpResponse::Ok()
        .content_type("application/json")
        .body(payload::json_16k())
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
