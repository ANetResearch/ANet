#!/usr/bin/env python3
"""py_backend_check.py — item 3 of docs/notes/0035: requesters' text tasks, through anet, to an A2A server
built with the Python SDK that stands behind the provider as its §11.6 backend (py_backend.py).

    python py_backend_check.py ENV_JSON BACKEND_LOG [BACKEND_URL]

Needs: up.sh backend <py_backend URL> <token file> done, and the requester and "other" on the provider's
peers.trust (the check takes "other" off it for the untrusted case and puts it back).

Checks: the reply comes from the backend (anet.reply is its summary), what the backend saw (its contextId
is not the requester's and is derived per peer — Q23: same peer + same context → same backend context;
another peer with the same contextId → another backend context; anet.peer_aid/anet.trusted in the message
metadata), input-required and the follow-up on the same task, and that an untrusted peer's task is not
forwarded. The requester side is driven with the SDK's client, as py_client.py does.
"""
import asyncio
import json
import subprocess
import sys
import uuid

import httpx

from a2a.client import A2ACardResolver, AuthInterceptor, ClientConfig, ClientFactory
from a2a.types import GetTaskRequest, TaskState
from a2a.utils.constants import TransportProtocol

import py_client as pc

S = TaskState
R = pc.R


def backend_log(path):
    out = []
    try:
        with open(path) as f:
            for ln in f:
                try:
                    out.append(json.loads(ln))
                except ValueError:
                    pass
    except OSError:
        pass
    return out


async def client_for(hx, node, agent):
    token = open(node["a2a_token_file"]).read().strip()
    base = "http://%s/a2a/v1/agents/%s" % (node["a2a"], agent)
    card = await A2ACardResolver(hx, base).get_agent_card(http_kwargs={"headers": {"Authorization": "Bearer " + token}})
    cfg = ClientConfig(streaming=False, httpx_client=hx, supported_protocol_bindings=[TransportProtocol.JSONRPC])
    return ClientFactory(cfg).create(card, interceptors=[AuthInterceptor(pc.Creds(token))])


async def send(cl, text, ctx=None, task_id=None, immediate=False):
    p = pc.Probe.__new__(pc.Probe)
    return await pc.Probe.send(p, cl, pc.text_msg(text, ctx, task_id), immediate=immediate)


async def main():
    env = json.load(open(sys.argv[1]))
    logp = sys.argv[2]
    agent = env["prov"]["aid"]
    nonce = "pb" + uuid.uuid4().hex[:8]
    upsh = __file__.rsplit("/", 1)[0] + "/up.sh"

    def untrust(aid, on):
        subprocess.run(["bash", upsh, "trust" if on else "untrust", aid], check=True,
                       env={"J": env["run"].rsplit("/run", 1)[0], "PATH": "/usr/bin:/bin"})

    async with httpx.AsyncClient(timeout=180, trust_env=False) as hx:
        rq = await client_for(hx, env["req"], agent)
        ot = await client_for(hx, env["other"], agent)

        # 1. a plain text task: answered by the backend.
        shared = "ctx-shared-" + nonce
        t1 = await send(rq, "please summarize this short text for the interop run %s" % nonce, ctx=shared)
        reply = pc.reply_text(t1) if t1 else None
        R.check(t1 and t1.status.state == S.TASK_STATE_COMPLETED and reply and reply.startswith("summary: please summarize"),
                "backend-reply", "%s, anet.reply %r, receipt %s" % (pc.state(t1), reply, pc.meta(t1, "anet.receipt_verified")))
        log = [x for x in backend_log(logp) if nonce in x.get("text", "")]
        first = log[0] if log else {}
        md = first.get("metadata", {})
        R.check(len(log) == 1 and md.get("anet.peer_aid") == env["req"]["aid"] and md.get("anet.trusted") is True,
                "backend-saw", "%d request(s); metadata anet.peer_aid=%s… anet.trusted=%s" % (
                    len(log), str(md.get("anet.peer_aid"))[:16], md.get("anet.trusted")))
        bctx = first.get("contextId")
        R.check(bctx and bctx != shared and bctx != (t1.context_id if t1 else None), "backend-ctx-derived",
                "the backend's contextId %s is neither the client's (%s) nor anet's" % (bctx, shared))

        # 2. Q23: the same peer and context → the same backend context; another context → another.
        t2 = await send(rq, "second message in the same context %s" % nonce, ctx=shared)
        t3 = await send(rq, "a message in another context %s" % nonce, ctx="ctx-other-" + nonce)
        log = [x for x in backend_log(logp) if nonce in x.get("text", "")]
        by = {x["text"].split(" %s" % nonce)[0]: x["contextId"] for x in log}
        R.check(t2 and t2.status.state == S.TASK_STATE_COMPLETED and by.get("second message in the same context") == bctx,
                "q23-same-peer-same-ctx", "second task %s, backend context %s" % (
                    pc.state(t2), "same" if by.get("second message in the same context") == bctx else by))
        R.check(t3 and by.get("a message in another context") not in (None, bctx), "q23-other-ctx",
                "another context of the same peer → another backend context")

        # 3. Q23: another peer with the very same contextId → a backend context of its own.
        t4 = await send(ot, "the other peer, same contextId %s" % nonce, ctx=shared)
        log = [x for x in backend_log(logp) if nonce in x.get("text", "")]
        o = [x for x in log if x["text"].startswith("the other peer")]
        octx = o[0]["contextId"] if o else None
        R.check(t4 and t4.status.state == S.TASK_STATE_COMPLETED and octx and octx != bctx and
                o[0]["metadata"].get("anet.peer_aid") == env["other"]["aid"], "q23-other-peer",
                "other's task %s; backend context %s vs requester's %s" % (pc.state(t4), octx, bctx))
        R.check(t4 and pc.reply_text(t4) and bctx not in pc.reply_text(t4), "q23-no-crosstalk",
                "the other peer's answer does not name the requester's backend context")

        # 4. input-required from the backend, then the follow-up on the same task.
        t5 = await send(rq, "[ask] summarize something %s" % nonce, ctx="ctx-ask-" + nonce)
        R.check(t5 and t5.status.state == S.TASK_STATE_INPUT_REQUIRED and "which part" in pc.d(t5).get("status", {}).get(
            "message", {}).get("parts", [{}])[0].get("text", ""), "backend-input-required",
                "%s, question %r" % (pc.state(t5), pc.d(t5).get("status", {}).get("message", {}).get("parts")))
        if t5:
            t6 = await send(rq, "the first paragraph, please %s" % nonce, task_id=t5.id, ctx=t5.context_id)
            log = [x for x in backend_log(logp) if nonce in x.get("text", "")]
            ask = [x for x in log if x["text"].startswith("[ask]")]
            fol = [x for x in log if x["text"].startswith("the first paragraph")]
            R.check(t6 and t6.id == t5.id and t6.status.state == S.TASK_STATE_COMPLETED and fol and ask and
                    fol[0]["contextId"] == ask[0]["contextId"], "backend-follow-up",
                    "follow-up on the same task: %s, same backend context %s" % (
                        pc.state(t6), bool(fol and ask and fol[0]["contextId"] == ask[0]["contextId"])))

        # 5. an untrusted peer: allowed to delegate (peers.allow) but not on peers.trust → not forwarded.
        untrust(env["other"]["aid"], False)
        try:
            t7 = await send(ot, "untrusted, must not reach the backend %s" % nonce, immediate=True)
            await asyncio.sleep(20)
            g = await ot.get_task(GetTaskRequest(id=t7.id), context=pc.Probe.ctx())
            log = [x for x in backend_log(logp) if "untrusted, must not" in x.get("text", "")]
            R.check(not log and g.status.state in (S.TASK_STATE_SUBMITTED, S.TASK_STATE_WORKING), "untrusted-not-forwarded",
                    "after 20 s: the backend saw %d request(s); the task is %s" % (len(log), pc.state(g)))
            tok = open(env["prov"]["ctl_token_file"]).read().strip()
            r = await hx.post("http://%s/tasks/get" % env["prov"]["ctl"], headers={"Authorization": "Bearer " + tok},
                              json={"task_id": t7.id})
            R.check(r.status_code == 200 and (r.json().get("metadata") or {}).get("anet.trust") == "peer", "untrusted-in-inbox",
                    "the provider holds it in its inbox: HTTP %d, anet.trust=%s" % (
                        r.status_code, (r.json().get("metadata") or {}).get("anet.trust")))
            await ot.cancel_task(pc.CancelTaskRequest(id=t7.id), context=pc.Probe.ctx())
        finally:
            untrust(env["other"]["aid"], True)

        # 6. the backend's own door: no token, no card.
        burl = sys.argv[3] if len(sys.argv) > 3 else "http://%s" % env["a2a_backend_addr"]
        if burl.startswith("unix://"):
            async with httpx.AsyncClient(transport=httpx.AsyncHTTPTransport(uds=burl[len("unix://"):])) as ux:
                r = await ux.get("http://localhost/.well-known/agent-card.json")
        else:
            r = await hx.get(burl + "/.well-known/agent-card.json")
        R.check(r.status_code == 401, "backend-token", "the backend refuses a caller without anet's token: %d" % r.status_code)
        refused = [x for x in backend_log(logp) if "refused" in x]
        R.note("backend-refusals", "%d refused request(s) in the backend log (this check's own included)" % len(refused))
    return 1 if R.fails else 0


if __name__ == "__main__":
    sys.exit(asyncio.run(main()))
