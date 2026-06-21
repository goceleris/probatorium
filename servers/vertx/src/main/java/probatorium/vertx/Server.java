// probatorium Eclipse Vert.x adapter.
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
// Driver-backed (/db, /cache, /mc, /session) and chain-* endpoints are OUT
// of scope for this adapter; the scenario applicability filter
// (servers/servers.go) never schedules them against a Static-only column, so
// the 404 they return here is never observed by loadgen.
//
// CLI (matches the Go / Rust / Python adapters so the runner invokes this
// binary identically):
//
//   -bind <host:port>  default 127.0.0.1:8080. Pass `:0` (or `host:0`) to let
//                      the kernel allocate a port; the bound address is then
//                      reported on stdout via the `ready addr=<addr>` line the
//                      runner waits for before opening loadgen.
//   -engine <value>    default "h1". One of:
//                        h1  — strict HTTP/1.1. HTTP/2 cleartext is disabled
//                              on the listener (setHttp2ClearTextEnabled(false))
//                              so an h2 preface gets no h2 service, mirroring
//                              the strict-h1 behaviour of the Rust adapters.
//                        h2c — HTTP/2 cleartext, prior-knowledge. Vert.x's
//                              HTTP server speaks h2c out of the box on a
//                              plaintext socket (no TLS, no ALPN); a client
//                              that opens with the h2 connection preface is
//                              served HTTP/2 directly. Matches the h2c-noupg
//                              convention (no h1->h2 Upgrade negotiation).
//                              Unknown values exit non-zero.
//
// Scaling: one HttpServerVerticle per event loop. The first instance binds
// the (resolved) port and reports readiness; the remaining instances reuse
// the same host/port — on Linux native epoll with SO_REUSEPORT each gets its
// own acceptor, otherwise Vert.x round-robins accepted connections across the
// event loops internally.
//
// Lifecycle: SIGTERM / SIGINT trigger vertx.close(), which drains in-flight
// requests and closes the listeners well inside the runner's 5-second grace
// window (servers/start.go) before its SIGKILL fallback.

package probatorium.vertx;

import io.vertx.core.AbstractVerticle;
import io.vertx.core.DeploymentOptions;
import io.vertx.core.Promise;
import io.vertx.core.Vertx;
import io.vertx.core.VertxOptions;
import io.vertx.core.buffer.Buffer;
import io.vertx.core.http.HttpServer;
import io.vertx.core.http.HttpServerOptions;
import io.vertx.ext.web.Router;
import io.vertx.ext.web.RoutingContext;
import io.vertx.ext.web.handler.BodyHandler;
import java.nio.charset.StandardCharsets;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.atomic.AtomicInteger;

public final class Server {

  // Static byte payloads, hoisted so every request reuses one immutable
  // Buffer — no per-request allocation, mirroring the Go adapters that serve a
  // pre-baked slice. Vert.x writes these verbatim with an auto-computed
  // Content-Length and never appends a charset to the Content-Type, so the
  // bytes are identical to common.Endpoints[*].ResponseBody.
  private static final Buffer HELLO_PLAIN =
      Buffer.buffer("Hello, World!".getBytes(StandardCharsets.US_ASCII));
  private static final Buffer HELLO_JSON =
      Buffer.buffer("{\"message\":\"Hello, World!\"}".getBytes(StandardCharsets.US_ASCII));
  private static final Buffer OK_PLAIN =
      Buffer.buffer("OK".getBytes(StandardCharsets.US_ASCII));

  private static final String CT_TEXT = "text/plain";
  private static final String CT_JSON = "application/json";

  private Server() {}

  public static void main(String[] args) {
    String bind = "127.0.0.1:8080";
    String engine = "h1";
    for (int i = 0; i < args.length; i++) {
      String a = args[i];
      if ((a.equals("-bind") || a.equals("--bind")) && i + 1 < args.length) {
        bind = args[++i];
      } else if (a.startsWith("-bind=")) {
        bind = a.substring("-bind=".length());
      } else if (a.startsWith("--bind=")) {
        bind = a.substring("--bind=".length());
      } else if ((a.equals("-engine") || a.equals("--engine")) && i + 1 < args.length) {
        engine = args[++i];
      } else if (a.startsWith("-engine=")) {
        engine = a.substring("-engine=".length());
      } else if (a.startsWith("--engine=")) {
        engine = a.substring("--engine=".length());
      }
    }

    final boolean h2c;
    switch (engine) {
      case "h1":
        h2c = false;
        break;
      case "h2c":
        h2c = true;
        break;
      default:
        System.err.println("vertx: unknown -engine \"" + engine + "\" (want h1|h2c)");
        System.exit(1);
        return;
    }

    String host;
    int port;
    try {
      int[] hp = new int[1];
      host = parseHost(bind, hp);
      port = hp[0];
    } catch (RuntimeException e) {
      System.err.println("vertx: bad -bind \"" + bind + "\": " + e.getMessage());
      System.exit(1);
      return;
    }

    // Prefer the native (epoll on Linux) transport so each verticle binds the
    // shared port with SO_REUSEPORT; harmless on platforms without it (Vert.x
    // silently falls back to NIO).
    Vertx vertx = Vertx.vertx(new VertxOptions().setPreferNativeTransport(true));

    int instances = Math.max(1, Runtime.getRuntime().availableProcessors());

    // The verticle deployment is async; block main() until either the first
    // server reports its bound port (success) or a verticle fails to start.
    CountDownLatch ready = new CountDownLatch(1);
    AtomicInteger boundPort = new AtomicInteger(-1);

    DeploymentOptions opts =
        new DeploymentOptions().setInstances(instances);

    vertx
        .deployVerticle(() -> new HttpVerticle(host, port, h2c, boundPort, ready), opts)
        .onFailure(
            err -> {
              System.err.println("vertx: deploy: " + err.getMessage());
              System.exit(1);
            });

    try {
      ready.await();
    } catch (InterruptedException e) {
      Thread.currentThread().interrupt();
      System.exit(1);
      return;
    }

    // The runner's TCP probe waits for this exact line on stdout. Print and
    // flush before the JVM settles into the reactor so the probe never races
    // the listener.
    System.out.println("ready addr=" + host + ":" + boundPort.get());
    System.out.flush();

    // Graceful shutdown: SIGTERM/SIGINT -> vertx.close() drains and exits.
    Runtime.getRuntime()
        .addShutdownHook(
            new Thread(
                () -> {
                  CountDownLatch closed = new CountDownLatch(1);
                  vertx.close().onComplete(ar -> closed.countDown());
                  try {
                    closed.await();
                  } catch (InterruptedException ignored) {
                    Thread.currentThread().interrupt();
                  }
                }));
  }

  /**
   * One event-loop verticle: builds its own Router + HttpServer and binds the
   * shared host/port. The first instance to bind records the actual port (for
   * the :0 case) and releases the readiness latch; later instances reuse the
   * resolved port so they all share one listening socket.
   */
  static final class HttpVerticle extends AbstractVerticle {
    private final String host;
    private final int requestedPort;
    private final boolean h2c;
    private final AtomicInteger boundPort;
    private final CountDownLatch ready;

    HttpVerticle(
        String host,
        int requestedPort,
        boolean h2c,
        AtomicInteger boundPort,
        CountDownLatch ready) {
      this.host = host;
      this.requestedPort = requestedPort;
      this.h2c = h2c;
      this.boundPort = boundPort;
      this.ready = ready;
    }

    @Override
    public void start(Promise<Void> startPromise) {
      Router router = Router.router(vertx);

      // /upload reads-and-discards the body, so the body parser is part of the
      // measured cost like every other adapter. BodyHandler is mounted ONLY on
      // that route (a global BodyHandler would buffer bodies for every request,
      // taxing the GET-heavy cells for nothing). File uploads are disabled so
      // the handler never spills to disk (no `file-uploads/` dir side effect)
      // and the body stays a discarded in-memory buffer.
      router.post("/upload").handler(BodyHandler.create(false));

      router.get("/").handler(ctx -> sendBuffer(ctx, CT_TEXT, HELLO_PLAIN));
      router.get("/json").handler(ctx -> sendBuffer(ctx, CT_JSON, HELLO_JSON));
      router.get("/json-1k").handler(ctx -> sendBuffer(ctx, CT_JSON, Payload.JSON_1K));
      router.get("/json-8k").handler(ctx -> sendBuffer(ctx, CT_JSON, Payload.JSON_8K));
      router.get("/json-16k").handler(ctx -> sendBuffer(ctx, CT_JSON, Payload.JSON_16K));
      router.get("/json-64k").handler(ctx -> sendBuffer(ctx, CT_JSON, Payload.JSON_64K));
      router
          .get("/users/:id")
          .handler(
              ctx ->
                  sendBuffer(
                      ctx,
                      CT_TEXT,
                      Buffer.buffer(
                          ("User ID: " + ctx.pathParam("id"))
                              .getBytes(StandardCharsets.UTF_8))));
      router.post("/upload").handler(ctx -> sendBuffer(ctx, CT_TEXT, OK_PLAIN));

      HttpServerOptions options =
          new HttpServerOptions()
              // SO_REUSEPORT so each verticle gets its own acceptor on the
              // native epoll transport (ignored on NIO; harmless either way).
              .setReusePort(true)
              .setReuseAddress(true)
              // h1 mode: refuse the h2 cleartext preface so the column is
              // strictly HTTP/1.1 (mirrors the Rust adapters' strict h1).
              // h2c mode: leave cleartext h2 enabled (the default) so a
              // prior-knowledge h2 client is served HTTP/2 directly.
              .setHttp2ClearTextEnabled(h2c);

      vertx
          .createHttpServer(options)
          .requestHandler(router)
          // Bind the requested port for the first instance (may be 0); later
          // instances bind the resolved port so they share the socket.
          .listen(resolvedPort(), host)
          .onSuccess(
              server -> {
                publishBound(server);
                startPromise.complete();
              })
          .onFailure(startPromise::fail);
    }

    // resolvedPort returns the port THIS instance should bind: the requested
    // one until the first instance has published a concrete port, then that
    // concrete port (so a :0 request lands every instance on the same kernel-
    // assigned port).
    private int resolvedPort() {
      int p = boundPort.get();
      return p >= 0 ? p : requestedPort;
    }

    private void publishBound(HttpServer server) {
      // compareAndSet so only the first instance to bind wins the race to
      // record the actual port and release the readiness latch.
      if (boundPort.compareAndSet(-1, server.actualPort())) {
        ready.countDown();
      }
    }
  }

  // sendBuffer writes a pre-built body with the exact content-type the contract
  // demands. Vert.x sets Content-Length automatically for a non-chunked end()
  // and never appends a charset to the header, keeping responses byte-identical
  // across adapters.
  private static void sendBuffer(RoutingContext ctx, String contentType, Buffer body) {
    ctx.response().putHeader("content-type", contentType).end(body);
  }

  // parseHost splits host:port into host + port (written into hp[0]). IPv6
  // literals are accepted in bracketed form ([::1]:8080).
  private static String parseHost(String bind, int[] hp) {
    if (bind.startsWith("[")) {
      int rb = bind.indexOf(']');
      if (rb < 0 || rb + 2 > bind.length() || bind.charAt(rb + 1) != ':') {
        throw new IllegalArgumentException("expected [host]:port");
      }
      hp[0] = Integer.parseInt(bind.substring(rb + 2));
      return bind.substring(1, rb);
    }
    int c = bind.lastIndexOf(':');
    if (c <= 0 || c == bind.length() - 1) {
      throw new IllegalArgumentException("expected host:port");
    }
    hp[0] = Integer.parseInt(bind.substring(c + 1));
    return bind.substring(0, c);
  }
}
