#!/usr/bin/env python3
"""a2a-tck-responder.py — the provider's agent for an a2a-tck run, answering by messageId prefix.

a2a-tck's scenarios/*.feature tell the system under test what to do from the client's messageId
prefix (tck-complete-task, tck-input-required, tck-artifact-text, ...). Through anet the system under
test is the remote provider, and the client's messageId reaches it in the metadata of the user message
in the task's history (a2a.messageId; the message's own id is the envelope's, redteam F33), so the
provider's agent can follow the scenarios over its own control plane
(/tasks/list, /tasks/reply — what MCP reply_task uses), the way tools/a2aprobe's responder answers
"echo: <text>". docs/notes/0019 §3.3, 0023 §3.

Test fixture only: it answers every inbound text task of the node it is pointed at.

    python3 scripts/a2a-tck-responder.py <provider control addr> <provider control_token.txt>

What anet cannot do as a scenario asks is left to fail visibly (and is logged here): a reply with a url
part is refused by the provider daemon (it fetches nothing, A2A-DESIGN §11.5); a reply's data part is
not kept as a data artifact (the artifacts of a text task are the kernel's: anet.reply, receipt,
attachments).
"""
import base64
import json
import sys
import time
import urllib.error
import urllib.request

if len(sys.argv) != 3:
    sys.exit(__doc__)
CTL, TOKF = sys.argv[1], sys.argv[2]
with open(TOKF) as f:
    TOKEN = f.read().strip()


def call(path, body):
    req = urllib.request.Request("http://%s%s" % (CTL, path), data=json.dumps(body).encode(),
                                 headers={"Authorization": "Bearer " + TOKEN, "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            return r.status, json.loads(r.read() or b"{}")
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode(errors="replace")
    except OSError as e:
        return 0, str(e)


def text(t):
    return {"parts": [{"text": t}]}


FILE = {"parts": [{"raw": base64.b64encode(b"file content").decode(), "filename": "output.txt", "mediaType": "text/plain"}]}

# (prefix, [(state, message or None), ...]); the first prefix that matches wins, so longer ones first.
PLAN = [
    ("tck-artifact-file-url", [("completed", {"parts": [{"url": "https://example.com/output.txt",
                                                         "filename": "output.txt", "mediaType": "text/plain"}]})]),
    ("tck-artifact-file", [("completed", FILE)]),
    ("tck-artifact-text", [("completed", text("Generated text content"))]),
    ("tck-artifact-data", [("completed", {"parts": [{"data": {"key": "value", "count": 42}}]})]),
    ("tck-input-required", [("input-required", text("Input required"))]),
    ("tck-reject-task", [("rejected", text("rejected"))]),
    ("tck-complete-task", [("completed", text("Hello from TCK"))]),
    ("tck-stream-artifact-chunked", [("working", None), ("completed", text("chunk-1 chunk-2"))]),
    ("tck-stream-artifact-text", [("working", None), ("completed", text("Streamed text content"))]),
    ("tck-stream-artifact-file", [("working", None), ("completed", FILE)]),
    ("tck-stream-ordering-001", [("working", None), ("completed", text("Ordered output"))]),
    ("tck-stream-001", [("working", None), ("completed", text("Stream hello from TCK"))]),
    ("tck-stream-002", [("completed", None)]),
    ("tck-stream-003", [("working", None), ("completed", text("Stream task lifecycle"))]),
]
TERMINAL = {"TASK_STATE_COMPLETED", "TASK_STATE_FAILED", "TASK_STATE_CANCELED", "TASK_STATE_REJECTED"}

# Per task, the user turn last dealt with: a2a-tck reuses one messageId for every follow-up of a task
# (tck-input-required-<session>), so the id alone does not tell a new turn from an answered one.
answered = {}
print("a2a-tck-responder: answering the inbound text tasks at %s by messageId prefix" % CTL, flush=True)
while True:
    code, page = call("/tasks/list", {"role": "inbound", "page_size": 100, "history_length": 50})
    if code != 200:
        time.sleep(0.5)
        continue
    for t in page.get("tasks", []):
        if t.get("status", {}).get("state") in TERMINAL or "anet.skill" in (t.get("metadata") or {}):
            continue
        hist = t.get("history") or []
        if not hist or hist[-1].get("role") != "ROLE_USER":
            continue
        # The client's messageId is in the message's metadata: a provider's view of a message the requester
        # wrote carries the envelope's id as messageId, and a2a.messageId only as the requester's metadata
        # (internal/a2ashape project.go msgID, redteam F33). Before that fix it was the messageId itself.
        mid = (hist[-1].get("metadata") or {}).get("a2a.messageId") or hist[-1].get("messageId") or ""
        turn = (mid, sum(1 for m in hist if m.get("role") == "ROLE_USER"))
        if answered.get(t["id"]) == turn:
            continue
        answered[t["id"]] = turn
        plan = next((p for pre, p in PLAN if mid.startswith(pre)), None)
        if plan is None:
            said = "\n".join(p["text"] for p in hist[-1].get("parts", []) if "text" in p)
            plan = [("completed", text("echo: " + said))]
        for state, msg in plan:
            body = {"task_id": t["id"], "state": state}
            if msg is not None:
                body["message"] = msg
            c, out = call("/tasks/reply", body)
            print(time.strftime("%H:%M:%S"), t["id"], mid, state, c, "" if c == 200 else str(out)[:200], flush=True)
            if c != 200:
                break
    time.sleep(0.2)
