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

Launch on Windows and POSIX (verified by reading the bundle of ZCode,
resources/glm/zcode.cjs):

  * ${ZCODE_PLUGIN_ROOT} is expanded in command, args, cwd and env of a stdio
    server (resolveMcpServerConfig). The server is spawned by the
    StdioClientTransport of the MCP TypeScript SDK with cross-spawn,
    shell:false, and the given cwd.
  * ZCode extracts a plugin archive with plain writeFile calls and keeps no
    file mode, so on Linux and macOS no file of the plugin is executable and
    the launcher has to be started by an interpreter: the command is "sh" and
    its argument ${ZCODE_PLUGIN_ROOT}/bin/anet-mcp.sh.
  * On Windows cross-spawn resolves "sh" with the module `which`, which looks
    in the working directory before PATH and tries each extension of PATHEXT,
    and runs the result through cmd.exe /d /s /c, which also looks in the
    working directory first. The working directory of the server is
    ${ZCODE_PLUGIN_ROOT}/bin/windows, which holds sh.cmd
    (targets/zcode.d/sh.cmd): it accepts only the argument anet-mcp.sh and
    starts bin/anet-mcp.cmd. The directory is not on any PATH, so the file
    shadows no other sh.
  * The agent runs commands in Git Bash on Windows when Git is installed, and
    in cmd otherwise; the skills give both runner forms.
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
    ctx.add("bin/windows/sh.cmd", ctx.crlf(ctx.target_file("sh.cmd") + "\n"))

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
                # sh of the system on Linux and macOS; bin/windows/sh.cmd on Windows (see the module docstring).
                "command": "sh",
                "args": ["${ZCODE_PLUGIN_ROOT}/bin/anet-mcp.sh"],
                "cwd": "${ZCODE_PLUGIN_ROOT}/bin/windows",
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
