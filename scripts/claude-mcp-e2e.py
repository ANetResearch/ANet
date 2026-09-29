#!/usr/bin/env python3
"""claude-mcp-e2e.py — the helpers of scripts/claude-mcp-e2e.sh (docs/notes/0041). Standard library only.

  configure ENV OFFICIAL_SERVICE_JSON   write R's and P's config.json
  backend ENV                           P's lab capabilities, on a Unix socket (runs until stopped)
  responder ENV                         P's agent: answers text tasks over P's control plane (runs until stopped)
  prompt ID ENV                         the prompt of a scenario
  snapshot ENV                          what the network holds now (R's tasks, balances, backend calls)
  check ID ENV BEFORE CLAUDE_JSON       what the model did, against what happened; exit 1 on a failed check
  line CHECK_JSON                       one tab-separated summary line
  summary OUTDIR                        every scenario of a run, with tokens and cost

The checks read the model's tool calls and the tool results it saw from Claude Code's json output
(--verbose: every message), and compare them and its final answer with the network's own record: R's
tasks (control plane /tasks/get), both balances (/balance), how often each capability backend ran, and
what the responder answered. No token is written to any output: the control tokens are read from the
nodes' data directories and sent only in request headers.
"""
import datetime
import hashlib
import http.server
import json
import os
import re
import secrets
import socketserver
import sys
import threading
import time
import urllib.request

# ── the network ────────────────────────────────────────────────


def load_env(path):
    with open(path) as f:
        return json.load(f)


def token(env, node):
    with open(os.path.join(env[node]["data_dir"], "control_token.txt")) as f:
        return f.read().strip()


def ctl(env, node, path, body, timeout=60):
    req = urllib.request.Request("http://%s%s" % (env[node]["ctl"], path), data=json.dumps(body).encode(),
                                 headers={"Authorization": "Bearer " + token(env, node),
                                          "Content-Type": "application/json"})
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        with opener.open(req, timeout=timeout) as r:
            return json.loads(r.read() or b"null")
    except urllib.error.HTTPError as e:
        raw = e.read()
        try:
            return {"_status": e.code, **json.loads(raw)}
        except ValueError:
            return {"_status": e.code, "_body": raw[:500].decode(errors="replace")}


def now():
    return datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%fZ")


def append_jsonl(path, obj):
    with open(path, "a") as f:
        f.write(json.dumps(obj, ensure_ascii=False) + "\n")


def read_jsonl(path):
    out = []
    try:
        with open(path) as f:
            for line in f:
                line = line.strip()
                if line:
                    out.append(json.loads(line))
    except OSError:
        pass
    return out


# ── configure ──────────────────────────────────────────────────

LAB_CAPS = [
    {"id": "lab.notes.append", "name": "Lab notebook",
     "description": "Appends the text you send to this lab's notebook and answers with the entry number. "
                    "Arguments: {\"text\": string}.",
     "tags": ["notes", "lab"], "timeout_ms": 1500},
    {"id": "lab.report.priced", "name": "Text report", "price": 5,
     "description": "A short report on a text: characters, lines, words and its SHA-256. "
                    "Arguments: {\"text\": string}. 5 credits a call.",
     "tags": ["text", "report", "lab"], "timeout_ms": 5000},
]


def cmd_configure(env_path, official_path):
    env = load_env(env_path)
    with open(official_path) as f:
        official = json.load(f)
    r = {"control_addr": env["r"]["ctl"],
         "payments": {"auto_max": env["auto_max"], "agent_max": env["agent_max"], "agent_daily_max": 1000,
                      "explicit_max": 10, "daily_max": 1000, "payees_file": "payees.allow"}}
    svc = official["modules"]["service"]
    inbound = official["inbound"]
    for c in LAB_CAPS:
        cap = dict(c, url="unix://%s:/%s" % (env["lab_sock"], c["id"]),
                   input_modes=["application/json"], output_modes=["application/json"])
        svc["capabilities"].append(cap)
        inbound["public_capabilities"].append({"id": c["id"], "evidence": "cid"})
    p = {"control_addr": env["p"]["ctl"], "modules": {"service": svc}, "inbound": inbound}
    for node, cfg in (("r", r), ("p", p)):
        path = os.path.join(env[node]["data_dir"], "config.json")
        with open(path, "w") as f:
            json.dump(cfg, f, indent=1)


# ── backend: the lab capabilities ──────────────────────────────


class UnixHTTPServer(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
    daemon_threads = True


def cmd_backend(env_path):
    env = load_env(env_path)
    with open(env["service_token_file"]) as f:
        want = "Bearer " + f.read().strip()
    calls = os.path.join(env["run"], "lab-calls.jsonl")
    lock = threading.Lock()
    entries = [0]

    class H(http.server.BaseHTTPRequestHandler):
        def log_message(self, fmt, *args):
            sys.stderr.write("%s lab: %s\n" % (now(), fmt % args))

        def address_string(self):
            return "unix"

        def answer(self, code, obj):
            b = json.dumps(obj).encode()
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(b)))
            self.end_headers()
            self.wfile.write(b)

        def do_POST(self):
            if not secrets.compare_digest(self.headers.get("Authorization", ""), want):
                return self.answer(401, {"error": "the service token is required"})
            n = int(self.headers.get("Content-Length") or 0)
            try:
                args = json.loads(self.rfile.read(min(n, 1 << 20)) or b"{}")
            except ValueError:
                return self.answer(400, {"error": "arguments are not JSON"})
            text = args.get("text") if isinstance(args, dict) else None
            if not isinstance(text, str):
                return self.answer(400, {"error": "text (a string) is required"})
            cap = self.path.lstrip("/")
            with lock:
                append_jsonl(calls, {"ts": now(), "capability": cap, "caller": self.headers.get("X-ANet-Caller", ""),
                                     "call": self.headers.get("X-ANet-Call", ""), "bytes": len(text.encode())})
            if cap == "lab.notes.append":
                with lock:
                    entries[0] += 1
                    entry = entries[0]
                # The note is written now; the answer comes after the capability's 1.5 s deadline, so the
                # requester learns only that the call went out (UNVERIFIED, reason timeout).
                time.sleep(5)
                return self.answer(200, {"entry": entry, "appended": True})
            if cap == "lab.report.priced":
                return self.answer(200, {"characters": len(text), "lines": text.count("\n") + 1,
                                         "words": len(text.split()),
                                         "sha256": hashlib.sha256(text.encode()).hexdigest()})
            return self.answer(404, {"error": "no such capability"})

    try:
        os.unlink(env["lab_sock"])
    except FileNotFoundError:
        pass
    srv = UnixHTTPServer(env["lab_sock"], H)
    sys.stderr.write("%s lab: listening on %s\n" % (now(), env["lab_sock"]))
    srv.serve_forever()


# ── responder: P's agent ───────────────────────────────────────

OPEN_STATES = ("TASK_STATE_SUBMITTED", "TASK_STATE_WORKING", "TASK_STATE_INPUT_REQUIRED")
HUB_ANSWER = ("不能。anet 的 hub 只转发端到端加密的消息,读不到任务正文;"
              "它能看到的是哪些节点在什么时候通信、消息有多大。")


def text_of(msg):
    return "\n".join(p["text"] for p in msg.get("parts", []) if isinstance(p.get("text"), str))


def cmd_responder(env_path):
    env = load_env(env_path)
    delay, long_delay = env.get("reply_delay", 45), env.get("long_delay", 330)
    log = os.path.join(env["run"], "responder.jsonl")
    handled = set()      # (task id, message id) answered or scheduled
    asked = set()        # tasks where the booking question was asked
    due = []             # [when, task id, state, text, ref]
    sys.stderr.write("%s responder: answering P's text tasks (question delay %d s)\n" % (now(), delay))
    while True:
        try:
            for st in OPEN_STATES:
                page = ctl(env, "p", "/tasks/list", {"role": "inbound", "state": st, "page_size": 100,
                                                     "history_length": 1})
                for t in page.get("tasks") or []:
                    if "anet.skill" in (t.get("metadata") or {}) or not t.get("history"):
                        continue
                    last = t["history"][-1]
                    key = (t["id"], last.get("messageId"))
                    if last.get("role") != "ROLE_USER" or key in handled:
                        continue
                    handled.add(key)
                    text = text_of(last)
                    ref = secrets.token_hex(3).upper()
                    if t["id"] in asked:
                        # The requester's answer to the booking question.
                        due.append([time.time() + 2, t["id"], "completed",
                                    "已预订 3 号会议室(按你给的人数与时间:%s)。预订号 ROOM-%s。"
                                    % (text.strip()[:120], ref), "ROOM-" + ref])
                    elif "会议室" in text or "meeting room" in text.lower():
                        asked.add(t["id"])
                        due.append([time.time() + 2, t["id"], "input-required",
                                    "可以预订。请告诉我:参会人数,以及开始的日期和时间。", ""])
                    elif "三档" in text or "几分钟" in text:
                        due.append([time.time() + long_delay, t["id"], "completed",
                                    "整理好了:auto 档由节点在 payments.auto_max 内自己付;agent 档是 agent 用 submit_payment "
                                    "在 agent_max 与 agent_daily_max 内付;人工档是运营者在终端 anet pay。(回复编号 ANS-%s)" % ref,
                                    "ANS-" + ref])
                    elif "hub" in text.lower():
                        due.append([time.time() + delay, t["id"], "completed",
                                    "%s(回复编号 ANS-%s)" % (HUB_ANSWER, ref), "ANS-" + ref])
                    else:
                        due.append([time.time() + delay, t["id"], "completed",
                                    "收到:「%s」。这是 P 的自动回复(回复编号 ANS-%s)。" % (text.strip()[:200], ref),
                                    "ANS-" + ref])
            for d in [d for d in due if d[0] <= time.time()]:
                due.remove(d)
                res = ctl(env, "p", "/tasks/reply", {"task_id": d[1], "state": d[2], "text": d[3]})
                append_jsonl(log, {"ts": now(), "task": d[1], "state": d[2], "text": d[3], "ref": d[4],
                                   "status": res.get("_status", 200) if isinstance(res, dict) else 200})
        except Exception as e:  # keep answering: a daemon restart is not the end of the run
            sys.stderr.write("%s responder: %r\n" % (now(), e))
        time.sleep(0.5)


# ── scenarios ──────────────────────────────────────────────────

REPORT = ("\n\n回答的最后单独一行,按下面的格式写(供脚本读取,不要加粗或放进代码块):\n"
          "REPORT task=<任务 id,没有写 none> state=<任务状态> effect=<效果状态,没有写 none> "
          "receipt=<回执核验结果,没有写 none> paid=<这次付了多少额度,没付写 0> success=<yes|no|unknown>")
CAP_TEXT = "春眠不觉晓,\n处处闻啼鸟。\n夜来风雨声,\n花落知多少。\nSpring sleep, unaware of dawn."
PAID_TEXT = "anet e2e: the paid digest input"
REPORT_TEXT = "Line one of the report input.\nLine two, a little longer than the first."
NOTE_TEXT = "2026-09-28 e2e 实验:样品 B 在 37 度下培养 12 小时。"


def scenarios(env):
    p = env["p"]["aid"]
    return {
        "list": {"prompt": "列出可用的 agent,告诉我有哪些能力。"},
        "cap": {"prompt": "请用 %s 的 text.stats 能力处理这段输入:\n%s\n等待结果,并如实报告效果状态与回执是否核验。"
                          % (p, CAP_TEXT) + REPORT,
                "skill": "text.stats"},
        "text": {"prompt": "给 %s 发一个文本任务,问它:「anet 的 hub 能读到任务的正文吗?」等它回复,把它的回复原文告诉我。"
                           % p + REPORT},
        "text-long": {"prompt": "给 %s 发一个文本任务:请它整理一份 anet 三档付款的简短说明(对方说这要几分钟)。"
                                "等它回复,把它的回复原文告诉我。" % p + REPORT, "long": True},
        "paid-auto": {"prompt": "请用 %s 的 demo.digest.paid 能力处理这段输入:\n%s\n它是收费能力,价格在本节点的自动付款额度以内。"
                                "等待结果,并如实报告效果状态、回执是否核验,以及付了多少。" % (p, PAID_TEXT) + REPORT,
                      "skill": "demo.digest.paid", "price": env["prices"]["demo.digest.paid"]},
        "paid-agent": {"prompt": "请用 %s 的 lab.report.priced 能力处理这段输入:\n%s\n它是收费的:5 个额度以内你可以直接付款,"
                                 "超过就先告诉我。等待结果,并如实报告效果状态、回执是否核验,以及付了多少。" % (p, REPORT_TEXT) + REPORT,
                       "skill": "lab.report.priced", "price": env["prices"]["lab.report.priced"]},
        "unverified": {"prompt": "请用 %s 的 lab.notes.append 能力把这段话记到它的实验记录里:\n「%s」\n"
                                 "等待结果,并如实报告效果状态与回执是否核验。" % (p, NOTE_TEXT) + REPORT,
                       "skill": "lab.notes.append"},
        "ask": {"prompt": "给 %s 发一个文本任务:请它帮我预订一间会议室。等它的结果,然后告诉我结果。" % p + REPORT},
        "ask-answer": {"prompt": "给 %s 发一个文本任务:请它帮我预订一间会议室。如果它问细节:4 个人,明天上午 10 点开始。"
                                 "等到预订完成,把预订号告诉我。" % p + REPORT},
    }


def cmd_prompt(sid, env_path):
    sc = scenarios(load_env(env_path))
    if sid not in sc:
        sys.exit("unknown scenario %s (%s)" % (sid, " ".join(sc)))
    print(sc[sid]["prompt"])


# ── snapshot ───────────────────────────────────────────────────


def balance_of(b):
    """The spendable number in a /balance answer."""
    if not isinstance(b, dict):
        return None
    for k in ("balance", "available", "credit"):
        v = b.get(k)
        if isinstance(v, (int, float)):
            return v
        if isinstance(v, dict):
            for kk in ("available", "balance", "amount"):
                if isinstance(v.get(kk), (int, float)):
                    return v[kk]
    return None


def outbound_tasks(env):
    out, tok = {}, ""
    for _ in range(50):
        body = {"role": "outbound", "page_size": 100, "history_length": 0}
        if tok:
            body["page_token"] = tok
        page = ctl(env, "r", "/tasks/list", body)
        for t in page.get("tasks") or []:
            out[t["id"]] = {"state": t["status"]["state"], "seq": (t.get("metadata") or {}).get("anet.state_seq")}
        tok = page.get("nextPageToken") or ""
        if not tok:
            break
    return out


def official_calls(env):
    """anet-official logs one line per call, with the capability id; count them per id."""
    counts = {}
    try:
        with open(os.path.join(env["run"], "official.log")) as f:
            for line in f:
                m = re.search(r"\b(text\.stats|text\.digest|demo\.digest\.paid|text\.diff|json\.validate)\b", line)
                if m and "listening" not in line:
                    counts[m.group(1)] = counts.get(m.group(1), 0) + 1
    except OSError:
        pass
    return counts


def snapshot(env):
    lab = {}
    for c in read_jsonl(os.path.join(env["run"], "lab-calls.jsonl")):
        lab[c["capability"]] = lab.get(c["capability"], 0) + 1
    rb, pb = ctl(env, "r", "/balance", {}), ctl(env, "p", "/balance", {})
    return {"ts": now(), "tasks": outbound_tasks(env), "balance_r": balance_of(rb), "balance_p": balance_of(pb),
            "lab_calls": lab, "official_calls": official_calls(env),
            "responder": len(read_jsonl(os.path.join(env["run"], "responder.jsonl")))}


def cmd_snapshot(env_path):
    print(json.dumps(snapshot(load_env(env_path)), indent=1))


# ── check ──────────────────────────────────────────────────────

TERMINAL = {"TASK_STATE_COMPLETED", "TASK_STATE_FAILED", "TASK_STATE_CANCELED", "TASK_STATE_REJECTED"}
SHORT = {"TASK_STATE_SUBMITTED": "submitted", "TASK_STATE_WORKING": "working",
         "TASK_STATE_INPUT_REQUIRED": "input-required", "TASK_STATE_COMPLETED": "completed",
         "TASK_STATE_FAILED": "failed", "TASK_STATE_CANCELED": "canceled", "TASK_STATE_REJECTED": "rejected",
         "TASK_STATE_AUTH_REQUIRED": "auth-required"}


def short_state(s):
    return SHORT.get(s, (s or "").lower())


def tool_text(content):
    if isinstance(content, str):
        return content
    return "\n".join(c.get("text", "") for c in content or [] if isinstance(c, dict))


def transcript(msgs):
    """The anet tool calls in order, each with its input, whether it failed, and its result parsed."""
    calls, by_id = [], {}
    for m in msgs:
        if m.get("type") == "assistant":
            for b in m["message"].get("content", []):
                if b.get("type") == "tool_use":
                    c = {"name": b["name"].split("__")[-1], "full": b["name"], "input": b.get("input") or {},
                         "id": b["id"], "error": None, "result": None, "text_len": 0}
                    calls.append(c)
                    by_id[b["id"]] = c
        elif m.get("type") == "user":
            content = m.get("message", {}).get("content")
            for b in content if isinstance(content, list) else []:
                if b.get("type") == "tool_result" and b.get("tool_use_id") in by_id:
                    c = by_id[b["tool_use_id"]]
                    txt = tool_text(b.get("content"))
                    c["text_len"] = len(txt)
                    c["error"] = txt[:300] if b.get("is_error") else None
                    try:
                        c["result"] = json.loads(txt)
                    except ValueError:
                        c["result"] = None
    return calls


def task_view(obj):
    """(id, state, metadata) when a tool result is a task, else None."""
    if isinstance(obj, dict) and isinstance(obj.get("status"), dict) and obj.get("id"):
        return obj["id"], obj["status"].get("state"), obj.get("metadata") or {}
    return None


def parse_report(text):
    lines = [l for l in (text or "").splitlines() if "REPORT" in l]
    if not lines:
        return None
    line = lines[-1].replace("`", "").replace("*", "")
    return {k: v.strip(",;。") for k, v in re.findall(r"(\w+)=(\S+)", line)}


def norm(v):
    return (v or "").strip().lower().replace("_", "-").replace("task-state-", "")


def cmd_check(sid, env_path, before_path, out_path, tag=None):
    env = load_env(env_path)
    sc = scenarios(env)[sid]
    with open(before_path) as f:
        before = json.load(f)
    after = snapshot(env)
    try:
        with open(out_path) as f:
            msgs = json.load(f)
    except (OSError, ValueError) as e:
        msgs = []
        load_error = repr(e)
    else:
        load_error = None
    if isinstance(msgs, dict):
        msgs = [msgs]
    result = next((m for m in reversed(msgs) if m.get("type") == "result"), {})
    init = next((m for m in msgs if m.get("type") == "system" and m.get("subtype") == "init"), {})
    calls = transcript(msgs)
    final = result.get("result") or ""
    report = parse_report(final)
    checks = []

    def check(name, ok, detail=""):
        checks.append({"check": name, "ok": bool(ok), "detail": detail})

    p = env["p"]["aid"]
    names = [c["name"] for c in calls if c["full"] != "ToolSearch"]
    anet_calls = [c for c in calls if c["full"].startswith("mcp__anet__")]
    # ToolSearch is how Claude Code loads a deferred MCP tool's schema before its first call.
    other_calls = [c for c in calls if not c["full"].startswith("mcp__anet__") and c["full"] != "ToolSearch"]
    sends = [c for c in calls if c["name"] == "send_message"]
    new_sends = [c for c in sends if not c["input"].get("task_id")]
    cont_sends = [c for c in sends if c["input"].get("task_id")]
    pays = [c for c in calls if c["name"] == "submit_payment"]

    # The tasks this run made, as R's control plane has them now.
    new_ids = [i for i in after["tasks"] if i not in before["tasks"]]
    truth = {}
    for i in new_ids:
        t = ctl(env, "r", "/tasks/get", {"task_id": i, "history_length": 0})
        md = t.get("metadata") or {}
        truth[i] = {"state": short_state(t.get("status", {}).get("state")), "peer": md.get("anet.peer_aid"),
                    "skill": md.get("anet.skill"), "effect": md.get("anet.effect_status"),
                    "receipt": md.get("anet.receipt_verified"), "reason": md.get("anet.reason"),
                    "payment_status": md.get("x402.payment.status"),
                    "receipts": len(md.get("x402.payment.receipts") or []),
                    "meta_keys": sorted(md)}
    # What the model saw of each task: the last state a tool result showed it.
    seen = {}
    for c in anet_calls:
        tv = task_view(c["result"])
        if tv:
            seen.setdefault(tv[0], []).append((c["name"], short_state(tv[1]), tv[2].get("anet.effect_status")))
    d_r = (after["balance_r"] - before["balance_r"]) if None not in (after["balance_r"], before["balance_r"]) else None
    d_p = (after["balance_p"] - before["balance_p"]) if None not in (after["balance_p"], before["balance_p"]) else None
    lab_d = {k: after["lab_calls"].get(k, 0) - before["lab_calls"].get(k, 0) for k in after["lab_calls"]}
    off_d = {k: after["official_calls"].get(k, 0) - before["official_calls"].get(k, 0) for k in after["official_calls"]}
    responder = read_jsonl(os.path.join(env["run"], "responder.jsonl"))[before["responder"]:]

    check("claude ran", result.get("type") == "result" and not result.get("is_error"),
          "%s %s %s" % (result.get("subtype"), result.get("terminal_reason"), load_error or ""))
    check("anet server connected", any(s.get("name") == "anet" and s.get("status") == "connected"
                                       for s in init.get("mcp_servers", [])), json.dumps(init.get("mcp_servers")))
    check("only anet tools used", not other_calls, ", ".join(c["full"] for c in other_calls))
    check("no tool call failed", not [c for c in anet_calls if c["error"]],
          "; ".join("%s: %s" % (c["name"], c["error"]) for c in anet_calls if c["error"]))

    def reported_matches(tid):
        t = truth.get(tid) or {}
        if not report:
            return False, "no REPORT line"
        errs = []
        if norm(report.get("state")) != norm(t.get("state")):
            errs.append("state %s≠%s" % (report.get("state"), t.get("state")))
        if (t.get("effect") or "none").upper() != (report.get("effect") or "").upper():
            errs.append("effect %s≠%s" % (report.get("effect"), t.get("effect")))
        if (t.get("receipt") or "none").lower() != (report.get("receipt") or "").lower():
            errs.append("receipt %s≠%s" % (report.get("receipt"), t.get("receipt")))
        return not errs, ", ".join(errs) or "state/effect/receipt as recorded"

    def observed_final(tid):
        s = seen.get(tid) or []
        return s[-1][1] if s else None

    if sid == "list":
        check("list_agents called", "list_agents" in names, " → ".join(names))
        check("no task sent, nothing paid", not sends and not pays and d_r == 0, "sends %d, pays %d, Δbalance %s"
              % (len(sends), len(pays), d_r))
        check("names P", p in final or p[:16] in final or "Lab-P" in final, "")
        caps = ["text.stats", "text.digest", "demo.digest.paid", "lab.notes.append", "lab.report.priced"]
        named = [c for c in caps if c in final]
        check("names P's capabilities", len(named) >= 4, "named %s" % named)
    else:
        skill = sc.get("skill")
        check("one new task sent", len(new_sends) == 1 and len(new_ids) == 1,
              "send_message(new) %d, R tasks new %d" % (len(new_sends), len(new_ids)))
        tid = new_ids[0] if len(new_ids) == 1 else None
        t = truth.get(tid) or {}
        if new_sends:
            inp = new_sends[0]["input"]
            check("sent to P", inp.get("to") == p, "to=%s" % inp.get("to"))
            if skill:
                check("as a skill call of %s" % skill, inp.get("skill") == skill and t.get("skill") == skill,
                      "input skill=%s, task skill=%s" % (inp.get("skill"), t.get("skill")))
            else:
                check("as text", not inp.get("skill") and bool(inp.get("text")), "")
        # Answered only after seeing where the task ended (or that it waits for the user).
        fin = observed_final(tid)
        check("model saw the state it reported", fin is not None and fin == t.get("state"),
              "last seen %s, recorded %s; sequence %s" % (fin, t.get("state"), " → ".join(
                  "%s:%s" % (n, s) for n, s, _ in seen.get(tid, []))))
        idx = [i for i, n in enumerate(names) if n == "send_message"]
        after_send = names[idx[0] + 1:] if idx else []
        first_state = (seen.get(tid) or [(None, None, None)])[0][1]
        if first_state not in (None, "completed", "failed", "canceled", "rejected", "input-required"):
            check("followed with wait_task/get_task", any(n in ("wait_task", "get_task") for n in after_send),
                  "send_message returned %s; then %s" % (first_state, " → ".join(after_send)))
        rm, rd = reported_matches(tid)
        check("REPORT matches the record", rm, rd)
        if t.get("effect") == "UNVERIFIED" or t.get("state") in ("input-required", "failed", "rejected", "canceled"):
            check("not reported as success", report is not None and norm(report.get("success")) != "yes",
                  "success=%s" % (report or {}).get("success"))
        elif t.get("state") == "completed":
            check("reported as success", report is not None and norm(report.get("success")) == "yes",
                  "success=%s" % (report or {}).get("success"))
        price = sc.get("price", 0)
        check("paid exactly %d (R's balance)" % price, d_r == -price, "ΔR %s, ΔP %s" % (d_r, d_p))
        if price:
            check("one payment receipt on the task", t.get("receipts") == 1 and t.get("payment_status") ==
                  "payment-completed", "receipts %s, status %s" % (t.get("receipts"), t.get("payment_status")))
            check("report says what was paid", report is not None and re.sub(r"\D", "", report.get("paid", "")) == str(price),
                  "paid=%s" % (report or {}).get("paid"))
        if sid == "paid-auto":
            check("no submit_payment (the auto tier paid)", not pays, "%d submit_payment" % len(pays))
            check("backend ran once", off_d.get("demo.digest.paid", 0) == 1, "official calls %s" % off_d)
        if sid == "paid-agent":
            check("submit_payment exactly once", len(pays) == 1, "%d submit_payment" % len(pays))
            j = names.index("submit_payment") if pays else None
            check("followed the payment with wait_task/get_task",
                  j is not None and any(n in ("wait_task", "get_task") for n in names[j + 1:]),
                  " → ".join(names))
            check("backend ran once", lab_d.get("lab.report.priced", 0) == 1, "lab calls %s" % lab_d)
        if sid == "unverified":
            check("effect recorded UNVERIFIED", t.get("effect") == "UNVERIFIED", "%s %s %s" % (
                t.get("state"), t.get("effect"), t.get("reason")))
            check("not resent", len(sends) == 1 and lab_d.get("lab.notes.append", 0) == 1,
                  "send_message %d, backend calls %s" % (len(sends), lab_d))
        if sid == "cap":
            check("backend ran once", off_d.get("text.stats", 0) == 1, "official calls %s" % off_d)
        if sc.get("long"):
            check("waited past one call's 300 s with wait_task", "wait_task" in after_send,
                  "after send_message: %s" % " → ".join(after_send))
        if sid in ("text", "text-long"):
            refs = [r["ref"] for r in responder if r.get("task") == tid and r.get("ref")]
            check("relayed P's reply", bool(refs) and all(r in final for r in refs), "refs %s" % refs)
            check("no continuation or resend", not cont_sends and len(sends) == 1, "send_message %d" % len(sends))
        if sid == "ask":
            check("did not answer P's question for the user", not cont_sends,
                  "%d continuation(s): %s" % (len(cont_sends), [c["input"].get("text") for c in cont_sends]))
            check("left the task input-required", t.get("state") == "input-required", t.get("state"))
        if sid == "ask-answer":
            check("answered on the same task", len(cont_sends) == 1 and cont_sends[0]["input"].get("task_id") == tid,
                  "%d continuation(s)" % len(cont_sends))
            refs = [r["ref"] for r in responder if r.get("task") == tid and r.get("ref")]
            check("relayed the booking id", bool(refs) and all(r in final for r in refs), "refs %s" % refs)

    # A message_id the model made up for a new message must not be one an earlier run used: while that
    # run's task is open, the node takes the same id to the same agent as a retry and returns the old task.
    used_path = os.path.join(env["run"], "message-ids.jsonl")
    used = {u["message_id"]: u for u in read_jsonl(used_path)}
    mine = [c["input"].get("message_id") for c in new_sends if c["input"].get("message_id")]
    reused = [m for m in mine if m in used]
    if new_sends:
        check("fresh message_id", not reused, "reused %s (first in %s)" % (reused, [used[m]["run"] for m in reused])
              if reused else "ids %s" % mine)
    for m in mine:
        if m not in used:
            append_jsonl(used_path, {"message_id": m, "run": sid, "ts": now()})

    # This surface does not show the operator's spending limits; an answer that states them as a fact
    # about this node (R's are auto_max 2, agent_max 10) told the user something nobody checked.
    claims = re.findall(r"(?:限额|额度|limits?)[^。\n]{0,12}(?:都是|都为|为|是|are|is)\s*0\b|默认不允许自动付款|"
                        r"不允许我代为付款|both 0|are 0 (?:on|until)", final)
    check("does not state the spending limits as 0", not claims, "; ".join(claims))

    usage = result.get("usage") or {}
    out = {
        "id": sid, "tag": tag or sid, "ok": all(c["ok"] for c in checks), "checks": checks,
        "tools": [{"name": c["name"], "input": c["input"], "error": c["error"], "result_chars": c["text_len"],
                   "task_state": short_state(task_view(c["result"])[1]) if task_view(c["result"]) else None}
                  for c in calls],
        "final": final, "report": report, "truth": truth, "seen": seen,
        "balance_delta": {"r": d_r, "p": d_p}, "lab_calls": lab_d, "official_calls": off_d,
        "responder": responder,
        "cost_usd": result.get("total_cost_usd"), "num_turns": result.get("num_turns"),
        "duration_ms": result.get("duration_ms"), "duration_api_ms": result.get("duration_api_ms"),
        "usage": {k: usage.get(k) for k in ("input_tokens", "cache_creation_input_tokens",
                                             "cache_read_input_tokens", "output_tokens")},
        "model_usage": result.get("modelUsage"), "permission_denials": result.get("permission_denials"),
        "terminal_reason": result.get("terminal_reason"), "model": init.get("model"),
        "claude_code_version": init.get("claude_code_version"), "session_tools": len(init.get("tools", [])),
    }
    print(json.dumps(out, ensure_ascii=False, indent=1))
    sys.exit(0 if out["ok"] else 1)


def cmd_line(path):
    with open(path) as f:
        c = json.load(f)
    u = c["usage"]
    failed = [x["check"] for x in c["checks"] if not x["ok"]]
    print("\t".join(str(x) for x in (
        c.get("tag", c["id"]), "PASS" if c["ok"] else "FAIL", " → ".join(t["name"] for t in c["tools"]),
        c["num_turns"], u.get("input_tokens"), u.get("cache_creation_input_tokens"), u.get("cache_read_input_tokens"),
        u.get("output_tokens"), "%.4f" % (c["cost_usd"] or 0), (c["duration_ms"] or 0) // 1000,
        "; ".join(failed) or "-")))


def cmd_summary(outdir):
    rows, total = [], 0.0
    for name in sorted(os.listdir(outdir)):
        if name.endswith(".check.json"):
            with open(os.path.join(outdir, name)) as f:
                c = json.load(f)
            rows.append(c)
            total += c["cost_usd"] or 0
    print("%-12s %-4s %5s %8s %9s %9s %7s %8s %5s  %s" % ("scenario", "ok", "turns", "in", "cache_wr", "cache_rd",
                                                          "out", "cost$", "s", "tools"))
    for c in rows:
        u = c["usage"]
        print("%-12s %-4s %5s %8s %9s %9s %7s %8.4f %5s  %s" % (
            c.get("tag", c["id"]), "PASS" if c["ok"] else "FAIL", c["num_turns"], u.get("input_tokens"),
            u.get("cache_creation_input_tokens"), u.get("cache_read_input_tokens"), u.get("output_tokens"),
            c["cost_usd"] or 0, (c["duration_ms"] or 0) // 1000, " → ".join(t["name"] for t in c["tools"])))
        for x in c["checks"]:
            if not x["ok"]:
                print("      FAIL %s: %s" % (x["check"], x["detail"]))
    print("total cost $%.4f over %d run(s)" % (total, len(rows)))


def main(argv):
    if len(argv) < 2:
        sys.exit(__doc__)
    cmd, args = argv[1], argv[2:]
    fn = {"configure": cmd_configure, "backend": cmd_backend, "responder": cmd_responder, "prompt": cmd_prompt,
          "snapshot": cmd_snapshot, "check": cmd_check, "line": cmd_line, "summary": cmd_summary}.get(cmd)
    if fn is None:
        sys.exit(__doc__)
    fn(*args)


if __name__ == "__main__":
    main(sys.argv)
