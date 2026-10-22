#!/usr/bin/env python3
"""Tests of the codex target: python3 deploy/plugins/tests/test_codex.py

They build the target into memory and check the facts about the Codex plugin
format on which the target depends (verified with Codex CLI 0.163.0): the
manifest .codex-plugin/plugin.json, the .mcp.json without any variable, the
paths that skills use to reach the runner, the catalogue files, and the
behaviour of the launcher with the environment that .mcp.json forwards.

With the Codex CLI available, the generated tree is also installed into a
scratch CODEX_HOME and inspected by the CLI itself:

    ANET_CODEX_BIN=<path of codex> python3 deploy/plugins/tests/test_codex.py
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

sys.dont_write_bytecode = True
PLUGINS = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(PLUGINS))

import generator as g  # noqa: E402
sys.path.insert(0, str(Path(__file__).resolve().parent))
import testguard  # noqa: E402,F401  (no test reaches the real anet or the installer)


def build_codex():
    res = g.build_all(["codex"])
    return res.contexts["codex"], res


def text_of(ctx, path):
    return ctx.files[path].data.decode("utf-8")


def json_of(ctx, path):
    return json.loads(text_of(ctx, path))


def front_matter(text):
    fields, _ = g.split_frontmatter(text)
    return fields


class ManifestTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.ctx, cls.res = build_codex()
        cls.manifest = json_of(cls.ctx, ".codex-plugin/plugin.json")

    def test_files(self):
        self.assertEqual(
            sorted(self.ctx.files),
            sorted([
                ".codex-plugin/plugin.json", ".mcp.json", "LICENSE", "README.md", "README_CN.md", "assets/icon.png",
                "bin/anet-install.cmd", "bin/anet-install.sh", "bin/anet-mcp", "bin/anet-mcp.cmd", "bin/anet-mcp.sh",
                "bin/anet-run.cmd", "bin/anet-run.ps1", "bin/anet-run.sh", "skills/anet-setup/SKILL.md",
                "skills/anet-status/SKILL.md", "skills/anet/SKILL.md",
            ]),
        )

    def test_no_other_manifest_that_codex_would_prefer(self):
        # Codex uses a root plugin.json only with the Agent Plugins $schema, and falls back
        # to the Claude and Cursor manifests only when .codex-plugin/plugin.json is absent.
        self.assertNotIn("plugin.json", self.ctx.files)
        for path in self.ctx.files:
            self.assertFalse(path.startswith((".claude-plugin", ".cursor-plugin", "commands/")), path)

    def test_identity_and_paths(self):
        m, meta = self.manifest, self.ctx.meta
        self.assertEqual((m["name"], m["version"], m["license"]), (meta["name"], meta["version"], "Apache-2.0"))
        self.assertEqual(m["author"]["name"], "Agent Network Research")
        self.assertEqual(m["skills"], "./skills/")
        self.assertEqual(m["mcpServers"], "./.mcp.json")
        self.assertIn(m["mcpServers"][2:], self.ctx.files)
        self.assertNotIn("commands", m)
        self.assertNotIn("hooks", m)
        self.assertNotIn("apps", m)

    def test_interface(self):
        i = self.manifest["interface"]
        self.assertLessEqual(len(i["displayName"]), 30)
        self.assertLessEqual(len(i["shortDescription"]), 30)
        self.assertLessEqual(len(i["longDescription"]), 4000)
        self.assertLessEqual(len(i["defaultPrompt"]), 3)
        self.assertTrue(all(len(p) <= 128 for p in i["defaultPrompt"]))
        self.assertLessEqual(len(i["capabilities"]), 20)
        self.assertTrue(all(len(c) <= 120 for c in i["capabilities"]))
        self.assertRegex(i["brandColor"], r"^#[0-9A-F]{6}$")
        for key in ("composerIcon", "logo"):
            self.assertTrue(i[key].startswith("./"), key)
            self.assertIn(i[key][2:], self.ctx.files)
        self.assertTrue(i["websiteURL"].startswith("https://"))

    def test_icon_is_a_square_png(self):
        data = self.ctx.files["assets/icon.png"].data
        self.assertEqual(data[:8], b"\x89PNG\r\n\x1a\n")
        width, height = int.from_bytes(data[16:20], "big"), int.from_bytes(data[20:24], "big")
        self.assertEqual(width, height)
        self.assertGreaterEqual(width, 48)

    def test_validate_rejects_a_bad_name(self):
        from copy import deepcopy

        class Fake:
            meta = deepcopy(self.ctx.meta)

        Fake.meta["name"] = "Anet"
        with self.assertRaises(ValueError):
            self.ctx.module.validate(Fake)


class McpConfigTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.ctx, _ = build_codex()
        cls.mcp = json_of(cls.ctx, ".mcp.json")

    def test_server(self):
        servers = self.mcp["mcpServers"]
        self.assertEqual(list(servers), ["anet"])
        s = servers["anet"]
        # Relative to cwd ".", the plugin root: bin/anet-mcp on Linux and macOS, bin/anet-mcp.cmd on
        # Windows, where Codex resolves the program with which::which_in and PATHEXT.
        self.assertEqual(s["command"], "./bin/anet-mcp")
        self.assertEqual(s["args"], [])
        self.assertEqual(s["cwd"], ".")
        self.assertIn(s["command"][2:], self.ctx.files)
        self.assertTrue(self.ctx.files[s["command"][2:]].executable)
        self.assertIn(s["command"][2:] + ".cmd", self.ctx.files)
        self.assertEqual(s["tool_timeout_sec"], self.ctx.meta["mcp"]["timeout_ms"] // 1000)

    def test_no_variable_is_expanded_in_the_legacy_format(self):
        # Codex passes ${...} through literally in .mcp.json; none may appear.
        self.assertNotIn("${", text_of(self.ctx, ".mcp.json"))
        self.assertNotIn("$", text_of(self.ctx, ".mcp.json"))

    def test_settings_are_forwarded_by_name(self):
        env_names = [s["env"] for s in self.ctx.meta["settings"]]
        self.assertEqual(sorted(self.mcp["mcpServers"]["anet"]["env_vars"]), sorted(env_names))
        self.assertEqual(sorted(env_names), ["ANET_PLUGIN_BIN", "ANET_PLUGIN_IDENTITY"])
        self.assertNotIn("env", self.mcp["mcpServers"]["anet"])

    def test_only_fields_of_the_codex_server_config(self):
        allowed = {"command", "args", "env", "env_vars", "cwd", "startup_timeout_sec", "tool_timeout_sec", "enabled", "type"}
        self.assertLessEqual(set(self.mcp["mcpServers"]["anet"]), allowed)


class TextTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.ctx, _ = build_codex()
        cls.texts = {p: text_of(cls.ctx, p) for p in cls.ctx.files if p.endswith((".md", ".sh", ".json")) or p == "LICENSE"}

    def test_no_template_residue_and_no_foreign_syntax(self):
        for path, text in self.texts.items():
            if path == "LICENSE":
                continue
            self.assertNotIn("{{", text, path)
            for foreign in ("ZCODE_", "ZCode", "CLAUDE_", "Claude", "user_config", "$ARGUMENTS", "${PLUGIN_ROOT}", ".zcode"):
                self.assertNotIn(foreign, text, f"{path} contains {foreign}")

    def test_no_slash_command_is_mentioned(self):
        for path, text in self.texts.items():
            if path.endswith(".md"):
                self.assertNotIn("/anet-status", text.replace("$anet:anet-status", ""), path)

    def test_every_skill_reaches_the_runner(self):
        names = sorted(p.split("/")[1] for p in self.ctx.files if p.startswith("skills/") and p.endswith("SKILL.md"))
        self.assertEqual(names, ["anet", "anet-setup", "anet-status"])
        for name in names:
            text = self.texts[f"skills/{name}/SKILL.md"]
            self.assertIn('sh "<skill-dir>/../../bin/anet-run.sh"', text, name)
            self.assertIn("`<skill-dir>` is the absolute path of the directory that contains this SKILL.md", text, name)
            # <skill-dir> is skills/<name>: two levels up is the plugin root, which holds bin/anet-run.sh.
            self.assertIn("bin/anet-run.sh", self.ctx.files)
            self.assertIn(f"skills/{name}/SKILL.md", self.ctx.files)

    def test_skill_front_matter(self):
        for name in ("anet", "anet-setup", "anet-status"):
            text = self.texts[f"skills/{name}/SKILL.md"]
            fields = front_matter(text)
            self.assertEqual(fields["name"], name)
            self.assertLessEqual(len(fields["description"]), 1024)
            self.assertTrue(fields["description"].endswith("."), name)
            self.assertNotIn("\n", fields["description"])

    def test_status_skill_is_the_shared_command(self):
        description, body = self.ctx.command("anet-status")
        status = self.texts["skills/anet-status/SKILL.md"]
        self.assertTrue(status.endswith(body))
        self.assertIn(json.dumps(description.rstrip(".") + ".", ensure_ascii=False)[1:-1], status)
        self.assertIn("topic `status`", status)
        self.assertIn('"<skill-dir>/../../bin/anet-run.cmd"', status)

    def test_skill_anet_is_the_entry_point(self):
        text = self.texts["skills/anet/SKILL.md"]
        self.assertIn("invokes this skill with a request", text)
        self.assertIn("$anet:anet", self.texts["README.md"])

    def test_settings_section_names_the_variables_in_both_languages(self):
        for readme in ("README.md", "README_CN.md"):
            text = self.texts[readme]
            self.assertIn("`ANET_PLUGIN_BIN`", text, readme)
            self.assertIn("`ANET_PLUGIN_IDENTITY`", text, readme)
            self.assertIn("$anet:anet-status", text, readme)
            self.assertIn("anet-status", text, readme)
        self.assertIn("codex plugin marketplace add ANetResearch/ANet", self.texts["README.md"])
        self.assertIn("anet@agentnetwork", self.texts["README_CN.md"])
        self.assertIn("Windows PowerShell 5.1", self.texts["README.md"])

    def test_readme_does_not_promise_a_command(self):
        self.assertNotIn("| Command |", self.texts["README.md"])
        self.assertNotIn("| 命令 |", self.texts["README_CN.md"])

    def test_text_is_enterprise_copy(self):
        for path, text in self.texts.items():
            if path == "LICENSE":
                continue
            if path.endswith(".md"):
                self.assertNotRegex(text, r"[!！](\s|$)", path)
            self.assertNotRegex(text, "[\U0001F300-\U0001FAFF\u2600-\u27BF]", path)


class TransformTests(unittest.TestCase):
    def test_drift_of_the_shared_text_is_an_error(self):
        module = build_codex()[0].module
        with self.assertRaises(ValueError):
            module._replace_once("abc", "x", "y", "t")
        with self.assertRaises(ValueError):
            module._replace_once("xx", "x", "y", "t")
        self.assertEqual(module._replace_once("axb", "x", "y", "t"), "ayb")

    def test_transform_fails_on_changed_shared_source(self):
        ctx, _ = build_codex()
        with self.assertRaises(ValueError):
            ctx.module.transform(ctx, "skills/anet-setup/SKILL.md", "no anchor here", "en")


class CatalogueTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.ctx, cls.res = build_codex()

    def test_mirror_catalogue(self):
        cat = json.loads(self.ctx.root_files[".agents/plugins/marketplace.json"].data)
        self.assertEqual(cat["name"], "agentnetwork")
        (entry,) = cat["plugins"]
        self.assertEqual(entry["name"], "anet")
        self.assertEqual(entry["source"], "./codex")
        self.assertEqual(entry["policy"], {"installation": "AVAILABLE", "authentication": "ON_INSTALL"})
        self.assertEqual(list(self.ctx.root_files), [".agents/plugins/marketplace.json"])

    def test_catalogue_sources_obey_codex_rules(self):
        for source in ("./codex", "./deploy/plugins/dist/codex"):
            entry = self.ctx.module.catalogue(source)["plugins"][0]
            self.assertTrue(entry["source"].startswith("./"))
            self.assertNotIn("..", entry["source"])

    def test_repository_root_catalogue_is_the_committed_source(self):
        path = PLUGINS / "src/codex/repo-root-marketplace.json"
        self.assertEqual(
            json.loads(path.read_text(encoding="utf-8")),
            self.ctx.module.catalogue("./deploy/plugins/dist/codex"),
        )
        self.assertTrue(path.read_text(encoding="utf-8").endswith("}\n"))

    def test_repository_root_file_if_present(self):
        root = PLUGINS.parent.parent / ".agents/plugins/marketplace.json"
        if not root.exists():
            self.skipTest("no .agents/plugins/marketplace.json at the repository root yet")
        self.assertEqual(root.read_text(encoding="utf-8"), (PLUGINS / "src/codex/repo-root-marketplace.json").read_text(encoding="utf-8"))

    def test_catalogue_source_exists_in_dist(self):
        dist = PLUGINS / "dist/codex/.codex-plugin/plugin.json"
        if not dist.exists():
            self.skipTest("dist/codex has not been generated")
        self.assertEqual(json.loads(dist.read_text(encoding="utf-8"))["name"], "anet")

    def test_markers_do_not_occur_in_other_targets(self):
        # Each other target is built together with this one; the generator fails when a file of
        # one contains a marker of the other. A leak between two other targets is not a failure here.
        others = [k for k in g.discover() if k != "codex"]
        for other in others:
            try:
                res = g.build_all([other, "codex"])
            except g.BuildError as e:
                self.fail(f"{other} with codex: {e}")
            self.assertIn("codex", res.contexts)


class LauncherTests(unittest.TestCase):
    """Run bin/anet-mcp.sh from a generated tree with the environment that .mcp.json forwards."""

    @classmethod
    def setUpClass(cls):
        cls.ctx, _ = build_codex()
        cls.tmp = tempfile.mkdtemp(prefix="anet-codex-test-")
        cls.root = Path(cls.tmp) / "plugin"
        for rel, f in cls.ctx.files.items():
            p = cls.root / rel
            p.parent.mkdir(parents=True, exist_ok=True)
            p.write_bytes(f.data)
        # The files keep no executable bit: the plugin starts them with sh.
        cls.fake = Path(cls.tmp) / "fake-anet"
        cls.fake.write_text('#!/bin/sh\nprintf "argv:"; for a in "$@"; do printf " [%s]" "$a"; done; printf "\\n"\n')
        cls.fake.chmod(0o755)
        cls.work = Path(cls.tmp) / "elsewhere"
        cls.work.mkdir()

    @classmethod
    def tearDownClass(cls):
        shutil.rmtree(cls.tmp, ignore_errors=True)

    def run_sh(self, args, env_extra, path=None):
        env = {"PATH": path if path is not None else "/usr/bin:/bin", "HOME": str(Path(self.tmp) / "home")}
        env.update(env_extra)
        return subprocess.run(["sh", *args], cwd=self.work, env=env, capture_output=True, text=True, timeout=30)

    def test_launcher_from_the_server_working_directory(self):
        # Codex starts the server with cwd "." = the plugin root.
        env = {"ANET_PLUGIN_BIN": str(self.fake), "ANET_PLUGIN_IDENTITY": "work"}
        r = subprocess.run(["sh", "./bin/anet-mcp.sh"], cwd=self.root, env={"PATH": "/usr/bin:/bin", **env}, capture_output=True, text=True, timeout=30)
        self.assertEqual((r.returncode, r.stdout.strip()), (0, "argv: [--id] [work] [mcp]"))

    def test_default_identity(self):
        r = self.run_sh([str(self.root / "bin/anet-mcp.sh")], {"ANET_PLUGIN_BIN": str(self.fake)})
        self.assertEqual((r.returncode, r.stdout.strip()), (0, "argv: [mcp]"))

    def test_invalid_executable_is_an_error_and_is_not_replaced(self):
        r = self.run_sh([str(self.root / "bin/anet-mcp.sh")], {"ANET_PLUGIN_BIN": "/nonexistent/anet"})
        self.assertEqual(r.returncode, 1)
        self.assertIn("not an executable file", r.stderr)

    def test_search_in_install_locations(self):
        home = Path(self.tmp) / "home"
        (home / ".local/bin").mkdir(parents=True, exist_ok=True)
        shutil.copy(self.fake, home / ".local/bin/anet")
        r = self.run_sh([str(self.root / "bin/anet-run.sh"), "--resolve"], {})
        self.assertEqual(r.returncode, 0)
        self.assertIn(f"executable: {home}/.local/bin/anet", r.stdout)
        shutil.rmtree(home)

    def test_skills_reach_the_same_settings(self):
        # In the shell of a session the runner reads the same two variables.
        runner = str(self.root / "skills/anet-status/../../bin/anet-run.sh")
        r = self.run_sh([runner, "--resolve"], {"ANET_PLUGIN_BIN": str(self.fake), "ANET_PLUGIN_IDENTITY": "work"})
        self.assertEqual(r.returncode, 0)
        self.assertIn(f"executable: {self.fake}", r.stdout)
        self.assertIn("identity: work", r.stdout)
        r = self.run_sh([runner, "status"], {"ANET_PLUGIN_BIN": str(self.fake), "ANET_PLUGIN_IDENTITY": "work"})
        self.assertEqual(r.stdout.strip(), "argv: [--id] [work] [status]")


@unittest.skipUnless(os.environ.get("ANET_CODEX_BIN"), "set ANET_CODEX_BIN to the path of the codex executable")
class CodexCliTests(unittest.TestCase):
    """Install the generated tree with the Codex CLI in a scratch CODEX_HOME and inspect it."""

    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.mkdtemp(prefix="anet-codex-cli-")
        cls.home = Path(cls.tmp) / "home"
        cls.out = Path(cls.tmp) / "mirror"
        g.write_tree(g.build_all(["codex"]).tree(), cls.out, force=True)
        cls.env = {
            "PATH": os.environ["PATH"], "HOME": str(cls.home), "CODEX_HOME": str(cls.home / ".codex"),
            "XDG_CONFIG_HOME": str(cls.home / ".config"), "XDG_DATA_HOME": str(cls.home / ".local/share"),
            "XDG_CACHE_HOME": str(cls.home / ".cache"), "XDG_STATE_HOME": str(cls.home / ".local/state"),
        }
        (cls.home / ".codex").mkdir(parents=True)

    @classmethod
    def tearDownClass(cls):
        shutil.rmtree(cls.tmp, ignore_errors=True)

    def codex(self, *args):
        r = subprocess.run([os.environ["ANET_CODEX_BIN"], *args], env=self.env, capture_output=True, text=True, timeout=180, stdin=subprocess.DEVNULL)
        return r

    def test_install_and_inspect(self):
        r = self.codex("plugin", "marketplace", "add", str(self.out))
        self.assertEqual(r.returncode, 0, r.stderr)
        r = self.codex("plugin", "add", "anet@agentnetwork")
        self.assertEqual(r.returncode, 0, r.stderr)
        installed = self.home / ".codex/plugins/cache/agentnetwork/anet" / self.ctx_version()
        self.assertTrue((installed / ".codex-plugin/plugin.json").is_file())
        self.assertTrue((installed / "skills/anet-status/SKILL.md").is_file())
        r = self.codex("mcp", "list")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn("anet", r.stdout)
        self.assertIn("./bin/anet-mcp.sh", r.stdout)
        self.assertIn("ANET_PLUGIN_BIN", r.stdout)
        r = self.codex("debug", "prompt-input", "x")
        self.assertEqual(r.returncode, 0, r.stderr)
        for name in ("anet:anet", "anet:anet-setup", "anet:anet-status"):
            self.assertIn(f"- {name}:", r.stdout)

    @staticmethod
    def ctx_version():
        return g.load_meta()["version"]


if __name__ == "__main__":
    unittest.main()
