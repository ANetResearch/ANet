"""uds_http.py — serve an http.server handler on a Unix domain socket.

The capability backends of the two operations nodes (dmax-svc.py,
cmax-svc.py) were plain HTTPServers on loopback TCP ports. anet 0.2.0 no
longer sends a call to a loopback port unless the module says allow_tcp:
while a backend is down any local account can bind its port and receive
the next call (docs/notes/0030 N1). A socket in a directory only the node's
account can write cannot be taken that way, and the daemon checks the
listener's uid before it writes anything (internal/backendconn).

serve(handler, path) binds path with the process umask (the units set
UMask=0077, so the socket is 0600 and only the node's own account — the
daemon runs as the same account — can connect) and serves until killed.
"""
import os
import socketserver
import stat
import sys
from http.server import BaseHTTPRequestHandler


class _Server(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
    daemon_threads = True

    def get_request(self):
        # BaseHTTPRequestHandler formats client_address[0] in its logs; a
        # Unix socket peer has no address.
        conn, _ = super().get_request()
        return conn, ("unix", 0)


def serve(handler: type, path: str) -> None:
    if not issubclass(handler, BaseHTTPRequestHandler):
        raise TypeError("handler must be a BaseHTTPRequestHandler")
    try:
        st = os.lstat(path)
    except FileNotFoundError:
        pass
    else:
        # A socket left by an earlier run is replaced; anything else at the
        # path is somebody else's file and is not ours to remove.
        if not stat.S_ISSOCK(st.st_mode):
            sys.exit(f"{path} exists and is not a socket")
        os.unlink(path)
    with _Server(path, handler) as srv:
        srv.serve_forever()
