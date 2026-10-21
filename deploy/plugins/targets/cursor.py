"""Target cursor: the plugin for Cursor (editor and Cursor Agent command-line program).

The target-specific sources are in deploy/plugins/src/cursor/: the runner
comment, the settings section of the README (English and Chinese) and
repo-root-marketplace.json, the catalogue for the root of the ANet repository
(see below). Output (dist/cursor/ is the plugin root, as Cursor loads it):

    .cursor-plugin/plugin.json   manifest, with the two plugin variables
    mcp.json                     the MCP server: ${CURSOR_PLUGIN_ROOT}/bin/anet-mcp
    bin/ skills/ commands/       the shared files
    assets/logo.png              logo, referenced by the manifest
    README.md README_CN.md LICENSE

Root file (for the mirror repository, whose root is dist/):

    .cursor-plugin/marketplace.json   catalogue "agentnetwork", source "cursor"

Cursor reads a catalogue only from the root of a Git repository (`plugin
marketplace add <git-url>`, a team marketplace, the submission form), and a
source is a path relative to that root. The ANet repository holds the plugin at
deploy/plugins/dist/cursor, so its own root needs a catalogue; the generator
cannot write outside dist/, and src/cursor/repo-root-marketplace.json is that
file, to be committed as .cursor-plugin/marketplace.json at the repository root.
tests/test_cursor.py keeps it consistent with this module.

There is no site() hook: Cursor does not install from an HTTPS catalogue or an
archive.

Cursor expands ${CURSOR_PLUGIN_ROOT} in the MCP configuration only. A skill has
no variable for its own directory and a command has no inline execution, so
the shared texts name the runner as <skill-dir>/../../bin/anet-run.sh (the
agent knows where it loaded the skill from) and the commands refer to the
plugin root in words (RUNNER_NOTE, RUNNER_COMMAND_NOTE). The @anet entry point
is the command /anet.

Settings: Cursor has no per-user settings file for a plugin. Any ${NAME} (or
${NAME:-default}) of mcp.json becomes a plugin variable, which a team
administrator configures. mcp.json passes the two settings of the shared
metadata that way, with an empty default, and the manifest declares them. The
launchers (bin/anet-mcp, bin/anet-mcp.cmd) read them from their environment; the stdio transport
of Cursor does not pass the rest of the environment, so nothing else is
relied on (on Windows the MCP SDK adds its default variables, among them
PATH, SYSTEMROOT, USERPROFILE and LOCALAPPDATA, which the runner uses).

Start of the MCP server, checked in the Cursor Agent program 2026.10.19 for
Linux and Windows (dist-package/index.js; nothing executed):

  * ${CURSOR_PLUGIN_ROOT} (and ${CLAUDE_PLUGIN_ROOT}) is replaced in command,
    args, env and cwd of a plugin MCP server; the documentation says the same
    (https://cursor.com/docs/plugins/building).
  * Windows: StdioClientTransport of the MCP SDK spawns with the bundled
    cross-spawn 7.0.6 and shell false. cross-spawn looks the extensionless
    command up with the extensions of PATHEXT, finds bin/anet-mcp.cmd and runs
    it through cmd.exe /d /s /c with escaped arguments; the extensionless POSIX
    file is not chosen, because isexe accepts only a PATHEXT extension.
  * Linux and macOS: the command bin/anet-mcp is executed directly, so it
    needs its executable bit. Cursor keeps it: a marketplace plugin is fetched
    with git and copied with fs.cp into ~/.cursor/plugins/cache, which both
    keep the mode, and node-tar keeps the mode of a release archive.
  * The shell of the agent on Windows is PowerShell (pwsh, else Windows
    PowerShell), or Git Bash when Cursor runs inside it; the texts name both
    runners.
  * The instructions of the server are sent with the request (mcpInstructions).
The editor application has its own MCP host and was not inspected.
"""

import re
from pathlib import Path

TITLE = "Cursor"
HELP = "Cursor (editor and Cursor Agent command-line program); plugin with plugin variables and a slash command."
MARKERS = ["CURSOR_PLUGIN_ROOT", ".cursor-plugin", "cursor-agent"]

MARKETPLACE = "agentnetwork"
_SRC = Path(__file__).resolve().parent.parent / "src" / "cursor"


def _src(name):
    text = (_SRC / name).read_text(encoding="utf-8")
    return text[:-1] if text.endswith("\n") else text


_SKILL_DIR = "<skill-dir>"
_COMMANDS = ("anet", "anet-status")
_NAME_RE = re.compile(r"^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$")
_SEMVER_RE = re.compile(r"^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$")
_VARIABLE_RE = re.compile(r"^[A-Z][A-Z0-9_]*$")

VARS = {
    "TOOL": "Cursor",
    "RUNNER_FROM_SKILL": _SKILL_DIR + "/../../bin/anet-run.sh",
    "RUNNER_FROM_COMMAND": "<plugin-root>/bin/anet-run.sh",
    "SETTINGS_STORE": False,
    "DOC_SLUG": "cursor",
    "ARGUMENTS": {
        "en": "any text that follows the command name",
        "zh-CN": "命令之后的文字（如有）",
    },
    "RUNNER_NOTE": (
        f"`{_SKILL_DIR}` is the directory that contains this `SKILL.md` file; use the absolute path under which Cursor "
        "loaded the skill. The runner sits two levels above it, in `bin`."
    ),
    "RUNNER_COMMAND_NOTE": (
        "`<plugin-root>` is the root directory of the anet plugin: the directory that holds `bin`, `skills` and "
        "`.cursor-plugin`, which is two levels above the file `SKILL.md` of the `anet-setup` skill."
    ),
    "INSTALL_STEP": {
        "en": (
            "Add the repository of anet as a plugin marketplace and install the plugin: "
            "`cursor-agent plugin marketplace add https://github.com/ANetResearch/ANet.git`, "
            "or import that repository as a team marketplace in the Cursor dashboard. "
            "To try the plugin from a local copy, start the command-line program with "
            "`cursor-agent --plugin-dir <directory of this plugin>`. "
            "If you registered anet earlier with `anet agents wire cursor`, run `anet agents unwire cursor` first, "
            "so that Cursor does not have two anet servers."
        ),
        "zh-CN": (
            "把 anet 的代码仓库添加为插件市场并安装插件："
            "`cursor-agent plugin marketplace add https://github.com/ANetResearch/ANet.git`，"
            "或在 Cursor 控制台中把该仓库导入为团队市场。"
            "如需从本地副本试用，用 `cursor-agent --plugin-dir <本插件所在目录>` 启动命令行程序。"
            "如果此前用 `anet agents wire cursor` 注册过 anet，请先执行 `anet agents unwire cursor`，"
            "以免 Cursor 中出现两个 anet 服务。"
        ),
    },
    "SETTINGS_SECTION": {"en": _src("settings-section.md"), "zh-CN": _src("settings-section_CN.md")},
    "SETTINGS_READ_NOTE": {
        "en": (
            " The MCP server receives the plugin variables from Cursor; the setup skill and the status command run in a shell "
            "of the agent, where the script reads the same two variables, and nothing else, from the environment."
        ),
        "zh-CN": (
            "MCP 服务从 Cursor 获得插件变量；安装 skill 与状态命令在智能体的 shell 中运行，"
            "此时脚本只从环境变量中读取同名的两个变量。"
        ),
    },
    "RUNNER_SETTINGS_COMMENT": _src("runner-settings-comment.txt"),
}


def validate(ctx):
    meta = ctx.meta
    if not _NAME_RE.match(meta["name"]):
        raise ValueError(f"plugin name {meta['name']!r} is not a valid Cursor plugin name")
    if not _SEMVER_RE.match(meta["version"]):
        raise ValueError(f"version {meta['version']!r} is not a semantic version")
    for s in meta["settings"]:
        if not _VARIABLE_RE.match(s["env"]):
            raise ValueError(f"environment variable {s['env']!r} cannot be a Cursor plugin variable (A-Z, 0-9, _)")
        if s["type"] != "string" or s["default"] != "":
            raise ValueError(f"setting {s['key']!r}: the MCP configuration passes it as a string with an empty default")
        if "}" in s["env"] or ":" in s["env"]:
            raise ValueError(f"environment variable {s['env']!r} breaks the variable syntax")


def _replace_once(text, old, new, rel):
    if text.count(old) != 1:
        raise ValueError(f"{rel}: expected exactly one occurrence of {old!r}; the shared source changed, update targets/cursor.py")
    return text.replace(old, new)


def transform(ctx, rel, text, lang):
    for name in _COMMANDS:
        if rel == f"commands/{name}.md":
            return _replace_once(text, "description:", f"name: {name}\ndescription:", rel)
    return text


def _variables(meta):
    """The plugin variables: the environment variables that mcp.json passes to the server."""
    return {
        "type": "object",
        "properties": {
            s["env"]: {"type": "string", "title": s["title"], "description": s["description"], "default": s["default"]}
            for s in meta["settings"]
        },
    }


def _catalogue(ctx, source):
    meta = ctx.meta
    return {
        "name": MARKETPLACE,
        "owner": {"name": meta["author"]["name"]},
        "metadata": {"description": "The AgentNetwork (anet) plugin for Cursor."},
        "plugins": [{"name": meta["name"], "source": source, "description": meta["description"]}],
    }


def build(ctx):
    meta = ctx.meta
    ctx.add_common()
    ctx.add("assets/logo.png", ctx.icon)

    ctx.add_json(".cursor-plugin/plugin.json", {
        "name": meta["name"],
        "displayName": meta["display_name"]["en"],
        "version": meta["version"],
        "description": meta["description"],
        "author": {"name": meta["author"]["name"]},
        "homepage": meta["homepage"],
        "repository": meta["repository"],
        "license": meta["license"],
        "logo": "assets/logo.png",
        "keywords": meta["keywords"],
        "category": meta["category"],
        "variables": _variables(meta),
    })
    ctx.add_json("mcp.json", {
        "mcpServers": {
            meta["mcp"]["server_name"]: {
                "type": "stdio",
                # bin/anet-mcp on Linux and macOS, bin/anet-mcp.cmd on Windows (see the module docstring).
                "command": "${CURSOR_PLUGIN_ROOT}/bin/anet-mcp",
                "args": [],
                # An unset variable falls back to the empty default instead of staying a literal ${...}.
                "env": {s["env"]: "${%s:-}" % s["env"] for s in meta["settings"]},
            }
        }
    })
    ctx.add_root_json(".cursor-plugin/marketplace.json", _catalogue(ctx, ctx.key))
