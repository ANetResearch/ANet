#!/usr/bin/env python3
"""Tests of the antigravity and gemini-cli targets: python3 deploy/plugins/tests/test_antigravity.py

They build both targets into memory and check

  * the manifest rules of the two hosts (Antigravity: plugin.json with the keys of
    the documented schema and a valid name; Gemini CLI: gemini-extension.json
    with name, version, the documented keys and settings),
  * the generated files (skills, the TOML command, no Markdown command, the
    wording the targets rewrite, the variable syntax that Gemini CLI processes),
  * the shell program that mcp_config.json runs (where it looks for the plugin
    and where it must not), and the settings that the runner reads from the
    .env file of a Gemini CLI extension,
  * the release archives of gemini-cli.

If Gemini CLI is installed in a sandbox, its own validator is run on the tree:

    ANET_GEMINI_RUN=<script that runs gemini with a sandbox HOME> python3 deploy/plugins/tests/test_antigravity.py
"""

import io
import json
import os
import re
import subprocess
import sys
import tarfile
import tempfile
import tomllib
import unittest
from pathlib import Path

sys.dont_write_bytecode = True
PLUGINS = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(PLUGINS))

import generator as g  # noqa: E402

PLUGIN_KEYS = {"$schema", "name", "description"}  # additionalProperties is false in the documented schema
GEMINI_KEYS = {"name", "version", "description", "mcpServers", "contextFileName", "excludeTools", "migratedTo", "plan", "settings", "themes"}
SETTING_KEYS = {"name", "description", "envVar", "sensitive"}


def build_both():
    res = g.build_all(["antigravity", "gemini-cli"])
    return res.contexts["antigravity"], res.contexts["gemini-cli"], res


def text_of(ctx, path):
    return ctx.files[path].data.decode("utf-8")


def json_of(ctx, path):
    return json.loads(text_of(ctx, path))


def write_stub(path, label):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(f'#!/bin/sh\nprintf "{label}:%s\\n" "$*"\n')
    path.chmod(0o755)


class AntigravityManifestTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.ctx, _, cls.res = build_both()
        cls.meta = cls.ctx.meta

    def test_plugin_json_has_only_the_documented_keys(self):
        plugin = json_of(self.ctx, "plugin.json")
        self.assertEqual(set(plugin), PLUGIN_KEYS)
        self.assertRegex(plugin["name"], r"^[a-zA-Z0-9_-]+$")
        self.assertEqual(plugin["name"], self.meta["name"])
        self.assertEqual(plugin["description"], self.meta["description"])

    def test_component_layout(self):
        names = set(self.ctx.files)
        self.assertLessEqual({"plugin.json", "mcp_config.json", "bin/anet-run.sh", "bin/anet-mcp.sh", "LICENSE", "README.md"}, names)
        self.assertEqual(
            {n.split("/")[1] for n in names if n.startswith("skills/")}, {"anet", "anet-setup", "anet-status"}
        )
        # Antigravity documents no command component and no settings component.
        self.assertFalse([n for n in names if n.startswith("commands/")])
        self.assertNotIn("hooks.json", names)
        self.assertFalse(self.ctx.root_files)

    def test_skill_names_match_their_directories(self):
        for name in ("anet", "anet-setup", "anet-status"):
            fields, body = g.split_frontmatter(text_of(self.ctx, f"skills/{name}/SKILL.md"))
            self.assertEqual(fields["name"], name)
            self.assertTrue(fields["description"])
            self.assertTrue(body.strip())

    def test_mcp_config_shape(self):
        cfg = json_of(self.ctx, "mcp_config.json")
        self.assertEqual(list(cfg), ["mcpServers"])
        server = cfg["mcpServers"]["anet"]
        self.assertEqual(set(server), {"command", "args"})  # keys of a stdio server; serverUrl is for remote servers
        self.assertEqual(server["command"], "sh")
        self.assertEqual(server["args"][0], "-c")

    def test_runner_is_named_through_the_skill_directory(self):
        for name in ("anet", "anet-setup", "anet-status"):
            text = text_of(self.ctx, f"skills/{name}/SKILL.md")
            self.assertIn("<skill-dir>/../../bin/anet-run.sh", text)
            self.assertNotIn("{{", text)
            self.assertNotIn("${", text)
        for text in (text_of(self.ctx, "README.md"), text_of(self.ctx, "README_CN.md")):
            self.assertIn("anet-status", text)
            self.assertNotIn("/anet-status", text)  # there is no slash command
            self.assertNotIn("{{", text)

    def test_setup_skill_keeps_the_safety_rules(self):
        text = text_of(self.ctx, "skills/anet-setup/SKILL.md")
        self.assertIn('sh "<skill-dir>/../../bin/anet-install.sh"', text)
        self.assertNotIn("mktemp", text)
        self.assertIn("Never read, print or copy files under `~/.anet`", text)
        self.assertIn("Do not run them on the user's behalf", text)
        self.assertIn("Never read or edit the configuration or credential files of Antigravity", text)
        self.assertIn("On Windows, stop", text)
        self.assertNotIn("ZCode", text)

    def test_no_text_of_another_host(self):
        for path, f in self.ctx.files.items():
            if isinstance(f.data, bytes) and path != "LICENSE":
                text = f.data.decode("utf-8", "ignore")
                for word in ("Gemini CLI", "gemini-extension", "extensionPath", "CLAUDE_", "CURSOR_"):
                    self.assertNotIn(word, text, f"{word} in {path}")


class LauncherProgramTests(unittest.TestCase):
    """The sh program of mcp_config.json."""

    @classmethod
    def setUpClass(cls):
        cls.ctx, _, _ = build_both()
        cls.program = json_of(cls.ctx, "mcp_config.json")["mcpServers"]["anet"]["args"][1]
        cls.tmp = tempfile.TemporaryDirectory()
        cls.root = Path(cls.tmp.name)
        cls.home = cls.root / "home"
        write_stub(cls.home / ".local" / "bin" / "anet", "ARGS")
        cls.plugin_files = {p: f for p, f in cls.ctx.files.items()}

    @classmethod
    def tearDownClass(cls):
        cls.tmp.cleanup()

    def install(self, where, label=None):
        g.write_tree(self.plugin_files, where, force=True)
        return where

    def run_program(self, cwd, env_extra=None):
        env = {"HOME": str(self.home), "PATH": "/usr/bin:/bin"}
        env.update(env_extra or {})
        cwd.mkdir(parents=True, exist_ok=True)
        return subprocess.run(
            ["sh", "-c", self.program, "anet-mcp"], cwd=cwd, env=env, capture_output=True, text=True, timeout=30
        )

    def fresh(self, name):
        d = self.root / name
        d.mkdir()
        return d

    def test_finds_the_global_plugin_directory(self):
        self.install(self.home / ".gemini" / "config" / "plugins" / "anet")
        r = self.run_program(self.fresh("w1"))
        self.assertEqual((r.returncode, r.stdout), (0, "ARGS:mcp\n"), r.stderr)

    def test_finds_the_cli_staging_directory(self):
        import shutil

        shutil.rmtree(self.home / ".gemini", ignore_errors=True)
        self.install(self.home / ".gemini" / "antigravity-cli" / "plugins" / "anet")
        r = self.run_program(self.fresh("w2"))
        self.assertEqual((r.returncode, r.stdout), (0, "ARGS:mcp\n"), r.stderr)

    def test_workspace_plugin_directory_is_not_run(self):
        # A workspace can come from an untrusted source: .agents/plugins/anet of the working
        # directory is not a place where the launcher looks, only ANTIGRAVITY_PLUGIN_ROOT can name it.
        import shutil

        shutil.rmtree(self.home / ".gemini", ignore_errors=True)
        w = self.fresh("w3")
        self.install(w / ".agents" / "plugins" / "anet")
        r = self.run_program(w)
        self.assertEqual((r.returncode, r.stdout), (1, ""), r.stderr)
        self.assertIn("plugin directory was not found", r.stderr)
        r = self.run_program(w, {"ANTIGRAVITY_PLUGIN_ROOT": str(w / ".agents" / "plugins" / "anet")})
        self.assertEqual((r.returncode, r.stdout), (0, "ARGS:mcp\n"), r.stderr)

    def test_unset_home_does_not_probe_the_root_directory(self):
        import shutil

        shutil.rmtree(self.home / ".gemini", ignore_errors=True)
        r = subprocess.run(["sh", "-c", self.program, "anet-mcp"], cwd=self.fresh("w7"), env={"PATH": "/usr/bin:/bin"},
                           capture_output=True, text=True, timeout=30)
        self.assertEqual((r.returncode, r.stdout), (1, ""))
        self.assertIn("plugin directory was not found", r.stderr)

    def test_host_variable_wins(self):
        import shutil

        shutil.rmtree(self.home / ".gemini", ignore_errors=True)
        elsewhere = self.install(self.root / "elsewhere" / "anet-copy")
        r = self.run_program(self.fresh("w4"), {"ANTIGRAVITY_PLUGIN_ROOT": str(elsewhere)})
        self.assertEqual((r.returncode, r.stdout), (0, "ARGS:mcp\n"), r.stderr)

    def test_working_directory_is_not_a_plugin_directory(self):
        # A workspace that carries its own bin/anet-mcp.sh must not be run by the launcher.
        import shutil

        shutil.rmtree(self.home / ".gemini", ignore_errors=True)
        w = self.fresh("w5")
        (w / "bin").mkdir()
        (w / "bin" / "anet-mcp.sh").write_text('#!/bin/sh\necho SPOOFED\n')
        r = self.run_program(w)
        self.assertEqual(r.returncode, 1)
        self.assertEqual(r.stdout, "")
        self.assertIn("plugin directory was not found", r.stderr)

    def test_environment_variables_reach_the_runner(self):
        other = self.root / "other-anet"
        write_stub(other, "OTHER")
        w = self.fresh("w6")
        plugin = self.install(self.root / "elsewhere6" / "anet-copy")
        r = self.run_program(w, {"ANTIGRAVITY_PLUGIN_ROOT": str(plugin), "ANET_PLUGIN_BIN": str(other), "ANET_PLUGIN_IDENTITY": "work"})
        self.assertEqual(r.stdout, "OTHER:--id work mcp\n", r.stderr)
        r = self.run_program(w, {"ANTIGRAVITY_PLUGIN_ROOT": str(plugin), "ANET_PLUGIN_BIN": str(self.root / "missing")})
        self.assertEqual(r.returncode, 1)
        self.assertIn("not an executable file", r.stderr)


class InstallerScriptTests(unittest.TestCase):
    """bin/anet-install.sh: the installer step as a plain command (Gemini CLI refuses command substitution in a shell command)."""

    @classmethod
    def setUpClass(cls):
        cls.agy, cls.gem, _ = build_both()

    def test_both_targets_ship_the_same_executable_script(self):
        for ctx in (self.agy, self.gem):
            f = ctx.files["bin/anet-install.sh"]
            self.assertTrue(f.executable)
            self.assertEqual(f.data, self.agy.files["bin/anet-install.sh"].data)

    def test_no_command_substitution_in_any_command_of_a_skill(self):
        for ctx in (self.agy, self.gem):
            for name in ("anet", "anet-setup", "anet-status"):
                text = text_of(ctx, f"skills/{name}/SKILL.md")
                for block in re.findall(r"```(?:bash|sh)\n(.*?)```", text, re.S):
                    self.assertNotIn("$(", block, f"{name}: {block}")
                    self.assertNotIn("`", block, f"{name}: {block}")

    def test_skills_forbid_workarounds_when_the_host_refuses_a_command(self):
        for ctx in (self.agy, self.gem):
            self.assertIn("do not write copies or wrapper scripts", text_of(ctx, "skills/anet-setup/SKILL.md"))
            self.assertIn("do not write copies or wrapper scripts", text_of(ctx, "skills/anet-status/SKILL.md"))

    def run_script(self, curl_body, *args):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            stubs = root / "stubs"
            stubs.mkdir()
            (stubs / "curl").write_text("#!/bin/sh\n" + curl_body)
            (stubs / "curl").chmod(0o755)
            tmp = root / "tmp"
            tmp.mkdir()
            script = root / "anet-install.sh"
            script.write_bytes(self.gem.files["bin/anet-install.sh"].data)
            env = {"PATH": f"{stubs}:/usr/bin:/bin", "TMPDIR": str(tmp), "HOME": str(root)}
            r = subprocess.run(["sh", str(script), *args], env=env, capture_output=True, text=True, timeout=30)
            return r, sorted(p.name for p in tmp.iterdir())

    def test_runs_the_downloaded_installer_and_removes_the_directory(self):
        body = 'while [ $# -gt 0 ]; do [ "$1" = -o ] && out=$2; shift; done\n' \
               'printf \'echo "installer ran: $*"\\n\' > "$out"\n'
        r, left = self.run_script(body, "--system")
        self.assertEqual((r.returncode, r.stdout), (0, "installer ran: --system\n"), r.stderr)
        self.assertEqual(left, [])

    def test_installer_exit_status_is_returned(self):
        body = 'while [ $# -gt 0 ]; do [ "$1" = -o ] && out=$2; shift; done\nprintf \'exit 7\\n\' > "$out"\n'
        r, left = self.run_script(body)
        self.assertEqual(r.returncode, 7)
        self.assertEqual(left, [])

    def test_download_failure_runs_nothing(self):
        r, left = self.run_script("exit 22\n")
        self.assertEqual(r.returncode, 1)
        self.assertEqual(r.stdout, "")
        self.assertTrue(r.stderr.startswith("Error: the installer could not be downloaded."), r.stderr)
        self.assertEqual(left, [])

    def test_uses_https_and_the_official_address(self):
        text = self.gem.files["bin/anet-install.sh"].data.decode()
        self.assertIn("--proto '=https' --tlsv1.2", text)
        self.assertIn("https://agentnetwork.org.cn/install.sh", text)


class GeminiManifestTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        _, cls.ctx, cls.res = build_both()
        cls.meta = cls.ctx.meta
        cls.manifest = json_of(cls.ctx, "gemini-extension.json")

    def test_required_fields_and_name(self):
        m = self.manifest
        self.assertLessEqual(set(m), GEMINI_KEYS)
        self.assertRegex(m["name"], r"^[a-zA-Z0-9-]+$")  # underscores are rejected by Gemini CLI
        self.assertRegex(m["version"], r"^\d+\.\d+\.\d+$")
        self.assertEqual((m["name"], m["version"]), (self.meta["name"], self.meta["version"]))
        self.assertTrue(m["description"])

    def test_mcp_server(self):
        server = self.manifest["mcpServers"]["anet"]
        self.assertEqual(server["command"], "sh")
        self.assertEqual(server["args"], ["${extensionPath}/bin/anet-mcp.sh"])
        self.assertNotIn("trust", server)  # extensions cannot set trust
        self.assertNotIn("env", server)

    def test_settings_declare_the_runner_variables(self):
        settings = self.manifest["settings"]
        self.assertEqual([s["envVar"] for s in settings], [s["env"] for s in self.meta["settings"]])
        for s in settings:
            self.assertEqual(set(s), SETTING_KEYS)
            self.assertFalse(s["sensitive"])

    def test_manifest_has_no_dollar_except_the_extension_path(self):
        # Gemini CLI resolves $VAR and ${VAR} in the manifest from the environment.
        text = text_of(self.ctx, "gemini-extension.json")
        self.assertEqual(re.findall(r"\$\{?[A-Za-z_/]+\}?", text), ["${extensionPath}", "${extensionPath}"])

    def test_layout(self):
        names = set(self.ctx.files)
        self.assertEqual(
            {n.split("/")[1] for n in names if n.startswith("skills/")}, {"anet", "anet-setup", "anet-status"}
        )
        self.assertIn("commands/anet-status.toml", names)
        self.assertNotIn("commands/anet-status.md", names)  # Gemini CLI reads TOML commands only
        self.assertFalse(self.ctx.root_files)

    def test_command_toml(self):
        data = tomllib.loads(text_of(self.ctx, "commands/anet-status.toml"))
        self.assertEqual(set(data), {"description", "prompt"})
        self.assertIn("anet-status", data["prompt"])
        self.assertIn("{{args}}", data["prompt"])
        self.assertNotIn("!{", data["prompt"])
        self.assertNotIn("@{", data["prompt"])

    def test_skills_use_only_the_extension_path_variable(self):
        # Gemini CLI replaces ${name} in every string of a skill and leaves unknown names alone;
        # only ${extensionPath} is intended.
        for name in ("anet", "anet-setup", "anet-status"):
            text = text_of(self.ctx, f"skills/{name}/SKILL.md")
            for var in re.findall(r"\$\{[^}]*\}", text):
                self.assertEqual(var, "${extensionPath}", f"{name}: {var}")
            self.assertIn('sh "${extensionPath}/bin/anet-run.sh"', text)
            fields, _ = g.split_frontmatter(text)
            self.assertEqual(fields["name"], name)

    def test_setup_skill_rules(self):
        text = text_of(self.ctx, "skills/anet-setup/SKILL.md")
        self.assertIn('sh "${extensionPath}/bin/anet-install.sh"', text)
        self.assertNotIn("mktemp", text)
        self.assertIn("Never read or edit the Gemini CLI configuration", text)
        self.assertIn("do not open\n  the `.env` file of the extension", text)
        self.assertIn("`ANET_PLUGIN_BIN`", text)
        self.assertNotIn("Antigravity", text)

    def test_readme_names_the_command_and_settings(self):
        text = text_of(self.ctx, "README.md")
        self.assertIn("`/anet-status`", text)
        self.assertIn("gemini extensions install", text)
        self.assertIn("gemini extensions config anet", text)
        self.assertNotIn("{{", text)


class GeminiRunnerSettingsTests(unittest.TestCase):
    """bin/anet-run.sh with the settings of a Gemini CLI extension (.env)."""

    @classmethod
    def setUpClass(cls):
        _, cls.ctx, _ = build_both()
        cls.tmp = tempfile.TemporaryDirectory()
        root = Path(cls.tmp.name)
        cls.home = root / "home"
        cls.plugin = cls.home / ".gemini" / "extensions" / "anet"
        g.write_tree(dict(cls.ctx.files), cls.plugin, force=True)
        write_stub(cls.home / ".local" / "bin" / "anet", "DEFAULT")
        cls.other = root / "my anet"  # a space makes Gemini CLI quote the value in .env
        write_stub(cls.other, "OTHER")
        cls.third = root / "third"
        write_stub(cls.third, "THIRD")

    @classmethod
    def tearDownClass(cls):
        cls.tmp.cleanup()

    def setUp(self):
        env_file = self.plugin / ".env"
        if env_file.exists():
            env_file.unlink()

    def run_runner(self, *args, env_extra=None, script="anet-run.sh"):
        env = {"HOME": str(self.home), "PATH": "/usr/bin:/bin"}
        env.update(env_extra or {})
        return subprocess.run(["sh", str(self.plugin / "bin" / script), *args], env=env, capture_output=True, text=True, timeout=30)

    def dotenv(self, text):
        (self.plugin / ".env").write_text(text)

    def test_without_settings_the_default_is_used(self):
        r = self.run_runner("status")
        self.assertEqual(r.stdout, "DEFAULT:status\n", r.stderr)

    def test_quoted_value_with_a_space_and_identity(self):
        self.dotenv(f'ANET_PLUGIN_BIN="{self.other}"\nANET_PLUGIN_IDENTITY=work\n')
        r = self.run_runner("status")
        self.assertEqual(r.stdout, "OTHER:--id work status\n", r.stderr)

    def test_escaped_quote_and_backslash_are_restored(self):
        odd = Path(self.tmp.name) / 'q"b\\ s'
        write_stub(odd, "ODD")
        escaped = str(odd).replace("\\", "\\\\").replace('"', '\\"')
        self.dotenv(f'ANET_PLUGIN_BIN="{escaped}"\n')
        r = self.run_runner("status")
        self.assertEqual(r.stdout, "ODD:status\n", r.stderr)

    def test_environment_wins_over_the_file(self):
        self.dotenv(f'ANET_PLUGIN_BIN="{self.other}"\nANET_PLUGIN_IDENTITY=work\n')
        r = self.run_runner("status", env_extra={"ANET_PLUGIN_BIN": str(self.third)})
        self.assertEqual(r.stdout, "THIRD:--id work status\n", r.stderr)

    def test_invalid_path_in_the_file_is_an_error_not_a_fallback(self):
        self.dotenv("ANET_PLUGIN_BIN=/nonexistent/anet\n")
        r = self.run_runner("status")
        self.assertEqual(r.returncode, 1)
        self.assertEqual(r.stdout, "")
        self.assertIn("not an executable file", r.stderr)
        r = self.run_runner("--resolve")
        self.assertEqual(r.returncode, 0)
        self.assertIn("executable: invalid setting anetPath=/nonexistent/anet", r.stdout)

    def test_only_the_two_keys_are_read(self):
        self.dotenv(f"OTHER_KEY=1\nANET_PLUGIN_BIN_X={self.other}\n# ANET_PLUGIN_BIN={self.other}\n")
        r = self.run_runner("status")
        self.assertEqual(r.stdout, "DEFAULT:status\n", r.stderr)

    def test_mcp_launcher_uses_the_environment_only(self):
        # Gemini CLI passes the settings of the extension in the environment of the MCP server.
        self.dotenv(f'ANET_PLUGIN_BIN="{self.other}"\n')
        r = self.run_runner(script="anet-mcp.sh")
        self.assertEqual(r.stdout, "DEFAULT:mcp\n", r.stderr)
        r = self.run_runner(script="anet-mcp.sh", env_extra={"ANET_PLUGIN_BIN": str(self.third)})
        self.assertEqual(r.stdout, "THIRD:mcp\n", r.stderr)

    def test_the_file_of_another_copy_is_not_read(self):
        # Only the .env next to the running script counts; a copy in the Gemini home is another installation.
        linked = Path(self.tmp.name) / "linked"
        g.write_tree(dict(self.ctx.files), linked, force=True)
        self.dotenv(f"ANET_PLUGIN_BIN={self.third}\n")
        env = {"HOME": str(self.home), "PATH": "/usr/bin:/bin"}
        r = subprocess.run(["sh", str(linked / "bin" / "anet-run.sh"), "status"], env=env, capture_output=True, text=True, timeout=30)
        self.assertEqual(r.stdout, "DEFAULT:status\n", r.stderr)


class GeminiSiteTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        _, cls.ctx, cls.res = build_both()
        cls.site = g.build_site(cls.res, "https://example.org")

    def test_assets_are_named_by_platform(self):
        paths = {p for p in self.site if p.startswith("plugins/gemini-cli/")}
        self.assertEqual(
            paths, {"plugins/gemini-cli/linux.anet.tar.gz", "plugins/gemini-cli/darwin.anet.tar.gz", "plugins/gemini-cli/SHA256SUMS"}
        )
        self.assertFalse([p for p in self.site if p.startswith("plugins/antigravity")])

    def test_archive_has_the_manifest_at_its_root(self):
        data = self.site["plugins/gemini-cli/linux.anet.tar.gz"].data
        with tarfile.open(fileobj=io.BytesIO(data), mode="r:gz") as tf:
            names = tf.getnames()
            self.assertIn("gemini-extension.json", names)
            self.assertIn("skills/anet-setup/SKILL.md", names)
            self.assertIn("commands/anet-status.toml", names)
            self.assertEqual(sorted(names), sorted(set(self.ctx.files)))
        self.assertEqual(data, self.site["plugins/gemini-cli/darwin.anet.tar.gz"].data)

    def test_checksums_match(self):
        sums = self.site["plugins/gemini-cli/SHA256SUMS"].data.decode()
        for line in sums.strip().splitlines():
            digest, name = line.split("  ")
            self.assertEqual(digest, g.sha256_hex(self.site[f"plugins/gemini-cli/{name}"].data))


@unittest.skipUnless(os.environ.get("ANET_GEMINI_RUN"), "set ANET_GEMINI_RUN to a script that runs gemini in a sandbox HOME")
class GeminiValidatorTests(unittest.TestCase):
    def test_gemini_validates_the_tree(self):
        _, ctx, _ = build_both()
        with tempfile.TemporaryDirectory() as d:
            g.write_tree(dict(ctx.files), Path(d) / "gemini-cli", force=True)
            r = subprocess.run(
                [os.environ["ANET_GEMINI_RUN"], "extensions", "validate", str(Path(d) / "gemini-cli")],
                capture_output=True, text=True, timeout=120, stdin=subprocess.DEVNULL,
                env=dict(os.environ, GEMINI_CLI_TRUST_WORKSPACE="true"),
            )
        self.assertIn("successfully validated", r.stdout + r.stderr)


if __name__ == "__main__":
    unittest.main()
