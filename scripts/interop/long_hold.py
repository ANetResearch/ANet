#!/usr/bin/env python3
"""long_hold.py — calls that stay open for a long time (docs/notes/0035, long-running part).

A task the provider does not answer ("[hold]", which a2aprobe's responder leaves alone) is held open by
the Python SDK for HOLD seconds in three ways at once — SendStreamingMessage on JSON-RPC, the same on
HTTP+JSON, and a blocking SendMessage (the way Hermes calls, nothing on the wire until the answer) — and
then answered on the provider's control plane (/tasks/reply, what MCP reply_task does). Each call must
come back with the answer, not a timeout or a dropped connection.

    python long_hold.py ENV_JSON HOLD_SECONDS
"""
import asyncio
import sys
import time
import uuid

import httpx

from a2a.types import SendMessageRequest
from a2a.utils.constants import TransportProtocol

import py_client as pc

R = pc.R
S = pc.TaskState


async def provider_task(hx, env, marker, secs=60):
    tok = open(env["prov"]["ctl_token_file"]).read().strip()
    end = time.time() + secs
    while time.time() < end:
        r = await hx.post("http://%s/tasks/list" % env["prov"]["ctl"], headers={"Authorization": "Bearer " + tok},
                          json={"role": "inbound", "page_size": 50, "history_length": 1})
        for t in r.json().get("tasks", []):
            for m in t.get("history", []):
                if any(marker in (p.get("text") or "") for p in m.get("parts", [])):
                    return t["id"]
        await asyncio.sleep(1)
    return None


async def reply(hx, env, tid, text):
    tok = open(env["prov"]["ctl_token_file"]).read().strip()
    r = await hx.post("http://%s/tasks/reply" % env["prov"]["ctl"], headers={"Authorization": "Bearer " + tok},
                      json={"task_id": tid, "text": text, "state": "completed"})
    return r.status_code


async def main():
    env = pc.Env(sys.argv[1])
    hold = int(sys.argv[2])
    nonce = uuid.uuid4().hex[:8]
    timeout = httpx.Timeout(hold + 300, connect=30)
    async with httpx.AsyncClient(timeout=timeout, trust_env=False) as hx:
        p = pc.Probe(env, hx)
        await p.cards()

        async def stream(tag, binding):
            cl = p.client(binding, streaming=True)
            marker = "%s long-%s-%s" % (pc.HOLD, tag, nonce)
            evs, t0 = [], time.time()
            try:
                async for ev in cl.send_message(SendMessageRequest(message=pc.text_msg(marker)),
                                                context=pc.Probe.ctx(timeout=hold + 300)):
                    evs.append((time.time() - t0, ev))
            except Exception as e:  # noqa: BLE001
                return tag, time.time() - t0, None, pc.errname(e)
            last = [ev.status_update.status.state for _, ev in evs if ev.HasField("status_update")]
            return tag, time.time() - t0, (last[-1] if last else None), "%d events" % len(evs)

        async def blocking():
            cl = p.client(TransportProtocol.JSONRPC)
            marker = "%s long-block-%s" % (pc.HOLD, nonce)
            t0 = time.time()
            try:
                t = await p.send(cl, pc.text_msg(marker), timeout=hold + 300)
                return "blocking", time.time() - t0, t.status.state, pc.reply_text(t)
            except Exception as e:  # noqa: BLE001
                return "blocking", time.time() - t0, None, pc.errname(e)

        jobs = [asyncio.create_task(stream("jsonrpc", TransportProtocol.JSONRPC)),
                asyncio.create_task(stream("rest", TransportProtocol.HTTP_JSON)),
                asyncio.create_task(blocking())]
        ids = {}
        for tag in ("jsonrpc", "rest", "block"):
            ids[tag] = await provider_task(hx, env.e, "long-%s-%s" % (tag, nonce))
        R.check(all(ids.values()), "hold-arrived", "the three held tasks are at the provider: %s" % ids)
        await asyncio.sleep(hold)
        codes = {tag: await reply(hx, env.e, tid, "answered after %ds (%s)" % (hold, tag)) for tag, tid in ids.items() if tid}
        R.check(all(c == 200 for c in codes.values()), "hold-replied", "the provider answers after %d s: %s" % (hold, codes))
        for tag, took, st, detail in await asyncio.gather(*jobs):
            R.check(st == S.TASK_STATE_COMPLETED and took >= hold, "hold-%s" % tag,
                    "open %.0f s, ended %s (%s)" % (took, S.Name(st) if st else None, detail))
    return 1 if R.fails else 0


if __name__ == "__main__":
    sys.exit(asyncio.run(main()))
