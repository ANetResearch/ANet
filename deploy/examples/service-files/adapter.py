"""adapter.py - a minimal image-describing service for the anet service module.

    POST /describe  {"args": {...}, "files": [{"name","mime","data_b64"}]}
        -> {"width": int, "height": int, "format": "png"|"jpeg"|"webp"}

The daemon calls this adapter on a Unix domain socket (argument 1) with a
bearer token read from the file in argument 2. Properties kept on purpose:

  * It listens on the Unix socket only, never on a TCP port. The socket is
    created under the process umask (the unit sets UMask=0077).
  * It reads width and height from the image header and refuses an image
    above MAX_PIXELS before anything decodes it.
  * It writes no file and logs nothing about a call. Input and result stay
    in memory for the length of the request.

Replace describe() with the real work. Keep the three properties above.
"""
import base64
import hmac
import json
import os
import socketserver
import stat
import struct
import sys
from http.server import BaseHTTPRequestHandler

MAX_BODY = 8 << 20          # request body, bytes
MAX_PIXELS = 16_000_000     # width * height, checked on the header


def header_size(data):
    """(format, width, height) from the image header, or None."""
    if data[:8] == b"\x89PNG\r\n\x1a\n" and len(data) >= 24:
        w, h = struct.unpack(">II", data[16:24])
        return "png", w, h
    if data[:2] == b"\xff\xd8":
        i = 2
        while i + 9 < len(data):
            if data[i] != 0xFF:
                i += 1
                continue
            m = data[i + 1]
            if m in (0xD8, 0x01) or 0xD0 <= m <= 0xD7:
                i += 2
                continue
            seg = struct.unpack(">H", data[i + 2:i + 4])[0]
            if m in (0xC0, 0xC1, 0xC2, 0xC3, 0xC5, 0xC6, 0xC7, 0xC9, 0xCA, 0xCB, 0xCD, 0xCE, 0xCF):
                h, w = struct.unpack(">HH", data[i + 5:i + 9])
                return "jpeg", w, h
            i += 2 + seg
        return None
    if data[:4] == b"RIFF" and data[8:12] == b"WEBP" and len(data) >= 30:
        kind = data[12:16]
        if kind == b"VP8X":
            w = 1 + int.from_bytes(data[24:27], "little")
            h = 1 + int.from_bytes(data[27:30], "little")
            return "webp", w, h
        if kind == b"VP8 " and data[23:26] == b"\x9d\x01\x2a":
            w, h = struct.unpack("<HH", data[26:30])
            return "webp", w & 0x3FFF, h & 0x3FFF
        if kind == b"VP8L" and data[20] == 0x2F:
            b = int.from_bytes(data[21:25], "little")
            return "webp", (b & 0x3FFF) + 1, ((b >> 14) & 0x3FFF) + 1
    return None


def describe(data):
    info = header_size(data)
    if info is None:
        return 415, {"error": "unsupported image"}
    fmt, w, h = info
    if w <= 0 or h <= 0 or w * h > MAX_PIXELS:
        return 413, {"error": "image too large"}
    return 200, {"width": w, "height": h, "format": fmt}


def make_handler(token):
    class H(BaseHTTPRequestHandler):
        def _send(self, code, obj):
            body = json.dumps(obj).encode()
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def _authorized(self):
            got = self.headers.get("Authorization", "")
            return hmac.compare_digest(got.encode(), ("Bearer " + token).encode())

        def do_GET(self):
            if self.path == "/health" and self._authorized():
                return self._send(200, {"status": "ok"})
            self._send(404, {"error": "not found"})

        def do_POST(self):
            if not self._authorized():
                return self._send(401, {"error": "unauthorized"})
            if self.path != "/describe":
                return self._send(404, {"error": "not found"})
            try:
                n = int(self.headers.get("Content-Length", ""))
            except ValueError:
                return self._send(400, {"error": "bad Content-Length"})
            if n < 0 or n > MAX_BODY:
                return self._send(413, {"error": "body too large"})
            try:
                req = json.loads(self.rfile.read(n))
                files = req["files"]
                if not isinstance(files, list) or len(files) != 1:
                    raise ValueError
                data = base64.b64decode(files[0]["data_b64"], validate=True)
            except Exception:
                return self._send(400, {"error": "bad request"})
            self._send(*describe(data))

        def log_message(self, *a):
            pass

    return H


class Server(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
    daemon_threads = True

    def get_request(self):
        conn, _ = super().get_request()
        return conn, ("unix", 0)


def main():
    if len(sys.argv) != 3:
        sys.exit("usage: adapter.py SOCKET TOKEN_FILE")
    sock, tok_file = sys.argv[1], sys.argv[2]
    with open(tok_file) as f:
        token = f.read().strip()
    if not token:
        sys.exit("token file is empty")
    try:
        st = os.lstat(sock)
    except FileNotFoundError:
        pass
    else:
        if not stat.S_ISSOCK(st.st_mode):
            sys.exit(sock + " exists and is not a socket")
        os.unlink(sock)
    with Server(sock, make_handler(token)) as srv:
        srv.serve_forever()


if __name__ == "__main__":
    main()
