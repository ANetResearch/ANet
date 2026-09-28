#!/usr/bin/env python3
"""soakreport.py STATE — the end checks of the lab soak (scripts/testnet/soak.sh check).

Reads what soak.sh pulled into STATE/pull (the load workers' events, the end-of-run dumps of every
node and both hubs, the hubs' /x402/supply, the canary scans) and STATE/samples.jsonl, and prints a
Markdown report; STATE/summary.json holds the same numbers. Checks:

  1. task success rate per operation and peer, failures by reason and by nearness to a restart;
  2. no stuck task: every task at every node terminal, or open for a known reason, older than
     STUCK_MIN minutes at the dump;
  3. no monotonic growth: RSS, threads, fds per process (each run of a process separately), SQLite
     sizes, hub mailboxes, outbox rows, open tasks — first/last/max and the slope over the second half;
  4. payments: per paid task exactly one settlement on the payer's hub (by pay_bind), none for an
     unpaid one, each cross-hub settlement cleared once at the payee's hub, the paid capability ran
     once per settlement; balances across both hubs equal the grants; due/owed agree; each hub's
     outstanding equals its balances and its issuance chain agrees;
  5. canary: no text of any task in either hub's data or logs (the same scan finds them in a
     provider's own store).
"""
import collections, glob, hashlib, json, math, os, statistics, sys, time

STATE = sys.argv[1]
P = os.path.join(STATE, "pull")
STUCK_MIN = float(os.environ.get("STUCK_MIN", "10"))
TERMINAL = {"completed", "failed", "canceled", "rejected"}
out = []
summary = {}


def w(s=""):
    out.append(s)


def load_json(p, default=None):
    try:
        return json.load(open(p))
    except (OSError, ValueError):
        return default


def pct(xs, q):
    if not xs:
        return float("nan")
    xs = sorted(xs)
    k = (len(xs) - 1) * q
    f = math.floor(k)
    c = min(f + 1, len(xs) - 1)
    return xs[f] + (xs[c] - xs[f]) * (k - f)


def hm(ts):
    return time.strftime("%H:%M", time.gmtime(ts))


t0 = float(open(os.path.join(STATE, "t0")).read().strip()) if os.path.exists(os.path.join(STATE, "t0")) else 0
chaos = []
for line in open(os.path.join(STATE, "chaos.log")) if os.path.exists(os.path.join(STATE, "chaos.log")) else []:
    ts, what = line.split(" ", 1)
    chaos.append((float(ts), what.strip()))

# ── 1. events ────────────────────────────────────────────────────────────────
events, harness1 = [], []
for f in glob.glob(os.path.join(P, "*", "logs", "events-*.jsonl")):
    dst = harness1 if ".harness1." in f else events
    for l in open(f):
        try:
            dst.append(json.loads(l))
        except ValueError:
            pass
events.sort(key=lambda e: e["ts"])
ops = [e for e in events if e["op"] != "worker-exit"]
t_first = min((e["ts"] - e["lat"] for e in ops), default=t0)
t_last = max((e["ts"] for e in ops), default=t0)


def near_chaos(e, before=60, after=180):
    """The restart an operation overlapped: it was running, or ended within `after` s, of the stop."""
    start = e["ts"] - e["lat"]
    for ts, what in chaos:
        # chaos.log is written when the restart finished; the stop began up to ~70 s earlier
        if start - after <= ts and e["ts"] >= ts - 70 - before:
            return what
    return None


w("# Soak report")
w()
w("Load from %s to %s UTC (%.2f h), %d operations; restarts: %d." % (
    time.strftime("%Y-%m-%d %H:%M", time.gmtime(t_first)), hm(t_last), (t_last - t_first) / 3600, len(ops), len(chaos)))
w()
w("## 1. Operations")
w()
w("| op | requester → peer | n | ok | fail | success | p50 s | p95 s | max s |")
w("|---|---|---:|---:|---:|---:|---:|---:|---:|")
groups = collections.defaultdict(list)
for e in ops:
    kind = e.get("skill") or e.get("mode") or ("?" if e["op"] in ("cap", "cas") else "")
    k = (("cap" if e["op"] == "cas" else e["op"]) + ("/" + kind if e["op"] in ("cap", "cancel", "paid", "cas") else ""),
         "%s → %s" % (e["node"], e["peer"]))
    groups[k].append(e)
tot_ok = tot = 0
by_op = collections.Counter()
by_op_ok = collections.Counter()
for k in sorted(groups):
    es = groups[k]
    ok = sum(1 for e in es if e["ok"])
    lats = [e["lat"] for e in es if e["ok"]]
    tot += len(es)
    tot_ok += ok
    by_op[k[0].split("/")[0]] += len(es)
    by_op_ok[k[0].split("/")[0]] += ok
    w("| %s | %s | %d | %d | %d | %.2f%% | %.1f | %.1f | %.1f |" % (k[0], k[1], len(es), ok, len(es) - ok,
      100.0 * ok / len(es), pct(lats, .5), pct(lats, .95), max(lats) if lats else float("nan")))
w("| **all** | | **%d** | **%d** | **%d** | **%.2f%%** | | | |" % (tot, tot_ok, tot - tot_ok, 100.0 * tot_ok / max(tot, 1)))
w()
summary["ops"] = {"total": tot, "ok": tot_ok, "by_op": {k: [by_op[k], by_op_ok[k]] for k in by_op}}
fails = [e for e in ops if not e["ok"]]
near = [(e, near_chaos(e)) for e in fails]


def classify(e, c):
    """Why an operation failed, when the reason is one the design gives: the requester's own daemon
    was down when the client tried to submit (nothing was sent), or a long call was cut by its
    provider's restart (reported failed, effect unverified, A2A-DESIGN §4.3). Else unexplained."""
    err = e.get("err") or ""
    if not (e.get("ix") or e.get("task")) and "Connection refused" in err and c and e["node"] in c:
        return "not submitted: own daemon restarting"
    if e["op"] == "cap" and e.get("skill") == "soak.long" and e.get("state") == "failed" and c and c.startswith("restart"):
        return "long call cut by the provider's restart (by design)"
    return "unexplained"


classes = collections.Counter(classify(e, c) for e, c in near)
w("Failures: %d — %s." % (len(fails), ", ".join("%s %d" % (k, v) for k, v in classes.most_common()) or "none"))
submitted = [e for e in ops if not (not e["ok"] and classify(e, near_chaos(e)).startswith("not submitted"))]
by_design = sum(v for k, v in classes.items() if k != "unexplained")
w("Success rate of submitted operations: %.3f%% (%d/%d); counting only unexplained failures: %.3f%%." % (
    100.0 * sum(1 for e in submitted if e["ok"]) / max(len(submitted), 1), sum(1 for e in submitted if e["ok"]),
    len(submitted), 100.0 * (len(ops) - classes.get("unexplained", 0)) / max(len(ops), 1)))
w()
summary["fail_classes"] = dict(classes)
if fails:
    w("| time | op | requester → peer | kind | restart | class | reason |")
    w("|---|---|---|---|---|---|---|")
    for e, c in near:
        w("| %s | %s | %s → %s | %s | %s | %s | %s |" % (hm(e["ts"]), e["op"], e["node"], e["peer"],
          e.get("skill") or e.get("mode") or e.get("binding") or "", c or "", classify(e, c),
          (e.get("err") or "")[-120:].replace("|", "/").replace("\n", " ")))
    w()
held = [e for e in ops if e.get("held_seen")]
if held:
    w("Auto-tier paid calls whose waiter saw input-required/%s before the automatic payment (F1): %d of %d." % (
        held[0]["held_seen"], len(held), sum(1 for e in ops if e["op"] == "paid" and e.get("by") == "auto")))
    w()
broken = [e for e in ops if e["op"] == "a2a" and (e.get("broken") or e.get("recovered"))]
if broken:
    w("A2A streams that broke and were finished by GetTask: %d (%s)." % (len(broken), ", ".join(
        "%s %s" % (hm(e["ts"]), near_chaos(e) or "-") for e in broken[:12])))
    w()
if harness1:
    w("Not counted: %d operations of the first 2 minutes run with a harness bug (image attachments not "
      "sniffed as images; %d reply mismatches, all of them images), rerun with the fix." % (
          len(harness1), sum(1 for e in harness1 if not e["ok"])))
    w()
exits = [e for e in events if e["op"] == "worker-exit"]
if exits:
    w("Load workers drained: %s." % ", ".join("%s %s" % (e["node"], "clean" if e["ok"] else "left %s" % e.get("left"))
                                               for e in exits))
    w()

# ── 2. stuck tasks ───────────────────────────────────────────────────────────
dumps = {}
for f in glob.glob(os.path.join(P, "dump-*.json")):
    d = load_json(f, {})
    dumps.update(d.get("nodes") or {})
    for h, v in (d.get("hubs") or {}).items():
        dumps.setdefault("_hubs", {})[h] = v
    for n, v in (d.get("backend") or {}).items():
        dumps.setdefault("_backend", {})[n] = v
hubs = dumps.pop("_hubs", {})
backend = dumps.pop("_backend", {})
dump_t = max((os.path.getmtime(f) for f in glob.glob(os.path.join(P, "dump-*.json"))), default=time.time())
aid2name = {v.get("aid"): n for n, v in dumps.items() if v.get("aid")}
w("## 2. Open tasks at the end")
w()
tasks_total = sum(len(v["interactions"]) for v in dumps.values())
stuck, waiting_op = [], []
for n, v in sorted(dumps.items()):
    for ix in v["interactions"]:
        if ix["state"] in TERMINAL:
            continue
        age = dump_t - ix["state_at"] / 1000.0
        rec = (n, ix["role"], aid2name.get(ix["peer"], ix["peer"][:12]), ix["state"], ix["pay_state"], round(age / 60, 1),
               ix["id"], ix.get("last_msg"))
        if ix["role"] == "outbound" and ix["state"] == "input-required" and ix["pay_state"] in ("required", "failed"):
            waiting_op.append(rec)
        elif age > STUCK_MIN * 60:
            stuck.append(rec)
w("%d tasks across %d nodes; open at the dump and older than %g min: **%d**; waiting for an operator's payment "
  "decision (quote above the agent tier, by design): %d." % (tasks_total, len(dumps), STUCK_MIN, len(stuck), len(waiting_op)))
w()
if stuck or waiting_op:
    w("| node | role | peer | state | pay | age min | task | last message |")
    w("|---|---|---|---|---|---:|---|---|")
    for r in stuck + waiting_op:
        w("| %s | %s | %s | %s | %s | %s | %s | %s |" % (r[0], r[1], r[2], r[3], r[4] or "", r[5], r[6],
          (str(r[7][0])[:60].replace("|", "/") if r[7] else "")))
    w()
states = collections.Counter((n, ix["role"], ix["state"]) for n, v in dumps.items() for ix in v["interactions"])
w("Tasks by node, role and state: " + "; ".join("%s %s %s %d" % (a, b, c, k) for (a, b, c), k in sorted(states.items())))
w()
outboxes = {n: v.get("outbox") or [] for n, v in dumps.items()}
w("Outbox rows left at the end: " + ", ".join("%s %d" % (n, len(r)) for n, r in sorted(outboxes.items())))
w()
summary["stuck"] = len(stuck)
summary["waiting_operator"] = len(waiting_op)
summary["tasks_total"] = tasks_total

# ── 3. resources ─────────────────────────────────────────────────────────────
samples = [json.loads(l) for l in open(os.path.join(STATE, "samples.jsonl"))] if os.path.exists(
    os.path.join(STATE, "samples.jsonl")) else []
w("## 3. Resources (sampled every 5 min)")
w()
series = collections.defaultdict(list)  # (kind, name, metric) -> [(ts, value, pid, age)]
for s in samples:
    ts = s["ts"]
    for name, p in (s.get("procs") or {}).items():
        if p.get("dead"):
            continue
        for m in ("rss_kb", "threads", "fds", "cpu_s"):
            series[("proc", name, m)].append((ts, p[m], p["pid"], p.get("age_s")))
    for name, nd in (s.get("nodes") or {}).items():
        series[("node", name, "db_bytes")].append((ts, nd.get("db_total", 0), 0, 0))
        series[("node", name, "home_bytes")].append((ts, nd.get("home_bytes", 0), 0, 0))
        series[("node", name, "outbox")].append((ts, (nd.get("outbox") or {}).get("rows", 0), 0, 0))
        series[("node", name, "open_tasks")].append((ts, sum(r[2] for r in nd.get("open") or []), 0, 0))
        series[("node", name, "tasks")].append((ts, (nd.get("tasks") or {}).get("total", 0), 0, 0))
        for t, c in (nd.get("rows") or {}).items():
            if isinstance(c, int):
                series[("node", name, "rows." + t)].append((ts, c, 0, 0))
        wal = sum(v for k, v in (nd.get("db") or {}).items() if k.endswith("-wal"))
        series[("node", name, "wal_bytes")].append((ts, wal, 0, 0))
    for name, h in (s.get("hubs") or {}).items():
        series[("hub", name, "db_bytes")].append((ts, h.get("db_total", 0), 0, 0))
        mb = h.get("mailbox") or {}
        series[("hub", name, "mailbox_msgs")].append((ts, mb.get("msgs", 0) or 0, 0, 0))
        series[("hub", name, "mailbox_bytes")].append((ts, mb.get("bytes", 0) or 0, 0, 0))
        for t, c in (h.get("rows") or {}).items():
            if isinstance(c, int):
                series[("hub", name, "rows." + t)].append((ts, c, 0, 0))
        wal = sum(v for k, v in (h.get("db") or {}).items() if k.endswith("-wal"))
        series[("hub", name, "wal_bytes")].append((ts, wal, 0, 0))


def segments(pts):
    """Split a process series at restarts (the pid changes)."""
    segs, cur, pid = [], [], None
    for p in pts:
        if pid is not None and p[2] != pid:
            segs.append(cur)
            cur = []
        cur.append(p)
        pid = p[2]
    if cur:
        segs.append(cur)
    return segs


def slope_per_h(pts):
    if len(pts) < 3:
        return float("nan")
    xs = [(p[0] - pts[0][0]) / 3600 for p in pts]
    ys = [p[1] for p in pts]
    mx, my = statistics.mean(xs), statistics.mean(ys)
    den = sum((x - mx) ** 2 for x in xs)
    return sum((x - mx) * (y - my) for x, y in zip(xs, ys)) / den if den else float("nan")


def monotone(pts):
    """Fraction of steps that do not go down."""
    if len(pts) < 3:
        return float("nan")
    return sum(1 for a, b in zip(pts, pts[1:]) if b[1] >= a[1]) / (len(pts) - 1)


verdicts = []
w("Per process (each run of a process between restarts separately; second-half slope of its longest run):")
w()
w("| process | metric | runs | first | last | max | slope/h (2nd half) | non-decreasing steps |")
w("|---|---|---:|---:|---:|---:|---:|---:|")
for (kind, name, m), pts in sorted(series.items()):
    if kind != "proc" or m == "cpu_s":
        continue
    segs = segments(pts)
    longest = max(segs, key=len)
    half = longest[len(longest) // 2:]
    sl = slope_per_h(half)
    mono = monotone(half)
    w("| %s | %s | %d | %s | %s | %s | %.1f | %.0f%% |" % (name, m, len(segs), pts[0][1], pts[-1][1],
      max(p[1] for p in pts), sl, 100 * mono if mono == mono else float("nan")))
    verdicts.append({"name": name, "metric": m, "runs": len(segs), "first": pts[0][1], "last": pts[-1][1],
                     "max": max(p[1] for p in pts), "slope_h": sl, "mono": mono, "n": len(longest),
                     "seg_first": longest[0][1], "seg_last": longest[-1][1]})
w()
w("Stores, mailboxes and queues:")
w()
w("| where | metric | first | last | max | slope/h (2nd half) |")
w("|---|---|---:|---:|---:|---:|")
for (kind, name, m), pts in sorted(series.items()):
    if kind == "proc":
        continue
    half = pts[len(pts) // 2:]
    w("| %s %s | %s | %s | %s | %s | %.1f |" % (kind, name, m, pts[0][1], pts[-1][1], max(p[1] for p in pts), slope_per_h(half)))
w()
summary["resources"] = verdicts

# the curves: one row per sample (every 30 min in the report; all in curves.tsv)
names = sorted({n for (k, n, m) in series if k == "proc"})
with open(os.path.join(STATE, "curves.tsv"), "w") as f:
    f.write("ts\tt_min\tprocess\trss_kb\tthreads\tfds\tcpu_s\n")
    for s in samples:
        for name, p in sorted((s.get("procs") or {}).items()):
            if not p.get("dead"):
                f.write("%d\t%.0f\t%s\t%s\t%s\t%s\t%s\n" % (s["ts"], (s["ts"] - t0) / 60 if t0 else 0, name,
                                                          p["rss_kb"], p["threads"], p["fds"], p["cpu_s"]))
w("RSS (MiB) / threads / fds over time, every ~30 min (all samples in `curves.tsv`):")
w()
picked, last_t = [], None
for s in samples:
    if last_t is None or s["ts"] - last_t >= 1700 or s is samples[-1]:
        if s.get("host") and (not picked or picked[-1]["ts"] != s["ts"]):
            picked.append(s)
            last_t = s["ts"]
by_t = collections.OrderedDict()
for s in samples:
    by_t.setdefault(round((s["ts"] - t0) / 60 / 5) * 5 if t0 else s["ts"], {}).update(s.get("procs") or {})
keys = list(by_t)
sel = [k for i, k in enumerate(keys) if i == 0 or i == len(keys) - 1 or (isinstance(k, (int, float)) and k % 30 == 0)]
w("| process | " + " | ".join("t+%s" % k for k in sel) + " |")
w("|---|" + "---:|" * len(sel))
for n in names:
    row = []
    for k in sel:
        p = by_t[k].get(n)
        row.append("%.0f/%d/%d" % (p["rss_kb"] / 1024, p["threads"], p["fds"]) if p and not p.get("dead") else "–")
    w("| %s | %s |" % (n, " | ".join(row)))
w()

# after the load: does RSS come back down?
post = [s for s in samples if s.get("label") in ("drained",) or str(s.get("label", "")).startswith("idle")]
if post:
    last_load = {}
    for s in samples:
        if str(s.get("label", "")).startswith("t+"):
            last_load.setdefault(s["host"], s)
            if s["ts"] >= last_load[s["host"]]["ts"]:
                last_load[s["host"]] = s
    cols = [("load end", None)] + [("%s (+%d min)" % (s["label"], (s["ts"] - t_last) / 60), s) for s in post]
    seen_cols = []
    for c in cols:
        if c[0] not in [x[0] for x in seen_cols]:
            seen_cols.append(c)
    w("RSS (MiB) at the end of the load and after it stopped (the Go scavenger returns freed heap within minutes; "
      "a leak would stay):")
    w()
    labels = []
    for s in post:
        if s["label"] not in labels:
            labels.append(s["label"])
    w("| process | load end | " + " | ".join("%s (+%.0f min)" % (l, (min(x["ts"] for x in post if x["label"] == l) - t_last) / 60)
                                         for l in labels) + " |")
    w("|---|---:|" + "---:|" * len(labels))
    for n in names:
        if n.startswith("soak-"):
            continue
        row = []
        ll = next((x for x in last_load.values() if n in (x.get("procs") or {})), None)
        row.append("%.0f" % (ll["procs"][n]["rss_kb"] / 1024) if ll and not ll["procs"][n].get("dead") else "–")
        for l in labels:
            x = next((x for x in post if x["label"] == l and n in (x.get("procs") or {})), None)
            row.append("%.0f" % (x["procs"][n]["rss_kb"] / 1024) if x and not x["procs"][n].get("dead") else "–")
        w("| %s | %s |" % (n, " | ".join(row)))
    w()

# goroutines: SIGQUIT dumps before each scheduled restart and of every process at the end
gd = []
for f in glob.glob(os.path.join(P, "*", "logs", "gdump.jsonl")):
    for l in open(f):
        try:
            gd.append(json.loads(l))
        except ValueError:
            pass
if gd:
    w("Goroutines (SIGQUIT dump: the runtime's stack of every goroutine; there is no pprof endpoint): %d dumps, "
      "%d–%d goroutines, after up to %.1f h of uptime." % (len(gd), min(r["goroutines"] for r in gd),
                                                           max(r["goroutines"] for r in gd), max(r["uptime_s"] for r in gd) / 3600))
    w()
    w("| process | uptime h | goroutines | busiest wait sites |")
    w("|---|---:|---:|---|")
    for r in sorted(gd, key=lambda r: (r["name"], r["uptime_s"])):
        w("| %s | %.1f | %d | %s |" % (r["name"], r["uptime_s"] / 3600, r["goroutines"], "; ".join(
            "%d× %s" % (c, k.replace("|", "/")[:70]) for k, c in r["top"][:2])))
    w()
    summary["goroutines"] = [(r["name"], r["uptime_s"], r["goroutines"]) for r in gd]

# ── 4. payments ──────────────────────────────────────────────────────────────
w("## 4. Payments")
w()
hub_aid = {}
for h in ("hub1", "hub2"):
    idf = load_json(os.path.join(P, "identity-%s.json" % h), {})
    hub_aid[h] = idf.get("aid")
settled = {}  # bind -> [rows]
settled_by_auth = {}
for h, hv in hubs.items():
    for r in hv.get("settled") or []:
        r = dict(r, hub=h)
        settled.setdefault(r["ix"], []).append(r)
        settled_by_auth[r["auth_id"]] = r
cleared = collections.Counter()
for h, hv in hubs.items():
    for r in hv.get("cleared") or []:
        cleared[r["auth_id"]] += 1
home_hub = {}
for h, hv in hubs.items():
    for aid in (hv.get("balances") or {}):
        home_hub.setdefault(aid, h)
paid_tasks = []
problems = []
for n, v in dumps.items():
    for ix in v["interactions"]:
        if ix["role"] != "outbound" or not ix["pay_state"]:
            continue
        rows = settled.get(ix.get("bind"), [])
        paid_tasks.append((n, ix, rows))
        if ix["pay_state"] == "completed":
            if len(rows) != 1:
                problems.append("%s %s completed with %d settlements" % (n, ix["id"], len(rows)))
        elif ix["pay_state"] in ("required", "rejected"):
            if rows:
                problems.append("%s %s is %s/%s but has %d settlement(s)" % (n, ix["id"], ix["state"], ix["pay_state"], len(rows)))
        elif ix["pay_state"] == "submitted":
            problems.append("%s %s still submitted (%s), %d settlement(s)" % (n, ix["id"], ix["state"], len(rows)))
        elif ix["pay_state"] == "failed" and rows:
            problems.append("%s %s payment failed but %d settlement(s)" % (n, ix["id"], len(rows)))
binds = {ix.get("bind") for _, ix, _ in paid_tasks}
orphans = [r for b, rs in settled.items() for r in rs if b not in binds]
for r in orphans:
    problems.append("settlement %s on %s (payer %s) matches no paid task" % (r["auth_id"], r["hub"], aid2name.get(r["payer"], r["payer"][:12])))
# cross-hub: cleared exactly once at the payee's hub
cross = 0
for a, r in settled_by_auth.items():
    payee_hub = home_hub.get(r["pay_to"])
    if payee_hub and payee_hub != r["hub"]:
        cross += 1
        if cleared[a] != 1:
            problems.append("cross-hub settlement %s (%s → %s) cleared %d times" % (a, r["hub"], payee_hub, cleared[a]))
# the paid capability ran once per settlement (backend call log, by caller)
ran = collections.Counter()
for node, calls in backend.items():
    for c in calls:
        if c.get("cap") in ("soak.paid", "soak.pricey") and not c.get("lost"):
            ran[(node, c["cap"])] += 1
settled_to = collections.Counter()
for n, ix, rows in paid_tasks:
    if ix["pay_state"] == "completed" and len(rows) == 1:
        settled_to[aid2name.get(ix["peer"], ix["peer"])] += 1
states = collections.Counter((n, ix["pay_state"], ix["state"]) for n, ix, _ in paid_tasks)
w("Paid tasks (requester side, pay_state/state): " + "; ".join("%s %s/%s %d" % (a, b, c, k) for (a, b, c), k in sorted(states.items())))
w()
w("Settlements on the hubs: %s; of which cross-hub %d, each cleared exactly once at the payee's hub: %s." % (
    ", ".join("%s %d" % (h, len(hv.get("settled") or [])) for h, hv in sorted(hubs.items())), cross,
    "yes" if not any("cleared" in p for p in problems) else "**no**"))
w()
w("Paid capability executions at the backends vs settled paid tasks per provider: " + ", ".join(
    "%s ran %d / settled %d" % (node, ran[(node, "soak.paid")] + ran[(node, "soak.pricey")], settled_to[node]) for node in sorted(backend)) +
  ("; off2 (official backend, no call log) settled %d" % settled_to.get("off2", 0)))
w()
# conservation
grants = 0
user_bal = 0
for h, hv in sorted(hubs.items()):
    for aid, amt, reason in hv.get("grants") or []:
        if not reason.startswith("issued:"):  # the agent's entry, not the hub row's matching debit
            grants += amt
    for aid, bal in (hv.get("balances") or {}).items():
        if aid not in hub_aid.values():
            user_bal += bal
sup = {h: (load_json(os.path.join(P, "supply-%s.json" % h), {}) or {}).get("supply") or {} for h in ("hub1", "hub2")}
w("| hub | issued | redeemed | outstanding | balances | chain outstanding | chain agrees | owed (to it) | due (from it) |")
w("|---|---:|---:|---:|---:|---:|---|---:|---:|")
for h in ("hub1", "hub2"):
    s = sup.get(h) or {}
    w("| %s | %s | %s | %s | %s | %s | %s | %s | %s |" % (h, s.get("issued"), s.get("redeemed"), s.get("outstanding"),
      s.get("balances"), s.get("chain_outstanding"), s.get("chain_agrees"), s.get("owed"), s.get("due")))
    if s.get("outstanding") != s.get("balances"):
        problems.append("%s: outstanding %s != balances %s" % (h, s.get("outstanding"), s.get("balances")))
    if s.get("chain_agrees") is not True:
        problems.append("%s: issuance chain does not agree" % h)
w()
d1, d2 = (hubs.get("hub1") or {}).get("due") or {}, (hubs.get("hub2") or {}).get("due") or {}
o1, o2 = (hubs.get("hub1") or {}).get("owed") or {}, (hubs.get("hub2") or {}).get("owed") or {}
due12 = sum(d1.values())
owed21 = o2.get(hub_aid.get("hub1"), 0)
due21 = sum(d2.values())
owed12 = o1.get(hub_aid.get("hub2"), 0)
w("Between the hubs: hub1 due %d = hub2 owed by hub1 %d: %s; hub2 due %d = hub1 owed by hub2 %d: %s." % (
    due12, owed21, "yes" if due12 == owed21 else "**no**", due21, owed12, "yes" if due21 == owed12 else "**no**"))
if due12 != owed21 or due21 != owed12:
    problems.append("due/owed disagree")
w()
w("Grants on both hubs (registration + operator) %d; the agents' balances on both hubs add up to %d: %s." % (
    grants, user_bal, "conserved" if grants == user_bal else "**not conserved (%+d)**" % (user_bal - grants)))
if grants != user_bal:
    problems.append("grants %d != balances %d" % (grants, user_bal))
w()
spent = []
for n, v in sorted(dumps.items()):
    pay = v.get("payments") or {}
    if "spent_24h" not in pay:
        continue
    aid = v.get("aid")
    hub_sum = sum(r["amount"] for rs in settled.values() for r in rs if r["payer"] == aid)
    spent.append("%s signed %s, settled %d" % (n, pay.get("spent_24h"), hub_sum))
w("Spend counters (24 h, signed authorizations) vs settled at the hubs: " + "; ".join(spent) + ".")
w()
if problems:
    w("**Payment problems (%d):**" % len(problems))
    w()
    for p in problems[:50]:
        w("- " + p)
    w()
else:
    w("Every paid task has exactly one settlement, no unpaid one has any, every cross-hub settlement was cleared "
      "once, and credit is conserved across both ledgers.")
    w()
summary["payments"] = {"paid_tasks": len(paid_tasks), "settled": {h: len(hv.get("settled") or []) for h, hv in hubs.items()},
                       "cross": cross, "grants": grants, "balances": user_bal, "problems": problems}

# ── 5. canary ────────────────────────────────────────────────────────────────
w("## 5. Canary scan")
w()
for f in sorted(glob.glob(os.path.join(P, "scan-*.json"))):
    s = load_json(f, {})
    w("- %s: %s canaries, %d file(s) hit%s" % (os.path.basename(f)[5:-5], s.get("canaries"), len(s.get("files_hit") or {}),
      (": " + ", ".join("%s (%d)" % (k.split("/nodes/")[-1], v) for k, v in list((s.get("files_hit") or {}).items())[:6]))
      if s.get("files_hit") else ""))
    summary.setdefault("canary", {})[os.path.basename(f)[5:-5]] = len(s.get("files_hit") or {})
w()

# ── 6. restarts ──────────────────────────────────────────────────────────────
w("## 6. Restarts")
w()
for ts, what in chaos:
    w("- t+%d min (%s): %s" % ((ts - t0) / 60 if t0 else 0, hm(ts), what))
w()
json.dump(summary, open(os.path.join(STATE, "summary.json"), "w"), indent=1, default=str)
print("\n".join(out))
