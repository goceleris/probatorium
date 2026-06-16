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
//   GET  /json-64k    -> deterministic 65618-byte JSON page
//   GET  /users/{id}  -> "User ID: <id>"  text/plain
//   POST /upload      -> read-and-discard body, reply "OK"  text/plain
//
// Driver-backed (/db, /cache, /mc, /session) and chain-* endpoints are
// out of scope (Capabilities all-false: static + concurrency scenarios
// only), so they intentionally 404 — the scenario applicability filter in
// servers/servers.go skips drogon for those classes.

#include <drogon/drogon.h>

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
    for (int i = 1; i < argc; ++i) {
        const std::string arg = argv[i];
        if ((arg == "-bind" || arg == "--bind") && i + 1 < argc) {
            bind = argv[++i];
        } else if (arg.rfind("-bind=", 0) == 0) {
            bind = arg.substr(6);
        } else if (arg.rfind("--bind=", 0) == 0) {
            bind = arg.substr(7);
        }
    }

    const std::string json1k = generateJSONPayload(1024);
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
