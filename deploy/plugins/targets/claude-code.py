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
ANET_PLUGIN_BIN with ${user_config.KEY}. Outside the MCP server (skills and
commands) the runner reads the same two options from the user settings
(bin/anet-run.sh on Linux and macOS, bin/anet-run.cmd with anet-run.ps1 on
Windows).

Launch on Windows and POSIX (verified in the native binary of Claude Code
2.1.306 for win32-x64 and linux-x64, by reading its embedded JavaScript):

  * The stdio transport is the one of the MCP TypeScript SDK and spawns with
    cross-spawn, shell:false. ${CLAUDE_PLUGIN_ROOT} is replaced in command,
    args and env (with forward slashes on Windows). The stdio schema has no
    cwd; the server starts in the project directory.
  * .mcp.json therefore names "${CLAUDE_PLUGIN_ROOT}/bin/anet-mcp" without an
    extension and without arguments. On Windows cross-spawn resolves it through
    PATHEXT to bin/anet-mcp.cmd (an extensionless file is not executable there)
    and runs it through cmd.exe /d /s /c. On Linux and macOS the file
    bin/anet-mcp is run itself: a shell script with the executable bit, which
    starts bin/anet-mcp.sh with sh.
  * The executable bit survives the installation: a marketplace is fetched with
    git clone (mode 100755 is kept), and the copy into the plugin cache uses
    copyFile, which creates the copy with the mode of the source.
  * The agent runs commands in Git Bash on Windows when Git is installed, or
    with a PowerShell tool otherwise; the skills therefore give both runner
    forms (sh anet-run.sh, which also works in Git Bash, and anet-run.cmd).
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

_COMMANDS = ("anet", "anet-status")
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
    "RUNNER_READ_SETTINGS_PS": _src("runner-read-settings.ps1"),
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


_RUNNER_ARGUMENTS = {"anet-status": ("--resolve", "guide", "status", "doctor"), "anet": ("--resolve", "guide")}


def _allowed_tools(meta, command):
    """The tools that a command may use without a prompt: read-only runner commands and the MCP tool guide.

    The status command reads the guide and, without the MCP tools, `status` and `doctor`; the
    @anet command reads the guide. Every other use of the runner stays subject to the normal
    permission prompt. Both spellings of the POSIX runner path are listed because the agent may
    quote it, and the Windows runner is listed for the Git Bash of Claude Code on Windows.
    """
    prefix = tool_prefix(meta)
    root = "${CLAUDE_PLUGIN_ROOT}/bin/"
    runs = []
    for arg in _RUNNER_ARGUMENTS[command]:
        runs += [f"Bash(sh {root}anet-run.sh {arg}:*)", f"Bash(sh \"{root}anet-run.sh\" {arg}:*)",
                 f"Bash(\"{root}anet-run.cmd\" {arg}:*)"]
    return runs + [prefix + "guide"]


def _sub(text, old, new, rel):
    """Replace old by new exactly once; fail loudly when the shared text has changed."""
    if text.count(old) != 1:
        raise ValueError(f"claude-code: {old!r} does not occur exactly once in {rel}; update targets/claude-code.py")
    return text.replace(old, new)


_CONNECT = {
    "en": "On Linux and macOS, the `anet` server stays connected while anet is not ready and offers one tool, `connect`: call it "
          "after the setup (the agent can do this), and the anet tools become available in the same session. If the server is "
          "shown as failed, reconnect it in `/mcp`.",
    "zh-CN": "在 Linux 和 macOS 上，anet 尚未就绪时，`anet` 服务保持连接，并只提供一个工具 `connect`：完成安装设置后调用它（智能体可以代为调用），"
             "anet 工具即在同一会话中可用。如果该服务显示为失败，请在 `/mcp` 中重新连接。",
}

_SETUP_RESTART = ("When the MCP tools of anet are still not available after the setup, tell the user to start a\n"
                  "new Claude Code session, because the MCP server of a plugin starts with the session.")


def transform(ctx, rel, text, lang):
    meta = ctx.meta
    prefix = tool_prefix(meta)
    # Claude Code starts a plugin command by its namespaced name: the short form is not recognised.
    for name in _COMMANDS:
        text = text.replace(f"`/{name}`", f"`/{meta['name']}:{name}`")
    if rel == "skills/anet/SKILL.md":
        if lang == "en":
            note = (
                f"Claude Code names the tools of the `{meta['mcp']['server_name']}` server `{prefix}<tool>`; the guide is "
                f"`{prefix}guide`."
            )
        else:
            note = f"Claude Code 把 `{meta['mcp']['server_name']}` 服务的工具命名为 `{prefix}<tool>`；指南工具为 `{prefix}guide`。"
        return text.rstrip("\n") + "\n\n" + note + "\n"
    if rel == "skills/anet-setup/SKILL.md":
        text = _sub(
            text,
            _SETUP_RESTART,
            "When the MCP tools of anet are still not available after the setup, call the `connect` tool of the\n"
            "`anet` server (`" + prefix + "connect`): on Linux and macOS the MCP server of this plugin stays\n"
            "connected while anet is not ready and offers only that tool, and `connect` makes the anet tools\n"
            "available in this session. If `connect` is not listed, tell the user to start a new Claude Code\n"
            "session; if the tools still do not appear, check the plugin setting **anet executable** and the\n"
            "MCP server status in `/mcp`.",
            rel,
        )
    elif rel == "README.md":
        text = _sub(text, "then start a new session. |", "then call the `connect` tool of the `anet` server or start a new session. " + _CONNECT["en"] + " |", rel)
    elif rel == "README_CN.md":
        text = _sub(text, "然后新建会话。 |", "然后调用 `anet` 服务的 `connect` 工具或新建会话。" + _CONNECT["zh-CN"] + " |", rel)
    for name in _COMMANDS:
        if rel == f"commands/{name}.md":
            lines = text.split("\n")
            i = next(n for n, line in enumerate(lines) if line.startswith("description:"))
            lines.insert(i + 1, "allowed-tools: " + ", ".join(_allowed_tools(meta, name)))
            return "\n".join(lines)
    return text


def build(ctx):
    meta = ctx.meta
    # The shared launcher fails when anet is missing (or, with anet before 0.3.8, when the node
    # does not run), and Claude Code remembers such a failed start; the launcher of this target
    # installs anet in the same way and then stays connected instead (src/claude-code/anet-mcp.sh).
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
                # bin/anet-mcp on Linux and macOS, bin/anet-mcp.cmd on Windows (see the module docstring).
                "command": "${CLAUDE_PLUGIN_ROOT}/bin/anet-mcp",
                "args": [],
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
    site.archive(f"{meta['name']}-{meta['version']}.zip", wrapper=meta["name"], kind="zip", exec_bits=True)
    source = {"source": "git-subdir", "url": ctx.repo_url, "path": f"{ctx.dist_path}/{ctx.key}"}
    site.add_json("marketplace.json", _catalogue(ctx, source))
