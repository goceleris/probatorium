// Probatorium elysia adapter (wave 4b, bun runtime).
//
// Elysia under Bun has two startup paths:
//   1. app.listen(port) — Elysia's wrapper that internally calls
//      Bun.serve({ fetch: app.fetch, ...opts }).
//   2. Bun.serve({ fetch: app.fetch }) — direct path with no
//      framework wrapper.
//
// Both end up at the same Bun.serve fast path, but option 2 lets us
// own hostname/port/reusePort wiring without re-deriving them inside
// Elysia's listen handler. Per the wave 4b brief ("Bun.serve direct,
// NOT through Elysia's .listen wrapper if it adds overhead") we go
// straight to Bun.serve.
//
// CLI contract (matched by every probatorium adapter):
//   -bind <addr>   address:port to listen on (default 0.0.0.0:8080).
//                  Port 0 supported; resolved port is echoed in
//                  "ready addr=...".
//   stdout         "ready addr=<final-bind-addr>" once listening.
//   SIGTERM        graceful shutdown within 5s.
//
// -engine flag: the bench passes `-engine <value>` only when the registry
// gives the adapter an Engine. Today elysia has none, so no -engine arrives
// — but we parse it defensively so an added Engine works without a code
// change. "h1" (or absent) stays on the Bun.serve fast path. "h2c" serves
// HTTP/2 cleartext prior-knowledge via the node:http2 bridge in h2c.ts.
// Any other value exits non-zero with a clear message.

import { Elysia } from "elysia";
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

// Pre-built Response objects can't be re-used (the body stream is
// single-shot), so we construct fresh Responses per request but with
// pre-encoded bodies + frozen header objects. Elysia's handler return
// of `Response` is the lowest-overhead path: it skips Elysia's
// auto-content-negotiation and writes our headers verbatim.
const headersText: Record<string, string> = { "Content-Type": TEXT };
const headersJSON: Record<string, string> = { "Content-Type": JSON_CT };

const app = new Elysia()
  .get(
    "/",
    () =>
      new Response(HELLO, {
        status: 200,
        headers: { ...headersText, "Content-Length": String(HELLO.length) },
      }),
  )
  .get(
    "/json",
    () =>
      new Response(JSON_HELLO, {
        status: 200,
        headers: { ...headersJSON, "Content-Length": String(JSON_HELLO.length) },
      }),
  )
  .get(
    "/json-1k",
    () =>
      new Response(JSON_1K, {
        status: 200,
        headers: { ...headersJSON, "Content-Length": String(JSON_1K.length) },
      }),
  )
  .get(
    "/json-8k",
    () =>
      new Response(JSON_8K, {
        status: 200,
        headers: { ...headersJSON, "Content-Length": String(JSON_8K.length) },
      }),
  )
  .get(
    "/json-16k",
    () =>
      new Response(JSON_16K, {
        status: 200,
        headers: { ...headersJSON, "Content-Length": String(JSON_16K.length) },
      }),
  )
  .get(
    "/json-64k",
    () =>
      new Response(JSON_64K, {
        status: 200,
        headers: { ...headersJSON, "Content-Length": String(JSON_64K.length) },
      }),
  )
  .get("/users/:id", ({ params }) => {
    // Echo path parameter verbatim — matches WritePath in
    // servers/common/common.go.
    const body = "User ID: " + params.id;
    return new Response(body, {
      status: 200,
      headers: { ...headersText },
    });
  })
  .post("/upload", async ({ request }) => {
    // Drain request body so the body parser is part of the measured
    // cost (every framework does this deliberately).
    await request.arrayBuffer();
    return new Response(OK, {
      status: 200,
      headers: { ...headersText, "Content-Length": String(OK.length) },
    });
  });

const { host, port } = parseBind(process.argv);
const engine = parseEngine(process.argv);

if (engine === "h2c") {
  // HTTP/2 cleartext prior-knowledge via node:http2 (see h2c.ts). Bun.serve
  // has no cleartext-h2 server option as of Bun 1.3.14, so we bridge the h2
  // streams to the same app.fetch handler the h1 path uses.
  const h2c = await serveH2C(host, port, app.fetch);
  console.log(`ready addr=${h2c.hostname}:${h2c.port}`);

  const shutdownH2C = (signal: string): void => {
    console.log(`elysia: received ${signal}, shutting down`);
    h2c.stop();
    setTimeout(() => process.exit(0), 50);
  };
  process.on("SIGTERM", () => shutdownH2C("SIGTERM"));
  process.on("SIGINT", () => shutdownH2C("SIGINT"));
} else {
  const server = Bun.serve({
    hostname: host,
    port,
    reusePort: true,
    fetch: app.fetch,
  });

  console.log(`ready addr=${server.hostname}:${server.port}`);

  const shutdown = (signal: string): void => {
    console.log(`elysia: received ${signal}, shutting down`);
    server.stop(true);
    setTimeout(() => process.exit(0), 50);
  };
  process.on("SIGTERM", () => shutdown("SIGTERM"));
  process.on("SIGINT", () => shutdown("SIGINT"));
}

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
    throw new Error(`elysia: invalid -bind value ${JSON.stringify(raw)}`);
  }
  const host = raw.slice(0, idx);
  const port = Number(raw.slice(idx + 1));
  if (!Number.isFinite(port) || port < 0 || port > 65535) {
    throw new Error(`elysia: invalid port in -bind ${JSON.stringify(raw)}`);
  }
  return { host, port };
}

// parseEngine walks argv for -engine (accepts `-engine h1` and
// `-engine=h1`). Recognised values:
//   "" / absent / "h1" → Bun.serve HTTP/1.1 fast path.
//   "h2c"              → HTTP/2 cleartext prior-knowledge (node:http2).
// Any other value is a hard error: better to fail loudly than silently
// serve the wrong protocol and skew the bench. The registry currently
// gives elysia no Engine, so this returns "h1" in practice — but an added
// Engine flows through here without further changes.
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
    `elysia: unsupported -engine ${JSON.stringify(raw)} ` +
      `(supported: h1, h2c)`,
  );
  process.exit(2);
}
