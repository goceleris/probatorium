/* probatorium libreactor adapter — wave 5 (C).
 *
 * Same canonical contract as servers/axum, servers/drogon, served on a
 * hand-rolled HTTP/1.1 epoll loop via libreactor's high-level `server`
 * abstraction (src/reactor/server.h). Lifecycle and CLI match the other
 * native competitors so servers.StartAdapter launches every adapter with
 * one invocation pattern (`{bin} -bind <addr>`, wait for the
 * `ready addr=<addr>` stdout line, SIGTERM for graceful shutdown).
 *
 * Endpoint set (canonical bytes in servers/common/contract.go):
 *   GET  /            -> "Hello, World!"               text/plain
 *   GET  /json        -> {"message":"Hello, World!"}   application/json
 *   GET  /json-1k     -> deterministic 1026-byte JSON page
 *   GET  /json-8k     -> deterministic 8286-byte JSON page
 *   GET  /json-16k    -> deterministic 16463-byte JSON page
 *   GET  /json-64k    -> deterministic 65618-byte JSON page
 *   GET  /users/:id   -> "User ID: <id>"               text/plain
 *   POST /upload      -> read-and-discard body, reply "OK"  text/plain
 *
 * Driver-backed (/db, /cache, /mc, /session) and chain-* endpoints are out
 * of scope (Capabilities{Static:true} only), so unknown targets 404 — the
 * scenario applicability filter in servers/servers.go never schedules those
 * classes against this column.
 *
 * Engine: libreactor's server speaks HTTP/1.1 only (it parses requests with
 * picohttpparser and serializes HTTP/1.1 responses; there is no HTTP/2
 * framing layer anywhere in the library). h2c cleartext prior-knowledge is
 * therefore impossible, so `-engine h2c` fails fast with a non-zero exit —
 * exactly like drogon — and no libreactor-h2 column is registered.
 *
 * Why bytes-by-hand for JSON: the conformance probe byte-compares response
 * bodies against the Go-generated payload in common.Endpoints. We emit the
 * paginated JSON by hand (no clo encoder) so every byte matches Go's
 * encoding/json compact output. libclo is still linked (the canonical
 * libreactor link line is -lreactor -ldynamic -lclo) but unused here.
 */

#include <arpa/inet.h>
#include <netinet/in.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <unistd.h>

/* The umbrella <reactor.h> pulls in reactor/{data,net,http,server,...}.h, so
 * the server API, the `data` view type, and net_resolve/net_socket are all
 * available from this one include (libreactor 3.x). */
#include <reactor.h>

/* The single live server, parked at file scope so the SIGTERM/SIGINT
 * handler can ask the reactor to stop. server_shutdown() is the library's
 * graceful path: it stops accepting, drains in-flight requests, and lets
 * reactor_loop() return. */
static server g_server;
static volatile sig_atomic_t g_stopping = 0;

/* Pre-baked response bodies. Built once in main() and pointed at by `data`
 * views so every request reuses the same immutable bytes — no per-request
 * allocation, mirroring the Go adapters that serve a pre-computed slice. */
static data g_hello_plain;  /* "Hello, World!" */
static data g_hello_json;   /* {"message":"Hello, World!"} */
static data g_ok_plain;     /* "OK" */
static data g_json_1k;
static data g_json_8k;
static data g_json_16k;
static data g_json_64k;

static const data CT_TEXT = {.base = "text/plain", .size = 10};
static const data CT_JSON = {.base = "application/json", .size = 16};

/* Byte-identical port of servers/common/payload.go generateJSONPayload.
 * The Go reference marshals (paginatedResponse, paginatedItem) with
 * encoding/json, which emits compact JSON in struct-declaration field order.
 * We emit those bytes by hand so the body is byte-for-byte equal to the Go
 * generator. Termination rule mirrors Go: append items until the full
 * marshalled length (header + items + footer) crosses target_size. Sizes:
 *   1 KiB target  -> 1026 bytes
 *   8 KiB target  -> 8286 bytes
 *   16 KiB target -> 16463 bytes
 *   64 KiB target -> 65618 bytes
 * Returns a heap buffer (never freed — lives for the process lifetime) and
 * writes its length through *out_len. */
static char *generate_json_payload(size_t target_size, size_t *out_len)
{
  static const char header[] =
      "{\"page\":1,\"per_page\":50,\"total\":1000,\"total_pages\":20,\"data\":[";
  static const char footer[] = "]}";
  const size_t header_len = sizeof header - 1;
  const size_t footer_len = sizeof footer - 1;

  size_t cap = target_size + 256;
  char *buf = malloc(cap);
  if (!buf)
  {
    perror("libreactor: malloc payload");
    exit(1);
  }
  size_t len = 0;
  memcpy(buf, header, header_len);
  len += header_len;

  for (unsigned long i = 1;; i++)
  {
    /* Grow generously: one item is well under 128 bytes, but keep a wide
     * margin against the decimal width of i and the footer. */
    if (len + 256 > cap)
    {
      cap = cap * 2 + 256;
      char *grown = realloc(buf, cap);
      if (!grown)
      {
        perror("libreactor: realloc payload");
        exit(1);
      }
      buf = grown;
    }
    if (i > 1)
      buf[len++] = ',';
    len += (size_t)snprintf(
        buf + len, cap - len,
        "{\"id\":%lu,\"name\":\"User %lu\",\"email\":\"user%lu@example.com\","
        "\"status\":\"active\",\"created_at\":\"2024-01-15T09:30:00Z\"}",
        i, i, i);
    if (len + footer_len >= target_size)
      break;
  }
  memcpy(buf + len, footer, footer_len);
  len += footer_len;

  *out_len = len;
  return buf;
}

/* The request callback. libreactor delivers one SERVER_REQUEST event per
 * parsed HTTP request; event->data is the server_request, event->state is
 * the &g_server we registered. request->method / request->target /
 * request->data are `data` views into the parsed request (method, request
 * target, body). server_ok / server_respond serialize an HTTP/1.1 response
 * (status line + Server/Date/Content-Type/Content-Length headers computed by
 * the library) and release the request. */
static void on_request(reactor_event *event)
{
  server_request *request = (server_request *)event->data;
  data target = request->target;
  data method = request->method;

  /* Fast exact-match routes first (the hot static paths). data_equal does a
   * length + memcmp comparison. */
  if (data_equal(target, data_string("/")))
  {
    server_ok(request, CT_TEXT, g_hello_plain);
    return;
  }
  if (data_equal(target, data_string("/json")))
  {
    server_ok(request, CT_JSON, g_hello_json);
    return;
  }
  if (data_equal(target, data_string("/json-1k")))
  {
    server_ok(request, CT_JSON, g_json_1k);
    return;
  }
  if (data_equal(target, data_string("/json-8k")))
  {
    server_ok(request, CT_JSON, g_json_8k);
    return;
  }
  if (data_equal(target, data_string("/json-16k")))
  {
    server_ok(request, CT_JSON, g_json_16k);
    return;
  }
  if (data_equal(target, data_string("/json-64k")))
  {
    server_ok(request, CT_JSON, g_json_64k);
    return;
  }

  /* POST /upload — the library has already buffered the request body by the
   * time the handler fires (request->data), so the parse cost is part of the
   * measured path. We read-and-discard and reply with the literal "OK". */
  if (data_equal(target, data_string("/upload")) &&
      data_equal(method, data_string("POST")))
  {
    server_ok(request, CT_TEXT, g_ok_plain);
    return;
  }

  /* GET /users/<id> — prefix match, then echo the path segment after
   * "/users/". We test the prefix with an explicit length + memcmp rather
   * than data_prefix so the match does not depend on that helper's argument
   * order (target-starts-with-prefix vs prefix-starts-with-target). The body
   * is built on the stack per request (the id segment is small and bounded);
   * server_ok copies it into the response stream synchronously before
   * returning, so the stack buffer is safe to let go. */
  {
    static const char pfx[] = "/users/";
    const size_t pfx_len = sizeof pfx - 1;
    if (target.size >= pfx_len &&
        memcmp(target.base, pfx, pfx_len) == 0)
    {
      const char *id = (const char *)target.base + pfx_len;
      size_t id_len = target.size - pfx_len;
      /* "User ID: " + id. Bound the id so a hostile long target cannot
       * overflow the stack buffer; the bench only ever sends short ids. */
      char body[64];
      static const char lead[] = "User ID: ";
      const size_t lead_len = sizeof lead - 1;
      if (id_len > sizeof body - lead_len)
        id_len = sizeof body - lead_len;
      memcpy(body, lead, lead_len);
      memcpy(body + lead_len, id, id_len);
      data b = {.base = body, .size = lead_len + id_len};
      server_ok(request, CT_TEXT, b);
      return;
    }
  }

  server_not_found(request);
}

static void on_signal(int signo)
{
  (void)signo;
  g_stopping = 1;
  /* server_shutdown is safe to call from here: it flips the server's
   * accept state and arms the drain. reactor_loop() then returns once
   * in-flight work completes. */
  server_shutdown(&g_server);
}

/* Split "host:port" on the LAST colon (drogon parity). An empty host means
 * the wildcard bind 0.0.0.0; an empty/":0" port means kernel-assigned. */
static void parse_bind(const char *bind, char *host, size_t host_cap, char *port,
                       size_t port_cap)
{
  const char *colon = strrchr(bind, ':');
  if (!colon)
  {
    /* No port — treat the whole thing as host, default port 8080. */
    snprintf(host, host_cap, "%s", bind);
    snprintf(port, port_cap, "%s", "8080");
  }
  else
  {
    size_t hlen = (size_t)(colon - bind);
    if (hlen >= host_cap)
      hlen = host_cap - 1;
    memcpy(host, bind, hlen);
    host[hlen] = '\0';
    snprintf(port, port_cap, "%s", colon + 1);
  }
  if (host[0] == '\0')
    snprintf(host, host_cap, "%s", "0.0.0.0");
}

/* Print "ready addr=<actual-host:port>" exactly once. The runner's TCP probe
 * waits for this line, so we resolve the REAL bound address with getsockname
 * (port may be kernel-assigned when the caller passed :0) and flush before
 * the accept loop starts. */
static void announce_ready(int fd)
{
  struct sockaddr_storage ss;
  socklen_t sl = sizeof ss;
  char ip[INET6_ADDRSTRLEN] = "0.0.0.0";
  unsigned port = 0;

  if (getsockname(fd, (struct sockaddr *)&ss, &sl) == 0)
  {
    if (ss.ss_family == AF_INET)
    {
      struct sockaddr_in *a = (struct sockaddr_in *)&ss;
      inet_ntop(AF_INET, &a->sin_addr, ip, sizeof ip);
      port = ntohs(a->sin_port);
    }
    else if (ss.ss_family == AF_INET6)
    {
      struct sockaddr_in6 *a = (struct sockaddr_in6 *)&ss;
      inet_ntop(AF_INET6, &a->sin6_addr, ip, sizeof ip);
      port = ntohs(a->sin6_port);
    }
  }
  printf("ready addr=%s:%u\n", ip, port);
  fflush(stdout);
}

int main(int argc, char *argv[])
{
  const char *bind = "127.0.0.1:8080";
  const char *engine = "h1";

  for (int i = 1; i < argc; i++)
  {
    const char *arg = argv[i];
    if ((strcmp(arg, "-bind") == 0 || strcmp(arg, "--bind") == 0) && i + 1 < argc)
      bind = argv[++i];
    else if (strncmp(arg, "-bind=", 6) == 0)
      bind = arg + 6;
    else if (strncmp(arg, "--bind=", 7) == 0)
      bind = arg + 7;
    else if ((strcmp(arg, "-engine") == 0 || strcmp(arg, "--engine") == 0) &&
             i + 1 < argc)
      engine = argv[++i];
    else if (strncmp(arg, "-engine=", 8) == 0)
      engine = arg + 8;
    else if (strncmp(arg, "--engine=", 9) == 0)
      engine = arg + 9;
  }

  /* Wire-protocol gate. h1 (or absent) -> HTTP/1.1. h2c -> unsupported:
   * libreactor's server has no HTTP/2 framing, so refuse rather than serve
   * h1 under an h2c label (which would corrupt a libreactor-h2 column). The
   * bench's bind-gate then records that column as not-applicable/DNF. */
  if (strcmp(engine, "h2c") == 0)
  {
    fprintf(stderr,
            "libreactor: h2c not supported (libreactor's server speaks "
            "HTTP/1.1 only — no HTTP/2 framing layer) — refusing to serve "
            "h1 under an h2c label\n");
    return 2;
  }
  if (strcmp(engine, "h1") != 0)
  {
    fprintf(stderr, "libreactor: unknown -engine value %s (want h1 or h2c)\n",
            engine);
    return 2;
  }

  /* Build the static bodies once. */
  g_hello_plain = data_string("Hello, World!");
  g_hello_json = data_string("{\"message\":\"Hello, World!\"}");
  g_ok_plain = data_string("OK");
  {
    size_t n;
    char *p;
    p = generate_json_payload(1024, &n);
    g_json_1k = data_construct(p, n);
    p = generate_json_payload(8192, &n);
    g_json_8k = data_construct(p, n);
    p = generate_json_payload(16384, &n);
    g_json_16k = data_construct(p, n);
    p = generate_json_payload(65536, &n);
    g_json_64k = data_construct(p, n);
  }

  char host[256];
  char port[32];
  parse_bind(bind, host, sizeof host, port, sizeof port);

  /* Resolve + create the listening socket. net_socket sets
   * SO_REUSEADDR/SO_REUSEPORT, binds, and listens (backlog INT_MAX) for an
   * AI_PASSIVE addrinfo, returning the listening fd. net_resolve takes
   * mutable char*, so pass our local buffers. */
  struct addrinfo *ai = net_resolve(host, port, AF_INET, SOCK_STREAM, AI_PASSIVE);
  if (!ai)
  {
    fprintf(stderr, "libreactor: net_resolve %s:%s failed\n", host, port);
    return 1;
  }
  int fd = net_socket(ai);
  if (fd < 0)
  {
    fprintf(stderr, "libreactor: bind/listen %s:%s failed\n", host, port);
    return 1;
  }

  reactor_construct();
  server_construct(&g_server, on_request, &g_server);
  server_open(&g_server, fd, NULL);

  /* Graceful shutdown on SIGTERM/SIGINT (well inside the runner's 5s grace
   * window). SIGPIPE off so a client reset mid-write never kills us. */
  signal(SIGPIPE, SIG_IGN);
  signal(SIGTERM, on_signal);
  signal(SIGINT, on_signal);

  announce_ready(fd);

  reactor_loop();

  server_destruct(&g_server);
  reactor_destruct();
  return 0;
}
