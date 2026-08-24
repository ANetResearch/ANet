#!/usr/bin/env python3
"""mcpcall.py — drive `anet mcp` over stdio and print one tool's result.

The MCP surface is what an agent actually reaches ANet through, and it
had only ever been exercised against a fake control plane. This speaks
the wire protocol directly so a shell test can check it against a real
daemon and a real hub without pulling in an MCP client library.

  mcpcall.py <anet-binary> list
  mcpcall.py <anet-binary> call <tool> '<json-args>'

Environment (HOME, ANET_HOME) is inherited, so the caller decides which
node this talks to.
"""
import json
import os
import subprocess
import sys


class Session:
    def __init__(self, argv):
        self.p = subprocess.Popen(
            argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
            stderr=subprocess.PIPE, text=True, bufsize=1)
        self.n = 0

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

    def close(self):
        try:
            self.p.stdin.close()
            self.p.wait(timeout=10)
        except Exception:
            self.p.kill()


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
        out = s.send("tools/list", {})
        print(json.dumps({"tools": [t["name"] for t in out.get("tools", [])]}))
    elif verb == "call":
        tool = sys.argv[3]
        args = json.loads(sys.argv[4]) if len(sys.argv) > 4 else {}
        out = s.send("tools/call", {"name": tool, "arguments": args})
        # A tool that failed reports isError with the reason in content;
        # both are printed, because "the call failed" and "the call
        # returned an unhappy answer" are different facts.
        text = "".join(c.get("text", "") for c in out.get("content", []))
        print(json.dumps({"is_error": bool(out.get("isError")), "text": text}))
    else:
        raise SystemExit(__doc__)
    s.close()


if __name__ == "__main__":
    main()
