"""Deterministic JSON payloads for /json-1k, /json-8k, /json-16k and /json-64k.

These MUST be byte-identical to the Go reference at
``servers/common/payload.go``. The conformance probe (``cmd/conformance``)
byte-compares response bodies against the Go-generated payload baked into
``servers.common.Endpoints``; any drift here produces a hard failure.

Equivalence contract with Go's ``encoding/json``:

* Compact separators — no whitespace between keys, values, or list items.
  ``orjson.dumps`` is compact by default; that matches Go's
  ``json.Marshal``.
* Key order matches the Go struct declaration (``page``, ``per_page``,
  ``total``, ``total_pages``, ``data``; per-item ``id``, ``name``,
  ``email``, ``status``, ``created_at``). Python 3.7+ ``dict`` preserves
  insertion order, and ``orjson`` honours it on serialise.
* Strings are ASCII; no characters that would trip Go's HTML escaping
  (``<`` ``>`` ``&``) appear in the payload, so the two encoders agree
  on every byte.
* The growth loop appends items until the marshalled length crosses the
  target — same termination predicate as ``generateJSONPayload`` in Go,
  which gives the same item count (8 items for 1 KiB, 583 for 64 KiB).
  The mid-size 8 KiB / 16 KiB payloads bridge the 1k→64k gap so the bench
  keeps differentiating adapters below the 20G fabric ceiling that makes
  the 64k cells NIC-bound (see ``servers/common/payload.go`` JSON8KPayload).
  Exact byte lengths (byte-compared by cluster conformance):
  1k = 1026, 8k = 8286, 16k = 16463, 64k = 65618.

This module is shared verbatim with the fastapi adapter — both serve the
identical bytes so a starlette-vs-fastapi delta reflects framework cost,
never a payload artefact. If a future change introduces a non-ASCII char
or a key Go would HTML-escape, the conformance probe will fail loudly —
keep this module's output passing through ``orjson.dumps`` with no
options set.
"""

from __future__ import annotations

import orjson


def _generate(target_size: int) -> bytes:
    """Build a paginated JSON response of at least ``target_size`` bytes."""
    items: list[dict[str, object]] = []
    resp: dict[str, object] = {
        "page": 1,
        "per_page": 50,
        "total": 1000,
        "total_pages": 20,
        "data": items,
    }
    i = 1
    while True:
        items.append(
            {
                "id": i,
                "name": f"User {i}",
                "email": f"user{i}@example.com",
                "status": "active",
                "created_at": "2024-01-15T09:30:00Z",
            }
        )
        data = orjson.dumps(resp)
        if len(data) >= target_size:
            return data
        i += 1


JSON_1K_PAYLOAD: bytes = _generate(1024)
JSON_8K_PAYLOAD: bytes = _generate(8192)
JSON_16K_PAYLOAD: bytes = _generate(16384)
JSON_64K_PAYLOAD: bytes = _generate(65536)
