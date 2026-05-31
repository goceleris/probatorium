// probatorium zig_zap adapter — the Zig event-loop competitor column.
//
// A direct architectural peer to celeris's iouring / epoll engines: a fixed
// pool of OS threads sharing one listener, each running its own blocking
// accept + HTTP/1.1 serve loop, so connections are load-balanced across cores
// with no userspace queue. The HTTP codec is the Zig standard library's
// std.http.Server (the one that ships with Zig 0.16); see NOTE below on why
// this adapter is not built on zigzap/zap.
//
// Serves the canonical contract endpoints declared in
// servers/common/contract.go:
//
//   GET  /            -> "Hello, World!"  text/plain
//   GET  /json        -> {"message":"Hello, World!"}  application/json
//   GET  /json-1k     -> deterministic 1026-byte JSON page
//   GET  /json-64k    -> deterministic 65618-byte JSON page
//   GET  /users/{id}  -> "User ID: <id>"  text/plain
//   POST /upload      -> read-and-discard body, reply "OK"  text/plain
//
// Driver-backed (/db, /cache, /mc, /session) and chain-* endpoints are not
// served — this column declares Capabilities{Static: true} only, so the
// scenario applicability filter (servers/servers.go) never schedules them
// against this adapter and the 404 returned off-contract is never observed
// by loadgen.
//
// NOTE on zap vs std.http: zigzap/zap 0.10.7 declares
// minimum_zig_version 0.15.0 and bundles facil.io (a C library). Building it
// under the installed Zig 0.16 toolchain fails (zap's facil.io glue does not
// compile against the 0.16 std), so the dependency cannot be used here.
// Rather than pin a stale Zig nightly for one competitor column, this adapter
// is built on std.http.Server — the from-scratch HTTP/1.1 server that ships
// with Zig 0.16 — which is itself a fair Zig event-loop entry.
//
// Zig 0.16 std notes (this toolchain): the networking API was reworked around
// std.Io (std.net was removed). Addresses are std.Io.net.IpAddress; parse()
// takes the host text and a separate port (NOT a "host:port" string), so the
// bind string is split on the last colon by hand. IpAddress.listen(io, .{...})
// returns a std.Io.net.Server whose accept(io) yields a std.Io.net.Stream;
// stream.reader/.writer expose an `.interface` field (*std.Io.Reader /
// *std.Io.Writer) consumed by http.Server.init. ListenOptions has no reuse_port
// field — on POSIX `reuse_address: true` already sets SO_REUSEADDR+SO_REUSEPORT
// — and there is no per-address ephemeral-port sharing primitive, so the
// SO_REUSEPORT fan-out used by celeris's engines is not expressible. Instead one
// shared listener is accepted on concurrently by every worker, the accept call
// serialized by a mutex (cheap next to the per-connection serve loop). The
// program entry point receives a std.process.Init: `init.gpa` is the allocator,
// `init.io` the blocking Io, and `init.minimal.args` a std.process.Args walked
// via its Iterator.
//
// CLI:
//   -bind <host:port>  default 127.0.0.1:8080. Pass `:0` (or any `host:0`)
//                      to let the kernel allocate a port; the bound address
//                      is reported on stdout via the `ready addr=<addr>`
//                      line the runner waits for before opening loadgen.

const std = @import("std");
const http = std.http;
const net = std.Io.net;
const Io = std.Io;

// Static contract bodies. The JSON payloads are built once at startup into
// process-global slices shared read-only across every worker thread.
const hello_body = "Hello, World!";
const json_hello = "{\"message\":\"Hello, World!\"}";
const upload_body = "OK";

var json_1k: []const u8 = undefined;
var json_64k: []const u8 = undefined;

// Io handed in via std.process.Init; shared read-only by every worker for the
// blocking net.Stream reader/writer and the serialized accept.
var gio: Io = undefined;

// The shared listener every worker accepts on, and the mutex serializing those
// accepts (ListenOptions has no reuse_port, so one socket is fanned out across
// the worker threads instead of one socket per worker). std.Thread.Mutex was
// removed in 0.16; synchronization primitives now live on std.Io and take the
// Io as a parameter.
var listener: *net.Server = undefined;
var accept_mutex: Io.Mutex = .init;

const ct_text = "text/plain";
const ct_json = "application/json";

// Per-connection buffers. The read buffer must hold the full request head
// (plus any pipelined /upload body); the write buffer must hold the largest
// response head + body in one shot — /json-64k is ~64 KiB, so 80 KiB leaves
// headroom for the status line and headers.
const read_buffer_size = 64 * 1024;
const write_buffer_size = 80 * 1024;

fn route(request: *http.Server.Request) !void {
    const method = request.head.method;
    const target = request.head.target;

    if (method == .GET) {
        if (eql(target, "/")) return respond(request, hello_body, ct_text);
        if (eql(target, "/json")) return respond(request, json_hello, ct_json);
        if (eql(target, "/json-1k")) return respond(request, json_1k, ct_json);
        if (eql(target, "/json-64k")) return respond(request, json_64k, ct_json);
        if (std.mem.startsWith(u8, target, "/users/")) {
            const id = target["/users/".len..];
            var buf: [64 + 32]u8 = undefined;
            const body = std.fmt.bufPrint(&buf, "User ID: {s}", .{id}) catch
                return respond(request, "User ID: ", ct_text);
            return respond(request, body, ct_text);
        }
    } else if (method == .POST and eql(target, "/upload")) {
        // respond() discards the request body before replying (see
        // Server.Request.respond -> discardBody), so /upload exercises the
        // body parser and returns the literal "OK" the contract demands
        // without any manual drain.
        return respond(request, upload_body, ct_text);
    }

    return notFound(request);
}

fn eql(a: []const u8, b: []const u8) bool {
    return std.mem.eql(u8, a, b);
}

fn respond(request: *http.Server.Request, body: []const u8, content_type: []const u8) !void {
    // RespondOptions in std 0.16 has no content_type field; the Content-Type
    // header is supplied via extra_headers.
    try request.respond(body, .{
        .status = .ok,
        .keep_alive = true,
        .extra_headers = &.{
            .{ .name = "content-type", .value = content_type },
        },
    });
}

fn notFound(request: *http.Server.Request) !void {
    try request.respond("", .{ .status = .not_found, .keep_alive = true });
}

// serveConn runs the keep-alive serve loop for one accepted stream, reusing
// the read/write buffers across pipelined requests until the peer closes or a
// malformed request aborts. http.Server.init takes a *std.Io.Reader /
// *std.Io.Writer produced by net.Stream.reader/.writer (each exposing an
// `.interface` field).
fn serveConn(stream: net.Stream, read_buffer: []u8, write_buffer: []u8) void {
    defer stream.close(gio);

    var stream_reader = stream.reader(gio, read_buffer);
    var stream_writer = stream.writer(gio, write_buffer);
    var server = http.Server.init(&stream_reader.interface, &stream_writer.interface);

    while (server.reader.state == .ready) {
        var request = server.receiveHead() catch {
            // Clean keep-alive close or any malformed/aborted request: drop
            // the connection. Under load these are expected churn.
            return;
        };
        route(&request) catch return;
    }
}

// worker is one accept loop. Each worker owns private read/write buffers and
// pulls streams off the shared listener; the accept itself is serialized by
// accept_mutex since the listener socket is shared across threads.
fn worker() void {
    var read_buffer: [read_buffer_size]u8 = undefined;
    var write_buffer: [write_buffer_size]u8 = undefined;
    while (true) {
        accept_mutex.lockUncancelable(gio);
        const accepted = listener.accept(gio);
        accept_mutex.unlock(gio);
        const stream = accepted catch continue;
        serveConn(stream, &read_buffer, &write_buffer);
    }
}

// parseBind walks argv (Go-flag style `-bind <value>`, matching every other
// adapter) and returns the bind string. The iterator's byte slices point into
// argv memory that lives for the whole process, so no copy is needed.
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

// boundPort reads the concrete local port off a bound socket via getsockname,
// decoding the network-byte-order port from sockaddr.in or sockaddr.in6. This
// resolves the kernel-assigned port in the `:0` case: the std backend does not
// reliably surface the bound ephemeral port through net.Socket.address.getPort
// (it returns 0 for the IpAddress.listen path), so the port is read straight
// from the socket. posix.system.getsockname is the raw libc/syscall wrapper —
// std.posix.getsockname does not exist in this toolchain. Returns 0 on failure;
// the caller only uses this for the human-readable ready line.
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
    gio = init.io;

    const bind = parseBind(init.minimal.args);

    json_1k = try payload.generate(alloc, 1024);
    json_64k = try payload.generate(alloc, 65536);

    // IpAddress.parse takes host and port separately, so split the bind string
    // on its last colon. A `:0` port is resolved to a concrete kernel-assigned
    // port once the socket is bound.
    const colon = std.mem.lastIndexOfScalar(u8, bind, ':') orelse return error.InvalidBind;
    const host = bind[0..colon];
    const port = try std.fmt.parseInt(u16, bind[colon + 1 ..], 10);
    const addr = try net.IpAddress.parse(host, port);

    var server = try net.IpAddress.listen(&addr, gio, .{ .reuse_address = true });
    defer server.deinit(gio);
    listener = &server;

    // Report the bound port (resolving the :0 case) on the ready line before
    // any worker accepts. The runner's TCP probe waits for this exact line.
    const bound_port = boundPort(server.socket.handle);
    var out_buf: [128]u8 = undefined;
    const msg = try std.fmt.bufPrint(&out_buf, "ready addr={s}:{d}\n", .{ host, bound_port });
    var stdout_buf: [128]u8 = undefined;
    var stdout = Io.File.stdout().writer(gio, &stdout_buf);
    try stdout.interface.writeAll(msg);
    try stdout.interface.flush();

    // One worker per CPU sharing the listener; the main thread runs one too.
    const cpus = std.Thread.getCpuCount() catch 1;
    const extra = if (cpus > 1) cpus - 1 else 0;

    const threads = try alloc.alloc(std.Thread, extra);
    defer alloc.free(threads);
    for (threads) |*t| {
        t.* = try std.Thread.spawn(.{}, worker, .{});
    }

    worker();
}

const payload = @import("payload.zig");
