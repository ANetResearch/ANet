#!/usr/bin/env python3
"""Tests of build.py and the launcher anet.js, with a release signed by a test key.

Run: python3 deploy/release/npm/test_npm.py   (needs ssh-keygen and node 18+)
"""

import gzip
import hashlib
import json
import os
import platform
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
NAMESPACE = "anet-release@agentnetwork.org.cn"
VERSION = "9.9.9"
GOOS = {"Linux": "linux", "Darwin": "darwin"}.get(platform.system())
GOARCH = {"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"}.get(platform.machine())
CPU = {"amd64": "x64", "arm64": "arm64"}


def sha(b):
    return hashlib.sha256(b).hexdigest()


def run(cmd, **kw):
    return subprocess.run(cmd, capture_output=True, text=True, **kw)


@unittest.skipUnless(GOOS and GOARCH and shutil.which("ssh-keygen") and shutil.which("node"), "needs Linux or macOS, ssh-keygen and node")
class NpmPackages(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp(prefix="anet-npm-test-"))
        self.addCleanup(shutil.rmtree, self.tmp)
        self.key = self.tmp / "key"
        run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "test", "-f", str(self.key)], check=True)
        pub = (self.tmp / "key.pub").read_text().split()
        self.allowed = self.tmp / "allowed_signers"
        self.allowed.write_text(f'{NAMESPACE} namespaces="{NAMESPACE}" {pub[0]} {pub[1]}\n')
        fp = run(["ssh-keygen", "-l", "-E", "sha256", "-f", str(self.key) + ".pub"], check=True).stdout.split()[1]
        self.dist = self.tmp / "dist"
        self.dist.mkdir()
        assets = {}
        # The builds of this machine, which the launcher runs, and the four
        # Windows builds (PE files in a release; any bytes here).
        builds = [(GOOS, GOARCH)] + [("windows", a) for a in ("amd64", "arm64") if (GOOS, GOARCH) != ("windows", a)]
        for goos, goarch in builds:
            for prefix, variant in (("anet", "default"), ("anet-shell", "shell")):
                asset = f"{prefix}-{goos}-{goarch}"
                binary = f"#!/bin/sh\necho 'anet {VERSION} {variant}'\necho \"args: $*\"\n".encode()
                if goos == "windows":
                    binary = f"MZ {asset}".encode()
                gz = gzip.compress(binary, mtime=0)
                (self.dist / f"{asset}.gz").write_bytes(gz)
                assets[asset] = {"variant": variant, "os": goos, "arch": goarch, "gz_sha256": sha(gz), "gz_size": len(gz), "sha256": sha(binary), "size": len(binary)}
        manifest = {
            "schema": "anet-release/1", "version": VERSION, "commit": "0" * 40, "key_fingerprint": fp,
            "variants": {"default": {"asset_prefix": "anet", "modules": []}, "shell": {"asset_prefix": "anet-shell", "modules": ["shell"]}},
            "assets": assets,
        }
        (self.dist / "release.json").write_text(json.dumps(manifest, indent=2) + "\n")
        (self.dist / "install.sh").write_text("#!/bin/sh\necho installer\n")
        (self.dist / "install.ps1").write_text("# installer\nWrite-Host installer\n")
        for f in ("release.json", "install.sh", "install.ps1"):
            self.sign(self.dist / f)

    def sign(self, path, namespace=NAMESPACE, key=None):
        Path(f"{path}.sig").unlink(missing_ok=True)
        run(["ssh-keygen", "-q", "-Y", "sign", "-f", str(key or self.key), "-n", namespace, str(path)], check=True)

    def build(self):
        out = self.tmp / "out"
        r = run([sys.executable, str(HERE / "build.py"), "--dist", str(self.dist), "--out", str(out), "--allowed-signers", str(self.allowed)])
        return r, out

    def install(self, out):
        """Lay the packages out as npm installs them."""
        nm = self.tmp / "prefix" / "node_modules" / "@agentnetwork"
        nm.mkdir(parents=True)
        for d in out.iterdir():
            shutil.copytree(d, nm / json.loads((d / "package.json").read_text())["name"].split("/")[1])
        return nm

    def launch(self, nm, *args):
        env = {**os.environ, "XDG_CACHE_HOME": str(self.tmp / "cache"), "HOME": str(self.tmp)}
        return run(["node", str(nm / "anet" / "bin" / "anet"), *args], env=env)

    def test_build_and_run(self):
        r, out = self.build()
        self.assertEqual(r.returncode, 0, r.stderr)
        pkgs = {}
        for d in out.iterdir():
            p = json.loads((d / "package.json").read_text())
            pkgs[p["name"]] = p
        cpu = CPU[GOARCH]
        want = {"@agentnetwork/anet", f"@agentnetwork/anet-{GOOS}-{cpu}", f"@agentnetwork/anet-shell-{GOOS}-{cpu}",
                "@agentnetwork/anet-win32-x64", "@agentnetwork/anet-win32-arm64",
                "@agentnetwork/anet-shell-win32-x64", "@agentnetwork/anet-shell-win32-arm64"}
        self.assertEqual(set(pkgs), want)
        for name, p in pkgs.items():
            if name == "@agentnetwork/anet":
                continue
            npm_os = "win32" if "-win32-" in name else GOOS
            self.assertEqual(p["os"], [npm_os], name)
            self.assertEqual(p["cpu"], [name.rsplit("-", 1)[1]], name)
        # Inside a win32 package the archive keeps the asset name.
        win = out / "@agentnetwork__anet-shell-win32-arm64"
        self.assertEqual(sorted(p.name for p in (win / "dl").iterdir()), sorted(["release.json", "release.json.sig", "anet-shell-windows-arm64.gz"]))
        self.assertIn("for Windows on arm64", (win / "README.md").read_text())
        main = pkgs["@agentnetwork/anet"]
        self.assertEqual(main["optionalDependencies"], {
            f"@agentnetwork/anet-{GOOS}-{cpu}": VERSION, "@agentnetwork/anet-win32-x64": VERSION, "@agentnetwork/anet-win32-arm64": VERSION})
        self.assertEqual(main["os"], ["darwin", "linux", "win32"])
        self.assertEqual(main["bin"], {"anet": "bin/anet"})
        for f in ("install.sh", "install.sh.sig", "install.ps1", "install.ps1.sig"):
            self.assertIn(f, main["files"])
            self.assertEqual((out / "@agentnetwork__anet" / f).read_bytes(), (self.dist / f).read_bytes())
        self.assertIn("anet runs on Linux, macOS and Windows (x64 and arm64).", (out / "@agentnetwork__anet" / "README.md").read_text())
        build = out / f"@agentnetwork__anet-{GOOS}-{cpu}"
        self.assertEqual(sorted(p.name for p in (build / "dl").iterdir()), sorted(["release.json", "release.json.sig", f"anet-{GOOS}-{GOARCH}.gz"]))
        self.assertEqual((build / "dl" / "release.json").read_bytes(), (self.dist / "release.json").read_bytes())

        nm = self.install(out)
        r = self.launch(nm, "version", "--x")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn(f"anet {VERSION} default", r.stdout)
        self.assertIn("args: version --x", r.stdout)
        cache = self.tmp / "cache" / "anet" / "npm" / VERSION
        self.assertEqual((cache / "anet.release.json").read_bytes(), (self.dist / "release.json").read_bytes())
        self.assertTrue((cache / "anet.release.json.sig").is_file())
        # A second run uses the checked copy; a changed copy is checked and written again.
        self.assertEqual(self.launch(nm, "version").returncode, 0)
        (cache / "anet").write_text("#!/bin/sh\necho replaced\n")
        r = self.launch(nm, "version")
        self.assertIn(f"anet {VERSION} default", r.stdout)

    def test_launcher_refuses(self):
        r, out = self.build()
        self.assertEqual(r.returncode, 0, r.stderr)
        cpu = CPU[GOARCH]
        cases = {
            "archive": lambda nm: (nm / f"anet-{GOOS}-{cpu}" / "dl" / f"anet-{GOOS}-{GOARCH}.gz").write_bytes(gzip.compress(b"#!/bin/sh\necho evil\n")),
            "manifest": lambda nm: (nm / f"anet-{GOOS}-{cpu}" / "dl" / "release.json").write_text(
                (self.dist / "release.json").read_text().replace(VERSION, "9.9.8")),
            "namespace": lambda nm: self.sign(nm / f"anet-{GOOS}-{cpu}" / "dl" / "release.json", namespace="other@example.org"),
            "key": lambda nm: self.resign_with_other_key(nm / f"anet-{GOOS}-{cpu}" / "dl" / "release.json"),
            "missing build": lambda nm: shutil.rmtree(nm / f"anet-{GOOS}-{cpu}"),
        }
        for name, spoil in cases.items():
            with self.subTest(name):
                shutil.rmtree(self.tmp / "prefix", ignore_errors=True)
                shutil.rmtree(self.tmp / "cache", ignore_errors=True)
                nm = self.install(out)
                spoil(nm)
                r = self.launch(nm, "version")
                self.assertNotEqual(r.returncode, 0)
                self.assertNotIn("anet 9.9.9 default", r.stdout)
                self.assertIn("anet (npm):", r.stderr)
                self.assertFalse((self.tmp / "cache" / "anet" / "npm" / VERSION / "anet").exists())

    def resign_with_other_key(self, path):
        other = self.tmp / "other"
        if not other.exists():
            run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(other)], check=True)
        self.sign(path, key=other)

    def test_target(self):
        """The launcher's map from Node's platform names to the build."""
        r = run(["node", "-e", "const a = require(process.argv[1]); console.log(JSON.stringify("
                 "[['win32','x64'],['win32','arm64'],['darwin','arm64'],['linux','x64'],['win32','ia32'],['freebsd','x64'],['aix','arm64']]"
                 ".map(([p, c]) => a.target(p, c))))", str(HERE / "anet.js")])
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(json.loads(r.stdout), [
            {"goos": "windows", "goarch": "amd64", "npmOS": "win32", "cpu": "x64", "exeName": "anet.exe"},
            {"goos": "windows", "goarch": "arm64", "npmOS": "win32", "cpu": "arm64", "exeName": "anet.exe"},
            {"goos": "darwin", "goarch": "arm64", "npmOS": "darwin", "cpu": "arm64", "exeName": "anet"},
            {"goos": "linux", "goarch": "amd64", "npmOS": "linux", "cpu": "x64", "exeName": "anet"},
            None, None, None,
        ])
        r = run(["node", "-e", "const re = require(process.argv[1]).asideRe('anet.exe'); console.log(JSON.stringify("
                 "['anet.exe.old-1712345678901', 'ANET.EXE.old-1', 'anet.exe.old-', 'anet.exe.old-1.tmp', 'anetXexe.old-1', 'anet.old-1']"
                 ".map((n) => re.test(n))))", str(HERE / "anet.js")])
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(json.loads(r.stdout), [True, True, False, False, False, False])

    def test_build_refuses(self):
        with self.subTest("archive"):
            gz = self.dist / f"anet-{GOOS}-{GOARCH}.gz"
            good = gz.read_bytes()
            gz.write_bytes(good + b"x")
            r, _ = self.build()
            self.assertNotEqual(r.returncode, 0)
            self.assertIn("does not match", r.stderr)
            gz.write_bytes(good)
            shutil.rmtree(self.tmp / "out", ignore_errors=True)
        with self.subTest("signature"):
            mf = self.dist / "release.json"
            good = mf.read_text()
            mf.write_text(good + " ")
            r, _ = self.build()
            self.assertNotEqual(r.returncode, 0)
            self.assertIn("does not verify", r.stderr)
            mf.write_text(good)
            shutil.rmtree(self.tmp / "out", ignore_errors=True)
        with self.subTest("installer signature"):
            ps1 = self.dist / "install.ps1"
            ps1.write_text(ps1.read_text() + "Write-Host changed\n")
            r, _ = self.build()
            self.assertNotEqual(r.returncode, 0)
            self.assertIn("install.ps1.sig does not verify", r.stderr)


# The launcher's run(): on win32 it never calls process.execve, which Node
# 22.15 and later define on every platform and which throws on Windows, and
# runs the binary with spawnSync; elsewhere it replaces the process.
RUN_PROBE = r"""
const cp = require("node:child_process")
const calls = []
cp.spawnSync = (exe, args, opts) => { calls.push(["spawnSync", exe, args, opts.stdio]); return { status: 7, signal: null } }
Object.defineProperty(process, "platform", { value: process.argv[2] })
process.execve = (exe, argv) => {
  calls.push(["execve", exe, argv])
  if (process.platform === "win32") {
    const e = new Error("process.execve is not available on win32")
    e.code = "ERR_FEATURE_UNAVAILABLE_ON_PLATFORM"
    throw e
  }
  process.stdout.write(JSON.stringify({ calls }) + "\n")
  process.reallyExit(0)
}
process.exit = (code) => { process.stdout.write(JSON.stringify({ calls, code }) + "\n"); process.reallyExit(0) }
require(process.argv[1]).run("C:\\anet\\anet.exe", ["version", "--json"])
"""


@unittest.skipUnless(shutil.which("node"), "needs node")
class LauncherRun(unittest.TestCase):
    def probe(self, platform_name):
        r = run(["node", "-e", RUN_PROBE, str(HERE / "anet.js"), platform_name])
        self.assertEqual(r.returncode, 0, r.stderr)
        return json.loads(r.stdout)

    def test_windows_does_not_call_execve(self):
        got = self.probe("win32")
        self.assertEqual(got["calls"], [["spawnSync", "C:\\anet\\anet.exe", ["version", "--json"], "inherit"]])
        self.assertEqual(got["code"], 7)

    def test_unix_replaces_the_process(self):
        for p in ("linux", "darwin"):
            got = self.probe(p)
            self.assertEqual(got["calls"], [["execve", "C:\\anet\\anet.exe", ["C:\\anet\\anet.exe", "version", "--json"]]], p)


if __name__ == "__main__":
    unittest.main(verbosity=2)
