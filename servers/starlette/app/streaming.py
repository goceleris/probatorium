"""WebSocket + SSE streaming surface for the Starlette adapter.

This is the Python/ASGI counterpart to the gorilla_ws reference adapter
(``servers/gorilla_ws/server.go``): the same connection-set broadcast Hub
and publish-to-N-subscribers SSE fan-out, so the streaming cell-columns pair
a Starlette column against the Go baseline on a like-for-like wire contract.

Both surfaces ride HTTP/1.1 (the WS upgrade and the SSE long-poll are H1-only),
so they mount on the uvicorn ``_serve_h1`` fast path. The h2c/hypercorn path is
left untouched — streaming scenarios gate on ``fs.HTTP1`` and skip H2C-only.

Wire constants are byte-identical to the reference: WS hub broadcasts the TEXT
frame ``"payload"``; SSE pushes the frame ``data: hello\\n\\n``; both tick at 1ms.

The two 1ms tickers must run on the live event loop, so they are launched from
a Starlette ``lifespan`` async context manager (one publisher per worker) rather
than at import time.
"""

from __future__ import annotations

import asyncio
import contextlib
from collections.abc import AsyncIterator

from starlette.applications import Starlette
from starlette.requests import Request
from starlette.responses import StreamingResponse
from starlette.websockets import WebSocket, WebSocketDisconnect

# ?mode= selectors — mirror scenarios.StreamMode* and the gorilla_ws adapter so
# the loadgen client drives both adapters identically.
_MODE_WS_HUB = "ws-hub"
_MODE_WS_LARGE_ECHO = "ws-large-echo"

_BROADCAST_PAYLOAD = "payload"
# Pre-format the SSE wire frame once: "data: hello\n\n".
_SSE_FRAME = b"data: hello\n\n"

_PUBLISH_INTERVAL = 0.001  # 1ms, matches streamPublishInterval

# Bounded per-subscriber queue: a slow SSE consumer drops events rather than
# wedging the publish loop (the drop policy the gorilla_ws broker also uses).
_SSE_QUEUE_MAXSIZE = 1024


# --- WebSocket: broadcast hub (the pattern the celeris Hub replaces) ---

# A plain set guarded by the GIL — every ASGI handler runs on the single event
# loop, so the broadcast loop and the (un)register sites never preempt mid-op.
_hub: set[WebSocket] = set()


async def ws_endpoint(websocket: WebSocket) -> None:
    await websocket.accept()
    mode = websocket.query_params.get("mode", "ws-echo")
    if mode == _MODE_WS_HUB:
        await _ws_hub_receiver(websocket)
    else:
        # ws-echo and ws-large-echo are identical: the websockets default
        # max_size is 1 MiB, which already bounds the 64 KiB large-echo
        # payload, so no raised read limit is needed.
        await _ws_echo(websocket)


async def _ws_echo(websocket: WebSocket) -> None:
    # Echo verbatim, preserving the text-vs-binary opcode.
    try:
        while True:
            msg = await websocket.receive()
            if msg["type"] == "websocket.disconnect":
                return
            if "text" in msg:
                await websocket.send_text(msg["text"])
            elif "bytes" in msg:
                await websocket.send_bytes(msg["bytes"])
    except WebSocketDisconnect:
        return


async def _ws_hub_receiver(websocket: WebSocket) -> None:
    # Pure receiver: join the hub, then drain inbound frames (the background
    # loop drives all writes) until the peer goes away. Never echo.
    _hub.add(websocket)
    try:
        while True:
            msg = await websocket.receive()
            if msg["type"] == "websocket.disconnect":
                return
    except WebSocketDisconnect:
        return
    finally:
        _hub.discard(websocket)


async def _broadcast_loop() -> None:
    while True:
        await asyncio.sleep(_PUBLISH_INTERVAL)
        # Snapshot so a disconnect mid-iteration cannot mutate the live set.
        for ws in tuple(_hub):
            try:
                await ws.send_text(_BROADCAST_PAYLOAD)
            except Exception:  # drop a dead conn rather than abort the fan-out
                _hub.discard(ws)


# --- SSE: subscriber-queue broker (the pattern the celeris sse.Broker replaces) ---

_sse_subscribers: set[asyncio.Queue[bytes]] = set()


async def sse_endpoint(request: Request) -> StreamingResponse:
    queue: asyncio.Queue[bytes] = asyncio.Queue(maxsize=_SSE_QUEUE_MAXSIZE)
    _sse_subscribers.add(queue)

    async def event_gen() -> AsyncIterator[bytes]:
        try:
            while True:
                if await request.is_disconnected():
                    return
                yield await queue.get()
        finally:
            _sse_subscribers.discard(queue)

    return StreamingResponse(
        event_gen(),
        media_type="text/event-stream",
        headers={"Cache-Control": "no-cache", "Connection": "keep-alive"},
    )


async def _publish_loop() -> None:
    while True:
        await asyncio.sleep(_PUBLISH_INTERVAL)
        for queue in tuple(_sse_subscribers):
            try:
                queue.put_nowait(_SSE_FRAME)
            except asyncio.QueueFull:  # slow subscriber: drop, never block fan-out
                pass


# --- lifespan wiring -------------------------------------------------------

_tasks: list[asyncio.Task[None]] = []


def start_streaming() -> None:
    """Launch the 1ms WS-broadcast and SSE-publish tickers on the live loop."""
    _tasks.append(asyncio.create_task(_broadcast_loop()))
    _tasks.append(asyncio.create_task(_publish_loop()))


async def stop_streaming() -> None:
    """Cancel the tickers on shutdown."""
    for task in _tasks:
        task.cancel()
    for task in _tasks:
        with contextlib.suppress(asyncio.CancelledError):
            await task
    _tasks.clear()


@contextlib.asynccontextmanager
async def lifespan(_app: Starlette) -> AsyncIterator[None]:
    start_streaming()
    try:
        yield
    finally:
        await stop_streaming()
