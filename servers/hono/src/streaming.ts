// WS/SSE streaming surface for the Bun (h1) path — the like-for-like
// counterpart to the gorilla_ws reference adapter (servers/gorilla_ws).
//
// Two routes ride the existing Bun.serve HTTP/1.1 listener:
//   - GET /ws      WebSocket; ?mode= selects echo / large-echo / hub-receiver
//   - GET /events  SSE fan-out; every subscriber joins the broker, a
//                  background publisher pushes a steady event stream
//
// Both are H1-only by design (the WS upgrade + SSE long-poll ride HTTP/1.1);
// this module is never wired into the h2c (node:http2) branch.

import type { ServerWebSocket, Server, WebSocketHandler } from "bun";

// ?mode= selectors — mirror the gorilla_ws reference + scenarios.StreamMode*
// so the loadgen client drives every adapter identically.
const STREAM_MODE_WS_HUB = "ws-hub";
const STREAM_MODE_WS_LARGE_ECHO = "ws-large-echo";

// 1 MiB read limit for large-echo; holds the 64 KiB large-echo payload.
export const LARGE_ECHO_MAX_PAYLOAD = 1 << 20;

const BROADCAST_PAYLOAD = "payload";
const SSE_EVENT_DATA = "hello";
const STREAM_PUBLISH_INTERVAL_MS = 1;

// Per-socket data threaded through server.upgrade → websocket handlers; the
// mode chosen at upgrade time decides receiver-vs-echo behaviour in message().
interface WSData {
  mode: string;
}

type HonoWebSocket = ServerWebSocket<WSData>;

// --- WebSocket hub (the naive connection-set the celeris Hub replaces) ---

// Pure broadcast receivers; the ticker fans BROADCAST_PAYLOAD to each. Echo
// sockets never join — they reply inline in message() and are not iterated.
const hub = new Set<HonoWebSocket>();

// Bun's native websocket handler. open() decides registration by mode;
// message() echoes for the echo modes; close() always leaves the hub.
export const websocket: WebSocketHandler<WSData> = {
  maxPayloadLength: LARGE_ECHO_MAX_PAYLOAD,
  open(ws: HonoWebSocket): void {
    if (ws.data.mode === STREAM_MODE_WS_HUB) hub.add(ws);
  },
  message(ws: HonoWebSocket, message: string | Buffer): void {
    // Hub receivers drain only; everything else echoes verbatim.
    if (ws.data.mode === STREAM_MODE_WS_HUB) return;
    ws.send(message);
  },
  close(ws: HonoWebSocket): void {
    hub.delete(ws);
  },
};

// --- SSE broker (the publish-to-N-subscribers fan-out the celeris Broker replaces) ---

// Pre-encode the SSE wire frame once: "data: hello\n\n".
const SSE_FRAME = new TextEncoder().encode(`data: ${SSE_EVENT_DATA}\n\n`);

// Subscribed response controllers. A slow/closed controller is dropped on the
// failing enqueue rather than blocking the shared publish loop.
const subscribers = new Set<ReadableStreamDefaultController<Uint8Array>>();

// --- background tickers ---

let broadcastTimer: ReturnType<typeof setInterval> | undefined;

// startBroadcast launches the single 1ms loop that drives both fan-outs: the
// WS hub broadcast and the SSE publish share one tick so the wire cadence
// matches the reference's two independent tickers without doubling timers.
export function startBroadcast(): void {
  if (broadcastTimer !== undefined) return;
  broadcastTimer = setInterval(() => {
    for (const ws of hub) ws.send(BROADCAST_PAYLOAD);
    for (const controller of subscribers) {
      try {
        controller.enqueue(SSE_FRAME);
      } catch {
        // closed or backpressured controller: drop rather than block the loop.
        subscribers.delete(controller);
      }
    }
  }, STREAM_PUBLISH_INTERVAL_MS);
}

export function stopBroadcast(): void {
  if (broadcastTimer !== undefined) {
    clearInterval(broadcastTimer);
    broadcastTimer = undefined;
  }
}

// --- fetch interception ---

const SSE_HEADERS: Record<string, string> = {
  "Content-Type": "text/event-stream",
  "Cache-Control": "no-cache",
  Connection: "keep-alive",
};

// handleStreaming intercepts the two streaming routes. Returns a Response for
// /events, undefined for a successful /ws upgrade (Bun owns the socket), and
// null for everything else so the caller falls through to app.fetch.
export function handleStreaming(
  req: Request,
  server: Server<WSData>,
): Response | undefined | null {
  const url = new URL(req.url);

  if (url.pathname === "/ws") {
    const mode = url.searchParams.get("mode") ?? "";
    // upgrade() hands the socket to the websocket handler on success; returning
    // undefined tells Bun the response is owned by the upgraded connection.
    if (server.upgrade(req, { data: { mode } })) return undefined;
    return new Response("expected websocket upgrade", { status: 426 });
  }

  if (url.pathname === "/events") {
    // Capture the controller so cancel() can deregister this exact subscriber
    // when the client disconnects (the explicit-unsubscribe path); the publish
    // loop's drop-on-enqueue is the backstop for controllers that error first.
    let ctrl: ReadableStreamDefaultController<Uint8Array> | undefined;
    const stream = new ReadableStream<Uint8Array>({
      start(controller): void {
        ctrl = controller;
        subscribers.add(controller);
      },
      cancel(): void {
        if (ctrl !== undefined) subscribers.delete(ctrl);
      },
    });
    return new Response(stream, { headers: SSE_HEADERS });
  }

  return null;
}
