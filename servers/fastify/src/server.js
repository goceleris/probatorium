// Probatorium fastify adapter (Node.js runtime).
//
// Serves the canonical contract endpoints declared in
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
// Driver-backed (/db, /cache, /mc, /session) and chain-* endpoints are out
// of scope here — the scenario applicability filter (servers/servers.go)
// never schedules those cells against a Static-only adapter, so they are
// simply not mounted.
//
// CLI contract (matched by every probatorium adapter):
//   -bind <host:port>  default 127.0.0.1:8080. Pass `:0` (or `host:0`) to
//                      let the kernel assign a port; the resolved address
//                      is echoed on stdout via the `ready addr=<addr>` line
//                      the runner waits for before opening loadgen. Both
//                      `-bind X` and `-bind=X` (and `--bind` spellings) are
//                      accepted, as is an env BIND fallback.
//   -engine <h1|h2c>   default h1.
//                        h1  — plain HTTP/1.1 (Fastify on node:http).
//                        h2c — HTTP/2 cleartext, PRIOR-KNOWLEDGE only:
//                              Fastify({ http2: true }) with no `https`
//                              stands up node:http2.createServer(), a
//                              cleartext h2 listener. No TLS, no h1->h2
//                              upgrade — a client must open with the h2
//                              preface (curl --http2-prior-knowledge),
//                              matching stdhttp-h2 / axum-h2's h2c-noupg
//                              semantics. Any other value exits non-zero.
//
// Lifecycle: SIGTERM (or SIGINT) triggers fastify.close(), which stops
// accepting and drains in-flight requests well within the runner's
// 5-second grace window (servers/start.go) before the SIGKILL fallback.

'use strict';

const Fastify = require('fastify');
const {
  JSON_1K_PAYLOAD,
  JSON_8K_PAYLOAD,
  JSON_16K_PAYLOAD,
  JSON_64K_PAYLOAD,
} = require('./payload');

// Static byte payloads. Pre-encoded Buffers reused across every request —
// no per-request allocation, and Fastify treats a Buffer as pre-serialized
// (sent verbatim, no response validation / re-encode), so the wire bytes
// are byte-identical to common.Endpoints[...].ResponseBody regardless of
// the Content-Type we set.
const HELLO_PLAIN = Buffer.from('Hello, World!');
const HELLO_JSON = Buffer.from('{"message":"Hello, World!"}');
const OK_PLAIN = Buffer.from('OK');

const TEXT = 'text/plain';
const JSON_CT = 'application/json';

function buildApp(engine) {
  // http2:true with no `https` ⇒ Fastify uses node:http2.createServer(),
  // i.e. cleartext HTTP/2 (h2c prior-knowledge). The h1 path stays on
  // node:http. disableRequestLogging + no logger keep the per-request cost
  // to the framework, not to logging.
  const app = Fastify({
    http2: engine === 'h2c',
    logger: false,
    disableRequestLogging: true,
  });

  // Catch-all body parser: read-and-discard the request body for ANY
  // content type. Without this, a POST /upload whose body is not valid
  // JSON (or carries an unmapped Content-Type) would fail in Fastify's
  // default parser. Draining the stream keeps the body parser in the
  // measured path (matching every other adapter) without buffering the
  // payload. done() with no value yields an undefined body the handler
  // ignores.
  app.addContentTypeParser('*', (_req, payload, done) => {
    payload.resume();
    payload.on('end', () => done(null, undefined));
    payload.on('error', done);
  });

  app.get('/', (_req, reply) => {
    reply.header('content-type', TEXT).send(HELLO_PLAIN);
  });

  app.get('/json', (_req, reply) => {
    reply.header('content-type', JSON_CT).send(HELLO_JSON);
  });

  app.get('/json-1k', (_req, reply) => {
    reply.header('content-type', JSON_CT).send(JSON_1K_PAYLOAD);
  });

  app.get('/json-8k', (_req, reply) => {
    reply.header('content-type', JSON_CT).send(JSON_8K_PAYLOAD);
  });

  app.get('/json-16k', (_req, reply) => {
    reply.header('content-type', JSON_CT).send(JSON_16K_PAYLOAD);
  });

  app.get('/json-64k', (_req, reply) => {
    reply.header('content-type', JSON_CT).send(JSON_64K_PAYLOAD);
  });

  app.get('/users/:id', (req, reply) => {
    // Echo the path param verbatim — matches WritePath in
    // servers/common/common.go. A string sent with text/plain set goes out
    // unmodified (no custom serializer registered for text/plain).
    reply.header('content-type', TEXT).send('User ID: ' + req.params.id);
  });

  app.post('/upload', (_req, reply) => {
    // Body was already drained-and-discarded by the catch-all parser above.
    reply.header('content-type', TEXT).send(OK_PLAIN);
  });

  return app;
}

// parseFlag walks argv for `-name <value>` / `-name=value` (and the `--`
// spellings), Go-flag style — the convention every probatorium adapter
// follows so the runner can invoke this binary identically. Returns
// undefined when the flag is absent.
function parseFlag(argv, name) {
  const short = '-' + name;
  const long = '--' + name;
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a === short || a === long) return argv[i + 1];
    if (a.startsWith(short + '=')) return a.slice(short.length + 1);
    if (a.startsWith(long + '=')) return a.slice(long.length + 1);
  }
  return undefined;
}

// parseBind splits `host:port` into { host, port }. IPv6 literals are
// accepted in bracketed form ([::1]:8080). port 0 ⇒ kernel-assigned.
function parseBind(raw) {
  if (raw.startsWith('[')) {
    const rb = raw.indexOf(']');
    if (rb < 0 || raw[rb + 1] !== ':') {
      throw new Error('fastify: invalid -bind ' + JSON.stringify(raw));
    }
    return { host: raw.slice(1, rb), port: Number(raw.slice(rb + 2)) };
  }
  const idx = raw.lastIndexOf(':');
  if (idx < 0) {
    throw new Error('fastify: invalid -bind ' + JSON.stringify(raw));
  }
  const host = raw.slice(0, idx);
  const port = Number(raw.slice(idx + 1));
  if (!host || !Number.isInteger(port) || port < 0 || port > 65535) {
    throw new Error('fastify: invalid -bind ' + JSON.stringify(raw));
  }
  return { host, port };
}

async function main() {
  const argv = process.argv.slice(2);

  const engineRaw = parseFlag(argv, 'engine') ?? 'h1';
  if (engineRaw !== 'h1' && engineRaw !== 'h2c') {
    process.stderr.write(
      'fastify: unknown -engine ' +
        JSON.stringify(engineRaw) +
        ' (want h1|h2c)\n',
    );
    process.exit(2);
  }

  const bindRaw = parseFlag(argv, 'bind') ?? process.env.BIND ?? '127.0.0.1:8080';
  let host;
  let port;
  try {
    ({ host, port } = parseBind(bindRaw));
  } catch (err) {
    process.stderr.write(String(err.message ?? err) + '\n');
    process.exit(1);
    return;
  }

  const app = buildApp(engineRaw);

  try {
    await app.listen({ host, port });
  } catch (err) {
    process.stderr.write('fastify: listen ' + bindRaw + ': ' + err + '\n');
    process.exit(1);
    return;
  }

  // Report the resolved address. We echo the requested host with the
  // resolved port (app.server.address().port) rather than the address
  // Fastify hands back from listen(): binding 0.0.0.0 makes Fastify report
  // the first concrete IPv4, which would surprise a runner that dialed the
  // host it asked for. The port is the only field that can change under
  // `:0`, so requested-host + resolved-port is the correct identity.
  const resolvedPort = app.server.address().port;
  process.stdout.write('ready addr=' + host + ':' + resolvedPort + '\n');

  let closing = false;
  const shutdown = () => {
    if (closing) return;
    closing = true;
    // fastify.close() stops accepting and drains in-flight requests, then
    // resolves. The runner's 5s SIGKILL backstop is the upper bound; we
    // exit 0 once close resolves.
    app.close().then(
      () => process.exit(0),
      () => process.exit(0),
    );
  };
  process.on('SIGTERM', shutdown);
  process.on('SIGINT', shutdown);
}

main().catch((err) => {
  process.stderr.write('fastify: fatal: ' + err + '\n');
  process.exit(1);
});
