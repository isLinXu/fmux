#!/usr/bin/env python3
"""Capture the exact HTTP bodies the `fm` CLI sends, as golden test fixtures.

Usage:
    python3 scripts/capture_fm.py [--out test/conformance/golden]

Why this exists: fm's wire format has quirks that are not documented anywhere
(a system message with no `content`, an always-present empty `tools` array,
`stream: true` even under `--no-stream`). The only reliable way to keep the
gateway honest is to record what fm actually sends and diff against it.

Requires `fm` on PATH and the macOS Foundation Models CLI installed.
"""

from __future__ import annotations

import argparse
import json
import os
import struct
import subprocess
import sys
import threading
import zlib
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path

REQUESTS: list[dict] = []


def sample_value(schema: dict):
    """Build a zero-value instance so fm's structured-output validation passes."""
    typ = schema.get("type")
    if typ == "object":
        return {
            k: sample_value(v)
            for k, v in (schema.get("properties") or {}).items()
            if isinstance(v, dict)
        }
    if typ == "array":
        items = schema.get("items")
        return [sample_value(items)] if isinstance(items, dict) else []
    if typ == "integer":
        return 0
    if typ == "number":
        return 0.0
    if typ == "boolean":
        return False
    return ""


def reply_for(body: dict) -> str:
    fmt = body.get("response_format") or {}
    spec = fmt.get("json_schema") or {}
    schema = spec.get("schema")
    if isinstance(schema, dict):
        return json.dumps(sample_value(schema))
    return "pong"


def make_png(path: Path, w: int = 8, h: int = 8) -> None:
    def chunk(tag: bytes, data: bytes) -> bytes:
        return (
            struct.pack(">I", len(data))
            + tag
            + data
            + struct.pack(">I", zlib.crc32(tag + data) & 0xFFFFFFFF)
        )

    raw = b"".join(b"\x00" + b"\xe0\x30\x30" * w for _ in range(h))
    png = (
        b"\x89PNG\r\n\x1a\n"
        + chunk(b"IHDR", struct.pack(">IIBBBBB", w, h, 8, 2, 0, 0, 0))
        + chunk(b"IDAT", zlib.compress(raw))
        + chunk(b"IEND", b"")
    )
    path.write_bytes(png)


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):  # silence
        pass

    def _send(self, code: int, ctype: str, body: bytes) -> None:
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self) -> None:
        if self.path.rstrip("/").endswith("/models"):
            body = json.dumps(
                {
                    "object": "list",
                    "data": [{"id": "cap-model", "object": "model", "owned_by": "cap"}],
                }
            ).encode()
            self._send(200, "application/json", body)
        else:
            self._send(404, "text/plain", b"nope")

    def do_POST(self) -> None:
        n = int(self.headers.get("Content-Length", 0))
        raw = self.rfile.read(n)
        try:
            body = json.loads(raw)
        except Exception:
            body = {"__raw__": raw.decode("utf-8", "replace")}
        REQUESTS.append(
            {
                "path": self.path,
                "headers": {
                    k: v
                    for k, v in self.headers.items()
                    if k.lower() not in ("content-length", "host", "connection")
                },
                "body": body,
            }
        )

        text = reply_for(body)
        if body.get("stream"):
            chunk = json.dumps(
                {
                    "id": "chatcmpl-cap",
                    "object": "chat.completion.chunk",
                    "created": 1700000000,
                    "model": body.get("model"),
                    "choices": [
                        {
                            "index": 0,
                            "delta": {"role": "assistant", "content": text},
                            "finish_reason": "stop",
                        }
                    ],
                }
            )
            usage = json.dumps(
                {
                    "id": "chatcmpl-cap",
                    "object": "chat.completion.chunk",
                    "created": 1700000000,
                    "model": body.get("model"),
                    "choices": [],
                    "usage": {
                        "prompt_tokens": 3,
                        "completion_tokens": 1,
                        "total_tokens": 4,
                    },
                }
            )
            payload = f"data: {chunk}\n\ndata: {usage}\n\ndata: [DONE]\n\n".encode()
            self._send(200, "text/event-stream", payload)
        else:
            payload = json.dumps(
                {
                    "id": "chatcmpl-cap",
                    "object": "chat.completion",
                    "created": 1700000000,
                    "model": body.get("model"),
                    "choices": [
                        {
                            "index": 0,
                            "message": {"role": "assistant", "content": text},
                            "finish_reason": "stop",
                        }
                    ],
                    "usage": {
                        "prompt_tokens": 3,
                        "completion_tokens": 1,
                        "total_tokens": 4,
                    },
                }
            ).encode()
            self._send(200, "application/json", payload)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", default="test/conformance/golden")
    ap.add_argument("--port", type=int, default=19781)
    args = ap.parse_args()

    out = Path(args.out)
    for d in (out, Path("/tmp/fmux-capture")):
        try:
            d.mkdir(parents=True, exist_ok=True)
        except FileExistsError:
            pass
    work = Path("/tmp/fmux-capture")
    img = work / "t.png"
    make_png(img)

    schema = work / "schema.json"
    subprocess.run(
        ["fm", "schema", "object", "--name", "Person", "--string", "name", "--int", "age"],
        check=True,
        stdout=schema.open("wb"),
        stderr=subprocess.DEVNULL,
    )

    server = HTTPServer(("127.0.0.1", args.port), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()

    base = f"http://127.0.0.1:{args.port}/v1"
    cases: list[tuple[str, list[str]]] = [
        ("plain", ["respond", "--no-stream", "--model-provider", base, "-m", "cap-model", "hi"]),
        (
            "instructions",
            ["respond", "--no-stream", "--model-provider", base, "-m", "cap-model",
             "--instructions", "be terse", "hi"],
        ),
        (
            "schema",
            ["respond", "--no-stream", "--model-provider", base, "-m", "cap-model",
             "--schema", str(schema), "zhang 30"],
        ),
        (
            "image",
            ["respond", "--no-stream", "--model-provider", base, "-m", "cap-model",
             "--image", str(img), "--text", "what is it"],
        ),
        (
            "tool",
            ["respond", "--no-stream", "--model-provider", base, "-m", "cap-model",
             "--tool", "ocr", "hi"],
        ),
        (
            "header",
            ["respond", "--no-stream", "--model-provider", base, "-m", "cap-model",
             "--model-provider-header", "X-Test: 1", "hi"],
        ),
        ("stream", ["respond", "--model-provider", base, "-m", "cap-model", "hi"]),
    ]

    failed = []
    for name, argv in cases:
        before = len(REQUESTS)
        proc = subprocess.run(["fm", *argv], capture_output=True, text=True, timeout=120)
        if proc.returncode != 0:
            failed.append((name, proc.stderr.strip()[:200]))
            continue
        if len(REQUESTS) == before:
            failed.append((name, "no request reached the capture server"))
            continue
        record = REQUESTS[-1]
        (out / f"{name}.json").write_text(
            json.dumps(record["body"], indent=2, ensure_ascii=False) + "\n"
        )
        print(f"  captured {name:12s} -> {out / (name + '.json')}")

    server.shutdown()

    if failed:
        print("\nfailed cases:", file=sys.stderr)
        for name, why in failed:
            print(f"  {name}: {why}", file=sys.stderr)
        return 1
    print(f"\n{len(cases)} golden files written to {out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
