#!/usr/bin/env python3
"""Tests of the opencode target: python3 deploy/plugins/tests/test_opencode.py

  * the generated package (package.json, entry points, shared files, wording);
  * the JavaScript of src/opencode/ with node:test (skipped when node is missing);
  * the shell behaviour of the generated runner and launcher with a fake anet;
  * optional: the real OpenCode CLI loads the plugin. Set OPENCODE_BIN to the
    opencode executable; the test uses its own HOME and XDG directories, and OpenCode
    may need network access the first time to install its own plugin dependencies.
"""

import json
import os
import re
import shutil
import stat
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

sys.dont_write_bytecode = True
PLUGINS = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(PLUGINS))

import generator as g  # noqa: E402


def build():
    tmp = Path(tempfile.mkdtemp(prefix="opencode-target-"))
    res = g.build_all(["opencode"])
    g.write_tree(res.tree(), tmp, force=True)
    return tmp, res


def find_node():
    node = shutil.which("node")
    if node:
        return node
    candidate = Path.home() / ".local/share/node24/current/bin/node"
    return str(candidate) if candidate.exists() else None


class Base(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.out, cls.res = build()
        cls.pkg = cls.out / "opencode"
        cls.meta = g.load_meta()

    @classmethod
    def tearDownClass(cls):
        shutil.rmtree(cls.out, ignore_errors=True)

    def text(self, rel):
        return (self.pkg / rel).read_text(encoding="utf-8")


class PackageTests(Base):
    def test_file_set(self):
        files = sorted(p.relative_to(self.pkg).as_posix() for p in self.pkg.rglob("*") if p.is_file())
        self.assertEqual(files, [
            "LICENSE", "README.md", "README_CN.md", "bin/anet-install.cmd", "bin/anet-install.sh", "bin/anet-mcp", "bin/anet-mcp.cmd",
            "bin/anet-mcp.sh", "bin/anet-run.cmd", "bin/anet-run.ps1", "bin/anet-run.sh", "commands/anet-status.md", "commands/anet.md",
            "lib.js", "package.json", "server.js", "skills/anet-setup/SKILL.md", "skills/anet/SKILL.md",
        ])

    def test_package_json(self):
        p = json.loads(self.text("package.json"))
        self.assertEqual(p["version"], self.meta["version"])
        self.assertEqual(p["type"], "module")
        self.assertEqual(p["license"], "Apache-2.0")
        self.assertRegex(p["name"], r"^@[a-z0-9-]+/[a-z0-9-]+$")
        # OpenCode accepts an entry from exports["./server"], "./tui" or main; this package has the server only.
        self.assertEqual(p["exports"], {"./server": "./server.js"})
        self.assertEqual(p["main"], "./server.js")
        self.assertRegex(p["engines"]["opencode"], r"^>=\d+\.\d+\.\d+$")
        for entry in p["files"]:
            self.assertTrue((self.pkg / entry).exists(), entry)
        for path in self.pkg.rglob("*"):
            if path.is_file():
                rel = path.relative_to(self.pkg).as_posix()
                self.assertTrue(rel == "package.json" or any(rel == f or rel.startswith(f + "/") for f in p["files"]), rel)
        self.assertEqual(p["repository"]["directory"], "deploy/plugins/dist/opencode")

    def test_placeholders_are_resolved(self):
        lib = self.text("lib.js")
        self.assertIn('export const SERVER_NAME = "anet"', lib)
        self.assertIn(f"const TIMEOUT_MS = {self.meta['mcp']['timeout_ms']}", lib)
        for rel in ("lib.js", "server.js", "package.json", "README.md", "README_CN.md", "commands/anet-status.md",
                    "skills/anet-setup/SKILL.md", "bin/anet-run.sh"):
            text = self.text(rel)
            self.assertNotIn("{{", text, rel)
            self.assertNotIn("__SERVER_NAME__", text, rel)
            self.assertNotIn("__TIMEOUT_MS__", text, rel)

    def test_skills_and_command_use_the_root_variable_only(self):
        for rel in ("skills/anet-setup/SKILL.md", "skills/anet/SKILL.md", "commands/anet-status.md", "commands/anet.md"):
            text = self.text(rel)
            self.assertNotRegex(text, r"ZCODE|CLAUDE_|CURSOR_|CODEX_", rel)
            self.assertNotIn("../../bin", text, rel)
            self.assertIn('sh "${ANET_PLUGIN_ROOT}/bin/anet-run.sh"', text, rel)
            self.assertIn('"${ANET_PLUGIN_ROOT}/bin/anet-run.cmd"', text, rel)
        for rel in ("skills/anet-setup/SKILL.md", "skills/anet/SKILL.md"):
            self.assertIn("$env:ANET_PLUGIN_ROOT", self.text(rel), "the skills say how to write the variable in PowerShell")

    def test_setup_skill_rules_are_kept(self):
        text = self.text("skills/anet-setup/SKILL.md")
        self.assertIn("bin/anet-install.sh", text)
        self.assertIn("bin/anet-install.cmd", text)
        self.assertNotIn("$(", text)
        self.assertIn("Pass no options to the installer", text)
        self.assertIn("Never read, print or copy the files of the anet data directory (`~/.anet`", text)
        self.assertIn("not run them on the user's behalf", text)
        self.assertIn("Never read or edit the OpenCode configuration", text)
        self.assertIn("Windows", text)
        self.assertIn("restart OpenCode", text)
        self.assertNotIn("new OpenCode session", text)

    def test_skill_names_are_valid_for_opencode(self):
        for name in ("anet", "anet-setup"):
            fields, _ = g.split_frontmatter(self.text(f"skills/{name}/SKILL.md"))
            self.assertEqual(fields["name"], name)
            self.assertRegex(fields["name"], r"^[a-z0-9]+(-[a-z0-9]+)*$")
            self.assertLessEqual(len(fields["name"]), 64)
            self.assertTrue(fields["description"])

    def test_readmes(self):
        for rel in ("README.md", "README_CN.md"):
            text = self.text(rel)
            self.assertIn("@agentnetwork/opencode-anet", text, rel)
            self.assertIn("opencode plugin", text, rel)
            self.assertIn("anetPath", text, rel)
            self.assertNotIn("start a new session", text, rel)
            self.assertNotIn("新建会话", text, rel)
            self.assertNotIn("!", re.sub(r"!`[^`]*`", "", text.replace("![", "")), rel)
        self.assertIn("Windows", self.text("README.md"))
        self.assertIn("agents/opencode.html", self.text("README.md"))

    def test_public_content_rules(self):
        for path in self.pkg.rglob("*"):
            if not path.is_file():
                continue
            text = path.read_text(encoding="utf-8")
            rel = path.relative_to(self.pkg).as_posix()
            self.assertNotRegex(text, r"/home/|/Users/|/tmp/|\b10\.\d+\.\d+\.\d+\b|\b192\.168\.\d+\.\d+\b", rel)
            self.assertIsNone(re.search("[\U0001F300-\U0001FAFF☀-➿]", text), rel)

    def test_runner_is_the_shared_runner(self):
        self.assertIn('read_settings() {\n  printf \'%s\\n\' "${ANET_PLUGIN_BIN:-}" "${ANET_PLUGIN_IDENTITY:-}"\n}', self.text("bin/anet-run.sh"))
        self.assertIn("ANET_PLUGIN_SETTINGS=env", self.text("bin/anet-mcp.sh"))

    def test_no_site_output(self):
        # OpenCode installs from npm or from a directory, so the target publishes nothing on the website.
        self.assertEqual(g.build_site(self.res, "https://example.org"), {})
        self.assertEqual(self.res.root, {})


@unittest.skipUnless(find_node(), "node is not installed")
class NodeTests(Base):
    def test_node_suite(self):
        work = Path(tempfile.mkdtemp(prefix="opencode-node-"))
        try:
            shutil.copy(PLUGINS / "src/opencode/test.mjs", work / "test.mjs")
            env = dict(os.environ, PLUGIN_DIR=str(self.pkg))
            r = subprocess.run([find_node(), "--test", str(work / "test.mjs")], env=env, capture_output=True, text=True, timeout=120)
            self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
            # The summary line is "# fail 0" (TAP) or "ℹ fail 0" (spec reporter), depending on the Node version.
            self.assertRegex(r.stdout, r"(?m)^\S+ fail 0$")
        finally:
            shutil.rmtree(work, ignore_errors=True)


class ShellTests(Base):
    """The generated runner and launcher with a fake anet that prints its arguments."""

    def setUp(self):
        self.work = Path(tempfile.mkdtemp(prefix="opencode-shell-"))
        self.home = self.work / "home"
        self.home.mkdir()
        self.fake = self.work / "fake anet"
        self.fake.write_text('#!/bin/sh\n[ "${1:-}" = --version ] && { echo "anet fake 0.0.0"; exit 0; }\n'
                             'printf "args:"; for a in "$@"; do printf " [%s]" "$a"; done; echo\n')
        self.fake.chmod(self.fake.stat().st_mode | stat.S_IXUSR)

    def tearDown(self):
        shutil.rmtree(self.work, ignore_errors=True)

    def run_sh(self, script, *args, **env):
        e = {"PATH": "/usr/bin:/bin", "HOME": str(self.home)}
        e.update({k: v for k, v in env.items() if v is not None})
        return subprocess.run(["sh", str(self.pkg / "bin" / script), *args], env=e, capture_output=True, text=True, timeout=60)

    def test_launcher_passes_the_settings_of_the_plugin(self):
        r = self.run_sh("anet-mcp.sh", ANET_PLUGIN_BIN=str(self.fake), ANET_PLUGIN_IDENTITY="work")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(r.stdout.splitlines()[0], "args: [--id] [work] [mcp]")

    def test_launcher_without_settings_and_without_anet(self):
        r = self.run_sh("anet-mcp.sh")
        self.assertEqual(r.returncode, 1)
        self.assertIn("anet is not installed", r.stderr)

    def test_invalid_anet_path_is_an_error_and_no_other_copy_is_used(self):
        other = self.home / ".local/bin"
        other.mkdir(parents=True)
        shutil.copy(self.fake, other / "anet")
        (other / "anet").chmod(0o755)
        r = self.run_sh("anet-mcp.sh", ANET_PLUGIN_BIN=str(self.work / "missing"))
        self.assertEqual(r.returncode, 1)
        self.assertIn("not an executable file", r.stderr)
        self.assertEqual(r.stdout, "")

    def test_runner_reads_the_settings_that_the_shell_hook_sets(self):
        # What the "shell.env" hook provides to the shell command of the agent, as the skill runs it.
        env = {"ANET_PLUGIN_ROOT": str(self.pkg), "ANET_PLUGIN_BIN": str(self.fake), "ANET_PLUGIN_IDENTITY": "work"}
        r = subprocess.run(["sh", "-c", 'sh "${ANET_PLUGIN_ROOT}/bin/anet-run.sh" --resolve'], env=dict(env, PATH="/usr/bin:/bin"),
                           capture_output=True, text=True, timeout=60, cwd=self.work)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn(f"executable: {self.fake}", r.stdout)
        self.assertIn("identity: work", r.stdout)
        self.assertIn("runner: sh " + str(self.pkg / "bin" / "anet-run.sh"), r.stdout)

    def test_the_launcher_runs_by_its_path(self):
        # The MCP entry names bin/anet-mcp itself (no sh): it is executable and starts anet mcp.
        r = subprocess.run([str(self.pkg / "bin" / "anet-mcp")], env={"PATH": "/usr/bin:/bin", "HOME": str(self.home),
                           "ANET_PLUGIN_BIN": str(self.fake), "ANET_PLUGIN_IDENTITY": ""}, capture_output=True, text=True, timeout=60)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn("mcp", r.stdout)


@unittest.skipUnless(os.environ.get("OPENCODE_BIN"), "OPENCODE_BIN is not set")
class OpenCodeCliTests(Base):
    """The real OpenCode loads the plugin from a directory and lists its components."""

    def test_debug_config_and_skills(self):
        work = Path(tempfile.mkdtemp(prefix="opencode-cli-"))
        try:
            home = work / "home"
            project = work / "project"
            project.mkdir()
            anet = work / "anet"
            anet.write_text("#!/bin/sh\nexit 0\n")
            anet.chmod(0o755)
            (project / "opencode.json").write_text(json.dumps({
                "$schema": "https://opencode.ai/config.json",
                "autoupdate": False,
                "plugin": [[str(self.pkg), {"anetPath": str(anet), "identity": "work"}]],
            }))
            env = dict(os.environ, HOME=str(home), XDG_CONFIG_HOME=str(home / "c"), XDG_DATA_HOME=str(home / "d"),
                       XDG_CACHE_HOME=str(home / "a"), XDG_STATE_HOME=str(home / "s"), OPENCODE_DISABLE_AUTOUPDATE="1",
                       OPENCODE_DISABLE_MODELS_FETCH="1", OPENCODE_DISABLE_LSP_DOWNLOAD="1")
            for name in ("ANET_PLUGIN_BIN", "ANET_PLUGIN_IDENTITY", "ANET_PLUGIN_ROOT"):
                env.pop(name, None)
            run = lambda *a: subprocess.run([os.environ["OPENCODE_BIN"], *a], cwd=project, env=env, capture_output=True, text=True, timeout=170)
            r = run("debug", "config")
            self.assertEqual(r.returncode, 0, r.stderr)
            cfg = json.loads(r.stdout)
            entry = cfg["mcp"]["anet"]
            self.assertEqual(entry["command"], [str(self.pkg / "bin" / "anet-mcp")])
            self.assertEqual(entry["environment"], {"ANET_PLUGIN_BIN": str(anet), "ANET_PLUGIN_IDENTITY": "work"})
            self.assertIn(str(self.pkg / "skills"), cfg["skills"]["paths"])
            self.assertIn(str(self.pkg / "bin" / "anet-run.sh"), cfg["command"]["anet-status"]["template"])
            r = run("debug", "skill")
            self.assertEqual(r.returncode, 0, r.stderr)
            found = {s["name"]: s for s in json.loads(r.stdout)}  # OpenCode lists its own skills as well
            for name in ("anet", "anet-setup"):
                self.assertIn(name, found)
                self.assertTrue(str(found[name].get("location", "")).startswith(str(self.pkg / "skills")), found[name])
        finally:
            shutil.rmtree(work, ignore_errors=True)


if __name__ == "__main__":
    unittest.main()
