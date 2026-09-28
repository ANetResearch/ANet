#!/usr/bin/env python3
"""hermes_check.py — Hermes' real A2A client code (hermes-agent plugins/platforms/a2a/tools.py) against a
requester's local A2A interface, as `anet agents wire hermes --a2a <AID>` configures it (docs/notes/0035
item 4; notes/0018).

No model runs: the plugin's tool functions are called directly (a2a_call, a2a_discover, a2a_list) with
the a2a_agents entries `anet agents wire` wrote into a private HERMES_HOME. Hermes' own modules are
imported from the checkout (PYTHONPATH), unmodified; only its import dependencies are installed.

    HERMES_SRC=<hermes-agent checkout> python hermes_check.py ENV_JSON ANET_BIN WORKDIR

The provider must answer through py_backend.py (up.sh backend …): "[ask]" gives input-required and
"[sleep N]" a slow answer, which the timeout case needs.
"""
import importlib
import json
import os
import re
import subprocess
import sys
import time
import urllib.request

import py_client as pc

R = pc.R


def ctl(node, path, body):
    tok = open(node["ctl_token_file"]).read().strip()
    req = urllib.request.Request("http://%s%s" % (node["ctl"], path), data=json.dumps(body).encode(),
                                 headers={"Authorization": "Bearer " + tok, "Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=60) as r:
        return json.loads(r.read())


def a2a_list_tasks(node, agent, ctx):
    """ListTasks by contextId on the local A2A interface (what 0018 §5.2 says to recover with)."""
    tok = open(node["a2a_token_file"]).read().strip()
    body = {"jsonrpc": "2.0", "id": 1, "method": "ListTasks", "params": {"contextId": ctx, "historyLength": 0}}
    req = urllib.request.Request("http://%s/a2a/v1/agents/%s/jsonrpc" % (node["a2a"], agent), data=json.dumps(body).encode(),
                                 headers={"Authorization": "Bearer " + tok, "Content-Type": "application/json",
                                          "A2A-Version": "1.0"})
    with urllib.request.urlopen(req, timeout=60) as r:
        return json.loads(r.read())["result"].get("tasks", [])


def main():
    env = json.load(open(sys.argv[1]))
    anet, work = sys.argv[2], sys.argv[3]
    agent = env["prov"]["aid"]
    home = os.path.join(work, "hermes-home")
    hh = os.path.join(home, ".hermes")
    os.makedirs(hh, mode=0o700, exist_ok=True)
    nonce = "hm" + os.urandom(4).hex()

    # 1. anet writes the a2a_agents entry, as an operator would.
    wenv = {"HOME": home, "HERMES_HOME": hh, "ANET_DATA_DIR": env["req"]["data_dir"], "PATH": "/usr/bin:/bin"}
    w = subprocess.run([anet, "agents", "wire", "hermes", "--a2a", agent], env=wenv, capture_output=True, text=True)
    R.check(w.returncode == 0, "wire", "anet agents wire hermes --a2a <provider>: exit %d %s" % (
        w.returncode, (w.stderr or w.stdout).strip().splitlines()[-1:] if w.returncode else ""))
    cfgp = os.path.join(hh, "config.yaml")
    cfg = open(cfgp).read()
    want_url = "http://%s/a2a/v1/agents/%s" % (env["req"]["a2a"], agent)
    R.check(want_url in cfg and "type: bearer" in cfg and "timeout: 3600" in cfg and
            oct(os.stat(cfgp).st_mode & 0o777) == "0o600", "wire-entry",
            "a2a_agents.%s…: url, bearer, timeout 3600, file mode %s" % (agent[:12], oct(os.stat(cfgp).st_mode & 0o777)))
    # Two more entries by hand, outside anet's block: a short timeout, and a wrong token.
    tok = open(env["req"]["a2a_token_file"]).read().strip()
    # a2a_agents is the file's last mapping (anet's block); the test entries follow it, outside the block.
    with open(cfgp, "a") as f:
        f.write("  # test entries (hermes_check.py), not anet's\n")
        f.write("  slow:\n    url: \"%s\"\n    auth: {type: bearer, token: \"%s\"}\n    timeout: 4\n" % (want_url, tok))
        f.write("  badtoken:\n    url: \"%s\"\n    auth: {type: bearer, token: \"%s\"}\n    timeout: 60\n" % (want_url, "0" * 64))

    # 2. Hermes' plugin, as its gateway imports it.
    os.environ["HERMES_HOME"] = hh
    os.environ["HOME"] = home
    sys.path.insert(0, os.environ["HERMES_SRC"])
    tools = importlib.import_module("plugins.platforms.a2a.tools")
    peers = tools._configured_peers()
    R.check(tools._a2a_tools_available() and agent in peers and "slow" in peers and "badtoken" in peers,
            "hermes-config", "Hermes reads a2a_agents: %s" % sorted(k[:12] for k in peers))
    lst = tools.a2a_list({})
    R.check(agent in lst and "auth: bearer" in lst, "a2a_list", lst.splitlines()[1] if len(lst.splitlines()) > 1 else lst)

    # 3. a2a_call: one question, one answer (blocking SendMessage), the reply text extracted.
    t0 = time.time()
    out = tools.a2a_call({"agent": agent, "message": "summarize: the quick brown fox jumps %s" % nonce})
    head, _, body = out.partition("\n")
    m = re.match(r"\[(\S+) · context (ctx-[0-9a-f]{16}) · (\S+)\]", head)
    ctx = m.group(2) if m else None
    R.check(m and m.group(3) == "completed" and body.startswith("summary: summarize: the quick brown fox"),
            "a2a_call", "%.1fs: %r / %r" % (time.time() - t0, head[:90], body[:80]))
    if ctx:
        ts = a2a_list_tasks(env["req"], agent, ctx)
        R.check(len(ts) == 1 and ts[0]["contextId"] == ctx, "a2a_call-context",
                "Hermes' own contextId %s is the task's (C22): %d task(s)" % (ctx, len(ts)))
        out2 = tools.a2a_call({"agent": agent, "message": "and once more %s" % nonce, "context_id": ctx})
        ts = a2a_list_tasks(env["req"], agent, ctx)
        R.check(out2.startswith("[%s · context %s · completed]" % (agent, ctx)) and len(ts) == 2, "a2a_call-continue",
                "continuing with context_id: %r, %d tasks in the context (a completed task is not continued)" % (
                    out2.splitlines()[0][-40:], len(ts)))

    # 4. input-required, then Hermes continues with context_id only (Q22: the waiting task takes it).
    out = tools.a2a_call({"agent": agent, "message": "[ask] summarize something %s" % nonce})
    head, _, body = out.partition("\n")
    m = re.match(r"\[(\S+) · context (ctx-[0-9a-f]{16}) · (\S+)\]", head)
    R.check(m and m.group(3) == "input-required" and "which part should I summarize?" in body and
            "needs more input" in body, "input-required", "%r / %r" % (head[-40:], body.replace("\n", " ")[:120]))
    if m:
        ctx = m.group(2)
        out = tools.a2a_call({"agent": agent, "message": "the first paragraph %s" % nonce, "context_id": ctx})
        ts = a2a_list_tasks(env["req"], agent, ctx)
        R.check(out.splitlines()[0].endswith("· completed]") and len(ts) == 1 and
                ts[0]["status"]["state"] == "TASK_STATE_COMPLETED", "input-required-continue",
                "the answer with context_id only lands on the waiting task (0017 Q22): %r, %d task(s)" % (
                    out.splitlines()[0][-30:], len(ts)))

    # 5. timeout: Hermes' socket timeout (entry timeout 4 s) on an 8 s answer.
    before = set(os.listdir(os.path.join(hh, "a2a_conversations"))) if os.path.isdir(os.path.join(hh, "a2a_conversations")) else set()
    t0 = time.time()
    out = tools.a2a_call({"agent": "slow", "message": "[sleep 8] slow one %s" % nonce})
    took = time.time() - t0
    R.check(out.startswith("Error: call to 'slow' failed") and "timed out" in out and "ctx-" not in out, "timeout",
            "after %.1fs: %r (no contextId in the error, 0018 §5.2)" % (took, out[:90]))
    new = sorted(set(os.listdir(os.path.join(hh, "a2a_conversations"))) - before)
    ctx = new[0].rsplit(".", 1)[0] if new else None
    if ctx:
        ok, ts = False, []
        for _ in range(60):
            ts = a2a_list_tasks(env["req"], agent, ctx)
            if ts and ts[0]["status"]["state"] == "TASK_STATE_COMPLETED":
                ok = True
                break
            time.sleep(0.5)
        R.check(ok and len(ts) == 1, "timeout-task-runs-on",
                "the task goes on after the client gave up and is found by the contextId Hermes persisted (%s): %s" % (
                    ctx, ts[0]["status"]["state"] if ts else None))
    else:
        R.fail("timeout-task-runs-on", "Hermes persisted no conversation for the timed-out call")

    # 6. 401: a wrong token; a2a_discover (never sends a token); a direct URL (no token either).
    out = tools.a2a_call({"agent": "badtoken", "message": "wrong token %s" % nonce})
    R.check(out == "Error: peer 'badtoken' rejected auth (HTTP 401). Check the configured token.", "401-call", out)
    out = tools.a2a_discover({"url": want_url})
    R.check(out == "Error: discovery failed — HTTP 401 from %s." % want_url, "401-discover", out)
    out = tools.a2a_call({"agent": want_url, "message": "no token %s" % nonce})
    R.check("rejected auth (HTTP 401)" in out, "401-direct-url", out[:100])

    # 7. what Hermes keeps in plain text (0018 §5.9), and that the token is not in it.
    audit = os.path.join(hh, "a2a_audit.jsonl")
    a = open(audit).read() if os.path.exists(audit) else ""
    R.check(nonce in a and tok not in a, "hermes-local-records", "a2a_audit.jsonl holds the texts (%d lines) and not the token" % a.count("\n"))
    return 1 if R.fails else 0


if __name__ == "__main__":
    sys.exit(main())
