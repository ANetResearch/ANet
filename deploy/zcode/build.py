#!/usr/bin/env python3
"""Build the ZCode marketplace of anet from deploy/zcode/anet.

    python3 deploy/zcode/build.py --out DIR [--base URL]

DIR receives the static tree that publish.sh --zcode puts under /zcode/ on
agentnetwork.org.cn:

    DIR/marketplace.json
    DIR/assets/anet/icon.png
    DIR/plugins/anet/<version>/plugin.zip

The archive is reproducible: entries are sorted, dated 1980-01-01 and stored
with mode 0644, as the official ZCode marketplace publishes them. Its SHA-256
is written into the marketplace entry, and ZCode refuses an archive that does
not match it. The manifest is checked first; nothing is written when a check
fails.
"""

import argparse
import hashlib
import io
import json
import re
import sys
import zipfile
from pathlib import Path

HERE = Path(__file__).resolve().parent
PLUGIN = HERE / "anet"
ICON = HERE / "icon.png"
BASE = "https://agentnetwork.org.cn/zcode"
MARKETPLACE = "agentnetwork"
NAME_RE = re.compile(r"^[a-z0-9][a-z0-9._-]{0,127}$")
VERSION_RE = re.compile(r"^\d+\.\d+\.\d+$")
ZIP_DATE = (1980, 1, 1, 0, 0, 0)


def fail(msg):
    print(f"build.py: {msg}", file=sys.stderr)
    sys.exit(1)


def load_manifest():
    manifest = json.loads((PLUGIN / ".zcode-plugin" / "plugin.json").read_text(encoding="utf-8"))
    name, version = manifest.get("name", ""), manifest.get("version", "")
    if not NAME_RE.match(name):
        fail(f"plugin name {name!r} is not a valid ZCode plugin name")
    if not VERSION_RE.match(version):
        fail(f"plugin version {version!r} is not MAJOR.MINOR.PATCH")
    i18n = manifest.get("description_i18n", {})
    if set(i18n) != {"en", "zh-CN"} or i18n["en"] != manifest.get("description"):
        fail("description_i18n must hold en and zh-CN, and en must equal description")
    for required in ("README.md", "README_CN.md", "LICENSE", ".mcp.json"):
        if not (PLUGIN / required).is_file():
            fail(f"{required} is missing from {PLUGIN}")
    return manifest


def plugin_files():
    files = []
    for path in sorted(PLUGIN.rglob("*")):
        rel = path.relative_to(PLUGIN).as_posix()
        if path.is_symlink():
            fail(f"{rel} is a symbolic link")
        if path.is_file():
            if rel.endswith((".pyc", ".DS_Store")) or "/__pycache__/" in f"/{rel}":
                fail(f"{rel} is not part of the plugin")
            files.append((rel, path))
    return files


def build_zip(name):
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w", zipfile.ZIP_DEFLATED) as zf:
        for rel, path in plugin_files():
            info = zipfile.ZipInfo(f"{name}/{rel}", date_time=ZIP_DATE)
            info.external_attr = 0o644 << 16
            info.compress_type = zipfile.ZIP_DEFLATED
            zf.writestr(info, path.read_bytes())
    return buf.getvalue()


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--out", required=True, type=Path)
    parser.add_argument("--base", default=BASE, help="public URL of the /zcode/ directory (default %(default)s)")
    args = parser.parse_args()
    base = args.base.rstrip("/")
    if not base.startswith("https://"):
        fail("--base must be an https:// URL: ZCode fetches the marketplace and the archive only over HTTPS")

    manifest = load_manifest()
    name, version = manifest["name"], manifest["version"]
    archive = build_zip(name)
    digest = hashlib.sha256(archive).hexdigest()
    rel_zip = f"plugins/{name}/{version}/plugin.zip"

    entry = {
        "name": name,
        "displayName": "AgentNetwork (anet)",
        "displayName_i18n": {"en": "AgentNetwork (anet)", "zh-CN": "AgentNetwork（anet）"},
        "version": version,
        "description": manifest["description"],
        "description_i18n": manifest["description_i18n"],
        "author": manifest["author"],
        "homepage": manifest.get("homepage"),
        "license": manifest.get("license"),
        "icon": f"{base}/assets/{name}/icon.png",
        "category": "developer-tools",
        "keywords": manifest.get("keywords", []),
        "source": {"source": "url", "type": "zip", "url": f"{base}/{rel_zip}", "sha256": digest, "path": name},
    }
    marketplace = {
        "name": MARKETPLACE,
        "description": "The AgentNetwork (anet) plugin for ZCode.",
        "description_i18n": {"en": "The AgentNetwork (anet) plugin for ZCode.", "zh-CN": "适用于 ZCode 的 AgentNetwork（anet）插件。"},
        "owner": {"name": "Agent Network Research", "url": "https://agentnetwork.org.cn"},
        "plugins": [entry],
    }

    out = args.out
    (out / "plugins" / name / version).mkdir(parents=True, exist_ok=True)
    (out / "assets" / name).mkdir(parents=True, exist_ok=True)
    (out / rel_zip).write_bytes(archive)
    (out / "assets" / name / "icon.png").write_bytes(ICON.read_bytes())
    (out / "marketplace.json").write_text(json.dumps(marketplace, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(f"{name} {version}: {rel_zip} sha256 {digest} ({len(archive)} bytes)")


if __name__ == "__main__":
    main()
