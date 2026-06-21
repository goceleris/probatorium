package io.goceleris.probatorium.netty;

import io.netty.bootstrap.ServerBootstrap;
import io.netty.channel.Channel;
import io.netty.channel.ChannelInitializer;
import io.netty.channel.ChannelOption;
import io.netty.channel.ChannelPipeline;
import io.netty.channel.EventLoopGroup;
import io.netty.channel.MultiThreadIoEventLoopGroup;
import io.netty.channel.epoll.Epoll;
import io.netty.channel.epoll.EpollChannelOption;
import io.netty.channel.epoll.EpollIoHandler;
import io.netty.channel.epoll.EpollServerSocketChannel;
import io.netty.channel.nio.NioIoHandler;
import io.netty.channel.socket.SocketChannel;
import io.netty.channel.socket.nio.NioServerSocketChannel;
import io.netty.handler.codec.http.HttpObjectAggregator;
import io.netty.handler.codec.http.HttpServerCodec;
import io.netty.handler.codec.http.HttpServerExpectContinueHandler;

import java.net.InetSocketAddress;
import java.util.concurrent.CountDownLatch;

/**
 * probatorium netty adapter — the raw JVM low-level baseline.
 *
 * <p>Serves the canonical contract endpoints declared in
 * servers/common/contract.go:
 *
 * <pre>
 *   GET  /            -&gt; "Hello, World!"             text/plain
 *   GET  /json        -&gt; {"message":"Hello, World!"} application/json
 *   GET  /json-1k     -&gt; deterministic 1026-byte JSON page
 *   GET  /json-8k     -&gt; deterministic 8286-byte JSON page
 *   GET  /json-16k    -&gt; deterministic 16463-byte JSON page
 *   GET  /json-64k    -&gt; deterministic 65618-byte JSON page
 *   GET  /users/:id   -&gt; "User ID: &lt;id&gt;"             text/plain
 *   POST /upload      -&gt; read-and-discard body, "OK"  text/plain
 * </pre>
 *
 * <p>Transport: epoll when the native is loadable (Linux — the bench host),
 * NIO otherwise (dev fallback). SO_REUSEPORT spreads accepts across the
 * event loops (one IO thread per CPU), the JVM analogue of the SO_REUSEPORT
 * worker pools every other native adapter uses.
 *
 * <p>CLI:
 * <ul>
 *   <li>{@code -bind <host:port>} — default 0.0.0.0:8080. Pass {@code :0}
 *       (or {@code host:0}) to let the kernel allocate a port; the actually
 *       bound address is reported on stdout via {@code ready addr=<addr>},
 *       the line the runner waits for before opening loadgen.</li>
 *   <li>{@code -engine <value>} — default "h1". Only "h1" (plain HTTP/1.1)
 *       is accepted: raw Netty's HttpServerCodec is an HTTP/1.x codec, and
 *       prior-knowledge h2c would need a separate Http2 pipeline. Any other
 *       value is a hard error (exit 2), so a typo fails fast and visibly
 *       rather than silently serving h1 — mirroring drogon's h1-only stance,
 *       which registers NO h2 column.</li>
 * </ul>
 *
 * <p>Lifecycle: SIGTERM or SIGINT triggers a graceful shutdown that finishes
 * in-flight requests well inside the runner's 5-second grace window.
 */
public final class NettyServer {

    public static void main(String[] args) throws Exception {
        String bind = "0.0.0.0:8080";
        String engine = "h1";
        for (int i = 0; i < args.length; i++) {
            if ("-bind".equals(args[i]) && i + 1 < args.length) {
                bind = args[++i];
            } else if ("-engine".equals(args[i]) && i + 1 < args.length) {
                engine = args[++i];
            }
        }

        if (!"h1".equals(engine)) {
            System.err.println("netty: unknown -engine \"" + engine + "\" (want \"h1\")");
            System.exit(2);
            return;
        }

        InetSocketAddress addr = parseBind(bind);

        boolean useEpoll = Epoll.isAvailable();
        int threads = Runtime.getRuntime().availableProcessors();

        // 4.2 API: one MultiThreadIoEventLoopGroup carrying a transport-specific
        // IoHandlerFactory replaces the deprecated per-transport EventLoopGroup
        // classes. A single group with N IO threads + SO_REUSEPORT is the
        // standard high-throughput shape (no separate boss group needed once
        // the kernel load-balances accepts across the reuseport sockets).
        final EventLoopGroup group = useEpoll
                ? new MultiThreadIoEventLoopGroup(threads, EpollIoHandler.newFactory())
                : new MultiThreadIoEventLoopGroup(threads, NioIoHandler.newFactory());

        try {
            ServerBootstrap b = new ServerBootstrap();
            b.group(group)
                    .channel(useEpoll ? EpollServerSocketChannel.class : NioServerSocketChannel.class)
                    .option(ChannelOption.SO_BACKLOG, 1024)
                    .option(ChannelOption.SO_REUSEADDR, true)
                    .childOption(ChannelOption.TCP_NODELAY, true)
                    .childHandler(new ChannelInitializer<SocketChannel>() {
                        @Override
                        protected void initChannel(SocketChannel ch) {
                            ChannelPipeline p = ch.pipeline();
                            p.addLast(new HttpServerCodec());
                            // Aggregate the body so /upload's read-and-discard
                            // is implicit and every request reaches the handler
                            // as a FullHttpRequest. 1 MiB cap is ample for the
                            // bench's small uploads.
                            p.addLast(new HttpObjectAggregator(1 << 20));
                            p.addLast(new HttpServerExpectContinueHandler());
                            p.addLast(new NettyHttpHandler());
                        }
                    });

            // SO_REUSEPORT: let every IO thread own an independent accept queue
            // on the same port so accepts scale linearly with cores. Epoll-only
            // (NIO has no portable SO_REUSEPORT option); NIO falls back to the
            // single-acceptor default, which is the dev path, not the bench.
            if (useEpoll) {
                b.option(EpollChannelOption.SO_REUSEPORT, true);
            }

            Channel ch = b.bind(addr).sync().channel();

            // Report the ACTUAL bound address — critical for -bind host:0, where
            // the kernel assigns the port and the runner needs the real value.
            InetSocketAddress local = (InetSocketAddress) ch.localAddress();
            String host = addr.getHostString();
            System.out.println("ready addr=" + host + ":" + local.getPort());
            System.out.flush();

            // Graceful shutdown on SIGTERM / SIGINT: close the listen channel,
            // release the loops, and let main() return.
            CountDownLatch done = new CountDownLatch(1);
            Runtime.getRuntime().addShutdownHook(new Thread(() -> {
                try {
                    ch.close().sync();
                } catch (InterruptedException ignored) {
                    Thread.currentThread().interrupt();
                } finally {
                    done.countDown();
                }
            }, "netty-shutdown"));

            ch.closeFuture().sync();
            done.await();
        } finally {
            group.shutdownGracefully();
        }
    }

    /**
     * Splits {@code host:port} into an InetSocketAddress, preserving the
     * caller-supplied host so the ready-line echoes the requested bind host
     * (the kernel-assigned port replaces the :0). An empty / wildcard host
     * binds all interfaces.
     */
    private static InetSocketAddress parseBind(String bind) {
        int idx = bind.lastIndexOf(':');
        if (idx < 0) {
            return new InetSocketAddress(Integer.parseInt(bind));
        }
        String host = bind.substring(0, idx);
        int port = Integer.parseInt(bind.substring(idx + 1));
        if (host.isEmpty() || "*".equals(host)) {
            return new InetSocketAddress(port);
        }
        return new InetSocketAddress(host, port);
    }

    private NettyServer() {}
}
