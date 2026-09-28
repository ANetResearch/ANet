"""dmax-svc.py — the capabilities dmax-services offers of its own.

Deliberately not an LLM. dmax has GPUs and could rent out inference, but
the point being tested is whether an ordinary service — something a person
already runs — becomes callable across the network without being rewritten
for it. So: image metadata, and text statistics.

    POST /image/inspect  {"image_b64": "..."}  -> bytes, crc32, format[, width, height, pixels]
    POST /text/stats     {"text": "..."}       -> chars, words, lines
    GET  /health                               -> {"status": "ok"}

Answers on the Unix socket given as the only argument (anet-dmax-svc.service:
/run/anet-dmax-svc/backend.sock); the daemon's modules.service points at it
with unix:///run/anet-dmax-svc/backend.sock:/text/stats and so on.
Same computation as the TCP backend it replaces (/data/anet-node/svc/serve.py,
0.1.x), so results are comparable across the upgrade.
"""
import base64
import json
import os
import struct
import sys
import zlib
from http.server import BaseHTTPRequestHandler

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from uds_http import serve  # noqa: E402

MAX_BODY = 8 << 20


def png_size(raw):
    if raw[:8] != b"\x89PNG\r\n\x1a\n" or len(raw) < 24:
        return None
    w, h = struct.unpack(">II", raw[16:24])
    return w, h


class H(BaseHTTPRequestHandler):
    def _send(self, code, obj):
        b = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    def do_GET(self):
        if self.path == "/health":
            return self._send(200, {"status": "ok", "host": "dmax"})
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
        except Exception as e:
            return self._send(400, {"error": f"bad json: {e}"})
        if not isinstance(req, dict):
            return self._send(400, {"error": "arguments must be a JSON object"})
        if self.path == "/image/inspect":
            try:
                raw = base64.b64decode(req.get("image_b64", ""))
            except Exception as e:
                return self._send(400, {"error": f"bad base64: {e}"})
            if not raw:
                return self._send(400, {"error": "image_b64 is required"})
            size = png_size(raw)
            out = {"bytes": len(raw), "crc32": format(zlib.crc32(raw) & 0xFFFFFFFF, "08x"),
                   "format": "png" if size else "unknown"}
            if size:
                out["width"], out["height"] = size
                out["pixels"] = size[0] * size[1]
            return self._send(200, out)
        if self.path == "/text/stats":
            t = str(req.get("text", ""))
            return self._send(200, {"chars": len(t), "words": len(t.split()),
                                    "lines": len(t.splitlines()) or (1 if t else 0)})
        self._send(404, {"error": "not found"})

    def log_message(self, *a):
        pass


if __name__ == "__main__":
    if len(sys.argv) != 2:
        sys.exit("usage: dmax-svc.py SOCKET")
    serve(H, sys.argv[1])
