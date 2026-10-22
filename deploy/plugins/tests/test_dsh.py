#!/usr/bin/env python3
"""Tests of the dsh target: python3 deploy/plugins/tests/test_dsh.py

They build the target into memory and check the bundle manifest of DeepSeek
Harness (DSH), the patch that mounts the MCP client, the skill files, the
wording that the target rewrites, the plugin market index of --site, and the
behaviour of the runner. The JavaScript of the bundle is checked with Node.js
(tests/dsh_check.mjs) when `node` is on PATH or named by ANET_NODE; the check
imports nothing of DSH. The runner tests need `sh`.
"""

import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

sys.dont_write_bytecode = True
PLUGINS = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(PLUGINS))

import generator as g  # noqa: E402
sys.path.insert(0, str(Path(__file__).resolve().parent))
import testguard  # noqa: E402,F401  (no test reaches the real anet or the installer)

SKILL_NAME = re.compile(r"^[a-z0-9]+(-[a-z0-9]+)*$")
COMMIT = "0123456789abcdef0123456789abcdef01234567"


def build():
    res = g.build_all(["dsh"])
    return res.contexts["dsh"]


def text(ctx, path):
    return ctx.files[path].data.decode("utf-8")


def write(ctx, dest):
    for rel, f in ctx.files.items():
        p = Path(dest) / rel
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_bytes(f.data)


class BundleTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.ctx = build()
        cls.pkg = json.loads(text(cls.ctx, "package.json"))

    def test_file_set(self):
        self.assertEqual(sorted(self.ctx.files), sorted([
            "LICENSE", "README.md", "README_CN.md", "bin/anet-install.cmd", "bin/anet-install.sh", "bin/anet-mcp", "bin/anet-mcp.cmd",
            "bin/anet-mcp.sh", "bin/anet-run.cmd", "bin/anet-run.ps1", "bin/anet-run.sh", "cordis.patch.yml",
            "index.js", "package.json", "skills.js", "skills/anet-setup/SKILL.md", "skills/anet-status/SKILL.md",
            "skills/anet/SKILL.md",
        ]))
        self.assertEqual(self.ctx.root_files, {})

    def test_manifest(self):
        pkg = self.pkg
        self.assertEqual(pkg["dsh"], {"bundle": {"patch": "./cordis.patch.yml"}})
        self.assertIn("cordis.patch.yml", self.ctx.files)
        self.assertEqual(pkg["type"], "module")
        self.assertIn(pkg["main"], self.ctx.files)
        self.assertEqual(pkg["version"], self.ctx.meta["version"])
        self.assertEqual(pkg["license"], "Apache-2.0")
        self.assertIn("dsh-plugin", pkg["keywords"])
        self.assertEqual(pkg["engines"], {"dsh": ">=0.2.0-0 <0.3.0"})
        self.assertNotIn("scripts", pkg)  # no prepare or build script: pnpm asks for no build approval
        self.assertNotIn("dependencies", pkg)
        # every entry of "files" exists in the bundle, and every file is covered (README and LICENSE are always packed)
        for entry in pkg["files"]:
            self.assertTrue(any(p == entry or p.startswith(entry + "/") for p in self.ctx.files), entry)
        for path in self.ctx.files:
            self.assertTrue(any(path == e or path.startswith(e + "/") for e in pkg["files"]) or path == "package.json", path)

    def test_patch_rows(self):
        patch = text(self.ctx, "cordis.patch.yml")
        self.assertNotIn("__", patch)
        self.assertIn("- insert:", patch)
        self.assertIn("name: dsh-anet\n", patch)  # the package name
        self.assertEqual(self.pkg["name"], "dsh-anet")
        self.assertIn("name: ./skills.js", patch)
        self.assertIn("name: '@deepseek-ai/dsh-mcp-client'", patch)
        self.assertIn("serverName: anet\n", patch)
        self.assertRegex("anet", r"^[A-Za-z0-9_-]{1,32}$")
        self.assertIn("transport: stdio", patch)
        # bin/anet-mcp on Linux and macOS (pnpm keeps the mode), bin/anet-mcp.cmd on Windows (PATHEXT).
        self.assertIn("command: !!js \"ctx.anetPlugin.root + '/bin/anet-mcp'\"\n", patch)
        self.assertIn("args: []", patch)
        self.assertTrue(self.ctx.files["bin/anet-mcp"].executable)
        self.assertIn(f"toolCallTimeoutMs: {self.ctx.meta['mcp']['timeout_ms']}", patch)
        for s in self.ctx.meta["settings"]:
            self.assertRegex(patch, rf"{s['env']}: !!js \"ctx\.anetPlugin\.settings\.{s['key']}\"")
        # the MCP row waits for the service that index.js provides
        self.assertIn("inject: [anetPlugin]", patch)
        self.assertIn("ctx.provide('anetPlugin'", text(self.ctx, "index.js"))
        # rows have unique ids
        ids = re.findall(r"^\s+- id: (\S+)$", patch, re.M)
        self.assertEqual(len(ids), len(set(ids)))
        self.assertEqual(ids, ["anet-plugin", "anet-skills", "anet-mcp"])

    def test_settings_match_the_module(self):
        index = text(self.ctx, "index.js")
        for s in self.ctx.meta["settings"]:
            self.assertIn(f"['{s['key']}', '{s['env']}']", index)

    def test_skills(self):
        names = set()
        for path in self.ctx.files:
            m = re.match(r"^skills/([^/]+)/SKILL\.md$", path)
            if not m:
                continue
            fields, body = g.split_frontmatter(text(self.ctx, path))
            self.assertEqual(fields.get("name"), m.group(1), path)
            self.assertRegex(fields["name"], SKILL_NAME)
            self.assertTrue(fields.get("description"), path)
            self.assertNotIn("\n", fields["description"])
            self.assertTrue(body.strip())
            names.add(fields["name"])
        self.assertEqual(names, {"anet", "anet-setup", "anet-status"})

    def test_root_token_only_inside_double_quoted_runner_paths(self):
        token = "${DSH_BUNDLE_DIR}"
        for path, f in self.ctx.files.items():
            if not path.startswith("skills/") and path not in ("index.js", "skills.js"):
                self.assertNotIn(token, f.data.decode("utf-8"), path)
                continue
            if path in ("index.js", "skills.js"):
                continue
            body = f.data.decode("utf-8")
            for line in body.splitlines():
                if token in line:
                    self.assertTrue(any(f'"{token}/bin/{name}"' in line for name in
                                        ("anet-run.sh", "anet-install.sh", "anet-run.cmd", "anet-install.cmd")), (path, line))
                    self.assertEqual(line.count(token), 1, path)

    def test_no_foreign_host_syntax(self):
        for path, f in self.ctx.files.items():
            if not path.startswith(("skills/", "README")):
                continue
            body = f.data.decode("utf-8")
            for foreign in ("$ARGUMENTS", "!`", "ZCODE_", "CLAUDE_", "CURSOR_", "/anet-status", "user_config"):
                self.assertNotIn(foreign, body, f"{path}: {foreign}")

    def test_status_skill(self):
        body = text(self.ctx, "skills/anet-status/SKILL.md")
        fields, rest = g.split_frontmatter(body)
        self.assertIn("topic `status`", rest)
        self.assertIn("<runner> guide status", rest)
        self.assertIn("DSH_HOME", rest)  # the agent must not read the DSH configuration
        self.assertIn("anet-setup", rest)
        self.assertIn("Use when", fields["description"])

    def test_setup_skill_keeps_the_rules(self):
        body = text(self.ctx, "skills/anet-setup/SKILL.md")
        self.assertIn("bin/anet-install.sh", body)
        self.assertNotIn("$(", body)
        self.assertIn("bin/anet-install.cmd", body)
        self.assertIn("not run them on the user's behalf", body)
        self.assertIn("Never read, print or copy the files of the anet data directory (`~/.anet`", body)
        self.assertIn("Never read or edit the DeepSeek Harness configuration", body)
        self.assertIn("Windows", body)
        self.assertNotIn("ZCode", body)

    def test_setup_skill_states_the_node_lifetime(self):
        # DSH ends the processes of its bash tool with the session, so the text
        # must not promise that the node keeps running and must hand over `anet up`.
        body = text(self.ctx, "skills/anet-setup/SKILL.md")
        self.assertNotIn("keeps running after this session", body)
        self.assertIn("ends every process that its shell tool started when the session ends", body)
        self.assertIn("to start the node again in a terminal of their own", body)
        self.assertLess(body.index("to start the node again in a terminal of their own"), body.index("start a new DeepSeek\nHarness session"))
        for name in ("README.md", "README_CN.md"):
            readme = text(self.ctx, name)
            self.assertGreaterEqual(readme.count("`anet up`"), 2, name)

    def test_readmes(self):
        en, cn = text(self.ctx, "README.md"), text(self.ctx, "README_CN.md")
        for body in (en, cn):
            self.assertIn("`anet-status`", body)
            self.assertIn("dsh plugin --profile <profile> add", body)
            self.assertIn("&path:/deploy/plugins/dist/dsh", body)
            self.assertIn("anet-plugin", body)
            self.assertIn("ANET_PLUGIN_BIN", body)
            self.assertNotIn("{{", body)
        self.assertIn("Linux, macOS or Windows", en)
        self.assertIn("Skill | `anet-status`", en)
        self.assertIn("Skill | `anet-status`", cn)
        # the installation and settings sections come from the target, once each
        self.assertEqual(en.count("## Installation"), 1)
        self.assertEqual(en.count("## Settings"), 1)
        self.assertEqual(cn.count("## 安装"), 1)
        self.assertEqual(cn.count("## 设置项"), 1)

    def test_public_content_rules(self):
        for path, f in self.ctx.files.items():
            body = f.data.decode("utf-8")
            self.assertNotRegex(body, r"(?<![0-9.])(10|192\.168|172\.(1[6-9]|2[0-9]|3[01]))\.[0-9]+\.[0-9]+\.[0-9]+", path)
            self.assertNotIn("/home/", body, path)
            self.assertNotRegex(body, r"/tmp/[A-Za-z0-9._-]+/", path)


class VersionedTexts(unittest.TestCase):
    def test_transform_fails_loudly_when_a_shared_text_changes(self):
        ctx = build()
        mod = ctx.module
        with self.assertRaises(ValueError):
            mod.transform(ctx, "README.md", "no requirements heading here", "en")
        with self.assertRaises(ValueError):
            mod.transform(ctx, "commands/anet-status.md", "unrelated", "en")
        self.assertEqual(mod.transform(ctx, "bin/anet-run.sh", "x", "en"), "x")


class SiteTests(unittest.TestCase):
    def site(self, env):
        res = g.build_all(["dsh"])
        with mock.patch.dict(os.environ, env, clear=False):
            for k in ("ANET_PLUGIN_COMMIT", "SOURCE_DATE_EPOCH"):
                if k not in env:
                    os.environ.pop(k, None)
            return g.build_site(res, "https://agentnetwork.org.cn")

    def test_nothing_without_the_inputs(self):
        self.assertEqual(self.site({}), {})

    def test_source_date_epoch_alone_is_not_an_error(self):
        # Build environments often export SOURCE_DATE_EPOCH; the index needs the commit as well.
        self.assertEqual(self.site({"SOURCE_DATE_EPOCH": "1790000000"}), {})

    def test_index(self):
        files = self.site({"ANET_PLUGIN_COMMIT": COMMIT, "SOURCE_DATE_EPOCH": "1790000000"})
        self.assertEqual(sorted(files), ["plugins/dsh/icons/dsh-anet.png", "plugins/dsh/index.json"])
        index = json.loads(files["plugins/dsh/index.json"].data)
        self.assertEqual(index["format"], 1)
        self.assertIn("en", index["title"])
        (plugin,) = index["plugins"]
        self.assertEqual(plugin["name"], "dsh-anet")
        self.assertEqual(plugin["icon"], "icons/dsh-anet.png")
        self.assertIn("en", plugin["title"])
        self.assertIn("en", plugin["description"])
        (version,) = plugin["versions"]
        self.assertEqual(version["version"], build().meta["version"])
        self.assertEqual(version["publishTimestamp"], 1790000000)
        self.assertEqual(version["source"], {"type": "git", "url": "https://github.com/ANetResearch/ANet.git", "commit": COMMIT,
                                             "path": "deploy/plugins/dist/dsh"})
        self.assertEqual(version["engines"], {"dsh": ">=0.2.0-0 <0.3.0"})

    def test_invalid_inputs(self):
        for env in ({"ANET_PLUGIN_COMMIT": "main", "SOURCE_DATE_EPOCH": "1"},
                    {"ANET_PLUGIN_COMMIT": COMMIT.upper(), "SOURCE_DATE_EPOCH": "1"},
                    {"ANET_PLUGIN_COMMIT": COMMIT, "SOURCE_DATE_EPOCH": "yesterday"},
                    {"ANET_PLUGIN_COMMIT": COMMIT}):
            with self.assertRaises(ValueError, msg=env):
                self.site(env)


@unittest.skipUnless(shutil.which("sh"), "sh is required")
class RunnerTests(unittest.TestCase):
    """bin/anet-run.sh with the environment that the plugin module sets."""

    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.TemporaryDirectory()
        cls.bundle = Path(cls.tmp.name) / "dsh"
        write(build(), cls.bundle)
        cls.fake = Path(cls.tmp.name) / "fake-anet"
        cls.fake.write_text('#!/bin/sh\nif [ "$1" = --version ]; then echo "anet 9.9.9"; exit 0; fi\necho "args: $*"\n')
        cls.fake.chmod(0o755)

    @classmethod
    def tearDownClass(cls):
        cls.tmp.cleanup()

    def run_runner(self, env_extra, *args):
        env = {"PATH": "/usr/bin:/bin", "HOME": self.tmp.name}
        env.update(env_extra)
        return subprocess.run(["sh", str(self.bundle / "bin" / "anet-run.sh"), *args], env=env, capture_output=True, text=True)

    def test_resolve_uses_the_settings_of_the_environment(self):
        r = self.run_runner({"ANET_PLUGIN_BIN": str(self.fake), "ANET_PLUGIN_IDENTITY": "work"}, "--resolve")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn(f"executable: {self.fake}", r.stdout)
        self.assertIn("version: anet 9.9.9", r.stdout)
        self.assertIn("identity: work", r.stdout)
        self.assertIn(f"runner: sh {self.bundle}/bin/anet-run.sh", r.stdout)

    def test_identity_is_passed_to_anet(self):
        r = self.run_runner({"ANET_PLUGIN_BIN": str(self.fake), "ANET_PLUGIN_IDENTITY": "work"}, "status")
        self.assertEqual(r.stdout.strip(), "args: --id work status")
        r = self.run_runner({"ANET_PLUGIN_BIN": str(self.fake)}, "status")
        self.assertEqual(r.stdout.strip(), "args: status")

    def test_invalid_path_is_an_error_and_no_other_copy_is_used(self):
        r = self.run_runner({"ANET_PLUGIN_BIN": "/nonexistent/anet"}, "status")
        self.assertEqual(r.returncode, 1)
        self.assertIn("not an executable file", r.stderr)
        r = self.run_runner({"ANET_PLUGIN_BIN": "/nonexistent/anet"}, "--resolve")
        self.assertEqual(r.returncode, 0)
        self.assertIn("executable: invalid setting anetPath=/nonexistent/anet", r.stdout)

    def test_mcp_launcher_reads_the_environment(self):
        r = subprocess.run(["sh", str(self.bundle / "bin" / "anet-mcp.sh")], capture_output=True, text=True,
                           env={"PATH": "/usr/bin:/bin", "HOME": self.tmp.name, "ANET_PLUGIN_BIN": str(self.fake),
                                "ANET_PLUGIN_IDENTITY": "work", "ANET_PLUGIN_SETTINGS": "ignored"})
        self.assertEqual(r.stdout.strip(), "args: --id work mcp")

    def test_runs_without_executable_bits(self):
        for path in (self.bundle / "bin").iterdir():
            path.chmod(0o644)
        r = self.run_runner({"ANET_PLUGIN_BIN": str(self.fake)}, "status")
        self.assertEqual(r.stdout.strip(), "args: status")


class NodeTests(unittest.TestCase):
    def test_javascript(self):
        node = os.environ.get("ANET_NODE") or shutil.which("node")
        if not node:
            self.skipTest("node is not available")
        with tempfile.TemporaryDirectory() as tmp:
            bundle = Path(tmp).resolve() / "dsh"
            write(build(), bundle)
            r = subprocess.run([node, str(Path(__file__).resolve().parent / "dsh_check.mjs"), str(bundle)], capture_output=True, text=True)
            self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
            self.assertIn("dsh_check: ok", r.stdout)


if __name__ == "__main__":
    unittest.main()
