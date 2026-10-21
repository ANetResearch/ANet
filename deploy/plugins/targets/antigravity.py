"""Target antigravity: the plugin for Google Antigravity (command-line program `agy`, desktop application and IDE).

The sibling target gemini-cli (targets/gemini-cli.py) builds the extension for
Gemini CLI from the same shared files. The target-specific sources are in
deploy/plugins/src/antigravity/: the MCP launcher bin/anet-mcp.cjs, the
settings section of the README (English and Chinese) and the draft
documentation page.

Shape of the host. `agy` needs a Google login and was not run; the facts below
come from the public documentation (https://antigravity.google/docs/plugins,
/docs/mcp) and from the shipped `agy` 1.4.4 binaries for Linux and Windows
(Go; symbols and strings inspected, nothing executed).

  * A plugin is a directory with plugin.json at its root. The documented
    schema allows only $schema, name and description (additionalProperties is
    false). Components are found by directory name: skills/<name>/SKILL.md,
    mcp_config.json. There are no slash commands, so there is no /anet entry
    point: the skill anet handles "@anet" requests, and the status command is
    the skill anet-status. There are no plugin settings.
  * MCP start: mcp.(*LocalSubprocessConnector).createTransport passes the
    command through mcp.resolveCommand, which changes only "node" (to the
    copy of Node.js bundled with agy when none is on PATH, log line "No node
    on PATH; starting MCP server %s with %s"), and runs it with os/exec, no
    shell. The validator of agy says "${PLUGIN_ROOT} and ${PLUGIN_DATA} aren't
    expanded in %s, only in "args", "env" and "cwd"" and "MCP configs don't
    expand environment variables". The command is therefore "node" and the
    plugin root is in the argument: node runs bin/anet-mcp.cjs, which starts
    bin/anet-mcp.sh with /bin/sh on Linux and macOS and bin/anet-mcp.cmd with
    %SystemRoot%\\System32\\cmd.exe /d /s /c on Windows. A .cmd file named
    as the command is not used on Windows: os/exec hands a batch file to
    CreateProcess, which runs cmd.exe /c with the command as written, and cmd
    splits a forward-slash path such as ./bin/anet-mcp at the first slash.
  * Executable bits are not needed: node, /bin/sh and cmd.exe read the files.
    (agy keeps them anyway: common.CopyFile copies the mode, plugins.extractZip
    uses the mode of the entry, plugins.gitClone runs git.)
  * The shell of the agent is PowerShell on Windows (changelog 1.0.10,
    "defaulted shell resolution to PowerShell"), so the skills name both
    runners. A skill refers to the runner as <skill-dir>/../../bin/anet-run.sh
    (or .cmd), where <skill-dir> is the directory of its SKILL.md.
  * The instructions of an MCP server are written to a file that the model
    reads on demand (McpManager.writeServerState), not added to the prompt;
    the skill anet tells the agent to read the guide.
  * Settings are the two environment variables ANET_PLUGIN_BIN and
    ANET_PLUGIN_IDENTITY of the process; the launchers and the runners read
    them.

Output (dist/antigravity/ is the plugin root; `agy plugin install <directory>`
installs it, and a GitHub URL install needs it at the root of a repository):

    plugin.json         manifest ($schema, name, description)
    mcp_config.json     the MCP server: node ${PLUGIN_ROOT}/bin/anet-mcp.cjs
    bin/ skills/        the shared files; skills/anet-status is made from the shared command
    README.md README_CN.md LICENSE

There is no catalogue and no site(): Antigravity accepts one marketplace name
only (antigravity-plugins-official), and a listing there is requested with the
interest form of Google.
"""

import json
import re
from pathlib import Path

TITLE = "Antigravity"
HELP = "Google Antigravity (agy, desktop application and IDE); plugin with skills and an MCP server, settings through environment variables."
MARKERS = ["ANTIGRAVITY_PLUGIN_ROOT", "agy plugin", "Antigravity"]

SRC = Path(__file__).resolve().parent.parent / "src" / "antigravity"
SCHEMA = "https://antigravity.google/schemas/v1/plugin.json"
SKILL_DIR = "<skill-dir>"
RUNNER_FROM_SKILL = SKILL_DIR + "/../../bin/anet-run.sh"
STATUS_SKILL = "anet-status"
_SKILL_DIR_NOTE = (
    "`" + SKILL_DIR + "` is the absolute path of the directory that contains this SKILL.md file. The runner sits two "
    "levels above it, in `bin`. Never run a script from the workspace."
)
_NAME_RE = re.compile(r"^[a-zA-Z0-9_-]+$")


def _read(name):
    t = (SRC / name).read_text(encoding="utf-8")
    return t[:-1] if t.endswith("\n") else t


VARS = {
    "TOOL": "Antigravity",
    "RUNNER_FROM_SKILL": RUNNER_FROM_SKILL,
    "RUNNER_FROM_COMMAND": RUNNER_FROM_SKILL,
    "HAS_COMMAND": False,  # no slash commands; the status command is a skill
    "SETTINGS_STORE": False,
    "DOC_SLUG": "antigravity",
    "SETTINGS_SECTION": {"en": _read("antigravity-settings-section.md"), "zh-CN": _read("antigravity-settings-section_CN.md")},
    "INSTALL_STEP": {
        "en": (
            "Install the plugin from a local copy of the ANet repository: `git clone https://github.com/ANetResearch/ANet.git`, "
            "then `agy plugin install ./ANet/deploy/plugins/dist/antigravity`. "
            "Antigravity installs from a GitHub URL only when the plugin sits at the root of the repository, "
            "so a directory of a larger repository is installed from a local path. "
            "If you added an anet MCP server to Antigravity yourself, remove that entry first, so that Antigravity does not have two anet servers."
        ),
        "zh-CN": (
            "从 ANet 代码仓库的本地副本安装插件：`git clone https://github.com/ANetResearch/ANet.git`，"
            "然后执行 `agy plugin install ./ANet/deploy/plugins/dist/antigravity`。"
            "Antigravity 仅在插件位于仓库根目录时才支持通过 GitHub URL 安装，因此大型仓库中的子目录需从本地路径安装。"
            "如果此前自行在 Antigravity 中添加过 anet MCP 服务，请先移除该条目，以免 Antigravity 中出现两个 anet 服务。"
        ),
    },
    "COMPONENTS_SAME_RUNNER": {
        "en": "the MCP server (`anet mcp`) and the skills `anet`, `anet-setup` and `anet-status`",
        "zh-CN": "MCP 服务（`anet mcp`）与 `anet`、`anet-setup`、`anet-status` skill ",
    },
    "RUNNER_NOTE": _SKILL_DIR_NOTE,
    "RUNNER_COMMAND_NOTE": _SKILL_DIR_NOTE,
    "ARGUMENTS": "the part of the request that names something specific to check, if any.",
}


def validate(ctx):
    meta = ctx.meta
    if not _NAME_RE.match(meta["name"]):
        raise ValueError(f"plugin name {meta['name']!r} is not a valid Antigravity plugin name (letters, digits, hyphens, underscores)")


def _replace_once(text, old, new, where):
    if text.count(old) != 1:
        raise ValueError(f"antigravity: {where}: expected one occurrence of {old!r}, found {text.count(old)}; update targets/antigravity.py")
    return text.replace(old, new)




def transform(ctx, rel, text, lang):
    """Antigravity-specific wording that the variables do not cover."""
    if rel == "skills/anet-setup/SKILL.md":
        text = _replace_once(text, "**anet executable** (`anetPath`)", "**anet executable** (the variable `ANET_PLUGIN_BIN`)", rel)
        text = _replace_once(text, "**anet identity** (`identity`)", "**anet identity** (the variable `ANET_PLUGIN_IDENTITY`)", rel)
        text = _replace_once(
            text,
            "the plugin setting **anet executable** names a file",
            "the setting **anet executable** (`ANET_PLUGIN_BIN`) names a file",
            rel,
        )
        text = _replace_once(
            text,
            "they hold the identity keys and the control token.\n",
            "they hold the identity keys and the control token.\n"
            "- Never read or edit the configuration or the credential files of Antigravity (under `~/.gemini`).\n"
            "- If the host refuses a command of this skill, report the refusal together with the command and stop: do not\n"
            "  write copies or wrapper scripts of the runner or of the installer to work around it.\n",
            rel,
        )
    elif rel == "commands/anet-status.md":
        text = _replace_once(
            text,
            "Do not change any setting. Optional focus from the user:",
            "Do not change any setting. If the host refuses a runner command, report the refusal together with the command and stop: "
            "do not write copies or wrapper scripts of the runner to work around it. Optional focus from the user:",
            rel,
        )
    elif rel == "README.md":
        text = _replace_once(
            text,
            "ask the agent to show the state of the anet node.",
            "ask the agent to run the `anet-status` skill, which shows the state of the anet node.",
            rel,
        )
        text = _replace_once(
            text,
            "| Skill | `anet-setup` |",
            "| Skill | `anet-status` | Shows the state of the node, as the guide of the installed anet describes. "
            "Antigravity has no plugin commands, so the check is a skill. |\n| Skill | `anet-setup` |",
            rel,
        )
    elif rel == "README_CN.md":
        text = _replace_once(
            text,
            "然后让智能体显示 anet 节点的状态。",
            "然后让智能体运行 `anet-status` skill，显示 anet 节点的状态。",
            rel,
        )
        text = _replace_once(
            text,
            "| Skill | `anet-setup` |",
            "| Skill | `anet-status` | 按已安装 anet 的指南显示节点状态。Antigravity 的插件没有命令，因此该检查以 skill 提供。 |\n| Skill | `anet-setup` |",
            rel,
        )
    return text


def _status_skill(ctx):
    description, body = ctx.command("anet-status")
    description = description.rstrip(".") + ". Use when the user asks for the state of the anet node or its balance."
    return "---\nname: " + STATUS_SKILL + "\ndescription: " + json.dumps(description, ensure_ascii=False) + "\n---\n\n" + body


def build(ctx):
    meta = ctx.meta
    ctx.add_common(groups=("bin", "skills", "docs"))
    ctx.add("bin/anet-mcp.cjs", _read("anet-mcp.cjs") + "\n")
    ctx.add(f"skills/{STATUS_SKILL}/SKILL.md", _status_skill(ctx))

    ctx.add_json("plugin.json", {
        "$schema": SCHEMA,
        "name": meta["name"],
        "description": meta["description"],
    })
    ctx.add_json("mcp_config.json", {
        "mcpServers": {
            meta["mcp"]["server_name"]: {
                # Only "args", "env" and "cwd" expand ${PLUGIN_ROOT}; see the module docstring.
                "command": "node",
                "args": ["${PLUGIN_ROOT}/bin/anet-mcp.cjs"],
            }
        }
    })
