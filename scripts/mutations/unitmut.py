#!/usr/bin/env python3
"""unitmut.py — unit-level mutations of the A2A-DESIGN §1 invariants and the §17 supplementary cases.

mutate.sh (beside this file) runs a patch through joint.sh; this runs a mutation through the Go tests
that carry the invariant: apply one small edit to one repository, run the named tests, record whether
they went red, restore the working tree. The lists are unit/si.py (SI-1..SI-10) and unit/supp.py (the
§17 supplementary rows), written for docs/notes/0026, and unit/fixwave.py (the round-5b red-team fixes
and 0017 Q28-Q33), written for docs/notes/0029.

  unitmut.py unit/si.py                 every mutation of the list not yet recorded in OUT/results.json
  unitmut.py unit/si.py ID…             these, recorded or not
  unitmut.py --check unit/si.py …       apply and restore each edit without running tests (does the list
                                        still match the code?)

A mutation that survives its key tests is run against the whole of each package in "full"; one that
survives that too is a gap (or an equivalent mutation: say which in the note). Verdicts: KILLED,
SURVIVED (full: SURVIVED-full or KILLED-by-other), BUILD-ERR and APPLY-ERR (the list needs fixing).

It edits the checkouts it runs in, one mutation at a time: every repository must be clean (git status)
before each mutation. Before applying it copies every file the mutation writes (its edits' paths and the
files its patch touches); afterwards it writes those copies back, deletes the files the mutation created
and any untracked file the tests left behind, and checks the repository is clean again. It does not use
`git stash`: refs/stash is shared by every work tree of a repository, so a stash entry pushed here can be
popped or dropped by a run in another work tree (0029). Commit first; never run two at once in one work
tree (UNITMUT_ROOT can point at a separate clone to run beside other work).

Environment:
  UNITMUT_ROOT  the directory holding ANetCore, ANet and ANetHub (default: the one above this ANet
                checkout, i.e. the work-tree root); its go.work is used when GOWORK is not set
  UNITMUT_OUT   results.json and one log per mutation (default ${TMPDIR:-/tmp}/anet-unitmut-<uid>)

A list is Python defining MUTS, each a dict:
  id, group, repo ("ANetCore" | "ANet" | "ANetHub"), desc
  edits  [(path, old, new[, count]), …]  exact replacement; old must occur count (default 1) times;
         old None creates the file path
  patch  a patch file for `git apply` (instead of, or before, edits)
  tests  [(pkg, run-regex[, tags]), …]   pkg is "Repo:./path" or relative to repo; "Repo:sh CMD"
         runs a shell command in that repository (non-zero = red)
  full   packages run whole when the key tests stay green
The list is executed with ROOT (the work-tree root) and MUTATIONS (this directory) defined.
"""
import json, os, re, subprocess, sys, time

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.environ.get("UNITMUT_ROOT") or os.path.abspath(os.path.join(HERE, "..", "..", ".."))
OUT = os.environ.get("UNITMUT_OUT") or os.path.join(os.environ.get("TMPDIR", "/tmp"), "anet-unitmut-%d" % os.getuid())
RES = os.path.join(OUT, "results.json")
REPOS = ("ANetCore", "ANet", "ANetHub")
ENV = dict(os.environ)
if os.path.exists(os.path.join(ROOT, "go.work")) and not ENV.get("GOWORK"):
    ENV["GOWORK"] = os.path.join(ROOT, "go.work")


def sh(args, cwd, timeout=None):
    p = subprocess.run(args, cwd=cwd, env=ENV, capture_output=True, text=True, timeout=timeout)
    return p.returncode, p.stdout + p.stderr


def porcelain(repo):
    return sh(["git", "status", "--porcelain"], os.path.join(ROOT, repo))[1].strip()


def touched(m):
    """The files (relative to the repository) the mutation writes: its edits' paths and its patch's files."""
    paths = []
    if m.get("patch"):
        rc, out = sh(["git", "apply", "--numstat", "-z", m["patch"]], os.path.join(ROOT, m["repo"]))
        if rc != 0:
            raise ValueError("git apply --numstat: " + out)
        f = out.split("\0")
        i = 0
        while i < len(f):
            if not f[i]:
                i += 1
                continue
            cols = f[i].split("\t")
            if len(cols) == 3 and cols[2]:
                paths.append(cols[2])
                i += 1
            else:  # a rename: "added\tdeleted\t" then the old and the new path
                paths += [f[i + 1], f[i + 2]]
                i += 3
    paths += [e[0] for e in m.get("edits", [])]
    return list(dict.fromkeys(paths))


def backup(m):
    """Copy (content, mode) of every file the mutation will write; None for one that does not exist yet."""
    saved = {}
    for path in touched(m):
        fp = os.path.join(ROOT, m["repo"], path)
        if os.path.exists(fp):
            with open(fp, "rb") as f:
                saved[path] = (f.read(), os.stat(fp).st_mode & 0o7777)
        else:
            saved[path] = None
    return saved


def restore(repo, mid, saved):
    """Put repo back as it was before the mutation, from the copies backup() took (no git stash)."""
    cwd = os.path.join(ROOT, repo)
    for path, v in (saved or {}).items():
        fp = os.path.join(cwd, path)
        if v is None:
            if os.path.lexists(fp):
                os.remove(fp)
            continue
        with open(fp, "wb") as f:
            f.write(v[0])
        os.chmod(fp, v[1])
    if not porcelain(repo):
        return "restored" if saved else "clean"
    # Whatever is left was made by the tests: the repository was clean before the mutation, so an
    # untracked file is theirs and a modified tracked file goes back to HEAD.
    how = "restored"
    rc, out = sh(["git", "ls-files", "--others", "--exclude-standard", "-z"], cwd)
    extra = [p for p in out.split("\0") if p]
    for p in extra:
        os.remove(os.path.join(cwd, p))
    rc, out = sh(["git", "diff", "--name-only", "-z", "HEAD"], cwd)
    mod = [p for p in out.split("\0") if p]
    if mod:
        rc, out2 = sh(["git", "checkout", "HEAD", "--"] + mod, cwd)
        if rc != 0:
            raise SystemExit("git checkout failed in %s: %s" % (repo, out2))
    if extra or mod:
        how += " (+%d left by the tests: %s)" % (len(extra) + len(mod), ", ".join((extra + mod)[:5]))
    if porcelain(repo):
        raise SystemExit("%s is not clean after restoring %s:\n%s" % (repo, mid, porcelain(repo)))
    return how


def apply(m):
    if m.get("patch"):
        rc, out = sh(["git", "apply", m["patch"]], os.path.join(ROOT, m["repo"]))
        if rc != 0:
            raise ValueError("git apply: " + out)
    for e in m.get("edits", []):
        path, old, new = e[0], e[1], e[2]
        n = e[3] if len(e) > 3 else 1
        fp = os.path.join(ROOT, m["repo"], path)
        if old is None:
            if os.path.exists(fp):
                raise ValueError("%s exists" % path)
            with open(fp, "w") as f:
                f.write(new)
            continue
        with open(fp) as f:
            s = f.read()
        if s.count(old) != n:
            raise ValueError("%s: %r occurs %d times, want %d" % (path, old[:80], s.count(old), n))
        with open(fp, "w") as f:
            f.write(s.replace(old, new))


def run_tests(m, tests):
    fails, logs, build = [], [], False
    for t in tests:
        pkg, rx = t[0], t[1]
        repo = m["repo"]
        if ":" in pkg:
            repo, pkg = pkg.split(":", 1)
        tags = t[2] if len(t) > 2 else ""
        if pkg.startswith("sh "):
            args = ["bash", "-c", pkg[3:]]
        else:
            args = ["go", "test", "-count=1"] + (["-tags", tags] if tags else []) + (["-run", rx] if rx else []) + [pkg]
        try:
            rc, out = sh(args, os.path.join(ROOT, repo), timeout=1200)
        except subprocess.TimeoutExpired:
            rc, out = 99, "TIMEOUT"
        logs.append("$ (%s) %s\n%s" % (repo, " ".join(args), out[-6000:]))
        if rc != 0:
            if not pkg.startswith("sh ") and "--- FAIL" not in out and (
                    "[build failed]" in out or "[setup failed]" in out or re.search(r"^# ", out, re.M)):
                build = True
            fails.append((repo + ":" + pkg, re.findall(r"--- FAIL: (\S+)", out) or ["(exit %d)" % rc]))
        elif "no tests to run" in out:
            logs.append("WARNING: no test matched " + rx)
    return fails, build, logs


def load(spec):
    g = {"ROOT": ROOT, "MUTATIONS": HERE}
    with open(spec) as f:
        exec(f.read(), g)
    return g["MUTS"]


def main(argv):
    check = argv[:1] == ["--check"]
    if check:
        argv = argv[1:]
    if not argv:
        sys.exit(__doc__)
    spec, ids = argv[0], argv[1:]
    os.makedirs(os.path.join(OUT, "logs"), exist_ok=True)
    res = {}
    if os.path.exists(RES):
        with open(RES) as f:
            res = json.load(f)
    for m in load(spec):
        if (ids and m["id"] not in ids) or (not ids and not check and m["id"] in res):
            continue
        for r in REPOS:
            if porcelain(r):
                sys.exit("%s is not clean before %s:\n%s" % (r, m["id"], porcelain(r)))
        t0 = time.time()
        rec = {"id": m["id"], "group": m["group"], "repo": m["repo"], "desc": m["desc"]}
        saved = None
        try:
            saved = backup(m)
            apply(m)
        except ValueError as e:
            restore(m["repo"], m["id"], saved)
            rec.update(verdict="APPLY-ERR", detail=str(e))
            print("%-12s APPLY-ERR %s" % (m["id"], e), flush=True)
            if not check:
                res[m["id"]] = rec
            continue
        if check:
            restore(m["repo"], m["id"], saved)
            print("%-12s applies" % m["id"], flush=True)
            continue
        try:
            fails, build, logs = run_tests(m, m["tests"])
            verdict = "BUILD-ERR" if build else ("KILLED" if fails else "SURVIVED")
            full = None
            if verdict == "SURVIVED" and m.get("full"):
                ff, _, fl = run_tests(m, [(p, "") for p in m["full"]])
                logs += fl
                full = "KILLED-by-other" if ff else "SURVIVED-full"
                fails = ff or fails
        finally:
            how = restore(m["repo"], m["id"], saved)
        rec.update(verdict=verdict, full=full, failing=fails, secs=round(time.time() - t0, 1), restore=how)
        with open(os.path.join(OUT, "logs", m["id"] + ".log"), "w") as f:
            f.write("\n\n".join(logs))
        res[m["id"]] = rec
        with open(RES, "w") as f:
            json.dump(res, f, indent=1, ensure_ascii=False)
        short = "; ".join("%s:%s" % (p.split("/")[-1], ",".join(n[:3])) for p, n in fails)
        print("%-12s %-9s %s %s (%ss)" % (m["id"], verdict, full or "", short[:240], rec["secs"]), flush=True)


if __name__ == "__main__":
    main(sys.argv[1:])
