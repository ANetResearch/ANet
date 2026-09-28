#!/usr/bin/env python3
"""burst.py — many real-client calls at once (docs/notes/0035, long-running part): N blocking SendMessage
on JSON-RPC, N SendStreamingMessage on HTTP+JSON and N free capability calls, all started together with
the Python SDK through one requester's local A2A interface. Every one must end completed with its own
answer; the latencies are reported.

    python burst.py ENV_JSON N
"""
import asyncio
import json
import statistics
import sys
import time
import uuid

import httpx

from a2a.types import SendMessageRequest
from a2a.utils.constants import TransportProtocol

import py_client as pc

R = pc.R
S = pc.TaskState


async def main():
    env = pc.Env(sys.argv[1])
    n = int(sys.argv[2])
    limits = httpx.Limits(max_connections=4 * n, max_keepalive_connections=4 * n)
    async with httpx.AsyncClient(timeout=600, limits=limits, trust_env=False) as hx:
        p = pc.Probe(env, hx)
        await p.cards()
        blk = p.client(TransportProtocol.JSONRPC)
        stm = p.client(TransportProtocol.HTTP_JSON, streaming=True)
        tag = uuid.uuid4().hex[:6]

        async def blocking(i):
            text = "burst %s blocking %d" % (tag, i)
            t0 = time.time()
            t = await p.send(blk, pc.text_msg(text), timeout=600)
            return "blocking", time.time() - t0, t.status.state == S.TASK_STATE_COMPLETED and pc.reply_text(t) == pc.ECHO + text

        async def stream(i):
            text = "burst %s stream %d" % (tag, i)
            t0 = time.time()
            evs = []
            async for ev in stm.send_message(SendMessageRequest(message=pc.text_msg(text)), context=pc.Probe.ctx(timeout=600)):
                evs.append(ev)
            art = "".join(q.text for ev in evs if ev.HasField("artifact_update") for q in ev.artifact_update.artifact.parts
                          if q.WhichOneof("content") == "text")
            last = [ev.status_update.status.state for ev in evs if ev.HasField("status_update")]
            return "stream", time.time() - t0, bool(last) and last[-1] == S.TASK_STATE_COMPLETED and art == pc.ECHO + text

        async def cap(i):
            text = "burst %s cap %d" % (tag, i)
            t0 = time.time()
            t = await p.send(blk, pc.data_msg({"skill": env.skills["free"], "args": {"text": text}}), timeout=600)
            res = pc.artifact(t, "anet.result")
            return "capability", time.time() - t0, t.status.state == S.TASK_STATE_COMPLETED and bool(res) and \
                pc.sha(text) in json.dumps(pc.d(res))

        t0 = time.time()
        out = await asyncio.gather(*[f(i) for i in range(n) for f in (blocking, stream, cap)], return_exceptions=True)
        wall = time.time() - t0
        for kind in ("blocking", "stream", "capability"):
            rows = [o for o in out if not isinstance(o, BaseException) and o[0] == kind]
            ok = [o for o in rows if o[2]]
            lat = sorted(o[1] for o in rows)
            R.check(len(ok) == n, "burst-%s" % kind, "%d/%d completed with their own answer; latency median %.1f s, max %.1f s" % (
                len(ok), n, statistics.median(lat) if lat else -1, lat[-1] if lat else -1))
        errs = [o for o in out if isinstance(o, BaseException)]
        R.check(not errs, "burst-errors", "%d raised%s; wall %.1f s for %d calls" % (
            len(errs), (": " + pc.errname(errs[0])) if errs else "", wall, 3 * n))
    return 1 if R.fails else 0


if __name__ == "__main__":
    sys.exit(asyncio.run(main()))
