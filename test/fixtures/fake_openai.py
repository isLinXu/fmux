#!/usr/bin/env python3
"""A minimal OpenAI-compatible server used by the supervision tests.

It exists so the gateway's process management can be exercised without a real
18 GB checkpoint: the point under test is the lifecycle (start, health probe,
reuse, idle reap), not the inference.

Usage: fake_openai.py <port> [model-name]
"""
import json
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


MODEL = "fake-model"


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def _send(self, code, ctype, body: bytes):
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        path = self.path.split("?")[0].rstrip("/")
        if path in ("/health", "/v1/models"):
            if path == "/health":
                self._send(200, "text/plain", b"ok")
                return
            self._send(200, "application/json", json.dumps({
                "object": "list",
                "data": [{"id": MODEL, "object": "model", "owned_by": "fake"}],
            }).encode())
            return
        self._send(404, "text/plain", b"not found")

    def do_POST(self):
        if not self.path.startswith("/v1/chat/completions"):
            self._send(404, "text/plain", b"not found")
            return
        n = int(self.headers.get("Content-Length", 0))
        req = json.loads(self.rfile.read(n) or b"{}")
        text = "served by the fake backend"
        if req.get("stream"):
            chunk = {
                "id": "chatcmpl-fake", "object": "chat.completion.chunk",
                "created": int(time.time()), "model": req.get("model"),
                "choices": [{"index": 0, "delta": {"role": "assistant", "content": text},
                             "finish_reason": None}],
            }
            body = ("data: " + json.dumps(chunk) + "\n\n").encode()
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        body = json.dumps({
            "id": "chatcmpl-fake", "object": "chat.completion",
            "created": int(time.time()), "model": req.get("model"),
            "choices": [{"index": 0, "message": {"role": "assistant", "content": text},
                         "finish_reason": "stop"}],
            "usage": {"prompt_tokens": 3, "completion_tokens": 4, "total_tokens": 7},
        }).encode()
        self._send(200, "application/json", body)


def main():
    global MODEL
    port = int(sys.argv[1])
    if len(sys.argv) > 2:
        MODEL = sys.argv[2]
    ThreadingHTTPServer(("127.0.0.1", port), Handler).serve_forever()


if __name__ == "__main__":
    main()
