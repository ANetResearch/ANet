#!/usr/bin/env python3
"""一个最小的 MCP stdio 客户端,按 Claude Code / Cursor 的方式驱动 `anet mcp`。

存在的理由:mcpserv 的单元测试对着一个 fake Control 跑,验的是工具表的形状。
它验不了真正会出问题的地方 —— 进程在 stdio 上说的到底是不是合法 MCP:一行
多余的 stdout 输出就是一个 framing 错误,而那种错误只在真客户端连上来时才出现。

工具名与参数名不写在这里,取自同目录 mcpcall.py 的 TOOLS_V1/TOOLS_V2/OPS/ARGS:
A2A-DESIGN §12 把工具按 A2A 概念改了名,探针按 tools/list 判断服务端是哪一代,
两代都能跑。
"""
import json, os, subprocess, sys, time

from mcpcall import OPS, build_args, generation, missing, payload

anet, home, hub_url = sys.argv[1], sys.argv[2], sys.argv[3]
env = dict(os.environ, HOME=home)
p = subprocess.Popen([anet, "mcp"], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                     stderr=subprocess.PIPE, env=env, text=True, bufsize=1)

_id = 0
def call(method, params=None, notify=False):
    global _id
    msg = {"jsonrpc": "2.0", "method": method}
    if params is not None:
        msg["params"] = params
    if not notify:
        _id += 1
        msg["id"] = _id
    p.stdin.write(json.dumps(msg) + "\n")
    p.stdin.flush()
    if notify:
        return None
    deadline = time.time() + 30
    while time.time() < deadline:
        line = p.stdout.readline()
        if not line:
            raise SystemExit("FAIL server closed stdout: " + p.stderr.read()[:400])
        line = line.strip()
        if not line:
            continue
        try:
            got = json.loads(line)
        except json.JSONDecodeError:
            # 这正是要抓的那类问题:stdio 上出现了不是 JSON-RPC 的东西。
            raise SystemExit("FAIL non-JSON on stdout: %r" % line[:200])
        if got.get("id") == _id:
            return got
    raise SystemExit("FAIL timed out waiting for " + method)

out = {}
try:
    init = call("initialize", {
        "protocolVersion": "2024-11-05",
        "capabilities": {},
        "clientInfo": {"name": "fleet-probe", "version": "0"},
    })
    out["server_name"] = (init.get("result", {}).get("serverInfo") or {}).get("name", "")
    call("notifications/initialized", {}, notify=True)

    tools = call("tools/list", {})
    schemas = {t["name"]: t.get("inputSchema") or {} for t in tools.get("result", {}).get("tools", [])}
    names = sorted(schemas)
    gen = generation(names)
    out["tools"] = names
    out["generation"] = gen
    out["missing"] = missing(names, gen)

    def tool_call(op, logical):
        name = OPS[op].get(gen, "")
        return name, call("tools/call", {"name": name, "arguments": build_args(op, logical, schemas.get(name))})

    # 真调用一次:按能力找 worker。这一步会穿到 daemon 的控制面再到 hub。
    _, r = tool_call("find", {"capability": "code.write"})
    body = payload(r.get("result"))
    agents = body if isinstance(body, list) else (body or {}).get("agents") or []
    out["found"] = len(agents)

    _, st = tool_call("status", {})
    out["status_hub"] = (payload(st.get("result")) or {}).get("hub_url", "")

    # 错误也要如实回到客户端,而不是变成一次成功的空回答。
    _, bad = tool_call("send", {"agent": "not-an-aid", "text": "x"})
    out["bad_is_error"] = bool(bad.get("result", {}).get("isError") or bad.get("error"))
finally:
    try:
        p.stdin.close(); p.wait(timeout=5)
    except Exception:
        p.kill()

print(json.dumps(out))
