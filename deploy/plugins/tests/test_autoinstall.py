#!/usr/bin/env python3
"""Tests of the automatic installation of anet by the MCP launcher: python3 deploy/plugins/tests/test_autoinstall.py

When the MCP launcher of a plugin (bin/anet-mcp.sh, and bin/anet-mcp.cmd through
bin/anet-run.ps1 on Windows) finds no anet executable, it runs the official
installer before it starts `anet mcp` (see common/bin/anet-install.sh and
common/bin/anet-run.ps1). These tests run the launcher of every target against
a fake installer and never reach the network:

  * POSIX: the launcher downloads the installer with `curl` from PATH. The tests
    put a fake `curl` first on PATH, which writes a fake install.sh instead of
    downloading anything. PATH is the environment of the host, which a
    workspace does not set, so the launcher needs no test hook of its own.
  * Windows: the launcher runs powershell.exe of %SystemRoot%\\System32. The
    tests run anet-run.ps1 with PowerShell 7 (ANET_PWSH, as
    test_windows_runner.py does) and a SystemRoot of their own that holds a
    fake powershell.exe.

Every test checks that the fake was used, and the fake installer refuses to run
when it was not called by the launcher under test (FAKE_STATE is unset).
"""

import json
import os
import shutil
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from pathlib import Path

sys.dont_write_bytecode = True
PLUGINS = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(PLUGINS))

import generator as g  # noqa: E402

PWSH = None if os.environ.get("ANET_PWSH") == "off" else os.environ.get("ANET_PWSH") or shutil.which("pwsh")
PS_SCRIPT = '& ([scriptblock]::Create([IO.File]::ReadAllText($env:ANET_RUN_SCRIPT)))'
INSTALLER_URL = "https://agentnetwork.org.cn/install.sh"
PS_ONE_LINER = ("[Net.ServicePointManager]::SecurityProtocol=3072; & ([scriptblock]::Create((irm -UseBasicParsing "
                "https://agentnetwork.org.cn/install.ps1)))")

FAKE_ANET = (
    "#!/bin/sh\n"
    'if [ "${1:-}" = --version ]; then echo "anet 9.9.9 (fake)"; exit 0; fi\n'
    "printf 'ANET %s\\n' \"$*\"\n"
    'if [ "${1:-}" = mcp ]; then cat; fi\n'
)

# The fake of the official installer: it records how it was run, prints a line on
# each output, and installs the fake anet into ~/.local/bin.
FAKE_INSTALL_SH = (
    "#!/bin/sh\n"
    '[ -n "${FAKE_STATE:-}" ] || { echo "the fake installer was run outside a test" >&2; exit 99; }\n'
    'echo run >>"$FAKE_STATE/runs"\n'
    'pwd >"$FAKE_STATE/cwd"\n'
    'echo "$#" >"$FAKE_STATE/argc"\n'
    'cat >"$FAKE_STATE/stdin"\n'
    'if ( : </dev/tty ) 2>/dev/null; then echo tty; else echo notty; fi >"$FAKE_STATE/tty"\n'
    'ps -o sid= -p $$ 2>/dev/null | tr -d " " >"$FAKE_STATE/sid"\n'
    'echo "fake installer output"\n'
    'echo "fake installer error" >&2\n'
    'sleep "${FAKE_SLEEP:-0}"\n'
    '[ "${FAKE_STATUS:-0}" = 0 ] || exit "$FAKE_STATUS"\n'
    'mkdir -p "$HOME/.local/bin"\n'
    'cp "$FAKE_STATE/anet" "$HOME/.local/bin/anet.tmp" && chmod 755 "$HOME/.local/bin/anet.tmp" && '
    'mv "$HOME/.local/bin/anet.tmp" "$HOME/.local/bin/anet"\n'
)

FAKE_CURL = (
    "#!/bin/sh\n"
    'printf "%s\\n" "$*" >>"$FAKE_STATE/curl"\n'
    "out=\n"
    'while [ $# -gt 0 ]; do case $1 in -o) out=$2; shift ;; esac; shift; done\n'
    '[ -n "$out" ] && cp "$FAKE_STATE/install.sh" "$out"\n'
)

# The fake of powershell.exe for the Windows runner: the same records, and the
# fake anet in %LOCALAPPDATA%\\Programs\\anet (named anet off Windows).
FAKE_POWERSHELL = (
    "#!/bin/sh\n"
    '[ -n "${FAKE_STATE:-}" ] || exit 99\n'
    'echo run >>"$FAKE_STATE/runs"\n'
    'pwd >"$FAKE_STATE/cwd"\n'
    'for a in "$@"; do printf "%s\\n" "$a"; done >"$FAKE_STATE/args"\n'
    'cat >"$FAKE_STATE/stdin"\n'
    'echo "fake installer output"\n'
    'echo "fake installer error" >&2\n'
    'sleep "${FAKE_SLEEP:-0}"\n'
    '[ "${FAKE_STATUS:-0}" = 0 ] || exit "$FAKE_STATUS"\n'
    'mkdir -p "$LOCALAPPDATA/Programs/anet"\n'
    'cp "$FAKE_STATE/anet" "$LOCALAPPDATA/Programs/anet/anet.tmp" && chmod 755 "$LOCALAPPDATA/Programs/anet/anet.tmp" && '
    'mv "$LOCALAPPDATA/Programs/anet/anet.tmp" "$LOCALAPPDATA/Programs/anet/anet"\n'
)


def system_anet():
    """An anet in a system directory that the runner searches: the launcher would find it and install nothing."""
    return any(Path(d, "anet").exists() for d in ("/usr/bin", "/bin", "/usr/local/bin", "/opt/homebrew/bin"))


def write_exec(path, text):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text)
    path.chmod(0o755)
    return path


class Fixture:
    """A home, a fake curl first on PATH, and the state directory of the fakes."""

    def __init__(self, root):
        self.root = Path(root)
        self.home = self.root / "home"
        self.home.mkdir()
        self.state = self.root / "state"
        self.state.mkdir()
        self.fakebin = self.root / "fakebin"
        write_exec(self.fakebin / "curl", FAKE_CURL)
        write_exec(self.state / "install.sh", FAKE_INSTALL_SH)
        write_exec(self.state / "anet", FAKE_ANET)
        self.work = self.root / "workspace"
        self.work.mkdir()

    def env(self, **extra):
        e = {"HOME": str(self.home), "PATH": f"{self.fakebin}:/usr/bin:/bin", "FAKE_STATE": str(self.state),
             "ANET_PLUGIN_BIN": "", "ANET_PLUGIN_IDENTITY": "", "LC_ALL": "C"}
        e.update(extra)
        return {k: v for k, v in e.items() if v is not None}

    def read(self, name):
        p = self.state / name
        return p.read_text() if p.exists() else None

    def runs(self):
        return len((self.read("runs") or "").splitlines())


@unittest.skipIf(system_anet(), "an anet executable is installed in a system directory that the runner searches")
class PosixLauncherTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.TemporaryDirectory()
        cls.out = Path(cls.tmp.name) / "out"
        res = g.build_all(None)
        g.write_tree(res.tree(), cls.out, owned_dirs=list(res.contexts))
        cls.launchers = {}
        for key, ctx in sorted(res.contexts.items()):
            rels = [rel for rel in ctx.files if rel.endswith("bin/anet-mcp.sh")]
            assert len(rels) == 1, (key, rels)
            cls.launchers[key] = cls.out / key / rels[0]

    @classmethod
    def tearDownClass(cls):
        cls.tmp.cleanup()

    def fixture(self):
        d = tempfile.TemporaryDirectory()
        self.addCleanup(d.cleanup)
        return Fixture(d.name)

    def launch(self, fx, launcher, stdin="PAYLOAD\n", timeout=120, **env):
        return subprocess.run(["/bin/sh", str(launcher)], input=stdin, capture_output=True, text=True, env=fx.env(**env),
                              cwd=fx.work, timeout=timeout)

    def test_every_launcher_installs_anet_when_it_is_missing(self):
        self.assertEqual(len(self.launchers), len(g.discover()))
        for key, launcher in self.launchers.items():
            with self.subTest(target=key):
                fx = self.fixture()
                r = self.launch(fx, launcher)
                # Standard output is the MCP session: the installed anet's, and nothing of the installer.
                self.assertEqual(r.stdout, "ANET mcp\nPAYLOAD\n", r.stderr)
                self.assertEqual(r.returncode, 0, r.stderr)
                self.assertIn("Installing it with the official installer", r.stderr)
                self.assertIn("ANET_PLUGIN_NO_INSTALL=1", r.stderr)
                self.assertIn("fake installer output", r.stderr)
                self.assertIn("fake installer error", r.stderr)
                self.assertEqual(fx.runs(), 1)
                self.assertIn(INSTALLER_URL, fx.read("curl"))
                self.assertEqual(fx.read("argc").strip(), "0", "the installer gets no argument")
                self.assertEqual(fx.read("stdin"), "", "the installer does not read the MCP session")
                self.assertEqual(fx.read("cwd").strip(), str(fx.home), "the installer runs in the home directory")
                self.assertTrue((fx.home / ".cache" / "anet-plugin" / "install.log").is_file())
                self.assertFalse((fx.home / ".cache" / "anet-plugin" / "install.lock").exists(), "the lock is released")
                # The next start finds the installed anet and installs nothing.
                r = self.launch(fx, launcher)
                self.assertEqual((r.returncode, r.stdout, fx.runs()), (0, "ANET mcp\nPAYLOAD\n", 1), r.stderr)
                self.assertNotIn("Installing", r.stderr)

    def test_the_installer_has_no_terminal_and_a_session_of_its_own(self):
        if not sys.platform.startswith("linux") or not (shutil.which("setsid") or shutil.which("perl")):
            self.skipTest("needs Linux and setsid or perl")
        import fcntl
        import pty
        import termios
        fx = self.fixture()
        master, slave = pty.openpty()
        self.addCleanup(os.close, master)

        def controlling_terminal():
            os.setsid()
            fcntl.ioctl(slave, termios.TIOCSCTTY, 0)

        p = subprocess.Popen(["/bin/sh", str(self.launchers["zcode"])], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                             stderr=subprocess.PIPE, text=True, env=fx.env(), cwd=fx.work, preexec_fn=controlling_terminal,
                             pass_fds=(slave,))
        os.close(slave)
        out, err = p.communicate("PAYLOAD\n", timeout=120)
        self.assertEqual((p.returncode, out), (0, "ANET mcp\nPAYLOAD\n"), err)
        self.assertEqual(fx.read("tty").strip(), "notty", "the installer could open the terminal of the host")
        self.assertNotEqual(fx.read("sid").strip(), str(p.pid), "the installer ran in the session of the launcher")

    def test_opt_out(self):
        for value in ("1", "yes"):
            with self.subTest(value=value):
                fx = self.fixture()
                r = self.launch(fx, self.launchers["zcode"], ANET_PLUGIN_NO_INSTALL=value)
                self.assertEqual((r.returncode, r.stdout), (1, ""))
                self.assertIn("anet is not installed", r.stderr)
                self.assertIsNone(fx.read("curl"))
                self.assertEqual(fx.runs(), 0)
        # 0 is not an opt-out.
        fx = self.fixture()
        r = self.launch(fx, self.launchers["zcode"], ANET_PLUGIN_NO_INSTALL="0")
        self.assertEqual((r.returncode, fx.runs()), (0, 1), r.stderr)

    def test_the_claude_code_launcher_stays_connected_when_opted_out(self):
        fx = self.fixture()
        init = json.dumps({"jsonrpc": "2.0", "id": 0, "method": "initialize",
                           "params": {"protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "t", "version": "1"}}})
        r = self.launch(fx, self.launchers["claude-code"], stdin=init + "\n", ANET_PLUGIN_NO_INSTALL="1")
        self.assertEqual(fx.runs(), 0)
        reply = json.loads(r.stdout.splitlines()[0])
        self.assertIn("anet is not installed", reply["result"]["instructions"])

    def test_a_configured_anet_path_is_never_replaced(self):
        fx = self.fixture()
        r = self.launch(fx, self.launchers["zcode"], ANET_PLUGIN_BIN=str(fx.root / "nowhere" / "anet"))
        self.assertEqual((r.returncode, r.stdout), (1, ""))
        self.assertIn("anetPath", r.stderr)
        self.assertEqual(fx.runs(), 0)
        self.assertIsNone(fx.read("curl"))

    def test_only_the_mcp_launcher_installs(self):
        fx = self.fixture()
        runner = self.launchers["zcode"].parent / "anet-run.sh"
        for args, env in ((["mcp"], {"ANET_PLUGIN_SETTINGS": "env"}), (["status"], {}), (["--resolve"], {})):
            r = subprocess.run(["/bin/sh", str(runner), *args], capture_output=True, text=True, env=fx.env(**env), cwd=fx.work,
                               timeout=60, stdin=subprocess.DEVNULL)
            self.assertNotIn("Installing", r.stderr, args)
        self.assertEqual(fx.runs(), 0)

    def test_two_hosts_at_once_install_once(self):
        fx = self.fixture()
        results = []

        def one():
            results.append(self.launch(fx, self.launchers["zcode"], FAKE_SLEEP="3"))

        threads = [threading.Thread(target=one) for _ in range(2)]
        for t in threads:
            t.start()
            time.sleep(0.3)
        for t in threads:
            t.join(timeout=180)
        self.assertEqual(len(results), 2)
        for r in results:
            self.assertEqual((r.returncode, r.stdout), (0, "ANET mcp\nPAYLOAD\n"), r.stderr)
        self.assertEqual(fx.runs(), 1, "two installers ran")
        self.assertTrue(any("Another installation of anet" in r.stderr for r in results), [r.stderr for r in results])

    def test_a_stale_lock_is_removed(self):
        fx = self.fixture()
        dead = subprocess.Popen(["true"])
        dead.wait()
        lock = fx.home / ".cache" / "anet-plugin" / "install.lock"
        lock.mkdir(parents=True)
        (lock / "pid").write_text(f"{dead.pid}\n")
        r = self.launch(fx, self.launchers["zcode"])
        self.assertEqual((r.returncode, r.stdout, fx.runs()), (0, "ANET mcp\nPAYLOAD\n", 1), r.stderr)
        self.assertFalse(lock.exists())

    def test_a_failed_installation_is_reported_and_nothing_else_runs(self):
        fx = self.fixture()
        r = self.launch(fx, self.launchers["zcode"], FAKE_STATUS="3")
        self.assertEqual((r.returncode, r.stdout), (1, ""))
        self.assertIn("ended with status 3", r.stderr)
        self.assertIn("fake installer error", r.stderr)
        self.assertIn("anet is not installed", r.stderr)

    def test_the_workspace_is_never_used(self):
        # A workspace with its own curl, install.sh and anet, and a PATH with relative directories.
        # (The shell itself is the host's: the host starts the launcher with sh.)
        fx = self.fixture()
        marker = fx.root / "spoofed"
        for name in ("curl", "anet"):
            write_exec(fx.work / name, f"#!/bin/sh\ntouch {marker}\nexit 0\n")
        write_exec(fx.work / "install.sh", f"#!/bin/sh\ntouch {marker}\n")
        r = self.launch(fx, self.launchers["zcode"], PATH=f".:workspace:{fx.fakebin}:/usr/bin:/bin")
        self.assertEqual((r.returncode, r.stdout), (0, "ANET mcp\nPAYLOAD\n"), r.stderr)
        self.assertFalse(marker.exists(), "a file of the workspace was run")
        self.assertEqual(fx.runs(), 1)

    def test_the_setup_helper_waits_for_a_running_installation(self):
        fx = self.fixture()
        helper = self.launchers["zcode"].parent / "anet-install.sh"
        p = subprocess.Popen(["/bin/sh", str(self.launchers["zcode"])], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                             stderr=subprocess.PIPE, text=True, env=fx.env(FAKE_SLEEP="3"), cwd=fx.work)
        lock = fx.home / ".cache" / "anet-plugin" / "install.lock"
        deadline = time.time() + 30
        while not lock.exists() and time.time() < deadline:
            time.sleep(0.1)
        self.assertTrue(lock.exists(), "the launcher took no lock")
        r = subprocess.run(["/bin/sh", str(helper)], capture_output=True, text=True, env=fx.env(), cwd=fx.work, timeout=120,
                           stdin=subprocess.DEVNULL)
        out, err = p.communicate("PAYLOAD\n", timeout=120)
        self.assertEqual((p.returncode, out), (0, "ANET mcp\nPAYLOAD\n"), err)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn("Another installation of anet has finished", r.stderr)
        self.assertEqual(fx.runs(), 1)


@unittest.skipUnless(PWSH, "pwsh is needed (set ANET_PWSH)")
class WindowsLauncherTests(unittest.TestCase):
    """anet-run.ps1 in the mode locate, as anet-mcp.cmd starts it (ANET_RUN_INSTALL=missing)."""

    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.TemporaryDirectory()
        cls.out = Path(cls.tmp.name) / "out"
        res = g.build_all(None)
        g.write_tree(res.tree(), cls.out, owned_dirs=list(res.contexts))
        cls.scripts = {key: cls.out / key / next(rel for rel in ctx.files if rel.endswith("anet-run.ps1"))
                       for key, ctx in sorted(res.contexts.items())}

    @classmethod
    def tearDownClass(cls):
        cls.tmp.cleanup()

    def setUp(self):
        d = tempfile.TemporaryDirectory()
        self.addCleanup(d.cleanup)
        self.root = Path(d.name)
        self.home = self.root / "home"
        self.home.mkdir()
        self.local = self.root / "local"
        self.local.mkdir()
        self.state = self.root / "state"
        self.state.mkdir()
        write_exec(self.state / "anet", FAKE_ANET)
        self.sysroot = self.root / "Windows"
        write_exec(self.sysroot / "System32" / "WindowsPowerShell" / "v1.0" / "powershell.exe", FAKE_POWERSHELL)

    def run_ps(self, script, **env):
        e = {"HOME": str(self.home), "USERPROFILE": str(self.home), "LOCALAPPDATA": str(self.local),
             "SystemRoot": str(self.sysroot), "PATH": "/usr/bin:/bin", "FAKE_STATE": str(self.state),
             "ANET_RUN_SCRIPT": str(script), "ANET_RUN_SELF": "/plugins/anet/bin/anet-run.cmd", "ANET_RUN_MODE": "locate",
             "ANET_PLUGIN_SETTINGS": "env", "ANET_RUN_INSTALL": "missing", "ANET_PLUGIN_BIN": "", "ANET_PLUGIN_IDENTITY": "",
             "DOTNET_CLI_TELEMETRY_OPTOUT": "1", "POWERSHELL_TELEMETRY_OPTOUT": "1"}
        e.update(env)
        e = {k: v for k, v in e.items() if v is not None}
        r = subprocess.run([PWSH, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", PS_SCRIPT], capture_output=True,
                           text=True, env=e, stdin=subprocess.DEVNULL, cwd=self.root, timeout=180)
        return r.returncode, r.stdout, r.stderr

    def runs(self):
        p = self.state / "runs"
        return len(p.read_text().splitlines()) if p.exists() else 0

    def installed(self):
        return str(self.local / "Programs" / "anet" / "anet")

    def test_every_runner_installs_anet_for_the_mcp_launcher(self):
        for key, script in self.scripts.items():
            with self.subTest(target=key):
                for p in (self.state / "runs", self.local / "Programs" / "anet" / "anet"):
                    if p.exists():
                        p.unlink()
                code, out, err = self.run_ps(script)
                self.assertEqual((code, out.splitlines()), (0, ["E|" + self.installed()]), err)
                self.assertIn("Installing it with the official installer", err)
                self.assertIn("fake installer output", err)
                self.assertIn("fake installer error", err)
                self.assertEqual(self.runs(), 1)
                args = (self.state / "args").read_text().splitlines()
                self.assertEqual(args, ["-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command",
                                        PS_ONE_LINER])
                self.assertEqual((self.state / "stdin").read_text(), "")
                self.assertEqual((self.state / "cwd").read_text().strip(), str(self.home))
                # Found the next time: nothing is installed again.
                code, out, err = self.run_ps(script)
                self.assertEqual((code, out.splitlines(), self.runs()), (0, ["E|" + self.installed()], 1), err)

    def test_nothing_is_installed_outside_the_mcp_launcher_or_when_opted_out(self):
        script = self.scripts["zcode"]
        for env in ({"ANET_RUN_INSTALL": None}, {"ANET_PLUGIN_SETTINGS": None}, {"ANET_PLUGIN_NO_INSTALL": "1"},
                    {"ANET_PLUGIN_BIN": str(self.root / "nowhere" / "anet")}, {"ANET_RUN_MODE": "resolve"}):
            with self.subTest(env=env):
                code, out, err = self.run_ps(script, **env)
                self.assertNotIn("E|", out)
                self.assertNotIn("Installing", err)
        self.assertEqual(self.runs(), 0)

    def test_a_failed_installation_is_reported(self):
        code, out, err = self.run_ps(self.scripts["zcode"], FAKE_STATUS="4")
        self.assertEqual((code, out), (1, ""))
        self.assertIn("ended with status 4", err)
        self.assertIn("anet is not installed", err)

    def test_two_hosts_at_once_install_once(self):
        script = self.scripts["zcode"]
        results = []

        def one():
            results.append(self.run_ps(script, FAKE_SLEEP="4"))

        threads = [threading.Thread(target=one) for _ in range(2)]
        for t in threads:
            t.start()
            time.sleep(0.5)
        for t in threads:
            t.join(timeout=240)
        self.assertEqual(len(results), 2)
        for code, out, err in results:
            self.assertEqual((code, out.splitlines()), (0, ["E|" + self.installed()]), err)
        self.assertEqual(self.runs(), 1, "two installers ran")


if __name__ == "__main__":
    unittest.main()
