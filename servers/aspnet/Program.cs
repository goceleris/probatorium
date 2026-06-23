using System.Text;
using AspnetAdapter;
using Microsoft.AspNetCore.Http;
using Microsoft.AspNetCore.Server.Kestrel.Core;

// ASP.NET Core (Kestrel, minimal APIs) competitor adapter for the
// probatorium benchmark matrix.
//
// Implements the same canonical contract as every other adapter
// (servers/common/contract.go): GET / /json /json-1k /json-8k /json-16k
// /json-64k /users/:id and POST /upload. The JSON payloads are produced by
// the deterministic generator in Payload.cs, byte-identical to the Go and
// Rust adapters so loadgen fixtures compare equal across languages.
//
// The server prints "ready addr=<addr>" on stdout once it is listening so
// the probatorium runner can detect readiness, and Kestrel handles
// SIGTERM for graceful shutdown within the runner's grace window.
//
// Configured for raw throughput: no logging providers, no dev middleware,
// no HTTPS redirection, no response buffering — endpoints write bytes
// straight to the response body.
//
// Wire protocol is selected by -engine (default "h1"):
//   - h1  → HTTP/1.1 cleartext only on the listener.
//   - h2c → HTTP/2 cleartext prior-knowledge only (no TLS, no HTTP/1.1
//           upgrade dance), mirroring stdhttp-h2's h2c-noupg mode.
// The two modes are strictly separated: an h1 listener never speaks h2 and
// an h2c listener never speaks h1.

var bind = "127.0.0.1:8080";
var engine = "h1";
for (var i = 0; i < args.Length; i++)
{
    if (args[i] == "-bind" && i + 1 < args.Length)
    {
        bind = args[i + 1];
        i++;
    }
    else if (args[i] == "-engine" && i + 1 < args.Length)
    {
        engine = args[i + 1];
        i++;
    }
}

if (engine != "h1" && engine != "h2c")
{
    Console.Error.Write($"aspnet: unknown -engine \"{engine}\" (want \"h1\" or \"h2c\")\n");
    Environment.Exit(2);
}

// Prior-knowledge protocol selection. h2c serves HTTP/2 cleartext ONLY;
// h1 serves HTTP/1.1 ONLY. Neither falls back to the other (no Http1AndHttp2
// negotiation), so the listener's wire behaviour is unambiguous for the
// conformance probe and the loadgen client.
var protocols = engine == "h2c" ? HttpProtocols.Http2 : HttpProtocols.Http1;

var (host, port) = SplitBind(bind);

var builder = WebApplication.CreateSlimBuilder(args);

// Strip every logging provider: no console/event-source overhead on the
// hot path.
builder.Logging.ClearProviders();

builder.WebHost.ConfigureKestrel(options =>
{
    options.AddServerHeader = false;
    options.AllowSynchronousIO = false;
    // Match celeris's advertised h2 flow-control profile so the matrix is a
    // fair fight: 1 MiB initial windows (Kestrel defaults below 1 MiB),
    // connection window >= stream window, 100 concurrent streams.
    options.Limits.Http2.InitialStreamWindowSize = 1024 * 1024;
    options.Limits.Http2.InitialConnectionWindowSize = 1024 * 1024;
    options.Limits.Http2.MaxStreamsPerConnection = 100;
    if (host is null)
    {
        options.ListenAnyIP(port, listenOptions => listenOptions.Protocols = protocols);
    }
    else
    {
        options.Listen(System.Net.IPAddress.Parse(host), port, listenOptions => listenOptions.Protocols = protocols);
    }
});

var app = builder.Build();

ReadOnlySpan<byte> hello = "Hello, World!"u8;
ReadOnlySpan<byte> jsonHello = "{\"message\":\"Hello, World!\"}"u8;

// Snapshot the spans into arrays once so the closures can capture them.
var helloBytes = hello.ToArray();
var jsonHelloBytes = jsonHello.ToArray();
var json1k = Payload.Json1k;
var json8k = Payload.Json8k;
var json16k = Payload.Json16k;
var json64k = Payload.Json64k;
var okBytes = "OK"u8.ToArray();

app.MapGet("/", (HttpContext ctx) => WriteBytes(ctx, "text/plain", helloBytes));
app.MapGet("/json", (HttpContext ctx) => WriteBytes(ctx, "application/json", jsonHelloBytes));
app.MapGet("/json-1k", (HttpContext ctx) => WriteBytes(ctx, "application/json", json1k));
app.MapGet("/json-8k", (HttpContext ctx) => WriteBytes(ctx, "application/json", json8k));
app.MapGet("/json-16k", (HttpContext ctx) => WriteBytes(ctx, "application/json", json16k));
app.MapGet("/json-64k", (HttpContext ctx) => WriteBytes(ctx, "application/json", json64k));

app.MapGet("/users/{id}", (HttpContext ctx, string id) =>
{
    var body = Encoding.UTF8.GetBytes("User ID: " + id);
    return WriteBytes(ctx, "text/plain", body);
});

app.MapPost("/upload", async (HttpContext ctx) =>
{
    // Read-and-discard the request body, then reply with a small "OK".
    var buffer = new byte[8192];
    var stream = ctx.Request.Body;
    while (await stream.ReadAsync(buffer) > 0)
    {
        // drain
    }

    await WriteBytes(ctx, "text/plain", okBytes);
});

await app.StartAsync();

// Report the actual bound address(es). The runner only needs one match;
// emit the address it dialed with.
Console.Out.Write($"ready addr={bind}\n");
Console.Out.Flush();

await app.WaitForShutdownAsync();
return;

static Task WriteBytes(HttpContext ctx, string contentType, byte[] body)
{
    var res = ctx.Response;
    res.StatusCode = StatusCodes.Status200OK;
    res.ContentType = contentType;
    res.ContentLength = body.Length;
    return res.Body.WriteAsync(body, 0, body.Length);
}

static (string? Host, int Port) SplitBind(string bind)
{
    var idx = bind.LastIndexOf(':');
    if (idx < 0)
    {
        return (null, int.Parse(bind));
    }

    var hostPart = bind[..idx];
    var portPart = bind[(idx + 1)..];
    var port = int.Parse(portPart);
    if (hostPart.Length == 0 || hostPart == "*" || hostPart == "0.0.0.0")
    {
        return (hostPart == "0.0.0.0" ? "0.0.0.0" : null, port);
    }

    return (hostPart, port);
}
