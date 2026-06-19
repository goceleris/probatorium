package io.goceleris.probatorium.netty;

import io.netty.buffer.Unpooled;
import io.netty.channel.ChannelFuture;
import io.netty.channel.ChannelFutureListener;
import io.netty.channel.ChannelHandlerContext;
import io.netty.channel.SimpleChannelInboundHandler;
import io.netty.handler.codec.http.DefaultFullHttpResponse;
import io.netty.handler.codec.http.FullHttpRequest;
import io.netty.handler.codec.http.FullHttpResponse;
import io.netty.handler.codec.http.HttpMethod;
import io.netty.handler.codec.http.HttpUtil;

import java.nio.charset.StandardCharsets;

import static io.netty.handler.codec.http.HttpHeaderNames.CONNECTION;
import static io.netty.handler.codec.http.HttpHeaderNames.CONTENT_LENGTH;
import static io.netty.handler.codec.http.HttpHeaderNames.CONTENT_TYPE;
import static io.netty.handler.codec.http.HttpHeaderValues.CLOSE;
import static io.netty.handler.codec.http.HttpHeaderValues.KEEP_ALIVE;
import static io.netty.handler.codec.http.HttpResponseStatus.NOT_FOUND;
import static io.netty.handler.codec.http.HttpResponseStatus.OK;

/**
 * The adapter's sole inbound handler: a hand-written (method, path) match
 * over the canonical probatorium contract endpoints. There is no router
 * abstraction — this is the raw-Netty baseline, so routing is a switch.
 *
 * <p>The pipeline runs an {@code HttpObjectAggregator} ahead of this handler,
 * so every request arrives as a {@link FullHttpRequest} with its body already
 * aggregated. That makes /upload's read-and-discard implicit (we simply never
 * read the content) and keeps the handler branch-free per request.
 *
 * <p>Responses reuse the pre-built static byte arrays; each response wraps a
 * fresh {@code Unpooled.wrappedBuffer} view (zero-copy, no per-request body
 * allocation). Keep-alive is honoured per RFC 9112 via {@link HttpUtil}.
 */
final class NettyHttpHandler extends SimpleChannelInboundHandler<FullHttpRequest> {

    private static final String TEXT_PLAIN = "text/plain";
    private static final String APPLICATION_JSON = "application/json";

    private static final byte[] HELLO = "Hello, World!".getBytes(StandardCharsets.UTF_8);
    private static final byte[] JSON_HELLO =
            "{\"message\":\"Hello, World!\"}".getBytes(StandardCharsets.UTF_8);
    private static final byte[] OK_BODY = "OK".getBytes(StandardCharsets.UTF_8);
    private static final byte[] NOT_FOUND_BODY = "Not Found".getBytes(StandardCharsets.UTF_8);

    @Override
    public void channelReadComplete(ChannelHandlerContext ctx) {
        ctx.flush();
    }

    @Override
    protected void channelRead0(ChannelHandlerContext ctx, FullHttpRequest req) {
        // Strip the query string; the contract paths are literal.
        String uri = req.uri();
        int q = uri.indexOf('?');
        String path = q >= 0 ? uri.substring(0, q) : uri;
        HttpMethod method = req.method();

        if (HttpMethod.GET.equals(method)) {
            switch (path) {
                case "/":
                    respond(ctx, req, OK, TEXT_PLAIN, HELLO);
                    return;
                case "/json":
                    respond(ctx, req, OK, APPLICATION_JSON, JSON_HELLO);
                    return;
                case "/json-1k":
                    respond(ctx, req, OK, APPLICATION_JSON, Payload.JSON_1K);
                    return;
                case "/json-8k":
                    respond(ctx, req, OK, APPLICATION_JSON, Payload.JSON_8K);
                    return;
                case "/json-16k":
                    respond(ctx, req, OK, APPLICATION_JSON, Payload.JSON_16K);
                    return;
                case "/json-64k":
                    respond(ctx, req, OK, APPLICATION_JSON, Payload.JSON_64K);
                    return;
                default:
                    if (path.startsWith("/users/")) {
                        String id = path.substring("/users/".length());
                        byte[] body = ("User ID: " + id).getBytes(StandardCharsets.UTF_8);
                        respond(ctx, req, OK, TEXT_PLAIN, body);
                        return;
                    }
            }
        } else if (HttpMethod.POST.equals(method) && "/upload".equals(path)) {
            // Read-and-discard: the aggregated body is simply never touched.
            respond(ctx, req, OK, TEXT_PLAIN, OK_BODY);
            return;
        }

        respond(ctx, req, NOT_FOUND, TEXT_PLAIN, NOT_FOUND_BODY);
    }

    private static void respond(ChannelHandlerContext ctx, FullHttpRequest req,
                                io.netty.handler.codec.http.HttpResponseStatus status,
                                String contentType, byte[] body) {
        boolean keepAlive = HttpUtil.isKeepAlive(req);
        FullHttpResponse response = new DefaultFullHttpResponse(
                req.protocolVersion(), status, Unpooled.wrappedBuffer(body));
        response.headers()
                .set(CONTENT_TYPE, contentType)
                .setInt(CONTENT_LENGTH, body.length);

        if (keepAlive) {
            if (!req.protocolVersion().isKeepAliveDefault()) {
                response.headers().set(CONNECTION, KEEP_ALIVE);
            }
        } else {
            response.headers().set(CONNECTION, CLOSE);
        }

        ChannelFuture f = ctx.write(response);
        if (!keepAlive) {
            f.addListener(ChannelFutureListener.CLOSE);
        }
    }

    @Override
    public void exceptionCaught(ChannelHandlerContext ctx, Throwable cause) {
        // Per-connection errors (client resets, partial sends) are expected
        // churn under load; close the offending channel and move on.
        ctx.close();
    }
}
