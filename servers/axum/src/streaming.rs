// probatorium axum adapter — WS/SSE streaming surface.
//
// Mirrors the gorilla_ws reference (servers/gorilla_ws/server.go) so the
// celeris streaming cell-columns have a like-for-like tower-stack baseline.
// Two routes, both HTTP/1.1 only (the WS upgrade and the SSE long-poll both
// ride h1; the h2c serve path never exercises them):
//
//   GET /ws      — RFC6455 upgrade; ?mode= selects echo / large-echo / hub.
//   GET /events  — SSE fan-out; every subscriber streams the published frame.
//
// Fan-out is a single broadcast "tick": one task pulses `()` every 1ms and
// every hub socket / SSE subscriber emits its own frame on each tick. This
// replaces the gorilla reference's per-broker connection-set + RWMutex with
// tokio's broadcast channel — slow consumers see `Lagged` and skip, which is
// exactly the drop-rather-than-block policy the reference's non-blocking
// channel send implements.

use std::collections::HashMap;
use std::time::Duration;

use axum::extract::ws::{Message, WebSocket, WebSocketUpgrade};
use axum::extract::{Query, State};
use axum::response::sse::{Event, KeepAlive, Sse};
use axum::response::Response;
use futures_util::stream::{self, Stream, StreamExt};
use tokio::sync::broadcast;

// ?mode= selectors — mirror scenarios.StreamMode* and the gorilla reference
// so the loadgen client drives every adapter identically.
const STREAM_MODE_WS_HUB: &str = "ws-hub";
const STREAM_MODE_WS_LARGE_ECHO: &str = "ws-large-echo";

const BROADCAST_PAYLOAD: &str = "payload";
const SSE_EVENT_DATA: &str = "hello";

const STREAM_PUBLISH_INTERVAL: Duration = Duration::from_millis(1);

// 1 MiB read limit for the large-echo mode — holds the 64 KiB large-echo
// payload with headroom, matching the gorilla reference's SetReadLimit.
const LARGE_ECHO_READ_LIMIT: usize = 1 << 20;

// Tick-channel capacity. A subscriber that falls this far behind the 1ms
// publisher sees Lagged and skips ahead — the drop policy. Sized generously
// so only genuinely stalled consumers ever lag.
const TICK_CHANNEL_CAPACITY: usize = 1024;

// AppState carries the shared broadcast tick. Cloning a Sender is cheap
// (Arc bump) and every handler subscribes off it, so the static handlers
// stay state-free while the streaming handlers extract this.
#[derive(Clone)]
pub struct AppState {
    tick: broadcast::Sender<()>,
}

impl AppState {
    pub fn new() -> Self {
        let (tick, _) = broadcast::channel(TICK_CHANNEL_CAPACITY);
        Self { tick }
    }

    // spawn_publisher starts the single 1ms pulse that drives every hub
    // socket and SSE subscriber. send() errors only when there are zero
    // receivers; that is the steady idle state, so the error is discarded.
    pub fn spawn_publisher(&self) {
        let tick = self.tick.clone();
        tokio::spawn(async move {
            let mut interval = tokio::time::interval(STREAM_PUBLISH_INTERVAL);
            loop {
                interval.tick().await;
                let _ = tick.send(());
            }
        });
    }
}

// ws_handler upgrades the connection and dispatches on ?mode=. large-echo
// raises the per-message read limit before the upgrade is negotiated.
pub async fn ws_handler(
    ws: WebSocketUpgrade,
    Query(params): Query<HashMap<String, String>>,
    State(state): State<AppState>,
) -> Response {
    let mode = params.get("mode").cloned();
    match mode.as_deref() {
        Some(STREAM_MODE_WS_HUB) => {
            let rx = state.tick.subscribe();
            ws.on_upgrade(move |socket| ws_hub(socket, rx))
        }
        Some(STREAM_MODE_WS_LARGE_ECHO) => ws
            .max_message_size(LARGE_ECHO_READ_LIMIT)
            .max_frame_size(LARGE_ECHO_READ_LIMIT)
            .on_upgrade(ws_echo),
        _ => ws.on_upgrade(ws_echo),
    }
}

// ws_echo reflects every received frame back verbatim (same opcode). Any
// read or write error ends the loop and drops the socket.
async fn ws_echo(mut socket: WebSocket) {
    while let Some(Ok(msg)) = socket.recv().await {
        // Stop on control-close; only data frames are echoed. recv() already
        // answers Ping with Pong internally, so we just forward the rest.
        if matches!(msg, Message::Close(_)) {
            return;
        }
        if socket.send(msg).await.is_err() {
            return;
        }
    }
}

// ws_hub is a pure broadcast RECEIVER: it never echoes. On each tick it
// pushes the TEXT "payload" frame, while concurrently draining inbound
// frames (control frames, eventual close) so the peer's reads are serviced
// and a close is noticed promptly. Lagged ticks are skipped (drop policy).
async fn ws_hub(mut socket: WebSocket, mut rx: broadcast::Receiver<()>) {
    loop {
        tokio::select! {
            tick = rx.recv() => match tick {
                Ok(()) => {
                    if socket.send(Message::text(BROADCAST_PAYLOAD)).await.is_err() {
                        return;
                    }
                }
                Err(broadcast::error::RecvError::Lagged(_)) => continue,
                Err(broadcast::error::RecvError::Closed) => return,
            },
            inbound = socket.recv() => match inbound {
                Some(Ok(_)) => {} // drain; receivers never echo
                _ => return,      // read error or peer gone
            },
        }
    }
}

// sse_handler answers 200 with the event-stream content type and streams a
// `data: hello\n\n` frame on every tick. A lagging subscriber drops the
// missed ticks (filter_map skips Lagged) rather than back-pressuring the
// publisher. The stream ends when the client disconnects: axum drops the
// response future, which drops the receiver.
pub async fn sse_handler(
    State(state): State<AppState>,
) -> Sse<impl Stream<Item = Result<Event, std::convert::Infallible>>> {
    let rx = state.tick.subscribe();
    let stream = stream::unfold(rx, |mut rx| async move {
        loop {
            match rx.recv().await {
                Ok(()) => {
                    let event = Event::default().data(SSE_EVENT_DATA);
                    return Some((Ok(event), rx));
                }
                Err(broadcast::error::RecvError::Lagged(_)) => continue,
                Err(broadcast::error::RecvError::Closed) => return None,
            }
        }
    });
    // KeepAlive is harmless here (the 1ms publisher already keeps the
    // connection hot) but matches axum's idiomatic SSE setup and guards the
    // idle window before the first subscriber tick.
    Sse::new(stream.boxed()).keep_alive(KeepAlive::default())
}
