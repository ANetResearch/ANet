#!/usr/bin/env python3
"""Generate reference/_generated/reason-codes.inc from the reason table in code.

The table is internal/a2ashape/reasons.go in the ANet repository. Rows come from
dump_reasons.go (which calls a2ashape.Rows), and the grouping of the rows comes
from the group comments that precede them in reasons.go. Run it from the ANet
checkout, with ANetCore resolvable by the Go toolchain (for example through a
go.work file):

    python3 docs/_tools/gen_reason_codes.py [--json rows.json]

--json reads rows that dump_reasons.go already wrote instead of running it.
The output file is committed; rerun the script when the table changes.
"""
import argparse
import json
import pathlib
import re
import subprocess
import sys

HERE = pathlib.Path(__file__).resolve().parent
REPO = HERE.parent.parent
SRC = REPO / "internal" / "a2ashape" / "reasons.go"
OUT = HERE.parent / "reference" / "_generated" / "reason-codes.inc"

# Group comment of reasons.go -> (section id, title). Sections are listed in
# the order they are written to the page.
GROUPS = [
    ("A", "Refused on receipt"),
    ("B", "Refused or failed after acceptance"),
    ("C", "Delivery"),
    ("D", "Payments"),
    ("E", "Identity, keys and local data"),
    ("F", "Joining a hub and admission"),
    ("G", "Refused by this node"),
    ("H", "Local sandbox"),
    ("I", "Egress refusals read by a sandboxed agent"),
    ("J", "Local backends"),
    ("K", "Audit findings"),
    ("M", "Terminal lines and prompts"),
    ("S", "Status summary lines"),
    ("X", "Diagnostic checks"),
    ("W", "Coding-agent registration"),
]

TITLES = dict(GROUPS)

BLURBS = {
    "A": "The other side refused the message when it arrived. The reason travels back with the delivery confirmation.",
    "B": "The other side accepted the message and then refused or failed it, or this node reached a verdict on the result.",
    "C": "Messages sent by this node: terminal failures, waits, the outbox and the delivery state sentences.",
    "D": "Payment decisions, payment outcomes, spending refusals and the limits summary.",
    "E": "Identity operations and states, tasks ended by identity events, local data, the clock and retention.",
    "F": "States of joining a hub and of admission.",
    "G": "Requests that this node refused at once. Nothing was sent.",
    "H": "State sentences of the local sandbox.",
    "I": "Sentences that the agent inside the sandbox reads. They are not shown to the operator. A few codes close the connection and carry no sentence.",
    "J": "State sentences of local service backends and of the C1 interface, for the node operator.",
    "K": "Findings of the audit of a hub's issuance chain and of the balance proofs.",
    "M": "Confirmation prompts and common lines printed by the command line.",
    "S": "Sentences of the status summary and of the text that tells an agent how to proceed.",
    "X": "Sentences that only the diagnostic report prints.",
    "W": "Output of ``anet agents``, ``anet agents wire`` and ``anet agents unwire``: the status words, the lines of a result, the notes, and the refusals with the ``wire_`` codes.",
}


def group_of(label):
    """Map the text after 'ux §' of a group comment to a section id."""
    t = label.strip()
    m = re.match(r"1\.4 ([A-M])", t)
    if m:
        return m.group(1)
    if t.startswith("3.4") or t.startswith("4.7"):
        return "S"
    if t.startswith("5.2"):
        return "X"
    return None


def sections_from_source():
    """Return {row key: section id} in table order."""
    text = SRC.read_text(encoding="utf-8")
    start = text.index("var reasonTable = []Row{")
    cur = None
    out = {}
    for line in text[start:].splitlines():
        if line.startswith("\t// `anet agents wire"):
            cur = "W"
            continue
        m = re.match(r"^\t// ux §(.*)$", line)
        if m:
            g = group_of(m.group(1))
            if g:
                cur = g
            continue
        m = re.match(r'^\t\{Code: "([^"]+)"(?:, Variant: "([^"]+)")?', line)
        if m and cur:
            key = m.group(1) + ("." + m.group(2) if m.group(2) else "")
            out.setdefault(key, cur)
    return out


ESC = re.compile(r"([\\*`|])")
FLAG = re.compile(r"(?<![\w`-])(--[a-z][a-z0-9-]*)")
CMD = re.compile(r"(?<![\w`-])(anet(?:-hub)?(?: [a-z][a-z-]*)?)(?![\w`-])")
# The words that can follow "anet" as a command. Any other word ("anet is
# running") is ordinary text, so only the program name is marked.
COMMANDS = set("""help version init up daemon stop down status doctor logs update id ids identity
hub-register hub-leave visibility profile find card delegate message msg task inbox thread results
pull end review peers inbound autoreply auto-reply preview pay payments payees balance credits redeem
reconcile audit-hub audit retention verify agents mcp console x402-authorize pay-header p2p-advertise
accept accept-end install evidence admit admissions admission grant quota supply due clear redemptions
restore""".split())


def rst(text, nexts=()):
    """Escape one sentence for a table cell and mark commands as literals."""
    marks = []

    def hold(s):
        marks.append(s)
        return "\x00%d\x00" % (len(marks) - 1)

    t = text
    for n in sorted({n for n in nexts if n}, key=len, reverse=True):
        t = t.replace(n, hold("``%s``" % n))
    t = FLAG.sub(lambda m: hold("``%s``" % m.group(1)), t)
    def cmd(m):
        full = m.group(1)
        prog, _, word = full.partition(" ")
        if word and word not in COMMANDS:
            return hold("``%s``" % prog) + full[len(prog):]
        return hold("``%s``" % full)

    t = CMD.sub(cmd, t)
    t = ESC.sub(r"\\\1", t)
    t = re.sub(r"(\w)_(?=\s|$|[.,;:)])", r"\1\\_", t)
    # A literal that touches a non-ASCII character needs an escaped space, or
    # docutils does not recognise its start or end (Chinese text has no spaces).
    def put(m):
        lead = "\\ " if m.start() > 0 and ord(t[m.start() - 1]) > 127 else ""
        end = m.end()
        trail = "\\ " if end < len(t) and ord(t[end]) > 127 else ""
        return lead + marks[int(m.group(1))] + trail

    return re.sub(r"\x00(\d+)\x00", put, t)


def cell_lines(text):
    return text.replace("\n", " ")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--json")
    ap.add_argument("--hub-text", help="cmd/anet-hub/testdata/text.json of the ANetHub repository")
    args = ap.parse_args()
    if args.json:
        rows = json.load(open(args.json, encoding="utf-8"))
    else:
        raw = subprocess.run(["go", "run", "docs/_tools/dump_reasons.go"], cwd=REPO,
                             check=True, capture_output=True).stdout
        rows = json.loads(raw)
    secs = sections_from_source()
    by = {g: [] for g, _ in GROUPS}
    for r in rows:
        g = secs.get(r["key"])
        if g is None:
            print("no group for %s" % r["key"], file=sys.stderr)
            sys.exit(1)
        by[g].append(r)

    out = []
    w = out.append
    w(".. Generated by docs/_tools/gen_reason_codes.py from internal/a2ashape/reasons.go.")
    w(".. Do not edit by hand; rerun the script when the reason table changes.")
    w("")
    for g, title in GROUPS:
        rs = by[g]
        if not rs:
            continue
        w(".. _reason-codes-%s:" % g.lower())
        w("")
        w("%s" % title)
        w("-" * len(title))
        w("")
        w(BLURBS[g])
        w("")
        w(".. list-table::")
        w("   :header-rows: 1")
        w("   :widths: 16 9 31 31 13")
        w("")
        w("   * - Code")
        w("     - Class")
        w("     - English")
        w("     - 中文")
        w("     - Next step")
        for r in rs:
            nexts = [r.get("next", "")]
            cls = r["classes"].replace(",", " ")
            extra = r.get("effect") or r.get("x402") or r.get("level") or ""
            if r.get("effect") and r.get("x402"):
                extra = r["effect"] + " / " + r["x402"]
            cell = cls + ("\n       " + extra if extra else "")
            key = r["key"]
            if r.get("no_sentence"):
                en = zh = "(connection closed, no sentence)"
            else:
                en = rst(cell_lines(r["en"]), nexts)
                zh = rst(cell_lines(r["zh"]), nexts)
            nk = r.get("next_kind", "")
            if nk == "command" and r.get("next"):
                nxt = "``%s``" % r["next"]
            else:
                nxt = nk.replace("_", " ") if nk else ""
            w("   * - ``%s``" % key)
            w("     - %s" % cell.replace("\n       ", " "))
            w("     - %s" % en)
            w("     - %s" % zh)
            w("     - %s" % nxt)
        w("")
    OUT.parent.mkdir(parents=True, exist_ok=True)
    OUT.write_text("\n".join(out) + "\n", encoding="utf-8")
    print("wrote %s (%d rows)" % (OUT, len(rows)))
    if args.hub_text:
        hub_sentences(args.hub_text)


def hub_sentences(path):
    """Write reference/_generated/hub-sentences.inc from the anet-hub sentence table."""
    data = json.load(open(path, encoding="utf-8"))
    out = [".. Generated by docs/_tools/gen_reason_codes.py from cmd/anet-hub/testdata/text.json (ANetHub).",
           ".. Do not edit by hand.", "", ".. list-table::", "   :header-rows: 1",
           "   :widths: 22 7 7 32 32", "",
           "   * - Key", "     - Class", "     - Exit", "     - English", "     - 中文"]
    for r in data["rows"]:
        nx = [r.get("next", "")]
        out.append("   * - ``%s``" % r["key"])
        out.append("     - %s" % r.get("class", ""))
        out.append("     - %s" % r.get("exit", ""))
        out.append("     - %s" % rst(r["en"].replace("\n", " "), nx))
        out.append("     - %s" % rst(r["zh"].replace("\n", " "), nx))
    dst = OUT.parent / "hub-sentences.inc"
    dst.write_text("\n".join(out) + "\n", encoding="utf-8")
    print("wrote %s (%d rows)" % (dst, len(data["rows"])))


if __name__ == "__main__":
    main()
