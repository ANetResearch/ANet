#!/usr/bin/env python3
"""unitmut.py — unit-level mutations of the A2A-DESIGN §1 invariants and the §17 supplementary cases.

mutate.sh (beside this file) runs a patch through joint.sh; this runs a mutation through the Go tests
that carry the invariant: apply one small edit to one repository, run the named tests, record whether
they went red, restore the working tree. The lists are unit/si.py (SI-1..SI-10) and unit/supp.py (the
§17 supplementary rows), written for an internal note, and unit/fixwave.py (the round-5b red-team fixes
and an internal note item-Q33), written for an internal note

  unitmut.py unit/si.py                 every mutation of the list not yet recorded in OUT/results.json
  unitmut.py unit/si.py ID…             these, recorded or not
  unitmut.py --check unit/si.py …       apply and restore each edit without running tests (does the list
                                        still match the code?)

Several lists may be given (an argument ending in .py is a list, any other is an ID): each is run or
checked in turn under a "== <list>" heading, and --check ends each with "<list>: N applies, M APPLY-ERR".
--check exits 1 when any entry is an APPLY-ERR; an ID that is in none of the lists exits 2. An entry
whose edit names a file that does not exist (or creates one in a directory that does not), or whose key or
full test package is not a directory, is an APPLY-ERR and the list goes on.

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
            if not os.path.isdir(os.path.dirname(fp)):
                # The package the new file joins was deleted or moved: drift, as for a missing file.
                raise ValueError("%s: no directory %s in %s" % (path, os.path.dirname(path) or ".", m["repo"]))
            with open(fp, "w") as f:
                f.write(new)
            continue
        if not os.path.isfile(fp):
            # Deleted or moved by a later change: the entry is to be pointed at the code's new place,
            # or retired, and the rest of the list still has to be checked.
            raise ValueError("%s: no such file in %s" % (path, m["repo"]))
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


def missing_packages(m):
    """The key and full test packages of m that are not directories ("Repo:./path" or relative to its repo).

    go test on a directory that is gone fails without a "--- FAIL" or a build marker, which would read as
    KILLED; a package deleted by a later change is drift in the list, like a file that is gone."""
    out = []
    for pkg in [t[0] for t in m["tests"]] + list(m.get("full", [])):
        repo, p = m["repo"], pkg
        if ":" in p:
            repo, p = p.split(":", 1)
        if p.startswith("sh "):
            continue
        d = p[:-4] if p.endswith("/...") else p
        if not os.path.isdir(os.path.join(ROOT, repo, d)):
            out.append(repo + ":" + p)
    return out


def main(argv):
    check = argv[:1] == ["--check"]
    if check:
        argv = argv[1:]
    specs = [a for a in argv if a.endswith(".py")]
    ids = [a for a in argv if not a.endswith(".py")]
    if not specs:
        sys.exit(__doc__)
    for spec in specs:
        if not os.path.isfile(spec):
            sys.exit("unitmut: no mutation list %s" % spec)
    lists = [(spec, load(spec)) for spec in specs]
    unknown = [i for i in ids if not any(m["id"] == i for _, muts in lists for m in muts)]
    os.makedirs(os.path.join(OUT, "logs"), exist_ok=True)
    res = {}
    if os.path.exists(RES):
        with open(RES) as f:
            res = json.load(f)
    apply_errs = 0
    for spec, muts in lists:
        print("== %s" % spec, flush=True)
        n_ok, n_err = run_list(muts, ids, check, res)
        apply_errs += n_err
        if check:
            print("%s: %d applies, %d APPLY-ERR" % (spec, n_ok, n_err), flush=True)
    if unknown:
        print("unitmut: no mutation named %s in %s" % (", ".join(unknown), ", ".join(specs)), file=sys.stderr)
        sys.exit(2)
    if check and apply_errs:
        sys.exit(1)


def run_list(muts, ids, check, res):
    """Check or run the mutations of one list; returns (applied, APPLY-ERR) counts."""
    n_ok = n_err = 0
    for m in muts:
        if (ids and m["id"] not in ids) or (not ids and not check and m["id"] in res):
            continue
        for r in REPOS:
            if porcelain(r):
                sys.exit("%s is not clean before %s:\n%s" % (r, m["id"], porcelain(r)))
        t0 = time.time()
        rec = {"id": m["id"], "group": m["group"], "repo": m["repo"], "desc": m["desc"]}
        saved = None
        try:
            gone = missing_packages(m)
            if gone:
                raise ValueError("test package %s does not exist" % ", ".join(gone))
            saved = backup(m)
            apply(m)
        except (ValueError, OSError) as e:
            # OSError: whatever else stops an edit from being written (a path that is now a directory,
            # a file that cannot be read) is drift in the list too, and must not end the run.
            restore(m["repo"], m["id"], saved)
            rec.update(verdict="APPLY-ERR", detail=str(e))
            print("%-12s APPLY-ERR %s" % (m["id"], e), flush=True)
            n_err += 1
            if not check:
                res[m["id"]] = rec
                with open(RES, "w") as f:
                    json.dump(res, f, indent=1, ensure_ascii=False)
            continue
        n_ok += 1
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
    return n_ok, n_err


if __name__ == "__main__":
    main(sys.argv[1:])
