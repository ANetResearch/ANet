#!/usr/bin/env python3
"""Tests of the trae target: python3 deploy/plugins/tests/test_trae.py

Trae is a graphical application with no command line, so nothing here starts
it. The tests check what can be checked: the generated files against the rules
of the Trae documentation (MCP entry, skill layout, the single variable
${workspaceFolder}, no spaces in a command), the wording of the shared texts
that the target rewrites, and the behaviour of install.sh, the runner and the
launcher, which are run with every POSIX shell that the host provides.
"""

import base64
import json
import os
import re
import shutil
import stat
import subprocess
import sys
import tempfile
import unittest
import urllib.parse
import zipfile
from pathlib import Path

sys.dont_write_bytecode = True
PLUGINS = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(PLUGINS))

import generator as g  # noqa: E402

SHELLS = [s for s in ("sh", "dash", "bash") if shutil.which(s)]
SYSTEM_PATH = "/usr/bin:/bin"

BUNDLE_FILES = {
    "LICENSE", "README.md", "README_CN.md", "install.sh", "mcp.json",
    "anet/bin/anet-run.sh", "anet/bin/anet-mcp.sh", "anet/bin/anet-install.sh",
    "skills/anet/SKILL.md", "skills/anet-setup/SKILL.md", "commands/anet-status.md",
}
INSTALLED = [
    "skills/anet/SKILL.md", "skills/anet-setup/SKILL.md", "commands/anet-status.md",
    "anet/bin/anet-run.sh", "anet/bin/anet-mcp.sh", "anet/bin/anet-install.sh",
]

_cache = {}


def built():
    """The generated target (in memory) and the same tree written to a directory that lives for the whole run."""
    if not _cache:
        res = g.build_all(["trae"])
        ctx = res.contexts["trae"]
        tmp = tempfile.mkdtemp(prefix="trae-bundle-")
        g.write_tree({f"trae/{p}": f for p, f in ctx.files.items()}, tmp)
        _cache.update(res=res, ctx=ctx, tmp=tmp, bundle=Path(tmp) / "trae")
    return _cache["res"], _cache["ctx"], _cache["bundle"]


def tearDownModule():
    if _cache:
        shutil.rmtree(_cache["tmp"], ignore_errors=True)


def text_of(ctx, path):
    return ctx.files[path].data.decode("utf-8")


def fake_anet(directory):
    """An anet that prints its own path and arguments; returns its path."""
    directory = Path(directory)
    directory.mkdir(parents=True, exist_ok=True)
    path = directory / "anet"
    path.write_text('#!/bin/sh\necho "$0 $*"\n')
    path.chmod(0o755)
    return str(path)


def run(argv, home, env_extra=None, path=SYSTEM_PATH, cwd=None):
    env = {"HOME": str(home), "PATH": path, "LC_ALL": "C"}
    env.update(env_extra or {})
    return subprocess.run(argv, env=env, cwd=cwd, capture_output=True, text=True, timeout=60)


class BundleTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.res, cls.ctx, cls.bundle = built()
        cls.meta = cls.ctx.meta

    def test_file_set(self):
        self.assertEqual(set(self.ctx.files), BUNDLE_FILES)
        self.assertEqual(self.ctx.root_files, {})

    def test_project_mcp_json(self):
        entry = json.loads(text_of(self.ctx, "mcp.json"))["mcpServers"]
        self.assertEqual(list(entry), [self.meta["mcp"]["server_name"]])
        e = entry["anet"]
        self.assertEqual(e["command"], "/bin/sh")
        self.assertNotRegex(e["command"], r"\s")
        self.assertEqual(e["args"], ["${workspaceFolder}/.trae/anet/bin/anet-mcp.sh"])
        # ${workspaceFolder} is the only variable that Trae expands.
        self.assertEqual(re.findall(r"\$\{[^}]*\}", json.dumps(e)), ["${workspaceFolder}"])
        self.assertEqual(e["env"], {"RUN_MCP_TIMEOUT_MS": str(self.meta["mcp"]["timeout_ms"])})
        for v in e["env"].values():
            self.assertIsInstance(v, str)
        self.assertNotIn("type", e)

    def test_launcher_path_of_the_project_entry_exists_in_the_bundle_layout(self):
        # The bundle mirrors a Trae configuration directory, so a manual copy of the directories works.
        self.assertTrue((self.bundle / "anet" / "bin" / "anet-mcp.sh").is_file())
        self.assertTrue((self.bundle / "anet" / "bin" / "anet-run.sh").is_file())

    def test_skills_have_front_matter_and_the_guide(self):
        for name in ("anet", "anet-setup"):
            fields, body = self.ctx.skill(name)
            self.assertEqual(fields["name"], name)
            self.assertTrue(fields["description"])
            self.assertIn("anet", body)
        guide = text_of(self.ctx, "skills/anet/SKILL.md")
        self.assertIn("<skill-dir>/../../anet/bin/anet-run.sh", guide)
        self.assertIn("directory that contains this `SKILL.md`", guide)

    def test_runner_path_resolves_from_the_skill_directory(self):
        for name in ("anet", "anet-setup"):
            self.assertTrue((self.bundle / "skills" / name / ".." / ".." / "anet" / "bin" / "anet-run.sh").is_file())

    def test_no_text_of_another_host_or_unrendered_template(self):
        for path, f in self.ctx.files.items():
            if path.endswith(".png"):
                continue
            text = f.data.decode("utf-8")
            self.assertNotIn("{{", text, path)
            for foreign in ("ZCode", "CLAUDE_PLUGIN_ROOT", "ZCODE_", "CURSOR_", "CODEBUDDY", "user_config", ".zcode"):
                self.assertNotIn(foreign, text, f"{path} mentions {foreign}")

    def test_command_has_no_inline_execution_and_names_the_runner(self):
        text = text_of(self.ctx, "commands/anet-status.md")
        self.assertNotIn("!`", text)
        self.assertIn("<skill-dir>/../../anet/bin/anet-run.sh", text)
        self.assertNotIn("`runner` command shown above", text)
        self.assertIn("do not read or edit the Trae configuration (`mcp.json`)", text)

    def test_setup_skill_keeps_the_safety_rules(self):
        text = text_of(self.ctx, "skills/anet-setup/SKILL.md")
        self.assertIn("bin/anet-install.sh", text)
        self.assertNotIn("$(", text)
        self.assertIn("Never read, print or copy files under `~/.anet`", text)
        self.assertIn("Do not run them on the user's behalf", text)
        self.assertIn("Never read or edit the Trae configuration (`mcp.json`)", text)
        self.assertNotIn("holds the user's model credentials", text)
        self.assertIn("anet/settings", text)
        self.assertIn("Windows", text)

    def test_readme_states_the_installation_the_settings_and_windows(self):
        en, cn = text_of(self.ctx, "README.md"), text_of(self.ctx, "README_CN.md")
        for text in (en, cn):
            self.assertIn("install.sh", text)
            self.assertIn("--project", text)
            self.assertIn("--cn", text)
            self.assertIn("ANET_PLUGIN_BIN", text)
            self.assertIn("trae://", text.replace("`trae://`", "trae://"))
            self.assertIn("Windows", text)
            self.assertNotIn("The plugin writes no files.", text)
        self.assertIn("At run time the plugin writes no files", en)
        self.assertIn("插件在运行时不写文件", cn)

    def test_only_documented_public_hosts_and_no_local_names(self):
        for path, f in self.ctx.files.items():
            if path.endswith(".png"):
                continue
            text = f.data.decode("utf-8")
            for bad in re.findall(r"\b\d{1,3}(?:\.\d{1,3}){3}\b", text):
                self.assertEqual(bad, "127.0.0.1", path)
            self.assertNotIn("/home/", text, path)
            self.assertNotIn("/tmp/", text, path)

    def test_scripts_parse(self):
        for rel in ("install.sh", "anet/bin/anet-run.sh", "anet/bin/anet-mcp.sh"):
            for shell in SHELLS:
                r = subprocess.run([shell, "-n", str(self.bundle / rel)], capture_output=True, text=True)
                self.assertEqual(r.returncode, 0, f"{shell} -n {rel}: {r.stderr}")

    def test_validate_rejects_a_name_that_cannot_be_a_directory(self):
        mod = self.ctx.module
        meta = json.loads(json.dumps(self.meta))
        meta["name"] = "Bad Name"
        ctx = g.Context(mod, meta, mod.VARS)
        with self.assertRaises(ValueError):
            mod.validate(ctx)

    def test_transform_fails_when_a_shared_source_changes(self):
        mod = self.ctx.module
        with self.assertRaises(ValueError):
            mod.transform(self.ctx, "skills/anet-setup/SKILL.md", "no anchor here", "en")
        with self.assertRaises(ValueError):
            mod.transform(self.ctx, "commands/anet-status.md", "no anchor here", "en")
        with self.assertRaises(ValueError):
            mod.transform(self.ctx, "README.md", "no anchor here", "en")


class SiteTests(unittest.TestCase):
    def test_archive_matches_its_digest_and_holds_the_bundle(self):
        res, ctx, _ = built()
        site = g.build_site(res, "https://agentnetwork.org.cn")
        info = json.loads(site["plugins/trae/trae.json"].data)
        self.assertEqual(info["archive"]["url"], f"https://agentnetwork.org.cn/plugins/trae/anet-trae-{ctx.meta['version']}.zip")
        data = site[f"plugins/trae/anet-trae-{ctx.meta['version']}.zip"].data
        self.assertEqual(g.sha256_hex(data), info["archive"]["sha256"])
        with tempfile.TemporaryDirectory() as tmp:
            zp = Path(tmp) / "a.zip"
            zp.write_bytes(data)
            with zipfile.ZipFile(zp) as z:
                names = set(z.namelist())
        self.assertEqual(names, {f"anet-trae/{p}" for p in BUNDLE_FILES})


class InstallerTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.res, cls.ctx, cls.bundle = built()
        cls.script = str(cls.bundle / "install.sh")

    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp(prefix="trae-test-"))
        self.home = self.tmp / "home"
        self.home.mkdir()
        self.anet = fake_anet(self.tmp / "opt")

    def tearDown(self):
        shutil.rmtree(self.tmp, ignore_errors=True)

    def install(self, shell, *args, home=None):
        return run([shell, self.script, *args], home or self.home)

    def printed_json(self, out):
        start = out.index("{\n")
        end = out.index("\n}\n", start) + 2
        return json.loads(out[start:end])

    def test_installer_does_not_write_through_links(self):
        for shell in SHELLS:
            with self.subTest(shell=shell):
                project = self.tmp / f"p-{shell}"
                outside = self.tmp / f"outside-{shell}"
                (project / ".trae").mkdir(parents=True)
                outside.mkdir()
                target = outside / "settings"
                target.write_text("keep\n")
                (project / ".trae" / "anet").symlink_to(outside)  # the directory anet leads out of .trae
                r = self.install(shell, "--project", str(project), "--identity", "work")
                self.assertNotEqual(r.returncode, 0)
                self.assertIn("symbolic link", r.stderr)
                self.assertEqual(target.read_text(), "keep\n")
                self.assertEqual(sorted(p.name for p in outside.iterdir()), ["settings"])
                r = self.install(shell, "--project", str(project), "--remove")
                self.assertNotEqual(r.returncode, 0)
                # A link to the directory .trae itself is refused for a project as well.
                other = self.tmp / f"q-{shell}"
                other.mkdir()
                (other / ".trae").symlink_to(outside)
                self.assertNotEqual(self.install(shell, "--project", str(other)).returncode, 0)
                self.assertEqual(sorted(p.name for p in outside.iterdir()), ["settings"])

    def test_remove_keeps_a_file_that_the_bundle_did_not_install(self):
        for shell in SHELLS:
            with self.subTest(shell=shell):
                home = self.tmp / f"r-{shell}"
                (home / ".trae" / "commands").mkdir(parents=True)
                foreign = home / ".trae" / "commands" / "anet-status.md"
                foreign.write_text("# my own command\n")
                r = self.install(shell, "--remove", home=home)
                self.assertNotEqual(r.returncode, 0)
                self.assertIn("not part of the anet bundle", r.stderr)
                self.assertEqual(foreign.read_text(), "# my own command\n")

    def test_user_scope_installs_every_file_and_prints_a_consistent_entry(self):
        for shell in SHELLS:
            with self.subTest(shell=shell):
                home = self.tmp / f"h-{shell}"
                home.mkdir()
                r = self.install(shell, "--identity", "work", "--anet", self.anet, home=home)
                self.assertEqual(r.returncode, 0, r.stderr)
                root = home / ".trae"
                for rel in INSTALLED:
                    self.assertEqual((root / rel).read_bytes(), (self.bundle / rel).read_bytes(), rel)
                self.assertEqual((root / "anet" / "settings").read_text(), f"anetPath={self.anet}\nidentity=work\n")
                entry = self.printed_json(r.stdout)["mcpServers"]["anet"]
                self.assertEqual(entry["command"], "/bin/sh")
                self.assertEqual(entry["args"], [str(root / "anet" / "bin" / "anet-mcp.sh")])
                self.assertEqual(entry["env"], {
                    "RUN_MCP_TIMEOUT_MS": "900000", "ANET_PLUGIN_BIN": self.anet, "ANET_PLUGIN_IDENTITY": "work",
                })
                # The install link is the Base64 of the single entry, URL encoded, as the Trae documentation describes.
                m = re.search(r"trae://trae\.ai-ide/mcp-import\?type=stdio&name=anet&config=(\S+)", r.stdout)
                self.assertIsNotNone(m, r.stdout)
                self.assertNotRegex(m.group(1), r"[+/=]")
                self.assertEqual(json.loads(base64.b64decode(urllib.parse.unquote(m.group(1)))), entry)
                self.assertFalse((home / ".trae-cn").exists())
                self.assertFalse((root / "mcp.json").exists())

    def test_trae_cn_uses_its_own_directory_and_prints_no_link(self):
        r = self.install(SHELLS[0], "--cn")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertTrue((self.home / ".trae-cn" / "skills" / "anet" / "SKILL.md").is_file())
        self.assertFalse((self.home / ".trae").exists())
        self.assertNotIn("trae://", r.stdout)
        self.assertEqual(self.printed_json(r.stdout)["mcpServers"]["anet"]["args"],
                         [str(self.home / ".trae-cn" / "anet" / "bin" / "anet-mcp.sh")])

    def test_project_scope_writes_the_bundle_mcp_json_when_none_exists(self):
        for shell in SHELLS:
            with self.subTest(shell=shell):
                project = self.tmp / f"my project {shell}"
                project.mkdir()
                r = self.install(shell, "--project", str(project))
                self.assertEqual(r.returncode, 0, r.stderr)
                self.assertEqual((project / ".trae" / "mcp.json").read_bytes(), (self.bundle / "mcp.json").read_bytes())
                self.assertFalse((self.home / ".trae").exists())
                self.assertFalse((project / ".trae" / "anet" / "settings").exists())
                self.assertIn("Project MCP", r.stdout)

    def test_project_scope_with_settings_adds_them_to_the_entry(self):
        project = self.tmp / "p"
        project.mkdir()
        r = self.install(SHELLS[0], "--project", str(project), "--identity", "a.b_c-1", "--anet", self.anet)
        self.assertEqual(r.returncode, 0, r.stderr)
        entry = json.loads((project / ".trae" / "mcp.json").read_text())["mcpServers"]["anet"]
        self.assertEqual(entry["args"], ["${workspaceFolder}/.trae/anet/bin/anet-mcp.sh"])
        self.assertEqual(entry["env"]["ANET_PLUGIN_IDENTITY"], "a.b_c-1")
        self.assertEqual(entry["env"]["ANET_PLUGIN_BIN"], self.anet)

    def test_an_existing_project_mcp_json_is_never_changed(self):
        project = self.tmp / "p"
        (project / ".trae").mkdir(parents=True)
        original = '{"mcpServers": {"other": {"command": "x"}}}\n'
        (project / ".trae" / "mcp.json").write_text(original)
        r = self.install(SHELLS[0], "--project", str(project))
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual((project / ".trae" / "mcp.json").read_text(), original)
        self.assertIn("was not changed", r.stdout)
        self.assertIn('"anet"', r.stdout)

    def test_no_file_of_trae_is_read_or_written_in_user_scope(self):
        trae = self.home / ".trae"
        trae.mkdir()
        sentinel = trae / "mcp.json"
        sentinel.write_text("SENTINEL\n")
        sentinel.chmod(0)  # an installer that reads it fails
        try:
            r = self.install(SHELLS[0])
            self.assertEqual(r.returncode, 0, r.stderr)
        finally:
            sentinel.chmod(0o600)
        self.assertEqual(sentinel.read_text(), "SENTINEL\n")
        self.assertEqual(sorted(p.name for p in trae.iterdir()), ["anet", "commands", "mcp.json", "skills"])

    def test_settings_are_kept_changed_and_cleared(self):
        settings = self.home / ".trae" / "anet" / "settings"
        self.assertEqual(self.install(SHELLS[0], "--identity", "work").returncode, 0)
        self.assertEqual(settings.read_text(), "anetPath=\nidentity=work\n")
        self.assertEqual(self.install(SHELLS[0], "--anet", self.anet).returncode, 0)
        self.assertEqual(settings.read_text(), f"anetPath={self.anet}\nidentity=work\n")
        r = self.install(SHELLS[0])
        self.assertEqual(settings.read_text(), f"anetPath={self.anet}\nidentity=work\n")
        self.assertEqual(self.printed_json(r.stdout)["mcpServers"]["anet"]["env"]["ANET_PLUGIN_IDENTITY"], "work")
        self.assertEqual(self.install(SHELLS[0], "--identity", "", "--anet", "").returncode, 0)
        self.assertEqual(settings.read_text(), "anetPath=\nidentity=\n")
        r = self.install(SHELLS[0])
        self.assertEqual(list(self.printed_json(r.stdout)["mcpServers"]["anet"]["env"]), ["RUN_MCP_TIMEOUT_MS"])

    def test_invalid_input_is_refused_before_anything_is_written(self):
        bad = [
            ["--identity", "a b"], ["--identity", 'a"b'], ["--identity", "../x"],
            ["--anet", "relative/anet"], ["--anet", "/opt/an et/anet"], ["--anet", '/opt/a"b/anet'], ["--anet", "/opt/a\\b/anet"],
            ["--project", str(self.tmp / "missing")], ["--cn", "--project", str(self.tmp)],
            ["--unknown"], ["--identity"], ["--remove", "--identity", "x"],
        ]
        for args in bad:
            with self.subTest(args=args):
                r = self.install(SHELLS[0], *args)
                self.assertEqual(r.returncode, 1, r.stdout)
                self.assertTrue(r.stderr.startswith("install.sh: ") or "Usage" in r.stderr, r.stderr)
                self.assertFalse((self.home / ".trae").exists())

    def test_a_home_directory_with_a_space_is_refused_with_a_reason(self):
        home = self.tmp / "my home"
        home.mkdir()
        r = self.install(SHELLS[0], home=home)
        self.assertEqual(r.returncode, 1)
        self.assertIn("space", r.stderr)
        self.assertFalse((home / ".trae").exists())

    def test_a_foreign_file_of_the_same_name_is_not_replaced(self):
        target = self.home / ".trae" / "skills" / "anet"
        target.mkdir(parents=True)
        (target / "SKILL.md").write_text("---\nname: mine\n---\nmy own\n")
        r = self.install(SHELLS[0])
        self.assertEqual(r.returncode, 1)
        self.assertIn("not part of the anet bundle", r.stderr)
        self.assertEqual((target / "SKILL.md").read_text(), "---\nname: mine\n---\nmy own\n")
        self.assertFalse((self.home / ".trae" / "anet").exists())

    def test_install_twice_is_idempotent_and_remove_deletes_only_its_files(self):
        self.assertEqual(self.install(SHELLS[0], "--identity", "w").returncode, 0)
        self.assertEqual(self.install(SHELLS[0]).returncode, 0)
        other = self.home / ".trae" / "skills" / "other" / "SKILL.md"
        other.parent.mkdir(parents=True)
        other.write_text("keep\n")
        mcp = self.home / ".trae" / "mcp.json"
        mcp.write_text("{}\n")
        r = self.install(SHELLS[0], "--remove")
        self.assertEqual(r.returncode, 0, r.stderr)
        left = sorted(p.relative_to(self.home / ".trae").as_posix() for p in (self.home / ".trae").rglob("*") if p.is_file())
        self.assertEqual(left, ["mcp.json", "skills/other/SKILL.md"])
        self.assertIn("Delete the MCP server", r.stdout)
        # Removing again is not an error.
        self.assertEqual(self.install(SHELLS[0], "--remove").returncode, 0)

    def test_remove_leaves_no_empty_trae_directory(self):
        for shell in SHELLS:
            self.assertEqual(self.install(shell).returncode, 0)
            r = self.install(shell, "--remove")
            self.assertEqual(r.returncode, 0, r.stderr)
            self.assertFalse((self.home / ".trae").exists(), shell)

    def test_the_installer_does_not_start_anything(self):
        marker = self.tmp / "ran"
        fake = self.tmp / "bin"
        fake.mkdir()
        for tool in ("anet", "curl", "wget"):
            (fake / tool).write_text(f'#!/bin/sh\necho "$0" >> {marker}\n')
            (fake / tool).chmod(0o755)
        r = run([SHELLS[0], self.script], self.home, path=f"{fake}:{SYSTEM_PATH}")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertFalse(marker.exists())


class RunnerAndLauncherTests(unittest.TestCase):
    """The installed copies, as Trae and the agent reach them."""

    @classmethod
    def setUpClass(cls):
        cls.res, cls.ctx, cls.bundle = built()

    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp(prefix="trae-test-"))
        self.home = self.tmp / "home"
        self.home.mkdir()
        self.anet = fake_anet(self.tmp / "opt")
        self.root = self.home / ".trae"
        r = run(["sh", str(self.bundle / "install.sh"), "--identity", "work", "--anet", self.anet], self.home)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.runner = str(self.root / "skills" / "anet-setup" / ".." / ".." / "anet" / "bin" / "anet-run.sh")

    def tearDown(self):
        shutil.rmtree(self.tmp, ignore_errors=True)

    def test_the_runner_of_the_skills_reads_the_settings_file(self):
        for shell in SHELLS:
            with self.subTest(shell=shell):
                r = run([shell, self.runner, "status"], self.home)
                self.assertEqual(r.returncode, 0, r.stderr)
                self.assertEqual(r.stdout.strip(), f"{self.anet} --id work status")
                r = run([shell, self.runner, "--resolve"], self.home)
                self.assertIn(f"executable: {self.anet}", r.stdout)
                self.assertIn("identity: work", r.stdout)

    def test_the_launcher_uses_the_environment_that_the_mcp_entry_passes(self):
        launcher = str(self.root / "anet" / "bin" / "anet-mcp.sh")
        other = fake_anet(self.tmp / "other")
        for shell in SHELLS:
            with self.subTest(shell=shell):
                r = run([shell, launcher], self.home, {"ANET_PLUGIN_BIN": other, "ANET_PLUGIN_IDENTITY": "ci", "RUN_MCP_TIMEOUT_MS": "1"})
                self.assertEqual(r.stdout.strip(), f"{other} --id ci mcp")
                r = run([shell, launcher], self.home, {"ANET_PLUGIN_BIN": other})
                self.assertEqual(r.stdout.strip(), f"{other} mcp")

    def test_a_configured_path_that_is_not_executable_is_an_error_in_both_ways(self):
        missing = str(self.tmp / "none" / "anet")
        r = run(["sh", str(self.root / "anet" / "bin" / "anet-mcp.sh")], self.home, {"ANET_PLUGIN_BIN": missing})
        self.assertEqual(r.returncode, 1)
        self.assertIn("anetPath", r.stderr)
        run(["sh", str(self.bundle / "install.sh"), "--anet", missing], self.home)
        r = run(["sh", self.runner, "status"], self.home)
        self.assertEqual(r.returncode, 1)
        self.assertIn("anetPath", r.stderr)

    def test_without_settings_the_install_locations_are_searched(self):
        run(["sh", str(self.bundle / "install.sh"), "--identity", "", "--anet", ""], self.home)
        local = fake_anet(self.home / ".local" / "bin")
        r = run(["sh", self.runner, "status"], self.home)
        self.assertEqual(r.stdout.strip(), f"{local} status")
        empty = self.tmp / "empty-home"
        empty.mkdir()
        r = run(["sh", self.runner, "status"], empty)
        # The installed runner of this home finds nothing for the other home: it reports the setup skill.
        self.assertEqual(r.returncode, 1)
        self.assertIn("anet-setup", r.stderr)

    def test_the_settings_file_is_data_not_code(self):
        marker = self.tmp / "executed"
        (self.root / "anet" / "settings").write_text(f"anetPath={self.anet}\nidentity=$(touch {marker})\n")
        r = run(["sh", self.runner, "status"], self.home)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertFalse(marker.exists())
        self.assertIn("--id $(touch", r.stdout)


if __name__ == "__main__":
    unittest.main()
