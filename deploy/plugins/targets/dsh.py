"""Target dsh: the plugin for DeepSeek Harness (DSH).

A DSH plugin is a bundle: an npm-style package whose package.json declares
"dsh": {"bundle": {"patch": "./cordis.patch.yml"}}. DSH installs it into a
profile (`dsh plugin --profile P add ...`) and applies the patch as a layer of
the profile composition. The host has no plugin variables, no markdown
commands and no declarative settings, so this target differs from the others:

  * The bundle module src/dsh/index.js provides the directory of the bundle to
    the MCP client row (as the service anetPlugin) and applies the two settings
    to the environment; src/dsh/skills.js registers the skills in the skill
    registry of DSH.
  * The MCP server is a row of the patch for @deepseek-ai/dsh-mcp-client that
    starts <bundle>/bin/anet-mcp, without an extension and without arguments.
  * The status command becomes the skill anet-status (user and model invocable).
    The @anet entry point is the skill anet itself, which the user can invoke
    with a request.
  * In the skill texts the token ${DSH_BUNDLE_DIR} stands for the bundle
    directory; skills.js replaces it by the absolute path when it registers a
    skill.

Output (dist/dsh/ is the package directory, as `dsh plugin add` takes it):

    package.json  cordis.patch.yml  index.js  skills.js
    bin/ skills/ (anet, anet-setup, anet-status)
    README.md README_CN.md LICENSE

Launch on Windows and POSIX (read in @deepseek-ai/dsh 0.2.1-rc.2 and its
dsh-mcp-client, which uses @modelcontextprotocol/client 2.2.0):

  * The stdio transport of the MCP SDK spawns with cross-spawn, shell:false.
    The command comes from the !!js expression of the patch; no variable is
    expanded.
  * On Windows cross-spawn tries <bundle>/bin/anet-mcp with each extension of
    PATHEXT (an extensionless file is not executable there), finds
    bin/anet-mcp.cmd and runs it through cmd.exe /d /s /c. On Linux and macOS
    bin/anet-mcp is run itself: a shell script that starts bin/anet-mcp.sh
    with sh.
  * The executable bit survives the installation: `dsh plugin add` installs
    with pnpm, whose store writes executable files with mode 0755.
  * The agent uses a PowerShell tool on Windows (the bash tools are disabled
    there); skills.js writes the bundle directory into the skills unescaped on
    Windows, so that the runner path is valid in PowerShell.

Site (only when ANET_PLUGIN_COMMIT and SOURCE_DATE_EPOCH are set): index.json,
a format-1 plugin market index for the "Custom plugin markets" feature of DSH.
The index pins a Git commit of the ANet repository, which exists only after
dist/ is committed, so the commit is an input of the command:

    ANET_PLUGIN_COMMIT=<40 hex> SOURCE_DATE_EPOCH=<unix seconds> \\
        python3 deploy/plugins/build.py --site DIR --target dsh
"""

import json
import os
import re
import sys
from pathlib import Path

TITLE = "DeepSeek Harness (DSH)"
HELP = "DeepSeek Harness (DSH): npm-style bundle with the MCP client row, three skills and settings in the profile patch."
MARKERS = ["DSH_BUNDLE_DIR", "DeepSeek Harness", "cordis.patch"]

SRC = Path(__file__).resolve().parent.parent / "src" / "dsh"
PACKAGE_NAME = "dsh-anet"
DSH_RANGE = ">=0.2.0-0 <0.3.0"
CONFIG_DIR_TEXT = "`~/.dsh`, or the directory named by `DSH_HOME`"

VARS = {
    "TOOL": "DeepSeek Harness",
    "RUNNER_FROM_SKILL": "${DSH_BUNDLE_DIR}/bin/anet-run.sh",
    "RUNNER_FROM_COMMAND": "${DSH_BUNDLE_DIR}/bin/anet-run.sh",
    "HAS_COMMAND": False,
    "SETTINGS_STORE": False,
    "DOC_SLUG": "dsh",
    "ARGUMENTS": "stated in the request, if any",
    "INSTALL_STEP": {
        "en": "Install the plugin into a DSH profile and restart the profile (see Installation below).",
        "zh-CN": "把本插件安装到某个 DSH profile 并重启该 profile（见下文“安装”）。",
    },
    "COMPONENTS_SAME_RUNNER": {
        "en": "the MCP server (`anet mcp`) and the skills `anet`, `anet-setup` and `anet-status`",
        "zh-CN": "MCP 服务（`anet mcp`）以及 `anet`、`anet-setup`、`anet-status` skill ",
    },
    "SETTINGS_SECTION": {
        "en": (SRC / "settings-section.md").read_text(encoding="utf-8").rstrip("\n"),
        "zh-CN": (SRC / "settings-section_CN.md").read_text(encoding="utf-8").rstrip("\n"),
    },
}

_STATUS_USE = " Use when the user asks for the state of the anet node, its hub or its balance, or runs anet-status."


def _replace_once(text, old, new, rel):
    if text.count(old) != 1:
        raise ValueError(f"dsh transform: expected exactly one occurrence in {rel} of: {old!r}")
    return text.replace(old, new)


def transform(ctx, rel, text, lang):
    """Wording that the variables do not cover (the host has a skill, not a command)."""
    if rel == "skills/anet-setup/SKILL.md":
        text = _replace_once(
            text,
            "When the MCP tools of anet are still not available after the setup, tell the user to start a\n"
            "new DeepSeek Harness session, because the MCP server of a plugin starts with the session.",
            "DeepSeek Harness ends every process that its shell tool started when the session ends, so a node\n"
            "that the setup guide starts in this session stops with it. Tell the user plainly that the user has\n"
            "to start the node again in a terminal of their own (the start command that the guide names, with\n"
            "`--id <name>` after `anet` when the plugin selects a named identity, and with the path of the\n"
            "executable when the plugin setting anetPath names one), and then has to start a new DeepSeek\n"
            "Harness session, because the MCP server of a plugin starts with the session and connects only to a\n"
            "node that is already running. Do not try to detach the node from the session.",
            rel,
        )
        return text.rstrip("\n") + (
            f"\n- Never read or edit the DeepSeek Harness configuration ({CONFIG_DIR_TEXT}): it holds the user's\n"
            "  profiles and model credentials, and the plugin settings in it belong to the user. The plugin passes\n"
            "  the two plugin settings to the runner in the environment.\n"
        )
    if rel == "commands/anet-status.md":
        return _replace_once(
            text,
            "Do not change any setting. Optional focus from the user: stated in the request, if any",
            "Do not change any setting, and do not read or edit the DeepSeek Harness configuration\n"
            f"({CONFIG_DIR_TEXT}); the plugin passes the settings to the runner itself.\n"
            "If the request names a focus, report on it first.",
            rel,
        )
    if rel == "README.md":
        text = _replace_once(
            text,
            "| Skill | `anet-setup` |",
            "| Skill | `anet-status` | Shows the state of the node, as the guide of the installed anet describes. |\n| Skill | `anet-setup` |",
            rel,
        )
        text = _replace_once(
            text,
            "3. Start a new session, so that the MCP server starts, and ask the agent to show the state of the anet node.",
            "3. Start the node in a terminal of your own (`anet up`; for a named identity, `anet --id <name> up`; for a setting `anetPath`, the path of that executable): DSH ends the processes that its shell tool started when the session ends, so a node that the agent started stops with the session. Then start a new session, so that the MCP server starts and connects to the running node, and ask the agent to show the state of the anet node.",
            rel,
        )
        return _replace_once(
            text,
            "Ask the agent to run the `anet-setup` skill, then start a new session. |",
            "Ask the agent to run the `anet-setup` skill, start the node in a terminal of your own (`anet up`), then start a new session. |",
            rel,
        )
    if rel == "README_CN.md":
        text = _replace_once(
            text,
            "| Skill | `anet-setup` |",
            "| Skill | `anet-status` | 按已安装 anet 的指南显示节点状态。 |\n| Skill | `anet-setup` |",
            rel,
        )
        text = _replace_once(
            text,
            "3. 新建一个会话，使 MCP 服务启动，然后让智能体显示 anet 节点的状态。",
            "3. 在您自己的终端中启动节点（`anet up`；具名身份使用 `anet --id <name> up`；设置了 `anetPath` 时使用该可执行文件的路径）：DSH 在会话结束时会终止其 shell 工具启动的进程，因此由智能体启动的节点会随会话结束而停止。然后新建一个会话，使 MCP 服务启动并连接到正在运行的节点，再让智能体显示 anet 节点的状态。",
            rel,
        )
        return _replace_once(
            text,
            "让智能体运行 `anet-setup` skill，然后新建会话。 |",
            "让智能体运行 `anet-setup` skill，在您自己的终端中启动节点（`anet up`），然后新建会话。 |",
            rel,
        )
    return text


def validate(ctx):
    if not re.match(r"^[a-z0-9][a-z0-9._-]{0,63}$", PACKAGE_NAME):
        raise ValueError(f"package name {PACKAGE_NAME!r} is not a valid unscoped npm name")
    if not re.match(r"^[A-Za-z0-9_-]{1,32}$", ctx.meta["mcp"]["server_name"]):
        raise ValueError("the MCP server name must match ^[A-Za-z0-9_-]{1,32}$ (DSH rule)")


def build(ctx):
    meta = ctx.meta
    ctx.add_common(groups=("bin", "skills", "docs"))

    # The status command of the other hosts is a skill here: DSH registers
    # commands only from code, and offers user-invocable skills to the user.
    description, body = ctx.command("anet-status")
    ctx.add(
        "skills/anet-status/SKILL.md",
        "---\nname: anet-status\ndescription: " + json.dumps(description + "." + _STATUS_USE, ensure_ascii=False) + "\n---\n\n" + body.lstrip("\n"),
    )

    ctx.add_json("package.json", {
        "name": PACKAGE_NAME,
        "version": meta["version"],
        "description": meta["description"],
        "license": meta["license"],
        "author": f"{meta['author']['name']} ({meta['author']['url']})",
        "homepage": meta["homepage"],
        "repository": {"type": "git", "url": "git+" + meta["repository"] + ".git", "directory": f"{ctx.dist_path}/{ctx.key}"},
        "keywords": ["dsh-plugin", "deepseek-harness"] + meta["keywords"],
        "type": "module",
        "main": "index.js",
        "files": ["index.js", "skills.js", "cordis.patch.yml", "bin", "skills", "README.md", "README_CN.md", "LICENSE"],
        "engines": {"dsh": DSH_RANGE},
        "dsh": {"bundle": {"patch": "./cordis.patch.yml"}},
    })
    patch = (SRC / "cordis.patch.yml").read_text(encoding="utf-8")
    patch = patch.replace("__SERVER_NAME__", meta["mcp"]["server_name"]).replace("__TIMEOUT_MS__", str(int(meta["mcp"]["timeout_ms"])))
    ctx.add("cordis.patch.yml", patch)
    for name in ("index.js", "skills.js"):
        ctx.add(name, (SRC / name).read_text(encoding="utf-8"))


def site(ctx, site):
    """A format-1 index for the custom plugin markets of DSH (needs the commit and a timestamp)."""
    commit = os.environ.get("ANET_PLUGIN_COMMIT", "")
    stamp = os.environ.get("SOURCE_DATE_EPOCH", "")
    if not commit:
        # SOURCE_DATE_EPOCH alone is common in build environments and must not fail the site build.
        print("dsh: no plugin market index written (set ANET_PLUGIN_COMMIT and SOURCE_DATE_EPOCH)", file=sys.stderr)
        return
    if not re.fullmatch(r"[0-9a-f]{40}", commit):
        raise ValueError("ANET_PLUGIN_COMMIT must be a lowercase 40-hex-digit commit hash")
    if not re.fullmatch(r"[0-9]{1,12}", stamp):
        raise ValueError("SOURCE_DATE_EPOCH must be a Unix time in seconds")
    meta = ctx.meta
    site.add(f"icons/{PACKAGE_NAME}.png", ctx.icon)
    site.add_json("index.json", {
        "format": 1,
        "title": {"en": "AgentNetwork plugins", "zh": "AgentNetwork 插件"},
        "plugins": [{
            "name": PACKAGE_NAME,
            "title": {"en": meta["display_name"]["en"], "zh": meta["display_name"]["zh-CN"]},
            "description": {"en": meta["description"], "zh": meta["description_i18n"]["zh-CN"]},
            "icon": f"icons/{PACKAGE_NAME}.png",
            "author": meta["author"]["name"],
            "homepage": meta["homepage"],
            "repository": {"url": meta["repository"], "directory": f"{ctx.dist_path}/{ctx.key}"},
            "versions": [{
                "version": meta["version"],
                "publishTimestamp": int(stamp),
                "source": {"type": "git", "url": meta["repository"] + ".git", "commit": commit, "path": f"{ctx.dist_path}/{ctx.key}"},
                "engines": {"dsh": DSH_RANGE},
            }],
        }],
    })
