// Probatorium uWebSockets.js adapter (Node.js, the JS speed leader).
//
// uWebSockets.js (uNetworking/uWebSockets.js) is a thin Node N-API
// binding over the µWS C++ HTTP server — the fastest HTTP path reachable
// from JavaScript. The npm package ships a prebuilt `.node` addon per
// (platform, arch, Node-ABI); there is NO native compile step (no
// node-gyp), uws.js just `require`s the matching prebuilt. Supported on
// glibc Linux / macOS / Windows for Node 22, 24, 26.
//
// Serves the canonical contract endpoints declared in
// servers/common/contract.go:
//
//   GET  /            -> "Hello, World!"            text/plain
//   GET  /json        -> {"message":"Hello, World!"} application/json
//   GET  /json-1k     -> deterministic 1026-byte JSON page
//   GET  /json-8k     -> deterministic 8286-byte JSON page
//   GET  /json-16k    -> deterministic 16463-byte JSON page
//   GET  /json-64k    -> deterministic 65618-byte JSON page
//   GET  /users/:id   -> "User ID: <id>"            text/plain
//   POST /upload      -> read-and-discard body, reply "OK"  text/plain
//
// Driver-backed (/db, /cache, /mc, /session) and chain-* endpoints are
// OUT OF SCOPE for this adapter — Capabilities declares Static only, so
// the scheduler never sends those scenarios here.
//
// CLI contract (matched by every probatorium adapter):
//   -bind <host:port>  default 0.0.0.0:8080. Pass `:0` (or `host:0`)
//                      to let the kernel assign a port; the resolved
//                      address is echoed via the "ready addr=..." line.
//   -engine <value>    "h1" (or absent) → HTTP/1.1. uWS's App speaks
//                      HTTP/1.1 only (no cleartext-h2c prior-knowledge in
//                      its public JS API), so h1 is the only supported
//                      engine. Any other value exits non-zero.
//   stdout             "ready addr=<final-bind-addr>" once listening
//                      (emitted ONCE by the primary, never per worker).
//   SIGTERM/SIGINT     graceful shutdown (close the listen socket).
//
// Multi-core: uWS's App is single-threaded (one Node event loop). The
// documented Linux fast path for multi-core is to listen to the SAME
// port from N processes — uWS's listen() sets SO_REUSEPORT so the kernel
// load-balances accepts across them (mirrors the fastapi tier's
// `uvicorn --workers $(nproc)` and the hono tier's `reusePort: true`).
// We fork one worker per logical CPU via node:cluster; each worker builds
// its own App and listens on the fixed port. The primary forks, waits for
// the first worker to report "listening", then prints the single ready
// line. Worker count is overridable via UWS_WORKERS (0/1 ⇒ single
// process; useful for local dev and the port-0 path below).
//
// Port 0 (kernel-assigned, used only for local testing — the bench always
// passes a fixed port) forces SINGLE-PROCESS: SO_REUSEPORT with port 0
// would hand each worker a DIFFERENT ephemeral port, so one ready line
// could not describe all workers. The fixed-port bench path is unaffected.
//
// uWS handler discipline encoded below:
//   * Synchronous handlers (every GET here) build the whole response in
//     one res.cork(...) so the status line, headers and body coalesce
//     into a single write — µWS's documented fast path.
//   * The async handler (/upload, which awaits the request body) MUST
//     register res.onAborted BEFORE the first await: if the client
//     disconnects mid-body, µWS invalidates `res` and any later use is
//     undefined behaviour. The aborted flag guards the late end().
//   * res.getParameter(0) reads the :id capture for /users/:id.

import cluster from "node:cluster";
import { availableParallelism } from "node:os";

import {
  App,
  us_socket_local_port,
  us_listen_socket_close,
} from "uWebSockets.js";

import {
  json1KPayload,
  json8KPayload,
  json16KPayload,
  json64KPayload,
} from "./payload.js";

// Pre-encoded static bodies. µWS's write/end accept a "RecognizedString"
// (string | ArrayBuffer | TypedArray); Buffers qualify and avoid a
// per-request UTF-8 encode. Built once at startup.
const HELLO = Buffer.from("Hello, World!", "utf8");
const JSON_HELLO = Buffer.from('{"message":"Hello, World!"}', "utf8");
const OK = Buffer.from("OK", "utf8");
const JSON_1K = json1KPayload();
const JSON_8K = json8KPayload();
const JSON_16K = json16KPayload();
const JSON_64K = json64KPayload();

const TEXT = "text/plain";
const JSON_CT = "application/json";

// writeStatic emits a 200 with the given content-type + body in one cork
// so µWS batches the whole response into a single send. µWS sets
// Content-Length automatically from end()'s body, so we do not write it
// by hand (writing it ourselves would double the header).
function writeStatic(res, contentType, body) {
  res.cork(() => {
    res.writeHeader("Content-Type", contentType).end(body);
  });
}

// buildApp wires the canonical contract routes onto a fresh µWS App.
// Called once per process (the primary never builds one; each worker —
// or the single process in the port-0 / UWS_WORKERS<=1 path — does).
function buildApp() {
  const app = App();

  app.get("/", (res) => {
    writeStatic(res, TEXT, HELLO);
  });

  app.get("/json", (res) => {
    writeStatic(res, JSON_CT, JSON_HELLO);
  });

  app.get("/json-1k", (res) => {
    writeStatic(res, JSON_CT, JSON_1K);
  });

  app.get("/json-8k", (res) => {
    writeStatic(res, JSON_CT, JSON_8K);
  });

  app.get("/json-16k", (res) => {
    writeStatic(res, JSON_CT, JSON_16K);
  });

  app.get("/json-64k", (res) => {
    writeStatic(res, JSON_CT, JSON_64K);
  });

  // /users/:id — the one parametrised route. getParameter(0) returns the
  // first path capture (the :id segment). Echo it verbatim, matching
  // WritePath in servers/common/common.go ("User ID: <id>"). This handler
  // reads req synchronously (req is invalid after the handler returns), so
  // the body is built inside the same tick — no onAborted needed.
  app.get("/users/:id", (res, req) => {
    const id = req.getParameter(0);
    writeStatic(res, TEXT, "User ID: " + id);
  });

  // POST /upload — drain-and-discard the request body, reply "OK". This is
  // the only async path: µWS streams the body via onData and may invalidate
  // `res` if the client aborts, so onAborted is registered FIRST and the
  // flag is checked before the terminal end().
  app.post("/upload", (res) => {
    let aborted = false;
    res.onAborted(() => {
      aborted = true;
    });
    // We discard every chunk; isLast signals the final piece. The chunk
    // ArrayBuffer is neutered on return from onData, but since we keep
    // nothing there is no copy to make.
    res.onData((_chunk, isLast) => {
      if (isLast && !aborted) {
        res.cork(() => {
          res.writeHeader("Content-Type", TEXT).end(OK);
        });
      }
    });
  });

  // Catch-all for unmatched (method, path) pairs. µWS has no automatic
  // 404/405; any('/*', ...) handles every method on every otherwise
  // unrouted path. The contract never exercises this (loadgen only dials
  // declared routes), but a clean 404 beats a hung socket if it does.
  app.any("/*", (res) => {
    res.cork(() => {
      res.writeStatus("404 Not Found").end("Not Found");
    });
  });

  return app;
}

const { host, port } = parseBind(process.argv);
const engine = parseEngine(process.argv);
if (engine !== "h1") {
  // uWS's App is HTTP/1.1-only; h2c is not supported. Fail loudly rather
  // than silently serving h1 under an h2c column.
  process.stderr.write(
    `uws: unsupported -engine ${JSON.stringify(engine)} (supported: h1)\n`,
  );
  process.exit(2);
}

// workerCount decides how many processes listen on the port. Default is
// one per logical CPU (uWS's documented Linux SO_REUSEPORT multi-core
// path). UWS_WORKERS overrides it. Port 0 pins to a single process so the
// one ready line describes the one (kernel-assigned) port — see the
// header note.
function workerCount() {
  const env = process.env.UWS_WORKERS;
  if (env !== undefined && env !== "") {
    const n = Number(env);
    if (Number.isInteger(n) && n > 0) return n;
  }
  return Math.max(1, availableParallelism());
}

const workers = port === 0 ? 1 : workerCount();

if (workers > 1 && cluster.isPrimary) {
  runPrimary(workers);
} else {
  runWorker();
}

// runPrimary forks `n` workers and emits the SINGLE ready line once the
// first worker reports it is listening (every worker binds the same fixed
// port via SO_REUSEPORT, so one line describes them all). It owns no
// listen socket itself.
function runPrimary(n) {
  let announced = false;
  let terminating = false;
  for (let i = 0; i < n; i++) cluster.fork();

  cluster.on("message", (_worker, msg) => {
    if (msg && msg.type === "listening" && !announced) {
      announced = true;
      // The runner's TCP probe waits for this exact line on stdout.
      process.stdout.write(`ready addr=${host}:${port}\n`);
    }
  });

  // If a worker dies before any has announced, the bind failed for all —
  // surface it and exit non-zero so the cell fails fast instead of hanging
  // on a ready line that never comes. Ignored once we are intentionally
  // tearing down (workers exit by our own SIGTERM then).
  cluster.on("exit", (_worker, code) => {
    if (!terminating && !announced && code !== 0) {
      process.stderr.write(`uws: worker exited (code ${code}) before listen\n`);
      process.exit(1);
    }
  });

  // Forward termination to the workers, then exit once they are gone.
  function shutdownPrimary(signal) {
    if (terminating) return;
    terminating = true;
    process.stderr.write(`uws: received ${signal}, shutting down\n`);
    for (const id in cluster.workers) cluster.workers[id]?.kill("SIGTERM");
    // Backstop: exit even if a worker is wedged. The runner's SIGKILL is
    // the outer bound; this keeps us well inside the grace window.
    setTimeout(() => process.exit(0), 200);
  }
  process.on("SIGTERM", () => shutdownPrimary("SIGTERM"));
  process.on("SIGINT", () => shutdownPrimary("SIGINT"));
}

// runWorker builds the App, listens, and (single-process path) prints the
// ready line directly; under cluster it instead messages the primary so
// the announcement fires exactly once.
function runWorker() {
  const app = buildApp();
  let listenSocket = null;

  // listen(host, port, cb): the callback's token is falsy on bind failure.
  // On success, us_socket_local_port resolves the kernel-assigned port
  // (when the caller passed 0), which we report in the ready line.
  app.listen(host, port, (token) => {
    if (!token) {
      process.stderr.write(`uws: bind ${host}:${port} failed\n`);
      process.exit(1);
      return;
    }
    listenSocket = token;
    if (cluster.isPrimary) {
      // Single-process path (workers<=1 or port 0): print directly.
      const boundPort = us_socket_local_port(token);
      process.stdout.write(`ready addr=${host}:${boundPort}\n`);
    } else {
      // Clustered worker: the primary owns the single ready line. We bind a
      // fixed port (SO_REUSEPORT), so the primary already knows it.
      process.send?.({ type: "listening" });
    }
  });

  // Graceful shutdown: closing the listen socket stops accepting new
  // connections and lets µWS's loop drain. The runner's SIGKILL backstop is
  // the upper bound, so we exit promptly after closing.
  function shutdownWorker(signal) {
    process.stderr.write(`uws: received ${signal}, shutting down\n`);
    if (listenSocket) {
      us_listen_socket_close(listenSocket);
      listenSocket = null;
    }
    // Give µWS's loop a tick to flush in-flight writes, then exit.
    setTimeout(() => process.exit(0), 50);
  }
  process.on("SIGTERM", () => shutdownWorker("SIGTERM"));
  process.on("SIGINT", () => shutdownWorker("SIGINT"));
}

// parseBind walks argv for the canonical -bind flag. Falls back to the
// BIND env var, then 0.0.0.0:8080. Accepts both `-bind 0.0.0.0:0`
// and `-bind=0.0.0.0:0`. Returns {host, port} for app.listen.
function parseBind(argv) {
  let raw = process.env.BIND ?? "0.0.0.0:8080";
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a === undefined) continue;
    if (a === "-bind" || a === "--bind") {
      const v = argv[i + 1];
      if (v !== undefined) raw = v;
      break;
    }
    if (a.startsWith("-bind=") || a.startsWith("--bind=")) {
      raw = a.slice(a.indexOf("=") + 1);
      break;
    }
  }
  const idx = raw.lastIndexOf(":");
  if (idx < 0) {
    throw new Error(`uws: invalid -bind value ${JSON.stringify(raw)}`);
  }
  const host = raw.slice(0, idx);
  const port = Number(raw.slice(idx + 1));
  if (!Number.isInteger(port) || port < 0 || port > 65535) {
    throw new Error(`uws: invalid port in -bind ${JSON.stringify(raw)}`);
  }
  return { host, port };
}

// parseEngine walks argv for -engine (accepts `-engine h1` and
// `-engine=h1`). Absent/empty defaults to "h1". Validation of the value
// against what uWS can serve happens at the call site.
function parseEngine(argv) {
  let raw = "";
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a === undefined) continue;
    if (a === "-engine" || a === "--engine") {
      raw = argv[i + 1] ?? "";
      break;
    }
    if (a.startsWith("-engine=") || a.startsWith("--engine=")) {
      raw = a.slice(a.indexOf("=") + 1);
      break;
    }
  }
  return raw === "" ? "h1" : raw;
}
