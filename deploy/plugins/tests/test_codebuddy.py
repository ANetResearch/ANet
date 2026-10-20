#!/usr/bin/env python3
"""Tests of the codebuddy target: python3 deploy/plugins/tests/test_codebuddy.py

They build the target into memory and check the facts about the CodeBuddy
plugin format on which the target depends (verified with CodeBuddy Code
2.169.0): the manifest .codebuddy-plugin/plugin.json, the .mcp.json with
${user_config.<key>}, the plugin variables in skills and commands, the two
catalogues, and the behaviour of the runner, which reads the plugin options
from the user settings of the host outside the MCP server.

With the CodeBuddy CLI available, the generated tree is also validated and
installed into a scratch HOME by the CLI itself:

    ANET_CODEBUDDY_BIN=<path of codebuddy> python3 deploy/plugins/tests/test_codebuddy.py
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

SYS_PATH = "/usr/bin:/bin"


def build_codebuddy():
    res = g.build_all(["codebuddy"])
    return res.contexts["codebuddy"], res


def text_of(ctx, path):
    return ctx.files[path].data.decode("utf-8")


def json_of(ctx, path):
    return json.loads(text_of(ctx, path))


def write_tree(ctx, dest):
    for rel, f in ctx.files.items():
        p = Path(dest) / rel
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_bytes(f.data)
        os.chmod(p, 0o755 if f.executable else 0o644)


class ManifestTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.ctx, cls.res = build_codebuddy()
        cls.manifest = json_of(cls.ctx, ".codebuddy-plugin/plugin.json")
        cls.mcp = json_of(cls.ctx, ".mcp.json")

    def test_files(self):
        self.assertEqual(
            sorted(self.ctx.files),
            sorted([
                ".codebuddy-plugin/plugin.json", ".mcp.json", "LICENSE", "README.md", "README_CN.md",
                "bin/anet-install.sh", "bin/anet-mcp.sh", "bin/anet-run.sh", "commands/anet-status.md",
                "skills/anet-setup/SKILL.md", "skills/anet/SKILL.md",
            ]),
        )

    def test_only_the_codebuddy_manifest_directory(self):
        # CodeBuddy tries .codebuddy-plugin, .workbuddy-plugin and the manifest directory of
        # another host in this order; a second manifest would only create doubt about which is read.
        dirs = {p.split("/", 1)[0] for p in self.ctx.files if p.startswith(".") and "/" in p}
        self.assertEqual(dirs, {".codebuddy-plugin"})

    def test_manifest(self):
        meta = self.ctx.meta
        m = self.manifest
        self.assertEqual(m["name"], meta["name"])
        self.assertEqual(m["version"], meta["version"])
        self.assertEqual(m["description"], meta["description"])
        self.assertEqual(m["license"], "Apache-2.0")
        self.assertEqual(m["author"]["name"], "Agent Network Research")
        self.assertRegex(m["name"], r"^[a-z0-9][a-z0-9-]*$")

    def test_user_config_matches_the_shared_settings(self):
        meta = self.ctx.meta
        cfg = self.manifest["userConfig"]
        self.assertEqual(list(cfg), [s["key"] for s in meta["settings"]])
        for s in meta["settings"]:
            entry = cfg[s["key"]]
            self.assertEqual(entry["type"], "string")
            self.assertEqual(entry["default"], "")
            self.assertTrue(entry["title"] and entry["description"])
            # Optional: a plugin without a value must start, so there is a default and no "required".
            self.assertNotIn("required", entry)
            self.assertNotIn("sensitive", entry)

    def test_mcp_server(self):
        meta = self.ctx.meta
        servers = self.mcp["mcpServers"]
        self.assertEqual(list(servers), [meta["mcp"]["server_name"]])
        server = servers["anet"]
        # Started with sh: the executable bit of the launcher does not matter.
        self.assertEqual(server["command"], "sh")
        self.assertEqual(server["args"], ["${CODEBUDDY_PLUGIN_ROOT}/bin/anet-mcp.sh"])
        self.assertEqual(server["env"], {s["env"]: "${user_config.%s}" % s["key"] for s in meta["settings"]})

    def test_every_user_config_reference_is_declared(self):
        declared = set(self.manifest["userConfig"])
        for path in (".mcp.json",) + tuple(p for p in self.ctx.files if p.endswith(".md")):
            for key in re.findall(r"\$\{user_config\.([^}]+)\}", text_of(self.ctx, path)):
                self.assertIn(key, declared, path)

    def test_plugin_variables_are_the_ones_the_host_expands(self):
        known = {"CODEBUDDY_PLUGIN_ROOT", "CODEBUDDY_SKILL_DIR", "CODEBUDDY_PLUGIN_DATA", "CODEBUDDY_PROJECT_DIR"}
        for path, f in self.ctx.files.items():
            if path.endswith(".sh") or not path.endswith((".md", ".json")):
                continue
            for var in re.findall(r"\$\{([A-Z_]+)\}", f.data.decode("utf-8")):
                if var.startswith(("CODEBUDDY_", "CLAUDE_", "ZCODE_", "CURSOR_")):
                    self.assertIn(var, known, path)

    def test_skills_and_command_reach_the_runner_through_the_plugin_root(self):
        runner = 'sh "${CODEBUDDY_PLUGIN_ROOT}/bin/anet-run.sh"'
        self.assertIn(runner + " --resolve", text_of(self.ctx, "skills/anet-setup/SKILL.md"))
        self.assertIn(runner + " --resolve", text_of(self.ctx, "commands/anet-status.md"))
        self.assertIn(runner + " doctor", text_of(self.ctx, "skills/anet/SKILL.md"))

    def test_command_is_named_as_codebuddy_names_it(self):
        # The host registers it as /<plugin>:<command> (slash_commands: "anet:anet-status").
        for path in ("README.md", "README_CN.md", "skills/anet/SKILL.md", "skills/anet-setup/SKILL.md"):
            text = text_of(self.ctx, path)
            self.assertNotRegex(text, r"`/anet-status`", path)
        self.assertIn("`/anet:anet-status`", text_of(self.ctx, "README.md"))
        self.assertIn("`/anet:anet-status`", text_of(self.ctx, "README_CN.md"))

    def test_guide_names_the_tools_as_the_host_does(self):
        guide = text_of(self.ctx, "skills/anet/SKILL.md")
        self.assertIn("mcp__anet__node_status", guide)
        self.assertEqual(guide.count("mcp__anet__<tool>"), 1)

    def test_setup_rules_of_zcode_are_kept(self):
        setup = text_of(self.ctx, "skills/anet-setup/SKILL.md")
        self.assertIn("bin/anet-install.sh", setup)
        self.assertNotIn("$(", setup)
        self.assertIn("Never read, print or copy files under `~/.anet`", setup)
        self.assertIn("Do not run them on the user's behalf", setup)
        self.assertIn("`~/.codebuddy` or `~/.workbuddy`", setup)  # the host configuration is named as off limits
        self.assertIn("Never read or edit the CodeBuddy configuration", setup)
        self.assertIn("do not run on Windows", setup)

    def test_readme(self):
        for name in ("README.md", "README_CN.md"):
            text = text_of(self.ctx, name)
            self.assertIn("codebuddy plugin marketplace add https://agentnetwork.org.cn/plugins/codebuddy/marketplace.json", text)
            self.assertIn("anet@agentnetwork", text)
            self.assertIn("pluginConfigs", text)
            self.assertNotIn("{{", text)
            self.assertNotRegex(text, r"!\s|[\U0001F300-\U0001FAFF]")
        self.assertIn("Windows", text_of(self.ctx, "README.md"))

    def test_scripts_are_portable_sh(self):
        for name in ("bin/anet-mcp.sh", "bin/anet-run.sh"):
            self.assertTrue(text_of(self.ctx, name).startswith("#!/bin/sh\n"), name)


class CatalogueTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.ctx, cls.res = build_codebuddy()
        cls.root = json.loads(cls.res.root[".codebuddy-plugin/marketplace.json"].data)
        cls.site = g.build_site(cls.res, "https://agentnetwork.org.cn")
        cls.web = json.loads(cls.site["plugins/codebuddy/marketplace.json"].data)

    def test_root_catalogue(self):
        self.assertEqual(self.root["name"], "agentnetwork")
        self.assertEqual(self.root["owner"]["name"], "Agent Network Research")
        [entry] = self.root["plugins"]
        self.assertEqual(entry["name"], "anet")
        self.assertEqual(entry["source"], "./codebuddy")
        self.assertEqual(entry["version"], self.ctx.meta["version"])
        self.assertTrue(entry["description"])

    def test_site_catalogue_names_the_subdirectory_of_the_repository(self):
        self.assertEqual(list(self.site), ["plugins/codebuddy/marketplace.json"])
        [entry] = self.web["plugins"]
        self.assertEqual(
            entry["source"],
            {"source": "git-subdir", "url": "https://github.com/ANetResearch/ANet.git", "path": "deploy/plugins/dist/codebuddy"},
        )
        self.assertEqual(self.web["name"], self.root["name"])
        self.assertEqual({k: v for k, v in entry.items() if k != "source"}, {k: v for k, v in self.root["plugins"][0].items() if k != "source"})

    def test_catalogue_version_equals_manifest_version(self):
        # The host caches an installed plugin by <marketplace>/<plugin>/<version>.
        manifest = json_of(self.ctx, ".codebuddy-plugin/plugin.json")
        for catalogue in (self.root, self.web):
            self.assertEqual(catalogue["plugins"][0]["version"], manifest["version"])

    def test_the_path_exists_in_the_committed_tree(self):
        dist = PLUGINS / "dist" / "codebuddy" / ".codebuddy-plugin" / "plugin.json"
        if not dist.is_file():
            self.skipTest("deploy/plugins/dist is not generated yet")
        self.assertEqual(json.loads(dist.read_text())["name"], "anet")


def fake_anet(directory):
    """An executable `anet` that prints its arguments."""
    directory = Path(directory)
    directory.mkdir(parents=True, exist_ok=True)
    path = directory / "anet"
    path.write_text('#!/bin/sh\nprintf "%s" "$0"; for a in "$@"; do printf " %s" "$a"; done; printf "\\n"\n')
    path.chmod(0o755)
    return str(path)


def system_anet():
    for d in ("/usr/local/bin", "/opt/homebrew/bin") + tuple(SYS_PATH.split(":")):
        if os.access(os.path.join(d, "anet"), os.X_OK):
            return True
    return False


@unittest.skipUnless(shutil.which("python3"), "the runner reads the host settings with python3")
@unittest.skipIf(system_anet(), "an anet executable sits in a directory that the runner searches")
class RunnerSettingsTests(unittest.TestCase):
    """anet-run.sh outside the MCP server: the plugin options come from the user settings of the host."""

    @classmethod
    def setUpClass(cls):
        ctx, _ = build_codebuddy()
        cls.tmp = tempfile.TemporaryDirectory()
        cls.plugin = Path(cls.tmp.name) / "plugin"
        write_tree(ctx, cls.plugin)
        cls.run_sh = str(cls.plugin / "bin" / "anet-run.sh")
        cls.mcp_sh = str(cls.plugin / "bin" / "anet-mcp.sh")

    @classmethod
    def tearDownClass(cls):
        cls.tmp.cleanup()

    def setUp(self):
        self.home = Path(tempfile.mkdtemp(prefix="home", dir=self.tmp.name))
        self.local = fake_anet(self.home / ".local" / "bin")

    def settings(self, data, directory=".codebuddy", raw=None):
        d = self.home / directory
        d.mkdir(parents=True, exist_ok=True)
        (d / "settings.json").write_text(raw if raw is not None else json.dumps(data))
        return d

    def run_script(self, script, *args, env=None):
        full = {"HOME": str(self.home), "PATH": SYS_PATH}
        full.update(env or {})
        p = subprocess.run(["/bin/sh", script, *args], env=full, capture_output=True, text=True, timeout=60)
        return p.returncode, p.stdout + p.stderr

    def entry(self, **options):
        return {"pluginConfigs": {"anet@agentnetwork": {"options": options}}}

    def test_without_settings_the_defaults_apply(self):
        code, out = self.run_script(self.run_sh, "status")
        self.assertEqual((code, out.strip()), (0, self.local + " status"))

    def test_identity_and_path_from_the_user_settings(self):
        custom = fake_anet(self.home / "custom")
        self.settings(self.entry(identity="work", anetPath=custom))
        code, out = self.run_script(self.run_sh, "status")
        self.assertEqual((code, out.strip()), (0, custom + " --id work status"))
        code, out = self.run_script(self.run_sh, "--resolve")
        self.assertEqual(code, 0)
        self.assertIn("executable: " + custom, out)
        self.assertIn("identity: work", out)

    def test_invalid_path_is_an_error_and_no_other_copy_is_used(self):
        self.settings(self.entry(anetPath=str(self.home / "nowhere" / "anet")))
        code, out = self.run_script(self.run_sh, "status")
        self.assertEqual(code, 1)
        self.assertIn("anetPath", out)
        self.assertNotIn(self.local, out)
        code, out = self.run_script(self.run_sh, "--resolve")
        self.assertEqual(code, 0)
        self.assertIn("executable: invalid setting anetPath=", out)

    def test_other_marketplace_name_is_accepted_when_unique(self):
        self.settings({"pluginConfigs": {"anet@codebuddy-plugins-official": {"options": {"identity": "work"}}}})
        code, out = self.run_script(self.run_sh, "status")
        self.assertEqual((code, out.strip()), (0, self.local + " --id work status"))

    def test_ambiguous_entries_select_nothing(self):
        self.settings({"pluginConfigs": {
            "anet@agentnetwork": {"options": {"identity": "one"}},
            "anet@other": {"options": {"identity": "two"}},
        }})
        code, out = self.run_script(self.run_sh, "status")
        self.assertEqual((code, out.strip()), (0, self.local + " status"))

    def test_entries_of_other_plugins_are_ignored(self):
        self.settings({"pluginConfigs": {"anetx@agentnetwork": {"options": {"identity": "no"}}, "other@m": {"options": {"identity": "no"}}}})
        code, out = self.run_script(self.run_sh, "status")
        self.assertEqual((code, out.strip()), (0, self.local + " status"))

    def test_malformed_settings_are_ignored(self):
        for raw in ("{not json", "[]", '{"pluginConfigs": []}', '{"pluginConfigs": {"anet@x": 3}}',
                    '{"pluginConfigs": {"anet@x": {"options": {"identity": 5, "anetPath": ["a"]}}}}'):
            self.settings(None, raw=raw)
            code, out = self.run_script(self.run_sh, "status")
            self.assertEqual((code, out.strip()), (0, self.local + " status"), raw)

    def test_config_directory_variables_select_the_file(self):
        self.settings(self.entry(identity="home"))
        other = self.settings(self.entry(identity="codebuddy-dir"), directory="cb-dir")
        code, out = self.run_script(self.run_sh, "x", env={"CODEBUDDY_CONFIG_DIR": str(other)})
        self.assertEqual(out.strip(), self.local + " --id codebuddy-dir x")
        wb = self.settings(self.entry(identity="workbuddy-dir"), directory="wb-dir")
        code, out = self.run_script(self.run_sh, "x", env={"CODEBUDDY_CONFIG_DIR": str(other), "WORKBUDDY_CONFIG_DIR": str(wb)})
        self.assertEqual(out.strip(), self.local + " --id workbuddy-dir x")
        # An explicit directory without an entry is final: the home directory is not consulted.
        empty = self.settings({}, directory="empty-dir")
        code, out = self.run_script(self.run_sh, "x", env={"WORKBUDDY_CONFIG_DIR": str(empty)})
        self.assertEqual(out.strip(), self.local + " x")

    def test_workbuddy_directory_is_the_fallback(self):
        self.settings({}, directory=".codebuddy")
        self.settings(self.entry(identity="wb"), directory=".workbuddy")
        code, out = self.run_script(self.run_sh, "x")
        self.assertEqual(out.strip(), self.local + " --id wb x")
        # Both hosts hold different options and the runner cannot tell which host started it: nothing is selected.
        self.settings(self.entry(identity="cb"), directory=".codebuddy")
        code, out = self.run_script(self.run_sh, "x")
        self.assertEqual(out.strip(), self.local + " x")
        # The same options in both files are not ambiguous.
        self.settings(self.entry(identity="wb"), directory=".codebuddy")
        code, out = self.run_script(self.run_sh, "x")
        self.assertEqual(out.strip(), self.local + " --id wb x")

    def test_the_mcp_launcher_uses_the_environment_and_not_the_file(self):
        custom = fake_anet(self.home / "custom")
        self.settings(self.entry(identity="file", anetPath=str(self.home / "nowhere" / "anet")))
        code, out = self.run_script(self.mcp_sh, env={"ANET_PLUGIN_BIN": custom, "ANET_PLUGIN_IDENTITY": "env"})
        self.assertEqual((code, out.strip()), (0, custom + " --id env mcp"))
        # An empty value (the default of an option that is not set) selects the defaults.
        code, out = self.run_script(self.mcp_sh, env={"ANET_PLUGIN_BIN": "", "ANET_PLUGIN_IDENTITY": ""})
        self.assertEqual((code, out.strip()), (0, self.local + " mcp"))

    def test_the_launcher_does_not_need_the_executable_bit(self):
        copy = Path(self.tmp.name) / "noexec"
        shutil.copytree(self.plugin, copy)
        for p in (copy / "bin").iterdir():
            p.chmod(0o644)
        code, out = self.run_script(str(copy / "bin" / "anet-mcp.sh"), env={"ANET_PLUGIN_BIN": "", "ANET_PLUGIN_IDENTITY": ""})
        self.assertEqual((code, out.strip()), (0, self.local + " mcp"))

    def test_settings_file_is_only_read_never_written(self):
        d = self.settings(self.entry(identity="work"))
        path = d / "settings.json"
        before = (path.read_bytes(), os.stat(path).st_mtime_ns)
        os.chmod(path, stat.S_IRUSR)
        self.run_script(self.run_sh, "status")
        self.run_script(self.run_sh, "--resolve")
        os.chmod(path, stat.S_IRUSR | stat.S_IWUSR)
        self.assertEqual((path.read_bytes(), os.stat(path).st_mtime_ns), before)
        self.assertEqual(sorted(p.name for p in d.iterdir()), ["settings.json"])


class TargetModuleTests(unittest.TestCase):
    def test_name_validation(self):
        ctx, _ = build_codebuddy()
        mod = ctx.module
        ctx.meta["name"] = "Anet_Plugin"
        with self.assertRaises(ValueError):
            mod.validate(ctx)
        ctx.meta["name"] = "anet"
        mod.validate(ctx)

    def test_no_text_of_another_target_leaks_in(self):
        ctx, _ = build_codebuddy()
        for path, f in ctx.files.items():
            if not path.endswith((".md", ".json", ".sh")):
                continue
            text = f.data.decode("utf-8")
            for word in ("ZCode", "ZCODE_", ".zcode", "CLAUDE_PLUGIN_ROOT", "CURSOR_PLUGIN_ROOT", ".claude-plugin", "{{"):
                self.assertNotIn(word, text, f"{path}: {word}")

    def test_public_content_rules(self):
        ctx, _ = build_codebuddy()
        for path, f in ctx.files.items():
            if not path.endswith((".md", ".json", ".sh")):
                continue
            text = f.data.decode("utf-8")
            self.assertNotRegex(text, r"/home/|/Users/|/tmp/", path)
            for ip in re.findall(r"\b\d{1,3}(?:\.\d{1,3}){3}\b", text):
                self.assertEqual(ip, "127.0.0.1", path)


CODEBUDDY = os.environ.get("ANET_CODEBUDDY_BIN")


@unittest.skipUnless(CODEBUDDY and Path(CODEBUDDY).exists(), "ANET_CODEBUDDY_BIN names no CodeBuddy executable")
class HostCliTests(unittest.TestCase):
    """The CodeBuddy CLI validates, adds and installs the generated tree in a scratch HOME."""

    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.TemporaryDirectory()
        cls.out = Path(cls.tmp.name) / "out"
        subprocess.run([sys.executable, str(PLUGINS / "build.py"), "--target", "codebuddy", "--out", str(cls.out)], check=True, capture_output=True)
        cls.home = Path(cls.tmp.name) / "home"
        cls.home.mkdir()
        cls.env = {
            "HOME": str(cls.home), "PATH": os.environ.get("PATH", SYS_PATH),
            "XDG_CONFIG_HOME": str(cls.home / ".config"), "XDG_DATA_HOME": str(cls.home / ".local/share"),
            "XDG_CACHE_HOME": str(cls.home / ".cache"), "XDG_STATE_HOME": str(cls.home / ".local/state"),
        }

    @classmethod
    def tearDownClass(cls):
        cls.tmp.cleanup()

    def cli(self, *args):
        p = subprocess.run([CODEBUDDY, *args], env=self.env, cwd=self.tmp.name, capture_output=True, text=True, timeout=180)
        return p.returncode, p.stdout + p.stderr

    def test_validate_plugin_and_marketplace(self):
        code, out = self.cli("plugin", "validate", str(self.out / "codebuddy"))
        self.assertEqual(code, 0, out)
        self.assertIn("Validation passed", out)
        code, out = self.cli("plugin", "validate", str(self.out))
        self.assertEqual(code, 0, out)
        self.assertIn("Validation passed", out)

    def test_add_install_list(self):
        code, out = self.cli("plugin", "marketplace", "add", str(self.out))
        self.assertEqual(code, 0, out)
        code, out = self.cli("plugin", "install", "anet@agentnetwork")
        self.assertEqual(code, 0, out)
        code, out = self.cli("plugin", "list")
        self.assertIn("anet@agentnetwork", out)
        self.assertIn("enabled", out)
        installed = self.home / ".codebuddy" / "plugins" / "cache" / "agentnetwork" / "anet"
        versions = [p.name for p in installed.iterdir()]
        self.assertEqual(versions, [json_of(build_codebuddy()[0], ".codebuddy-plugin/plugin.json")["version"]])
        self.assertTrue((installed / versions[0] / ".mcp.json").is_file())


if __name__ == "__main__":
    unittest.main()
