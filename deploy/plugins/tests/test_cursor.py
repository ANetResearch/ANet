#!/usr/bin/env python3
"""Tests of the cursor target: python3 deploy/plugins/tests/test_cursor.py

They build the target into memory and check the rules of the Cursor plugin
format that Cursor's own repository validator enforces (additionalProperties
false, the name pattern, the marketplace entry), the variable contract between
mcp.json and the manifest, the wording of the shared texts that the target
rewrites, and the behaviour of the launcher with the environment that mcp.json
produces. If the validator of github.com/cursor/plugins is available, it is run
on the generated tree as well:

    ANET_CURSOR_VALIDATOR=<directory with scripts/validate-plugins.mjs, schemas/, node_modules/> \
        python3 deploy/plugins/tests/test_cursor.py
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

# Keys that .cursor-plugin/plugin.json accepts (schemas/plugin.schema.json of github.com/cursor/plugins,
# additionalProperties false) and the keys of a marketplace file and of an entry.
PLUGIN_KEYS = {
    "name", "displayName", "description", "version", "minClientVersions", "author", "publisher", "homepage",
    "repository", "license", "logo", "keywords", "category", "tags", "commands", "agents", "skills", "rules",
    "hooks", "variables", "mcpServers",
}
MARKETPLACE_KEYS = {"name", "owner", "metadata", "plugins"}
ENTRY_KEYS = {"name", "source", "description", "minClientVersions"}
OWNER_KEYS = {"name", "email"}
AUTHOR_KEYS = {"name", "email"}


def build_cursor():
    res = g.build_all(["cursor"])
    ctx = res.contexts["cursor"]
    return ctx, res


def text_of(ctx, path):
    return ctx.files[path].data.decode("utf-8")


def json_of(ctx, path):
    return json.loads(text_of(ctx, path))


class ManifestTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.ctx, cls.res = build_cursor()
        cls.meta = cls.ctx.meta
        cls.plugin = json_of(cls.ctx, ".cursor-plugin/plugin.json")

    def test_manifest_uses_only_keys_of_the_schema(self):
        self.assertLessEqual(set(self.plugin), PLUGIN_KEYS)
        self.assertLessEqual(set(self.plugin["author"]), AUTHOR_KEYS)
        # Fields of other hosts must not leak in: they fail the schema of Cursor.
        for key in ("userConfig", "description_i18n"):
            self.assertNotIn(key, self.plugin)

    def test_name_version_and_required_fields(self):
        self.assertRegex(self.plugin["name"], r"^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$")
        self.assertEqual(self.plugin["name"], self.meta["name"])
        self.assertEqual(self.plugin["version"], self.meta["version"])
        self.assertTrue(self.plugin["description"])
        self.assertEqual(self.plugin["license"], "Apache-2.0")

    def test_logo_is_a_committed_relative_file(self):
        logo = self.plugin["logo"]
        self.assertFalse(logo.startswith("/") or ".." in Path(logo).parts)
        self.assertIn(logo, self.ctx.files)
        self.assertEqual(self.ctx.files[logo].data, self.ctx.icon)

    def test_no_component_path_replaces_the_default_discovery(self):
        for key in ("skills", "commands", "agents", "rules", "hooks", "mcpServers"):
            self.assertNotIn(key, self.plugin)
        self.assertIn("mcp.json", self.ctx.files)
        self.assertNotIn(".mcp.json", self.ctx.files)


class McpTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.ctx, _ = build_cursor()
        cls.meta = cls.ctx.meta
        cls.plugin = json_of(cls.ctx, ".cursor-plugin/plugin.json")
        cls.mcp = json_of(cls.ctx, "mcp.json")

    def test_one_stdio_server_started_by_its_path(self):
        # bin/anet-mcp on Linux and macOS, bin/anet-mcp.cmd on Windows (PATHEXT); see the module docstring.
        servers = self.mcp["mcpServers"]
        self.assertEqual(list(servers), [self.meta["mcp"]["server_name"]])
        server = servers["anet"]
        self.assertEqual(server["command"], "${CURSOR_PLUGIN_ROOT}/bin/anet-mcp")
        self.assertEqual(server["args"], [])
        self.assertTrue(self.ctx.files["bin/anet-mcp"].executable)
        self.assertIn("bin/anet-mcp.cmd", self.ctx.files)

    def test_every_variable_is_declared_and_has_an_empty_default(self):
        declared = self.plugin["variables"]["properties"]
        used = set()
        for value in json.dumps(self.mcp["mcpServers"]).split("${")[1:]:
            ref = value.split("}", 1)[0]
            used.add(ref)
        self.assertIn("CURSOR_PLUGIN_ROOT", used)
        for ref in used - {"CURSOR_PLUGIN_ROOT"}:
            name, _, default = ref.partition(":-")
            self.assertIn(name, declared, "every ${VAR} of mcp.json must be declared in the manifest")
            self.assertEqual(default, "", "an unset variable must fall back to the empty string")
            self.assertEqual(declared[name]["type"], "string")
            self.assertEqual(declared[name]["default"], "")
        self.assertEqual(set(declared), {s["env"] for s in self.meta["settings"]})

    def test_the_server_receives_exactly_the_variables_the_launcher_reads(self):
        env = self.mcp["mcpServers"]["anet"]["env"]
        self.assertEqual(set(env), {"ANET_PLUGIN_BIN", "ANET_PLUGIN_IDENTITY"})
        launcher = text_of(self.ctx, "bin/anet-run.sh")
        for name in env:
            self.assertIn(name, launcher)

    def test_unsupported_placeholders_are_absent(self):
        blob = "\n".join(
            text_of(self.ctx, p) for p in self.ctx.files if p.endswith((".json", ".md", ".sh"))
        )
        for forbidden in ("${PLUGIN_ROOT}", "${PLUGIN_DATA}", "${user_config.", "ZCODE_PLUGIN_ROOT", "CLAUDE_PLUGIN_ROOT",
                          "CLAUDE_SKILL_DIR", ".claude-plugin", ".zcode"):
            self.assertNotIn(forbidden, blob)


class MarketplaceTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.ctx, cls.res = build_cursor()
        cls.market = json.loads(cls.res.root[".cursor-plugin/marketplace.json"].data)
        cls.plugin = json_of(cls.ctx, ".cursor-plugin/plugin.json")

    def check_catalogue(self, market, base):
        self.assertLessEqual(set(market), MARKETPLACE_KEYS)
        self.assertLessEqual(set(market["owner"]), OWNER_KEYS)
        self.assertEqual(len(market["plugins"]), 1)
        entry = market["plugins"][0]
        self.assertLessEqual(set(entry), ENTRY_KEYS)
        self.assertEqual(entry["name"], self.plugin["name"], "the entry name must equal plugin.json name")
        self.assertIsInstance(entry["source"], str)
        self.assertFalse(entry["source"].startswith("/") or ".." in Path(entry["source"]).parts)
        self.assertTrue((base / entry["source"] / ".cursor-plugin" / "plugin.json").is_file())

    def test_mirror_catalogue_names_the_generated_directory(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            g.write_tree(self.res.tree(), root, force=True)
            self.check_catalogue(self.market, root)
            self.assertEqual(self.market["plugins"][0]["source"], "cursor")

    def test_catalogue_for_the_root_of_the_anet_repository(self):
        # src/cursor/repo-root-marketplace.json is committed as .cursor-plugin/marketplace.json at the root of
        # the ANet repository; its source is relative to that root and must reach the committed copy of the plugin.
        market = json.loads((PLUGINS / "src" / "cursor" / "repo-root-marketplace.json").read_text(encoding="utf-8"))
        self.assertEqual(market["plugins"][0]["source"], f"{g.DIST_PATH_IN_REPO}/cursor")
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            g.write_tree({f"{g.DIST_PATH_IN_REPO}/cursor/{p}": f for p, f in self.ctx.files.items()}, root, force=True)
            self.check_catalogue(market, root)
        # Same catalogue as the mirror one apart from the source.
        mirror = json.loads(json.dumps(self.market))
        mirror["plugins"][0]["source"] = market["plugins"][0]["source"]
        self.assertEqual(market, mirror)


class ComponentTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.ctx, _ = build_cursor()

    def frontmatter(self, path):
        fields, body = g.split_frontmatter(text_of(self.ctx, path))
        return fields, body

    def test_skills_have_name_and_description_and_defer_to_the_guide(self):
        for name in ("anet", "anet-setup"):
            fields, _ = self.frontmatter(f"skills/{name}/SKILL.md")
            self.assertEqual(fields["name"], name)
            self.assertTrue(fields["description"])
        self.assertIn("@anet", self.frontmatter("skills/anet/SKILL.md")[0]["description"])
        self.assertIn("<runner> guide setup", text_of(self.ctx, "skills/anet-setup/SKILL.md"))

    def test_commands_have_name_and_description(self):
        for name in ("anet", "anet-status"):
            fields, body = self.frontmatter(f"commands/{name}.md")
            self.assertEqual(fields["name"], name)
            self.assertTrue(fields["description"])
            self.assertNotIn("!`", body, "Cursor commands have no inline execution")
            self.assertNotIn("$ARGUMENTS", body, "Cursor commands have no argument variable")
            self.assertNotIn("{{", body)
            self.assertIn("`<plugin-root>` is the root directory of the anet plugin", body)

    def test_status_command_defers_to_the_guide(self):
        _, body = self.frontmatter("commands/anet-status.md")
        self.assertIn("topic `status`", body)
        self.assertIn("<runner> guide status", body)

    def test_scripts_are_executable(self):
        for rel in ("bin/anet-run.sh", "bin/anet-mcp.sh", "bin/anet-mcp", "bin/anet-install.sh"):
            self.assertTrue(self.ctx.files[rel].executable, rel)

    def test_runner_is_named_in_the_syntax_the_agent_can_resolve(self):
        for rel in ("skills/anet-setup/SKILL.md", "skills/anet/SKILL.md"):
            text = text_of(self.ctx, rel)
            self.assertIn('sh "<skill-dir>/../../bin/anet-run.sh"', text, rel)
            self.assertIn('"<skill-dir>/../../bin/anet-run.cmd"', text, rel)
            self.assertIn("`<skill-dir>` is the directory that contains this `SKILL.md` file", text, rel)
        # The relative path really reaches the runner from a skill directory.
        self.assertIn("bin/anet-run.sh", self.ctx.files)
        self.assertEqual(Path("skills/anet-setup", "../../bin/anet-run.sh").as_posix().count(".."), 2)

    def test_setup_rules_are_kept(self):
        setup = text_of(self.ctx, "skills/anet-setup/SKILL.md")
        self.assertIn("bin/anet-install.sh", setup)
        self.assertIn("bin/anet-install.cmd", setup)
        self.assertNotIn("$(", setup)
        self.assertIn("Never read, print or copy the files of the anet data directory (`~/.anet`", setup)
        self.assertIn("not run them on the user's behalf", setup)
        self.assertIn("Pass no options to the installer", setup)
        # No settings file of Cursor exists; the text must not tell the agent about one.
        self.assertNotIn("~/.cursor", setup)

    def test_readme_states_windows_and_settings(self):
        readme = text_of(self.ctx, "README.md")
        self.assertIn("Linux, macOS or Windows", readme)
        self.assertIn("Windows PowerShell 5.1", readme)
        self.assertIn("`/anet`", readme)
        self.assertIn("ANET_PLUGIN_BIN", readme)
        self.assertIn("cursor-agent --plugin-dir", readme)
        self.assertNotIn("{{", readme)
        cn = text_of(self.ctx, "README_CN.md")
        self.assertIn("ANET_PLUGIN_IDENTITY", cn)
        self.assertNotIn("{{", cn)

    def test_runners_of_the_texts_are_started_with_sh(self):
        # The texts start the POSIX runner with sh, so a lost executable bit does not stop them.
        self.assertTrue(self.ctx.vars["RUNNER_FROM_SKILL"].startswith("<skill-dir>"))
        self.assertIn('exec sh "$(dirname "$0")/anet-mcp.sh"', text_of(self.ctx, "bin/anet-mcp"))
        self.assertIn('exec sh "$(dirname "$0")/anet-run.sh" mcp', text_of(self.ctx, "bin/anet-mcp.sh"))

    def test_transform_fails_when_a_shared_command_changes(self):
        mod = self.ctx.module
        for name in ("anet", "anet-status"):
            with self.assertRaises(ValueError):
                mod.transform(self.ctx, f"commands/{name}.md", "no marker here\n", "en")

    def test_validate_rejects_a_setting_that_cannot_be_a_plugin_variable(self):
        mod = self.ctx.module
        bad = type("C", (), {})()
        bad.meta = json.loads(json.dumps(self.ctx.meta))
        bad.meta["settings"][0]["env"] = "anet-plugin-bin"
        with self.assertRaises(ValueError):
            mod.validate(bad)
        bad.meta = json.loads(json.dumps(self.ctx.meta))
        bad.meta["name"] = "Anet"
        with self.assertRaises(ValueError):
            mod.validate(bad)
        bad.meta = json.loads(json.dumps(self.ctx.meta))
        bad.meta["settings"][1]["default"] = "x"
        with self.assertRaises(ValueError):
            mod.validate(bad)


class LauncherTests(unittest.TestCase):
    """bin/anet-mcp.sh with the environment that Cursor builds from mcp.json."""

    @classmethod
    def setUpClass(cls):
        cls.ctx, cls.res = build_cursor()
        cls.tmp = tempfile.TemporaryDirectory()
        root = Path(cls.tmp.name)
        cls.plugin = root / "plugin"
        g.write_tree({p: f for p, f in cls.ctx.files.items()}, cls.plugin, force=True)
        cls.home = root / "home"
        (cls.home / ".local" / "bin").mkdir(parents=True)
        cls.stub = cls.home / ".local" / "bin" / "anet"
        cls.stub.write_text('#!/bin/sh\nprintf "ARGS:%s\\n" "$*"\n')
        cls.stub.chmod(0o755)
        cls.other = root / "other-anet"
        cls.other.write_text('#!/bin/sh\nprintf "OTHER:%s\\n" "$*"\n')
        cls.other.chmod(0o755)

    @classmethod
    def tearDownClass(cls):
        cls.tmp.cleanup()

    def run_mcp(self, env_extra, script="anet-mcp", args=()):
        # The stdio transport of Cursor passes HOME, PATH and the configured env only. bin/anet-mcp is run by
        # its path, as mcp.json names it; the runner is run with sh, as the skills name it.
        env = {"HOME": str(self.home), "PATH": "/usr/bin:/bin"}
        env.update(env_extra)
        argv = [str(self.plugin / "bin" / script)] if script == "anet-mcp" else ["sh", str(self.plugin / "bin" / script)]
        return subprocess.run([*argv, *args], env=env, capture_output=True, text=True, timeout=30)

    def test_empty_variables_use_the_default_search(self):
        # What mcp.json yields when nothing is configured: both variables empty.
        r = self.run_mcp({"ANET_PLUGIN_BIN": "", "ANET_PLUGIN_IDENTITY": ""})
        self.assertEqual((r.returncode, r.stdout), (0, "ARGS:mcp\n"), r.stderr)

    def test_unset_variables_behave_like_empty_ones(self):
        r = self.run_mcp({})
        self.assertEqual((r.returncode, r.stdout), (0, "ARGS:mcp\n"), r.stderr)

    def test_identity_is_passed_before_the_command(self):
        r = self.run_mcp({"ANET_PLUGIN_BIN": "", "ANET_PLUGIN_IDENTITY": "work"})
        self.assertEqual(r.stdout, "ARGS:--id work mcp\n", r.stderr)

    def test_configured_path_wins_and_is_never_replaced(self):
        r = self.run_mcp({"ANET_PLUGIN_BIN": str(self.other), "ANET_PLUGIN_IDENTITY": ""})
        self.assertEqual(r.stdout, "OTHER:mcp\n", r.stderr)
        r = self.run_mcp({"ANET_PLUGIN_BIN": str(self.home / "missing"), "ANET_PLUGIN_IDENTITY": ""})
        self.assertEqual(r.returncode, 1)
        self.assertEqual(r.stdout, "")
        self.assertIn("not an executable file", r.stderr)

    def test_unexpanded_placeholder_is_an_error_not_a_silent_fallback(self):
        # If Cursor left ${ANET_PLUGIN_BIN} unexpanded, the runner must report it rather than pick another copy.
        r = self.run_mcp({"ANET_PLUGIN_BIN": "${ANET_PLUGIN_BIN}", "ANET_PLUGIN_IDENTITY": ""})
        self.assertEqual(r.returncode, 1)
        self.assertIn("not an absolute path", r.stderr)
        self.assertEqual(r.stdout, "")

    def test_relative_path_is_refused(self):
        # A relative anetPath would depend on the working directory of the session.
        r = self.run_mcp({"ANET_PLUGIN_BIN": "./anet", "ANET_PLUGIN_IDENTITY": ""})
        self.assertEqual(r.returncode, 1)
        self.assertIn("not an absolute path", r.stderr)

    def test_resolve_from_a_shell_reads_the_environment(self):
        r = self.run_mcp({"ANET_PLUGIN_BIN": str(self.other)}, script="anet-run.sh", args=("--resolve",))
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn(f"executable: {self.other}", r.stdout)
        self.assertIn("identity: default", r.stdout)
        self.assertIn("runner: sh ", r.stdout)

    def test_missing_anet_exits_1_with_a_reason(self):
        env = {"HOME": str(self.plugin), "PATH": "/usr/bin:/bin"}
        r = subprocess.run([str(self.plugin / "bin" / "anet-mcp")], env=env, capture_output=True, text=True, timeout=30)
        if Path("/usr/local/bin/anet").exists() or Path("/opt/homebrew/bin/anet").exists():
            self.skipTest("an anet executable exists in a system directory")
        self.assertEqual(r.returncode, 1)
        self.assertIn("anet is not installed", r.stderr)


@unittest.skipUnless(os.environ.get("ANET_CURSOR_VALIDATOR"), "set ANET_CURSOR_VALIDATOR to run the validator of github.com/cursor/plugins")
class CursorValidatorTests(unittest.TestCase):
    def run_validator(self, tree):
        src = Path(os.environ["ANET_CURSOR_VALIDATOR"])
        node = shutil.which("node")
        if not node:
            self.skipTest("node is not on PATH")
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            (root / "scripts").mkdir()
            (root / "schemas").mkdir()
            shutil.copy(src / "scripts" / "validate-plugins.mjs", root / "scripts")
            for f in (src / "schemas").glob("*.json"):
                shutil.copy(f, root / "schemas")
            (root / "node_modules").symlink_to(src / "node_modules")
            g.write_tree(tree, root, force=True)
            return subprocess.run([node, "scripts/validate-plugins.mjs"], cwd=root, capture_output=True, text=True, timeout=120)

    def test_mirror_tree_passes(self):
        ctx, res = build_cursor()
        tree = {p: f for p, f in res.tree().items() if p.startswith(("cursor/", ".cursor-plugin/"))}
        r = self.run_validator(tree)
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)

    def test_repository_root_tree_passes(self):
        ctx, res = build_cursor()
        tree = {f"{g.DIST_PATH_IN_REPO}/cursor/{p}": f for p, f in ctx.files.items()}
        tree[".cursor-plugin/marketplace.json"] = g.File((PLUGINS / "src" / "cursor" / "repo-root-marketplace.json").read_bytes())
        r = self.run_validator(tree)
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)

    def test_a_foreign_manifest_field_is_rejected(self):
        ctx, res = build_cursor()
        tree = {p: f for p, f in res.tree().items() if p.startswith(("cursor/", ".cursor-plugin/"))}
        manifest = json.loads(tree["cursor/.cursor-plugin/plugin.json"].data)
        manifest["userConfig"] = {}
        tree["cursor/.cursor-plugin/plugin.json"] = g.File(g.json_text(manifest))
        r = self.run_validator(tree)
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("userConfig", r.stdout + r.stderr)


if __name__ == "__main__":
    unittest.main()
