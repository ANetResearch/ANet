"""Target zcode: the plugin for ZCode, the agentic coding application of Z.ai.

The shell fragments and the settings section of the README are the conventional
files of targets/zcode.d/ (runner-settings-comment.txt, runner-read-settings.sh,
settings-section.md, settings-section_CN.md).

Output (dist/zcode/ is the plugin root, as ZCode installs it):

    .zcode-plugin/plugin.json   manifest with userConfig (identity, anetPath)
    .mcp.json                   the MCP server, started through bin/anet-mcp.sh
    bin/ skills/ commands/      the shared files
    README.md README_CN.md LICENSE

Site (served at https://agentnetwork.org.cn/zcode/, the layout that
publish.sh --zcode installs and that ZCode fetches over HTTPS):

    marketplace.json, assets/anet/icon.png, plugins/anet/<version>/plugin.zip

ZCode checks the SHA-256 of the archive against marketplace.json, so the
archive is reproducible; a published version is never replaced.
"""

import re

TITLE = "ZCode"
HELP = "ZCode of Z.ai (desktop application and command-line program); plugin with user settings and a slash command."
SITE_PATH = "zcode"  # the URL path of the marketplace is fixed: ZCode users have it configured
MARKERS = ["ZCODE_", "ZCode", ".zcode"]

VARS = {
    "TOOL": "ZCode",
    "RUNNER_FROM_SKILL": "${ZCODE_SKILL_DIR}/../../bin/anet-run.sh",
    "RUNNER_FROM_COMMAND": "${ZCODE_PLUGIN_ROOT}/bin/anet-run.sh",
    "SETTINGS_STORE": True,
    "CONFIG_DIR": "~/.zcode",
    "DOC_SLUG": "zcode",
    "SETTINGS_READ_NOTE": {
        "en": " Outside the MCP server, the script reads these two settings, and nothing else, from the ZCode configuration.",
        "zh-CN": "在 MCP 服务之外，该脚本从 ZCode 配置中只读取这两项设置。",
    },
}

_NAME_RE = re.compile(r"^[a-z0-9][a-z0-9._-]{0,127}$")


def validate(ctx):
    name = ctx.meta["name"]
    if not _NAME_RE.match(name):
        raise ValueError(f"plugin name {name!r} is not a valid ZCode plugin name")


def build(ctx):
    meta = ctx.meta
    ctx.add_common()

    ctx.add_json(".zcode-plugin/plugin.json", {
        "name": meta["name"],
        "version": meta["version"],
        "description": meta["description"],
        "description_i18n": meta["description_i18n"],
        "author": meta["author"],
        "homepage": meta["homepage"],
        "repository": meta["repository"],
        "license": meta["license"],
        "keywords": meta["keywords"],
        "userConfig": {
            s["key"]: {k: s[k] for k in ("title", "description", "type", "default")} for s in meta["settings"]
        },
    })
    ctx.add_json(".mcp.json", {
        "mcpServers": {
            meta["mcp"]["server_name"]: {
                "type": "stdio",
                "command": "sh",
                "args": ["${ZCODE_PLUGIN_ROOT}/bin/anet-mcp.sh"],
                "env": {s["env"]: "${user_config.%s}" % s["key"] for s in meta["settings"]},
                "enabled": True,
                "timeoutMs": meta["mcp"]["timeout_ms"],
            }
        }
    })


def site(ctx, site):
    if not site.base.startswith("https://"):
        raise ValueError("the site base must be an https:// URL: ZCode fetches the marketplace and the archive only over HTTPS")
    meta = ctx.meta
    name, version = meta["name"], meta["version"]
    archive = site.archive(f"plugins/{name}/{version}/plugin.zip", wrapper=name, kind="zip")
    site.add(f"assets/{name}/icon.png", ctx.icon)
    entry = {
        "name": name,
        "displayName": meta["display_name"]["en"],
        "displayName_i18n": meta["display_name"],
        "version": version,
        "description": meta["description"],
        "description_i18n": meta["description_i18n"],
        "author": meta["author"],
        "homepage": meta["homepage"],
        "license": meta["license"],
        "icon": site.url(f"assets/{name}/icon.png"),
        "category": meta["category"],
        "keywords": meta["keywords"],
        "source": {"source": "url", "type": "zip", "url": archive["url"], "sha256": archive["sha256"], "path": name},
    }
    site.add_json("marketplace.json", {
        "name": "agentnetwork",
        "description": "The AgentNetwork (anet) plugin for ZCode.",
        "description_i18n": {
            "en": "The AgentNetwork (anet) plugin for ZCode.",
            "zh-CN": "适用于 ZCode 的 AgentNetwork（anet）插件。",
        },
        "owner": {"name": meta["author"]["name"], "url": meta["author"]["url"]},
        "plugins": [entry],
    })
