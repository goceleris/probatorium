// HTTP/2 cleartext (h2c) prior-knowledge server for the Bun runtime.
//
// Why this exists
// ---------------
// Bun.serve speaks HTTP/1.1 natively and offers HTTP/2 ONLY behind TLS
// (the `tls:` option) — and HTTP/3 only behind TLS+QUIC. There is no
// cleartext-h2 switch on Bun.serve as of Bun 1.3.14 — verified via
// `bun --help` (the only http2 flag is `--experimental-http2-fetch`,
// which is the *client* h2-over-TLS-ALPN path, not a server option). So
// to serve h2c prior-knowledge we drop to Bun's node:http2 compatibility
// layer, whose `http2.createServer()` stands up a cleartext h2 listener.
// We then bridge each h2 stream to the raw WHATWG `fetch` handler so the
// route table and payloads stay byte-identical to the Bun.serve (h1)
// fast path.
//
// Bun-specific gotcha
// -------------------
// Converting the inbound h2 request stream to a web ReadableStream via
// `Readable.toWeb(stream)` and handing it to `new Request(..., { body })`
// HANGS under Bun's node:http2 (the body never drains, /upload stalls).
// We therefore collect the request body manually off the node stream's
// "data"/"end" events into a Buffer before constructing the Request. GET
// requests carry no body and skip this entirely.
//
// This path is only taken for `-engine h2c`. `-engine h1` (or no flag)
// stays on Bun.serve and never imports this module's runtime cost.

import http2 from "node:http2";
import type {
  Http2Server,
  ServerHttp2Stream,
  IncomingHttpHeaders,
} from "node:http2";

// FetchHandler is the WHATWG handler shape the raw-Bun server exposes: a
// Request in, a Response (or Promise) out. Same shape Hono (app.fetch)
// and Elysia (app.fetch) hand the h2c bridge in the framework adapters.
export type FetchHandler = (req: Request) => Response | Promise<Response>;

export interface H2CListenResult {
  hostname: string;
  port: number;
  stop: () => void;
}

// serveH2C stands up a cleartext HTTP/2 (prior-knowledge) listener on
// host:port that dispatches every stream through `handler`. The returned
// promise resolves once the socket is listening, mirroring the synchronous
// readiness Bun.serve gives the h1 path. `port: 0` is honoured (the kernel
// assigns one) and the resolved port is reported back.
export function serveH2C(
  host: string,
  port: number,
  handler: FetchHandler,
): Promise<H2CListenResult> {
  const server: Http2Server = http2.createServer();

  server.on("stream", (stream, headers) => {
    // node's "stream" event types the first arg as the base Http2Stream,
    // but a server stream is always a ServerHttp2Stream (it carries
    // respond()/headersSent — the half we use). Narrow it here.
    void dispatch(stream as ServerHttp2Stream, headers, handler);
  });

  // A stream-level error (client RST, malformed frame) must not crash the
  // process — the bench probes edge cases. Swallow it; the stream is gone.
  server.on("error", () => {});

  return new Promise<H2CListenResult>((resolve, reject) => {
    server.once("error", reject);
    server.listen(port, host, () => {
      server.removeListener("error", reject);
      const addr = server.address();
      const resolvedPort =
        addr && typeof addr === "object" ? addr.port : port;
      resolve({
        hostname: host,
        port: resolvedPort,
        stop: () => server.close(),
      });
    });
  });
}

async function dispatch(
  stream: ServerHttp2Stream,
  headers: IncomingHttpHeaders,
  handler: FetchHandler,
): Promise<void> {
  try {
    const method = (headers[":method"] as string | undefined) ?? "GET";
    const path = (headers[":path"] as string | undefined) ?? "/";
    const authority =
      (headers[":authority"] as string | undefined) ?? "127.0.0.1";
    const scheme = (headers[":scheme"] as string | undefined) ?? "http";
    const url = scheme + "://" + authority + path;

    // Copy non-pseudo headers across so the handler sees the real
    // request headers (Content-Type on /upload, etc.).
    const reqHeaders = new Headers();
    for (const key of Object.keys(headers)) {
      if (key.startsWith(":")) continue;
      const value = headers[key];
      if (value === undefined) continue;
      if (Array.isArray(value)) {
        for (const v of value) reqHeaders.append(key, v);
      } else {
        reqHeaders.set(key, String(value));
      }
    }

    const init: RequestInit = { method, headers: reqHeaders };
    const hasBody = method !== "GET" && method !== "HEAD";
    if (hasBody) {
      // Manual drain — Readable.toWeb(stream) hangs under Bun's
      // node:http2 (see module header).
      init.body = await collectBody(stream);
    }

    const response = await handler(new Request(url, init));

    const outHeaders: Record<string, string | number> = {
      ":status": response.status,
    };
    response.headers.forEach((value, key) => {
      // node:http2 forbids connection-specific headers on h2 frames.
      if (key === "connection" || key === "keep-alive") return;
      outHeaders[key] = value;
    });

    const body = Buffer.from(await response.arrayBuffer());
    stream.respond(outHeaders);
    stream.end(body);
  } catch {
    if (!stream.headersSent) {
      try {
        stream.respond({ ":status": 500 });
      } catch {
        // stream already torn down — nothing to do.
      }
    }
    try {
      stream.end();
    } catch {
      // ignore
    }
  }
}

// collectBody pulls the inbound h2 request body off the node stream into a
// single Buffer. Used only for body-bearing methods (POST /upload).
function collectBody(stream: ServerHttp2Stream): Promise<Buffer> {
  return new Promise<Buffer>((resolve, reject) => {
    const chunks: Buffer[] = [];
    stream.on("data", (chunk: Buffer) => chunks.push(chunk));
    stream.on("end", () => resolve(Buffer.concat(chunks)));
    stream.on("error", reject);
  });
}
