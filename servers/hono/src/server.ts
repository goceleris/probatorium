// Probatorium hono adapter (wave 4b, bun runtime).
//
// Fastest documented path on Bun: build a Hono app and hand its
// .fetch to Bun.serve directly, skipping the @hono/node-server
// adapter (Node compat shim). Bun.serve speaks the WHATWG Fetch
// interface natively, which is exactly what hono.fetch produces, so
// there is zero translation overhead between the two.
//
// CLI contract (matched by every probatorium adapter):
//   -bind <addr>   address:port to listen on (default 0.0.0.0:8080).
//                  Port 0 is supported and the resolved port is echoed
//                  back via the "ready addr=..." line.
//   stdout         "ready addr=<final-bind-addr>" once listening.
//   SIGTERM        graceful shutdown within 5s (Bun.serve.stop()).
//
// argv parsing is hand-rolled because Bun supports `process.argv` but
// the canonical CLI form `bun run dist/server -- -bind 127.0.0.1:0`
// puts our flags after `--`; we walk the array looking for `-bind` to
// stay robust to either invocation shape.
//
// -engine flag: the bench passes `-engine <value>` only when the registry
// gives the adapter an Engine. Today hono has none, so no -engine arrives
// — but we parse it defensively so an added Engine works without a code
// change. "h1" (or absent) stays on the Bun.serve fast path. "h2c" serves
// HTTP/2 cleartext prior-knowledge via the node:http2 bridge in h2c.ts.
// Any other value exits non-zero with a clear message.

import { Hono } from "hono";
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

const app = new Hono();

// Routes mirror servers/common/contract.go exactly. Responses are
// pre-encoded Uint8Array buffers so per-request work is just a
// Response constructor — no JSON.stringify, no header allocation
// beyond the two we set.
app.get("/", (c) =>
  c.body(HELLO, 200, {
    "Content-Type": TEXT,
    "Content-Length": String(HELLO.length),
  }),
);

app.get("/json", (c) =>
  c.body(JSON_HELLO, 200, {
    "Content-Type": JSON_CT,
    "Content-Length": String(JSON_HELLO.length),
  }),
);

app.get("/json-1k", (c) =>
  c.body(JSON_1K, 200, {
    "Content-Type": JSON_CT,
    "Content-Length": String(JSON_1K.length),
  }),
);

app.get("/json-8k", (c) =>
  c.body(JSON_8K, 200, {
    "Content-Type": JSON_CT,
    "Content-Length": String(JSON_8K.length),
  }),
);

app.get("/json-16k", (c) =>
  c.body(JSON_16K, 200, {
    "Content-Type": JSON_CT,
    "Content-Length": String(JSON_16K.length),
  }),
);

app.get("/json-64k", (c) =>
  c.body(JSON_64K, 200, {
    "Content-Type": JSON_CT,
    "Content-Length": String(JSON_64K.length),
  }),
);

app.get("/users/:id", (c) => {
  const id = c.req.param("id");
  // Echo path parameter verbatim — matches WritePath in
  // servers/common/common.go.
  const body = "User ID: " + id;
  return c.body(body, 200, { "Content-Type": TEXT });
});

app.post("/upload", async (c) => {
  // Drain request body so the body parser is part of the measured
  // cost (every framework does this deliberately). Bun's Request.body
  // is a ReadableStream; consuming arrayBuffer() exhausts it.
  await c.req.arrayBuffer();
  return c.body(OK, 200, {
    "Content-Type": TEXT,
    "Content-Length": String(OK.length),
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
    console.log(`hono: received ${signal}, shutting down`);
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
    fetch: app.fetch,
  });

  // Bun.serve.port is the resolved port (kernel-assigned when the
  // caller passed 0). Print the ready line in the exact shape every
  // other adapter uses so the runner's TCP-probe loop can attach.
  console.log(`ready addr=${server.hostname}:${server.port}`);

  const shutdown = (signal: string): void => {
    console.log(`hono: received ${signal}, shutting down`);
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
// `-bind 127.0.0.1:0` and `-bind=127.0.0.1:0`. Returns a
// {host, port} pair Bun.serve consumes directly — Bun resolves the
// hostname via getaddrinfo, so passing it as a string is fine.
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
    throw new Error(`hono: invalid -bind value ${JSON.stringify(raw)}`);
  }
  const host = raw.slice(0, idx);
  const port = Number(raw.slice(idx + 1));
  if (!Number.isFinite(port) || port < 0 || port > 65535) {
    throw new Error(`hono: invalid port in -bind ${JSON.stringify(raw)}`);
  }
  return { host, port };
}

// parseEngine walks argv for -engine (accepts `-engine h1` and
// `-engine=h1`). Recognised values:
//   "" / absent / "h1" → Bun.serve HTTP/1.1 fast path.
//   "h2c"              → HTTP/2 cleartext prior-knowledge (node:http2).
// Any other value is a hard error: better to fail loudly than silently
// serve the wrong protocol and skew the bench. The registry currently
// gives hono no Engine, so this returns "h1" in practice — but an added
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
    `hono: unsupported -engine ${JSON.stringify(raw)} ` +
      `(supported: h1, h2c)`,
  );
  process.exit(2);
}
