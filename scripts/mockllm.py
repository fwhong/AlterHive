#!/usr/bin/env python3
"""Minimal OpenAI-compatible mock for AlterHive manager/sensor verification.

Serves POST /v1/chat/completions with a fixed completion and logs every
request, so the verification script can prove the request originated from
the manager process (not the sensor).
"""
import json
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

COMPLETION = (
    "nmap mock scan report for staging-web-01\n"
    "PORT   STATE SERVICE VERSION\n"
    "22/tcp open  ssh     OpenSSH 8.9p1 (MOCKLLM-RENDER)\n"
    "80/tcp open  http    nginx\n"
)

LOG_LOCK = threading.Lock()
REQUESTS = []


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_POST(self):
        if self.path != "/v1/chat/completions":
            self.send_error(404)
            return
        length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(length)
        try:
            payload = json.loads(body)
        except Exception:
            payload = {}
        with LOG_LOCK:
            REQUESTS.append({
                "ts": time.time(),
                "model": payload.get("model"),
                "messages": len(payload.get("messages", [])),
                "auth": self.headers.get("Authorization", ""),
            })
            sys.stdout.write(
                f"[mockllm] completion request model={payload.get('model')} "
                f"messages={len(payload.get('messages', []))}\n")
            sys.stdout.flush()
        resp = {
            "id": "mock-1",
            "object": "chat.completion",
            "model": payload.get("model", "mock-model"),
            "choices": [{
                "index": 0,
                "message": {"role": "assistant", "content": COMPLETION},
                "finish_reason": "stop",
            }],
            "usage": {"prompt_tokens": 10, "completion_tokens": 20, "total_tokens": 30},
        }
        data = json.dumps(resp).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        if self.path == "/v1/models":
            data = json.dumps({"data": [{"id": "mock-model", "owned_by": "mock"}]}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
            return
        if self.path == "/request-count":
            with LOG_LOCK:
                data = json.dumps({"requests": len(REQUESTS)}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
            return
        self.send_error(404)

    def log_message(self, fmt, *args):  # silence default access log
        pass


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 9999
    server = ThreadingHTTPServer(("127.0.0.1", port), Handler)
    print(f"[mockllm] listening on 127.0.0.1:{port}", flush=True)
    server.serve_forever()
