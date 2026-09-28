"""cmax-svc.py — the capability cmax-anet4 sells: sha256 of a text.

    POST <any path> {"text": "..."} -> {"digest": "<sha256 hex>", "length": <chars>}

prodtest section 5 checks the digest of "anet" byte for byte, so the result
shape is the one the 0.1.x TCP backend (/root/anet4/svc/svc.py) returned.
Answers on the Unix socket given as the only argument (anet4-svc.service:
/run/anet4-svc/backend.sock); text.digest and text.digest.paid both point at
unix:///run/anet4-svc/backend.sock.
"""
import hashlib
import json
import os
import sys
from http.server import BaseHTTPRequestHandler

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from uds_http import serve  # noqa: E402

MAX_BODY = 8 << 20


class H(BaseHTTPRequestHandler):
    def _send(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path == "/health":
            return self._send(200, {"status": "ok", "host": "cmax"})
        self._send(404, {"error": "not found"})

    def do_POST(self):
        try:
            n = int(self.headers.get("Content-Length", 0))
        except ValueError:
            return self._send(400, {"error": "bad Content-Length"})
        if n < 0 or n > MAX_BODY:
            return self._send(413, {"error": "body too large"})
        try:
            req = json.loads(self.rfile.read(n) or b"{}")
        except Exception:
            req = {}
        if not isinstance(req, dict):
            req = {}
        text = str(req.get("text", ""))
        self._send(200, {"digest": hashlib.sha256(text.encode()).hexdigest(),
                         "length": len(text)})

    def log_message(self, *a):
        pass


if __name__ == "__main__":
    if len(sys.argv) != 2:
        sys.exit("usage: cmax-svc.py SOCKET")
    serve(H, sys.argv[1])
