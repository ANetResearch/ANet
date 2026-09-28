#!/usr/bin/env python3
"""soak.py — the real-client suites over and over against one running network (docs/notes/0035,
long-running part): py_client.py and js_client.mjs alternately until DURATION seconds have passed,
sampling every process of the network between rounds (RSS, open descriptors, threads), the size of each
data directory and of what a few read routes return, so that growth shows.

    A2A_JS_DIR=… NODE=… PY=… python soak.py J DURATION OUT_DIR

J is up.sh's work directory (its env.json and bin/). OUT_DIR gets rounds.jsonl (one line per round),
samples.jsonl (one line per sample) and the output of every failed round.
"""
import json
import os
import subprocess
import sys
import time
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))


def procs(binroot, extra_cmd=()):
    """pid → name for the processes run from binroot (anet, anet-hub, a2aprobe)."""
    out = {}
    for p in os.listdir("/proc"):
        if not p.isdigit():
            continue
        try:
            exe = os.readlink("/proc/%s/exe" % p)
            cmd = open("/proc/%s/cmdline" % p, "rb").read().split(b"\0")
        except OSError:
            continue
        if exe.startswith(binroot + "/"):
            name = os.path.basename(exe)
            env = open("/proc/%s/environ" % p, "rb").read().split(b"\0") if name == "anet" else []
            home = [e[5:].decode() for e in env if e.startswith(b"HOME=")]
            if home:
                name += ":" + os.path.basename(home[0])
            elif len(cmd) > 1 and name == "a2aprobe":
                name += ":" + cmd[1].decode()
            out[int(p)] = name
    return out


def stat(pid):
    d = {}
    try:
        for ln in open("/proc/%d/status" % pid):
            k, _, v = ln.partition(":")
            if k in ("VmRSS", "Threads"):
                d[k] = int(v.split()[0])
        d["fds"] = len(os.listdir("/proc/%d/fd" % pid))
    except OSError:
        pass
    return d


def du(path):
    total = 0
    for root, _, files in os.walk(path):
        for f in files:
            try:
                total += os.lstat(os.path.join(root, f)).st_size
            except OSError:
                pass
    return total


def post(addr, path, tokfile, body):
    tok = open(tokfile).read().strip()
    req = urllib.request.Request("http://%s%s" % (addr, path), data=json.dumps(body).encode(),
                                 headers={"Authorization": "Bearer " + tok, "Content-Type": "application/json"})
    t0 = time.time()
    with urllib.request.urlopen(req, timeout=60) as r:
        b = r.read()
    return b, time.time() - t0


def sample(J, env):
    s = {"t": time.time(), "procs": {}, "du": {}}
    for pid, name in procs(os.path.join(J, "bin")).items():
        s["procs"][name] = stat(pid)
    for n in ("req", "prov", "other"):
        s["du"][n] = du(env[n]["data_dir"])
    s["du"]["hub"] = du(os.path.join(env["run"], "hub"))
    r = env["req"]
    b, dt = post(r["ctl"], "/balance", r["ctl_token_file"], {})
    s["balance_bytes"], s["balance_ms"] = len(b), int(dt * 1000)
    b, dt = post(r["ctl"], "/tasks/list", r["ctl_token_file"], {"page_size": 1, "history_length": 0})
    s["tasks_total"], s["list_ms"] = json.loads(b).get("totalSize"), int(dt * 1000)
    b, dt = post(r["ctl"], "/evidence", r["ctl_token_file"], {"limit": 1})
    s["evidence_ms"] = int(dt * 1000)
    return s


def main():
    J, duration, out = sys.argv[1], int(sys.argv[2]), sys.argv[3]
    os.makedirs(out, exist_ok=True)
    env = json.load(open(os.path.join(J, "env.json")))
    py = os.environ.get("PY", sys.executable)
    node = os.environ.get("NODE", "node")
    suites = [("py", [py, os.path.join(HERE, "py_client.py"), os.path.join(J, "env.json")]),
              ("js", [node, os.path.join(HERE, "js_client.mjs"), os.path.join(J, "env.json")])]
    end = time.time() + duration
    rnd = 0
    with open(os.path.join(out, "samples.jsonl"), "a") as sf, open(os.path.join(out, "rounds.jsonl"), "a") as rf:
        sf.write(json.dumps(sample(J, env)) + "\n")
        sf.flush()
        while time.time() < end:
            name, cmd = suites[rnd % len(suites)]
            t0 = time.time()
            p = subprocess.run(cmd, capture_output=True, text=True, timeout=1800)
            lines = p.stdout.splitlines()
            rec = {"round": rnd, "suite": name, "t": t0, "secs": round(time.time() - t0, 1), "exit": p.returncode,
                   "pass": sum(1 for ln in lines if ln.startswith("PASS ")),
                   "fail": [ln[:200] for ln in lines if ln.startswith("FAIL ")],
                   "note": [ln[:160] for ln in lines if ln.startswith("NOTE ") and "sdk:" not in ln]}
            rf.write(json.dumps(rec) + "\n")
            rf.flush()
            if p.returncode != 0:
                with open(os.path.join(out, "round-%03d-%s.txt" % (rnd, name)), "w") as f:
                    f.write(p.stdout + "\n--- stderr ---\n" + p.stderr)
            print("round %d %s: exit %d, %d pass, %d fail, %.0fs" % (rnd, name, p.returncode, rec["pass"],
                                                                      len(rec["fail"]), rec["secs"]), flush=True)
            sf.write(json.dumps(sample(J, env)) + "\n")
            sf.flush()
            rnd += 1


if __name__ == "__main__":
    main()
