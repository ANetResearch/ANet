#!/usr/bin/env python3
"""Tests of the generator core: python3 deploy/plugins/test_generator.py"""

import sys
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parent))

import generator as g  # noqa: E402


class RenderTests(unittest.TestCase):
    def test_substitution_and_languages(self):
        v = {"A": "x", "B": {"en": "hello", "zh-CN": "你好"}}
        self.assertEqual(g.render("{{A}} {{B}}\n", v), "x hello\n")
        self.assertEqual(g.render("{{A}} {{B}}\n", v, "zh-CN"), "x 你好\n")

    def test_values_are_not_scanned_again(self):
        self.assertEqual(g.render("{{A}}\n", {"A": "{{B}}", "B": "no"}), "{{B}}\n")

    def test_conditionals_nest(self):
        t = "a\n{{#if X}}\nb\n{{#if Y}}\nc\n{{#else}}\nd\n{{#end}}\ne\n{{#else}}\nf\n{{#end}}\ng\n"
        self.assertEqual(g.render(t, {"X": True, "Y": True}), "a\nb\nc\ne\ng\n")
        self.assertEqual(g.render(t, {"X": True, "Y": False}), "a\nb\nd\ne\ng\n")
        self.assertEqual(g.render(t, {"X": False, "Y": True}), "a\nf\ng\n")
        self.assertEqual(g.render(t, {"X": False, "Y": False}), "a\nf\ng\n")

    def test_errors(self):
        for text, v in (("{{#if X}}\n{{#else}}\n{{#else}}\n{{#end}}\n", {"X": True}), ("{{#else X}}\n", {"X": True}),
                        ("{{#end X}}\n", {"X": True}), ("{{#unknown}}\n", {}), ("{{NOPE}}\n", {}), ("{{#if NOPE}}\n{{#end}}\n", {}), ("{{#if X}}\n", {"X": 1}),
                        ("{{#end}}\n", {}), ("{{#else}}\n", {}), ("{{X}}\n", {"X": True}),
                        ("{{X}}\n", {"X": {"en": "a"}})):
            with self.assertRaises(g.BuildError, msg=text):
                g.render(text, v, "zh-CN" if isinstance(v.get("X"), dict) else "en")

    def test_unknown_variable_in_a_hidden_block_is_still_an_error_only_in_conditions(self):
        # a substitution in an inactive branch is not evaluated; a condition always is
        self.assertEqual(g.render("{{#if X}}\n{{NOPE}}\n{{#end}}\n", {"X": False}), "")


class HelperTests(unittest.TestCase):
    def test_frontmatter(self):
        f, body = g.split_frontmatter('---\nname: a\ndescription: "b: c"\n---\n\nBody\n')
        self.assertEqual(f, {"name": "a", "description": "b: c"})
        self.assertEqual(body, "Body\n")
        self.assertEqual(g.split_frontmatter("plain\n"), ({}, "plain\n"))

    def test_archives_are_reproducible_and_sorted_by_component(self):
        files = {"skills/anet-setup/x": g.File("1"), "skills/anet/x": g.File("2"), "bin/run": g.File("3", True)}
        z1, z2 = g.zip_bytes(files, "p"), g.zip_bytes(dict(reversed(list(files.items()))), "p")
        self.assertEqual(z1, z2)
        import io
        import zipfile
        names = zipfile.ZipFile(io.BytesIO(z1)).namelist()
        self.assertEqual(names, ["p/bin/run", "p/skills/anet/x", "p/skills/anet-setup/x"])
        self.assertEqual(g.tar_gz_bytes(files, "p"), g.tar_gz_bytes(dict(reversed(list(files.items()))), "p"))

    def test_collisions(self):
        g._check_collisions({"a/b": 1, "c": 2})
        with self.assertRaises(g.BuildError):
            g._check_collisions({"a": 1, "a/b": 2})

    def test_output_paths(self):
        for bad in ("", "/a", "../a", "a/../b", "a/", "a\\b", "x/.git/y"):
            with self.assertRaises(g.BuildError, msg=bad):
                g._check_path(bad)


class MarkerTests(unittest.TestCase):
    def test_leak_between_targets_is_detected(self):
        a = SimpleNamespace(files={"x": g.File("mentions BETA_ROOT")}, root_files={})
        b = SimpleNamespace(files={"y": g.File("clean")}, root_files={})
        res = SimpleNamespace(contexts={"a": a, "b": b})
        mods = {"a": SimpleNamespace(MARKERS=["ALPHA_ROOT"]), "b": SimpleNamespace(MARKERS=["BETA_ROOT"])}
        with self.assertRaises(g.BuildError):
            g.check_markers(res, mods)
        a.files["x"] = g.File("own ALPHA_ROOT only")
        g.check_markers(res, mods)


class BuildTests(unittest.TestCase):
    def test_every_target_renders_without_leftovers(self):
        res = g.build_all(None)
        for key, ctx in res.contexts.items():
            for rel, f in ctx.files.items():
                if rel.endswith((".md", ".sh", ".json", ".txt")):
                    text = f.data.decode("utf-8")
                    self.assertNotRegex(text, r"\{\{#?[A-Z/]", f"{key}/{rel} has an unrendered tag")

    def test_zcode_and_example_do_not_leak_into_each_other(self):
        res = g.build_all(["zcode", "_example"])
        self.assertEqual(set(res.contexts), {"zcode", "_example"})

    def test_write_refuses_to_replace_without_force(self):
        res = g.build_all(["zcode"])
        with tempfile.TemporaryDirectory() as d:
            g.write_tree(res.tree(), d, owned_dirs=["zcode"])
            with self.assertRaises(g.BuildError):
                g.write_tree(res.tree(), d, owned_dirs=["zcode"])
            g.write_tree(res.tree(), d, owned_dirs=["zcode"], force=True)
            self.assertEqual(g.compare_tree(res.tree(), d, ["zcode"]), [])
            (Path(d) / "zcode" / "stale.txt").write_text("x")
            self.assertEqual(len(g.compare_tree(res.tree(), d, ["zcode"])), 1)

    def test_a_hard_linked_file_is_replaced_not_modified(self):
        import os
        with tempfile.TemporaryDirectory() as d:
            root_file = Path(d) / "README.md"
            other = Path(d) / "other-name"
            root_file.write_bytes(b"old")
            os.link(root_file, other)
            # A root-level file that shares its inode with another name must not change the other name.
            g.write_tree({"README.md": g.File("new")}, d, force=True)
            self.assertEqual(other.read_bytes(), b"old")
            self.assertEqual(root_file.read_bytes(), b"new")

    def test_a_dangling_link_is_a_clash_and_a_linked_destination_is_refused(self):
        import os
        with tempfile.TemporaryDirectory() as d:
            os.symlink(Path(d) / "nowhere", Path(d) / "README.md")
            with self.assertRaises(g.BuildError):
                g.write_tree({"README.md": g.File("new")}, d)
            g.write_tree({"README.md": g.File("new")}, d, force=True)
            self.assertFalse((Path(d) / "README.md").is_symlink())
            link = Path(d) / "out"
            os.symlink(Path(d) / "nowhere", link)
            with self.assertRaises(g.BuildError):
                g.write_tree({"a": g.File("x")}, link)
            self.assertFalse((Path(d) / "nowhere").exists())

    def test_a_site_base_must_be_https(self):
        ctx = g.build_all(["zcode"]).contexts["zcode"]
        with self.assertRaises(g.BuildError):
            g.Site(ctx, "http://example.org/zcode")
        self.assertEqual(g.Site(ctx, "https://example.org/zcode/").base, "https://example.org/zcode")

    def test_a_subset_build_does_not_import_the_other_targets(self):
        from unittest import mock
        with tempfile.TemporaryDirectory() as d:
            targets = Path(d)
            (targets / "good.py").write_text((g.TARGETS / "_example.py").read_text(encoding="utf-8"), encoding="utf-8")
            (targets / "bad.py").write_text("raise RuntimeError('a defect in another adapter')\n", encoding="utf-8")
            with mock.patch.object(g, "TARGETS", targets):
                self.assertEqual(list(g.discover(["good"])), ["good"])
                with self.assertRaises(RuntimeError):
                    g.discover()
                with self.assertRaises(g.BuildError) as raised:
                    g.discover(["nothing"])
                self.assertIn("available: bad, good", str(raised.exception))


class CommandLineTests(unittest.TestCase):
    def run_build(self, *args):
        import subprocess
        return subprocess.run([sys.executable, "-I", str(Path(__file__).resolve().parent / "build.py"), *args],
                              capture_output=True, text=True)

    def test_prune_is_validated_before_anything_is_written(self):
        with tempfile.TemporaryDirectory() as d:
            r = self.run_build("--out", str(Path(d) / "out"), "--target", "zcode", "--prune")
            self.assertNotEqual(r.returncode, 0)
            self.assertIn("--prune needs a full build", r.stderr)
            self.assertFalse((Path(d) / "out").exists(), "nothing is written for an invalid combination")
            r = self.run_build("--list", "--prune")
            self.assertNotEqual(r.returncode, 0)
            self.assertIn("--prune needs --out", r.stderr)


if __name__ == "__main__":
    unittest.main()
