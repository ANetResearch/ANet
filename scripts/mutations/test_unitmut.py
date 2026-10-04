"""Tests of unitmut.py itself: listing, checking and running mutations against throw-away repositories.

Run from ANet: python3 -B scripts/mutations/test_unitmut.py (-B keeps __pycache__ out of the tree).

Each test builds a work-tree root with three small git repositories (ANetCore, ANet, ANetHub) in a
temporary directory and points UNITMUT_ROOT at it, so nothing here touches the real checkouts.
"""

import json
import os
import subprocess
import sys
import tempfile
import unittest

sys.dont_write_bytecode = True

HERE = os.path.dirname(os.path.abspath(__file__))
UNITMUT = os.path.join(HERE, "unitmut.py")
REPOS = ("ANetCore", "ANet", "ANetHub")


def git(cwd, *args):
    subprocess.run(["git", "-c", "user.name=unitmut-test", "-c", "user.email=unitmut-test@example.invalid",
                    "-c", "commit.gpgsign=false"] + list(args), cwd=cwd, check=True, capture_output=True)


class Root:
    """A work-tree root: three committed repositories, each holding a.txt and a pkg/ directory."""

    def __init__(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="unitmut-test-")
        self.path = self.tmp.name
        for r in REPOS:
            d = os.path.join(self.path, r)
            os.makedirs(os.path.join(d, "pkg"))
            with open(os.path.join(d, "a.txt"), "w") as f:
                f.write("alpha\nbeta\n")
            with open(os.path.join(d, "pkg", "b.txt"), "w") as f:
                f.write("gamma\n")
            git(d, "init", "-q")
            git(d, "add", "-A")
            git(d, "commit", "-q", "-m", "fixture")
        self.out = os.path.join(self.path, "out")

    def close(self):
        self.tmp.cleanup()

    def write_list(self, name, muts):
        """Write a mutation list (Python, as unitmut.py loads it) and return its path."""
        p = os.path.join(self.path, name)
        with open(p, "w") as f:
            f.write("MUTS = " + repr(muts) + "\n")
        return p

    def run(self, *args):
        env = dict(os.environ, UNITMUT_ROOT=self.path, UNITMUT_OUT=self.out)
        env.pop("GOWORK", None)
        p = subprocess.run([sys.executable, "-B", UNITMUT] + list(args), env=env, capture_output=True, text=True,
                           timeout=120)
        return p.returncode, p.stdout, p.stderr

    def clean(self):
        for r in REPOS:
            out = subprocess.run(["git", "status", "--porcelain"], cwd=os.path.join(self.path, r),
                                 capture_output=True, text=True, check=True).stdout
            if out.strip():
                return "%s: %s" % (r, out)
        return ""


def mut(mid, repo="ANetCore", path="a.txt", old="beta\n", new="BETA\n", tests=None):
    return dict(id=mid, group="T", repo=repo, desc="test mutation " + mid,
                edits=[(path, old, new)], tests=tests or [("sh true", "")])


def verdicts(stdout):
    """{id: first word after the id} for every result line (not the "== list" headings or the
    "list: N applies, M APPLY-ERR" summaries)."""
    out = {}
    for line in stdout.splitlines():
        parts = line.split()
        if len(parts) >= 2 and not line.startswith("==") and not parts[0].endswith(":"):
            out[parts[0]] = parts[1]
    return out


class Check(unittest.TestCase):
    def setUp(self):
        self.root = Root()

    def tearDown(self):
        self.root.close()

    def test_a_missing_target_file_is_an_apply_error_and_the_list_goes_on(self):
        lst = self.root.write_list("one.py", [
            mut("m-before"),
            mut("m-gone", path="pkg/deleted.go", old="x", new="y"),
            mut("m-after", repo="ANetHub"),
        ])
        rc, out, err = self.root.run("--check", lst)
        v = verdicts(out)
        self.assertEqual(v.get("m-before"), "applies", out + err)
        self.assertEqual(v.get("m-gone"), "APPLY-ERR", out + err)
        self.assertIn("pkg/deleted.go", out)
        self.assertEqual(v.get("m-after"), "applies", "the entries after a missing file were not checked:\n" + out + err)
        self.assertNotIn("Traceback", err)
        self.assertEqual(rc, 1, "an APPLY-ERR must fail --check")
        self.assertEqual(self.root.clean(), "")

    def test_several_lists_are_each_checked_and_reported(self):
        a = self.root.write_list("first.py", [mut("a-1"), mut("a-2", repo="ANet")])
        b = self.root.write_list("second.py", [mut("b-1", repo="ANetHub"),
                                               mut("b-bad", old="no such text\n")])
        rc, out, err = self.root.run("--check", a, b)
        v = verdicts(out)
        for mid in ("a-1", "a-2", "b-1"):
            self.assertEqual(v.get(mid), "applies", "%s missing from:\n%s%s" % (mid, out, err))
        self.assertEqual(v.get("b-bad"), "APPLY-ERR", out + err)
        # Each list is named, and summed up, so a reader can tell which list an entry came from.
        self.assertIn("== " + a, out)
        self.assertIn("== " + b, out)
        self.assertLess(out.index("== " + a), out.index("a-1"))
        self.assertLess(out.index("a-2"), out.index("== " + b))
        self.assertIn("first.py: 2 applies, 0 APPLY-ERR", out)
        self.assertIn("second.py: 1 applies, 1 APPLY-ERR", out)
        self.assertEqual(rc, 1)
        self.assertEqual(self.root.clean(), "")

    def test_a_clean_check_exits_zero(self):
        a = self.root.write_list("first.py", [mut("a-1")])
        b = self.root.write_list("second.py", [mut("b-1", repo="ANet")])
        rc, out, err = self.root.run("--check", a, b)
        self.assertEqual(rc, 0, out + err)
        self.assertEqual(verdicts(out), {"a-1": "applies", "b-1": "applies"})

    def test_a_key_test_package_that_does_not_exist_is_an_apply_error(self):
        # A deleted package would otherwise fail "go test" with a missing directory, which reads as a kill.
        lst = self.root.write_list("one.py", [
            mut("m-pkg", tests=[("ANetHub:./internal/deleted", "^TestX$")]),
            mut("m-full", tests=[("./pkg", "^TestX$")]),
        ])
        lst2 = self.root.write_list("two.py", [dict(mut("m-fullgone"), full=["ANet:./gone"])])
        rc, out, err = self.root.run("--check", lst, lst2)
        v = verdicts(out)
        self.assertEqual(v.get("m-pkg"), "APPLY-ERR", out + err)
        self.assertIn("ANetHub:./internal/deleted", out)
        self.assertEqual(v.get("m-full"), "applies", out + err)
        self.assertEqual(v.get("m-fullgone"), "APPLY-ERR", out + err)
        self.assertEqual(rc, 1)
        self.assertEqual(self.root.clean(), "")

    def test_an_unknown_id_is_reported(self):
        a = self.root.write_list("first.py", [mut("a-1")])
        b = self.root.write_list("second.py", [mut("b-1")])
        rc, out, err = self.root.run("--check", a, b, "b-1", "no-such-id")
        self.assertEqual(verdicts(out), {"b-1": "applies"}, out + err)
        self.assertIn("no-such-id", err)
        self.assertEqual(rc, 2)

    def test_a_list_that_does_not_exist_is_reported(self):
        rc, out, err = self.root.run("--check", os.path.join(self.root.path, "missing.py"))
        self.assertIn("missing.py", err)
        self.assertNotEqual(rc, 0)
        self.assertNotIn("Traceback", err)


class Run(unittest.TestCase):
    def setUp(self):
        self.root = Root()

    def tearDown(self):
        self.root.close()

    def test_mutations_of_several_lists_run_and_restore(self):
        # The key test greps the file: red while the mutation is applied, green otherwise.
        killed = mut("k-1", tests=[("sh ! grep -q BETA a.txt", "")])
        survived = mut("s-1", repo="ANet", tests=[("sh true", "")])
        gone = mut("g-1", repo="ANetHub", path="pkg/deleted.go", old="x", new="y")
        a = self.root.write_list("first.py", [killed, gone])
        b = self.root.write_list("second.py", [survived])
        rc, out, err = self.root.run(a, b)
        v = verdicts(out)
        self.assertEqual(v, {"k-1": "KILLED", "g-1": "APPLY-ERR", "s-1": "SURVIVED"}, out + err)
        self.assertEqual(self.root.clean(), "")
        with open(os.path.join(self.root.out, "results.json")) as f:
            res = json.load(f)
        self.assertEqual(sorted(res), ["g-1", "k-1", "s-1"])
        with open(os.path.join(self.root.path, "ANetCore", "a.txt")) as f:
            self.assertEqual(f.read(), "alpha\nbeta\n")


if __name__ == "__main__":
    unittest.main()
