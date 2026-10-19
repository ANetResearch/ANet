#!/usr/bin/env python3
"""Tests of the claude-code target: python3 deploy/plugins/tests/test_claude_code.py

The plugin is generated into a temporary directory. The runner is executed with a
fake anet executable and a temporary Claude Code configuration directory. When the
environment variable ANET_CLAUDE_CLI names a Claude Code executable, the generated
tree is also checked with `claude plugin validate --strict` (with HOME and
CLAUDE_CONFIG_DIR pointing into a temporary directory).
"""

import json
import os
import re
import subprocess
import sys
import tempfile
import unittest
import zipfile
from pathlib import Path
from types import SimpleNamespace

sys.dont_write_bytecode = True
PLUGINS = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(PLUGINS))

import generator as g  # noqa: E402

BUILD = PLUGINS / "build.py"
KEY = "claude-code"
PREFIX = "mcp__plugin_anet_anet__"
# The 14 tools of `anet mcp` (the README of the plugin lists them).
TOOLS = ["list_agents", "get_agent_card", "send_message", "wait_task", "get_task", "list_tasks", "cancel_task",
         "submit_payment", "reject_payment", "reply_task", "inbound_pending", "get_balance", "audit", "node_status"]


def run_build(*args):
    return subprocess.run([sys.executable, "-I", str(BUILD), *args], capture_output=True, text=True, check=True)


class Built(unittest.TestCase):
    """One generation for all tests of this module."""

    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.TemporaryDirectory()
        cls.out = Path(cls.tmp.name) / "out"
        cls.site_dir = Path(cls.tmp.name) / "site"
        run_build("--target", KEY, "--out", str(cls.out))
        run_build("--target", KEY, "--site", str(cls.site_dir), "--site-base", "https://example.test")
        cls.plugin = cls.out / KEY
        cls.meta = g.load_meta()

    @classmethod
    def tearDownClass(cls):
        cls.tmp.cleanup()

    def read(self, rel):
        return (self.plugin / rel).read_text(encoding="utf-8")

    def load(self, rel):
        return json.loads(self.read(rel))


class ManifestTests(Built):
    def test_layout(self):
        files = sorted(str(p.relative_to(self.plugin)) for p in self.plugin.rglob("*") if p.is_file())
        self.assertEqual(files, [
            ".claude-plugin/plugin.json", ".mcp.json", "LICENSE", "README.md", "README_CN.md", "bin/anet-install.sh", "bin/anet-mcp.sh",
            "bin/anet-run.sh", "commands/anet-status.md", "skills/anet-setup/SKILL.md", "skills/anet/SKILL.md"])

    def test_plugin_json(self):
        m = self.load(".claude-plugin/plugin.json")
        for key in ("name", "version", "author", "homepage", "repository", "license", "keywords"):
            self.assertEqual(m[key], self.meta[key], key)
        self.assertEqual(m["description"], self.meta["description"])
        # Only fields that Claude Code knows: an unknown field is a warning, and an error with --strict.
        self.assertLessEqual(set(m), {"name", "version", "description", "author", "homepage", "repository", "license",
                                      "keywords", "userConfig"})
        self.assertNotIn("description_i18n", m)

    def test_user_config_is_complete(self):
        uc = self.load(".claude-plugin/plugin.json")["userConfig"]
        self.assertEqual(set(uc), {s["key"] for s in self.meta["settings"]})
        for key, spec in uc.items():
            self.assertRegex(key, r"^[A-Za-z_]\w*$")
            # type, title and description are required by Claude Code.
            for field in ("type", "title", "description"):
                self.assertTrue(spec.get(field), f"{key}.{field}")
            self.assertIn(spec["type"], ("string", "number", "boolean", "directory", "file"))
            self.assertLessEqual(set(spec), {"type", "title", "description", "default"})

    def test_reserved_names_are_refused(self):
        mod = g.discover([KEY])[KEY]
        for name in ("claude-anet", "anthropic-anet", "cc-plugin-anet", "claude", "claude-code", "Anet", "-x"):
            ctx = SimpleNamespace(meta={**self.meta, "name": name})
            with self.assertRaises(ValueError, msg=name):
                mod.validate(ctx)
        mod.validate(SimpleNamespace(meta=self.meta))

    def test_invalid_user_config_keys_are_refused(self):
        mod = g.discover([KEY])[KEY]
        bad = [{**self.meta["settings"][0], "key": "has-hyphen"}]
        with self.assertRaises(ValueError):
            mod.validate(SimpleNamespace(meta={**self.meta, "settings": bad}))
        bad = [{**self.meta["settings"][0], "type": "text"}]
        with self.assertRaises(ValueError):
            mod.validate(SimpleNamespace(meta={**self.meta, "settings": bad}))


class McpConfigTests(Built):
    def test_server(self):
        cfg = self.load(".mcp.json")
        self.assertEqual(list(cfg), ["mcpServers"])
        servers = cfg["mcpServers"]
        self.assertEqual(list(servers), [self.meta["mcp"]["server_name"]])
        s = servers["anet"]
        # Started with sh, so that a missing executable bit does not matter.
        self.assertEqual(s["command"], "sh")
        self.assertEqual(s["args"], ["${CLAUDE_PLUGIN_ROOT}/bin/anet-mcp.sh"])
        self.assertEqual(s["type"], "stdio")
        self.assertEqual(s["timeout"], self.meta["mcp"]["timeout_ms"])
        self.assertNotIn("timeoutMs", s)
        self.assertNotIn("enabled", s)

    def test_options_reach_the_server_through_user_config(self):
        # Claude Code exports no option to an MCP server by itself: each one is passed explicitly.
        env = self.load(".mcp.json")["mcpServers"]["anet"]["env"]
        self.assertEqual(env, {s["env"]: "${user_config.%s}" % s["key"] for s in self.meta["settings"]})
        keys = set(self.load(".claude-plugin/plugin.json")["userConfig"])
        for value in env.values():
            self.assertIn(re.fullmatch(r"\$\{user_config\.(\w+)\}", value).group(1), keys)


class ComponentTests(Built):
    def test_no_foreign_variables(self):
        for p in self.plugin.rglob("*"):
            if p.is_file():
                text = p.read_text(encoding="utf-8")
                self.assertNotRegex(text, r"ZCODE|ZCode|\.zcode|\{\{", str(p))

    def test_runner_references_use_claude_variables(self):
        self.assertIn('sh "${CLAUDE_SKILL_DIR}/../../bin/anet-run.sh" --resolve', self.read("skills/anet-setup/SKILL.md"))
        self.assertIn('sh "${CLAUDE_SKILL_DIR}/../../bin/anet-run.sh" doctor', self.read("skills/anet/SKILL.md"))
        self.assertIn('!`sh "${CLAUDE_PLUGIN_ROOT}/bin/anet-run.sh" --resolve`', self.read("commands/anet-status.md"))

    def test_command(self):
        text = self.read("commands/anet-status.md")
        fields, body = g.split_frontmatter(text)
        self.assertEqual(set(fields), {"description", "allowed-tools"})
        allowed = [a.strip() for a in fields["allowed-tools"].split(", ")]
        # The injection of the command needs a rule for the quoted form of the runner.
        self.assertIn('Bash(sh "${CLAUDE_PLUGIN_ROOT}/bin/anet-run.sh" --resolve:*)', allowed)
        self.assertNotIn("Bash(sh:*)", allowed)  # not every sh command
        # Only the three read-only runner commands of the command, never the runner with any argument.
        runs = [a for a in allowed if a.startswith("Bash(")]
        self.assertEqual(len(runs), 6, runs)
        for a in runs:
            self.assertRegex(a, r"^Bash\(sh \"?\$\{CLAUDE_PLUGIN_ROOT\}/bin/anet-run\.sh\"? (--resolve|status|doctor):\*\)$")
        tools = [a for a in allowed if a.startswith("mcp__")]
        self.assertEqual(sorted(tools), [PREFIX + "get_balance", PREFIX + "node_status"])
        for t in tools:
            self.assertIn(t[len(PREFIX):], TOOLS)
        self.assertTrue(body.lstrip().startswith("Report the state"))
        # The injection starts a line: Claude Code runs it only after whitespace or at a line start.
        self.assertRegex(text, r"(?m)^!`sh ")

    def test_command_name_is_namespaced(self):
        for rel in ("README.md", "README_CN.md"):
            text = self.read(rel)
            self.assertIn("/anet:anet-status", text, rel)
            self.assertNotRegex(text, r"(?<![\w:])/anet-status", rel)

    def test_guide_carries_the_tool_names(self):
        text = self.read("skills/anet/SKILL.md")
        self.assertIn(PREFIX + "node_status", text)
        self.assertEqual(text.count("\n## When something is off\n"), 1)
        self.assertTrue(text.index(PREFIX) > text.index("## When something is off"), "the note belongs to the last section")
        self.assertTrue(text.endswith("\n") and not text.endswith("\n\n"))

    def test_skills_have_front_matter(self):
        for name in ("anet", "anet-setup"):
            fields, _ = g.split_frontmatter(self.read(f"skills/{name}/SKILL.md"))
            self.assertEqual(fields["name"], name)
            self.assertTrue(fields["description"])

    def test_setup_skill_rules(self):
        text = self.read("skills/anet-setup/SKILL.md")
        self.assertIn("Never read, print or copy files under `~/.anet`", text)
        self.assertIn("do not open the Claude Code\n  configuration file", text)
        self.assertIn("Never read or edit the Claude Code configuration (`~/.claude`)", text)
        self.assertIn("bin/anet-install.sh", text)
        self.assertNotIn("$(", text)
        self.assertIn("Do not run them on the user's behalf", text)

    def test_connect_tool_is_explained(self):
        # Claude Code keeps a failed start of an MCP server for a while (observed with claude -p), so the
        # launcher never fails at the start and offers a connect tool while the node is not ready.
        for rel in ("skills/anet-setup/SKILL.md", "README.md", "README_CN.md"):
            self.assertIn("`connect`", self.read(rel), rel)
        self.assertIn(PREFIX + "connect", self.read("skills/anet-setup/SKILL.md"))

    def test_readme_documents_the_options(self):
        for rel, needle in (("README.md", "claude plugin configure anet@agentnetwork"), ("README_CN.md", "claude plugin configure anet@agentnetwork")):
            self.assertIn(needle, self.read(rel))
        self.assertIn("Windows", self.read("README.md"))
        self.assertIn("Windows", self.read("README_CN.md"))
        self.assertIn("anet agents unwire claude", self.read("README.md"))


class RunnerTests(Built):
    """bin/anet-run.sh reads the two options from the user settings of Claude Code."""

    def setUp(self):
        self.home = tempfile.TemporaryDirectory()
        self.addCleanup(self.home.cleanup)
        h = Path(self.home.name)
        self.cfg = h / "cfg"
        self.cfg.mkdir()
        self.bin = h / "bin"
        self.bin.mkdir()
        self.anet = self.fake(self.bin / "anet")
        self.custom = self.fake(h / "custom" / "anet")

    @staticmethod
    def fake(path):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text('#!/bin/sh\nif [ "${1:-}" = --version ]; then echo "anet 9.9.9 (fake)"; exit 0; fi\necho "FAKE $*"\n')
        path.chmod(0o755)
        return str(path)

    def settings(self, obj):
        text = obj if isinstance(obj, str) else json.dumps(obj)
        (self.cfg / "settings.json").write_text(text)

    def run_runner(self, *args, env=None, config_dir=True):
        e = {"HOME": self.home.name, "PATH": f"{self.bin}:/usr/bin:/bin"}
        if config_dir:
            e["CLAUDE_CONFIG_DIR"] = str(self.cfg)
        e.update(env or {})
        r = subprocess.run(["/bin/sh", str(self.plugin / "bin" / "anet-run.sh"), *args], capture_output=True, text=True, env=e)
        return r.returncode, r.stdout.strip(), r.stderr.strip()

    def options(self, **opts):
        self.settings({"pluginConfigs": {"anet@agentnetwork": {"options": opts}}})

    def test_no_settings_file_searches_path(self):
        self.assertEqual(self.run_runner("status"), (0, "FAKE status", ""))

    def test_options_select_executable_and_identity(self):
        self.options(anetPath=self.custom, identity="work")
        self.assertEqual(self.run_runner("status"), (0, "FAKE --id work status", ""))
        code, out, _ = self.run_runner("--resolve")
        self.assertEqual(code, 0)
        self.assertIn(f"executable: {self.custom}", out)
        self.assertIn("identity: work", out)
        self.assertIn("version: anet 9.9.9 (fake)", out)

    def test_empty_options_are_the_defaults(self):
        self.options(anetPath="", identity="")
        self.assertEqual(self.run_runner("status"), (0, "FAKE status", ""))

    def test_invalid_anet_path_is_an_error_and_no_other_copy_is_used(self):
        self.options(anetPath=str(Path(self.home.name) / "missing" / "anet"))
        code, out, err = self.run_runner("status")
        self.assertEqual((code, out), (1, ""))
        self.assertIn("anetPath", err)
        code, out, _ = self.run_runner("--resolve")
        self.assertEqual(code, 0)
        self.assertIn("executable: invalid setting anetPath=", out)

    def test_default_config_directory_is_dot_claude_in_home(self):
        d = Path(self.home.name) / ".claude"
        d.mkdir()
        (d / "settings.json").write_text(json.dumps({"pluginConfigs": {"anet@agentnetwork": {"options": {"identity": "dflt"}}}}))
        self.assertEqual(self.run_runner("status", config_dir=False), (0, "FAKE --id dflt status", ""))

    def test_any_marketplace_name_but_not_two_entries(self):
        self.settings({"pluginConfigs": {"anet@mirror": {"options": {"identity": "m"}}}})
        self.assertEqual(self.run_runner("status")[1], "FAKE --id m status")
        self.settings({"pluginConfigs": {"anet@mirror": {"options": {"identity": "m"}}, "anet@agentnetwork": {"options": {"identity": "n"}}}})
        self.assertEqual(self.run_runner("status")[1], "FAKE status", "an ambiguous store is ignored")

    def test_other_plugins_and_other_settings_are_not_read(self):
        self.settings({"pluginConfigs": {"other@x": {"options": {"identity": "no"}}}, "env": {"ANET_PLUGIN_IDENTITY": "no"},
                       "anet": {"identity": "no"}})
        self.assertEqual(self.run_runner("status")[1], "FAKE status")

    def test_malformed_settings_are_ignored(self):
        for text in ("{not json", "[]", '{"pluginConfigs": []}', '{"pluginConfigs": {"anet@a": {"options": "x"}}}',
                     '{"pluginConfigs": {"anet@a": {"options": {"identity": 5, "anetPath": ["x"]}}}}'):
            self.settings(text)
            self.assertEqual(self.run_runner("status"), (0, "FAKE status", ""), text)

    def test_mcp_launcher_uses_the_environment_only(self):
        # The launcher takes the options from the MCP environment (${user_config.KEY} in .mcp.json),
        # not from the settings file.
        self.options(anetPath=self.custom, identity="from-file")
        launcher = str(self.plugin / "bin" / "anet-mcp.sh")
        env = {"HOME": self.home.name, "PATH": f"{self.bin}:/usr/bin:/bin", "CLAUDE_CONFIG_DIR": str(self.cfg),
               "ANET_PLUGIN_IDENTITY": "from-env", "ANET_PLUGIN_BIN": self.anet}
        r = subprocess.run(["/bin/sh", launcher], capture_output=True, text=True, env=env)
        self.assertEqual(r.stdout.strip(), "FAKE --id from-env mcp")
        env.update({"ANET_PLUGIN_IDENTITY": "", "ANET_PLUGIN_BIN": ""})
        r = subprocess.run(["/bin/sh", launcher], capture_output=True, text=True, env=env)
        self.assertEqual(r.stdout.strip(), "FAKE mcp")

    def test_runs_without_executable_bits(self):
        # The launcher is started with sh in .mcp.json and starts the runner with sh: the bit is not needed.
        copy = Path(self.home.name) / "copy"
        copy.mkdir()
        for name in ("anet-run.sh", "anet-mcp.sh"):
            (copy / name).write_text((self.plugin / "bin" / name).read_text())
            (copy / name).chmod(0o644)
        env = {"HOME": self.home.name, "PATH": f"{self.bin}:/usr/bin:/bin", "ANET_PLUGIN_BIN": "", "ANET_PLUGIN_IDENTITY": ""}
        r = subprocess.run(["sh", str(copy / "anet-mcp.sh")], capture_output=True, text=True, env=env)
        self.assertEqual(r.stdout.strip(), "FAKE mcp")


class LauncherTests(Built):
    """bin/anet-mcp.sh: starts `anet mcp` when it can, otherwise stays connected with a connect tool."""

    FAKE = (
        "#!/bin/sh\n"
        'if [ "${1:-}" = --version ]; then echo "anet 9.9.9 (fake)"; exit 0; fi\n'
        'if [ "${1:-}" = mcp ]; then\n'
        '  [ -f "$FAKE_STATE/up" ] || { echo "error: anet mcp: the daemon is not answering" >&2; exit 1; }\n'
        "  while IFS= read -r l; do\n"
        "    case $l in\n"
        '      *\\"initialize\\"*) echo \'{"jsonrpc":"2.0","id":0,"result":{"serverInfo":{"name":"real"}}}\' ;;\n'
        '      *tools/list*) echo \'{"jsonrpc":"2.0","id":9,"result":{"tools":[{"name":"node_status"}]}}\' ;;\n'
        "    esac\n"
        "  done\n"
        "  exit 0\n"
        "fi\n"
    )

    def setUp(self):
        self.home = tempfile.TemporaryDirectory()
        self.addCleanup(self.home.cleanup)
        h = Path(self.home.name)
        self.state = h / "state"
        self.state.mkdir()
        self.anet = h / "bin" / "anet"
        self.anet.parent.mkdir()
        self.anet.write_text(self.FAKE)
        self.anet.chmod(0o755)
        self.env = {"HOME": self.home.name, "PATH": "/usr/bin:/bin", "ANET_PLUGIN_BIN": str(self.anet),
                    "ANET_PLUGIN_IDENTITY": "", "FAKE_STATE": str(self.state)}

    def start(self, **env):
        p = subprocess.Popen(["sh", str(self.plugin / "bin" / "anet-mcp.sh")], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                             stderr=subprocess.PIPE, text=True, env=dict(self.env, **env))
        self.addCleanup(self.stop, p)
        return p

    @staticmethod
    def stop(p):
        if p.poll() is None:
            p.kill()
        p.wait()
        for f in (p.stdin, p.stdout, p.stderr):
            f.close()

    @staticmethod
    def send(p, obj):
        p.stdin.write(json.dumps(obj) + "\n")
        p.stdin.flush()

    @staticmethod
    def recv(p):
        return json.loads(p.stdout.readline())

    def handshake(self, p):
        self.send(p, {"method": "initialize", "params": {"protocolVersion": "2025-06-18", "capabilities": {},
                                                         "clientInfo": {"name": "t", "version": "1"}}, "jsonrpc": "2.0", "id": 0})
        init = self.recv(p)
        self.send(p, {"method": "notifications/initialized", "jsonrpc": "2.0"})
        return init

    def call(self, p, id_, name="connect"):
        self.send(p, {"method": "tools/call", "params": {"name": name, "arguments": {}}, "jsonrpc": "2.0", "id": id_})

    def test_ready_node_starts_the_server_directly(self):
        (self.state / "up").touch()
        p = self.start()
        self.send(p, {"method": "initialize", "jsonrpc": "2.0", "id": 0})
        self.assertEqual(self.recv(p)["result"]["serverInfo"]["name"], "real")
        p.stdin.close()
        self.assertEqual(p.wait(timeout=10), 0)

    def test_waiting_server_is_connected_and_offers_connect(self):
        p = self.start()
        init = self.handshake(p)
        self.assertEqual(init["id"], 0)
        self.assertEqual(init["result"]["protocolVersion"], "2025-06-18")
        self.assertTrue(init["result"]["capabilities"]["tools"]["listChanged"])
        self.assertIn("the daemon is not answering", init["result"]["instructions"])
        self.assertIn("anet-setup", init["result"]["instructions"])
        self.send(p, {"method": "tools/list", "jsonrpc": "2.0", "id": 1})
        self.assertEqual([t["name"] for t in self.recv(p)["result"]["tools"]], ["connect"])
        self.send(p, {"method": "ping", "jsonrpc": "2.0", "id": 2})
        self.assertEqual(self.recv(p), {"jsonrpc": "2.0", "id": 2, "result": {}})
        self.send(p, {"method": "resources/list", "jsonrpc": "2.0", "id": "r"})
        self.assertEqual(self.recv(p)["error"]["code"], -32601)
        self.call(p, 3)
        r = self.recv(p)
        self.assertTrue(r["result"]["isError"])
        self.assertIn("the daemon is not answering", r["result"]["content"][0]["text"])
        p.stdin.close()
        self.assertEqual(p.wait(timeout=10), 1)
        self.assertIn("the daemon is not answering", p.stderr.read())

    def test_connect_hands_over_to_the_real_server(self):
        p = self.start()
        self.handshake(p)
        self.call(p, 1)
        self.assertTrue(self.recv(p)["result"]["isError"])
        (self.state / "up").touch()
        self.call(p, 2)
        self.assertEqual(self.recv(p)["id"], 2)
        self.assertEqual(self.recv(p), {"jsonrpc": "2.0", "method": "notifications/tools/list_changed"})
        self.send(p, {"method": "tools/list", "jsonrpc": "2.0", "id": 3})
        r = self.recv(p)
        self.assertEqual([t["name"] for t in r["result"]["tools"]], ["node_status"], "answered by the real server")
        p.stdin.close()
        self.assertEqual(p.wait(timeout=10), 0)

    def test_next_list_request_hands_over_when_the_node_became_ready(self):
        p = self.start()
        self.handshake(p)
        (self.state / "up").touch()
        self.send(p, {"method": "tools/list", "jsonrpc": "2.0", "id": 1})
        self.assertEqual([t["name"] for t in self.recv(p)["result"]["tools"]], ["node_status"])
        p.stdin.close()
        self.assertEqual(p.wait(timeout=10), 0)

    def test_invalid_anet_path_keeps_the_server_connected(self):
        p = self.start(ANET_PLUGIN_BIN=str(Path(self.home.name) / "nowhere" / "anet"))
        init = self.handshake(p)
        self.assertIn("anetPath", init["result"]["instructions"])
        p.stdin.close()
        self.assertEqual(p.wait(timeout=10), 1)

    def test_nested_keys_of_the_arguments_are_not_taken_for_request_fields(self):
        p = self.start()
        self.handshake(p)
        # "id" and "name" occur inside "arguments" after the top-level ones, and the id is a string.
        self.send(p, {"jsonrpc": "2.0", "id": "req-1", "method": "tools/call",
                      "params": {"name": "connect", "arguments": {"id": 9, "name": "foo", "method": "ping"}}})
        r = self.recv(p)
        self.assertEqual(r["id"], "req-1")
        self.assertTrue(r["result"]["isError"], "the tool is connect, and the node is still down")
        (self.state / "up").touch()
        self.send(p, {"jsonrpc": "2.0", "id": 5, "method": "tools/call",
                      "params": {"arguments": {"id": 9, "name": "other"}, "name": "connect"}})
        self.assertEqual(self.recv(p)["id"], 5)
        self.assertEqual(self.recv(p)["method"], "notifications/tools/list_changed")
        p.stdin.close()
        p.wait(timeout=10)

    def test_non_ascii_reason_stays_valid_json(self):
        weird = Path(self.home.name) / "r\u00e9pertoire \u4e2d\u6587" / "anet"
        p = self.start(ANET_PLUGIN_BIN=str(weird), LC_ALL="C", LANG="C")
        init = self.handshake(p)
        self.assertIn("anetPath", init["result"]["instructions"])
        p.stdin.close()
        p.wait(timeout=10)

    def test_reason_with_special_characters_stays_valid_json(self):
        weird = Path(self.home.name) / 'a "b"\\c' / "anet"
        p = self.start(ANET_PLUGIN_BIN=str(weird))
        init = self.handshake(p)
        self.assertIn(str(weird), init["result"]["instructions"])
        p.stdin.close()
        p.wait(timeout=10)


class CatalogueTests(Built):
    def test_root_catalogue_of_the_mirror(self):
        cat = json.loads((self.out / ".claude-plugin" / "marketplace.json").read_text())
        self.assertEqual(cat["name"], "agentnetwork")
        self.assertTrue(cat["owner"]["name"])
        self.assertTrue(cat["metadata"]["description"])
        (entry,) = cat["plugins"]
        self.assertEqual(entry["name"], self.meta["name"])
        self.assertEqual(entry["source"], f"./{KEY}")
        self.assertTrue((self.out / entry["source"][2:] / ".claude-plugin" / "plugin.json").is_file())
        self.assertNotIn("version", entry, "the version is declared once, in plugin.json")

    def test_site_catalogue_uses_an_object_source(self):
        # A relative source is refused by Claude Code when the catalogue is downloaded from a URL.
        base = self.site_dir / "plugins" / KEY
        cat = json.loads((base / "marketplace.json").read_text())
        self.assertEqual(cat["name"], "agentnetwork")
        (entry,) = cat["plugins"]
        self.assertEqual(entry["source"], {"source": "git-subdir", "url": "https://github.com/ANetResearch/ANet",
                                           "path": "deploy/plugins/dist/claude-code"})
        self.assertEqual(entry["name"], "anet")

    def test_site_archive_is_a_complete_plugin(self):
        archive = self.site_dir / "plugins" / KEY / f"anet-{self.meta['version']}.zip"
        with zipfile.ZipFile(archive) as z:
            names = z.namelist()
            self.assertTrue(all(n.startswith("anet/") for n in names), names[:3])
            self.assertIn("anet/.claude-plugin/plugin.json", names)
            self.assertIn("anet/.mcp.json", names)
            self.assertEqual(z.read("anet/.mcp.json").decode(), self.read(".mcp.json"))

    def test_site_archive_is_reproducible(self):
        again = Path(self.tmp.name) / "site2"
        run_build("--target", KEY, "--site", str(again), "--site-base", "https://example.test")
        name = Path("plugins") / KEY / f"anet-{self.meta['version']}.zip"
        self.assertEqual((again / name).read_bytes(), (self.site_dir / name).read_bytes())

    def test_site_needs_https(self):
        with tempfile.TemporaryDirectory() as d:
            r = subprocess.run([sys.executable, "-I", str(BUILD), "--target", KEY, "--site", d, "--site-base", "http://example.test"],
                               capture_output=True, text=True)
            self.assertNotEqual(r.returncode, 0)


class CommittedTreeTests(unittest.TestCase):
    def test_dist_has_the_target(self):
        dist = PLUGINS / "dist" / KEY
        if not dist.is_dir():
            self.skipTest("dist/claude-code is not generated yet")
        r = subprocess.run([sys.executable, "-I", str(BUILD), "--check", "--target", KEY], capture_output=True, text=True)
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)


@unittest.skipUnless(os.environ.get("ANET_CLAUDE_CLI"), "ANET_CLAUDE_CLI is not set")
class ClaudeCliTests(Built):
    def cli(self, *args):
        tmp = Path(self.tmp.name) / "cli-home"
        tmp.mkdir(exist_ok=True)
        env = {"HOME": str(tmp), "CLAUDE_CONFIG_DIR": str(tmp / ".claude"), "PATH": os.environ["PATH"],
               "DISABLE_AUTOUPDATER": "1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1"}
        return subprocess.run([os.environ["ANET_CLAUDE_CLI"], *args], capture_output=True, text=True, env=env, timeout=120)

    def test_validate_plugin_strict(self):
        r = self.cli("plugin", "validate", str(self.plugin), "--strict")
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)

    def test_validate_marketplace_strict(self):
        r = self.cli("plugin", "validate", str(self.out), "--strict")
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)

    def test_validate_site_marketplace_strict(self):
        r = self.cli("plugin", "validate", str(self.site_dir / "plugins" / KEY / "marketplace.json"), "--strict")
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)


if __name__ == "__main__":
    unittest.main()
