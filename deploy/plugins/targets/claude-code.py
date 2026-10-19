"""Target claude-code: the plugin for Claude Code, the coding agent of Anthropic.

The target-specific sources are in deploy/plugins/src/claude-code/ (the runner
fragments and the settings section of the README, English and Chinese).

Output (dist/claude-code/ is the plugin root, as Claude Code installs it):

    .claude-plugin/plugin.json   manifest with userConfig (identity, anetPath)
    .mcp.json                    the MCP server, started through bin/anet-mcp.sh
    bin/ skills/ commands/       the shared files
    README.md README_CN.md LICENSE

Root file (for the mirror repository, whose root is dist/):

    .claude-plugin/marketplace.json   catalogue "agentnetwork", source ./claude-code

Site (served at https://agentnetwork.org.cn/plugins/claude-code/):

    marketplace.json        catalogue whose entry is a git-subdir source of the
                            ANet repository (a relative source cannot be used
                            in a catalogue that is downloaded from a URL)
    anet-<version>.zip      the plugin as a reproducible archive, for
                            `claude --plugin-dir` and `claude --plugin-url`

Claude Code does not export the plugin options to an MCP server as environment
variables of their own, so .mcp.json passes them as ANET_PLUGIN_IDENTITY and
ANET_PLUGIN_BIN with ${user_config.KEY}. Outside the MCP server (skill and
command) the runner reads the same two options from the user settings.
"""

import re
from pathlib import Path

TITLE = "Claude Code"
HELP = "Claude Code of Anthropic (terminal, editor extensions, desktop application); plugin with user options and a slash command."
MARKERS = ["CLAUDE_PLUGIN_ROOT", "CLAUDE_SKILL_DIR", ".claude-plugin", "mcp__plugin_anet", "claude plugin"]

MARKETPLACE = "agentnetwork"
_SRC = Path(__file__).resolve().parent.parent / "src" / "claude-code"


def _src(name):
    text = (_SRC / name).read_text(encoding="utf-8")
    return text[:-1] if text.endswith("\n") else text


# Reserved by Claude Code: a plugin name may not start with these, nor equal them.
_RESERVED_PREFIXES = ("claude-", "anthropic-", "anthropics-", "cc-plugin-")
_RESERVED_NAMES = ("claude", "anthropic", "anthropics", "claude-code", "claude-mods")
_NAME_RE = re.compile(r"^[a-z0-9][a-z0-9._-]*$")
_USER_CONFIG_KEY_RE = re.compile(r"^[A-Za-z_]\w*$")

_COMMAND_NAME = "anet-status"
_TOOL_PREFIX = "mcp__plugin_{plugin}_{server}__"

VARS = {
    "TOOL": "Claude Code",
    "RUNNER_FROM_SKILL": "${CLAUDE_SKILL_DIR}/../../bin/anet-run.sh",
    "RUNNER_FROM_COMMAND": "${CLAUDE_PLUGIN_ROOT}/bin/anet-run.sh",
    "SETTINGS_STORE": True,
    "CONFIG_DIR": "~/.claude",
    "DOC_SLUG": "claude-code",
    "INSTALL_STEP": {
        "en": (
            "Add the marketplace and install the plugin: "
            "`claude plugin marketplace add https://agentnetwork.org.cn/plugins/claude-code/marketplace.json`, "
            "then `claude plugin install anet@" + MARKETPLACE + "`. "
            "If you registered anet earlier with `anet agents wire claude`, run `anet agents unwire claude` first, "
            "so that Claude Code does not have two anet servers."
        ),
        "zh-CN": (
            "添加插件市场并安装插件："
            "`claude plugin marketplace add https://agentnetwork.org.cn/plugins/claude-code/marketplace.json`，"
            "然后执行 `claude plugin install anet@" + MARKETPLACE + "`。"
            "如果此前用 `anet agents wire claude` 注册过 anet，请先执行 `anet agents unwire claude`，"
            "以免 Claude Code 中出现两个 anet 服务。"
        ),
    },
    "SETTINGS_SECTION": {"en": _src("settings-section.md"), "zh-CN": _src("settings-section_CN.md")},
    "SETTINGS_READ_NOTE": {
        "en": " Outside the MCP server, the script reads these two options, and nothing else, from the user settings of Claude Code.",
        "zh-CN": "在 MCP 服务之外，该脚本从 Claude Code 的用户设置中只读取这两项选项。",
    },
    "RUNNER_SETTINGS_COMMENT": _src("runner-settings-comment.txt"),
    "RUNNER_READ_SETTINGS": _src("runner-read-settings.sh"),
}


def validate(ctx):
    meta = ctx.meta
    name = meta["name"]
    if not _NAME_RE.match(name):
        raise ValueError(f"plugin name {name!r} is not a valid Claude Code plugin name")
    if name in _RESERVED_NAMES or name.startswith(_RESERVED_PREFIXES):
        raise ValueError(f"plugin name {name!r} is reserved by Claude Code")
    for s in meta["settings"]:
        if not _USER_CONFIG_KEY_RE.match(s["key"]):
            raise ValueError(f"settings key {s['key']!r} is not a valid userConfig key")
        if s["type"] not in ("string", "number", "boolean", "directory", "file"):
            raise ValueError(f"settings key {s['key']!r}: type {s['type']!r} is not a userConfig type")


def tool_prefix(meta):
    """Prefix of the MCP tools of the plugin in Claude Code: mcp__plugin_<plugin>_<server>__."""
    return _TOOL_PREFIX.format(plugin=meta["name"], server=meta["mcp"]["server_name"])


_RUNNER_ARGUMENTS = ("--resolve", "status", "doctor")


def _allowed_tools(meta):
    """The tools that /anet-status uses: three read-only runner commands and two read-only MCP tools.

    The command resolves the settings (`--resolve`) and, without the MCP tools, reads
    `status` and `doctor`; every other use of the runner stays subject to the normal
    permission prompt. Both spellings of the path are listed because the agent may quote it.
    """
    prefix = tool_prefix(meta)
    runner = "${CLAUDE_PLUGIN_ROOT}/bin/anet-run.sh"
    runs = [f"Bash(sh {runner} {arg}:*)" for arg in _RUNNER_ARGUMENTS] + [f"Bash(sh \"{runner}\" {arg}:*)" for arg in _RUNNER_ARGUMENTS]
    return runs + [prefix + "node_status", prefix + "get_balance"]


def _sub(text, old, new, rel):
    """Replace old by new exactly once; fail loudly when the shared text has changed."""
    if text.count(old) != 1:
        raise ValueError(f"claude-code: {old!r} does not occur exactly once in {rel}; update targets/claude-code.py")
    return text.replace(old, new)


_CONNECT = {
    "en": "The `anet` server stays connected while anet is not ready and offers one tool, `connect`: call it after the setup "
          "(the agent can do this), and the anet tools become available in the same session. If the server is shown as failed, "
          "reconnect it in `/mcp`.",
    "zh-CN": "anet 尚未就绪时，`anet` 服务保持连接，并只提供一个工具 `connect`：完成安装设置后调用它（智能体可以代为调用），"
             "anet 工具即在同一会话中可用。如果该服务显示为失败，请在 `/mcp` 中重新连接。",
}


def transform(ctx, rel, text, lang):
    meta = ctx.meta
    prefix = tool_prefix(meta)
    # Claude Code starts a plugin command by its namespaced name: the short form is not recognised.
    text = re.sub(r"(?<![\w:])/" + _COMMAND_NAME + r"\b", f"/{meta['name']}:{_COMMAND_NAME}", text)
    if rel == "skills/anet/SKILL.md":
        # The tail of the guide (the part that is not generated from guide.go).
        if lang == "en":
            note = (
                f"Claude Code names the tools of the `{meta['mcp']['server_name']}` server `{prefix}<tool>`, "
                f"for example `{prefix}node_status`."
            )
        else:
            note = (
                f"Claude Code 把 `{meta['mcp']['server_name']}` 服务的工具命名为 `{prefix}<tool>`，例如 `{prefix}node_status`。"
            )
        return text.rstrip("\n") + "\n\n" + note + "\n"
    if rel == "skills/anet-setup/SKILL.md":
        text = _sub(
            text,
            "Then tell the user to start a new Claude Code session, because the MCP server of a plugin starts with\n"
            "the session. In the new session, the `node_status` tool of the `anet` server shows the node. If\n"
            "the tools still do not appear, check the plugin setting **anet executable** and the MCP server\n"
            "status in the Claude Code settings.",
            "Then call the `connect` tool of the `anet` server (`" + prefix + "connect`). The MCP server of this plugin stays "
            "connected while anet is not ready and offers only that tool; once the node answers, `connect` makes the 14 anet "
            "tools available in this session, and the `node_status` tool shows the node. If `connect` is not listed, tell the "
            "user to start a new Claude Code session. If the tools still do not appear, check the plugin setting "
            "**anet executable** and the MCP server status in `/mcp`.",
            rel,
        )
    elif rel == "README.md":
        text = _sub(text, "then start a new session. |", "then call the `connect` tool of the `anet` server or start a new session. " + _CONNECT["en"] + " |", rel)
    elif rel == "README_CN.md":
        text = _sub(text, "然后新建会话。 |", "然后调用 `anet` 服务的 `connect` 工具或新建会话。" + _CONNECT["zh-CN"] + " |", rel)
    if rel == f"commands/{_COMMAND_NAME}.md":
        lines = text.split("\n")
        i = next(n for n, line in enumerate(lines) if line.startswith("description:"))
        lines.insert(i + 1, "allowed-tools: " + ", ".join(_allowed_tools(meta)))
        return "\n".join(lines)
    return text


def build(ctx):
    meta = ctx.meta
    # The shared launcher fails when the node is not ready, and Claude Code remembers
    # such a failed start; the launcher of this target stays connected instead.
    ctx.add_common(skip=("bin/anet-mcp.sh",))
    ctx.add("bin/anet-mcp.sh", _src("anet-mcp.sh") + "\n", executable=True)

    ctx.add_json(".claude-plugin/plugin.json", {
        "name": meta["name"],
        "version": meta["version"],
        "description": meta["description"],
        "author": meta["author"],
        "homepage": meta["homepage"],
        "repository": meta["repository"],
        "license": meta["license"],
        "keywords": meta["keywords"],
        "userConfig": {
            s["key"]: {k: s[k] for k in ("type", "title", "description", "default")} for s in meta["settings"]
        },
    })
    ctx.add_json(".mcp.json", {
        "mcpServers": {
            meta["mcp"]["server_name"]: {
                "type": "stdio",
                "command": "sh",
                "args": ["${CLAUDE_PLUGIN_ROOT}/bin/anet-mcp.sh"],
                "env": {s["env"]: "${user_config.%s}" % s["key"] for s in meta["settings"]},
                "timeout": meta["mcp"]["timeout_ms"],
            }
        }
    })
    ctx.add_root_json(".claude-plugin/marketplace.json", _catalogue(ctx, f"./{ctx.key}"))


def _entry(ctx, source):
    meta = ctx.meta
    return {
        "name": meta["name"],
        "source": source,
        "description": meta["description"],
        "author": meta["author"],
        "homepage": meta["homepage"],
        "license": meta["license"],
        "keywords": meta["keywords"],
        "category": meta["category"],
    }


def _catalogue(ctx, source):
    meta = ctx.meta
    return {
        "name": MARKETPLACE,
        "owner": {"name": meta["author"]["name"], "url": meta["author"]["url"]},
        "metadata": {"description": "The AgentNetwork (anet) plugin for Claude Code."},
        "plugins": [_entry(ctx, source)],
    }


def site(ctx, site):
    if not site.base.startswith("https://"):
        raise ValueError("the site base must be an https:// URL")
    meta = ctx.meta
    site.archive(f"{meta['name']}-{meta['version']}.zip", wrapper=meta["name"], kind="zip")
    source = {"source": "git-subdir", "url": ctx.repo_url, "path": f"{ctx.dist_path}/{ctx.key}"}
    site.add_json("marketplace.json", _catalogue(ctx, source))
