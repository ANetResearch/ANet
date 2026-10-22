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


class CrlfTests(unittest.TestCase):
    def test_crlf(self):
        self.assertEqual(g.crlf("a\nb\r\nc\n"), "a\r\nb\r\nc\r\n")


# The tools of `anet mcp`, read from the sources of the server when they are
# present (the ANet repository), so that a tool added later is covered too.
_MCPSERV = Path(__file__).resolve().parents[2] / "internal" / "mcpserv"
_KNOWN_TOOLS = {"list_agents", "get_agent_card", "send_message", "wait_task", "get_task", "list_tasks", "cancel_task",
                "submit_payment", "reject_payment", "reply_task", "inbound_pending", "get_balance", "audit", "node_status"}


def mcp_tool_names():
    import re
    names = set(_KNOWN_TOOLS)
    if _MCPSERV.is_dir():
        for f in _MCPSERV.glob("*.go"):
            if not f.name.endswith("_test.go"):
                names.update(re.findall(r'(?m)^\s*Name:\s*"([a-z][a-z0-9_]*)",\s*$', f.read_text(encoding="utf-8")))
    # `guide` is the permanent entry point that the plugins name on purpose.
    names.discard("guide")
    return names


class SharedTextTests(unittest.TestCase):
    """The shared skills and commands are frozen: they hold nothing that changes with a release of anet."""

    def shared_texts(self):
        out = {}
        for p in sorted((g.COMMON / "skills").glob("*/SKILL.md")) + sorted((g.COMMON / "commands").glob("*.md")):
            out[p.relative_to(g.COMMON).as_posix()] = p.read_text(encoding="utf-8")
        self.assertEqual(set(out), {"skills/anet/SKILL.md", "skills/anet-setup/SKILL.md", "commands/anet.md", "commands/anet-status.md"})
        return out

    def rendered_texts(self):
        res = g.build_all(None)
        for key, ctx in res.contexts.items():
            for rel, f in ctx.files.items():
                if rel.endswith((".md", ".toml")) and ("skills/" in rel or "commands/" in rel):
                    yield f"{key}/{rel}", f.data.decode("utf-8")

    def check_frozen(self, name, text):
        import re
        tools = mcp_tool_names()
        self.assertGreater(len(tools), 5)
        for tool in tools:
            self.assertNotRegex(text, r"\b" + re.escape(tool) + r"\b", f"{name} names the tool {tool}")
        self.assertNotRegex(text, r"(?i)\b\d+\s+(anet\s+)?(mcp\s+)?tools\b", f"{name} counts tools")
        self.assertNotRegex(text, r"\b\d+\.\d+(\.\d+)?\b", f"{name} holds a version number")
        self.assertNotRegex(text, r"\bv\d", f"{name} holds a version number")
        # Steps of a setup and decisions of the operator are described by the installed guide.
        for word in ("hub-register", "peers allow", "inbound policy", "spending limit", "payments.", "x402"):
            self.assertNotIn(word, text, f"{name} describes {word}")

    def test_shared_sources_are_frozen(self):
        for name, text in self.shared_texts().items():
            self.check_frozen(name, text)

    def test_every_generated_skill_and_command_is_frozen(self):
        n = 0
        for name, text in self.rendered_texts():
            self.check_frozen(name, text)
            n += 1
        self.assertGreater(n, 20)

    def test_the_skills_defer_to_the_guide(self):
        texts = self.shared_texts()
        anet = texts["skills/anet/SKILL.md"]
        fields, body = g.split_frontmatter(anet)
        for word in ("@anet", "anet", "AgentNetwork", "agents", "AID"):
            self.assertIn(word, fields["description"])
        for needle in ("tool `guide`", "<runner> guide", "@anet", "~/.anet", "belong to the user", "untrusted input",
                       "{{RUNNER_FROM_SKILL}}", "{{RUNNER_CMD_FROM_SKILL}}", "the guide applies"):
            self.assertIn(needle, body)
        setup = texts["skills/anet-setup/SKILL.md"]
        for needle in ("--resolve", "<runner> guide setup", "{{INSTALLER_FROM_SKILL}}", "{{INSTALLER_CMD_FROM_SKILL}}",
                       "consent to install anet", "ANET_PLUGIN_NO_INSTALL=1", "{{RUNNER_CMD_FROM_SKILL}}"):
            self.assertIn(needle, setup)
        self.assertIn("topic `status`", texts["commands/anet-status.md"])
        self.assertIn("<runner> guide status", texts["commands/anet-status.md"])
        entry = texts["commands/anet.md"]
        for needle in ("tool\n`guide`", "<runner> guide", "@anet\nrequests", "Request: {{ARGUMENTS}}", "If the request is empty"):
            self.assertIn(needle, entry)


class WindowsFileTests(unittest.TestCase):
    """The Windows launchers: CRLF, no setting on a command line of cmd.exe, PowerShell of the system only."""

    def cmd_files(self):
        res = g.build_all(None)
        for key, ctx in res.contexts.items():
            for rel, f in ctx.files.items():
                if rel.endswith(".cmd"):
                    yield f"{key}/{rel}", f.data

    def test_every_target_ships_the_windows_runner(self):
        res = g.build_all(None)
        for key, ctx in res.contexts.items():
            names = {rel.rsplit("/", 1)[-1] for rel in ctx.files}
            for name in ("anet-run.cmd", "anet-run.ps1", "anet-mcp.cmd", "anet-install.cmd", "anet-run.sh", "anet-mcp.sh", "anet-mcp"):
                self.assertIn(name, names, f"{key} lacks {name}")
            for rel, f in ctx.files.items():
                if rel.endswith("/anet-mcp") or rel.endswith(".sh"):
                    self.assertTrue(f.executable, f"{key}/{rel} is not executable")

    def test_cmd_files_are_crlf_and_ascii(self):
        n = 0
        for name, data in self.cmd_files():
            n += 1
            data.decode("ascii")
            self.assertNotIn(b"\n", data.replace(b"\r\n", b""), name)
            self.assertTrue(data.startswith(b"@echo off\r\n"), name)
        self.assertGreater(n, 30)

    def test_no_setting_is_expanded_on_a_command_line(self):
        import re
        allowed = {"SystemRoot", "ERRORLEVEL", "ANET_RUN_EXE", "ANET_RUN_ID"}
        for name, data in self.cmd_files():
            text = data.decode("ascii")
            for line in text.split("\r\n"):
                if line.lower().startswith("rem") or not line.strip():
                    continue
                for var in re.findall(r"%([A-Za-z_][A-Za-z0-9_]*)%", line):
                    self.assertIn(var, allowed, f"{name}: %{var}% in {line!r}")
                self.assertNotIn("!", line, f"{name}: delayed expansion in {line!r}")
                # The selected executable and identity are only ever used inside double quotes,
                # and the identity is validated by anet-run.ps1.
                for var in ("%ANET_RUN_EXE%", "%ANET_RUN_ID%"):
                    for m in re.finditer(re.escape(var), line):
                        self.assertEqual(line[:m.start()].count('"') % 2, 1, f"{name}: {var} outside quotes in {line!r}")
                        self.assertEqual(line[m.end():m.end() + 1], '"', f"{name}: {line!r}")
                if "powershell.exe" in line:
                    self.assertIn(r"%SystemRoot%\System32\WindowsPowerShell\v1.0\powershell.exe", line, name)
                    self.assertRegex(line, r"0\^?<NUL", f"{name}: PowerShell must not read the input of the session")
                    self.assertIn("-NoProfile", line)

    def test_the_powershell_runner_is_ascii_and_reads_no_argument(self):
        res = g.build_all(None)
        for key, ctx in res.contexts.items():
            for rel, f in ctx.files.items():
                if rel.endswith("anet-run.ps1"):
                    text = f.data.decode("ascii")
                    self.assertNotIn("$args", text, f"{key}/{rel}")
                    self.assertNotRegex(text, r"Invoke-Expression|iex |Start-Process", f"{key}/{rel}")
                    self.assertNotIn("{{", text, f"{key}/{rel}")


class AutoInstallStructureTests(unittest.TestCase):
    """Only the MCP launcher installs anet, before the runner starts, with no input and its output on stderr."""

    def test_the_posix_launchers_install_before_the_runner(self):
        res = g.build_all(None)
        n = 0
        for key, ctx in res.contexts.items():
            for rel, f in ctx.files.items():
                text = f.data.decode("utf-8") if rel.endswith((".sh", ".cmd", ".ps1")) else ""
                if rel.endswith("bin/anet-mcp.sh"):
                    n += 1
                    lines = [line.strip() for line in text.splitlines()]
                    step = [i for i, line in enumerate(lines) if "anet-install.sh\" --mcp" in line]
                    run = [i for i, line in enumerate(lines) if '"$runner"' in line or "anet-run.sh\" mcp" in line]
                    self.assertEqual(len(step), 1, f"{key}/{rel}")
                    self.assertTrue(lines[step[0]].endswith("--mcp </dev/null >&2"), f"{key}/{rel}: {lines[step[0]]}")
                    self.assertLess(step[0], min(run), f"{key}/{rel}: the installation comes after the runner")
                elif rel.endswith("bin/anet-run.sh") or rel.endswith("bin/anet-run.cmd"):
                    self.assertNotIn("anet-install", text, f"{key}/{rel}")
                if rel.endswith("bin/anet-mcp.cmd"):
                    self.assertIn('set "ANET_RUN_INSTALL=missing"', text, f"{key}/{rel}")
                if rel.endswith("bin/anet-run.cmd"):
                    self.assertIn('if not "%~1"=="mcp" set "ANET_RUN_INSTALL="', text, f"{key}/{rel}")
                    # Cleared before anet runs, so that nothing below anet sees it.
                    self.assertLess(text.index('set "ANET_RUN_INSTALL="\r\nif defined ANET_RUN_ID'), text.index('"%ANET_RUN_EXE%" %*'))
                if rel.endswith("bin/anet-run.ps1"):
                    self.assertIn("(Get-AnetEnv 'ANET_RUN_INSTALL') -eq 'missing'", text)
                    self.assertIn("$AnetPath -eq ''", text)
                    self.assertIn("Test-AnetInstallOptOut", text)
                    self.assertIn("RedirectStandardInput = $true", text)
                    self.assertIn("[IO.FileShare]::None", text)
        self.assertEqual(n, len(res.contexts))

    def test_the_installer_url_is_fixed(self):
        text = (g.COMMON / "bin" / "anet-install.sh").read_text(encoding="utf-8")
        self.assertEqual(text.count("https://agentnetwork.org.cn/install.sh -o"), 1)
        ps = (g.COMMON / "bin" / "anet-run.ps1").read_text(encoding="utf-8")
        self.assertIn("irm -UseBasicParsing https://agentnetwork.org.cn/install.ps1", ps)


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
                if rel.endswith((".md", ".sh", ".json", ".txt", ".cmd", ".ps1", ".toml", ".js", ".yml")):
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
