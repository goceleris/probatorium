/*
 * probatorium h2o adapter — C, libh2o (the H2O project's embeddable HTTP
 * server library). Same contract as servers/axum, servers/drogon: the
 * eight canonical static endpoints, served byte-identically.
 *
 * Endpoint set (see servers/common/contract.go for the canonical bytes):
 *   GET  /            -> "Hello, World!"             text/plain
 *   GET  /json        -> {"message":"Hello, World!"} application/json
 *   GET  /json-1k     -> deterministic 1026-byte JSON page
 *   GET  /json-8k     -> deterministic 8286-byte JSON page
 *   GET  /json-16k    -> deterministic 16463-byte JSON page
 *   GET  /json-64k    -> deterministic 65618-byte JSON page
 *   GET  /users/:id   -> "User ID: <id>"             text/plain
 *   POST /upload      -> read-and-discard body, reply "OK"  text/plain
 *
 * Driver-backed (/db, /cache, /mc, /session) and chain-* endpoints are out
 * of scope (Capabilities all-false: static + concurrency scenarios only),
 * so anything unmatched returns 404 — the scenario applicability filter in
 * servers/servers.go skips h2o for those classes.
 *
 * CLI: `{bin} -bind <addr> [-engine h1|h2c]`. Mirrors the Rust/C++
 * adapters so servers.StartAdapter launches every native adapter with one
 * pattern (`{bin} -bind {addr}`, wait for the `ready addr=<addr>` stdout
 * line, SIGTERM for graceful drain).
 *
 *   -bind <host:port>  default 127.0.0.1:8080. `host:0` lets the kernel
 *                      pick the port; the actually-bound address is read
 *                      back with getsockname and reported on stdout.
 *   -engine <value>    default "h1".
 *       h1  -> plain HTTP/1.1 on a single cleartext h2o_evloop listener.
 *       h2c -> HTTP/2 cleartext prior-knowledge. NOT served as a distinct
 *              column: h2o's cleartext accept path speaks h1 AND h2c
 *              prior-knowledge on the SAME socket (it sniffs the
 *              "PRI * HTTP/2.0" preface and dispatches to the HTTP/2
 *              handler, otherwise stays h1) — it cannot REFUSE h1 the way
 *              the strict h2c-noupg contract demands. Serving h1 under an
 *              h2c label would corrupt the column, so -engine h2c fails
 *              fast (exit 2), exactly like the drogon adapter. h2o stays
 *              H1-only; no h2o-h2 column is registered.
 *
 * Threading: one h2o_evloop PER CORE via fork + SO_REUSEPORT. libh2o has no
 * in-process multi-loop accept sharing, so a single evloop pins the adapter
 * to one core (~190k RPS on a 16-core host — a thread-pool artefact, not
 * h2o's real ceiling). Instead we resolve the port once, print ready once,
 * then fork one worker per online CPU; each worker runs its own evloop and
 * binds the same host:port with its own SO_REUSEPORT listen fd, letting the
 * kernel spread connections across all cores. SIGTERM reaches the whole
 * process group (the runner signals the group), so every worker drains.
 */

#include <arpa/inet.h>
#include <errno.h>
#include <limits.h>
#include <netinet/in.h>
#include <signal.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <unistd.h>

#include "h2o.h"
#include "h2o/http1.h"
#include "h2o/http2.h"

static h2o_globalconf_t config;
static h2o_context_t ctx;
static h2o_accept_ctx_t accept_ctx;

static void on_accept(h2o_socket_t *listener, const char *err);

/* Pre-baked response bodies. Built once at startup so each request writes a
 * shared, immutable buffer — no per-request allocation, mirroring the Go
 * adapters that serve a pre-computed slice from servers/common. */
static h2o_iovec_t json1k, json8k, json16k, json64k;

/* ---- payload generator ----
 * Byte-identical port of servers/common/payload.go generateJSONPayload. The
 * Go reference marshals a (paginatedResponse, paginatedItem) struct pair with
 * encoding/json, which emits compact JSON in struct-declaration field order.
 * We emit those bytes by hand. Termination rule mirrors Go: append items
 * until the full marshalled length (header + items + footer) crosses
 * targetSize, so the resulting sizes match exactly:
 *   1 KiB target  -> 1026 bytes
 *   8 KiB target  -> 8286 bytes
 *   16 KiB target -> 16463 bytes
 *   64 KiB target -> 65618 bytes
 * The returned buffer is heap-allocated for process lifetime (never freed —
 * it lives until exit). */
static h2o_iovec_t generate_json_payload(size_t target_size)
{
    static const char header[] =
        "{\"page\":1,\"per_page\":50,\"total\":1000,\"total_pages\":20,\"data\":[";
    static const char footer[] = "]}";
    const size_t header_len = sizeof(header) - 1;
    const size_t footer_len = sizeof(footer) - 1;

    size_t cap = target_size + 256;
    char *buf = h2o_mem_alloc(cap);
    size_t len = 0;

    memcpy(buf, header, header_len);
    len += header_len;

    for (uint64_t i = 1;; ++i) {
        char item[160];
        int n;
        if (i > 1) {
            n = snprintf(item, sizeof(item),
                         ",{\"id\":%llu,\"name\":\"User %llu\",\"email\":\"user%llu@example.com\","
                         "\"status\":\"active\",\"created_at\":\"2024-01-15T09:30:00Z\"}",
                         (unsigned long long)i, (unsigned long long)i, (unsigned long long)i);
        } else {
            n = snprintf(item, sizeof(item),
                         "{\"id\":%llu,\"name\":\"User %llu\",\"email\":\"user%llu@example.com\","
                         "\"status\":\"active\",\"created_at\":\"2024-01-15T09:30:00Z\"}",
                         (unsigned long long)i, (unsigned long long)i, (unsigned long long)i);
        }
        if (len + (size_t)n + footer_len + 1 > cap) {
            cap = (len + (size_t)n + footer_len + 1) * 2;
            buf = h2o_mem_realloc(buf, cap);
        }
        memcpy(buf + len, item, (size_t)n);
        len += (size_t)n;

        if (len + footer_len >= target_size)
            break;
    }
    memcpy(buf + len, footer, footer_len);
    len += footer_len;

    return h2o_iovec_init(buf, len);
}

/* ---- response helpers ---- */

/* Send a fixed, process-lifetime body. The buffer is NOT duplicated into the
 * request pool (it outlives every request), so h2o_send borrows it directly. */
static int send_static(h2o_req_t *req, h2o_iovec_t body, const char *content_type, size_t ct_len)
{
    static h2o_generator_t generator = {NULL, NULL};
    req->res.status = 200;
    req->res.reason = "OK";
    h2o_add_header(&req->pool, &req->res.headers, H2O_TOKEN_CONTENT_TYPE, NULL, content_type, ct_len);
    h2o_start_response(req, &generator);
    h2o_send(req, &body, 1, H2O_SEND_STATE_FINAL);
    return 0;
}

#define CT_TEXT "text/plain"
#define CT_JSON "application/json"

/* ---- handlers ---- */

static int on_root(h2o_handler_t *self, h2o_req_t *req)
{
    if (!h2o_memis(req->method.base, req->method.len, H2O_STRLIT("GET")))
        return -1;
    /* "/" is a prefix-match handler: it must only answer the bare root, not
     * every unmatched path. Anything longer falls through (return -1) so the
     * core emits 404 for unknown routes. */
    if (!h2o_memis(req->path_normalized.base, req->path_normalized.len, H2O_STRLIT("/")))
        return -1;
    return send_static(req, h2o_iovec_init(H2O_STRLIT("Hello, World!")), H2O_STRLIT(CT_TEXT));
}

static int on_json(h2o_handler_t *self, h2o_req_t *req)
{
    if (!h2o_memis(req->method.base, req->method.len, H2O_STRLIT("GET")))
        return -1;
    return send_static(req, h2o_iovec_init(H2O_STRLIT("{\"message\":\"Hello, World!\"}")),
                       H2O_STRLIT(CT_JSON));
}

static int on_json_1k(h2o_handler_t *self, h2o_req_t *req)
{
    if (!h2o_memis(req->method.base, req->method.len, H2O_STRLIT("GET")))
        return -1;
    return send_static(req, json1k, H2O_STRLIT(CT_JSON));
}

static int on_json_8k(h2o_handler_t *self, h2o_req_t *req)
{
    if (!h2o_memis(req->method.base, req->method.len, H2O_STRLIT("GET")))
        return -1;
    return send_static(req, json8k, H2O_STRLIT(CT_JSON));
}

static int on_json_16k(h2o_handler_t *self, h2o_req_t *req)
{
    if (!h2o_memis(req->method.base, req->method.len, H2O_STRLIT("GET")))
        return -1;
    return send_static(req, json16k, H2O_STRLIT(CT_JSON));
}

static int on_json_64k(h2o_handler_t *self, h2o_req_t *req)
{
    if (!h2o_memis(req->method.base, req->method.len, H2O_STRLIT("GET")))
        return -1;
    return send_static(req, json64k, H2O_STRLIT(CT_JSON));
}

/* /users/:id — h2o has no built-in path-param router; a path handler matches
 * by prefix. We register this handler on "/users/" and slice the id out of
 * the normalized path (everything after the prefix). The body is built in the
 * request pool (request-lifetime) and duplicated by h2o_send. */
static int on_users(h2o_handler_t *self, h2o_req_t *req)
{
    static const char prefix[] = "/users/";
    const size_t prefix_len = sizeof(prefix) - 1;

    if (!h2o_memis(req->method.base, req->method.len, H2O_STRLIT("GET")))
        return -1;
    if (req->path_normalized.len < prefix_len)
        return -1;

    const char *id = req->path_normalized.base + prefix_len;
    size_t id_len = req->path_normalized.len - prefix_len;

    static const char body_prefix[] = "User ID: ";
    const size_t body_prefix_len = sizeof(body_prefix) - 1;

    h2o_iovec_t body;
    body.len = body_prefix_len + id_len;
    body.base = h2o_mem_alloc_pool(&req->pool, char, body.len);
    memcpy(body.base, body_prefix, body_prefix_len);
    memcpy(body.base + body_prefix_len, id, id_len);

    static h2o_generator_t generator = {NULL, NULL};
    req->res.status = 200;
    req->res.reason = "OK";
    h2o_add_header(&req->pool, &req->res.headers, H2O_TOKEN_CONTENT_TYPE, NULL, H2O_STRLIT(CT_TEXT));
    h2o_start_response(req, &generator);
    h2o_send(req, &body, 1, H2O_SEND_STATE_FINAL);
    return 0;
}

/* /upload — read-and-discard the body, reply "OK". h2o buffers the full entity
 * before invoking the handler (req->entity is the whole body; base == NULL if
 * none), so the parse cost is already on the measured path — we just touch it
 * and reply. */
static int on_upload(h2o_handler_t *self, h2o_req_t *req)
{
    if (!h2o_memis(req->method.base, req->method.len, H2O_STRLIT("POST")))
        return -1;
    /* Discard: a volatile read keeps the buffered body on the measured path
     * without the compiler eliding it. */
    if (req->entity.base != NULL) {
        volatile char sink = 0;
        for (size_t i = 0; i < req->entity.len; ++i)
            sink ^= req->entity.base[i];
        (void)sink;
    }
    return send_static(req, h2o_iovec_init(H2O_STRLIT("OK")), H2O_STRLIT(CT_TEXT));
}

static void register_handler(h2o_hostconf_t *hostconf, const char *path,
                             int (*on_req)(h2o_handler_t *, h2o_req_t *))
{
    h2o_pathconf_t *pathconf = h2o_config_register_path(hostconf, path, 0);
    h2o_handler_t *handler = h2o_create_handler(pathconf, sizeof(*handler));
    handler->on_req = on_req;
}

/* ---- listener ---- */

/* create_listener binds host:port, reads the actually-bound address back
 * (handles port 0), prints the ready line, and wires the fd into the evloop.
 * Returns 0 on success, -1 on failure. */
static int create_listener(const char *host, uint16_t port, char *bound_out, size_t bound_cap)
{
    struct sockaddr_in addr;
    int fd, reuseaddr_flag = 1;
    h2o_socket_t *sock;

    memset(&addr, 0, sizeof(addr));
    addr.sin_family = AF_INET;
    addr.sin_port = htons(port);
    if (host == NULL || host[0] == '\0') {
        addr.sin_addr.s_addr = htonl(INADDR_ANY);
    } else if (inet_pton(AF_INET, host, &addr.sin_addr) != 1) {
        fprintf(stderr, "h2o: bad -bind host %s (IPv4 literal expected)\n", host);
        return -1;
    }

    if ((fd = socket(AF_INET, SOCK_STREAM, 0)) == -1) {
        perror("h2o: socket");
        return -1;
    }
    if (setsockopt(fd, SOL_SOCKET, SO_REUSEADDR, &reuseaddr_flag, sizeof(reuseaddr_flag)) != 0) {
        perror("h2o: setsockopt(SO_REUSEADDR)");
        close(fd);
        return -1;
    }
    /* SO_REUSEPORT lets every forked worker bind the SAME host:port with its
     * own listen fd; the kernel load-balances incoming connections across
     * them. This is what scales the single-evloop-per-process model to all
     * cores (libh2o has no in-process multi-loop accept sharing). */
    if (setsockopt(fd, SOL_SOCKET, SO_REUSEPORT, &reuseaddr_flag, sizeof(reuseaddr_flag)) != 0) {
        perror("h2o: setsockopt(SO_REUSEPORT)");
        close(fd);
        return -1;
    }
    if (bind(fd, (struct sockaddr *)&addr, sizeof(addr)) != 0) {
        perror("h2o: bind");
        close(fd);
        return -1;
    }
    if (listen(fd, SOMAXCONN) != 0) {
        perror("h2o: listen");
        close(fd);
        return -1;
    }

    /* Read back the bound address so a `host:0` bind reports the
     * kernel-assigned port on the ready line. */
    struct sockaddr_in bound;
    socklen_t bound_len = sizeof(bound);
    if (getsockname(fd, (struct sockaddr *)&bound, &bound_len) != 0) {
        perror("h2o: getsockname");
        close(fd);
        return -1;
    }
    char ip[INET_ADDRSTRLEN];
    inet_ntop(AF_INET, &bound.sin_addr, ip, sizeof(ip));
    snprintf(bound_out, bound_cap, "%s:%u", ip, (unsigned)ntohs(bound.sin_port));

    sock = h2o_evloop_socket_create(ctx.loop, fd, H2O_SOCKET_FLAG_DONT_READ);
    h2o_socket_read_start(sock, on_accept);
    return 0;
}

static void on_accept(h2o_socket_t *listener, const char *err)
{
    h2o_socket_t *sock;
    if (err != NULL)
        return;
    if ((sock = h2o_evloop_socket_accept(listener)) == NULL)
        return;
    h2o_accept(&accept_ctx, sock);
}

/* ---- lifecycle ---- */

static volatile sig_atomic_t shutdown_requested = 0;

static void on_signal(int signo)
{
    (void)signo;
    shutdown_requested = 1;
}

/* parse host:port. host may be empty ("" -> INADDR_ANY). Returns 0 on success.
 * IPv6-bracketed forms are rejected (the bench dials IPv4 literals). */
static int parse_bind(const char *bind, char *host_out, size_t host_cap, uint16_t *port_out)
{
    const char *colon = strrchr(bind, ':');
    if (colon == NULL) {
        fprintf(stderr, "h2o: bad -bind %s (want host:port)\n", bind);
        return -1;
    }
    size_t host_len = (size_t)(colon - bind);
    if (host_len >= host_cap) {
        fprintf(stderr, "h2o: -bind host too long\n");
        return -1;
    }
    memcpy(host_out, bind, host_len);
    host_out[host_len] = '\0';

    char *end = NULL;
    long port = strtol(colon + 1, &end, 10);
    if (end == colon + 1 || *end != '\0' || port < 0 || port > 65535) {
        fprintf(stderr, "h2o: bad -bind port %s\n", colon + 1);
        return -1;
    }
    *port_out = (uint16_t)port;
    return 0;
}

/* run_worker sets up an independent h2o context + evloop + SO_REUSEPORT
 * listener and serves until SIGTERM flips shutdown_requested. Called once per
 * forked worker (and by the parent). config/handlers are shared read-only;
 * the loop + listener fd are private to this process. */
static int run_worker(const char *host, uint16_t port)
{
    char bound[INET_ADDRSTRLEN + 8];

    h2o_context_init(&ctx, h2o_evloop_create(), &config);
    accept_ctx.ctx = &ctx;
    accept_ctx.hosts = config.hosts;

    if (create_listener(host, port, bound, sizeof(bound)) != 0)
        return -1;

    while (!shutdown_requested) {
        if (h2o_evloop_run(ctx.loop, 100) != 0)
            break;
    }
    return 0;
}

/* resolve_listen_port turns a (possibly :0) request into a concrete port + a
 * "host:port" string for the ready line WITHOUT holding a socket open. For an
 * explicit port we format directly (no bind → no race). For port 0 we temp-bind
 * with SO_REUSEPORT to learn the kernel-assigned port, then close; the workers
 * re-bind it with SO_REUSEPORT. */
static int resolve_listen_port(const char *host, uint16_t want_port,
                               uint16_t *out_port, char *bound_out, size_t bound_cap)
{
    if (want_port != 0) {
        *out_port = want_port;
        snprintf(bound_out, bound_cap, "%s:%u",
                 (host && host[0]) ? host : "0.0.0.0", (unsigned)want_port);
        return 0;
    }

    struct sockaddr_in addr;
    int fd, one = 1;
    memset(&addr, 0, sizeof(addr));
    addr.sin_family = AF_INET;
    addr.sin_port = 0;
    if (host == NULL || host[0] == '\0') {
        addr.sin_addr.s_addr = htonl(INADDR_ANY);
    } else if (inet_pton(AF_INET, host, &addr.sin_addr) != 1) {
        fprintf(stderr, "h2o: bad -bind host %s (IPv4 literal expected)\n", host);
        return -1;
    }
    if ((fd = socket(AF_INET, SOCK_STREAM, 0)) == -1) {
        perror("h2o: socket");
        return -1;
    }
    setsockopt(fd, SOL_SOCKET, SO_REUSEADDR, &one, sizeof(one));
    setsockopt(fd, SOL_SOCKET, SO_REUSEPORT, &one, sizeof(one));
    if (bind(fd, (struct sockaddr *)&addr, sizeof(addr)) != 0) {
        perror("h2o: bind");
        close(fd);
        return -1;
    }
    struct sockaddr_in b;
    socklen_t bl = sizeof(b);
    if (getsockname(fd, (struct sockaddr *)&b, &bl) != 0) {
        perror("h2o: getsockname");
        close(fd);
        return -1;
    }
    char ip[INET_ADDRSTRLEN];
    inet_ntop(AF_INET, &b.sin_addr, ip, sizeof(ip));
    *out_port = ntohs(b.sin_port);
    snprintf(bound_out, bound_cap, "%s:%u", ip, (unsigned)*out_port);
    close(fd);
    return 0;
}

int main(int argc, char **argv)
{
    const char *bind = "127.0.0.1:8080";
    const char *engine = "h1";

    for (int i = 1; i < argc; ++i) {
        if ((strcmp(argv[i], "-bind") == 0 || strcmp(argv[i], "--bind") == 0) && i + 1 < argc) {
            bind = argv[++i];
        } else if (strncmp(argv[i], "-bind=", 6) == 0) {
            bind = argv[i] + 6;
        } else if (strncmp(argv[i], "--bind=", 7) == 0) {
            bind = argv[i] + 7;
        } else if ((strcmp(argv[i], "-engine") == 0 || strcmp(argv[i], "--engine") == 0) && i + 1 < argc) {
            engine = argv[++i];
        } else if (strncmp(argv[i], "-engine=", 8) == 0) {
            engine = argv[i] + 8;
        } else if (strncmp(argv[i], "--engine=", 9) == 0) {
            engine = argv[i] + 9;
        }
    }

    /* Wire-protocol selection mirrors the drogon adapter. h2o's cleartext
     * accept path serves h1 AND h2c prior-knowledge on one socket and cannot
     * refuse h1, so a strict h2c-noupg column is impossible here: fail fast on
     * -engine h2c rather than report h1 numbers under an h2c label. */
    if (strcmp(engine, "h2c") == 0) {
        fprintf(stderr,
                "h2o: h2c not served as a distinct column — h2o's cleartext "
                "listener speaks both HTTP/1.1 and h2c prior-knowledge on the "
                "same socket and cannot refuse h1, so it cannot satisfy the "
                "strict h2c-noupg contract; refusing to serve h1 under an h2c "
                "label\n");
        return 2;
    }
    if (strcmp(engine, "h1") != 0) {
        fprintf(stderr, "h2o: unknown -engine %s (want h1 or h2c)\n", engine);
        return 2;
    }

    char host[256];
    uint16_t port;
    if (parse_bind(bind, host, sizeof(host), &port) != 0)
        return 1;

    signal(SIGPIPE, SIG_IGN);
    signal(SIGTERM, on_signal);
    signal(SIGINT, on_signal);

    json1k = generate_json_payload(1024);
    json8k = generate_json_payload(8192);
    json16k = generate_json_payload(16384);
    json64k = generate_json_payload(65536);

    h2o_config_init(&config);
    h2o_hostconf_t *hostconf =
        h2o_config_register_host(&config, h2o_iovec_init(H2O_STRLIT("default")), 65535);

    /* Exact-path handlers first. h2o matches the LONGEST registered prefix, so
     * "/users/" and "/json-1k" win over "/" for their paths; "/" is guarded to
     * answer only the bare root (see on_root). */
    register_handler(hostconf, "/json-1k", on_json_1k);
    register_handler(hostconf, "/json-8k", on_json_8k);
    register_handler(hostconf, "/json-16k", on_json_16k);
    register_handler(hostconf, "/json-64k", on_json_64k);
    register_handler(hostconf, "/json", on_json);
    register_handler(hostconf, "/users/", on_users);
    register_handler(hostconf, "/upload", on_upload);
    register_handler(hostconf, "/", on_root);

    /* Resolve the concrete port ONCE (handles :0) so every worker binds the
     * same host:port via SO_REUSEPORT, then announce readiness once. The
     * runner's TCP probe waits for this exact line. */
    uint16_t bound_port;
    char bound[INET_ADDRSTRLEN + 8];
    if (resolve_listen_port(host, port, &bound_port, bound, sizeof(bound)) != 0) {
        fprintf(stderr, "h2o: failed to resolve listen port for %s\n", bind);
        return 1;
    }
    printf("ready addr=%s\n", bound);
    fflush(stdout);

    /* One worker per online CPU (capped). The parent forks nworkers-1 children
     * and then serves as a worker itself; each process owns an independent
     * evloop + SO_REUSEPORT listener, so the kernel spreads connections across
     * all cores. SIGTERM reaches the whole process group, draining every
     * worker. A worker that cannot bind exits non-zero; the rest keep serving. */
    long nworkers = sysconf(_SC_NPROCESSORS_ONLN);
    if (nworkers < 1)
        nworkers = 1;
    if (nworkers > 256)
        nworkers = 256;
    for (long i = 1; i < nworkers; ++i) {
        pid_t pid = fork();
        if (pid == 0)
            return run_worker(host, bound_port) == 0 ? 0 : 1; /* child */
        if (pid < 0)
            perror("h2o: fork");                              /* fewer workers, continue */
    }
    return run_worker(host, bound_port) == 0 ? 0 : 1;         /* parent serves too */
}
