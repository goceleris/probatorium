// probatorium lithium adapter — C++ (matt-42/lithium, the TechEmpower C++
// speed leader).
//
// Same contract as servers/drogon, servers/axum, servers/ntex, served by
// lithium's HTTP stack. Lifecycle and CLI match the other native
// competitors so servers.StartAdapter launches every native adapter with
// one invocation pattern (`{bin} -bind {addr}`, wait for `ready
// addr=<addr>` on stdout, SIGTERM → graceful drain).
//
// Endpoint set (see servers/common/contract.go for canonical bytes):
//   GET  /            -> "Hello, World!"  text/plain
//   GET  /json        -> {"message":"Hello, World!"}  application/json
//   GET  /json-1k     -> deterministic 1026-byte JSON page
//   GET  /json-8k     -> deterministic 8286-byte JSON page
//   GET  /json-16k    -> deterministic 16463-byte JSON page
//   GET  /json-64k    -> deterministic 65618-byte JSON page
//   GET  /users/{{id}}-> "User ID: <id>"  text/plain
//   POST /upload      -> read-and-discard body, reply "OK"  text/plain
//
// Driver-backed (/db, /cache, /mc, /session) and chain-* endpoints are out
// of scope (Capabilities all-false: static + concurrency scenarios only),
// so they intentionally 404 — the scenario applicability filter in
// servers/servers.go skips lithium for those classes.
//
// CLI: `{bin} -bind <addr> [-engine h1|h2c]`. -engine selects the wire
// protocol:
//   h1  (or absent) -> plain HTTP/1.1, exactly as benched.
//   h2c             -> HTTP/2 cleartext prior-knowledge — NOT supported.
//
// h2c investigation (lithium master): lithium's http_server speaks HTTP/1.x
// only. Its request parser / response serializer (libraries/http_server/
// http_server/{http_ctx,http_top_header_builder}.hh) emit an HTTP/1.1
// status line + headers and parse an HTTP/1 request line; there is no
// HTTP/2 framing layer, no h2c upgrade path, and http_serve()'s only
// protocol-adjacent options are TLS (s::ssl_key / s::ssl_certificate /
// s::ssl_ciphers) — TLS, not h2c prior-knowledge. So we fail fast on -engine
// h2c instead of silently downgrading to h1 (which would corrupt a
// lithium-h2 column by reporting h1 numbers under an h2 label).
//
// Bind handling (important — two lithium quirks worked around here):
//
//   1. Port 0 (kernel-assigned). lithium's http_serve() binds the listen
//      socket inside a detached worker thread and never reports the actual
//      port, so it cannot serve the `ready addr=<host>:<realport>` contract
//      for `-bind host:0` on its own. We therefore reserve the port
//      ourselves: bind a throwaway socket to host:0, read the kernel-
//      assigned port back with getsockname(), close it, and hand the now-
//      concrete port to http_serve(). SO_REUSEADDR/SO_REUSEPORT (lithium
//      sets both on its listen socket) makes the immediate rebind safe.
//      This mirrors the reserve-then-pass pattern the Go conformance
//      harness uses for native adapters.
//
//   2. The s::ip explicit-bind byte-swap. lithium's create_and_bind() sets
//      `addr.sin_port = port` WITHOUT htons() on the explicit-IP path, so
//      passing s::ip = "127.0.0.1" mis-binds to the byte-swapped port. Its
//      default (no s::ip) path uses getaddrinfo(AI_PASSIVE) which sets the
//      port correctly and binds an IPv6 dual-stack socket (IPV6_V6ONLY=0)
//      that accepts IPv4 too. The probatorium runner always dials
//      127.0.0.1:<port>, which a dual-stack all-interfaces listener serves,
//      so we bind all interfaces (omit s::ip) to get a correct port. The
//      requested host is still echoed verbatim in the ready line so the
//      runner's dial target matches what it asked for.

#include "lithium_http_server.hh"

#include <arpa/inet.h>
#include <netinet/in.h>
#include <sys/socket.h>
#include <unistd.h>

#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <string>
#include <string_view>

namespace {

// Byte-identical port of servers/common/payload.go generateJSONPayload (the
// same hand emitter the drogon adapter uses, proven byte-for-byte equal to
// Go's encoding/json output for the paginatedResponse/paginatedItem structs).
// The Go reference marshals compact JSON in struct-declaration field order;
// we emit those bytes directly so the conformance probe's exact-byte compare
// passes.
//
// Termination rule mirrors Go: append items until the full marshalled length
// (header + items + footer) crosses targetSize. Resulting sizes:
//   1 KiB target  -> 1026 bytes
//   8 KiB target  -> 8286 bytes
//   16 KiB target -> 16463 bytes
//   64 KiB target -> 65618 bytes
std::string generateJSONPayload(std::size_t targetSize) {
    static const std::string header =
        R"({"page":1,"per_page":50,"total":1000,"total_pages":20,"data":[)";
    static const std::string footer = "]}";

    std::string buf;
    buf.reserve(targetSize + 256);
    buf += header;

    for (std::uint64_t i = 1;; ++i) {
        if (i > 1) {
            buf += ',';
        }
        const std::string n = std::to_string(i);
        buf += R"({"id":)";
        buf += n;
        buf += R"(,"name":"User )";
        buf += n;
        buf += R"(","email":"user)";
        buf += n;
        buf += R"(@example.com","status":"active","created_at":"2024-01-15T09:30:00Z"})";
        if (buf.size() + footer.size() >= targetSize) {
            break;
        }
    }
    buf += footer;
    return buf;
}

// reservePort binds a throwaway socket to host:wantPort and returns the
// actual port the kernel assigned (== wantPort when wantPort != 0). The
// socket is closed before returning; SO_REUSEADDR + SO_REUSEPORT let
// lithium rebind the same port immediately. Returns 0 on failure.
std::uint16_t reservePort(const std::string &host, std::uint16_t wantPort) {
    const int fd = ::socket(AF_INET, SOCK_STREAM, 0);
    if (fd < 0) {
        return 0;
    }
    int on = 1;
    ::setsockopt(fd, SOL_SOCKET, SO_REUSEADDR, &on, sizeof(on));
#ifdef SO_REUSEPORT
    ::setsockopt(fd, SOL_SOCKET, SO_REUSEPORT, &on, sizeof(on));
#endif

    struct sockaddr_in addr;
    std::memset(&addr, 0, sizeof(addr));
    addr.sin_family = AF_INET;
    addr.sin_port = htons(wantPort);
    const std::string bindHost =
        (host.empty() || host == "*") ? std::string("0.0.0.0") : host;
    if (::inet_pton(AF_INET, bindHost.c_str(), &addr.sin_addr) != 1) {
        // Non-numeric host (e.g. "localhost") — fall back to all interfaces;
        // the kernel-assigned port is what we need, the address is moot.
        addr.sin_addr.s_addr = htonl(INADDR_ANY);
    }

    if (::bind(fd, reinterpret_cast<struct sockaddr *>(&addr), sizeof(addr)) != 0) {
        ::close(fd);
        return 0;
    }

    struct sockaddr_in bound;
    socklen_t blen = sizeof(bound);
    std::uint16_t got = wantPort;
    if (::getsockname(fd, reinterpret_cast<struct sockaddr *>(&bound), &blen) == 0) {
        got = ntohs(bound.sin_port);
    }
    ::close(fd);
    return got;
}

}  // namespace

int main(int argc, char *argv[]) {
    std::string bind = "127.0.0.1:8080";
    std::string engine = "h1";
    for (int i = 1; i < argc; ++i) {
        const std::string arg = argv[i];
        if ((arg == "-bind" || arg == "--bind") && i + 1 < argc) {
            bind = argv[++i];
        } else if (arg.rfind("-bind=", 0) == 0) {
            bind = arg.substr(6);
        } else if (arg.rfind("--bind=", 0) == 0) {
            bind = arg.substr(7);
        } else if ((arg == "-engine" || arg == "--engine") && i + 1 < argc) {
            engine = argv[++i];
        } else if (arg.rfind("-engine=", 0) == 0) {
            engine = arg.substr(8);
        } else if (arg.rfind("--engine=", 0) == 0) {
            engine = arg.substr(9);
        }
    }

    if (engine == "h2c") {
        std::fprintf(stderr,
                     "lithium: h2c not supported (lithium's http_server speaks "
                     "HTTP/1.x only; no HTTP/2 framing or h2c upgrade path) — "
                     "refusing to serve h1 under an h2c label\n");
        return 2;
    }
    if (engine != "h1") {
        std::fprintf(stderr,
                     "lithium: unknown -engine value %s (want h1 or h2c)\n",
                     engine.c_str());
        return 2;
    }

    // Split host:port. host kept for the ready line; port resolved (incl. :0)
    // via reservePort below.
    std::string host = bind;
    std::uint16_t wantPort = 8080;
    const auto colon = bind.find_last_of(':');
    if (colon != std::string::npos) {
        host = bind.substr(0, colon);
        wantPort = static_cast<std::uint16_t>(std::stoul(bind.substr(colon + 1)));
    }
    if (host.empty()) {
        host = "0.0.0.0";
    }

    const std::uint16_t port = reservePort(host, wantPort);
    if (port == 0) {
        std::fprintf(stderr, "lithium: could not reserve %s:%u\n", host.c_str(),
                     wantPort);
        return 1;
    }

    const std::string json1k = generateJSONPayload(1024);
    const std::string json8k = generateJSONPayload(8192);
    const std::string json16k = generateJSONPayload(16384);
    const std::string json64k = generateJSONPayload(65536);

    li::http_api api;

    api.get("/") = [](li::http_request &, li::http_response &response) {
        response.set_header("Content-Type", "text/plain");
        response.write("Hello, World!");
    };

    api.get("/json") = [](li::http_request &, li::http_response &response) {
        response.set_header("Content-Type", "application/json");
        response.write(R"({"message":"Hello, World!"})");
    };

    api.get("/json-1k") = [&json1k](li::http_request &,
                                    li::http_response &response) {
        response.set_header("Content-Type", "application/json");
        response.write(std::string_view(json1k));
    };

    api.get("/json-8k") = [&json8k](li::http_request &,
                                    li::http_response &response) {
        response.set_header("Content-Type", "application/json");
        response.write(std::string_view(json8k));
    };

    api.get("/json-16k") = [&json16k](li::http_request &,
                                      li::http_response &response) {
        response.set_header("Content-Type", "application/json");
        response.write(std::string_view(json16k));
    };

    api.get("/json-64k") = [&json64k](li::http_request &,
                                      li::http_response &response) {
        response.set_header("Content-Type", "application/json");
        response.write(std::string_view(json64k));
    };

    api.get("/users/{{id}}") = [](li::http_request &request,
                                  li::http_response &response) {
        auto params = request.url_parameters(s::id = std::string());
        response.set_header("Content-Type", "text/plain");
        response.write("User ID: ", params.id);
    };

    api.post("/upload") = [](li::http_request &request,
                             li::http_response &response) {
        // Read-and-discard the body so the parse cost is on the measured
        // path, exactly as the contract specifies, then reply the literal
        // "OK". read_whole_body() returns a string_view into the already-
        // buffered request body.
        (void)request.http_ctx.read_whole_body();
        response.set_header("Content-Type", "text/plain");
        response.write("OK");
    };

    // Emit the ready line BEFORE http_serve() blocks. http_serve installs
    // lithium's own SIGINT/SIGTERM/SIGQUIT handlers (start_tcp_server →
    // shutdown_handler sets quit_signal_catched), so SIGTERM from
    // servers.spawn() drains gracefully — we deliberately do NOT install our
    // own handlers. The host echoed here is the requested host so the
    // runner's dial target matches; the port is the resolved (kernel-
    // assigned when :0) port. We bind all interfaces (no s::ip) to dodge
    // lithium's explicit-IP port byte-swap; an all-interfaces dual-stack
    // listener still serves the runner's 127.0.0.1 dial.
    std::printf("ready addr=%s:%u\n", host.c_str(), port);
    std::fflush(stdout);

    // One IO worker per hardware core (lithium's default), HTTP/1.1, all
    // interfaces. Blocks until a quit signal is caught.
    li::http_serve(api, port, s::nthreads = int(std::thread::hardware_concurrency()));
    return 0;
}
