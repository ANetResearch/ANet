#!/usr/bin/env python3
"""soakhost.py — the on-host half of the lab soak test (scripts/testnet/soak.sh, docs/notes/0036).

soak.sh copies this file to <base>/soak/ on each lab host (~/anet-testnet/soak/soak/) and runs it
there over ssh. Tokens are read here, on the node's host, and never printed or put in an argv.

  soakhost.py mock-llm  --addr 127.0.0.1:P                 OpenAI-compatible stand-in for auto_reply
  soakhost.py backend   --node N --socket S --token-file F  capability service (module/service, UDS)
  soakhost.py load      --plan FILE                         one requester's load worker (until stop)
  soakhost.py sample                                        one resource sample of this host (JSON line)
  soakhost.py dump      --nodes a,b --hubs h                final state for the end checks (JSON)
  soakhost.py scan      --list FILE --paths P…             canary scan (files holding any listed text)
  soakhost.py gdump     NAME                                SIGQUIT one testnet Go process: the runtime's
                                                            goroutine dump, from its log (the process exits)
  soakhost.py ctl       NODE PATH                           POST stdin JSON to a node's control API

The mock model answers "echo: <last user text>" plus one line per image it was given, then the
completion sentinel, so the requester can check that the reply belongs to its own task and that its
attachment arrived intact. "[hold]" in the text makes it fail (auto_reply then sends its error reply
and the task waits for the requester, which cancels it); "[multi]" asks one question first.
"""
import argparse, base64, hashlib, http.client, http.server, json, os, random, re, secrets, signal
import socket, socketserver, sqlite3, subprocess, sys, threading, time, traceback, uuid

BASE = os.path.expanduser(os.environ.get("SOAK_BASE", "~/anet-testnet/soak"))
NODES = os.path.join(BASE, "nodes")
SOAK = os.path.join(BASE, "soak")
LOGS = os.path.join(SOAK, "logs")
TERMINAL = {"completed", "failed", "canceled", "rejected"}
A2A_TERMINAL = {"TASK_STATE_COMPLETED", "TASK_STATE_FAILED", "TASK_STATE_CANCELED", "TASK_STATE_REJECTED"}
DONE = "<<ANET_TASK_DONE>>"


def now():
    return time.time()


def home(node):
    return os.path.join(NODES, node, "home", ".anet")


def sha(b):
    return hashlib.sha256(b).hexdigest()


def canary():
    return "skc" + secrets.token_hex(8)


# ── control API ──────────────────────────────────────────────────────────────

class Ctl:
    """The control API of one node on this host (token read from its file, never printed)."""

    def __init__(self, node):
        self.node = node
        h = home(node)
        self.addr = json.load(open(os.path.join(h, "config.json")))["control_addr"]
        self.token = open(os.path.join(h, "control_token.txt")).read().strip()

    def call(self, path, body=None, timeout=90):
        host, port = self.addr.rsplit(":", 1)
        c = http.client.HTTPConnection(host, int(port), timeout=timeout)
        try:
            data = json.dumps(body if body is not None else {}).encode()
            c.request("POST", path, data, {"Authorization": "Bearer " + self.token,
                                           "Content-Type": "application/json"})
            r = c.getresponse()
            raw = r.read()
            try:
                return r.status, json.loads(raw) if raw else {}
            except ValueError:
                return r.status, {"_raw": raw[:300].decode("utf-8", "replace")}
        finally:
            c.close()


def cmd_ctl(a):
    code, out = Ctl(a.node).call(a.path, json.loads(sys.stdin.read() or "{}"), timeout=a.timeout)
    print(json.dumps(out))
    return 0 if code == 200 else 1


# ── mock model ───────────────────────────────────────────────────────────────

class LLMHandler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    stats = {"calls": 0, "hold": 0, "multi": 0, "echo": 0, "slow": 0, "images": 0}
    lock = threading.Lock()

    def log_message(self, *a):
        pass

    def _send(self, code, obj):
        b = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    def do_GET(self):
        with self.lock:
            self._send(200, dict(self.stats))

    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        try:
            body = json.loads(self.rfile.read(n))
        except ValueError:
            return self._send(400, {"error": "not json"})
        msgs = body.get("messages") or []
        users = [m for m in msgs if m.get("role") == "user"]
        asst = [m for m in msgs if m.get("role") == "assistant"]
        text, imgs = "", []
        if users:
            c = users[-1].get("content")
            if isinstance(c, str):
                text = c
            elif isinstance(c, list):
                for p in c:
                    if p.get("type") == "text":
                        text += p.get("text") or ""
                    elif p.get("type") == "image_url":
                        url = (p.get("image_url") or {}).get("url") or ""
                        raw = base64.b64decode(url.split(",", 1)[1]) if "," in url else b""
                        imgs.append(raw)
        with self.lock:
            self.stats["calls"] += 1
            self.stats["images"] += len(imgs)
        if "[hold]" in text:
            with self.lock:
                self.stats["hold"] += 1
            return self._send(503, {"error": {"message": "held by the soak test"}})
        if random.random() < 0.01:
            with self.lock:
                self.stats["slow"] += 1
            time.sleep(8)
        else:
            time.sleep(random.uniform(0, 0.3))
        if "[multi]" in text and not asst:
            with self.lock:
                self.stats["multi"] += 1
            content = "which part? q-" + sha(text.encode())[:12]
        else:
            with self.lock:
                self.stats["echo"] += 1
            content = "echo: " + text + "".join("\n[img sha256=%s bytes=%d]" % (sha(b), len(b)) for b in imgs)
            content += "\n" + DONE
        self._send(200, {"id": "soak-" + uuid.uuid4().hex[:8], "object": "chat.completion",
                         "model": body.get("model") or "soak",
                         "choices": [{"index": 0, "finish_reason": "stop",
                                      "message": {"role": "assistant", "content": content}}]})


class ThreadingHTTPServer(socketserver.ThreadingMixIn, http.server.HTTPServer):
    daemon_threads = True
    allow_reuse_address = True


def cmd_mock_llm(a):
    host, port = a.addr.rsplit(":", 1)
    assert host == "127.0.0.1"
    srv = ThreadingHTTPServer((host, int(port)), LLMHandler)
    print("mock-llm on %s" % a.addr, flush=True)
    srv.serve_forever()


# ── capability backend (module/service's JSON contract, over a Unix socket) ──

class UnixHTTPServer(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
    daemon_threads = True


class BackendHandler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    token = ""
    log = None
    lock = threading.Lock()
    counts = {}

    def log_message(self, *a):
        pass

    def address_string(self):
        return "unix"

    def _send(self, code, obj):
        b = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    def do_GET(self):
        if self.path == "/healthz":
            return self._send(200, {"ok": True})
        if self.path == "/calls":
            with self.lock:
                return self._send(200, dict(self.counts))
        self._send(404, {"error": "not found"})

    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(n)
        if self.headers.get("Authorization") != "Bearer " + self.token:
            return self._send(401, {"error": "the service token is required"})
        try:
            args = json.loads(raw or b"{}")
        except ValueError:
            return self._send(400, {"error": "args are not JSON"})
        cap = self.headers.get("X-ANet-Capability") or self.path.strip("/")
        call = self.headers.get("X-ANet-Call") or ""
        caller = self.headers.get("X-ANet-Caller") or ""
        t0 = now()
        text = str(args.get("text") or "")
        if self.path == "/long":
            secs = float(args.get("seconds") or 30)
            time.sleep(max(0.5, min(secs, 280)))
        elif self.path == "/short":
            time.sleep(random.uniform(0, 0.2))
        digest = sha(text.encode())
        with self.lock:
            self.counts[cap] = self.counts.get(cap, 0) + 1
            with open(self.log, "a") as f:
                f.write(json.dumps({"ts": round(t0, 3), "cap": cap, "call": call, "caller": caller,
                                    "digest": digest[:16], "dur": round(now() - t0, 3)}) + "\n")
        try:
            self._send(200, {"digest": digest, "length": len(text), "cap": cap})
        except (BrokenPipeError, ConnectionResetError):
            with self.lock, open(self.log, "a") as f:
                f.write(json.dumps({"ts": round(now(), 3), "cap": cap, "call": call, "lost": True}) + "\n")


def cmd_backend(a):
    BackendHandler.token = open(a.token_file).read().strip()
    BackendHandler.log = os.path.join(LOGS, "backend-%s.jsonl" % a.node)
    d = os.path.dirname(a.socket)
    os.makedirs(d, mode=0o700, exist_ok=True)
    os.chmod(d, 0o700)
    if os.path.exists(a.socket):
        os.unlink(a.socket)
    old = os.umask(0o077)
    srv = UnixHTTPServer(a.socket, BackendHandler)
    os.umask(old)
    print("backend %s on %s" % (a.node, a.socket), flush=True)
    srv.serve_forever()


# ── load worker ──────────────────────────────────────────────────────────────

class Load:
    def __init__(self, plan):
        self.plan = plan
        self.node = plan["node"]
        self.ctl = Ctl(self.node)
        self.stop_file = os.path.join(SOAK, "run", "stop")
        self.events = open(os.path.join(LOGS, "events-%s.jsonl" % self.node), "a", buffering=1)
        self.canaries = open(os.path.join(LOGS, "canaries-%s.txt" % self.node), "a", buffering=1)
        self.att_dir = os.path.join(SOAK, "att", self.node)
        os.makedirs(self.att_dir, mode=0o700, exist_ok=True)
        self.lock = threading.Lock()
        self.inflight = {}
        a2a = os.path.join(home(self.node), "modules", "a2a")
        try:
            self.a2a_addr = open(os.path.join(a2a, "a2a_addr.txt")).read().strip()
            self.a2a_token = open(os.path.join(a2a, "a2a_token.txt")).read().strip()
        except OSError:
            self.a2a_addr = self.a2a_token = ""

    def stopping(self):
        return os.path.exists(self.stop_file)

    def mark(self, text):
        with self.lock:
            self.canaries.write(text + "\n")

    def emit(self, op, peer, ok, t0, **kw):
        ev = {"ts": round(now(), 3), "node": self.node, "op": op, "peer": peer, "ok": bool(ok),
              "lat": round(now() - t0, 3)}
        ev.update(kw)
        with self.lock:
            self.events.write(json.dumps(ev) + "\n")

    # control-plane helpers
    def wait_task(self, ix, until, stop_on=("input-required",), poll=1.5):
        """Wait for a terminal state (or one in stop_on) or the deadline: half the waits long-poll
        /tasks/wait (the daemon's event bus, after_seq = the last state_seq seen), half poll
        /tasks/get, as the two kinds of client do."""
        last, seq, use_wait = None, 0, random.random() < 0.5
        while now() < until:
            code = 0
            try:
                if use_wait:
                    ms = int(max(1, min(25, until - now())) * 1000)
                    code, t = self.ctl.call("/tasks/wait", {"task_id": ix, "after_seq": seq, "timeout_ms": ms,
                                                            "history_length": 0}, timeout=60)
                else:
                    code, t = self.ctl.call("/tasks/get", {"task_id": ix, "history_length": 0}, timeout=30)
            except OSError as e:
                last = {"_err": str(e)}
                time.sleep(poll * 2)
                continue
            if code == 200:
                last = t
                st = short_state((t.get("status") or {}).get("state"))
                if st in TERMINAL or st in stop_on:
                    return t
                try:
                    seq = max(seq, int((t.get("metadata") or {}).get("anet.state_seq") or 0))
                except (TypeError, ValueError):
                    pass
            if not use_wait or code != 200:
                time.sleep(poll)
        return last

    def thread(self, ix):
        code, t = self.ctl.call("/thread", {"interaction_id": ix}, timeout=30)
        return (t or {}).get("thread") or {}

    @staticmethod
    def result(t):
        """The capability's effect: the anet.result artifact's DataPart of the task view (not /results,
        which reads every receipt this node holds on each call)."""
        for a in (t or {}).get("artifacts") or []:
            for p in a.get("parts") or []:
                if isinstance(p.get("data"), dict):
                    return p["data"]
        return None

    def make_att(self, kind):
        """One attachment file of a random size class; returns (path, sha256, is_image, size)."""
        c = canary()
        self.mark(c)
        r = random.random()
        size = 0
        if r < 0.40:
            return None
        elif r < 0.70:
            size = random.randint(1 << 10, 16 << 10)
        elif r < 0.90:
            size = random.randint(64 << 10, 256 << 10)
        elif r < 0.98:
            size = random.randint(900 << 10, 1200 << 10)
        else:
            size = random.randint(3 << 20, 5 << 20)
        img = random.random() < 0.5
        # The stored type is sniffed from the bytes (A2A-DESIGN §7.6), not taken from the name: an
        # "image" starts with the PNG signature so auto_reply hands it to the model.
        head = (b"\x89PNG\r\n\x1a\n" if img else b"") + ("SOAKCANARY %s\n" % c).encode()
        body = head + os.urandom(max(0, size - len(head)))
        name = "a-%s.%s" % (c, "png" if img else "bin")
        p = os.path.join(self.att_dir, name)
        with open(p, "wb") as f:
            f.write(body)
        return p, sha(body), img, len(body)

    # ── operations ──
    def op_text(self, o):
        """/delegate a text task (maybe with an attachment, maybe two turns), wait for completion,
        check the provider's reply is the mock's echo of this task's own text (and image)."""
        t0 = now()
        peer = o["peer"]
        c = canary()
        self.mark(c)
        multi = random.random() < o.get("multi", 0.0)
        goal = "soak text %s%s" % (c, " [multi]" if multi else "")
        att = self.make_att("text") if o.get("attach", True) else None
        body = {"provider": peer, "goal": goal}
        if att:
            body["attachments"] = [att[0]]
        code, out = self.ctl.call("/delegate", body, timeout=120)
        ix = (out or {}).get("interaction_id")
        if att:
            try:
                os.unlink(att[0])
            except OSError:
                pass
        if code != 200 or not ix:
            return self.emit("text", o["name"], False, t0, err="delegate %s %s" % (code, str(out)[:200]))
        want = "echo: " + goal
        if multi:
            t = self.wait_task(ix, t0 + o.get("timeout", 600), stop_on=("input-required",))
            st = short_state(((t or {}).get("status") or {}).get("state"))
            if st != "input-required":
                return self.emit("text", o["name"], False, t0, ix=ix, err="multi: no question (%s)" % st,
                                 multi=True)
            c2 = canary()
            self.mark(c2)
            follow = "the second part %s" % c2
            code, out = self.ctl.call("/message", {"interaction_id": ix, "body": follow}, timeout=120)
            if code != 200:
                return self.emit("text", o["name"], False, t0, ix=ix, err="message %s %s" % (code, str(out)[:200]))
            want = "echo: " + follow
        elif att and att[2]:
            want += "\n[img sha256=%s bytes=%d]" % (att[1], att[3])
        t = self.wait_task(ix, t0 + o.get("timeout", 600), stop_on=())
        st = short_state(((t or {}).get("status") or {}).get("state"))
        if st != "completed":
            return self.emit("text", o["name"], False, t0, ix=ix, err="state %s" % st, multi=multi,
                             att=att[3] if att else 0)
        th = self.thread(ix)
        replies = [m.get("body") or "" for m in th.get("messages") or []
                   if m.get("from") == "them" and (m.get("body") or "")]
        ok = bool(replies) and replies[-1] == want
        self.emit("text", o["name"], ok, t0, ix=ix, multi=multi, att=att[3] if att else 0,
                  img=bool(att and att[2]), **({} if ok else {"err": "reply mismatch",
                                                            "got": (replies[-1] if replies else "")[:120]}))

    def op_cap(self, o):
        """A capability call through /tasks/send (skill + args), result checked against the args."""
        t0 = now()
        c = canary()
        self.mark(c)
        skill = random.choice(o["skills"])
        text = "soak cap %s" % c
        if skill in ("cas.put",):
            args = {"body": base64.b64encode(text.encode()).decode()}
        elif skill == "soak.long":
            args = {"text": text, "seconds": random.randint(o.get("min_s", 15), o.get("max_s", 120))}
        else:
            args = {"text": text}
        code, out = self.ctl.call("/tasks/send", {"to": o["peer"], "skill": skill, "args": args,
                                                  "return_immediately": True}, timeout=120)
        ix = (out or {}).get("id")
        if code != 200 or not ix:
            return self.emit("cap", o["name"], False, t0, skill=skill, err="send %s %s" % (code, str(out)[:200]))
        t = self.wait_task(ix, t0 + o.get("timeout", 420), stop_on=())
        st = short_state(((t or {}).get("status") or {}).get("state"))
        res = self.result(t) if st == "completed" else None
        ok, err = check_cap(skill, args, text, st, res)
        self.emit("cap", o["name"], ok, t0, ix=ix, skill=skill, state=st, **({"err": err} if err else {}))

    def cap_call(self, peer, skill, args, until):
        code, out = self.ctl.call("/tasks/send", {"to": peer, "skill": skill, "args": args,
                                                  "return_immediately": True}, timeout=120)
        ix = (out or {}).get("id")
        if code != 200 or not ix:
            return None, None, "send %s %s" % (code, str(out)[:200])
        t = self.wait_task(ix, until, stop_on=())
        st = short_state(((t or {}).get("status") or {}).get("state"))
        res = self.result(t) if st == "completed" else None
        if st != "completed" or not isinstance(res, dict) or res.get("status") != "OK":
            return ix, res, "%s: state %s, %s" % (skill, st, (res or {}).get("message") or (res or {}).get("status"))
        return ix, res, ""

    def op_cas(self, o):
        """cas.put, then cas.get of the returned CID (the same bytes back), then cas.stat."""
        t0 = now()
        c = canary()
        self.mark(c)
        blob = base64.b64encode(("soak cas %s " % c).encode() + os.urandom(random.choice((16, 2048, 65536)))).decode()
        until = t0 + o.get("timeout", 300)
        ix, res, err = self.cap_call(o["peer"], "cas.put", {"body": blob}, until)
        cid = ((res or {}).get("evidence") or {}).get("observed_state") or ""
        if err or not cid:
            return self.emit("cap", o["name"], False, t0, ix=ix, skill="cas", err=err or "no cid")
        ix2, res2, err = self.cap_call(o["peer"], "cas.get", {"cid": cid}, until)
        if err or blob not in json.dumps(res2):
            return self.emit("cap", o["name"], False, t0, ix=ix2, skill="cas", err=err or "cas.get: other bytes")
        ix3, res3, err = self.cap_call(o["peer"], "cas.stat", {"cid": cid}, until)
        self.emit("cap", o["name"], not err, t0, ix=ix3, skill="cas", **({"err": err} if err else {}))

    def op_paid(self, o):
        """A priced capability: quote in the task, paid by the auto tier or (tier=agent) by /tasks/pay,
        settled, executed; exactly one payment is checked later against both hubs' ledgers."""
        t0 = now()
        c = canary()
        self.mark(c)
        skill = o["skill"]
        text = "soak paid %s" % c
        args = {"text": text}
        code, out = self.ctl.call("/tasks/send", {"to": o["peer"], "skill": skill, "args": args,
                                                  "return_immediately": True}, timeout=120)
        ix = (out or {}).get("id")
        if code != 200 or not ix:
            return self.emit("paid", o["name"], False, t0, skill=skill, err="send %s %s" % (code, str(out)[:200]))
        paid_by = "auto"
        until = t0 + o.get("timeout", 300)
        t = self.wait_task(ix, until, stop_on=("input-required",))
        st = short_state(((t or {}).get("status") or {}).get("state"))
        md = (t or {}).get("metadata") or {}
        price = quoted_price(md)
        held_seen = None
        if st == "input-required" and md.get("x402.payment.status") == "payment-required":
            if o.get("tier") != "agent":
                # Within the auto tier the node pays by itself; a client that stops at this
                # input-required is told "needs_operator_approval" all the same (0036 §F1).
                held_seen = md.get("anet.reason") or "?"
                t = self.wait_task(ix, now() + 20, stop_on=())
                st2 = short_state(((t or {}).get("status") or {}).get("state"))
                md2 = (t or {}).get("metadata") or {}
                if st2 == "input-required" and md2.get("x402.payment.status") == "payment-required":
                    return self.emit("paid", o["name"], False, t0, ix=ix, skill=skill, price=price,
                                     held_seen=held_seen, err="quote left for the operator: %s" % md2.get("anet.reason"))
        if st == "input-required" and md.get("x402.payment.status") == "payment-required" and o.get("tier") == "agent":
            paid_by = "agent"
            code, out = self.ctl.call("/tasks/pay", {"task_id": ix, "decision": "submit"}, timeout=120)
            if code != 200 or (out or {}).get("spend_refusal"):
                return self.emit("paid", o["name"], False, t0, ix=ix, skill=skill, price=price,
                                 err="pay %s %s" % (code, str(out)[:200]))
        t = self.wait_task(ix, until, stop_on=())
        st = short_state(((t or {}).get("status") or {}).get("state"))
        md = (t or {}).get("metadata") or {}
        res = self.result(t) if st == "completed" else None
        ok, err = check_cap(skill, args, text, st, res)
        if ok and md.get("x402.payment.status") != "payment-completed":
            ok, err = False, "payment status %s" % md.get("x402.payment.status")
        self.emit("paid", o["name"], ok, t0, ix=ix, skill=skill, price=price or quoted_price(md),
                  by=paid_by, pay=md.get("x402.payment.status"), **({"held_seen": held_seen} if held_seen else {}),
                  **({"err": err} if err else {}))

    def op_cancel(self, o):
        """Cancel: mode quick (a held text task, canceled 0-1 s after /delegate — racing its
        delivery), late (after the provider's error reply: input-required), long (a long capability
        call, canceled while it runs)."""
        t0 = now()
        c = canary()
        self.mark(c)
        mode = o["mode"]
        if mode == "long":
            code, out = self.ctl.call("/tasks/send", {"to": o["peer"], "skill": "soak.long",
                                                      "args": {"text": "soak cancel %s" % c, "seconds": 150},
                                                      "return_immediately": True}, timeout=120)
            ix = (out or {}).get("id")
        else:
            code, out = self.ctl.call("/delegate", {"provider": o["peer"], "goal": "soak cancel [hold] %s" % c},
                                      timeout=120)
            ix = (out or {}).get("interaction_id")
        if code != 200 or not ix:
            return self.emit("cancel", o["name"], False, t0, mode=mode, err="send %s %s" % (code, str(out)[:200]))
        if mode == "quick":
            time.sleep(random.uniform(0, 1.0))
        elif mode == "late":
            t = self.wait_task(ix, t0 + 180, stop_on=("input-required",))
            st = short_state(((t or {}).get("status") or {}).get("state"))
            if st != "input-required":
                return self.emit("cancel", o["name"], False, t0, ix=ix, mode=mode, err="no error reply (%s)" % st)
        else:
            time.sleep(random.uniform(5, 20))
        code, out = self.ctl.call("/tasks/cancel", {"task_id": ix}, timeout=120)
        t = self.wait_task(ix, now() + 120, stop_on=())
        st = short_state(((t or {}).get("status") or {}).get("state"))
        self.emit("cancel", o["name"], st == "canceled", t0, ix=ix, mode=mode, state=st,
                  **({} if st == "canceled" else {"err": "cancel %s %s -> %s" % (code, str(out)[:120], st)}))

    def a2a_req(self, path, body, stream, timeout=240):
        host, port = self.a2a_addr.rsplit(":", 1)
        c = http.client.HTTPConnection(host, int(port), timeout=timeout)
        hdr = {"Authorization": "Bearer " + self.a2a_token, "Content-Type": "application/json",
               "A2A-Version": "1.0"}
        if stream:
            hdr["Accept"] = "text/event-stream"
        c.request("POST", path, json.dumps(body).encode(), hdr)
        return c, c.getresponse()

    def op_a2a(self, o):
        """The local A2A interface: SendStreamingMessage over JSON-RPC or HTTP+JSON (alternating), the
        SSE read to the terminal event, the reply artifact checked; a stream that breaks (a restart)
        falls back to GetTask."""
        t0 = now()
        c = canary()
        self.mark(c)
        binding = random.choice(o.get("bindings", ["jsonrpc", "rest"]))
        text = "soak a2a %s %s" % (binding, c)
        msg = {"messageId": "m-" + uuid.uuid4().hex, "role": "ROLE_USER", "parts": [{"text": text}]}
        base = "/a2a/v1/agents/%s" % o["peer"]
        if binding == "jsonrpc":
            path, body = base + "/jsonrpc", {"jsonrpc": "2.0", "id": uuid.uuid4().hex,
                                             "method": "SendStreamingMessage", "params": {"message": msg}}
        else:
            path, body = base + "/rest/message:stream", {"message": msg}
        tid, state, arts, nev, broken = None, None, [], 0, ""
        try:
            conn, r = self.a2a_req(path, body, True)
            if r.status != 200:
                raw = r.read()[:200]
                conn.close()
                return self.emit("a2a", o["name"], False, t0, binding=binding, err="HTTP %d %s" % (r.status, raw))
            buf = b""
            while True:
                line = r.fp.readline()
                if not line:
                    broken = "stream ended before a terminal state"
                    break
                line = line.rstrip(b"\r\n")
                if line.startswith(b"data:"):
                    buf += line[5:].strip()
                    continue
                if line or not buf:
                    continue
                ev = json.loads(buf)
                buf = b""
                nev += 1
                if binding == "jsonrpc":
                    if "error" in ev:
                        broken = "error %s" % json.dumps(ev["error"])[:200]
                        break
                    ev = ev.get("result") or {}
                if "task" in ev:
                    tid = ev["task"].get("id") or tid
                    state = (ev["task"].get("status") or {}).get("state")
                    for a in ev["task"].get("artifacts") or []:
                        arts.append(a)
                elif "statusUpdate" in ev:
                    tid = ev["statusUpdate"].get("taskId") or tid
                    state = (ev["statusUpdate"].get("status") or {}).get("state")
                elif "artifactUpdate" in ev:
                    arts.append(ev["artifactUpdate"].get("artifact") or {})
                if state in A2A_TERMINAL:
                    break
            conn.close()
        except (OSError, ValueError, http.client.HTTPException) as e:
            broken = "%s: %s" % (type(e).__name__, e)
        recovered = False
        if state not in A2A_TERMINAL and tid:
            # The stream broke: GetTask until terminal, as a client would.
            until = t0 + o.get("timeout", 420)
            while now() < until and state not in A2A_TERMINAL:
                time.sleep(3)
                try:
                    conn, r = self.a2a_req(base + "/jsonrpc", {"jsonrpc": "2.0", "id": 1, "method": "GetTask",
                                                              "params": {"id": tid}}, False, timeout=30)
                    res = json.loads(r.read()).get("result") or {}
                    conn.close()
                    state = (res.get("status") or {}).get("state")
                    arts = res.get("artifacts") or arts
                    recovered = True
                except (OSError, ValueError, http.client.HTTPException):
                    pass
        texts = [p.get("text") for a in arts for p in (a.get("parts") or []) if p.get("text")]
        want = "echo: " + text
        ok = state == "TASK_STATE_COMPLETED" and want in texts
        err = "" if ok else "state %s, texts %s; %s" % (state, [t[:60] for t in texts][:3], broken)
        self.emit("a2a", o["name"], ok, t0, binding=binding, task=tid, events=nev, recovered=recovered,
                  **({"err": err} if err else {}), **({"broken": broken} if broken and ok else {}))

    def op_a2a_list(self, o):
        t0 = now()
        try:
            conn, r = self.a2a_req("/a2a/v1/agents/%s/jsonrpc" % o["peer"],
                                   {"jsonrpc": "2.0", "id": 1, "method": "ListTasks", "params": {"pageSize": 20}},
                                   False, timeout=60)
            res = json.loads(r.read())
            conn.close()
            ok = r.status == 200 and "result" in res
            self.emit("a2a-list", o["name"], ok, t0, n=len((res.get("result") or {}).get("tasks") or []),
                      **({} if ok else {"err": str(res)[:200]}))
        except (OSError, ValueError, http.client.HTTPException) as e:
            self.emit("a2a-list", o["name"], False, t0, err=str(e))

    def op_a2aprobe(self, o):
        """a2a-go's own client (tools/a2aprobe run): the full scenario against the local interface."""
        t0 = now()
        out_dir = os.path.join(LOGS, "a2aprobe")
        os.makedirs(out_dir, exist_ok=True)
        f = os.path.join(out_dir, "%s-%s.log" % (self.node, time.strftime("%H%M%S")))
        cmd = [os.path.join(SOAK, "bin", "a2aprobe"), "run", "--a2a-addr", self.a2a_addr,
               "--token-file", os.path.join(home(self.node), "modules", "a2a", "a2a_token.txt"),
               "--agent", o["peer"], "--timeout", "150s"]
        if o.get("paid"):
            cmd += ["--paid", o["paid"], "--pricey", o["pricey"]]
        if o.get("registry"):
            cmd += ["--registry"]
        with open(f, "w") as fo:
            p = subprocess.run(cmd, stdout=fo, stderr=subprocess.STDOUT, stdin=subprocess.DEVNULL,
                               env=dict(os.environ, NO_PROXY="*", no_proxy="*"), timeout=1500)
        lines = open(f).read().splitlines()
        npass = sum(1 for l in lines if l.startswith("PASS "))
        fails = [l[5:120] for l in lines if l.startswith("FAIL ")]
        self.emit("a2aprobe", o["name"], p.returncode == 0 and not fails, t0, passed=npass, failed=len(fails),
                  exit=p.returncode, log=os.path.basename(f), **({"err": "; ".join(fails[:4])} if fails else {}))

    def op_mcp(self, o):
        """`anet mcp` (scripts/mcpcall.py): send_message(skill) to the peer, as an MCP host would."""
        t0 = now()
        c = canary()
        self.mark(c)
        d = os.path.join(NODES, self.node)
        env = {"PATH": "/usr/local/bin:/usr/bin:/bin", "HOME": os.path.join(d, "home"),
               "ANET_DATA_DIR": home(self.node), "XDG_RUNTIME_DIR": os.path.join(d, "xdg"),
               "NO_PROXY": "*", "no_proxy": "*"}
        args = json.dumps({"to": o["peer"], "skill": o["skill"], "args": {"text": "soak mcp %s" % c},
                           "timeout_seconds": 120})
        try:
            p = subprocess.run(["python3", os.path.join(SOAK, "mcpcall.py"), os.path.join(BASE, "bin", "anet"),
                                "call", "send_message", args], capture_output=True, text=True, env=env,
                               stdin=subprocess.DEVNULL, timeout=300)
            out = p.stdout + p.stderr
            ok = p.returncode == 0 and ("completed" in out.lower()) and sha(("soak mcp %s" % c).encode()) in out \
                if o["skill"] == "text.digest" else (p.returncode == 0 and "completed" in out.lower())
            self.emit("mcp", o["name"], ok, t0, skill=o["skill"], **({} if ok else {"err": out[-300:]}))
        except subprocess.TimeoutExpired:
            self.emit("mcp", o["name"], False, t0, skill=o["skill"], err="timeout")

    # ── scheduling ──
    def runner(self, o):
        fn = getattr(self, "op_" + o["op"].replace("-", "_"))
        key = o["name"] + ":" + o["op"] + ":" + o.get("mode", "")
        every = float(o["every"])
        time.sleep(random.uniform(0, min(every, 30)))
        while not self.stopping():
            with self.lock:
                busy = self.inflight.get(key, 0)
            if busy < o.get("max_inflight", 3):
                with self.lock:
                    self.inflight[key] = busy + 1
                threading.Thread(target=self.wrap, args=(fn, o, key), daemon=True).start()
            time.sleep(every * random.uniform(0.6, 1.4))

    def wrap(self, fn, o, key):
        try:
            fn(o)
        except Exception as e:  # a bug here must not stop the worker
            self.emit(o["op"], o.get("name"), False, now(), err="worker: %s" % traceback.format_exc()[-400:])
        finally:
            with self.lock:
                self.inflight[key] -= 1

    def run(self):
        for o in self.plan["ops"]:
            threading.Thread(target=self.runner, args=(o,), daemon=True).start()
        while not self.stopping():
            time.sleep(2)
        drain_until = now() + self.plan.get("drain", 900)
        while now() < drain_until:
            with self.lock:
                n = sum(self.inflight.values())
            if n == 0:
                break
            time.sleep(2)
        with self.lock:
            left = {k: v for k, v in self.inflight.items() if v}
        self.emit("worker-exit", "-", not left, now(), left=left)


def short_state(s):
    if not s:
        return s
    s = s.lower()
    if s.startswith("task_state_"):
        s = s[len("task_state_"):]
    return s.replace("_", "-")


def quoted_price(md):
    pr = md.get("x402.payment.required")
    if isinstance(pr, str):
        try:
            pr = json.loads(pr)
        except ValueError:
            pr = None
    if isinstance(pr, dict):
        for a in pr.get("accepts") or []:
            try:
                return int(a.get("amount"))
            except (TypeError, ValueError):
                pass
    return None


def check_cap(skill, args, text, st, res):
    if st != "completed":
        return False, "state %s" % st
    if not isinstance(res, dict):
        return False, "no result"
    if res.get("status") != "OK":
        return False, "status %s %s" % (res.get("status"), str(res.get("message") or "")[:120])
    obs = (res.get("evidence") or {}).get("observed_state") or ""
    if skill in ("cas.put",):
        return (obs.startswith("bafk") or obs.startswith("bafy")), ("" if obs else "no cid")
    if skill in ("net.echo",):
        return (text in obs), ("" if text in obs else "echo mismatch")
    want = sha(text.encode())
    return (want in obs), ("" if want in obs else "digest mismatch: %s" % obs[:100])


def cmd_once(a):
    """Every op of a plan once, concurrently (the smoke test): the same code the load runs."""
    plan = json.load(open(a.plan))
    L = Load(plan)
    L.events = open(os.path.join(LOGS, "smoke-%s.jsonl" % plan["node"]), "a", buffering=1)
    start = now()
    ts = []
    for o in plan["ops"]:
        if o["op"] == "a2aprobe" and not a.probe:
            continue
        t = threading.Thread(target=L.wrap, args=(getattr(L, "op_" + o["op"].replace("-", "_")), o,
                                                  o["name"] + ":" + o["op"]))
        L.inflight[o["name"] + ":" + o["op"]] = L.inflight.get(o["name"] + ":" + o["op"], 0) + 1
        t.start()
        ts.append(t)
    for t in ts:
        t.join()
    bad = 0
    for l in open(os.path.join(LOGS, "smoke-%s.jsonl" % plan["node"])):
        e = json.loads(l)
        if e["ts"] < start:
            continue
        bad += not e["ok"]
        print("%-4s %-9s %-9s %-16s %6.1fs %s" % ("ok" if e["ok"] else "FAIL", e["op"], e["peer"],
              e.get("skill") or e.get("mode") or e.get("binding") or "", e["lat"], e.get("err", "")[:200]))
    return 1 if bad else 0


def cmd_load(a):
    plan = json.load(open(a.plan))
    signal.signal(signal.SIGTERM, lambda *x: open(os.path.join(SOAK, "run", "stop"), "a").close())
    Load(plan).run()


# ── sampling ─────────────────────────────────────────────────────────────────

def proc_info(pid):
    try:
        st = open("/proc/%d/status" % pid).read()
        rss = int(re.search(r"VmRSS:\s+(\d+)", st).group(1))
        hwm = int(re.search(r"VmHWM:\s+(\d+)", st).group(1))
        thr = int(re.search(r"Threads:\s+(\d+)", st).group(1))
        fds = len(os.listdir("/proc/%d/fd" % pid))
        f = open("/proc/%d/stat" % pid).read().rsplit(")", 1)[1].split()
        cpu = (int(f[11]) + int(f[12])) / os.sysconf("SC_CLK_TCK")
        start = int(f[19]) / os.sysconf("SC_CLK_TCK")
        up = float(open("/proc/uptime").read().split()[0])
        return {"pid": pid, "rss_kb": rss, "hwm_kb": hwm, "threads": thr, "fds": fds, "cpu_s": round(cpu, 2),
                "age_s": round(up - start)}
    except (OSError, AttributeError, ValueError, IndexError):
        return None


def pidfile_procs():
    """Every testnet process of this run on this host: <node>/<name>.pid (deploy.sh), and the soak
    helpers' pid files in <base>/soak/run/*.pid."""
    out = {}
    for root, dirs, files in os.walk(NODES):
        if root.count(os.sep) - NODES.count(os.sep) > 2:
            continue
        for f in files:
            if f.endswith(".pid"):
                try:
                    pid = int(open(os.path.join(root, f)).read().strip())
                except (OSError, ValueError):
                    continue
                out[f[:-4]] = pid
    rd = os.path.join(SOAK, "run")
    for f in os.listdir(rd) if os.path.isdir(rd) else []:
        if f.endswith(".pid"):
            try:
                out["soak-" + f[:-4]] = int(open(os.path.join(rd, f)).read().strip())
            except (OSError, ValueError):
                pass
    return out


def ro(path):
    return sqlite3.connect("file:%s?mode=ro" % path, uri=True, timeout=10)


def q1(db, sql, *args):
    try:
        return db.execute(sql, args).fetchone()
    except sqlite3.Error as e:
        return ("err: %s" % e,)


def files_sizes(d):
    out, total_db = {}, 0
    for root, dirs, files in os.walk(d):
        for f in files:
            if re.search(r"\.(db|sqlite)(-wal|-shm)?$", f):
                p = os.path.join(root, f)
                try:
                    s = os.path.getsize(p)
                except OSError:
                    continue
                out[os.path.relpath(p, d)] = s
                total_db += s
    return out, total_db


def du(d):
    t = 0
    for root, dirs, files in os.walk(d):
        for f in files:
            try:
                t += os.lstat(os.path.join(root, f)).st_size
            except OSError:
                pass
    return t


def log_counts(path):
    n = {"lines": 0, "error": 0, "panic": 0, "warn_deliver": 0}
    try:
        with open(path, "rb") as f:
            for line in f:
                n["lines"] += 1
                l = line.lower()
                if b"panic" in l or b"fatal" in l:
                    n["panic"] += 1
                if b"error" in l:
                    n["error"] += 1
                if b"delivering" in l and b"failed" in l:
                    n["warn_deliver"] += 1
    except OSError:
        pass
    return n


def event_counts():
    out = {}
    for f in os.listdir(LOGS) if os.path.isdir(LOGS) else []:
        if f.startswith("events-") and f.endswith(".jsonl"):
            with open(os.path.join(LOGS, f)) as fh:
                for line in fh:
                    try:
                        e = json.loads(line)
                    except ValueError:
                        continue
                    k = "%s/%s" % (e["node"], e["op"])
                    c = out.setdefault(k, [0, 0])
                    c[0 if e["ok"] else 1] += 1
    return out


def cmd_sample(a):
    s = {"ts": round(now(), 1), "host": socket.gethostname(), "procs": {}, "nodes": {}, "hubs": {}}
    mem = open("/proc/meminfo").read()
    s["mem_avail_kb"] = int(re.search(r"MemAvailable:\s+(\d+)", mem).group(1))
    s["load1"] = float(open("/proc/loadavg").read().split()[0])
    st = os.statvfs(BASE)
    s["disk_free_mb"] = st.f_bavail * st.f_frsize >> 20
    for name, pid in sorted(pidfile_procs().items()):
        s["procs"][name] = proc_info(pid) or {"pid": pid, "dead": True}
    for n in sorted(os.listdir(NODES)):
        d = os.path.join(NODES, n)
        if os.path.isdir(os.path.join(d, "data")):  # a hub
            h = {}
            h["db"], h["db_total"] = files_sizes(os.path.join(d, "data"))
            db = os.path.join(d, "data", "hub.db")
            cand = [p for p in h["db"] if p.endswith(".db")]
            if cand:
                dbp = os.path.join(d, "data", sorted(cand, key=lambda p: -h["db"][p])[0])
                try:
                    c = ro(dbp)
                    r = q1(c, "SELECT COUNT(*), COALESCE(SUM(size),0), COALESCE(MIN(created_at),0) FROM relay_message")
                    h["mailbox"] = {"msgs": r[0], "bytes": r[1] if len(r) > 1 else None,
                                    "oldest_age_s": (round(now() - (r[2] / 1000 if r[2] > 1e12 else r[2]))
                                                     if len(r) > 2 and r[2] else 0)}
                    h["mailbox_by_to"] = dict(c.execute(
                        "SELECT to_aid, COUNT(*) FROM relay_message GROUP BY to_aid").fetchall())
                    for t in ("credit_settled", "credit_cleared", "credit_entry", "agent", "p2p_addr",
                              "fed_card", "fed_a2a_card", "review", "agent_keys", "departed_kel"):
                        h.setdefault("rows", {})[t] = q1(c, "SELECT COUNT(*) FROM %s" % t)[0]
                    c.close()
                    fp = os.path.join(d, "data", "federation.db")
                    if os.path.exists(fp):
                        c = ro(fp)
                        for t in ("fed_dedupe", "fed_peer_kel", "fed_cursor", "fed_cursor_v2"):
                            h["rows"][t] = q1(c, "SELECT COUNT(*) FROM %s" % t)[0]
                        c.close()
                except sqlite3.Error as e:
                    h["db_err"] = str(e)
            h["log"] = log_counts(os.path.join(d, n + ".log"))
            s["hubs"][n] = h
            continue
        hm = os.path.join(d, "home", ".anet")
        if not os.path.isfile(os.path.join(hm, "config.json")):
            continue
        nd = {}
        nd["db"], nd["db_total"] = files_sizes(hm)
        nd["home_bytes"] = du(os.path.join(d, "home"))
        ip = os.path.join(hm, "interactions", "interactions.db")
        if os.path.exists(ip):
            try:
                c = ro(ip)
                nd["outbox"] = dict(zip(("rows", "max_attempts", "oldest_ms", "maybe"),
                                        q1(c, "SELECT COUNT(*), COALESCE(MAX(attempts),0), COALESCE(MIN(created_at),0),"
                                              " COALESCE(SUM(maybe_delivered != 0),0) FROM outbox")))
                nd["tasks"] = {"total": q1(c, "SELECT COUNT(*) FROM interaction")[0],
                               "messages": q1(c, "SELECT COUNT(*) FROM message")[0],
                               "attachments": q1(c, "SELECT COUNT(*) FROM attachment")[0]}
                nd["open"] = [list(r) for r in c.execute(
                    "SELECT role, state, COUNT(*), MIN(state_at) FROM interaction "
                    "WHERE state NOT IN ('completed','failed','canceled','rejected') GROUP BY role, state")]
                for t in ("replay", "refused", "pending", "peer_identity"):
                    nd.setdefault("rows", {})[t] = q1(c, "SELECT COUNT(*) FROM %s" % t)[0]
                c.close()
            except sqlite3.Error as e:
                nd["db_err"] = str(e)
        try:
            code, stt = Ctl(n).call("/status", {}, timeout=15)
            nd["status_code"] = code
            nd["receive"] = stt.get("receive")
            for k in ("relay", "p2p", "outbox"):
                if k in stt:
                    nd[k] = stt[k]
        except (OSError, ValueError, KeyError) as e:
            nd["status_err"] = str(e)[:100]
        nd["log"] = log_counts(os.path.join(hm, "daemon.log"))
        s["nodes"][n] = nd
    s["events"] = event_counts()
    print(json.dumps(s))


# ── end-of-run dump ──────────────────────────────────────────────────────────

def cmd_dump(a):
    out = {"host": socket.gethostname(), "nodes": {}, "hubs": {}, "backend": {}}
    for n in [x for x in a.nodes.split(",") if x]:
        ip = os.path.join(home(n), "interactions", "interactions.db")
        c = ro(ip)
        rows = c.execute("SELECT id, role, peer_aid, state, state_at, created_at, is_capability, pay_state, "
                         "pay_auth_ids, task_nonce, trust, quote_expires_at FROM interaction").fetchall()
        cols = ["id", "role", "peer", "state", "state_at", "created_at", "cap", "pay_state", "auth_ids",
                "nonce", "trust", "quote_exp"]
        ixs = []
        for r in rows:
            d = dict(zip(cols, r))
            if d["nonce"]:
                d["bind"] = sha(b"anet/x402-bind/v1\0" + d["id"].encode() + b"\0" + d["nonce"].encode())
            del d["nonce"]
            ixs.append(d)
        # the last status line of each open task: why it waits
        for d in ixs:
            if d["state"] not in TERMINAL:
                m = c.execute("SELECT body, created_at FROM message WHERE interaction_id=? ORDER BY seq DESC LIMIT 1",
                              (d["id"],)).fetchone()
                d["last_msg"] = (m[0][:100], m[1]) if m else None
        ob = [list(r) for r in c.execute("SELECT ix, to_aid, typ, attempts, last_error, created_at FROM outbox")]
        c.close()
        try:
            code, stt = Ctl(n).call("/status", {}, timeout=15)
            aid = stt.get("aid")
            code, pay = Ctl(n).call("/payments/status", {}, timeout=15)
            code, bal = Ctl(n).call("/balance", {}, timeout=30)
        except OSError as e:
            aid, pay, bal = None, {"err": str(e)}, {}
        out["nodes"][n] = {"aid": aid, "interactions": ixs, "outbox": ob, "payments": pay, "balance": bal}
    for h in [x for x in a.hubs.split(",") if x]:
        d = os.path.join(NODES, h, "data")
        dbs = [f for f in os.listdir(d) if f.endswith(".db")]
        dbp = os.path.join(d, max(dbs, key=lambda f: os.path.getsize(os.path.join(d, f))))
        c = ro(dbp)
        hub = {"db": os.path.basename(dbp)}
        hub["settled"] = [dict(zip(("auth_id", "payer", "pay_to", "amount", "ix", "at"), r)) for r in
                          c.execute("SELECT auth_id, payer, pay_to, amount, interaction_id, at FROM credit_settled")]
        hub["cleared"] = [dict(zip(("auth_id", "peer", "pay_to", "amount"), r)) for r in
                          c.execute("SELECT auth_id, peer_aid, pay_to, amount FROM credit_cleared")]
        hub["balances"] = dict(c.execute("SELECT aid, credits FROM credit_balance").fetchall())
        hub["entries_by_aid"] = {r[0]: [r[1], r[2]] for r in c.execute(
            "SELECT aid, COALESCE(SUM(delta),0), COUNT(*) FROM credit_entry GROUP BY aid")}
        hub["grants"] = [list(r) for r in c.execute(
            "SELECT aid, delta, reason FROM credit_entry WHERE reason IN ('registration grant') OR reason LIKE '%grant%'")]
        hub["owed"] = dict(c.execute("SELECT peer_aid, amount FROM hub_owed").fetchall())
        hub["due"] = dict(c.execute("SELECT payee_aid, amount FROM hub_due").fetchall())
        hub["mailbox"] = q1(c, "SELECT COUNT(*) FROM relay_message")[0]
        c.close()
        out["hubs"][h] = hub
    for f in os.listdir(LOGS):
        if f.startswith("backend-") and f.endswith(".jsonl"):
            out["backend"][f[8:-6]] = [json.loads(l) for l in open(os.path.join(LOGS, f)) if l.strip()]
    json.dump(out, sys.stdout)


def gsummary(text):
    """Goroutines in a Go runtime dump (SIGQUIT), and the busiest wait sites: state and the first
    frame outside the runtime and the standard library."""
    if "goroutine " not in text:
        return 0, []
    blocks = re.split(r"\n\n(?=goroutine \d+ )", text[text.find("goroutine "):])
    sites = {}
    std = re.compile(r"(runtime|internal/|sync\.|syscall\.|net\.|io\.|bufio\.|os\.|time\.|context\.|crypto/tls|"
                     r"database/sql\.|reflect\.|net/http\.\(\*persistConn\))")
    for b in blocks:
        lines = b.splitlines()
        st = re.search(r"\[([^\],]+)", lines[0])
        frames = [lines[i] for i in range(1, len(lines)) if lines[i] and not lines[i].startswith("\t")]
        top = next((f for f in frames if not std.match(f)), frames[0] if frames else "?")
        k = "%s | %s" % (st.group(1) if st else "?", re.sub(r"\(0x[0-9a-f, .?]*\)$|\(\.\.\.\)$", "", top)[:100])
        sites[k] = sites.get(k, 0) + 1
    return len(blocks), sorted(sites.items(), key=lambda kv: -kv[1])


def cmd_gdump(a):
    """SIGQUIT one process of this run (found by its launcher, pid file and executable path, like
    deploy.sh stops them): Go writes every goroutine's stack to stderr, which the launcher appends to
    <name>.log, and exits. The dump is cut from the log into logs/gdump/<name>-<time>.txt, headed with
    the process's uptime; one JSON line goes to stdout and to logs/gdump.jsonl."""
    name = a.name
    assert re.match(r"^[a-z0-9-]+$", name)
    launcher = None
    for root, dirs, files in os.walk(NODES):
        if root.count(os.sep) - NODES.count(os.sep) <= 2 and "run-%s.sh" % name in files:
            launcher = root
            break
    if not launcher:
        print(json.dumps({"name": name, "error": "no launcher"}))
        return 1
    pf, log = os.path.join(launcher, name + ".pid"), os.path.join(launcher, name + ".log")
    try:
        pid = int(open(pf).read().strip())
        exe = os.path.realpath("/proc/%d/exe" % pid)
    except (OSError, ValueError):
        print(json.dumps({"name": name, "error": "not running"}))
        return 1
    if not exe.startswith(os.path.dirname(BASE) + os.sep):
        print(json.dumps({"name": name, "error": "pid %d is %s, not ours" % (pid, exe)}))
        return 1
    up = proc_info(pid)["age_s"]
    off = os.path.getsize(log)
    os.kill(pid, signal.SIGQUIT)
    for _ in range(60):
        if not os.path.exists("/proc/%d" % pid):
            break
        time.sleep(0.25)
    with open(log, "rb") as f:
        f.seek(off)
        dump = f.read().decode("utf-8", "replace")
    d = os.path.join(LOGS, "gdump")
    os.makedirs(d, exist_ok=True)
    out = os.path.join(d, "%s-%s.txt" % (name, time.strftime("%H%M%S", time.gmtime())))
    with open(out, "w") as f:
        f.write("# soak gdump %s pid %d uptime_s %d at %d\n" % (name, pid, up, now()) + dump)
    n, sites = gsummary(dump)
    rec = {"ts": round(now()), "name": name, "uptime_s": up, "goroutines": n, "top": sites[:8],
           "file": os.path.basename(out)}
    with open(os.path.join(LOGS, "gdump.jsonl"), "a") as f:
        f.write(json.dumps(rec) + "\n")
    print(json.dumps(rec))
    return 0


def cmd_scan(a):
    """Files under the given paths holding any canary of the list (grep -F -f), with counts."""
    paths = [p for p in a.paths if os.path.exists(p)]
    n = sum(1 for l in open(a.list) if l.strip())
    p = subprocess.run(["grep", "-a", "-r", "-c", "-F", "-f", a.list] + paths, capture_output=True, text=True)
    hits = {}
    for line in p.stdout.splitlines():
        f, _, c = line.rpartition(":")
        if c.isdigit() and int(c) > 0:
            hits[f] = int(c)
    print(json.dumps({"canaries": n, "paths": paths, "files_hit": hits, "grep_exit": p.returncode}))


def main():
    ap = argparse.ArgumentParser()
    sp = ap.add_subparsers(dest="cmd", required=True)
    p = sp.add_parser("mock-llm"); p.add_argument("--addr", required=True); p.set_defaults(fn=cmd_mock_llm)
    p = sp.add_parser("backend"); p.add_argument("--node", required=True); p.add_argument("--socket", required=True)
    p.add_argument("--token-file", required=True); p.set_defaults(fn=cmd_backend)
    p = sp.add_parser("load"); p.add_argument("--plan", required=True); p.set_defaults(fn=cmd_load)
    p = sp.add_parser("once"); p.add_argument("--plan", required=True); p.add_argument("--probe", action="store_true")
    p.set_defaults(fn=cmd_once)
    p = sp.add_parser("sample"); p.set_defaults(fn=cmd_sample)
    p = sp.add_parser("dump"); p.add_argument("--nodes", default=""); p.add_argument("--hubs", default="")
    p.set_defaults(fn=cmd_dump)
    p = sp.add_parser("scan"); p.add_argument("--list", required=True); p.add_argument("--paths", nargs="+")
    p.set_defaults(fn=cmd_scan)
    p = sp.add_parser("gdump"); p.add_argument("name"); p.set_defaults(fn=cmd_gdump)
    p = sp.add_parser("ctl"); p.add_argument("node"); p.add_argument("path")
    p.add_argument("--timeout", type=float, default=90); p.set_defaults(fn=cmd_ctl)
    a = ap.parse_args()
    os.makedirs(LOGS, exist_ok=True)
    sys.exit(a.fn(a) or 0)


if __name__ == "__main__":
    main()
