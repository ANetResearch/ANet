#!/usr/bin/env python3
"""mcp_check.py — item 5 of docs/notes/0035, the fallback path: the official MCP Python SDK (mcp) drives
`anet mcp` exactly as Claude Code would after `anet agents wire claude` — the command, arguments and
environment are read from the mcpServers.anet entry that wire wrote into a private HOME's ~/.claude.json —
through the tool sequence the prompt asks a model for: "list the agents, send <provider> a task, wait for
the result and report it". Used when `claude -p` cannot run there (not logged in).

    python mcp_check.py ENV_JSON CLAUDE_HOME

Also: the tool list and its annotations (A2A-DESIGN §12 table), the a2a-x402 path through MCP
(submit_payment within the agent tier; above it the task keeps waiting), and a size estimate of what a
model would read (instructions, tool definitions, the skill anet wrote, and each result), in tokens at
~4 characters per token — an order of magnitude, not a count.
"""
import asyncio
import json
import os
import sys

from mcp import ClientSession, StdioServerParameters
from mcp.client.stdio import stdio_client

import py_client as pc

R = pc.R

# §12: name → (readOnly, destructive, idempotent, openWorld)
WANT = {
    "list_agents": (True, None, None, True), "get_agent_card": (True, None, None, True),
    "send_message": (False, False, None, True), "get_task": (True, None, None, False),
    "list_tasks": (True, None, None, False), "wait_task": (True, None, None, False),
    "cancel_task": (False, True, True, True), "reply_task": (False, False, None, True),
    "submit_payment": (False, True, None, True), "reject_payment": (False, False, True, True),
    "get_balance": (True, None, None, True), "audit": (True, None, None, False),
    "node_status": (True, None, None, False), "inbound_pending": (True, None, None, False),
}


def text_of(res):
    return "".join(getattr(c, "text", "") for c in res.content)


async def main():
    env = json.load(open(sys.argv[1]))
    home = sys.argv[2]
    agent = env["prov"]["aid"]
    entry = json.load(open(os.path.join(home, ".claude.json")))["mcpServers"]["anet"]
    params = StdioServerParameters(command=entry["command"], args=entry["args"],
                                   env={**entry.get("env", {}), "PATH": "/usr/bin:/bin", "HOME": home})
    chars = {}
    calls = []
    async with stdio_client(params) as (r, w):
        async with ClientSession(r, w) as s:
            init = await s.initialize()
            chars["instructions"] = len(init.instructions or "")
            R.check(init.server_info.name and init.instructions, "mcp-initialize",
                    "%s %s, instructions %d chars" % (init.server_info.name, init.server_info.version, chars["instructions"]))
            tl = await s.list_tools()
            names = [t.name for t in tl.tools]
            chars["tools"] = len(json.dumps([t.model_dump(exclude_none=True) for t in tl.tools]))
            bad = []
            for t in tl.tools:
                a = t.annotations
                got = (a.read_only_hint, a.destructive_hint, a.idempotent_hint, a.open_world_hint) if a else None
                want = WANT.get(t.name)
                # MCP defaults: a hint left unset reads as the default (destructive/openWorld true).
                if want is None or got is None or any(w is not None and g is not None and w != g for w, g in zip(want, got)) \
                        or got[0] is None or got[3] is None:
                    bad.append("%s %s" % (t.name, got))
            R.check(sorted(names) == sorted(WANT) and not bad, "mcp-tools",
                    "%d tools, annotations as §12%s" % (len(names), "" if not bad else "; differ: %s" % bad))

            async def call(name, args):
                res = await s.call_tool(name, args)
                txt = text_of(res)
                chars.setdefault("results", 0)
                chars["results"] += len(txt)
                calls.append(name)
                if res.is_error:
                    return None, txt
                try:
                    return json.loads(txt), txt
                except ValueError:
                    return None, txt

            # The sequence the prompt asks for.
            la, raw = await call("list_agents", {})
            ids = [a.get("aid") for a in (la or {}).get("agents", [])]
            ver = [a.get("verification") for a in (la or {}).get("agents", []) if a.get("aid") == agent]
            R.check(agent in ids and ver == ["VERIFIED"], "mcp-list_agents", "%d agent(s), the provider %s" % (len(ids), ver))
            text = "请用一句话总结:anet 让本机 agent 经 A2A 协议与远端 agent 协作。"
            t, raw = await call("send_message", {"to": agent, "text": text})
            tid = (t or {}).get("id")
            st = ((t or {}).get("status") or {}).get("state")
            R.check(tid and st, "mcp-send_message", "task %s, %s" % (tid, st))
            if st not in ("TASK_STATE_COMPLETED", "TASK_STATE_FAILED", "TASK_STATE_REJECTED", "TASK_STATE_CANCELED"):
                t, raw = await call("wait_task", {"task_id": tid, "timeout_seconds": 120})
                st = ((t or {}).get("status") or {}).get("state")
            g, raw = await call("get_task", {"task_id": tid})
            reply = "".join(p.get("text", "") for a in (g or {}).get("artifacts", []) if a.get("artifactId") == "anet.reply"
                            for p in a.get("parts", []))
            R.check(st == "TASK_STATE_COMPLETED" and reply and g["metadata"].get("anet.receipt_verified") == "verified",
                    "mcp-result", "%s, anet.reply %r, receipt %s" % (st, reply[:90], (g or {}).get("metadata", {}).get("anet.receipt_verified")))
            R.note("mcp-sequence", " → ".join(calls))

            # a2a-x402 through MCP (§8.6 agent tier): quote, submit_payment, wait.
            q, raw = await call("send_message", {"to": agent, "skill": env["skills"]["paid"], "args": {"text": "mcp paid"}})
            qst = ((q or {}).get("status") or {}).get("state")
            if qst == "TASK_STATE_INPUT_REQUIRED":
                p, raw = await call("submit_payment", {"task_id": q["id"]})
                w, raw = await call("wait_task", {"task_id": q["id"], "timeout_seconds": 60})
                R.check(((w or {}).get("status") or {}).get("state") == "TASK_STATE_COMPLETED" and
                        (w or {}).get("metadata", {}).get("x402.payment.status") == "payment-completed", "mcp-pay",
                        "submit_payment → %s, %s" % (((w or {}).get("status") or {}).get("state"),
                                                     (w or {}).get("metadata", {}).get("x402.payment.status")))
            else:
                R.fail("mcp-pay", "no quote: %s %s" % (qst, raw[:200]))
            q, raw = await call("send_message", {"to": agent, "skill": env["skills"]["pricey"], "args": {"text": "mcp pricey"}})
            if ((q or {}).get("status") or {}).get("state") == "TASK_STATE_INPUT_REQUIRED":
                p, raw = await call("submit_payment", {"task_id": q["id"]})
                # /tasks/pay answers with the decision, not the task (§12): nothing signed, the task waits.
                p = p or {}
                R.check(p.get("anet.reason") == "needs_operator_approval" and p.get("spend_refusal") == "over_single_limit" and
                        p.get("x402.payment.status") != "payment-submitted", "mcp-pay-over-limit",
                        "above agent_max: state %s, anet.reason=%s, spend_refusal=%s" % (
                            p.get("state"), p.get("anet.reason"), p.get("spend_refusal")))
                g2, raw = await call("get_task", {"task_id": q["id"]})
                R.check(((g2 or {}).get("status") or {}).get("state") == "TASK_STATE_INPUT_REQUIRED", "mcp-pay-over-limit-task",
                        "the task still waits: %s" % (((g2 or {}).get("status") or {}).get("state")))
                await call("reject_payment", {"task_id": q["id"]})
            else:
                R.fail("mcp-pay-over-limit", "no quote for %s" % env["skills"]["pricey"])
    skill = os.path.join(home, ".claude", "skills", "anet", "SKILL.md")
    chars["skill"] = os.path.getsize(skill) if os.path.exists(skill) else 0
    total = sum(chars.values())
    R.note("mcp-size", "a model would read ~%d tokens: instructions %d, tool definitions %d, skill %d, results %d chars (÷4)" % (
        total // 4, chars.get("instructions", 0), chars.get("tools", 0), chars.get("skill", 0), chars.get("results", 0)))
    return 1 if R.fails else 0


if __name__ == "__main__":
    sys.exit(asyncio.run(main()))
