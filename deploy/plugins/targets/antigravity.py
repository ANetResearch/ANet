"""Target antigravity: the plugin for Google Antigravity (command-line program `agy`, desktop application and IDE).

The sibling target gemini-cli (targets/gemini-cli.py) builds the extension for
Gemini CLI from the same shared files. The target-specific sources are in
deploy/plugins/src/antigravity/: the MCP launcher fragment, the settings
section of the README (English and Chinese) and the draft documentation page.

Shape of the host. Only the public documentation was available
(https://antigravity.google/docs/plugins, /docs/mcp, /docs/marketplace); `agy`
needs a Google login and was not run, so the layout below is checked against
the documented rules and not against the program.

  * A plugin is a directory with plugin.json at its root. The documented
    schema allows only $schema, name and description (additionalProperties is
    false), so the manifest carries nothing else.
  * Components are found by directory name at the plugin root: skills/<name>/
    SKILL.md, mcp_config.json, hooks.json, agents/, rules/. There are no
    slash commands and no plugin settings.
  * The documentation defines no variable for the plugin directory in
    mcp_config.json. The server is therefore started by `sh -c` with a short
    program (src/antigravity/mcp-launch.sh) that looks for bin/anet-mcp.sh in
    the user-level places where the host keeps an installed plugin (and in
    ANTIGRAVITY_PLUGIN_ROOT when the host sets it). It never runs a file of the
    working directory or of the workspace, which can come from an untrusted
    source, so a plugin installed at workspace scope is not started unless
    ANTIGRAVITY_PLUGIN_ROOT names it.
  * Settings are the two environment variables ANET_PLUGIN_BIN and
    ANET_PLUGIN_IDENTITY of the process; the launcher bin/anet-mcp.sh and the
    runner read them.
  * The status command is the skill anet-status. A skill refers to the runner
    as <skill-dir>/../../bin/anet-run.sh, where <skill-dir> is the directory
    of its SKILL.md.

Output (dist/antigravity/ is the plugin root; `agy plugin install <directory>`
installs it, and a GitHub URL install needs it at the root of a repository):

    plugin.json         manifest ($schema, name, description)
    mcp_config.json    the MCP server, started through bin/anet-mcp.sh
    bin/ skills/       the shared files; skills/anet-status is made from the shared command
    bin/anet-install.sh  the official installer as one plain command (common/bin/anet-install.sh)
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
        "en": "the MCP server (`bin/anet-mcp.sh`, `anet mcp`) and the `anet-status` and `anet-setup` skills",
        "zh-CN": "MCP 服务（`bin/anet-mcp.sh`，即 `anet mcp`）与 `anet-status`、`anet-setup` skill ",
    },
    "RESOLVE_INVOCATION": (
        '```bash\nsh "' + RUNNER_FROM_SKILL + '" --resolve\n```\n\n'
        "`" + SKILL_DIR + "` is the absolute path of the directory that contains this SKILL.md file."
    ),
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
    if rel == "skills/anet/SKILL.md":
        # The last section is owned by the target; the part generated from guide.go is untouched.
        text = _replace_once(
            text,
            "setup with the executable and identity of the plugin settings.",
            "setup with the executable and identity of the plugin settings. Here `" + SKILL_DIR
            + "` is the absolute path of the directory that contains this SKILL.md file.",
            rel,
        )
    elif rel == "skills/anet-setup/SKILL.md":
        text = _replace_once(
            text,
            "The `runner` line of the output is the command to use;",
            "Here `" + SKILL_DIR + "` is the absolute path of the directory that contains this SKILL.md file; the runner sits\n"
            "two levels above it, in `bin`. If no file exists there, the plugin directory `anet` is in `~/.gemini/config/plugins`,\n"
            "or in `~/.gemini/antigravity-cli/plugins`; use its `bin/anet-run.sh`. Never run a script from the workspace.\n"
            "The `runner` line of the output is the command to use;",
            rel,
        )
        text = _replace_once(text, "**anet executable** (`anetPath`)", "**anet executable** (the variable `ANET_PLUGIN_BIN`)", rel)
        text = _replace_once(text, "**anet identity** (`identity`)", "**anet identity** (the variable `ANET_PLUGIN_IDENTITY`)", rel)
        text = _replace_once(text, "status in the Antigravity settings.", "status in the MCP server list of Antigravity.", rel)
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
            "Never read or edit the configuration or credential files of Antigravity (under `~/.gemini`); the only access the\n"
            "plugin needs there is to find the plugin directory `anet`, as described in step 2.\n"
            "If the host refuses a command of this skill, report the refusal together with the command and stop: do not write copies or wrapper scripts of the runner or of the installer to work around it.\n",
            rel,
        )
    elif rel == "commands/anet-status.md":
        text = _replace_once(
            text,
            "The plugin settings select this executable and identity (resolved when the command started):",
            "The plugin settings select an executable and an identity. Resolve them first:",
            rel,
        )
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
            "| Skill | `anet-status` | Shows the node, its hub, the balance and the spending limits. Antigravity has no plugin commands, so the check is a skill. |\n| Skill | `anet-setup` |",
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
            "| Skill | `anet-status` | 显示节点、所加入的 hub、余额和支付额度。Antigravity 的插件没有命令，因此该检查以 skill 提供。 |\n| Skill | `anet-setup` |",
            rel,
        )
    return text


def _status_skill(ctx):
    description, body = ctx.command("anet-status")
    description = description.rstrip(".") + ". Use when the user asks for the state of the anet node, the balance or the spending limits."
    return "---\nname: " + STATUS_SKILL + "\ndescription: " + json.dumps(description, ensure_ascii=False) + "\n---\n\n" + body


def mcp_launch_program(name):
    """The sh program that finds and starts bin/anet-mcp.sh of the installed plugin."""
    return _read("mcp-launch.sh").replace("@NAME@", name) + "\n"


def build(ctx):
    meta = ctx.meta
    ctx.add_common(groups=("bin", "skills", "docs"))
    ctx.add(f"skills/{STATUS_SKILL}/SKILL.md", _status_skill(ctx))

    ctx.add_json("plugin.json", {
        "$schema": SCHEMA,
        "name": meta["name"],
        "description": meta["description"],
    })
    ctx.add_json("mcp_config.json", {
        "mcpServers": {
            meta["mcp"]["server_name"]: {
                "command": "sh",
                "args": ["-c", mcp_launch_program(meta["name"]), "anet-mcp"],
            }
        }
    })
