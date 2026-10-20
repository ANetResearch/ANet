#!/usr/bin/env python3
"""Build the npm packages of an anet release from a signed release directory.

Usage: build.py --dist DIR --out DIR [--allowed-signers FILE]

DIR holds what build-release.sh writes and the download hosts serve under
/dl/ and at the root: release.json, release.json.sig, install.sh,
install.sh.sig, install.ps1 and install.ps1.sig (exactly when release.json
names a Windows build) and the .gz of every build that release.json names. The signatures are verified with ssh-keygen against
internal/release/allowed_signers, and every .gz against its sha256 and size
in release.json, before anything is written. The packages, one directory
each under --out, are:

  @agentnetwork/<asset prefix>-<os>-<cpu>   one per build of release.json
      (<os> and <cpu> in npm's names: linux, darwin or win32; x64 or arm64)
      dl/release.json, dl/release.json.sig, dl/<asset>.gz, README.md, LICENSE
      The directory dl/ is a download base for install.sh, install.ps1 and
      `anet update` when a content delivery network for npm serves it.
  @agentnetwork/anet
      bin/anet (the launcher, anet.js), allowed_signers, install.sh,
      install.sh.sig, install.ps1, install.ps1.sig, README.md, LICENSE; the
      default builds are optional dependencies of exactly this version.

Publish the build packages first and @agentnetwork/anet last, so that its
optional dependencies exist when it appears (see README.md). Nothing is
published by this script.
"""

import argparse
import hashlib
import json
import os
import shutil
import subprocess
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
REPO = HERE.parent.parent.parent
SCOPE = "@agentnetwork"
NAMESPACE = "anet-release@agentnetwork.org.cn"
IDENTITY = "anet-release@agentnetwork.org.cn"
CPU = {"amd64": "x64", "arm64": "arm64"}
# npm's name of each operating system (process.platform), by Go's name.
NPM_OS = {"linux": "linux", "darwin": "darwin", "windows": "win32"}
OS_NAME = {"linux": "Linux", "darwin": "macOS", "windows": "Windows"}
INSTALLERS = ("install.sh", "install.sh.sig", "install.ps1", "install.ps1.sig")
# The installers of a release without Windows builds.
INSTALLERS_UNIX = ("install.sh", "install.sh.sig")
DOCS = "https://docs.agentnetwork.org.cn/docs/en/latest/intro/install.html"
REPOSITORY = {"type": "git", "url": "git+https://github.com/ANetResearch/ANet.git", "directory": "deploy/release/npm"}


def die(msg):
    sys.exit(f"build.py: {msg}")


def sha256(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def verify(path, allowed):
    with open(path, "rb") as f:
        r = subprocess.run(
            ["ssh-keygen", "-Y", "verify", "-f", str(allowed), "-I", IDENTITY, "-n", NAMESPACE, "-s", f"{path}.sig"],
            stdin=f, capture_output=True, text=True,
        )
    if r.returncode != 0:
        die(f"{path.name}.sig does not verify against {allowed} in namespace {NAMESPACE}: {r.stderr.strip() or r.stdout.strip()}")


def write_json(path, data):
    path.write_text(json.dumps(data, indent=2, ensure_ascii=False) + "\n")


def package_dir(out, name):
    d = out / name.replace("/", "__")
    if d.exists():
        die(f"{d} exists; give an empty --out")
    d.mkdir(parents=True)
    return d


def common(version, description):
    return {
        "version": version,
        "description": description,
        "license": "SEE LICENSE IN LICENSE",
        "author": {"name": "Agent Network Research", "url": "https://agentnetwork.org.cn"},
        "homepage": DOCS,
        "repository": REPOSITORY,
        "publishConfig": {"access": "public"},
    }


BUILD_README = """# {name}

The {variant} build of [anet](https://github.com/ANetResearch/ANet) {version} for {os_name} on {cpu}, with the signed release manifest of that version.

This package is not run directly. The directory `dl/` holds `release.json`, `release.json.sig` and `{asset}.gz` exactly as the download hosts of the project serve them under `/dl/`:

* The npm package `@agentnetwork/anet` depends on the default builds and runs the binary after it has verified the signature of `release.json` and the SHA-256 of the archive and of the binary.
* `install.sh`, `install.ps1` and `anet update` use the copies of `dl/` that content delivery networks for npm serve as download hosts when the project's own hosts cannot be reached. Every file is checked against the signed manifest, as from any other host.

Installation: {docs}
"""

MAIN_README = """# @agentnetwork/anet

[anet](https://github.com/ANetResearch/ANet) {version}, the command-line node of AgentNetwork: it holds the identity of an AI agent, encrypts every message end to end and exchanges A2A tasks with other agents through a hub that relays messages and cannot read them.

```sh
npm install --global @agentnetwork/anet
anet version
```

anet runs on Linux, macOS and Windows (x64 and arm64).

## What the package runs

The package contains a launcher (`bin/anet`, Node.js 18 or later) and no binary. The build of the platform is in the optional dependency `@agentnetwork/anet-<os>-<cpu>` (`<os>` is `linux`, `darwin` or `win32`), as the archive and the signed release manifest of the project. Before the first run of a version the launcher checks that `release.json` is signed by the anet release key (`allowed_signers` in this package, the same key as in `SECURITY.md` of the repository), that it names this version, and that the SHA-256 of the archive and of the binary are the ones it lists. Any failure stops the launcher before anything is run. The checked binary is written to `~/.cache/anet/npm/<version>/` (`~/Library/Caches/anet/npm/<version>/` on macOS, `%LOCALAPPDATA%\\anet\\npm\\<version>\\` on Windows) and run from there.

Update with `npm update --global @agentnetwork/anet`. The installers of the project (`install.sh` and `install.ps1`, also in this package with their signatures) install the same binary into `~/.local/bin`, or on Windows into a private per-user directory they choose (`%LOCALAPPDATA%\\Programs\\anet` when it qualifies), instead:

```sh
curl --proto '=https' --tlsv1.2 -fsSL https://agentnetwork.org.cn/install.sh | sh
```

```text
powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command "[Net.ServicePointManager]::SecurityProtocol=3072; & ([scriptblock]::Create((irm -UseBasicParsing https://agentnetwork.org.cn/install.ps1)))"
```

Documentation: {docs}
"""


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--dist", required=True, type=Path)
    ap.add_argument("--out", required=True, type=Path)
    ap.add_argument("--allowed-signers", type=Path, default=REPO / "internal" / "release" / "allowed_signers")
    a = ap.parse_args()
    dist, out, allowed = a.dist.resolve(), a.out.resolve(), a.allowed_signers.resolve()

    for f in ("release.json", "release.json.sig"):
        if not (dist / f).is_file():
            die(f"{dist / f} is missing")
    verify(dist / "release.json", allowed)
    m = json.loads((dist / "release.json").read_text())
    if m.get("schema") != "anet-release/1":
        die(f"release.json: schema {m.get('schema')} is not anet-release/1")
    # install.ps1 pins the Windows builds: a release that lists one carries
    # it, and a release without Windows builds has none.
    windows = any(info.get("os") == "windows" for info in m["assets"].values())
    installers = INSTALLERS if windows else INSTALLERS_UNIX
    if not windows and any((dist / f).exists() for f in INSTALLERS if f not in INSTALLERS_UNIX):
        die(f"{dist / 'install.ps1'} is present but release.json lists no Windows build")
    for f in installers:
        if not (dist / f).is_file():
            die(f"{dist / f} is missing" + (" (release.json lists a Windows build)" if f.startswith("install.ps1") else ""))
    for f in installers:
        if not f.endswith(".sig"):
            verify(dist / f, allowed)
    version = m["version"]
    prefixes = {k: v["asset_prefix"] for k, v in m["variants"].items()}

    builds = []
    for asset, info in sorted(m["assets"].items()):
        gz = dist / f"{asset}.gz"
        if not gz.is_file():
            die(f"{gz} is missing")
        if gz.stat().st_size != info["gz_size"] or sha256(gz) != info["gz_sha256"]:
            die(f"{gz.name} does not match its sha256 and size in release.json")
        goos, goarch, variant = info["os"], info["arch"], info["variant"]
        if goos not in OS_NAME or goarch not in CPU or asset != f"{prefixes[variant]}-{goos}-{goarch}":
            die(f"release.json: unexpected build {asset}")
        builds.append((asset, goos, goarch, variant))

    out.mkdir(parents=True, exist_ok=True)
    licence = REPO / "LICENSE"
    optional = {}
    for asset, goos, goarch, variant in builds:
        name = f"{SCOPE}/{prefixes[variant]}-{NPM_OS[goos]}-{CPU[goarch]}"
        d = package_dir(out, name)
        (d / "dl").mkdir()
        for f in ("release.json", "release.json.sig", f"{asset}.gz"):
            shutil.copyfile(dist / f, d / "dl" / f)
        shutil.copyfile(licence, d / "LICENSE")
        variant_text = "default" if variant == "default" else f"{variant} (operator-approved commands)"
        (d / "README.md").write_text(BUILD_README.format(
            name=name, variant=variant_text, version=version, os_name=OS_NAME[goos], cpu=CPU[goarch],
            asset=asset, docs=DOCS))
        pkg = {"name": name, **common(version, f"The {variant} build of anet {version} for {OS_NAME[goos]} {CPU[goarch]}, with its signed release manifest.")}
        pkg.update({"os": [NPM_OS[goos]], "cpu": [CPU[goarch]], "files": ["dl", "README.md", "LICENSE"]})
        write_json(d / "package.json", pkg)
        if variant == "default":
            optional[name] = version

    name = f"{SCOPE}/anet"
    d = package_dir(out, name)
    (d / "bin").mkdir()
    shutil.copyfile(HERE / "anet.js", d / "bin" / "anet")
    os.chmod(d / "bin" / "anet", 0o755)
    shutil.copyfile(allowed, d / "allowed_signers")
    for f in installers:
        shutil.copyfile(dist / f, d / f)
    shutil.copyfile(licence, d / "LICENSE")
    (d / "README.md").write_text(MAIN_README.format(version=version, docs=DOCS))
    pkg = {"name": name, **common(version, "anet, the command-line node of AgentNetwork: A2A tasks between AI agents, end-to-end encrypted, through a hub that cannot read them.")}
    pkg.update({
        "keywords": ["anet", "agentnetwork", "a2a", "agent-to-agent", "mcp", "x402", "multi-agent", "cli"],
        "bin": {"anet": "bin/anet"},
        "files": ["bin", "allowed_signers", *installers, "README.md", "LICENSE"],
        "os": sorted(v for k, v in NPM_OS.items() if windows or k != "windows"),
        "cpu": sorted(set(CPU.values())),
        "engines": {"node": ">=18"},
        "optionalDependencies": dict(sorted(optional.items())),
    })
    write_json(d / "package.json", pkg)

    for p in sorted(out.iterdir()):
        print(f"{json.loads((p / 'package.json').read_text())['name']}@{version}  {p}")


if __name__ == "__main__":
    main()
