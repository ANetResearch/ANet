"""Target codex: the plugin for Codex, the coding agent of OpenAI (Codex CLI, desktop app).

Shape of the host (verified with Codex CLI 0.163.0; see the page of the anet documentation):

  * The manifest is .codex-plugin/plugin.json. The portable manifest of the
    Agent Plugins specification (plugin.json and mcp.json at the root) is not
    used, because it cannot forward environment variables to the MCP server and
    the runner needs ANET_PLUGIN_BIN and ANET_PLUGIN_IDENTITY.
  * .mcp.json is read without any variable expansion; a relative "cwd" is
    joined to the installed plugin root (codex-rs/codex-mcp/src/plugin_config.rs).
    The server is therefore started as "./bin/anet-mcp" with the working
    directory ".". `env_vars` forwards the two settings from the Codex process;
    on Windows the default environment of an MCP server also holds PATH,
    PATHEXT, SYSTEMROOT, USERPROFILE and LOCALAPPDATA
    (codex-rs/protocol/src/shell_environment.rs, WINDOWS_CORE_ENV_VARS).
  * Launch on Windows (read in the source of Codex, openai/codex at d138398):
    codex-rs/rmcp-client/src/program_resolver.rs resolves the program with
    which::which_in against the PATH of the server and the working directory.
    A path with a separator is joined to the working directory and tried with
    each extension of PATHEXT; a file without an extension counts only when it
    is a Windows executable, so "./bin/anet-mcp" becomes bin/anet-mcp.cmd. The
    standard library of Rust then runs the .cmd file through cmd.exe with its
    own argument escaping (stdio_server_launcher.rs). On Linux and macOS the
    relative program is run after the change into the working directory:
    bin/anet-mcp, a shell script that starts bin/anet-mcp.sh with sh.
  * The executable bit survives the installation: marketplaces are fetched
    with git clone, the copy into the plugin cache uses std::fs::copy, which
    keeps the permissions (codex-rs/core-plugins/src/store.rs), and remote
    bundles are tar archives unpacked with their modes.
  * The agent runs commands in PowerShell on Windows
    (codex-rs/shell-command/src/shell_detect.rs), so the skills give the
    Windows runner bin/anet-run.cmd next to sh bin/anet-run.sh.
  * There are no plugin settings and no slash commands. The status command is
    shipped as the skill anet-status ($anet:anet-status), not as commands/,
    because Codex would convert a command into a skill named
    source-command-anet-status. The @anet entry point is the skill anet
    itself ($anet:anet followed by the request).
  * A skill cannot reference the plugin root by a variable. The skills name
    <skill-dir>/../../bin/anet-run.sh, and <skill-dir> is the directory that
    holds the SKILL.md file, whose path Codex lists with the skill.

Output (dist/codex/ is the plugin root):

    .codex-plugin/plugin.json   manifest with the interface block
    .mcp.json                   the MCP server, started through bin/anet-mcp.sh
    bin/ skills/                the shared files; skills/anet-status is made from the shared command
    assets/icon.png             logo and composer icon
    README.md README_CN.md LICENSE

Root file (dist/ is the root of the optional mirror repository):

    .agents/plugins/marketplace.json   the catalogue "agentnetwork", source ./codex

Codex reads a catalogue only from the root of a Git repository or a local
directory. For the ANet repository itself the same catalogue, with the source
./deploy/plugins/dist/codex, has to sit at .agents/plugins/marketplace.json in
the repository root; it is kept as src/codex/repo-root-marketplace.json and
checked by tests/test_codex.py. Codex cannot install from an HTTPS catalogue, so
the module has no site().
"""

import json
import re
from pathlib import Path

TITLE = "Codex"
HELP = "Codex of OpenAI (command-line program and desktop application); plugin with skills and an MCP server, settings through environment variables."
MARKERS = [".codex-plugin", "codex plugin"]

SRC = Path(__file__).resolve().parent.parent / "src" / "codex"

MARKETPLACE_NAME = "agentnetwork"
MARKETPLACE_TITLE = "AgentNetwork"
CATEGORY = "Developer Tools"
BRAND_COLOR = "#E93024"  # the logo red of docs/media/brand/palette.json
SHORT_DESCRIPTION = "Hand work to other AI agents"
CAPABILITIES = ["Find agents", "Delegate tasks", "Answer tasks", "Pay within limits"]
DEFAULT_PROMPTS = [
    "What can anet do on this machine?",
    "Show the state of my anet node.",
    "Find an agent on anet that can summarize text and send it a task.",
]
SKILL_DIR = "<skill-dir>"
RUNNER_FROM_SKILL = SKILL_DIR + "/../../bin/anet-run.sh"
STATUS_SKILL = "anet-status"


def _read(name):
    t = (SRC / name).read_text(encoding="utf-8")
    return t[:-1] if t.endswith("\n") else t


VARS = {
    "TOOL": "Codex",
    "RUNNER_FROM_SKILL": RUNNER_FROM_SKILL,
    "RUNNER_FROM_COMMAND": RUNNER_FROM_SKILL,
    "HAS_COMMAND": False,  # Codex has no slash commands; the status command is a skill
    "SETTINGS_STORE": False,
    "DOC_SLUG": "codex",
    "SETTINGS_SECTION": {"en": _read("settings-section.md"), "zh-CN": _read("settings-section_CN.md")},
    "INSTALL_STEP": {
        "en": "Add the marketplace and install the plugin: `codex plugin marketplace add ANetResearch/ANet`, then `codex plugin add anet@agentnetwork`. Codex 0.163 or later is required.",
        "zh-CN": "添加 marketplace 并安装插件：`codex plugin marketplace add ANetResearch/ANet`，然后 `codex plugin add anet@agentnetwork`。需要 Codex 0.163 或更高版本。",
    },
    "COMPONENTS_SAME_RUNNER": {
        "en": "the MCP server (`anet mcp`) and the skills `anet`, `anet-setup` and `anet-status`",
        "zh-CN": "MCP 服务（`anet mcp`）与 `anet`、`anet-setup`、`anet-status` skill ",
    },
    "RUNNER_NOTE": (
        "`" + SKILL_DIR + "` is the absolute path of the directory that contains this SKILL.md file; the list of skills "
        "gives the path of the file as a skill root and a relative path."
    ),
    "RUNNER_COMMAND_NOTE": (
        "`" + SKILL_DIR + "` is the absolute path of the directory that contains this SKILL.md file; the list of skills "
        "gives the path of the file as a skill root and a relative path."
    ),
    "ARGUMENTS": "the part of the request that names something specific to check, if any.",
}

_NAME_RE = re.compile(r"^[a-z0-9][a-z0-9.-]*[a-z0-9]$")


def _replace_once(text, old, new, where):
    """Replace old by new; fail when the shared text no longer contains old exactly once."""
    if text.count(old) != 1:
        raise ValueError(f"codex: {where}: expected one occurrence of {old!r}, found {text.count(old)}; update targets/codex.py")
    return text.replace(old, new)


def catalogue(source):
    """The Codex marketplace catalogue for a plugin source path relative to the marketplace root."""
    return {
        "name": MARKETPLACE_NAME,
        "interface": {"displayName": MARKETPLACE_TITLE},
        "plugins": [{
            "name": "anet",
            "source": source,
            "policy": {"installation": "AVAILABLE", "authentication": "ON_INSTALL"},
            "category": CATEGORY,
        }],
    }


def validate(ctx):
    meta = ctx.meta
    name = meta["name"]
    if not _NAME_RE.match(name) or len(name) > 64 or ".." in name or "--" in name:
        raise ValueError(f"plugin name {name!r} is not a valid Codex plugin name")
    if not meta["version"]:
        raise ValueError("the plugin version is empty: Codex installs a plugin into a directory named after it")
    display = meta["display_name"]["en"]
    if len(display) > 30 or len(SHORT_DESCRIPTION) > 30:
        raise ValueError("interface.displayName and interface.shortDescription are limited to 30 characters")
    if len(DEFAULT_PROMPTS) > 3 or any(len(p) > 128 for p in DEFAULT_PROMPTS):
        raise ValueError("interface.defaultPrompt allows three prompts of at most 128 characters")
    if len(CAPABILITIES) > 20 or any(len(c) > 120 for c in CAPABILITIES):
        raise ValueError("interface.capabilities allows 20 labels of at most 120 characters")
    if len(meta["description"]) > 4000:
        raise ValueError("interface.longDescription is limited to 4000 characters")
    if not re.fullmatch(r"#[0-9A-Fa-f]{6}", BRAND_COLOR):
        raise ValueError("brandColor must be #RRGGBB")


def transform(ctx, rel, text, lang):
    """Codex-specific wording that the variables do not cover."""
    if rel == "skills/anet-setup/SKILL.md":
        text = _replace_once(text, "**anet executable** (`anetPath`)", "**anet executable** (the variable `ANET_PLUGIN_BIN`)", rel)
        text = _replace_once(text, "**anet identity** (`identity`)", "**anet identity** (the variable `ANET_PLUGIN_IDENTITY`)", rel)
        text = _replace_once(
            text,
            "the plugin setting **anet executable** names a file",
            "the setting **anet executable** (`ANET_PLUGIN_BIN`) names a file",
            rel,
        )
    elif rel == "README.md":
        text = _replace_once(
            text,
            "ask the agent to show the state of the anet node.",
            "run the `anet-status` skill by entering `$anet:anet-status`, or ask the agent to show the state of the anet node.",
            rel,
        )
        text = _replace_once(
            text,
            "| Skill | `anet-setup` |",
            "| Skill | `anet-status` | Shows the state of the node, as the guide of the installed anet describes. Enter `$anet:anet-status` or ask for the state of the node. |\n| Skill | `anet-setup` |",
            rel,
        )
        text = _replace_once(
            text,
            "4. Make a request that begins with `@anet`,",
            "4. Make a request that begins with `@anet` (or `$anet:anet` followed by the request),",
            rel,
        )
    elif rel == "README_CN.md":
        text = _replace_once(
            text,
            "然后让智能体显示 anet 节点的状态。",
            "然后运行 `anet-status` skill（输入 `$anet:anet-status`），或让智能体显示 anet 节点的状态。",
            rel,
        )
        text = _replace_once(
            text,
            "| Skill | `anet-setup` |",
            "| Skill | `anet-status` | 按已安装 anet 的指南显示节点状态。输入 `$anet:anet-status`，或直接询问节点状态。 |\n| Skill | `anet-setup` |",
            rel,
        )
        text = _replace_once(
            text,
            "4. 输入以 `@anet` 开头的请求，",
            "4. 输入以 `@anet` 开头的请求（或输入 `$anet:anet` 后接请求），",
            rel,
        )
    return text


def _status_skill(ctx):
    description, body = ctx.command("anet-status")
    description = description.rstrip(".") + ". Use when the user asks for the state of the anet node or its balance."
    return "---\nname: " + STATUS_SKILL + "\ndescription: " + json.dumps(description, ensure_ascii=False) + "\n---\n\n" + body


def build(ctx):
    meta = ctx.meta
    ctx.add_common(groups=("bin", "skills", "docs", "icon"))
    ctx.add(f"skills/{STATUS_SKILL}/SKILL.md", _status_skill(ctx))

    ctx.add_json(".codex-plugin/plugin.json", {
        "name": meta["name"],
        "version": meta["version"],
        "description": meta["description"],
        "author": meta["author"],
        "homepage": meta["homepage"],
        "repository": meta["repository"],
        "license": meta["license"],
        "keywords": meta["keywords"],
        "skills": "./skills/",
        "mcpServers": "./.mcp.json",
        "interface": {
            "displayName": meta["display_name"]["en"],
            "shortDescription": SHORT_DESCRIPTION,
            "longDescription": meta["description"],
            "developerName": meta["author"]["name"],
            "category": CATEGORY,
            "capabilities": CAPABILITIES,
            "websiteURL": meta["author"]["url"],
            "defaultPrompt": DEFAULT_PROMPTS,
            "brandColor": BRAND_COLOR,
            "composerIcon": "./assets/icon.png",
            "logo": "./assets/icon.png",
        },
    })
    # No variable is expanded in this file. cwd "." is the plugin root; the two
    # settings reach the server only through env_vars. "./bin/anet-mcp" is
    # bin/anet-mcp on Linux and macOS and bin/anet-mcp.cmd on Windows.
    ctx.add_json(".mcp.json", {
        "mcpServers": {
            meta["mcp"]["server_name"]: {
                "command": "./bin/anet-mcp",
                "args": [],
                "cwd": ".",
                "env_vars": [s["env"] for s in meta["settings"]],
                "startup_timeout_sec": 30,
                "tool_timeout_sec": meta["mcp"]["timeout_ms"] // 1000,
            }
        }
    })
    ctx.add_root_json(".agents/plugins/marketplace.json", catalogue(f"./{ctx.key}"))
