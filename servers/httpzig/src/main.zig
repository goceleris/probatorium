// probatorium httpzig adapter — the Zig event-loop competitor column,
// rebuilt on karlseguin/http.zig after the std.http.Server entrant
// (servers/zig_zap) was retired for choking at 64+ concurrent dials.
//
// Why http.zig and not std.http.Server: Zig 0.16's IpAddress.listen has
// no usable SO_REUSEPORT fan-out for a worker pool, so the old adapter ran
// one shared listener accepted on under a mutex — fine at 8/16/32 conns,
// deadlocked at the bench's default 64. http.zig instead spins one worker
// thread per CPU, each binding the *same* address with SO_REUSEPORT_LB /
// SO_REUSEPORT (see its listen()), so the kernel load-balances accepts
// across cores with no userspace queue — the same architecture as
// celeris's epoll / iouring engines and the Rust/Go/.NET multi-listener
// competitors.
//
// Serves the canonical contract endpoints declared in
// servers/common/contract.go:
//
//   GET  /            -> "Hello, World!"            text/plain
//   GET  /json        -> {"message":"Hello, World!"} application/json
//   GET  /json-1k     -> deterministic JSON page (>= 1 KiB)
//   GET  /json-8k     -> deterministic JSON page (>= 8 KiB)
//   GET  /json-16k    -> deterministic JSON page (>= 16 KiB)
//   GET  /json-64k    -> deterministic JSON page (>= 64 KiB)
//   GET  /users/:id   -> "User ID: <id>"            text/plain
//   POST /upload      -> read-and-discard body, reply "OK"  text/plain
//
// Driver-backed (/db, /cache, /mc, /session) and chain-* endpoints are not
// served — this column declares Capabilities{Static: true} only, so the
// scenario applicability filter (servers/servers.go) never schedules them
// against this adapter.
//
// CLI:
//   -bind <host:port>  default 127.0.0.1:8080. Pass `:0` (or any `host:0`)
//                      to let the kernel allocate a port; the concrete
//                      bound address is reported on stdout via the
//                      `ready addr=<addr>` line the runner waits for before
//                      opening loadgen.
//   -engine <h1|h2c>   accepted for CLI parity with the registry. http.zig
//                      is HTTP/1.1-only, so anything other than h1 is
//                      rejected (the registry registers no httpzig-h2
//                      column, so this is never exercised in practice).
//
// Zig 0.16 entry point: main receives a std.process.Init — `init.gpa` is
// the allocator, `init.io` the blocking Io, `init.minimal.args` the argv
// iterator. http.zig's Server.init takes (io, allocator, config, handler).

const std = @import("std");
const httpz = @import("httpz");
const net = std.Io.net;
const Io = std.Io;
const payload = @import("payload.zig");

// Static contract bodies. The JSON payloads are built once at startup into
// process-global slices shared read-only across every worker thread.
const hello_body = "Hello, World!";
const json_hello = "{\"message\":\"Hello, World!\"}";
const upload_body = "OK";

const ct_text = "text/plain";
const ct_json = "application/json";

var json_1k: []const u8 = undefined;
var json_8k: []const u8 = undefined;
var json_16k: []const u8 = undefined;
var json_64k: []const u8 = undefined;

// Server pointer parked for the signal handler so SIGINT/SIGTERM can call
// stop(), unblocking listen() for a graceful drain. Default SIGTERM would
// terminate the process anyway (the runner SIGTERMs the process group),
// but an explicit stop() lets in-flight responses finish first.
const Server = httpz.Server(void);
var gserver: ?*Server = null;

fn onSignal(_: std.posix.SIG) callconv(.c) void {
    if (gserver) |s| s.stop();
}

// ---- handlers -------------------------------------------------------------

// Each static handler writes a fixed body + bare Content-Type. We set the
// header explicitly (not res.content_type) so the wire value is exactly
// "text/plain" / "application/json" — byte-identical to the Go adapters —
// rather than http.zig's enum form ("text/plain; charset=UTF-8").

fn root(_: *httpz.Request, res: *httpz.Response) !void {
    res.header("Content-Type", ct_text);
    res.body = hello_body;
}

fn jsonHello(_: *httpz.Request, res: *httpz.Response) !void {
    res.header("Content-Type", ct_json);
    res.body = json_hello;
}

fn json1k(_: *httpz.Request, res: *httpz.Response) !void {
    res.header("Content-Type", ct_json);
    res.body = json_1k;
}

fn json8k(_: *httpz.Request, res: *httpz.Response) !void {
    res.header("Content-Type", ct_json);
    res.body = json_8k;
}

fn json16k(_: *httpz.Request, res: *httpz.Response) !void {
    res.header("Content-Type", ct_json);
    res.body = json_16k;
}

fn json64k(_: *httpz.Request, res: *httpz.Response) !void {
    res.header("Content-Type", ct_json);
    res.body = json_64k;
}

// users echoes the :id path param. The formatted body is allocated on
// res.arena (reset per request) so it outlives the handler return — a
// stack buffer would dangle when http.zig serializes the response after
// the handler completes.
fn users(req: *httpz.Request, res: *httpz.Response) !void {
    const id = req.param("id") orelse "";
    res.header("Content-Type", ct_text);
    res.body = try std.fmt.allocPrint(res.arena, "User ID: {s}", .{id});
}

// upload reads-and-discards the request body (req.body() consumes it) and
// replies with the literal "OK" the contract demands.
fn upload(req: *httpz.Request, res: *httpz.Response) !void {
    _ = req.body();
    res.header("Content-Type", ct_text);
    res.body = upload_body;
}

// ---- bind helpers ---------------------------------------------------------

// parseBind walks argv (Go-flag style `-bind <value>`, matching every other
// adapter) and returns the bind string. The iterator's byte slices point
// into argv memory that lives for the whole process, so no copy is needed.
fn parseBind(args: std.process.Args) []const u8 {
    var it = std.process.Args.Iterator.init(args);
    _ = it.next(); // argv[0]
    while (it.next()) |arg| {
        if (eql(arg, "-bind") or eql(arg, "--bind")) {
            if (it.next()) |v| return v;
        }
        if (std.mem.startsWith(u8, arg, "-bind=")) return arg["-bind=".len..];
        if (std.mem.startsWith(u8, arg, "--bind=")) return arg["--bind=".len..];
    }
    return "127.0.0.1:8080";
}

// parseEngine returns the -engine value (default "h1"). http.zig is
// HTTP/1.1-only; main rejects anything else.
fn parseEngine(args: std.process.Args) []const u8 {
    var it = std.process.Args.Iterator.init(args);
    _ = it.next();
    while (it.next()) |arg| {
        if (eql(arg, "-engine") or eql(arg, "--engine")) {
            if (it.next()) |v| return v;
        }
        if (std.mem.startsWith(u8, arg, "-engine=")) return arg["-engine=".len..];
        if (std.mem.startsWith(u8, arg, "--engine=")) return arg["--engine=".len..];
    }
    return "h1";
}

fn eql(a: []const u8, b: []const u8) bool {
    return std.mem.eql(u8, a, b);
}

// resolvePort binds a throwaway listener on host:port and reads back the
// concrete local port via getsockname, resolving the `:0` (kernel-assigned)
// case. The probe socket is closed immediately; http.zig then re-binds the
// same concrete port (both sockets set SO_REUSEADDR, so the rebind window
// is harmless). This mirrors servers/zig_zap's boundPort approach because
// http.zig keeps its listener fd private and only binds inside the blocking
// listen() call — so we cannot read the port off http.zig itself before
// the ready line must be printed.
fn resolvePort(io: Io, host: []const u8, port: u16) !u16 {
    if (port != 0) return port;
    const addr = try net.IpAddress.parse(host, 0);
    var probe = try net.IpAddress.listen(&addr, io, .{ .reuse_address = true });
    defer probe.deinit(io);
    return boundPort(probe.socket.handle);
}

// boundPort reads the concrete local port off a bound socket via
// getsockname. posix.system.getsockname is the raw syscall wrapper —
// std.posix.getsockname does not exist in this toolchain. Returns 0 on
// failure.
fn boundPort(handle: net.Socket.Handle) u16 {
    var storage: std.posix.sockaddr.storage = undefined;
    var len: std.posix.socklen_t = @sizeOf(std.posix.sockaddr.storage);
    if (std.posix.system.getsockname(handle, @ptrCast(&storage), &len) != 0) return 0;
    const port_be: u16 = switch (storage.family) {
        std.posix.AF.INET => @as(*const std.posix.sockaddr.in, @ptrCast(@alignCast(&storage))).port,
        std.posix.AF.INET6 => @as(*const std.posix.sockaddr.in6, @ptrCast(@alignCast(&storage))).port,
        else => return 0,
    };
    return std.mem.bigToNative(u16, port_be);
}

pub fn main(init: std.process.Init) !void {
    const alloc = init.gpa;
    const io = init.io;

    const bind = parseBind(init.minimal.args);
    const engine = parseEngine(init.minimal.args);
    if (!eql(engine, "h1")) {
        std.debug.print("httpzig: unsupported -engine {s} (HTTP/1.1 only)\n", .{engine});
        return error.UnsupportedEngine;
    }

    json_1k = try payload.generate(alloc, 1024);
    json_8k = try payload.generate(alloc, 8192);
    json_16k = try payload.generate(alloc, 16384);
    json_64k = try payload.generate(alloc, 65536);

    // IpAddress.parse takes host and port separately, so split the bind
    // string on its last colon. A `:0` port is resolved to a concrete
    // kernel-assigned port before http.zig binds.
    const colon = std.mem.lastIndexOfScalar(u8, bind, ':') orelse return error.InvalidBind;
    const host = bind[0..colon];
    const req_port = try std.fmt.parseInt(u16, bind[colon + 1 ..], 10);
    const port = try resolvePort(io, host, req_port);
    const ip = try net.IpAddress.parse(host, port);

    // One worker per CPU so http.zig fans accepts out across cores via
    // SO_REUSEPORT_LB / SO_REUSEPORT — the whole reason this adapter
    // replaces the single-listener std.http entrant.
    const cpus: u16 = @intCast(std.Thread.getCpuCount() catch 1);

    var server = try Server.init(io, alloc, .{
        .address = .{ .ip = ip },
        .workers = .{ .count = cpus },
    }, {});
    defer {
        server.stop();
        server.deinit();
    }
    gserver = &server;

    var router = try server.router(.{});
    router.get("/", root, .{});
    router.get("/json", jsonHello, .{});
    router.get("/json-1k", json1k, .{});
    router.get("/json-8k", json8k, .{});
    router.get("/json-16k", json16k, .{});
    router.get("/json-64k", json64k, .{});
    router.get("/users/:id", users, .{});
    router.post("/upload", upload, .{});

    // Graceful shutdown: SIGINT/SIGTERM -> server.stop() -> listen() returns.
    installSignals();

    // Report the bound port (resolving the :0 case) on the ready line
    // before listen() blocks. The runner's TCP probe waits for the addr to
    // answer; the conformance harness waits for this exact line.
    var out_buf: [128]u8 = undefined;
    const msg = try std.fmt.bufPrint(&out_buf, "ready addr={s}:{d}\n", .{ host, port });
    var stdout_buf: [128]u8 = undefined;
    var stdout = Io.File.stdout().writer(io, &stdout_buf);
    try stdout.interface.writeAll(msg);
    try stdout.interface.flush();

    try server.listen(); // blocks until stop()
}

fn installSignals() void {
    var act = std.posix.Sigaction{
        .handler = .{ .handler = onSignal },
        .mask = std.posix.sigemptyset(),
        .flags = 0,
    };
    std.posix.sigaction(std.posix.SIG.INT, &act, null);
    std.posix.sigaction(std.posix.SIG.TERM, &act, null);
}
