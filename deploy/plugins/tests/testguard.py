"""A guard for the tests of the plugins: no test may reach the real user's anet or the network.

Every test module imports this module. It checks each subprocess that a test starts, before it
starts, and raises AssertionError (which fails the test) when the process could

  * run with the real user's home, anet home, data directory or runtime directory (the daemon
    pointer in /run/user/<uid>/anet): HOME, ANET_HOME, ANET_DATA_DIR, XDG_RUNTIME_DIR,
    XDG_CACHE_HOME, USERPROFILE and LOCALAPPDATA must, when they are set, name temporary
    directories;
  * find a real anet executable: every anet that the runner of the plugin would find (the
    absolute directories of PATH, ~/.local/bin, /usr/local/bin and /opt/homebrew/bin) must be a
    fake in a temporary directory;
  * run the real installer: a launcher must run with the automatic installation turned off
    (ANET_PLUGIN_NO_INSTALL), or with a fake curl (POSIX) or a fake SystemRoot (Windows) in a
    temporary directory.

For a process that runs a script of a plugin with an environment that does not mention
ANET_PLUGIN_NO_INSTALL at all, the guard sets ANET_PLUGIN_NO_INSTALL=1 and puts a curl that
always fails first on PATH before the checks. A test of the automatic installation sets the
variable itself (to "" or "0") and provides its fake installer, which the checks then require.

A process that inherits the environment of the test (env=None) may not run a script of a plugin
at all, except for a syntax check (sh -n).

The guard was added after a test started the launcher of a plugin without anet, and the real
installer installed anet and registered a node at the public hub.
"""

import atexit
import os
import shutil
import subprocess
import tempfile

TMP = os.path.realpath(tempfile.gettempdir())
REAL_HOME = os.path.realpath(os.path.expanduser("~"))
REAL_RUNTIME = f"/run/user/{os.getuid()}" if hasattr(os, "getuid") else None
_LOCATIONS = ("HOME", "ANET_HOME", "ANET_DATA_DIR", "XDG_RUNTIME_DIR", "XDG_CACHE_HOME", "USERPROFILE", "LOCALAPPDATA")


def _temporary(path):
    p = os.path.realpath(path)
    return (p == TMP or p.startswith(TMP + os.sep)) and not (p == REAL_HOME or p.startswith(REAL_HOME + os.sep))


def _runs_plugin_script(argv, env):
    if env is not None and ("ANET_RUN_SCRIPT" in env or "ANET_PLUGIN_ROOT" in env):
        return True
    for a in argv:
        s = os.fspath(a) if isinstance(a, (str, bytes, os.PathLike)) else ""
        s = s.decode() if isinstance(s, bytes) else s
        if "bin/anet-" in s or "anet-run.ps1" in s or s.endswith("/anet-mcp"):
            return True
    return False


def check(argv, env):
    """Raises AssertionError when the process could reach the real user's anet or the network."""
    if isinstance(argv, (str, bytes, os.PathLike)):
        argv = [argv]
    argv = list(argv)
    plugin = _runs_plugin_script(argv, env)
    if env is None:
        if plugin and "-n" not in argv:
            raise AssertionError(f"testguard: {argv!r} runs a plugin script with the environment of the test")
        return
    for key in _LOCATIONS:
        v = env.get(key)
        if v and os.path.isabs(v) and not _temporary(v):
            raise AssertionError(f"testguard: {key}={v} is not a temporary directory ({argv!r})")
        if v and REAL_RUNTIME and os.path.realpath(v).startswith(REAL_RUNTIME):
            raise AssertionError(f"testguard: {key}={v} is the runtime directory of the user ({argv!r})")
    if not plugin:
        return
    dirs = [d for d in env.get("PATH", "").split(os.pathsep) if os.path.isabs(d)]
    home = env.get("HOME") or ""
    dirs += [os.path.join(home, ".local", "bin") if home else "/.local/bin", "/usr/local/bin", "/opt/homebrew/bin"]
    for d in dirs:
        for name in ("anet", "anet.exe"):
            p = os.path.join(d, name)
            if os.path.exists(p) and not _temporary(p):
                raise AssertionError(f"testguard: the runner would find the real {p} ({argv!r})")
    if env.get("ANET_PLUGIN_NO_INSTALL", "") not in ("", "0"):
        return
    if "ANET_RUN_SCRIPT" in env:
        # The Windows runner: the installer is powershell.exe of the system directory.
        root = env.get("SystemRoot")
        if root and os.path.isabs(root) and not _temporary(root):
            raise AssertionError(f"testguard: SystemRoot={root} would run the real installer ({argv!r})")
        return
    curl = next((os.path.join(d, "curl") for d in dirs if os.path.exists(os.path.join(d, "curl"))), None)
    if curl is not None and not _temporary(curl):
        raise AssertionError(f"testguard: a launcher could run the real installer with {curl}; set "
                             f"ANET_PLUGIN_NO_INSTALL=1 or put a fake curl first on PATH ({argv!r})")


_FAILBIN = tempfile.mkdtemp(prefix="testguard-failbin-")
with open(os.path.join(_FAILBIN, "curl"), "w") as _f:
    _f.write("#!/bin/sh\necho 'testguard: curl is not available in the tests of the plugins' >&2\nexit 22\n")
os.chmod(os.path.join(_FAILBIN, "curl"), 0o755)
atexit.register(shutil.rmtree, _FAILBIN, True)


def with_defaults(argv, env):
    """env with the installation turned off and a failing curl first on PATH, for a process that
    runs a script of a plugin and does not mention ANET_PLUGIN_NO_INSTALL."""
    if env is None or "ANET_PLUGIN_NO_INSTALL" in env:
        return env
    if isinstance(argv, (str, bytes, os.PathLike)):
        argv = [argv]
    if not _runs_plugin_script(list(argv), env):
        return env
    env = dict(env)
    env["ANET_PLUGIN_NO_INSTALL"] = "1"
    env["PATH"] = _FAILBIN + (os.pathsep + env["PATH"] if env.get("PATH") else "")
    return env


_Popen = subprocess.Popen


class GuardedPopen(_Popen):
    def __init__(self, args, *a, **kw):
        if "env" in kw:
            kw["env"] = with_defaults(args, kw["env"])
        check(args, kw.get("env"))
        super().__init__(args, *a, **kw)


subprocess.Popen = GuardedPopen
