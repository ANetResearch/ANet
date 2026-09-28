#!/usr/bin/env python3
"""py_client.py — the official A2A Python SDK (a2a-sdk) against a requester's local A2A interface
(docs/notes/0035 item 1; A2A-DESIGN §11, §8.7, §10.3).

Only the SDK's public client surface is used, as its README shows it: A2ACardResolver, ClientFactory
with ClientConfig (JSON-RPC and HTTP+JSON), AuthInterceptor with a CredentialService for the card's
Bearer scheme, ClientCallContext.service_parameters with with_a2a_extensions for a2a-x402, and
a2a.utils.signing.create_signature_verifier for the cards. What the SDK hands back is compared with
anet's own projection of the same task: the raw JSON-RPC GetTask body and the control plane's
/tasks/get.

    python py_client.py ENV_JSON [--only NAME,...]

ENV_JSON is what scripts/interop/up.sh wrote. One line per check: PASS/FAIL/NOTE id: detail.
Exit 0 when nothing failed, 1 when something did, 2 when it could not run.
"""
import asyncio
import base64
import datetime
import re
import hashlib
import json
import os
import sys
import time
import uuid

import httpx
from google.protobuf import json_format, struct_pb2

from a2a.client import (A2ACardResolver, AgentCardResolutionError, AuthInterceptor,
                        ClientCallContext, ClientConfig, ClientFactory, CredentialService)
from a2a.client.card_resolver import parse_agent_card
from a2a.client.service_parameters import ServiceParametersFactory, with_a2a_extensions
from a2a.types import (CancelTaskRequest, GetExtendedAgentCardRequest, GetTaskRequest, ListTasksRequest, Message,
                       Part, Role, SendMessageConfiguration, SendMessageRequest, SubscribeToTaskRequest,
                       TaskPushNotificationConfig, TaskState)
from a2a.utils.constants import TransportProtocol
from a2a.utils.signing import InvalidSignaturesError, create_signature_verifier
from jwt.api_jwk import PyJWK

X402 = "https://github.com/google-agentic-commerce/a2a-x402/blob/main/spec/v0.2"
ORIGIN = "https://agentnetwork.org.cn/a2a/ext/anet-origin/v1"
ECHO = "echo: "
HOLD = "[hold]"
S = TaskState
TS = re.compile(r"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?Z")


class Report:
    def __init__(self):
        self.fails = 0

    t0 = time.time()

    def line(self, kind, cid, msg):
        # PYCLIENT_TIMES=1: each line ends with the seconds since the start (to see which step is slow).
        at = " [%.1fs]" % (time.time() - self.t0) if os.environ.get("PYCLIENT_TIMES") else ""
        print("%s %s: %s%s" % (kind, cid, msg, at), flush=True)

    def ok(self, cid, msg=""):
        self.line("PASS", cid, msg)

    def fail(self, cid, msg=""):
        self.fails += 1
        self.line("FAIL", cid, msg)

    def note(self, cid, msg=""):
        self.line("NOTE", cid, msg)

    def check(self, cond, cid, msg=""):
        (self.ok if cond else self.fail)(cid, msg)
        return cond


R = Report()


def d(msg):
    """A proto message as the dict the SDK would show a caller."""
    return json_format.MessageToDict(msg)


def meta(task, key):
    """key on the task, else on its status message (as a2aprobe reads it)."""
    td = d(task)
    if key in td.get("metadata", {}):
        return td["metadata"][key]
    return ((td.get("status") or {}).get("message") or {}).get("metadata", {}).get(key)


def status_meta(task, key):
    return ((d(task).get("status") or {}).get("message") or {}).get("metadata", {}).get(key)


def state(task):
    return TaskState.Name(task.status.state) if task is not None else "none"


def artifact(task, name):
    for a in task.artifacts:
        if a.artifact_id == name or a.name == name:
            return a
    return None


def reply_text(task):
    a = artifact(task, "anet.reply")
    return "".join(p.text for p in a.parts if p.WhichOneof("content") == "text") if a else None


def struct(obj):
    s = struct_pb2.Struct()
    s.update(obj)
    return s


def text_msg(text, ctx=None, task_id=None, md=None):
    m = Message(role=Role.ROLE_USER, message_id=uuid.uuid4().hex, parts=[Part(text=text)])
    if ctx:
        m.context_id = ctx
    if task_id:
        m.task_id = task_id
    if md:
        m.metadata.CopyFrom(struct(md))
    return m


def data_msg(data, md=None, ctx=None):
    v = struct_pb2.Value()
    v.struct_value.update(data)
    m = Message(role=Role.ROLE_USER, message_id=uuid.uuid4().hex, parts=[Part(data=v)])
    if md:
        m.metadata.CopyFrom(struct(md))
    if ctx:
        m.context_id = ctx
    return m


def sha(text):
    return hashlib.sha256(text.encode()).hexdigest()


class Creds(CredentialService):
    """The card's Bearer scheme gets the node's local A2A token."""

    def __init__(self, token):
        self.token = token

    async def get_credentials(self, scheme, context):
        return self.token


class Env:
    def __init__(self, path):
        e = json.load(open(path))
        self.e = e
        self.hub = e["hub"]
        self.req, self.prov, self.other = e["req"], e["prov"], e["other"]
        self.token = open(self.req["a2a_token_file"]).read().strip()
        self.ctl_token = open(self.req["ctl_token_file"]).read().strip()
        self.prov_ctl_token = open(self.prov["ctl_token_file"]).read().strip()
        self.agent = self.prov["aid"]
        self.base = "http://%s/a2a/v1/agents/%s" % (self.req["a2a"], self.agent)
        self.skills = e["skills"]


class Probe:
    def __init__(self, env, hx):
        self.env = env
        self.hx = hx
        self.nonce = "py" + uuid.uuid4().hex[:10]
        self.card = None
        self.resp_headers = []  # response headers of the SDK's own calls, newest last

    # -- plumbing ------------------------------------------------------
    def client(self, binding, streaming=False, polling=False, card=None):
        cfg = ClientConfig(streaming=streaming, polling=polling, httpx_client=self.hx,
                           supported_protocol_bindings=[binding])
        return ClientFactory(cfg).create(card or self.card, interceptors=[AuthInterceptor(Creds(self.env.token))])

    @staticmethod
    def ctx(x402=False, timeout=120):
        c = ClientCallContext(timeout=timeout)
        if x402:
            c.service_parameters = ServiceParametersFactory.create([with_a2a_extensions([X402])])
        return c

    async def send(self, cl, msg, immediate=False, x402=False, timeout=120):
        """SendMessage (non-streaming client): the one Task it answers."""
        req = SendMessageRequest(message=msg)
        if immediate:
            req.configuration.CopyFrom(SendMessageConfiguration(return_immediately=True))
        out = None
        async for ev in cl.send_message(req, context=self.ctx(x402, timeout)):
            if ev.HasField("task"):
                out = ev.task
            elif ev.HasField("message"):
                raise AssertionError("answered with a Message, not a Task")
        return out

    async def stream(self, cl, msg, x402=False, timeout=120):
        evs = []
        async for ev in cl.send_message(SendMessageRequest(message=msg), context=self.ctx(x402, timeout)):
            evs.append(ev)
        return evs

    async def wait_terminal(self, cl, tid, secs=90):
        end = time.time() + secs
        t = None
        while time.time() < end:
            t = await cl.get_task(GetTaskRequest(id=tid), context=self.ctx())
            if t.status.state in (S.TASK_STATE_COMPLETED, S.TASK_STATE_FAILED, S.TASK_STATE_CANCELED,
                                  S.TASK_STATE_REJECTED, S.TASK_STATE_INPUT_REQUIRED):
                return t
            await asyncio.sleep(0.3)
        return t

    async def raw_rpc(self, method, params, token=None, headers=None):
        """The same call as bytes on the wire, no SDK: anet's projection as it sends it."""
        h = {"Authorization": "Bearer " + (token or self.env.token), "A2A-Version": "1.0",
             "Content-Type": "application/json"}
        h.update(headers or {})
        r = await self.hx.post(self.env.base + "/jsonrpc", headers=h,
                               json={"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
        return r

    async def ctl(self, path, body, prov=False):
        n = self.env.prov if prov else self.env.req
        tok = self.env.prov_ctl_token if prov else self.env.ctl_token
        r = await self.hx.post("http://%s%s" % (n["ctl"], path), headers={"Authorization": "Bearer " + tok},
                               json=body)
        return r.json()

    async def same_as_projection(self, cid, task):
        """What the SDK parsed equals what anet sent (JSON-RPC GetTask) and what the control plane shows."""
        r = await self.raw_rpc("GetTask", {"id": task.id})
        raw = r.json().get("result")
        sdk = d(task)
        diffs = diff(canon(raw), canon(sdk))
        c = await self.ctl("/tasks/get", {"task_id": task.id})
        cstate = (c.get("status") or {}).get("state")
        ckeys = sorted(k for k in (c.get("metadata") or {}) if k.startswith("anet.") or k.startswith("x402."))
        skeys = sorted(k for k in sdk.get("metadata", {}) if k.startswith("anet.") or k.startswith("x402."))
        same_meta = all(canon(c["metadata"][k]) == canon(sdk["metadata"].get(k)) for k in ckeys
                        if k not in ("anet.receipt",))
        R.check(not diffs and cstate == state(task) and ckeys == skeys and same_meta, cid,
                "SDK view == JSON-RPC bytes%s; control plane %s, metadata keys %s" % (
                    "" if not diffs else " EXCEPT " + "; ".join(diffs[:4]), cstate,
                    "equal" if ckeys == skeys and same_meta else "differ: ctl %s sdk %s" % (ckeys, skeys)))

    # -- 1. cards --------------------------------------------------------
    async def key_for(self, kid, jku):
        """did:anet:<AID>#<seq> → the key of that AID's JWKS at the hub (the jku when the card names one)."""
        if not kid or not kid.startswith("did:anet:"):
            raise ValueError("kid %r" % kid)
        aid = kid[len("did:anet:"):].split("#")[0]
        url = jku or "%s/agents/%s/jwks.json" % (self.env.hub, aid)
        if not url.startswith(self.env.hub + "/"):
            raise ValueError("jku %s is not this hub's" % url)
        keys = (await self.hx.get(url)).json()["keys"]
        for k in keys:
            if k.get("kid") == kid:
                return PyJWK(k)
        raise ValueError("no key %s in %s" % (kid, url))

    async def cards(self):
        env = self.env
        # Keys are fetched ahead: the SDK's verifier calls the key provider synchronously.
        keys = {}

        def provider(kid, jku):
            return keys[kid]

        verifier = create_signature_verifier(provider, ["EdDSA"])
        # The proxy card is signed by the requester's own key (no jku).
        req_kid = "did:anet:%s#0" % env.req["aid"]
        keys[req_kid] = await self.key_for(req_kid, None)

        res = A2ACardResolver(self.hx, env.base)
        try:
            await res.get_agent_card()
            R.fail("card-no-token", "the card was served without the bearer")
        except AgentCardResolutionError as e:
            R.check("401" in str(e), "card-no-token", "A2ACardResolver without the token: %s" % str(e)[:100])
        hk = {"headers": {"Authorization": "Bearer " + env.token}}
        card = await res.get_agent_card(http_kwargs=hk, signature_verifier=verifier)
        self.card = card
        R.check(card.name and [i.protocol_binding for i in card.supported_interfaces] == ["JSONRPC", "HTTP+JSON"],
                "card", "A2ACardResolver + Bearer: %r, interfaces %s" % (
                    card.name, [i.protocol_binding for i in card.supported_interfaces]))
        R.ok("card-signature-proxy", "create_signature_verifier(EdDSA) accepts the proxy card (kid %s)" % req_kid)
        # The alias path (§11.2): the base URL itself.
        alias = await A2ACardResolver(self.hx, env.base, agent_card_path="").get_agent_card(http_kwargs=hk)
        R.check(d(alias) == d(card), "card-alias", "GET <base> serves the same card")
        # A tampered copy must not verify.
        bad = type(card)()
        bad.CopyFrom(card)
        bad.name = card.name + " (edited)"
        try:
            verifier(bad)
            R.fail("card-signature-tamper", "an edited proxy card verified")
        except InvalidSignaturesError:
            R.ok("card-signature-tamper", "an edited proxy card does not verify")
        # The network card: at the hub, and inside the proxy card (originCard, only when VERIFIED).
        raw = (await self.hx.get("%s/a2a/v1/agents/%s/card" % (env.hub, env.agent))).content
        net = parse_agent_card(json.loads(raw))
        prot = json.loads(base64.urlsafe_b64decode(net.signatures[0].protected + "=="))
        keys[prot["kid"]] = await self.key_for(prot["kid"], prot.get("jku"))
        try:
            verifier(net)
            R.ok("card-signature-network", "the provider's network card verifies (kid %s, jku %s)" % (
                prot["kid"], prot.get("jku")))
        except Exception as e:  # noqa: BLE001
            R.fail("card-signature-network", "%s: %s" % (type(e).__name__, e))
        origin = None
        for ext in card.capabilities.extensions:
            if ext.uri == ORIGIN:
                origin = d(ext).get("params", {})
        oc = origin.get("originCard") if origin else None
        R.check(origin and origin.get("originVerification") == "VERIFIED" and oc and
                base64.urlsafe_b64decode(oc + "=" * (-len(oc) % 4)) == raw, "card-origin",
                "anet-origin: VERIFIED, originCard == the hub's bytes")
        # The SDK's own one-call path: create_from_url with the verifier.
        f = ClientFactory(ClientConfig(httpx_client=self.hx, streaming=False))
        try:
            await f.create_from_url(env.base, resolver_http_kwargs=hk, signature_verifier=verifier,
                                    interceptors=[AuthInterceptor(Creds(env.token))])
            R.ok("card-create-from-url", "ClientFactory.create_from_url with signature_verifier")
        except Exception as e:  # noqa: BLE001
            R.fail("card-create-from-url", "%s: %s" % (type(e).__name__, e))
        # The list route (§11.2) — not an A2A operation; read raw.
        lst = (await self.hx.get("http://%s/a2a/v1/agents" % env.req["a2a"], headers=hk["headers"])).json()
        aids = [a.get("aid") for a in lst.get("agents", [])]
        R.check(env.agent in aids and env.req["aid"] not in aids, "agents-list", "GET /a2a/v1/agents: %s" % aids)

    # -- 2. text tasks on one binding ------------------------------------
    async def text(self, tag, binding):
        env = self.env
        cl = self.client(binding)
        ctx = "ctx-%s-%s" % (tag, uuid.uuid4().hex[:8])
        text = "py %s blocking %s" % (tag, self.nonce)
        t0 = time.time()
        t = await self.send(cl, text_msg(text, ctx))
        ok = (t and t.status.state == S.TASK_STATE_COMPLETED and reply_text(t) == ECHO + text and
              t.context_id == ctx and meta(t, "anet.receipt_verified") == "verified" and
              meta(t, "anet.peer_aid") == env.agent)
        R.check(ok, "%s-blocking" % tag, "%s in %.1fs, anet.reply %r, contextId kept %s, receipt %s" % (
            state(t), time.time() - t0, reply_text(t) if t else None, t and t.context_id == ctx,
            meta(t, "anet.receipt_verified") if t else None))
        if t:
            await self.same_as_projection("%s-projection" % tag, t)
        blocking = t

        # returnImmediately, then GetTask until done.
        text2 = "py %s immediate %s" % (tag, self.nonce)
        t = await self.send(cl, text_msg(text2, ctx), immediate=True)
        R.check(t and t.status.state in (S.TASK_STATE_SUBMITTED, S.TASK_STATE_WORKING), "%s-immediate" % tag,
                "returnImmediately: %s" % state(t))
        if t:
            done = await self.wait_terminal(cl, t.id)
            R.check(done and done.status.state == S.TASK_STATE_COMPLETED and reply_text(done) == ECHO + text2,
                    "%s-immediate-done" % tag, "GetTask: %s, %r" % (state(done), reply_text(done) if done else None))
        immediate = t

        # SendStreamingMessage.
        scl = self.client(binding, streaming=True)
        text3 = "py %s stream %s" % (tag, self.nonce)
        evs = await self.stream(scl, text_msg(text3, ctx))
        kinds = [ev.WhichOneof("payload") for ev in evs]
        arts = [ev.artifact_update for ev in evs if ev.HasField("artifact_update")]
        last = [ev.status_update for ev in evs if ev.HasField("status_update")]
        art_text = "".join(p.text for a in arts if "anet.reply" in (a.artifact.artifact_id, a.artifact.name)
                           for p in a.artifact.parts
                           if p.WhichOneof("content") == "text")
        R.check(kinds and kinds[0] == "task" and last and last[-1].status.state == S.TASK_STATE_COMPLETED
                and art_text == ECHO + text3, "%s-stream" % tag,
                "events %s; anet.reply %r" % (compress(kinds), art_text))
        stream_tid = evs[0].task.id if evs and evs[0].HasField("task") else None

        # GetTask with historyLength.
        if blocking:
            g = await cl.get_task(GetTaskRequest(id=blocking.id, history_length=1), context=self.ctx())
            R.check(g.id == blocking.id and len(g.history) == 1 and g.status.state == S.TASK_STATE_COMPLETED,
                    "%s-get" % tag, "GetTask(historyLength=1): %s, %d history" % (state(g), len(g.history)))

        # ListTasks by contextId: the three, newest first.
        lr = await cl.list_tasks(ListTasksRequest(context_id=ctx, page_size=10), context=self.ctx())
        ids = [x.id for x in lr.tasks]
        want = [x for x in (stream_tid, immediate and immediate.id, blocking and blocking.id) if x]
        R.check(ids == want and lr.total_size == len(want), "%s-list" % tag,
                "ListTasks(contextId): %d tasks, totalSize %d, newest first %s" % (len(ids), lr.total_size, ids == want))
        lr2 = await cl.list_tasks(ListTasksRequest(context_id=ctx, page_size=1), context=self.ctx())
        R.check(len(lr2.tasks) == 1 and lr2.next_page_token, "%s-list-page" % tag,
                "pageSize 1: %d task, nextPageToken %s" % (len(lr2.tasks), bool(lr2.next_page_token)))
        if lr2.next_page_token:
            lr3 = await cl.list_tasks(ListTasksRequest(context_id=ctx, page_size=1, page_token=lr2.next_page_token),
                                      context=self.ctx())
            R.check([x.id for x in lr3.tasks] == ids[1:2], "%s-list-page2" % tag, "second page is the next task")

        # CancelTask on a task the provider holds.
        t = await self.send(cl, text_msg("py %s %s cancel me %s" % (tag, HOLD, self.nonce)), immediate=True)
        if t:
            await asyncio.sleep(1.0)
            c = await cl.cancel_task(CancelTaskRequest(id=t.id), context=self.ctx())
            R.check(c.status.state == S.TASK_STATE_CANCELED, "%s-cancel" % tag, "CancelTask: %s" % state(c))
            ok = False
            for _ in range(60):
                p = await self.ctl("/tasks/get", {"task_id": t.id}, prov=True)
                if (p.get("status") or {}).get("state") == "TASK_STATE_CANCELED":
                    ok = True
                    break
                await asyncio.sleep(0.5)
            R.check(ok, "%s-cancel-provider" % tag, "the provider's side is canceled too")
            try:
                await cl.cancel_task(CancelTaskRequest(id=t.id), context=self.ctx())
                R.note("%s-cancel-again" % tag, "canceling a canceled task answered without error")
            except Exception as e:  # noqa: BLE001 — the SDK's A2A errors are not A2AClientError
                R.check("TaskNotCancelable" in errname(e), "%s-cancel-again" % tag,
                        "second CancelTask: %s" % errname(e))
            # SubscribeToTask on a terminal task: UnsupportedOperation, as a plain error (§11.4 Q31).
            try:
                async for _ in scl.subscribe(SubscribeToTaskRequest(id=t.id), context=self.ctx()):
                    pass
                R.fail("%s-subscribe-terminal" % tag, "subscribed to a canceled task")
            except Exception as e:  # noqa: BLE001
                named = "UnsupportedOperation" in errname(e)
                R.check(named or "400" in str(e), "%s-subscribe-terminal" % tag,
                        "refused, no stream opened: %s" % errname(e))
                if not named:
                    # §11.4 Q31: a stream that cannot start is refused with the binding's error at a
                    # non-200 status; this SDK's JSON-RPC stream reader reads an error body only at 200
                    # (docs/a2a/issue-a2a-python.md P1, issue-a2a-go.md A12).
                    R.note("%s-subscribe-terminal-name" % tag, "the SDK does not read the error name "
                           "(UnsupportedOperation) from a non-200 JSON-RPC answer to a streaming call")

        # Errors a client must be able to read.
        try:
            await cl.get_task(GetTaskRequest(id="ix_" + "0" * 32), context=self.ctx())
            R.fail("%s-notfound" % tag, "an unknown task was found")
        except Exception as e:  # noqa: BLE001
            R.check("TaskNotFound" in errname(e), "%s-notfound" % tag, errname(e))
        try:
            await cl.create_task_push_notification_config(
                TaskPushNotificationConfig(task_id=blocking.id if blocking else "x", url="http://127.0.0.1:1/x"),
                context=self.ctx())
            R.fail("%s-push" % tag, "push config accepted")
        except Exception as e:  # noqa: BLE001
            R.check("PushNotificationNotSupported" in errname(e), "%s-push" % tag, errname(e))
        # GetExtendedAgentCard: the card does not declare extendedAgentCard, so the SDK answers from
        # the card it has, without a call; the server's own answer is read raw.
        n0 = len(self.resp_headers)
        c2 = await cl.get_extended_agent_card(GetExtendedAgentCardRequest(), context=self.ctx())
        R.check(d(c2) == d(self.card) and len(self.resp_headers) == n0, "%s-extended-card-sdk" % tag,
                "the SDK returns the public card itself, no request made")
        if tag == "jsonrpc":
            r = (await self.raw_rpc("GetExtendedAgentCard", {})).json()
            R.check((r.get("error") or {}).get("code") == -32004, "extended-card-server",
                    "the server refuses GetExtendedAgentCard: %s" % r.get("error"))
        # A client with the wrong token.
        bad = ClientFactory(ClientConfig(streaming=False, httpx_client=self.hx, supported_protocol_bindings=[binding])) \
            .create(self.card, interceptors=[AuthInterceptor(Creds("0" * 64))])
        try:
            await self.send(bad, text_msg("py wrong token %s" % self.nonce))
            R.fail("%s-401" % tag, "a wrong token was accepted")
        except Exception as e:  # noqa: BLE001
            R.check("401" in str(e) or "Unauthenticated" in errname(e) or "auth" in str(e).lower(),
                    "%s-401" % tag, "%s: %s" % (errname(e), str(e)[:80]))
        return ctx

    # -- 3. capability calls ------------------------------------------
    async def capability(self, tag, binding):
        cl = self.client(binding)
        free = self.env.skills["free"]
        for form, msg in (("metadata", data_msg({"text": "py free %s" % self.nonce}, md={"anet.skill": free})),
                          ("datapart", data_msg({"skill": free, "args": {"text": "py free %s" % self.nonce}}))):
            t = await self.send(cl, msg)
            res = artifact(t, "anet.result") if t else None
            body = json.dumps(d(res)) if res else ""
            R.check(t and t.status.state == S.TASK_STATE_COMPLETED and sha("py free %s" % self.nonce) in body and
                    meta(t, "anet.effect_status") == "OK", "%s-cap-%s" % (tag, form),
                    "%s → %s, anet.result carries sha256(text), effect %s" % (
                        form, state(t), meta(t, "anet.effect_status") if t else None))
            if t and form == "datapart":
                await self.same_as_projection("%s-cap-projection" % tag, t)

    # -- 4. a2a-x402 --------------------------------------------------
    async def quote(self, cl, skill, text, x402=True):
        t = await self.send(cl, data_msg({"skill": skill, "args": {"text": text}}), x402=x402)
        req = status_meta(t, "x402.payment.required") or meta(t, "x402.payment.required") or {}
        return t, req.get("accepts") or []

    async def pay_msg(self, cl, t, md, x402=True, stream=False):
        m = text_msg("payment", ctx=t.context_id, task_id=t.id, md=md)
        if stream:
            evs = await self.stream(cl, m, x402=x402)
            return evs
        return await self.send(cl, m, x402=x402)

    # -- 5. what else a real client sends --------------------------------
    async def provider_reply(self, marker, text, secs=60):
        """Answer, as the provider, the held task whose text holds marker (MCP reply_task's route)."""
        tok = self.env.prov_ctl_token
        end = time.time() + secs
        while time.time() < end:
            r = await self.hx.post("http://%s/tasks/list" % self.env.prov["ctl"], headers={"Authorization": "Bearer " + tok},
                                   json={"role": "inbound", "page_size": 50, "history_length": 1})
            for t in r.json().get("tasks", []):
                if any(marker in (p.get("text") or "") for m in t.get("history", []) for p in m.get("parts", [])):
                    r2 = await self.hx.post("http://%s/tasks/reply" % self.env.prov["ctl"],
                                            headers={"Authorization": "Bearer " + tok},
                                            json={"task_id": t["id"], "text": text, "state": "completed"})
                    return r2.status_code
            await asyncio.sleep(0.5)
        return None

    async def extras(self, tag, binding):
        cl = self.client(binding)
        scl = self.client(binding, streaming=True)
        # SubscribeToTask on a task still open, answered while subscribed.
        marker = "py %s %s subscribe %s" % (tag, HOLD, self.nonce)
        t = await self.send(cl, text_msg(marker), immediate=True)
        evs = []

        async def sub():
            async for ev in scl.subscribe(SubscribeToTaskRequest(id=t.id), context=self.ctx(timeout=120)):
                evs.append(ev)

        job = asyncio.create_task(sub())
        await asyncio.sleep(2)
        code = await self.provider_reply(marker, "answered while subscribed")
        try:
            await asyncio.wait_for(job, 60)
        except Exception as e:  # noqa: BLE001
            R.fail("%s-subscribe" % tag, "subscription: %s" % errname(e))
        else:
            ups = [ev.status_update.status.state for ev in evs if ev.HasField("status_update")]
            R.check(code == 200 and evs and evs[0].HasField("task") and ups and ups[-1] == S.TASK_STATE_COMPLETED,
                    "%s-subscribe" % tag, "SubscribeToTask: events %s, last %s" % (
                        compress([ev.WhichOneof("payload") for ev in evs]), TaskState.Name(ups[-1]) if ups else None))
        # Text that is not plain ASCII, and a large one through a stream (the echo comes back whole).
        uni = "py %s 多语言 ✓ émoji 🧪 עברית \u202e rtl \u0000? %s" % (tag, self.nonce)
        uni = uni.replace("\u0000?", "")
        t = await self.send(cl, text_msg(uni))
        R.check(t and reply_text(t) == ECHO + uni, "%s-unicode" % tag, "echo of non-ASCII text is exact: %s" % (
            reply_text(t) == ECHO + uni if t else None))
        big = "py %s big %s " % (tag, self.nonce) + ("0123456789abcdef" * 65536 * 3)  # ~3 MiB
        evs = await self.stream(scl, text_msg(big), timeout=300)
        art = "".join(p.text for ev in evs if ev.HasField("artifact_update") for p in ev.artifact_update.artifact.parts
                      if p.WhichOneof("content") == "text")
        last = [ev.status_update.status.state for ev in evs if ev.HasField("status_update")]
        R.check(art == ECHO + big and last and last[-1] == S.TASK_STATE_COMPLETED, "%s-big-stream" % tag,
                "a %.1f MiB echo through SendStreamingMessage: %s, %d bytes back" % (len(big) / 2**20,
                                                                                    TaskState.Name(last[-1]) if last else None, len(art)))
        # A file as a raw part: accepted as an attachment; a url part: InvalidParams (§11.5 C44).
        blob = bytes(range(256)) * 64
        m = Message(role=Role.ROLE_USER, message_id=uuid.uuid4().hex,
                    parts=[Part(text="py %s file %s" % (tag, self.nonce)),
                           Part(raw=blob, filename="blob.bin", media_type="application/octet-stream")])
        t = await self.send(cl, m)
        hist = d(t).get("history", [{}])[0].get("parts", []) if t else []
        R.check(t and t.status.state == S.TASK_STATE_COMPLETED and any(p.get("filename") == "blob.bin" for p in hist),
                "%s-raw-part" % tag, "%s; the file is in the task history: %s" % (
                    state(t), [p.get("filename") or ("text" if "text" in p else "?") for p in hist]))
        try:
            await self.send(cl, Message(role=Role.ROLE_USER, message_id=uuid.uuid4().hex,
                                        parts=[Part(url="https://example.com/x.pdf", filename="x.pdf")]))
            R.fail("%s-url-part" % tag, "a url part was accepted")
        except Exception as e:  # noqa: BLE001
            R.check("InvalidParams" in errname(e) or "url" in str(e), "%s-url-part" % tag, errname(e))
        # ListTasks filters as the SDK sends them.
        now = time.time()
        lr = await cl.list_tasks(ListTasksRequest(status=S.TASK_STATE_COMPLETED, page_size=5, history_length=0),
                                 context=self.ctx())
        R.check(lr.tasks and all(x.status.state == S.TASK_STATE_COMPLETED and not x.history for x in lr.tasks),
                "%s-list-status" % tag, "status=COMPLETED, historyLength=0: %d task(s), states %s, histories %s" % (
                    len(lr.tasks), sorted({TaskState.Name(x.status.state) for x in lr.tasks}),
                    [len(x.history) for x in lr.tasks]))
        req = ListTasksRequest(page_size=100, include_artifacts=True)
        req.status_timestamp_after.FromSeconds(int(now) - 30)
        lr = await cl.list_tasks(req, context=self.ctx())
        R.check(lr.tasks and all(x.status.timestamp.ToSeconds() >= int(now) - 31 for x in lr.tasks) and
                any(x.artifacts for x in lr.tasks), "%s-list-after" % tag,
                "statusTimestampAfter (30 s ago) + includeArtifacts: %d task(s), with artifacts %d" % (
                    len(lr.tasks), sum(1 for x in lr.tasks if x.artifacts)))
        lr = await cl.list_tasks(ListTasksRequest(page_size=5), context=self.ctx())
        R.check(lr.tasks and not any(x.artifacts for x in lr.tasks), "%s-list-noartifacts" % tag,
                "includeArtifacts unset: no artifacts in the list (%d tasks)" % len(lr.tasks))

    async def pay(self, tag, binding):
        env = self.env
        paid, pricey = env.skills["paid"], env.skills["pricey"]
        cl = self.client(binding)
        self.resp_headers.clear()
        text = "py %s paid %s" % (tag, self.nonce)
        t, opts = await self.quote(cl, paid, text)
        echoed = [h.get("a2a-extensions") for h in self.resp_headers]
        R.check(t and t.status.state == S.TASK_STATE_INPUT_REQUIRED and meta(t, "x402.payment.status") ==
                "payment-required" and opts, "%s-quote" % tag, "%s: %s, %s, %d option(s)" % (
                    paid, state(t), meta(t, "x402.payment.status") if t else None, len(opts)))
        R.check(X402 in (echoed[-1] or "") if echoed else False, "%s-ext-echo" % tag,
                "A2A-Extensions echoed: %r" % (echoed[-1] if echoed else None))
        if not opts:
            return
        # anet.payment.accept: the option as the SDK read it, sent back through the SDK (§8.7).
        res = await self.pay_msg(cl, t, {"x402.payment.status": "payment-submitted", "anet.payment.accept": opts[0]})
        if res.status.state == S.TASK_STATE_COMPLETED:
            ok = (meta(res, "x402.payment.status") == "payment-completed" and
                  sha(text) in json.dumps(d(res).get("artifacts", [])) and
                  len([r for r in (meta(res, "x402.payment.receipts") or []) if r.get("success")]) == 1)
            R.check(ok, "%s-pay-accept" % tag, "payment-submitted + anet.payment.accept → %s, %s" % (
                state(res), meta(res, "x402.payment.status")))
            await self.same_as_projection("%s-pay-projection" % tag, res)
        else:
            R.fail("%s-pay-accept" % tag, "payment-submitted + anet.payment.accept (the option as the SDK "
                   "carries it) → %s, %s, anet.reason=%s" % (state(res), status_meta(res, "x402.payment.status"),
                                                           status_meta(res, "anet.reason")))
            # One option: the accept may be left out (§8.7) — the flow still completes.
            res = await self.pay_msg(cl, t, {"x402.payment.status": "payment-submitted"})
            R.check(res.status.state == S.TASK_STATE_COMPLETED and meta(res, "x402.payment.status") ==
                    "payment-completed", "%s-pay-noaccept" % tag, "payment-submitted without accept → %s, %s" % (
                        state(res), meta(res, "x402.payment.status")))

        # Above the agent tier: needs_operator_approval; then declined.
        t2, opts2 = await self.quote(cl, pricey, "py %s pricey %s" % (tag, self.nonce))
        if t2 is None or not opts2:
            R.fail("%s-over-quote" % tag, "no quote for %s" % pricey)
            return
        res = await self.pay_msg(cl, t2, {"x402.payment.status": "payment-submitted"})
        R.check(res.status.state == S.TASK_STATE_INPUT_REQUIRED and meta(res, "anet.reason") ==
                "needs_operator_approval", "%s-over-limit" % tag, "%s, anet.reason=%s, %s" % (
                    state(res), meta(res, "anet.reason"), meta(res, "x402.payment.status")))
        res = await self.pay_msg(cl, t2, {"x402.payment.status": "payment-rejected"})
        R.check(res.status.state == S.TASK_STATE_CANCELED, "%s-reject" % tag, "payment-rejected → %s" % state(res))

        # Streaming the payment message (SendStreamingMessage) on a fresh quote.
        scl = self.client(binding, streaming=True)
        t3, opts3 = await self.quote(cl, paid, "py %s paid-stream %s" % (tag, self.nonce))
        if t3 is not None and opts3:
            evs = await self.pay_msg(scl, t3, {"x402.payment.status": "payment-submitted"}, stream=True)
            ups = [ev.status_update for ev in evs if ev.HasField("status_update")]
            R.check(ups and ups[-1].status.state == S.TASK_STATE_COMPLETED, "%s-pay-stream" % tag,
                    "streamed payment: events %s, last %s" % (
                        compress([ev.WhichOneof("payload") for ev in evs]),
                        TaskState.Name(ups[-1].status.state) if ups else None))

        # Not activated: above the auto tier the node says so (§8.7).
        t4, _ = await self.quote(cl, paid, "py %s paid-noext %s" % (tag, self.nonce), x402=False)
        R.check(t4 and t4.status.state == S.TASK_STATE_INPUT_REQUIRED and meta(t4, "anet.reason") ==
                "payment_extension_not_activated", "%s-not-activated" % tag, "%s, anet.reason=%s" % (
                    state(t4), meta(t4, "anet.reason") if t4 else None))
        if t4:
            await cl.cancel_task(CancelTaskRequest(id=t4.id), context=self.ctx())


def errname(e):
    return type(e).__name__ + (": " + str(e)[:120] if str(e) else "")


def compress(xs):
    out = []
    for x in xs:
        if out and out[-1][0] == x:
            out[-1][1] += 1
        else:
            out.append([x, 1])
    return ",".join("%s×%d" % (k, n) if n > 1 else k for k, n in out)


def canon(v):
    """JSON as proto3 JSON would carry it: numbers as floats, empty members dropped, timestamps as instants
    (RFC 3339 allows "…36.44Z" and "…36.440Z" alike)."""
    if isinstance(v, str) and TS.fullmatch(v):
        return datetime.datetime.fromisoformat(v.replace("Z", "+00:00")).isoformat()
    if isinstance(v, dict):
        out = {k: canon(x) for k, x in v.items()}
        return {k: x for k, x in out.items() if x not in (None, "", [], {}, False)}
    if isinstance(v, list):
        return [canon(x) for x in v]
    if isinstance(v, bool):
        return v
    if isinstance(v, (int, float)):
        return float(v)
    return v


def diff(a, b, path=""):
    if type(a) is not type(b):
        return ["%s: %r vs %r" % (path or "/", short(a), short(b))]
    if isinstance(a, dict):
        out = []
        for k in sorted(set(a) | set(b)):
            if k not in a or k not in b:
                out.append("%s/%s only in %s" % (path, k, "anet" if k in a else "sdk"))
            else:
                out += diff(a[k], b[k], path + "/" + k)
        return out
    if isinstance(a, list):
        if len(a) != len(b):
            return ["%s: %d vs %d items" % (path, len(a), len(b))]
        out = []
        for i, (x, y) in enumerate(zip(a, b)):
            out += diff(x, y, "%s[%d]" % (path, i))
        return out
    return [] if a == b else ["%s: %r vs %r" % (path, short(a), short(b))]


def short(v):
    s = repr(v)
    return s if len(s) < 60 else s[:57] + "..."


async def main():
    if len(sys.argv) < 2:
        print(__doc__)
        return 2
    env = Env(sys.argv[1])
    only = set(sys.argv[3].split(",")) if len(sys.argv) > 3 and sys.argv[2] == "--only" else None

    async def hook(resp):
        p.resp_headers.append({k.lower(): v for k, v in resp.headers.items()})

    async with httpx.AsyncClient(timeout=180, event_hooks={"response": [hook]}, trust_env=False) as hx:
        p = Probe(env, hx)
        import importlib.metadata as md
        print("NOTE sdk: a2a-sdk %s, httpx %s, protobuf %s" % (md.version("a2a-sdk"), md.version("httpx"),
                                                               md.version("protobuf")))
        steps = [("cards", p.cards)]
        for tag, b in (("jsonrpc", TransportProtocol.JSONRPC), ("rest", TransportProtocol.HTTP_JSON)):
            steps += [(tag + "-text", lambda tag=tag, b=b: p.text(tag, b)),
                      (tag + "-cap", lambda tag=tag, b=b: p.capability(tag, b)),
                      (tag + "-pay", lambda tag=tag, b=b: p.pay(tag, b)),
                      (tag + "-extras", lambda tag=tag, b=b: p.extras(tag, b))]
        for name, fn in steps:
            if name != "cards" and p.card is None:
                break
            if only and name not in only and name != "cards":
                continue
            try:
                await fn()
            except Exception as e:  # noqa: BLE001
                R.fail(name, "raised %s" % errname(e))
    return 1 if R.fails else 0


if __name__ == "__main__":
    sys.exit(asyncio.run(main()))
