#!/usr/bin/env python3
"""py_backend.py — a minimal A2A server built with the official Python SDK (a2a-sdk), to stand behind an
anet provider as its §11.6 backend (docs/notes/0035 item 3).

It "summarizes" what it is sent: the reply is "summary: <first words> (<n> words)" and names the
contextId it saw, so a test can tell which conversation a message landed in. A message containing
"[ask]" is answered with input-required; "[sleep N]" delays the answer N seconds. Every request is appended to --log as one JSON line
(contextId, taskId, text, message metadata, whether the bearer matched), which is what the test reads
for Q23 (contextId per peer) and for "nothing from an untrusted peer arrived".

    python py_backend.py --tcp 127.0.0.1:PORT | --uds PATH  --token-file F --log F

Built only from the SDK's documented server pieces: AgentExecutor, TaskUpdater, DefaultRequestHandler,
InMemoryTaskStore, create_agent_card_routes, create_jsonrpc_routes, create_rest_routes, on Starlette
and uvicorn. The bearer check is a Starlette middleware (the SDK leaves authentication to the app).
"""
import argparse
import asyncio
import hmac
import re
import json
import time

import uvicorn
from google.protobuf import json_format
from starlette.applications import Starlette
from starlette.middleware.base import BaseHTTPMiddleware
from starlette.responses import JSONResponse

from a2a.helpers.proto_helpers import get_message_text, new_task, new_text_message
from a2a.server.agent_execution import AgentExecutor, RequestContext
from a2a.server.events import EventQueue
from a2a.server.request_handlers import DefaultRequestHandler
from a2a.server.routes import create_agent_card_routes, create_jsonrpc_routes, create_rest_routes
from a2a.server.tasks import InMemoryTaskStore, TaskUpdater
from a2a.types import (AgentCapabilities, AgentCard, AgentInterface, AgentSkill, HTTPAuthSecurityScheme, Part,
                       SecurityRequirement, SecurityScheme, StringList, TaskState)


class Summarizer(AgentExecutor):
    def __init__(self, log_path):
        self.log_path = log_path

    def record(self, **kw):
        with open(self.log_path, "a") as f:
            f.write(json.dumps(kw) + "\n")

    async def execute(self, context: RequestContext, event_queue: EventQueue) -> None:
        msg = context.message
        text = get_message_text(msg) if msg else ""
        self.record(t=time.time(), contextId=context.context_id, taskId=context.task_id, text=text,
                    metadata=json_format.MessageToDict(msg.metadata) if msg and msg.metadata else {},
                    requestMetadata=context.metadata)
        up = TaskUpdater(event_queue, context.task_id, context.context_id)
        if context.current_task is None:
            # a2a-sdk 1.x: the Task itself comes first, then its status updates.
            await event_queue.enqueue_event(new_task(context.task_id, context.context_id, TaskState.TASK_STATE_SUBMITTED,
                                                     history=[msg] if msg else None))
        await up.start_work()
        m = re.search(r"\[sleep (\d+)\]", text)
        if m:
            await asyncio.sleep(min(int(m.group(1)), 600))
        if "[ask]" in text:
            await up.requires_input(up.new_agent_message([Part(text="which part should I summarize?")]))
            return
        words = text.split()
        reply = "summary: %s (%d words; context %s)" % (" ".join(words[:12]), len(words), context.context_id)
        await up.add_artifact([Part(text=reply)], name="summary")
        await up.complete(new_text_message(reply, context_id=context.context_id, task_id=context.task_id))

    async def cancel(self, context: RequestContext, event_queue: EventQueue) -> None:
        up = TaskUpdater(event_queue, context.task_id, context.context_id)
        await up.cancel()


class Bearer(BaseHTTPMiddleware):
    def __init__(self, app, token, log):
        super().__init__(app)
        self.token, self.log = token, log

    async def dispatch(self, request, call_next):
        got = request.headers.get("authorization", "")
        if self.token and not hmac.compare_digest(got.encode(), ("Bearer " + self.token).encode()):
            self.log.record(t=time.time(), refused=request.url.path, auth=bool(got))
            return JSONResponse({"error": "unauthorized"}, status_code=401)
        return await call_next(request)


def main():
    ap = argparse.ArgumentParser()
    g = ap.add_mutually_exclusive_group(required=True)
    g.add_argument("--tcp", help="host:port on loopback")
    g.add_argument("--uds", help="unix socket path")
    ap.add_argument("--token-file", default="")
    ap.add_argument("--log", required=True)
    a = ap.parse_args()
    token = open(a.token_file).read().strip() if a.token_file else ""
    # Over a socket the host in the card is only a label: anet dials the configured socket (§11.6).
    base = "http://%s" % (a.tcp if a.tcp else "localhost")
    card = AgentCard(
        name="py-summarizer", description="Summarizes text it is sent (a2a-sdk sample backend for anet).",
        version="0.0.1", default_input_modes=["text/plain"], default_output_modes=["text/plain"],
        capabilities=AgentCapabilities(streaming=True),
        supported_interfaces=[AgentInterface(url=base + "/", protocol_binding="JSONRPC", protocol_version="1.0"),
                              AgentInterface(url=base + "/rest", protocol_binding="HTTP+JSON", protocol_version="1.0")],
        skills=[AgentSkill(id="summarize", name="Summarize", description="A short summary of the text.",
                           tags=["text"])],
    )
    if token:
        card.security_schemes["bearer"].CopyFrom(
            SecurityScheme(http_auth_security_scheme=HTTPAuthSecurityScheme(scheme="Bearer")))
        req = SecurityRequirement()
        req.schemes["bearer"].CopyFrom(StringList(list=["a2a"]))
        card.security_requirements.append(req)
    ex = Summarizer(a.log)
    handler = DefaultRequestHandler(agent_executor=ex, task_store=InMemoryTaskStore(), agent_card=card)
    routes = create_agent_card_routes(card) + create_jsonrpc_routes(handler, "/") + create_rest_routes(handler, path_prefix="/rest")
    app = Starlette(routes=routes)
    app.add_middleware(Bearer, token=token, log=ex)
    if a.tcp:
        host, port = a.tcp.rsplit(":", 1)
        uvicorn.run(app, host=host, port=int(port), log_level="warning")
    else:
        uvicorn.run(app, uds=a.uds, log_level="warning")


if __name__ == "__main__":
    main()
