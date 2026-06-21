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

    python -m app.server -bind 127.0.0.1:8080 [-engine h1|h2c]

``-bind`` accepts ``host:port``. ``-engine`` selects the wire protocol:

* ``h1`` (or absent) — plain HTTP/1.1 on uvicorn + uvloop + httptools,
  the tuned fast path, mapped onto ``uvicorn.Server`` in single-process
  mode. The cluster launcher script prefers ``uvicorn`` directly with
  ``--workers $(nproc)``; this entry-point is for local development and
  for the dev-mac smoke import test.
* ``h2c`` — HTTP/2 cleartext, prior-knowledge, no TLS. uvicorn cannot
  speak HTTP/2, so this path launches the same ASGI app under
  **hypercorn** instead. Hypercorn's h11 reader detects the HTTP/2
  connection preface (``PRI * HTTP/2.0``) on a plaintext bind and swaps
  the connection to its H2 protocol with no ALPN / TLS handshake — i.e.
  exactly h2c prior-knowledge. See ``hypercorn/protocol/h11.py``
  (``H2ProtocolAssumedError``) and ``config.ssl_enabled`` (False with no
  certfile ⇒ insecure/cleartext sockets). Single ``--workers``-equivalent
  process; hypercorn's ``asyncio.serve`` runs one worker.

Both long (``--bind``/``--engine``) and short (``-bind``/``-engine``)
spellings are accepted so the launcher shim and the Go orchestrator can
pass either.

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
import os
import socket
import sys

import uvicorn
from fastapi import FastAPI, Request
from fastapi.responses import ORJSONResponse, PlainTextResponse, Response

from .payload import (
    JSON_1K_PAYLOAD,
    JSON_8K_PAYLOAD,
    JSON_16K_PAYLOAD,
    JSON_64K_PAYLOAD,
)

# Static byte payloads. Hoisted to module scope so each request reuses the
# same immutable bytes object — no per-request allocation, mirrors the Go
# adapters that serve a pre-baked slice from ``servers/common``.
_HELLO_PLAIN: bytes = b"Hello, World!"
_HELLO_JSON: bytes = b'{"message":"Hello, World!"}'
_OK_PLAIN: bytes = b"OK"


def _announce_ready(bound_host: str, bound_port: int) -> None:
    """Print the ``ready addr=...`` banner once, unless suppressed.

    The cluster launcher emits the banner from an external TCP probe so
    the count is exactly one regardless of worker count / server. It sets
    ``PROBATORIUM_SUPPRESS_READY=1`` to silence this in-process banner and
    avoid a duplicate. Local-dev runs (no env var) still get the banner.
    """
    if os.environ.get("PROBATORIUM_SUPPRESS_READY") == "1":
        return
    print(f"ready addr={bound_host}:{bound_port}", flush=True)
    sys.stdout.flush()


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


@app.get("/json-8k")
async def json_8k() -> Response:
    return Response(content=JSON_8K_PAYLOAD, media_type="application/json")


@app.get("/json-16k")
async def json_16k() -> Response:
    return Response(content=JSON_16K_PAYLOAD, media_type="application/json")


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


def _bind_socket(host: str, port: int) -> socket.socket:
    """Open a listening socket on ``host:port`` (port 0 ⇒ OS-assigned)."""
    sock = socket.socket(socket.AF_INET if ":" not in host else socket.AF_INET6,
                         socket.SOCK_STREAM)
    sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    sock.bind((host, port))
    sock.listen(2048)
    return sock


def _serve_h1(sock: socket.socket, bound_host: str, bound_port: int) -> None:
    """HTTP/1.1 fast path: uvicorn + uvloop + httptools on the pre-bound fd."""
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

    _announce_ready(bound_host, bound_port)

    asyncio.run(server.serve())


def _serve_h2c(sock: socket.socket, bound_host: str, bound_port: int) -> None:
    """HTTP/2 cleartext (prior-knowledge, no TLS) via hypercorn.

    uvicorn has no HTTP/2 support, so the h2c column runs hypercorn. With
    no certfile/keyfile, ``Config.ssl_enabled`` is False and the bind is
    served on an insecure (cleartext) socket; hypercorn's h11 reader then
    upgrades any connection that opens with the ``PRI * HTTP/2.0`` preface
    straight to HTTP/2 — that is h2c prior-knowledge. uvloop is selected
    via the loop installed before ``serve``.
    """
    try:
        import uvloop
        from hypercorn.asyncio import serve
        from hypercorn.config import Config
    except ImportError as exc:  # pragma: no cover - dep guard
        print(
            f"fastapi-h2: hypercorn/uvloop unavailable for -engine h2c: {exc}",
            file=sys.stderr,
            flush=True,
        )
        raise SystemExit(3) from exc

    config = Config()
    config.bind = [f"{bound_host}:{bound_port}"]
    config.insecure_bind = []
    config.accesslog = None
    config.errorlog = None
    config.loglevel = "WARNING"
    # With no certfile/keyfile, ssl_enabled is False, so `bind` is served on
    # an insecure (cleartext) socket. Advertise h2 first anyway; on cleartext
    # ALPN is never consulted — prior-knowledge keys solely off the preface.
    config.alpn_protocols = ["h2", "http/1.1"]

    # We bound the socket up front to learn the final address (port may be
    # 0). hypercorn's serve() re-binds from `config.bind`, so release ours
    # first to avoid a double-bind on the same address. SO_REUSEADDR was set
    # and this is a single process, so there is no contention.
    sock.close()

    uvloop.install()

    _announce_ready(bound_host, bound_port)

    asyncio.run(serve(app, config))


def main() -> None:
    """Local-dev entry point. The cluster launcher invokes the server directly."""
    parser = argparse.ArgumentParser(prog="probatorium-fastapi")
    parser.add_argument("-bind", "--bind", dest="bind", default="127.0.0.1:8080")
    parser.add_argument(
        "-engine",
        "--engine",
        dest="engine",
        default="h1",
        choices=["h1", "h2c"],
    )
    args = parser.parse_args()
    host, port = _parse_bind(args.bind)

    # Bind the socket up front so we know the final address (port may be
    # 0 in dev) before announcing readiness.
    sock = _bind_socket(host, port)
    bound_host, bound_port = sock.getsockname()[:2]

    if args.engine == "h2c":
        _serve_h2c(sock, bound_host, bound_port)
    else:
        _serve_h1(sock, bound_host, bound_port)


if __name__ == "__main__":
    main()
