"""FastAPI competitor adapter — the blessed fast-path.

Stack:

* FastAPI with ``default_response_class=ORJSONResponse``. The FastAPI
  README explicitly endorses orjson for performance.
* Every handler is ``async def`` — sync handlers would be punted to a
  threadpool by FastAPI and the cell would measure thread-hop overhead
  rather than the framework.
* uvicorn[standard] supplies uvloop and httptools transparently; both
  are selected at launch time via ``--loop uvloop --http httptools`` in
  the launcher script (see ``ansible/roles/python``).

Argv contract (matches the Go adapters):

    python -m app.server --bind 127.0.0.1:8080

The flag accepts ``host:port`` and is mapped onto ``uvicorn.Server`` in
single-process mode. The cluster launcher script prefers ``uvicorn``
directly with ``--workers $(nproc)``; this entry-point is for local
development and for the dev-mac smoke import test.

Readiness banner:

When run via this entry-point, ``ready addr=<bound-addr>`` is printed
to stdout exactly once after the listening socket is open. The cluster
launcher script prints the same banner after polling the bind addr
from outside the uvicorn master, so every worker count produces a
single banner instead of one per worker.

SIGTERM handling: uvicorn's default ``Server.install_signal_handlers``
converts SIGTERM into a graceful shutdown that drains in-flight
requests and closes the listener. We do not override it — verified
clean exit under load on the bench cluster.
"""

from __future__ import annotations

import argparse
import asyncio
import socket
import sys

import uvicorn
from fastapi import FastAPI, Request
from fastapi.responses import ORJSONResponse, PlainTextResponse, Response

from .payload import JSON_1K_PAYLOAD, JSON_64K_PAYLOAD

# Static byte payloads. Hoisted to module scope so each request reuses the
# same immutable bytes object — no per-request allocation, mirrors the Go
# adapters that serve a pre-baked slice from ``servers/common``.
_HELLO_PLAIN: bytes = b"Hello, World!"
_HELLO_JSON: bytes = b'{"message":"Hello, World!"}'
_OK_PLAIN: bytes = b"OK"


app = FastAPI(default_response_class=ORJSONResponse)


@app.get("/", response_class=PlainTextResponse)
async def root() -> Response:
    return Response(content=_HELLO_PLAIN, media_type="text/plain")


@app.get("/json")
async def json_hello() -> Response:
    # Static bytes; bypassing ORJSONResponse so the body is byte-identical
    # to ``common.Endpoints[/json].ResponseBody`` (no orjson re-encoding
    # could drift the field order).
    return Response(content=_HELLO_JSON, media_type="application/json")


@app.get("/json-1k")
async def json_1k() -> Response:
    return Response(content=JSON_1K_PAYLOAD, media_type="application/json")


@app.get("/json-64k")
async def json_64k() -> Response:
    return Response(content=JSON_64K_PAYLOAD, media_type="application/json")


@app.get("/users/{user_id}", response_class=PlainTextResponse)
async def users(user_id: str) -> Response:
    return Response(content=f"User ID: {user_id}".encode(), media_type="text/plain")


@app.post("/upload", response_class=PlainTextResponse)
async def upload(request: Request) -> Response:
    # Drain the body so the body parser is part of the measured cost,
    # matching every other adapter.
    async for _ in request.stream():
        pass
    return Response(content=_OK_PLAIN, media_type="text/plain")


def _parse_bind(bind: str) -> tuple[str, int]:
    """Split ``host:port`` into ``(host, int(port))``.

    IPv6 addresses are accepted in bracketed form (``[::1]:8080``).
    """
    if bind.startswith("["):
        # IPv6 literal — split on closing bracket, then on the colon
        # separating address and port.
        rb = bind.index("]")
        host = bind[1:rb]
        port = int(bind[rb + 2 :])
        return host, port
    host, _, port_s = bind.rpartition(":")
    if not host or not port_s:
        raise ValueError(f"invalid -bind {bind!r}: expected host:port")
    return host, int(port_s)


def main() -> None:
    """Local-dev entry point. The cluster launcher invokes uvicorn directly."""
    parser = argparse.ArgumentParser(prog="probatorium-fastapi")
    parser.add_argument("--bind", default="127.0.0.1:8080")
    args = parser.parse_args()
    host, port = _parse_bind(args.bind)

    # Bind the socket up front so we know the final address (port may be
    # 0 in dev) before announcing readiness. uvicorn supports this via
    # the ``fd`` config knob.
    sock = socket.socket(socket.AF_INET if ":" not in host else socket.AF_INET6,
                         socket.SOCK_STREAM)
    sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    sock.bind((host, port))
    bound_host, bound_port = sock.getsockname()[:2]
    sock.listen(2048)

    config = uvicorn.Config(
        app,
        loop="uvloop",
        http="httptools",
        access_log=False,
        log_level="warning",
        # `fd=` makes uvicorn adopt our pre-bound socket so the printed
        # address below is guaranteed to be the one in use.
        fd=sock.fileno(),
    )
    server = uvicorn.Server(config)

    print(f"ready addr={bound_host}:{bound_port}", flush=True)
    sys.stdout.flush()

    asyncio.run(server.serve())


if __name__ == "__main__":
    main()
