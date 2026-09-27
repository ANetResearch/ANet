#!/usr/bin/env python3
"""canary.py — the SI-1 canary tooling behind scripts/joint.sh (A2A-DESIGN §1 SI-1, §17).

SI-1 says the hub process, its disk (WAL and backups included), the hub admin's data directory and every
HTTP response of either never hold task content. The joint run proves it the only way a claim about
"nowhere" can be proved: it puts unique random strings (canaries) into every kind of content — a prose
goal, chat turns, attachment bytes, capability arguments, a paid call, a call to an official agent — and
then searches every byte the hub side has for them. This file is that search, plus the tap that records
what the hub was sent and what it answered, plus the structured check of the settlement bodies.

  canary.py scan   --canaries FILE [--out REPORT] [--label NAME] [--expect BASENAME]… [--want LABEL]… PATH…
  canary.py tap    --listen HOST:PORT --upstream URL --dir DIR
  canary.py settle --dir TAPDIR [--hub-db PATH] [--out REPORT]

scan: every regular file under each PATH (symlinks are not followed), searched for each canary as raw
bytes, as lower- and upper-case hex, as standard and URL-safe base64 at each of the three alignments, and
as base64 of those base64 forms. A gzip file is searched decompressed as well; an SQLite database is
also read table by table (a value that spills onto overflow pages is not contiguous in the file), from a
private copy that includes its -wal. Exit 0 when nothing was found, 1 when a canary was found, 2 when
nothing could be scanned or an --expect file was not among the scanned. With --want the sense flips: exit
0 when each wanted canary label was found (the positive control that shows the search would see it).

tap: a reverse proxy the canary nodes use as their hub. Every request and response body is appended raw to
DIR/traffic.log (so a byte search of that file is a byte search of everything the hub was told and
answered for those nodes), with a line per exchange in DIR/index.jsonl; /x402/settle exchanges are also
kept one file each under DIR/settle for the structured check. Accept-Encoding is not forwarded, so every
response body is recorded as the bytes it means.

settle: each captured /x402/settle request body must be x402 v2's {x402Version, paymentPayload,
paymentRequirements} with no field outside the x402 v2 objects, and resource, description and extra
empty wherever they appear (A2A-DESIGN SI-1, X4); at least one must have been answered success:true.
With --hub-db the hub's settlement tables are also checked to have no such columns and at least one row.
"""

import argparse
import base64
import binascii
import gzip
import json
import os
import shutil
import sys
import tempfile
import threading
import time

MAX_FILE = 512 << 20  # larger files are reported, not read

try:  # absent from some minimal Python builds; the raw byte search does not need it
    import sqlite3
except ImportError:  # pragma: no cover
    sqlite3 = None


# ── needles ──────────────────────────────────────────────────────

def load_canaries(path):
    """label<TAB>value per line; a value is at least 16 bytes."""
    out = []
    try:
        f = open(path, encoding="utf-8")
    except OSError as e:
        _fatal("canaries: %s" % e)
    with f:
        for n, line in enumerate(f, 1):
            line = line.rstrip("\n")
            if not line.strip():
                continue
            label, sep, value = line.partition("\t")
            if not sep or len(value) < 16:
                _fatal("%s:%d is not 'label<TAB>value' with a value of 16+ bytes" % (path, n))
            out.append((label, value))
    if not out:
        _fatal("%s holds no canaries" % path)
    return out


def _fatal(msg):
    """Exit 2: a usage or input error, which must not read as "a canary was found" (exit 1)."""
    sys.stderr.write("canary.py: %s\n" % msg)
    sys.exit(2)


def _b64_aligned(raw, enc):
    """The encodings of raw that appear in enc(stream) wherever raw sits in the stream.

    Base64 maps 3-byte groups to 4 characters, so which characters raw becomes depends on where it starts
    modulo 3. Dropping the first j bytes (j = 0, 1, 2) puts the rest on a group boundary in one of the three
    cases; only whole groups are kept, since the last characters also depend on what follows."""
    out = []
    for j in range(3):
        k = (len(raw) - j) // 3 * 3
        if k >= 12:
            out.append(enc(raw[j:j + k]))
    return out


def needles_for(canaries):
    """(label, variant, bytes) for every form a canary is searched in; duplicates removed."""
    seen, out = set(), []

    def add(label, variant, b):
        if len(b) >= 16 and b not in seen:
            seen.add(b)
            out.append((label, variant, b))

    for label, value in canaries:
        raw = value.encode("utf-8")
        add(label, "raw", raw)
        add(label, "hex", binascii.hexlify(raw))
        add(label, "HEX", binascii.hexlify(raw).upper())
        std = _b64_aligned(raw, base64.b64encode)
        for i, b in enumerate(std):
            add(label, "base64/%d" % i, b)
            for k, bb in enumerate(_b64_aligned(b, base64.b64encode)):
                add(label, "base64(base64/%d)/%d" % (i, k), bb)
        for i, b in enumerate(_b64_aligned(raw, base64.urlsafe_b64encode)):
            add(label, "base64url/%d" % i, b)
    return out


def find_all(data, needles, where, hits, extra=None):
    for label, variant, b in needles:
        i = data.find(b)
        if i >= 0:
            h = {"canary": label, "variant": variant, "where": where, "offset": i}
            if extra:
                h.update(extra)
            hits.append(h)


# ── scan ─────────────────────────────────────────────────────────

def sqlite_rows(path, errors):
    """Every value of every table of the database at path, read from a private copy (with its -wal, so
    committed pages not yet checkpointed are included). Yields (table, column, rowid, value)."""
    if sqlite3 is None:
        errors.append("%s: python3 has no sqlite3 module; searched as raw bytes only" % path)
        return
    tmp = tempfile.mkdtemp(prefix="canary-sqlite-")
    try:
        copy = os.path.join(tmp, "db")
        shutil.copyfile(path, copy)
        if os.path.isfile(path + "-wal"):
            shutil.copyfile(path + "-wal", copy + "-wal")
        con = sqlite3.connect(copy)
        con.text_factory = bytes
        try:
            tables = [r[0].decode() if isinstance(r[0], bytes) else r[0] for r in
                      con.execute("SELECT name FROM sqlite_master WHERE type='table'")]
            for t in tables:
                q = '"' + t.replace('"', '""') + '"'
                try:
                    cur = con.execute("SELECT rowid, * FROM %s" % q)
                except sqlite3.Error:
                    try:
                        cur = con.execute("SELECT NULL, * FROM %s" % q)
                    except sqlite3.Error as e:
                        errors.append("%s: table %s: %s" % (path, t, e))
                        continue
                cols = [d[0] for d in cur.description][1:]
                for row in cur:
                    for c, v in zip(cols, row[1:]):
                        yield t, c, row[0], v
        finally:
            con.close()
    except (sqlite3.Error, OSError) as e:
        errors.append("%s: sqlite: %s" % (path, e))
    finally:
        shutil.rmtree(tmp, ignore_errors=True)


def scan_file(path, needles, hits, stats, errors):
    try:
        size = os.path.getsize(path)
    except OSError as e:
        errors.append("%s: %s" % (path, e))
        return
    if size > MAX_FILE:
        errors.append("%s: %d bytes, larger than the scan limit; not read" % (path, size))
        return
    try:
        with open(path, "rb") as f:
            data = f.read()
    except OSError as e:
        errors.append("%s: %s" % (path, e))
        return
    stats["files"] += 1
    stats["bytes"] += len(data)
    stats["names"].add(os.path.basename(path))
    find_all(data, needles, path, hits)
    if data[:2] == b"\x1f\x8b":
        try:
            plain = gzip.decompress(data)
            if len(plain) <= MAX_FILE:
                find_all(plain, needles, path + " (gunzipped)", hits)
        except (OSError, EOFError, ValueError) as e:
            errors.append("%s: gzip: %s" % (path, e))
    if data[:16] == b"SQLite format 3\x00":
        stats["sqlite"] += 1
        for table, col, rowid, v in sqlite_rows(path, errors):
            if isinstance(v, str):
                v = v.encode("utf-8", "surrogateescape")
            if isinstance(v, (bytes, bytearray)) and v:
                find_all(bytes(v), needles, "%s [table %s, column %s, rowid %s]" % (path, table, col, rowid), hits)


def cmd_scan(a):
    canaries = load_canaries(a.canaries)
    needles = needles_for(canaries)
    hits, errors = [], []
    stats = {"files": 0, "bytes": 0, "sqlite": 0, "names": set()}
    for p in a.paths:
        if os.path.islink(p):
            continue
        if os.path.isfile(p):
            scan_file(p, needles, hits, stats, errors)
        elif os.path.isdir(p):
            for root, dirs, files in os.walk(p, followlinks=False):
                dirs.sort()
                for fn in sorted(files):
                    fp = os.path.join(root, fn)
                    if os.path.isfile(fp) and not os.path.islink(fp):
                        scan_file(fp, needles, hits, stats, errors)
        else:
            errors.append("%s: not found" % p)
    missing = [e for e in a.expect if e not in stats["names"]]
    report = {
        "label": a.label, "paths": a.paths, "canaries": len(canaries), "needles": len(needles),
        "files": stats["files"], "bytes": stats["bytes"], "sqlite_databases": stats["sqlite"],
        "hits": hits, "errors": errors, "expected_missing": missing,
    }
    if a.out:
        with open(a.out, "w") as f:
            json.dump(report, f, indent=1)
    found = sorted({h["canary"] for h in hits})
    if a.want:
        lacking = [w for w in a.want if w not in found]
        print("%s: %d files, %d bytes, %d databases; found %s%s" % (
            a.label, stats["files"], stats["bytes"], stats["sqlite"], ",".join(found) or "nothing",
            "; not found: " + ",".join(lacking) if lacking else ""))
        return 0 if not lacking and stats["files"] else 1
    if stats["files"] == 0 or missing:
        print("%s: nothing to scan%s" % (a.label, " (missing: %s)" % ",".join(missing) if missing else ""))
        return 2
    if hits:
        where = sorted({h["where"] for h in hits})
        print("%s: %d hits of %s in %s" % (a.label, len(hits), ",".join(found), "; ".join(where[:6])))
        return 1
    print("%s: 0 hits in %d files, %d bytes, %d databases (%d canaries, %d forms)%s" % (
        a.label, stats["files"], stats["bytes"], stats["sqlite"], len(canaries), len(needles),
        "; %d read errors, see %s" % (len(errors), a.out or "--out") if errors else ""))
    return 0


# ── tap ──────────────────────────────────────────────────────────

# Not forwarded in either direction: hop-by-hop headers, the length (re-framed here), and Accept-Encoding,
# so that no response body reaches the record compressed.
_HOP = {"connection", "keep-alive", "proxy-connection", "proxy-authenticate", "proxy-authorization", "te",
        "trailer", "trailers", "transfer-encoding", "upgrade", "accept-encoding", "host", "content-length"}


def cmd_tap(a):
    import http.client
    import http.server
    import urllib.parse

    up = urllib.parse.urlsplit(a.upstream)
    if up.scheme != "http" or not up.hostname:
        _fatal("tap: --upstream must be http://host:port")
    uhost, uport = up.hostname, up.port or 80
    lhost, _, lport = a.listen.rpartition(":")
    os.makedirs(os.path.join(a.dir, "settle"), exist_ok=True)
    lock = threading.Lock()
    state = {"n": 0}
    log = open(os.path.join(a.dir, "traffic.log"), "ab")
    idx = open(os.path.join(a.dir, "index.jsonl"), "a")

    def record(method, path, req_headers, body, status, resp_headers, rbody):
        with lock:
            state["n"] += 1
            n = state["n"]
            head = "\n=== %d %s %s -> %d\n--- request headers\n" % (n, method, path, status)
            log.write(head.encode("utf-8", "replace"))
            for k, v in req_headers:
                log.write(("%s: %s\n" % (k, v)).encode("utf-8", "replace"))
            log.write(b"--- request body\n" + body + b"\n--- response headers\n")
            for k, v in resp_headers:
                log.write(("%s: %s\n" % (k, v)).encode("utf-8", "replace"))
            log.write(b"--- response body\n" + rbody + b"\n")
            log.flush()
            idx.write(json.dumps({"n": n, "t": time.time(), "method": method, "path": path, "status": status,
                                  "request_bytes": len(body), "response_bytes": len(rbody)}) + "\n")
            idx.flush()
            if path.split("?", 1)[0] == "/x402/settle":
                base = os.path.join(a.dir, "settle", "%06d" % n)
                with open(base + ".req", "wb") as f:
                    f.write(body)
                with open(base + ".resp", "wb") as f:
                    f.write(rbody)

    class Handler(http.server.BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, *args):
            pass

        def _read_body(self):
            if "chunked" in self.headers.get("Transfer-Encoding", "").lower():
                parts = []
                while True:
                    line = self.rfile.readline()
                    size = int(line.split(b";", 1)[0].strip() or b"0", 16)
                    if size == 0:
                        while self.rfile.readline() not in (b"\r\n", b"\n", b""):
                            pass
                        return b"".join(parts)
                    parts.append(self.rfile.read(size))
                    self.rfile.readline()
            n = int(self.headers.get("Content-Length") or 0)
            return self.rfile.read(n) if n > 0 else b""

        def _proxy(self):
            body = self._read_body()
            fwd = [(k, v) for k, v in self.headers.items() if k.lower() not in _HOP]
            try:
                c = http.client.HTTPConnection(uhost, uport, timeout=a.timeout)
                send_body = body if (body or self.command in ("POST", "PUT", "PATCH")) else None
                c.request(self.command, self.path, body=send_body, headers=dict(fwd))
                r = c.getresponse()
                rbody = r.read()
                status, reason, rh = r.status, r.reason, r.getheaders()
                c.close()
            except Exception as e:  # the hub is down or timed out: say so, as a gateway would
                status, reason = 502, "Bad Gateway"
                rh = [("Content-Type", "application/json")]
                rbody = json.dumps({"error": "canary tap: upstream: %s" % e}).encode()
            record(self.command, self.path, fwd, body, status, rh, rbody)
            self.send_response(status, reason)
            for k, v in rh:
                if k.lower() not in _HOP:
                    self.send_header(k, v)
            self.send_header("Content-Length", str(len(rbody)))
            self.end_headers()
            if self.command != "HEAD":
                self.wfile.write(rbody)

        do_GET = do_POST = do_PUT = do_PATCH = do_DELETE = do_HEAD = do_OPTIONS = _proxy

    class Server(http.server.ThreadingHTTPServer):
        daemon_threads = True
        allow_reuse_address = True

    srv = Server((lhost, int(lport)), Handler)
    with open(os.path.join(a.dir, "ready"), "w") as f:
        f.write("%s:%d\n" % srv.server_address[:2])
    srv.serve_forever()


# ── settle ───────────────────────────────────────────────────────

# x402 v2 objects as the hub's facilitator takes them (ANetCore payment.FacilitatorRequest): nothing else
# may ride along, since anything else is something the hub would learn about the work.
_TOP = {"x402Version", "paymentPayload", "paymentRequirements"}
_PAYLOAD = {"x402Version", "accepted", "payload", "extensions"}
_REQUIREMENTS = {"scheme", "network", "amount", "asset", "payTo", "maxTimeoutSeconds", "extra"}
_WORK_FIELDS = ("resource", "description", "extra")


def _empty(v):
    return v is None or v == "" or v == {} or v == []


def settle_violations(body):
    """What is wrong with one /x402/settle request body, as a list of sentences; [] when nothing is."""
    out = []
    try:
        d = json.loads(body)
    except ValueError as e:
        return ["not JSON: %s" % e]
    if not isinstance(d, dict):
        return ["not a JSON object"]
    for k in sorted(set(d) - _TOP):
        out.append("unexpected top-level field %r" % k)
    pp, pr = d.get("paymentPayload"), d.get("paymentRequirements")
    if not isinstance(pr, dict):
        out.append("paymentRequirements missing or not an object")
    else:
        for k in sorted(set(pr) - _REQUIREMENTS):
            out.append("paymentRequirements carries %r" % k)
    if not isinstance(pp, dict):
        out.append("paymentPayload missing or not an object")
    else:
        for k in sorted(set(pp) - _PAYLOAD):
            out.append("paymentPayload carries %r" % k)
        if not _empty(pp.get("extensions")):
            out.append("paymentPayload.extensions is not empty")
        acc = pp.get("accepted")
        if isinstance(acc, dict):
            for k in sorted(set(acc) - _REQUIREMENTS):
                out.append("paymentPayload.accepted carries %r" % k)
        inner = pp.get("payload")
        if not isinstance(inner, dict) or set(inner) != {"authorization"}:
            out.append("paymentPayload.payload is not exactly {authorization}: %s" %
                       (sorted(inner) if isinstance(inner, dict) else type(inner).__name__))

    def walk(v, path):
        if isinstance(v, dict):
            for k, x in v.items():
                if k in _WORK_FIELDS and not _empty(x):
                    out.append("%s.%s is %s, not empty" % (path, k, json.dumps(x)[:80]))
                walk(x, path + "." + k)
        elif isinstance(v, list):
            for i, x in enumerate(v):
                walk(x, "%s[%d]" % (path, i))

    walk(d, "body")
    return out


def cmd_settle(a):
    sdir = os.path.join(a.dir, "settle")
    names = sorted(f for f in os.listdir(sdir) if f.endswith(".req")) if os.path.isdir(sdir) else []
    report = {"count": len(names), "succeeded": 0, "violations": [], "hub_db": None}
    for n in names:
        with open(os.path.join(sdir, n), "rb") as f:
            body = f.read()
        for v in settle_violations(body):
            report["violations"].append("%s: %s" % (n, v))
        try:
            with open(os.path.join(sdir, n[:-4] + ".resp"), "rb") as f:
                resp = json.loads(f.read())
            if isinstance(resp, dict) and resp.get("success") is True:
                report["succeeded"] += 1
        except (OSError, ValueError):
            pass
    ok = report["count"] >= 1 and report["succeeded"] >= 1 and not report["violations"]
    if a.hub_db:
        hub = {"settled_rows": None, "columns": []}
        errors = []
        tmp = tempfile.mkdtemp(prefix="canary-hubdb-")
        try:
            copy = os.path.join(tmp, "hub.db")
            shutil.copyfile(a.hub_db, copy)
            if os.path.isfile(a.hub_db + "-wal"):
                shutil.copyfile(a.hub_db + "-wal", copy + "-wal")
            con = sqlite3.connect(copy)
            try:
                hub["settled_rows"] = con.execute("SELECT COUNT(*) FROM credit_settled").fetchone()[0]
                for t in ("credit_settled", "credit_cleared", "credit_entry", "credit_redemption", "hub_owed"):
                    for r in con.execute("SELECT name FROM pragma_table_info(?)", (t,)):
                        if r[0] in _WORK_FIELDS:
                            hub["columns"].append("%s.%s" % (t, r[0]))
            finally:
                con.close()
        except Exception as e:  # OSError, sqlite3.Error, or no sqlite3 module at all
            errors.append(str(e))
        finally:
            shutil.rmtree(tmp, ignore_errors=True)
        hub["errors"] = errors
        report["hub_db"] = hub
        if errors or not hub["settled_rows"] or hub["columns"]:
            ok = False
    if a.out:
        with open(a.out, "w") as f:
            json.dump(report, f, indent=1)
    hub = report["hub_db"]
    print("settle bodies: %d captured, %d answered success%s%s" % (
        report["count"], report["succeeded"],
        "; hub settlement rows %s%s" % (hub["settled_rows"], ", work columns " + ",".join(hub["columns"])
                                        if hub["columns"] else "") if hub else "",
        "; " + "; ".join(report["violations"][:4]) if report["violations"] else ""))
    return 0 if ok else 1


def main(argv):
    p = argparse.ArgumentParser(prog="canary.py", description=__doc__.split("\n\n")[0])
    sub = p.add_subparsers(dest="cmd")
    s = sub.add_parser("scan")
    s.add_argument("--canaries", required=True)
    s.add_argument("--out")
    s.add_argument("--label", default="scan")
    s.add_argument("--expect", action="append", default=[])
    s.add_argument("--want", action="append", default=[])
    s.add_argument("paths", nargs="+")
    t = sub.add_parser("tap")
    t.add_argument("--listen", required=True)
    t.add_argument("--upstream", required=True)
    t.add_argument("--dir", required=True)
    t.add_argument("--timeout", type=float, default=120)
    st = sub.add_parser("settle")
    st.add_argument("--dir", required=True)
    st.add_argument("--hub-db")
    st.add_argument("--out")
    a = p.parse_args(argv)
    if a.cmd == "scan":
        return cmd_scan(a)
    if a.cmd == "tap":
        return cmd_tap(a)
    if a.cmd == "settle":
        return cmd_settle(a)
    p.print_help()
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
