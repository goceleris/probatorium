// Probatorium express adapter — the Node.js baseline.
//
// Express 5 on the stock Node.js `http` server. This is the Node-runtime
// counterpart to the bun adapters (hono/elysia/bunraw) and the python
// adapter (fastapi): a mainstream, batteries-included web framework on its
// language's default runtime, serving the canonical contract from
// servers/common/contract.go:
//
//   GET  /            -> "Hello, World!"               text/plain
//   GET  /json        -> {"message":"Hello, World!"}   application/json
//   GET  /json-1k     -> deterministic 1026-byte JSON page
//   GET  /json-8k     -> deterministic 8286-byte JSON page
//   GET  /json-16k    -> deterministic 16463-byte JSON page
//   GET  /json-64k    -> deterministic 65618-byte JSON page
//   GET  /users/:id   -> "User ID: <id>"               text/plain
//   POST /upload      -> read-and-discard body, reply "OK"  text/plain
//
// Driver-backed (/db, /cache, /mc, /session) and chain-* endpoints are
// out of scope for this adapter (Capabilities = Static only), so the
// scenario applicability filter never dials them here.
//
// CLI contract (matched by every probatorium adapter):
//   -bind <host:port>  default 127.0.0.1:8080. Pass `:0` (or `host:0`) to
//                      let the kernel allocate a port; the bound address
//                      is reported on stdout via the `ready addr=<addr>`
//                      line the runner's TCP probe waits for.
//   -engine <value>    default "h1". Only "h1" (plain HTTP/1.1) is
//                      supported: Express is built on Node's HTTP/1.x
//                      `http` server and has no HTTP/2 request/response
//                      shim, so there is no cheap h2c path. Any other
//                      value (including "h2c") exits non-zero rather than
//                      silently serving h1 and skewing the bench.
//
// Lifecycle: SIGTERM / SIGINT trigger a graceful shutdown — stop
// accepting, close idle keep-alive sockets, drain in-flight requests, and
// exit. The runner's 5s SIGKILL backstop is the upper bound, so a short
// hard-close timeout keeps us well inside it.

"use strict";

const express = require("express");
const {
  json1KPayload,
  json8KPayload,
  json16KPayload,
  json64KPayload,
} = require("./payload");

// Static byte payloads, hoisted to module scope so every request reuses
// the same immutable Buffer — no per-request allocation, mirroring the Go
// adapters that serve a pre-baked slice from servers/common.
const HELLO = Buffer.from("Hello, World!", "utf-8");
const JSON_HELLO = Buffer.from('{"message":"Hello, World!"}', "utf-8");
const OK = Buffer.from("OK", "utf-8");
const JSON_1K = json1KPayload();
const JSON_8K = json8KPayload();
const JSON_16K = json16KPayload();
const JSON_64K = json64KPayload();

const TEXT = "text/plain";
const JSON_CT = "application/json";

// sendBytes writes a pre-encoded Buffer verbatim with status 200 and an
// explicit Content-Type. res.end(buffer) bypasses Express's res.send body
// transforms (no charset suffix, no ETag, no JSON re-encoding), so the
// wire body is byte-identical to common.Endpoints[...].ResponseBody.
// Content-Length is set from the Buffer length so HTTP/1.1 keep-alive
// frames the response exactly.
function sendBytes(res, body, contentType) {
  res.status(200);
  res.setHeader("Content-Type", contentType);
  res.setHeader("Content-Length", body.length);
  res.end(body);
}

const app = express();

// Strip framework-added response headers that would otherwise cost work
// per request and are irrelevant to the contract. etag is disabled so
// Express does not hash every body; x-powered-by is removed for parity
// with the lean Go/bun adapters.
app.disable("etag");
app.disable("x-powered-by");

app.get("/", (_req, res) => sendBytes(res, HELLO, TEXT));
app.get("/json", (_req, res) => sendBytes(res, JSON_HELLO, JSON_CT));
app.get("/json-1k", (_req, res) => sendBytes(res, JSON_1K, JSON_CT));
app.get("/json-8k", (_req, res) => sendBytes(res, JSON_8K, JSON_CT));
app.get("/json-16k", (_req, res) => sendBytes(res, JSON_16K, JSON_CT));
app.get("/json-64k", (_req, res) => sendBytes(res, JSON_64K, JSON_CT));

// /users/:id — Express's :param syntax matches the contract template
// verbatim. Echo the captured segment, matching WritePath in
// servers/common/common.go ("User ID: <id>").
app.get("/users/:id", (req, res) => {
  sendBytes(res, Buffer.from("User ID: " + req.params.id, "utf-8"), TEXT);
});

// /upload — drain the request body so the body parser is part of the
// measured cost (every adapter does this deliberately), then reply "OK".
// No body-parser middleware is mounted: we consume the raw stream so the
// cell measures socket-read + discard, not JSON parsing.
app.post("/upload", (req, res) => {
  req.on("data", () => {});
  req.on("end", () => sendBytes(res, OK, TEXT));
  req.on("error", () => sendBytes(res, OK, TEXT));
});

const { host, port } = parseBind(process.argv);
const engine = parseEngine(process.argv);

if (engine !== "h1") {
  // Express has no cheap HTTP/2 path (see header comment). Fail loudly so
  // a mis-routed -engine value is visible rather than silently h1.
  console.error(
    "express: unsupported -engine " +
      JSON.stringify(engine) +
      " (supported: h1)",
  );
  process.exit(2);
}

// app.listen returns the underlying http.Server. Passing port 0 lets the
// kernel assign a port; server.address().port then reports the resolved
// value for the ready line.
const server = app.listen(port, host, () => {
  const addr = server.address();
  const boundHost = typeof addr === "object" && addr ? addr.address : host;
  const boundPort = typeof addr === "object" && addr ? addr.port : port;
  // The runner's TCP probe waits for this exact line on stdout.
  console.log(`ready addr=${boundHost}:${boundPort}`);
});

server.on("error", (err) => {
  console.error(`express: listen ${host}:${port}: ${err.message}`);
  process.exit(1);
});

let shuttingDown = false;
function shutdown(signal) {
  if (shuttingDown) return;
  shuttingDown = true;
  console.log(`express: received ${signal}, shutting down`);
  // Stop accepting, then drop idle keep-alives so close() does not hang
  // on connections parked between requests. In-flight requests still get
  // to finish. closeIdleConnections is available on Node's http.Server
  // (>=18.2); closeAllConnections is the hard backstop.
  if (typeof server.closeIdleConnections === "function") {
    server.closeIdleConnections();
  }
  server.close(() => process.exit(0));
  // Hard cap well below the runner's 5s SIGKILL backstop: force-close any
  // stragglers and exit so a stuck keep-alive never strands the process.
  setTimeout(() => {
    if (typeof server.closeAllConnections === "function") {
      server.closeAllConnections();
    }
    process.exit(0);
  }, 1500).unref();
}

process.on("SIGTERM", () => shutdown("SIGTERM"));
process.on("SIGINT", () => shutdown("SIGINT"));

// parseBind walks argv for the canonical -bind flag (accepts both
// `-bind 127.0.0.1:0` and `-bind=127.0.0.1:0`), falling back to the BIND
// env var then 127.0.0.1:8080. Returns {host, port} for app.listen.
// IPv6 literals in bracketed form ([::1]:8080) are supported.
function parseBind(argv) {
  let raw = process.env.BIND || "127.0.0.1:8080";
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
  let host;
  let portStr;
  const m = /^\[(.+)\]:(\d+)$/.exec(raw);
  if (m) {
    host = m[1];
    portStr = m[2];
  } else {
    const idx = raw.lastIndexOf(":");
    if (idx < 0) {
      throw new Error("express: invalid -bind value " + JSON.stringify(raw));
    }
    host = raw.slice(0, idx);
    portStr = raw.slice(idx + 1);
  }
  const port = Number(portStr);
  if (!Number.isInteger(port) || port < 0 || port > 65535) {
    throw new Error("express: invalid port in -bind " + JSON.stringify(raw));
  }
  return { host, port };
}

// parseEngine walks argv for -engine (accepts `-engine h1` and
// `-engine=h1`), defaulting to "h1". The value is validated by the caller
// above; we only normalise it here.
function parseEngine(argv) {
  let raw = "";
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a === undefined) continue;
    if (a === "-engine" || a === "--engine") {
      raw = argv[i + 1] || "";
      break;
    }
    if (a.startsWith("-engine=") || a.startsWith("--engine=")) {
      raw = a.slice(a.indexOf("=") + 1);
      break;
    }
  }
  return raw === "" ? "h1" : raw;
}
