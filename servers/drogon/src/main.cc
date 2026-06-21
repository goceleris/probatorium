// probatorium drogon adapter — wave 4 (C++).
//
// Same contract as servers/axum, servers/ntex, served
// by drogon's HTTP stack. Lifecycle and CLI match the Rust competitors so
// servers.StartAdapter can launch every native adapter with one
// invocation pattern (`{bin} -bind {addr}`, wait for `ready addr=<addr>`).
//
// Endpoint set (see servers/common/contract.go for canonical bytes):
//   GET  /            -> "Hello, World!"  text/plain
//   GET  /json        -> {"message":"Hello, World!"}  application/json
//   GET  /json-1k     -> deterministic 1026-byte JSON page
//   GET  /json-8k     -> deterministic 8286-byte JSON page
//   GET  /json-16k    -> deterministic 16463-byte JSON page
//   GET  /json-64k    -> deterministic 65618-byte JSON page
//   GET  /users/{id}  -> "User ID: <id>"  text/plain
//   POST /upload      -> read-and-discard body, reply "OK"  text/plain
//
// Driver-backed (/db, /cache, /mc, /session) and chain-* endpoints are
// out of scope (Capabilities all-false: static + concurrency scenarios
// only), so they intentionally 404 — the scenario applicability filter in
// servers/servers.go skips drogon for those classes.
//
// CLI: `{bin} -bind <addr> [-engine h1|h2c]`. -engine selects the wire
// protocol: "h1" (default) serves plain HTTP/1.1; "h2c" would serve
// HTTP/2 cleartext prior-knowledge, but drogon does not support it (see
// the -engine handling in main() for the API investigation) so it
// fails fast with a non-zero exit, letting the bench's bind-gate record
// the drogon-h2 column as not-applicable/DNF instead of silently
// serving h1.

#include <drogon/drogon.h>
#include <drogon/version.h>

#include <cstdint>
#include <cstdio>
#include <string>

using drogon::HttpRequestPtr;
using drogon::HttpResponse;
using drogon::HttpResponsePtr;

namespace {

// Byte-identical port of servers/common/payload.go generateJSONPayload.
// The Go reference marshals a (paginatedResponse, paginatedItem) struct
// pair with encoding/json, which emits compact JSON in struct-declaration
// field order. We emit the bytes by hand so the response is byte-for-byte
// equal to the Go generator (the conformance probe compares exact bytes).
//
// Termination rule mirrors Go: append items until the full marshalled
// length (header + items + footer) crosses targetSize. Resulting sizes:
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

HttpResponsePtr makeResponse(std::string body, drogon::ContentType ct) {
    auto resp = HttpResponse::newHttpResponse();
    resp->setStatusCode(drogon::k200OK);
    resp->setContentTypeCode(ct);
    resp->setBody(std::move(body));
    return resp;
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

    // Wire-protocol selection. The bench launches
    //   ./drogon -bind <addr> -engine <value>
    // and gates "up" on the `ready addr=<addr>` stdout line. Anything other
    // than a successful bind+ready must exit non-zero so the bind-gate fails
    // fast rather than recording a mislabelled result.
    //
    //   h1  (or absent) -> plain HTTP/1.1, exactly as before.
    //   h2c             -> HTTP/2 cleartext prior-knowledge — NOT supported.
    //
    // h2c investigation (drogon 1.9.13, the version the cpp role installs):
    // drogon has no server-side HTTP/2 of any kind, cleartext or TLS. The
    // public API was inspected directly:
    //   * <drogon/HttpTypes.h> `enum class Version` contains only kHttp10 and
    //     kHttp11 — there is no kHttp2, so the request parser / response
    //     serializer cannot speak the HTTP/2 framing layer at all.
    //   * <drogon/HttpAppFramework.h> addListener() exposes only
    //     useSSL/certFile/keyFile/useOldTLS/sslConfCmds — no protocol toggle,
    //     no h2c / prior-knowledge / "enableH2" listener flag, and there is no
    //     app-level enableHttp2()/setHttp2()-style method anywhere in the
    //     header. enableServerHeader()/setServerHeaderField() only touch the
    //     `Server:` response header, not the protocol.
    //   * The only HTTP/2-adjacent surface in the whole include tree is
    //     <trantor/net/TLSPolicy.h> setAlpnProtocols(), i.e. TLS/ALPN
    //     negotiation — TLS-oriented and irrelevant to cleartext h2c
    //     prior-knowledge (which by definition skips ALPN and TLS entirely).
    // Conclusion: this drogon build cannot serve h2c prior-knowledge, so we
    // fail fast instead of silently downgrading to h1 (which would corrupt
    // the drogon-h2 column by reporting h1 numbers under an h2 label).
    if (engine == "h2c") {
        std::fprintf(stderr,
                     "drogon: h2c not supported by this drogon build "
                     "(drogon %s exposes no server-side HTTP/2; "
                     "Version enum is kHttp10/kHttp11 only, addListener has no "
                     "h2c flag) — refusing to serve h1 under an h2c label\n",
                     DROGON_VERSION);
        return 2;
    }
    if (engine != "h1") {
        std::fprintf(stderr,
                     "drogon: unknown -engine value %s (want h1 or h2c)\n",
                     engine.c_str());
        return 2;
    }

    const std::string json1k = generateJSONPayload(1024);
    const std::string json8k = generateJSONPayload(8192);
    const std::string json16k = generateJSONPayload(16384);
    const std::string json64k = generateJSONPayload(65536);

    std::string host = bind;
    std::uint16_t port = 8080;
    const auto colon = bind.find_last_of(':');
    if (colon != std::string::npos) {
        host = bind.substr(0, colon);
        port = static_cast<std::uint16_t>(std::stoul(bind.substr(colon + 1)));
    }
    if (host.empty()) {
        host = "0.0.0.0";
    }

    auto &app = drogon::app();

    app.registerHandler(
        "/",
        [](const HttpRequestPtr &,
           std::function<void(const HttpResponsePtr &)> &&callback) {
            callback(makeResponse("Hello, World!", drogon::CT_TEXT_PLAIN));
        },
        {drogon::Get});

    app.registerHandler(
        "/json",
        [](const HttpRequestPtr &,
           std::function<void(const HttpResponsePtr &)> &&callback) {
            callback(makeResponse(R"({"message":"Hello, World!"})",
                                  drogon::CT_APPLICATION_JSON));
        },
        {drogon::Get});

    app.registerHandler(
        "/json-1k",
        [&json1k](const HttpRequestPtr &,
                  std::function<void(const HttpResponsePtr &)> &&callback) {
            callback(makeResponse(json1k, drogon::CT_APPLICATION_JSON));
        },
        {drogon::Get});

    app.registerHandler(
        "/json-8k",
        [&json8k](const HttpRequestPtr &,
                  std::function<void(const HttpResponsePtr &)> &&callback) {
            callback(makeResponse(json8k, drogon::CT_APPLICATION_JSON));
        },
        {drogon::Get});

    app.registerHandler(
        "/json-16k",
        [&json16k](const HttpRequestPtr &,
                   std::function<void(const HttpResponsePtr &)> &&callback) {
            callback(makeResponse(json16k, drogon::CT_APPLICATION_JSON));
        },
        {drogon::Get});

    app.registerHandler(
        "/json-64k",
        [&json64k](const HttpRequestPtr &,
                   std::function<void(const HttpResponsePtr &)> &&callback) {
            callback(makeResponse(json64k, drogon::CT_APPLICATION_JSON));
        },
        {drogon::Get});

    app.registerHandler(
        "/users/{id}",
        [](const HttpRequestPtr &,
           std::function<void(const HttpResponsePtr &)> &&callback,
           const std::string &id) {
            callback(makeResponse("User ID: " + id, drogon::CT_TEXT_PLAIN));
        },
        {drogon::Get});

    app.registerHandler(
        "/upload",
        [](const HttpRequestPtr &,
           std::function<void(const HttpResponsePtr &)> &&callback) {
            // Body is read-and-discarded; drogon has already buffered it by
            // the time the handler fires, so the parse cost is part of the
            // measured path. The contract reply is the literal "OK".
            callback(makeResponse("OK", drogon::CT_TEXT_PLAIN));
        },
        {drogon::Post});

    app.setThreadNum(0);  // one IO loop per hardware core
    app.disableSession();
    app.setLogLevel(trantor::Logger::kError);
    app.addListener(host, port);

    // Beginning advice fires after listeners are bound and just before the
    // server starts accepting, so the ready line the runner waits for is
    // only emitted once the socket is live.
    app.registerBeginningAdvice([&bind]() {
        std::printf("ready addr=%s\n", bind.c_str());
        std::fflush(stdout);
    });

    app.run();
    return 0;
}
