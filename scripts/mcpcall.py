#!/usr/bin/env python3
"""mcpcall.py — drive `anet mcp` over stdio and print one tool's result.

The MCP surface is what an agent actually reaches ANet through, and it
had only ever been exercised against a fake control plane. This speaks
the wire protocol directly so a shell test can check it against a real
daemon and a real hub without pulling in an MCP client library.

  mcpcall.py <anet-binary> list
  mcpcall.py <anet-binary> call <tool> '<json-args>'
  mcpcall.py <anet-binary> op <find|send|result|status> '<json-args>'

`list` also says which generation of tool names the server speaks and
which of that generation's tools are missing. `op` names an operation
rather than a tool: it picks the tool and the argument names for the
generation the server speaks, and prints a normalised answer, so a shell
test is written once for both generations (see TOOLS_* below).

Environment (HOME, ANET_HOME) is inherited, so the caller decides which
node this talks to.
"""
import json
import subprocess
import sys

# ── tool names, two generations ─────────────────────────────────
# A2A-DESIGN §12 regroups the MCP tools around A2A's concepts
# (list_agents, send_message, get_task …). Until that lands the daemon
# still serves the first generation. Every joint script takes tool and
# argument names from here and nowhere else: tools/list decides which
# generation a server speaks, and a rename is a change in this one place.
TOOLS_V2 = (
    "list_agents", "get_agent_card", "send_message", "get_task", "list_tasks",
    "wait_task", "cancel_task", "reply_task", "submit_payment", "reject_payment",
    "get_balance", "audit", "node_status", "inbound_pending",
)
TOOLS_V1 = (
    "agents_find", "task_delegate", "task_results", "task_inbox", "task_message",
    "task_end", "evidence_read", "credit_balance", "node_status",
)

# The operations the joint scripts perform, and the tool each generation
# performs it with. "result" reads one task: get_task in v2; in v1 there
# is no single-task read, so task_results is scanned for the id.
OPS = {
    "find":   {"v2": "list_agents",  "v1": "agents_find"},
    "send":   {"v2": "send_message", "v1": "task_delegate"},
    "result": {"v2": "get_task",     "v1": "task_results"},
    "status": {"v2": "node_status",  "v1": "node_status"},
}

# Logical argument name → the real names it may have, tried in order
# against the tool's inputSchema. The first one the schema declares wins;
# a schema that declares none of them gets the first. The first-generation
# names are listed too, so the same table serves both.
ARGS = {
    "agent":      ("agent", "agent_aid", "aid", "to", "provider"),
    "capability": ("skill", "capability"),
    "args":       ("args", "arguments", "skill_args"),
    "text":       ("text", "message", "goal"),
    "task":       ("task_id", "id", "interaction_id"),
    "query":      ("query", "q", "text"),
}
# find by capability id: v2 filters by skill (or tag); a schema with
# neither gets the id as free text.
FIND_CAPABILITY = ("skill", "capability", "tag", "query")

TERMINAL = ("completed", "failed", "canceled", "rejected")


def generation(names):
    """"v2", "v1" or "?" for a tools/list result."""
    s = set(names)
    if "send_message" in s or "list_agents" in s:
        return "v2"
    if "task_delegate" in s or "agents_find" in s:
        return "v1"
    return "?"


def missing(names, gen):
    want = TOOLS_V2 if gen == "v2" else TOOLS_V1 if gen == "v1" else ()
    have = set(names)
    return [t for t in want if t not in have]


def arg_names(logical, schema, candidates=None):
    """The real argument name for a logical one, per the tool's schema."""
    cands = candidates or ARGS.get(logical, (logical,))
    props = (schema or {}).get("properties") or {}
    for c in cands:
        if c in props:
            return c
    return cands[0]


def build_args(op, logical, schema):
    """Rename logical arguments to the ones this tool declares."""
    out = {}
    for k, v in logical.items():
        if op == "find" and k == "capability":
            out[arg_names(k, schema, FIND_CAPABILITY)] = v
        else:
            out[arg_names(k, schema)] = v
    return out


def payload(result):
    """A tool result as JSON: structuredContent, else the text parsed."""
    if result is None:
        return None
    sc = result.get("structuredContent")
    if sc is not None:
        return sc
    text = "".join(c.get("text", "") for c in result.get("content") or [])
    try:
        return json.loads(text)
    except (ValueError, TypeError):
        return None


def task_state(s):
    """An A2A state in either spelling ("TASK_STATE_COMPLETED" or
    "completed"), lower-cased and without the prefix."""
    s = (s or "").lower()
    return s[len("task_state_"):] if s.startswith("task_state_") else s


def normalise_task(t):
    """The fields a joint script asserts on, from an A2A Task (v2)."""
    if not isinstance(t, dict):
        return {}
    if isinstance(t.get("task"), dict):  # a SendMessage response wraps it
        t = t["task"]
    md = t.get("metadata") or {}
    state = task_state((t.get("status") or {}).get("state"))
    # The receipt is task metadata (0017 Q21 P2); older daemons sent it as
    # an artifact.
    receipt = bool(md.get("anet.result_cid")) or bool(md.get("anet.receipt")) or any(
        (a or {}).get("name") == "anet.receipt" for a in t.get("artifacts") or [])
    return {
        "task_id": t.get("id", ""),
        "state": state,
        "done": state in TERMINAL,
        "effect_status": md.get("anet.effect_status", ""),
        "receipt": receipt,
    }


class Session:
    def __init__(self, argv):
        self.p = subprocess.Popen(
            argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
            stderr=subprocess.PIPE, text=True, bufsize=1)
        self.n = 0
        self.tools = None  # name → inputSchema, from tools/list

    def send(self, method, params=None, notify=False):
        msg = {"jsonrpc": "2.0", "method": method}
        if params is not None:
            msg["params"] = params
        if not notify:
            self.n += 1
            msg["id"] = self.n
        self.p.stdin.write(json.dumps(msg) + "\n")
        self.p.stdin.flush()
        if notify:
            return None
        # Skip anything that is not the reply to this id: a server may
        # emit notifications, and treating the first line as the answer
        # would read one of those as the result.
        while True:
            line = self.p.stdout.readline()
            if not line:
                err = self.p.stderr.read()
                raise SystemExit("mcp: server closed the pipe: " + err.strip())
            try:
                got = json.loads(line)
            except json.JSONDecodeError:
                continue
            if got.get("id") == msg["id"]:
                if "error" in got:
                    raise SystemExit("mcp: " + json.dumps(got["error"]))
                return got.get("result")

    def list_tools(self):
        if self.tools is None:
            out = self.send("tools/list", {})
            self.tools = {t["name"]: t.get("inputSchema") or {} for t in out.get("tools", [])}
        return self.tools

    def call(self, tool, args):
        return self.send("tools/call", {"name": tool, "arguments": args})

    def close(self):
        try:
            self.p.stdin.close()
            self.p.wait(timeout=10)
        except Exception:
            self.p.kill()


def text_of(result):
    return "".join(c.get("text", "") for c in (result or {}).get("content", []))


def op(s, name, logical):
    """Run one logical operation and print a normalised answer."""
    tools = s.list_tools()
    gen = generation(tools)
    if name not in OPS:
        raise SystemExit("mcp: unknown op %r (want one of %s)" % (name, ", ".join(OPS)))
    tool = OPS[name].get(gen)
    if not tool or tool not in tools:
        return {"generation": gen, "tool": tool or "", "is_error": True,
                "text": "server has no tool for %r" % name}
    out = {"generation": gen, "tool": tool}
    if name == "result" and gen == "v1":
        # No single-task read in v1: list the finished ones and look.
        want = logical.get("task", "")
        res = s.call(tool, {})
        out.update(is_error=bool(res.get("isError")), text=text_of(res), done=False)
        for r in (payload(res) or {}).get("results") or []:
            if r.get("interaction_id") != want:
                continue
            # A text task's result is prose, which may even parse as a
            # bare JSON number or string; only an object has a status.
            try:
                parsed = json.loads(r.get("result") or "{}")
            except ValueError:
                parsed = None
            if isinstance(parsed, dict):
                status = parsed.get("status", "")
            else:
                status = "OK" if '"status":"OK"' in (r.get("result") or "") else ""
            out.update(task_id=want, done=True, state="completed",
                       effect_status=status, receipt=bool(r.get("receipt_cid")))
            break
        return out
    res = s.call(tool, build_args(name, logical, tools[tool]))
    body = payload(res)
    out.update(is_error=bool(res.get("isError")), text=text_of(res))
    if name == "find":
        agents = body if isinstance(body, list) else (body or {}).get("agents") or []
        out["aids"] = [a.get("aid", "") for a in agents if isinstance(a, dict)]
    elif name == "send" and gen == "v1":
        out["task_id"] = (body or {}).get("interaction_id", "")
    elif name in ("send", "result"):
        out.update(normalise_task(body))
    return out


def main():
    if len(sys.argv) < 3:
        raise SystemExit(__doc__)
    binary, verb = sys.argv[1], sys.argv[2]
    s = Session([binary, "mcp"])
    s.send("initialize", {
        "protocolVersion": "2025-06-18",
        "capabilities": {},
        "clientInfo": {"name": "prodtest", "version": "1"},
    })
    s.send("notifications/initialized", {}, notify=True)
    if verb == "list":
        names = list(s.list_tools())
        gen = generation(names)
        print(json.dumps({"tools": names, "generation": gen, "missing": missing(names, gen)}))
    elif verb == "call":
        tool = sys.argv[3]
        args = json.loads(sys.argv[4]) if len(sys.argv) > 4 else {}
        out = s.call(tool, args)
        # A tool that failed reports isError with the reason in content;
        # both are printed, because "the call failed" and "the call
        # returned an unhappy answer" are different facts.
        print(json.dumps({"is_error": bool(out.get("isError")), "text": text_of(out)}))
    elif verb == "op":
        args = json.loads(sys.argv[4]) if len(sys.argv) > 4 else {}
        print(json.dumps(op(s, sys.argv[3], args)))
    else:
        raise SystemExit(__doc__)
    s.close()


if __name__ == "__main__":
    main()
