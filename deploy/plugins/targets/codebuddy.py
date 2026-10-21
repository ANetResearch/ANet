"""Target codebuddy: the plugin for CodeBuddy Code (Tencent CodeBuddy) and WorkBuddy.

WorkBuddy embeds the CodeBuddy engine and reads the same plugin layout, so one
output serves both.

Output (dist/codebuddy/ is the plugin root, as the host installs it):

    .codebuddy-plugin/plugin.json   manifest with userConfig (identity, anetPath)
    .mcp.json                       the MCP server, started through bin/anet-mcp.sh
    bin/ skills/ commands/          the shared files
    README.md README_CN.md LICENSE

Root file of the output (dist/ is the root of the optional mirror repository):

    .codebuddy-plugin/marketplace.json   the catalogue, entry source "./codebuddy"

Site (served at https://agentnetwork.org.cn/plugins/codebuddy/):

    marketplace.json   the same catalogue with a git-subdir source that names
                       deploy/plugins/dist/codebuddy in the ANet repository;
                       the host adds it by URL:
                       codebuddy plugin marketplace add <URL of marketplace.json>

The settings (identity, anetPath) are userConfig of the plugin. The host
substitutes ${user_config.<key>} in the MCP configuration, so the MCP server
receives ANET_PLUGIN_IDENTITY and ANET_PLUGIN_BIN. Outside the MCP server
(setup procedure and status command) the runner reads only the two plugin
options from the user settings of the host (~/.codebuddy/settings.json or
~/.workbuddy/settings.json, or the directory that CODEBUDDY_CONFIG_DIR or
WORKBUDDY_CONFIG_DIR names) and nothing else of that file, which also holds
other settings of the host; the agent is told never to read it itself. When both
files hold different options of the plugin, nothing is selected.

Host facts (CodeBuddy Code 2.169.0): the manifest directory is
.codebuddy-plugin (then .workbuddy-plugin, then .claude-plugin); the variables
${CODEBUDDY_PLUGIN_ROOT} and ${CLAUDE_PLUGIN_ROOT} are expanded in .mcp.json,
in hooks, and in skill and command text; ${CODEBUDDY_SKILL_DIR} in skill text.

Launch on Windows and POSIX (read in @tencent-ai/codebuddy-code 2.171.0,
dist/codebuddy.js):

  * A stdio server is spawned by the StdioClientTransport of the MCP
    TypeScript SDK with cross-spawn, shell:false. ${CODEBUDDY_PLUGIN_ROOT} and
    ${user_config.KEY} are replaced in command, args, env and cwd
    (substitutePluginVariables).
  * The command is ${CODEBUDDY_PLUGIN_ROOT}/bin/anet-mcp, without an extension.
    On Windows cross-spawn tries the path with each extension of PATHEXT (an
    extensionless file is not executable there), finds bin/anet-mcp.cmd and
    runs it through cmd.exe /d /s /c. On Linux and macOS bin/anet-mcp is run
    itself: a shell script that starts bin/anet-mcp.sh with sh.
  * The executable bit survives the installation: a marketplace is fetched
    with git clone, and copyPluginToCache copies each file with copyFile and
    then applies the mode of the source with chmod.
  * The agent uses Git Bash on Windows when it finds one, otherwise a
    PowerShell tool that refuses calls of bash and sh; the skills give the
    Windows runner bin/anet-run.cmd next to sh bin/anet-run.sh.
"""

import re
from pathlib import Path

TITLE = "CodeBuddy"
HELP = "CodeBuddy Code of Tencent and WorkBuddy (same plugin engine); plugin with user settings, a slash command and a git catalogue."
MARKERS = ["CODEBUDDY_", "CodeBuddy", ".codebuddy"]

_ROOT = "${CODEBUDDY_PLUGIN_ROOT}"
_SRC = Path(__file__).resolve().parent.parent / "src" / "codebuddy"
_MARKETPLACE = "agentnetwork"
_CATALOGUE_URL = "https://agentnetwork.org.cn/plugins/codebuddy/marketplace.json"


def _src(name):
    text = (_SRC / name).read_text(encoding="utf-8")
    return text[:-1] if text.endswith("\n") else text


VARS = {
    "TOOL": "CodeBuddy",
    # The host expands both variables in the text of skills and commands when it loads them.
    "RUNNER_FROM_SKILL": _ROOT + "/bin/anet-run.sh",
    "RUNNER_FROM_COMMAND": _ROOT + "/bin/anet-run.sh",
    "SETTINGS_STORE": True,
    "CONFIG_DIR": "~/.codebuddy` or `~/.workbuddy",
    "DOC_SLUG": "codebuddy",
    "INSTALL_STEP": {
        "en": (
            "Add the marketplace and install the plugin: "
            "`codebuddy plugin marketplace add " + _CATALOGUE_URL + "`, "
            "then `codebuddy plugin install anet@" + _MARKETPLACE + "` "
            "(in a session: `/plugin marketplace add ...` and `/plugin install anet@" + _MARKETPLACE + "`). "
            "The marketplace names a subdirectory of the ANet repository, so `git` must be installed. "
            "WorkBuddy embeds the CodeBuddy engine and reads the same plugin layout. "
            "If you registered anet earlier with `anet agents wire workbuddy`, run `anet agents unwire workbuddy` first, "
            "so that the host does not have two anet servers."
        ),
        "zh-CN": (
            "添加插件市场并安装插件："
            "`codebuddy plugin marketplace add " + _CATALOGUE_URL + "`，"
            "然后执行 `codebuddy plugin install anet@" + _MARKETPLACE + "`"
            "（会话中为 `/plugin marketplace add ...` 与 `/plugin install anet@" + _MARKETPLACE + "`）。"
            "该插件市场指向 ANet 仓库的一个子目录，因此需要安装 `git`。"
            "WorkBuddy 内嵌 CodeBuddy 引擎，读取相同的插件目录结构。"
            "如果此前用 `anet agents wire workbuddy` 注册过 anet，请先执行 `anet agents unwire workbuddy`，"
            "以免宿主中出现两个 anet 服务。"
        ),
    },
    "SETTINGS_SECTION": {"en": _src("settings-section.md"), "zh-CN": _src("settings-section_CN.md")},
    "SETTINGS_READ_NOTE": {
        "en": " Outside the MCP server, the script reads these two options, and nothing else, from the user settings of CodeBuddy.",
        "zh-CN": "在 MCP 服务之外，该脚本从 CodeBuddy 的用户设置中只读取这两项选项。",
    },
    "RUNNER_SETTINGS_COMMENT": _src("runner-settings-comment.txt"),
    "RUNNER_READ_SETTINGS": _src("runner-read-settings.sh"),
    "RUNNER_READ_SETTINGS_PS": _src("runner-read-settings.ps1"),
}

_NAME_RE = re.compile(r"^[a-z0-9][a-z0-9-]{0,63}$")


def validate(ctx):
    name = ctx.meta["name"]
    if not _NAME_RE.match(name):
        raise ValueError(f"plugin name {name!r} is not a valid CodeBuddy plugin name (kebab-case)")


def transform(ctx, rel, text, lang):
    """Name the commands as CodeBuddy does and, in the guide, say how it names and loads the MCP tools."""
    # CodeBuddy namespaces the commands of a plugin: /<plugin>:<command>.
    for name in ("anet", "anet-status"):
        text = text.replace("`/%s`" % name, "`/%s:%s`" % (ctx.meta["name"], name))
    if rel != "skills/anet/SKILL.md":
        return text
    server = ctx.meta["mcp"]["server_name"]
    if lang == "en":
        note = (
            f"CodeBuddy names the tools of the `{server}` server `mcp__{server}__<tool>`; the guide is "
            f"`mcp__{server}__guide`. It can load tools on demand: when they are not in the list of tools, "
            "search for `anet` with the tool search of CodeBuddy, then call the tool it returns."
        )
    else:
        note = (
            f"CodeBuddy 把 `{server}` 服务的工具命名为 `mcp__{server}__<tool>`；指南工具为 `mcp__{server}__guide`。"
            "它可以按需加载工具：若工具列表中没有它们，先用 CodeBuddy 的工具搜索查找 `anet`，再调用返回的工具。"
        )
    return text.rstrip("\n") + "\n\n" + note + "\n"


def _entry(ctx, source):
    meta = ctx.meta
    return {
        "name": meta["name"],
        "description": meta["description"],
        "version": meta["version"],
        "author": {"name": meta["author"]["name"]},
        "homepage": meta["homepage"],
        "license": meta["license"],
        "keywords": meta["keywords"],
        "category": meta["category"],
        "source": source,
    }


def _catalogue(ctx, source):
    return {
        "name": _MARKETPLACE,
        "description": "The AgentNetwork (anet) plugin for CodeBuddy and WorkBuddy.",
        "owner": {"name": ctx.meta["author"]["name"], "url": ctx.meta["author"]["url"]},
        "plugins": [_entry(ctx, source)],
    }


def build(ctx):
    meta = ctx.meta
    ctx.add_common()

    ctx.add_json(".codebuddy-plugin/plugin.json", {
        "name": meta["name"],
        "version": meta["version"],
        "description": meta["description"],
        "author": {"name": meta["author"]["name"], "url": meta["author"]["url"]},
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
                # bin/anet-mcp on Linux and macOS, bin/anet-mcp.cmd on Windows (see the module docstring).
                "command": _ROOT + "/bin/anet-mcp",
                "args": [],
                "env": {s["env"]: "${user_config.%s}" % s["key"] for s in meta["settings"]},
            }
        }
    })
    ctx.add_root_json(".codebuddy-plugin/marketplace.json", _catalogue(ctx, "./" + ctx.key))


def site(ctx, site):
    source = {
        "source": "git-subdir",
        "url": ctx.repo_url + ".git",
        "path": ctx.dist_path + "/" + ctx.key,
    }
    site.add_json("marketplace.json", _catalogue(ctx, source))
