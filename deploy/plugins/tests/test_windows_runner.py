#!/usr/bin/env python3
"""Tests of the Windows runner of every target: python3 deploy/plugins/tests/test_windows_runner.py

bin/anet-run.cmd and bin/anet-mcp.cmd hand every decision to bin/anet-run.ps1
(see the comments of these files). The script is run here with PowerShell 7
(pwsh) in the way the .cmd files start it: as a script block made from the text
of the file, with its inputs in the environment. On Linux and macOS the script
treats an absolute POSIX path as absolute and searches for "anet" instead of
"anet.exe"; every other rule is the same as on Windows.

The tests need pwsh. They use the program named by ANET_PWSH, or pwsh on PATH,
and are skipped without one or when ANET_PWSH is "off". The .cmd files themselves need cmd.exe and are
checked statically by test_generator.py.
"""

import json
import os
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

PWSH = None if os.environ.get("ANET_PWSH") == "off" else os.environ.get("ANET_PWSH") or shutil.which("pwsh")
SCRIPT = '& ([scriptblock]::Create([IO.File]::ReadAllText($env:ANET_RUN_SCRIPT)))'


def runner_paths(ctx):
    return [rel for rel in ctx.files if rel.endswith("anet-run.ps1")]


@unittest.skipUnless(PWSH, "pwsh is needed (set ANET_PWSH)")
class WindowsRunnerTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.TemporaryDirectory()
        cls.out = Path(cls.tmp.name) / "out"
        res = g.build_all(None)
        g.write_tree(res.tree(), cls.out, owned_dirs=list(res.contexts))
        cls.contexts = res.contexts

    @classmethod
    def tearDownClass(cls):
        cls.tmp.cleanup()

    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        d = Path(self.dir.name)
        self.home = d / "home"
        self.home.mkdir()
        self.local = d / "local"
        self.local.mkdir()
        self.pathdir = d / "path"
        self.pathdir.mkdir()

    @staticmethod
    def fake(path):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text('#!/bin/sh\nif [ "${1:-}" = --version ]; then echo "anet 9.9.9 (fake)"; exit 0; fi\necho "FAKE $*"\n')
        path.chmod(0o755)
        return str(path)

    def run_ps(self, script, mode, env=None, path=None, cwd=None):
        e = {"HOME": str(self.home), "USERPROFILE": str(self.home), "LOCALAPPDATA": str(self.local),
             "PATH": path if path is not None else f"{self.pathdir}:/usr/bin:/bin",
             "ANET_RUN_SCRIPT": str(script), "ANET_RUN_SELF": "/plugins/anet/bin/anet-run.cmd", "ANET_RUN_MODE": mode,
             "DOTNET_CLI_TELEMETRY_OPTOUT": "1", "POWERSHELL_TELEMETRY_OPTOUT": "1"}
        e.update(env or {})
        r = subprocess.run([PWSH, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", SCRIPT], capture_output=True,
                           text=True, env=e, stdin=subprocess.DEVNULL, cwd=cwd or self.dir.name, timeout=120)
        return r.returncode, r.stdout, r.stderr

    def scripts(self):
        for key, ctx in sorted(self.contexts.items()):
            for rel in runner_paths(ctx):
                yield key, self.out / key / rel

    def test_shared_rules_for_every_target(self):
        n = 0
        for key, script in self.scripts():
            n += 1
            with self.subTest(target=key):
                self.setUp()  # fresh directories: the checks create fake executables
                self.check_shared_rules(script)
        self.assertEqual(n, len(self.contexts))

    def check_shared_rules(self, script):
        env = {"ANET_PLUGIN_SETTINGS": "env"}
        custom = self.fake(Path(self.dir.name) / "custom" / "anet")
        # The MCP environment selects the executable and the identity.
        code, out, err = self.run_ps(script, "locate", dict(env, ANET_PLUGIN_BIN=custom, ANET_PLUGIN_IDENTITY="work"))
        self.assertEqual((code, out.splitlines()), (0, ["E|" + custom, "I|work"]), err)
        # A relative anetPath is refused; no other copy is used.
        on_path = self.fake(self.pathdir / "anet")
        code, out, err = self.run_ps(script, "locate", dict(env, ANET_PLUGIN_BIN="anet"))
        self.assertEqual((code, out), (1, ""))
        self.assertIn("not an absolute path", err)
        # A missing anetPath is refused.
        code, out, err = self.run_ps(script, "locate", dict(env, ANET_PLUGIN_BIN=str(Path(self.dir.name) / "nowhere" / "anet")))
        self.assertEqual((code, out), (1, ""))
        self.assertIn("anetPath", err)
        # An identity that is not a plain name is refused: nothing reaches a command line of cmd.exe.
        for bad in ('a&calc', 'a"b', "a b", "-x", "a%PATH%", "x" * 65, "a\nb", "a^b", "a|b", "a)b"):
            code, out, err = self.run_ps(script, "locate", dict(env, ANET_PLUGIN_IDENTITY=bad))
            self.assertNotIn("I|", out, repr(bad))
            if "\n" not in bad:
                self.assertEqual(code, 1, repr(bad))
                self.assertIn("identity", err)
        # Without settings, PATH is searched.
        code, out, _ = self.run_ps(script, "locate", env)
        self.assertEqual((code, out.splitlines()), (0, ["E|" + on_path]))
        Path(on_path).unlink()
        # A relative directory of PATH and the working directory are never searched.
        work = Path(self.dir.name) / "workspace"
        self.fake(work / "anet")
        code, out, err = self.run_ps(script, "locate", env, path=f".:{work.name}:/usr/bin:/bin", cwd=work.parent)
        self.assertEqual((code, out), (1, ""))
        code, out, err = self.run_ps(script, "locate", env, path="/usr/bin:/bin", cwd=work)
        self.assertEqual((code, out), (1, ""))
        self.assertIn("anet-setup", err)
        # The install locations of install.ps1, in their order.
        third = self.fake(self.home / ".anet-bin" / "anet")
        self.assertEqual(self.run_ps(script, "locate", env)[1].splitlines(), ["E|" + third])
        second = self.fake(self.local / "anet" / "bin" / "anet")
        self.assertEqual(self.run_ps(script, "locate", env)[1].splitlines(), ["E|" + second])
        first = self.fake(self.local / "Programs" / "anet" / "anet")
        self.assertEqual(self.run_ps(script, "locate", env)[1].splitlines(), ["E|" + first])
        # A relative LOCALAPPDATA is not used.
        code, out, _ = self.run_ps(script, "locate", dict(env, LOCALAPPDATA="local"), cwd=self.dir.name)
        self.assertEqual(out.splitlines(), ["E|" + third])
        # --resolve: the four lines, exit status 0 in every case.
        code, out, err = self.run_ps(script, "resolve", env)
        self.assertEqual(code, 0, err)
        self.assertEqual(out.splitlines(), ["executable: " + first, "version: anet 9.9.9 (fake)", "identity: default",
                                            'runner: /plugins/anet/bin/anet-run.cmd'])
        code, out, _ = self.run_ps(script, "resolve", dict(env, ANET_PLUGIN_BIN="rel", ANET_PLUGIN_IDENTITY="a b"))
        self.assertEqual(code, 0)
        self.assertIn("executable: invalid setting anetPath=rel", out)
        self.assertIn("identity: invalid setting identity=a b", out)
        # The runner line is quoted when the path holds a space or another special character.
        code, out, _ = self.run_ps(script, "resolve", dict(env, ANET_RUN_SELF="/Application Support/it's/anet-run.cmd"))
        self.assertIn('runner: "/Application Support/it\'s/anet-run.cmd"', out)
        # Started without a mode (directly), the script refuses.
        code, out, err = self.run_ps(script, "", env)
        self.assertEqual((code, out), (1, ""))
        self.assertIn("anet-run.cmd", err)

    # -- the settings stores of the hosts that have one

    def target_script(self, key):
        return self.out / key / runner_paths(self.contexts[key])[0]

    def test_claude_code_reads_its_user_settings(self):
        script = self.target_script("claude-code")
        custom = self.fake(Path(self.dir.name) / "custom" / "anet")
        cfg = self.home / ".claude"
        cfg.mkdir()
        (cfg / "settings.json").write_text(json.dumps({
            "env": {"ANET_PLUGIN_IDENTITY": "no"},
            "pluginConfigs": {"other@x": {"options": {"identity": "no"}},
                              "anet@agentnetwork": {"options": {"anetPath": custom, "identity": "work"}}}}))
        self.assertEqual(self.run_ps(script, "locate")[1].splitlines(), ["E|" + custom, "I|work"])
        other = Path(self.dir.name) / "cfg"
        other.mkdir()
        (other / "settings.json").write_text(json.dumps({"pluginConfigs": {"anet@a": {"options": {"identity": "one"}},
                                                                          "anet@b": {"options": {"identity": "two"}}}}))
        self.fake(self.pathdir / "anet")
        out = self.run_ps(script, "locate", {"CLAUDE_CONFIG_DIR": str(other)})[1]
        self.assertEqual(out.splitlines(), ["E|" + str(self.pathdir / "anet")], "an ambiguous store selects nothing")
        for text in ("{not json", "[]", '{"pluginConfigs": []}', '{"pluginConfigs": {"anet@a": {"options": {"identity": 5}}}}'):
            (other / "settings.json").write_text(text)
            self.assertEqual(self.run_ps(script, "locate", {"CLAUDE_CONFIG_DIR": str(other)})[1].splitlines(),
                             ["E|" + str(self.pathdir / "anet")], text)

    def test_zcode_reads_its_configuration(self):
        script = self.target_script("zcode")
        custom = self.fake(Path(self.dir.name) / "custom" / "anet")
        cfg = self.home / ".zcode" / "cli"
        cfg.mkdir(parents=True)
        (cfg / "config.json").write_text(json.dumps({
            "provider": {"x": {"options": {"apiKey": "SECRET-KEY"}}},
            "plugins": {"options": {"anet@zcode-plugins-official": {"anetPath": custom, "identity": "work"}}}}))
        code, out, _ = self.run_ps(script, "locate")
        self.assertEqual(out.splitlines(), ["E|" + custom, "I|work"])
        code, out, _ = self.run_ps(script, "resolve")
        self.assertNotIn("SECRET-KEY", out)
        self.assertIn("identity: work", out)
        # In the MCP server the environment applies, not the file.
        self.fake(self.pathdir / "anet")
        out = self.run_ps(script, "locate", {"ANET_PLUGIN_SETTINGS": "env"})[1]
        self.assertEqual(out.splitlines(), ["E|" + str(self.pathdir / "anet")])

    def test_codebuddy_reads_both_hosts_and_refuses_a_conflict(self):
        script = self.target_script("codebuddy")
        self.fake(self.pathdir / "anet")
        for name, ident in ((".codebuddy", "cb"), (".workbuddy", "wb")):
            d = self.home / name
            d.mkdir()
            (d / "settings.json").write_text(json.dumps({"pluginConfigs": {"anet@agentnetwork": {"options": {"identity": ident}}}}))
        self.assertEqual(self.run_ps(script, "locate")[1].splitlines(), ["E|" + str(self.pathdir / "anet")])
        (self.home / ".workbuddy" / "settings.json").unlink()
        self.assertEqual(self.run_ps(script, "locate")[1].splitlines(), ["E|" + str(self.pathdir / "anet"), "I|cb"])
        self.assertEqual(self.run_ps(script, "locate", {"WORKBUDDY_CONFIG_DIR": str(self.home / ".workbuddy")})[1].splitlines(),
                         ["E|" + str(self.pathdir / "anet")])

    def test_trae_reads_the_settings_file_of_the_bundle(self):
        script = self.target_script("trae")
        custom = self.fake(Path(self.dir.name) / "custom" / "anet")
        settings = script.parent.parent / "settings"
        settings.write_text(f"anetPath={custom}\r\nidentity=work\r\nidentity=ignored\r\n")
        self.addCleanup(settings.unlink)
        self.assertEqual(self.run_ps(script, "locate")[1].splitlines(), ["E|" + custom, "I|work"])

    def test_gemini_cli_reads_the_environment_then_the_env_file(self):
        script = self.target_script("gemini-cli")
        custom = self.fake(Path(self.dir.name) / "custom" / "anet")
        dotenv = script.parent.parent / ".env"
        dotenv.write_text(f'ANET_PLUGIN_BIN="{custom}"\nANET_PLUGIN_IDENTITY=\'work\'\n')
        self.addCleanup(dotenv.unlink)
        self.assertEqual(self.run_ps(script, "locate")[1].splitlines(), ["E|" + custom, "I|work"])
        self.assertEqual(self.run_ps(script, "locate", {"ANET_PLUGIN_IDENTITY": "env"})[1].splitlines(), ["E|" + custom, "I|env"])



@unittest.skipUnless(PWSH, "pwsh is needed (set ANET_PWSH)")
class TraeWindowsInstallerTests(unittest.TestCase):
    """install.ps1 of the trae bundle. It refuses to run outside Windows, so a copy with the platform check set to
    Windows is run; paths use the separator of the test platform, settings use Windows forms (strings only)."""

    WIN_CHECK = "$OnWindows = ([Environment]::OSVersion.Platform -eq [PlatformID]::Win32NT)"

    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.TemporaryDirectory()
        res = g.build_all(["trae"])
        cls.ctx = res.contexts["trae"]
        cls.bundle = Path(cls.tmp.name) / "bundle"
        g.write_tree(cls.ctx.files, cls.bundle, force=True)
        text = (cls.bundle / "install.ps1").read_text(encoding="utf-8")
        assert text.count(cls.WIN_CHECK) == 1
        (cls.bundle / "install-test.ps1").write_text(text.replace(cls.WIN_CHECK, "$OnWindows = $true"), encoding="utf-8")

    @classmethod
    def tearDownClass(cls):
        cls.tmp.cleanup()

    def setUp(self):
        d = tempfile.TemporaryDirectory()
        self.addCleanup(d.cleanup)
        self.home = Path(d.name) / "home"
        self.home.mkdir()
        self.work = Path(d.name)

    def run_install(self, *args, script="install-test.ps1"):
        env = {"HOME": str(self.home), "USERPROFILE": str(self.home), "PATH": "/usr/bin:/bin",
               "DOTNET_CLI_TELEMETRY_OPTOUT": "1", "POWERSHELL_TELEMETRY_OPTOUT": "1"}
        r = subprocess.run([PWSH, "-NoLogo", "-NoProfile", "-NonInteractive", "-File", str(self.bundle / script), *args],
                           capture_output=True, text=True, env=env, stdin=subprocess.DEVNULL, cwd=str(self.work), timeout=120)
        return r.returncode, r.stdout, r.stderr

    @staticmethod
    def entry(out):
        start = out.index("{\n")
        end = out.index("\n}\n", start) + 2
        return json.loads(out[start:end])

    def test_the_original_refuses_outside_windows(self):
        code, out, err = self.run_install(script="install.ps1")
        self.assertEqual((code, out), (1, ""))
        self.assertIn("sh install.sh", err)

    def test_user_scope_installs_and_prints_the_entry(self):
        code, out, err = self.run_install()
        self.assertEqual(code, 0, err)
        root = self.home / ".trae"
        for rel in ("skills/anet/SKILL.md", "commands/anet.md", "anet/bin/anet-run.cmd", "anet/bin/anet-run.ps1",
                    "anet/bin/anet-mcp.cmd", "anet/bin/anet-run.sh"):
            self.assertEqual((root / rel).read_bytes(), (self.bundle / rel).read_bytes(), rel)
        self.assertFalse((root / "anet" / "settings").exists(), "no settings file without settings")
        server = self.entry(out)["mcpServers"]["anet"]
        self.assertEqual(server["command"], str(root / "anet" / "bin" / "anet-mcp.cmd"))
        self.assertEqual(server["args"], [])
        self.assertEqual(set(server["env"]), {"RUN_MCP_TIMEOUT_MS"})
        link = [line for line in out.splitlines() if "trae://" in line][0].strip()
        import base64
        import urllib.parse
        config = urllib.parse.unquote(link.split("&config=")[1])
        self.assertEqual(json.loads(base64.b64decode(config)), server)
        code, out, _ = self.run_install("-Cn")
        self.assertEqual(code, 0)
        self.assertTrue((self.home / ".trae-cn" / "commands" / "anet-status.md").is_file())
        self.assertNotIn("trae://", out)

    def test_settings_are_stored_kept_and_cleared(self):
        code, out, err = self.run_install("-Identity", "work", "-Anet", "C:\\Program Files\\anet\\anet.exe")
        self.assertEqual(code, 0, err)
        settings = self.home / ".trae" / "anet" / "settings"
        self.assertEqual(settings.read_text(), "anetPath=C:\\Program Files\\anet\\anet.exe\nidentity=work\n")
        self.assertEqual(self.entry(out)["mcpServers"]["anet"]["env"],
                         {"RUN_MCP_TIMEOUT_MS": str(self.ctx.meta["mcp"]["timeout_ms"]),
                          "ANET_PLUGIN_BIN": "C:\\Program Files\\anet\\anet.exe", "ANET_PLUGIN_IDENTITY": "work"})
        code, out, _ = self.run_install()
        self.assertEqual(code, 0)
        self.assertIn("anetPath=C:\\Program Files\\anet\\anet.exe", settings.read_text())
        code, out, _ = self.run_install("-ClearIdentity", "-ClearAnet")
        self.assertEqual(code, 0)
        self.assertEqual(settings.read_text(), "anetPath=\nidentity=\n")
        self.assertEqual(set(self.entry(out)["mcpServers"]["anet"]["env"]), {"RUN_MCP_TIMEOUT_MS"})

    def test_invalid_settings_are_refused(self):
        for args in (("-Identity", "a&b"), ("-Identity", "-x"), ("-Anet", "anet.exe"), ("-Anet", "C:\\anet\\anet"),
                     ("-Identity", "a", "-ClearIdentity")):
            code, out, err = self.run_install(*args)
            self.assertEqual(code, 1, args)
            self.assertFalse((self.home / ".trae" / "anet" / "settings").exists(), args)

    def test_a_foreign_file_is_not_replaced(self):
        target = self.home / ".trae" / "commands" / "anet.md"
        target.parent.mkdir(parents=True)
        target.write_text("my own command\n")
        code, out, err = self.run_install()
        self.assertEqual(code, 1)
        self.assertIn("not part of the anet bundle", err)
        self.assertEqual(target.read_text(), "my own command\n")
        code, out, err = self.run_install("-Remove")
        self.assertEqual(code, 1)
        self.assertEqual(target.read_text(), "my own command\n")

    def test_project_scope_writes_mcp_json_once(self):
        project = self.work / "proj"
        project.mkdir()
        code, out, err = self.run_install("-Project", str(project))
        self.assertEqual(code, 0, err)
        mcp = project / ".trae" / "mcp.json"
        server = json.loads(mcp.read_text())["mcpServers"]["anet"]
        self.assertEqual(server["command"], str(project / ".trae" / "anet" / "bin" / "anet-mcp.cmd"))
        mcp.write_text("{}\n")
        code, out, _ = self.run_install("-Project", str(project), "-Identity", "work")
        self.assertEqual(code, 0)
        self.assertEqual(mcp.read_text(), "{}\n")
        self.assertIn("exists and was not changed", out)
        self.assertEqual(self.run_install("-Project", str(project), "-Cn")[0], 1)

    def test_a_link_in_the_project_is_refused(self):
        project = self.work / "proj"
        (project / ".trae").mkdir(parents=True)
        outside = self.work / "outside"
        outside.mkdir()
        (project / ".trae" / "skills").symlink_to(outside, target_is_directory=True)
        code, out, err = self.run_install("-Project", str(project))
        self.assertEqual(code, 1)
        self.assertIn("link", err)
        self.assertEqual(list(outside.iterdir()), [])

    def test_remove_deletes_the_files_and_empty_directories(self):
        self.assertEqual(self.run_install("-Identity", "work")[0], 0)
        other = self.home / ".trae" / "skills" / "mine" / "SKILL.md"
        other.parent.mkdir(parents=True)
        other.write_text("x")
        code, out, err = self.run_install("-Remove")
        self.assertEqual(code, 0, err)
        self.assertFalse((self.home / ".trae" / "anet").exists())
        self.assertFalse((self.home / ".trae" / "commands").exists())
        self.assertTrue(other.is_file(), "a file of another skill stays")
        self.assertEqual(self.run_install("-Remove", "-Identity", "x")[0], 1)


if __name__ == "__main__":
    unittest.main()
