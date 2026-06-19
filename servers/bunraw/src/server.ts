// Probatorium bunraw adapter (wave 4b, bun runtime).
//
// The raw-Bun baseline: Bun.serve driven by a single hand-rolled
// `fetch(req)` handler with a manual (method, path) router — NO web
// framework (no Hono, no Elysia). This column is the floor the Bun
// framework adapters (hono, elysia) add their routing/abstraction cost
// on top of, the Bun analogue of the rust `hyper` baseline under axum /
// ntex.
//
// The router is deliberately minimal: an exact-match switch for the
// static endpoints plus two prefix checks for the only dynamic shapes in
// the contract (/users/:id, /upload). It does NOT use Bun.serve's
// `routes` table (Bun v1.2.3+) — that is Bun's own router and would make
// this a "Bun router" column rather than a raw fetch baseline. Hand
// matching keeps the measured cost to a string compare + a Response
// constructor and stays portable across Bun versions.
//
// CLI contract (matched by every probatorium adapter):
//   -bind <addr>   address:port to listen on (default 0.0.0.0:8080).
//                  Port 0 is supported and the resolved port is echoed
//                  back via the "ready addr=..." line.
//   -engine <eng>  "h1" (or absent) → Bun.serve HTTP/1.1 fast path.
//                  "h2c" → HTTP/2 cleartext prior-knowledge (node:http2).
//   stdout         "ready addr=<final-bind-addr>" once listening.
//   SIGTERM/SIGINT graceful shutdown within 5s (Bun.serve.stop(true)).
//
// argv parsing is hand-rolled because Bun supports `process.argv` but
// the canonical CLI form `bun run dist/server -- -bind 127.0.0.1:0`
// puts our flags after `--`; we walk the array looking for -bind /
// -engine to stay robust to either invocation shape.
//
// -engine flag: the bench passes `-engine <value>` only when the registry
// gives the adapter an Engine. The h1 column (bunraw) has none, so no
// -engine arrives — but we parse it defensively so the bunraw-h2 column
// (engine h2c-noupg, sharing this same launcher) works without a code
// change. "h1" (or absent) stays on the Bun.serve fast path. "h2c" serves
// HTTP/2 cleartext prior-knowledge via the node:http2 bridge in h2c.ts.
// Any other value exits non-zero with a clear message.

import {
  json1KPayload,
  json8KPayload,
  json16KPayload,
  json64KPayload,
} from "./payload";
import { serveH2C } from "./h2c";

const HELLO = new TextEncoder().encode("Hello, World!");
const JSON_HELLO = new TextEncoder().encode('{"message":"Hello, World!"}');
const OK = new TextEncoder().encode("OK");
const JSON_1K = json1KPayload();
const JSON_8K = json8KPayload();
const JSON_16K = json16KPayload();
const JSON_64K = json64KPayload();

const TEXT = "text/plain";
const JSON_CT = "application/json";

// Frozen header objects reused across requests. A new Response is needed
// per request (the body stream is single-shot), but the header init
// objects are immutable and shared — no per-request header allocation
// beyond what the Response constructor copies internally.
const HDR_HELLO = {
  "Content-Type": TEXT,
  "Content-Length": String(HELLO.length),
} as const;
const HDR_JSON_HELLO = {
  "Content-Type": JSON_CT,
  "Content-Length": String(JSON_HELLO.length),
} as const;
const HDR_JSON_1K = {
  "Content-Type": JSON_CT,
  "Content-Length": String(JSON_1K.length),
} as const;
const HDR_JSON_8K = {
  "Content-Type": JSON_CT,
  "Content-Length": String(JSON_8K.length),
} as const;
const HDR_JSON_16K = {
  "Content-Type": JSON_CT,
  "Content-Length": String(JSON_16K.length),
} as const;
const HDR_JSON_64K = {
  "Content-Type": JSON_CT,
  "Content-Length": String(JSON_64K.length),
} as const;
const HDR_OK = {
  "Content-Type": TEXT,
  "Content-Length": String(OK.length),
} as const;

const NOT_FOUND = new Response("Not Found", { status: 404 });
const METHOD_NOT_ALLOWED = new Response("Method Not Allowed", { status: 405 });

// handle is the raw fetch router. Routes mirror servers/common/contract.go
// exactly. Static responses are pre-encoded Uint8Array buffers so the
// per-request work is just URL.pathname extraction, a switch, and a
// Response constructor — no JSON.stringify, no framework dispatch.
async function handle(req: Request): Promise<Response> {
  // URL parsing is the documented way to read the path under Bun.serve.
  // new URL(req.url).pathname strips the query string and decodes the
  // path, matching how the Go router sees the request-target.
  const path = new URL(req.url).pathname;
  const method = req.method;

  if (method === "GET") {
    switch (path) {
      case "/":
        return new Response(HELLO, { status: 200, headers: HDR_HELLO });
      case "/json":
        return new Response(JSON_HELLO, { status: 200, headers: HDR_JSON_HELLO });
      case "/json-1k":
        return new Response(JSON_1K, { status: 200, headers: HDR_JSON_1K });
      case "/json-8k":
        return new Response(JSON_8K, { status: 200, headers: HDR_JSON_8K });
      case "/json-16k":
        return new Response(JSON_16K, { status: 200, headers: HDR_JSON_16K });
      case "/json-64k":
        return new Response(JSON_64K, { status: 200, headers: HDR_JSON_64K });
    }
    // /users/:id — the one parametrised GET route. Echo the path segment
    // verbatim, matching WritePath in servers/common/common.go
    // ("User ID: <id>").
    if (isUserPath(path)) {
      const id = path.slice("/users/".length);
      const body = "User ID: " + id;
      return new Response(body, { status: 200, headers: { "Content-Type": TEXT } });
    }
  } else if (method === "POST" && path === "/upload") {
    // Drain request body so the body parser is part of the measured cost
    // (every framework does this deliberately). Bun's Request.body is a
    // ReadableStream; consuming arrayBuffer() exhausts it.
    await req.arrayBuffer();
    return new Response(OK, { status: 200, headers: HDR_OK });
  }

  // No handler matched. Distinguish a wrong-method hit on a known path
  // (405) from a genuinely unknown path (404), method-independently, so
  // GET /upload and POST / both report 405 like the Go radix router's
  // allowedMethods 405 detection.
  if (isKnownPath(path)) return METHOD_NOT_ALLOWED;
  return NOT_FOUND;
}

// isUserPath reports whether path is a valid /users/:id capture: exactly
// one non-empty segment after /users/ (no trailing slash, no nesting),
// matching the Go radix router's :param semantics.
function isUserPath(path: string): boolean {
  if (!path.startsWith("/users/")) return false;
  const id = path.slice("/users/".length);
  return id.length > 0 && !id.includes("/");
}

// isKnownPath reports whether path is one of the contract's routes,
// regardless of method — used to choose 405 vs 404 for an unmatched
// (method, path) pair.
function isKnownPath(path: string): boolean {
  switch (path) {
    case "/":
    case "/json":
    case "/json-1k":
    case "/json-8k":
    case "/json-16k":
    case "/json-64k":
    case "/upload":
      return true;
  }
  return isUserPath(path);
}

const { host, port } = parseBind(process.argv);
const engine = parseEngine(process.argv);

if (engine === "h2c") {
  // HTTP/2 cleartext prior-knowledge via node:http2 (see h2c.ts). Bun.serve
  // has no cleartext-h2 server option as of Bun 1.3.14, so we bridge the h2
  // streams to the same `handle` fetch handler the h1 path uses.
  const h2c = await serveH2C(host, port, handle);
  console.log(`ready addr=${h2c.hostname}:${h2c.port}`);

  const shutdownH2C = (signal: string): void => {
    console.log(`bunraw: received ${signal}, shutting down`);
    h2c.stop();
    setTimeout(() => process.exit(0), 50);
  };
  process.on("SIGTERM", () => shutdownH2C("SIGTERM"));
  process.on("SIGINT", () => shutdownH2C("SIGINT"));
} else {
  const server = Bun.serve({
    hostname: host,
    port,
    // reusePort lets the kernel SO_REUSEPORT load-balance across
    // multiple Bun.serve workers if a future operator launches more
    // than one process — harmless on a single-process bench.
    reusePort: true,
    fetch: handle,
  });

  // Bun.serve.port is the resolved port (kernel-assigned when the
  // caller passed 0). Print the ready line in the exact shape every
  // other adapter uses so the runner's TCP-probe loop can attach.
  console.log(`ready addr=${server.hostname}:${server.port}`);

  const shutdown = (signal: string): void => {
    console.log(`bunraw: received ${signal}, shutting down`);
    // stop(true) closes idle keep-alives immediately; in-flight
    // requests still get to drain. Bun resolves the returned promise
    // when the listener is fully torn down, but we don't await it —
    // the runner's 5s SIGKILL backstop is the upper bound.
    server.stop(true);
    // Give Bun's loop a tick to finish flushing logs, then exit.
    setTimeout(() => process.exit(0), 50);
  };
  process.on("SIGTERM", () => shutdown("SIGTERM"));
  process.on("SIGINT", () => shutdown("SIGINT"));
}

// parseBind walks argv looking for -bind (the canonical probatorium
// flag). Falls back to BIND env var, then 0.0.0.0:8080. Accepts both
// `-bind 127.0.0.1:0` and `-bind=127.0.0.1:0`. Returns a {host, port}
// pair Bun.serve consumes directly — Bun resolves the hostname via
// getaddrinfo, so passing it as a string is fine.
function parseBind(argv: readonly string[]): { host: string; port: number } {
  let raw = process.env["BIND"] ?? "0.0.0.0:8080";
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
    throw new Error(`bunraw: invalid -bind value ${JSON.stringify(raw)}`);
  }
  const host = raw.slice(0, idx);
  const port = Number(raw.slice(idx + 1));
  if (!Number.isFinite(port) || port < 0 || port > 65535) {
    throw new Error(`bunraw: invalid port in -bind ${JSON.stringify(raw)}`);
  }
  return { host, port };
}

// parseEngine walks argv for -engine (accepts `-engine h1` and
// `-engine=h1`). Recognised values:
//   "" / absent / "h1" → Bun.serve HTTP/1.1 fast path.
//   "h2c"              → HTTP/2 cleartext prior-knowledge (node:http2).
// Any other value is a hard error: better to fail loudly than silently
// serve the wrong protocol and skew the bench. The bunraw column gives no
// Engine, so this returns "h1" in practice — but the bunraw-h2 column
// (h2c-noupg) flows through here without further changes.
function parseEngine(argv: readonly string[]): "h1" | "h2c" {
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
  if (raw === "" || raw === "h1") return "h1";
  if (raw === "h2c") return "h2c";
  console.error(
    `bunraw: unsupported -engine ${JSON.stringify(raw)} ` +
      `(supported: h1, h2c)`,
  );
  process.exit(2);
}
