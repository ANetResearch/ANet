"""Target gemini-cli: the extension for Gemini CLI (@google/gemini-cli).

The sibling target antigravity builds the plugin for Google Antigravity from
the same shared files; the target-specific sources of both are in
deploy/plugins/src/antigravity/ (files whose name starts with gemini-).

Shape of the host (verified with Gemini CLI 0.64.0, by running it and by
reading its bundle and its documentation):

  * The manifest is gemini-extension.json at the extension root. `name` and
    `version` are required, and the name allows letters, digits and hyphens
    only. `gemini extensions validate <directory>` checks it.
  * Components are found by directory name: skills/<name>/SKILL.md,
    commands/*.toml (TOML only), hooks/hooks.json, agents/, policies/. The MCP
    servers are declared in the manifest.
  * ${extensionPath} is replaced by the extension directory in the manifest,
    in hooks.json and in every string of a skill, so a skill names the runner
    as ${extensionPath}/bin/anet-run.sh. A TOML command is not processed, so
    the command only asks the agent to activate the skill anet-status.
  * Settings: the manifest declares the two settings with an envVar. Gemini CLI
    prompts for them at installation, stores them in <extension>/.env and
    passes them to the MCP server; other variables of the environment are not
    passed. The runner reads the same variables from its environment and, as a
    fallback, from that .env file (src/antigravity/gemini-runner-read-settings.sh).
  * `extensions install` accepts a Git or GitHub URL of the form owner/repo
    (the manifest at the root of the repository or of a release archive) or a
    local path, not a subdirectory of a repository.

Launch on Windows and POSIX (read in the bundle of @google/gemini-cli 0.65.0):

  * A stdio server is spawned by the StdioClientTransport of the MCP
    TypeScript SDK with cross-spawn, shell:false (only in a trusted folder).
    ${extensionPath} and ${/} are replaced in the whole manifest.
  * The command is ${extensionPath}${/}bin${/}anet-mcp, without an extension.
    On Windows cross-spawn tries the path with each extension of PATHEXT (an
    extensionless file is not executable there), finds bin/anet-mcp.cmd and
    runs it through cmd.exe /d /s /c. On Linux and macOS bin/anet-mcp is run
    itself: a shell script that starts bin/anet-mcp.sh with sh.
  * The executable bit survives every installation: a local installation is
    copied with fs.promises.cp, a Git installation is a clone, and the release
    archives (tar.gz, built with mode 0755 for executables) are unpacked by
    node-tar with their modes.
  * The agent runs commands in PowerShell on Windows; the skills give the
    Windows runner bin/anet-run.cmd next to sh bin/anet-run.sh.

Output (dist/gemini-cli/ is the extension root):

    gemini-extension.json   manifest with mcpServers and settings
    commands/anet.toml      /anet, the @anet entry point
    commands/anet-status.toml   /anet-status
    bin/ skills/            the shared files; skills/anet-status is made from the shared command
    bin/anet-install.sh  the official installer as one plain command (common/bin/anet-install.sh)
    README.md README_CN.md LICENSE

Site (--site): the reproducible archives linux.anet.tar.gz, darwin.anet.tar.gz and
win32.anet.tar.gz (identical content) and SHA256SUMS. Attached to a GitHub release of a repository,
they make `gemini extensions install <owner>/<repo>` install the extension from
the release asset, whose name Gemini CLI matches by the platform prefix, so
that the repository itself needs no manifest at its root.
"""

import json
import re
from pathlib import Path

TITLE = "Gemini CLI"
HELP = "Gemini CLI extension (@google/gemini-cli); skills, a slash command, MCP server and settings."
SITE_PATH = "plugins/gemini-cli"
MARKERS = ["gemini-extension.json", "extensionPath", "gemini extensions"]

SRC = Path(__file__).resolve().parent.parent / "src" / "antigravity"
RUNNER_FROM_SKILL = "${extensionPath}/bin/anet-run.sh"
STATUS_SKILL = "anet-status"
_NAME_RE = re.compile(r"^[a-zA-Z0-9-]+$")
_SEMVER_RE = re.compile(r"^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$")
_ENV_RE = re.compile(r"^[a-zA-Z_][a-zA-Z0-9_]*$")


def _read(name):
    t = (SRC / name).read_text(encoding="utf-8")
    return t[:-1] if t.endswith("\n") else t


VARS = {
    "TOOL": "Gemini CLI",
    "RUNNER_FROM_SKILL": RUNNER_FROM_SKILL,
    "RUNNER_FROM_COMMAND": RUNNER_FROM_SKILL,
    "SETTINGS_STORE": False,  # the runner reads the .env of the extension itself, not a store of the host
    "DOC_SLUG": "gemini-cli",
    "SETTINGS_SECTION": {"en": _read("gemini-settings-section.md"), "zh-CN": _read("gemini-settings-section_CN.md")},
    "RUNNER_SETTINGS_COMMENT": _read("gemini-runner-settings-comment.txt"),
    "RUNNER_READ_SETTINGS": _read("gemini-runner-read-settings.sh"),
    "RUNNER_READ_SETTINGS_PS": _read("gemini-runner-read-settings.ps1"),
    "INSTALL_STEP": {
        "en": (
            "Install the extension from a local copy of the ANet repository: `git clone https://github.com/ANetResearch/ANet.git`, "
            "then `gemini extensions install ./ANet/deploy/plugins/dist/gemini-cli`. "
            "Gemini CLI asks whether to trust the directory and for the two settings below; leave the settings empty to use the defaults. "
            "If you added an anet MCP server to Gemini CLI yourself (`gemini mcp list` shows it), remove it with `gemini mcp remove anet` first, so that Gemini CLI does not have two anet servers."
        ),
        "zh-CN": (
            "从 ANet 代码仓库的本地副本安装扩展：`git clone https://github.com/ANetResearch/ANet.git`，"
            "然后执行 `gemini extensions install ./ANet/deploy/plugins/dist/gemini-cli`。"
            "Gemini CLI 会询问是否信任该目录，并询问下面的两项设置；留空即使用默认值。"
            "如果此前自行在 Gemini CLI 中添加过 anet MCP 服务（`gemini mcp list` 可见），请先执行 `gemini mcp remove anet` 移除，以免 Gemini CLI 中出现两个 anet 服务。"
        ),
    },
    "COMPONENTS_SAME_RUNNER": {
        "en": "the MCP server (`anet mcp`), the commands `/anet` and `/anet-status` and the skills",
        "zh-CN": "MCP 服务（`anet mcp`）、`/anet` 与 `/anet-status` 命令以及各 skill ",
    },
    "ARGUMENTS": "the part of the request that names something specific to check, if any.",
}


def validate(ctx):
    meta = ctx.meta
    if not _NAME_RE.match(meta["name"]):
        raise ValueError(f"extension name {meta['name']!r} is not valid for Gemini CLI (letters, digits and hyphens only)")
    if not _SEMVER_RE.match(meta["version"]):
        raise ValueError(f"version {meta['version']!r} is not a semantic version")
    for s in meta["settings"]:
        if not _ENV_RE.match(s["env"]):
            raise ValueError(f"environment variable {s['env']!r} cannot be written to the .env file of an extension")
    for field in (meta["description"], *(s["description"] + s["title"] for s in meta["settings"])):
        # The manifest is processed with the rules for $VAR and ${VAR} of Gemini CLI.
        if "$" in field:
            raise ValueError("the manifest text must not contain the character $")
    if "'''" in "".join(meta["description_i18n"].values()):
        raise ValueError("the description would end the TOML string of the command")


def _replace_once(text, old, new, where):
    if text.count(old) != 1:
        raise ValueError(f"gemini-cli: {where}: expected one occurrence of {old!r}, found {text.count(old)}; update targets/gemini-cli.py")
    return text.replace(old, new)




def transform(ctx, rel, text, lang):
    """Gemini CLI-specific wording that the variables do not cover."""
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
            "user has to correct or clear it. Do not change the setting yourself, and do not install\n  another copy.",
            "user has to correct or clear it (`gemini extensions config anet`). Do not change the setting yourself, do not open\n  the `.env` file of the extension, and do not install another copy.",
            rel,
        )
        text = _replace_once(
            text,
            "they hold the identity keys and the control token.\n",
            "they hold the identity keys and the control token.\n"
            "- Never read or edit the Gemini CLI configuration (`~/.gemini`, including its `settings.json`, `.env` and\n"
            "  credential files): it holds the user's credentials. The runner reads the two extension settings itself.\n"
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
            "| Skill | `anet-setup` |",
            "| Skill | `anet-status` | Shows the state of the node, as the guide of the installed anet describes. The command `/anet-status` activates it. |\n| Skill | `anet-setup` |",
            rel,
        )
    elif rel == "README_CN.md":
        text = _replace_once(
            text,
            "| Skill | `anet-setup` |",
            "| Skill | `anet-status` | 按已安装 anet 的指南显示节点状态。命令 `/anet-status` 会激活该 skill。 |\n| Skill | `anet-setup` |",
            rel,
        )
    return text


def _status_skill(ctx):
    description, body = ctx.command("anet-status")
    description = description.rstrip(".") + ". Use when the user asks for the state of the anet node or its balance."
    return "---\nname: " + STATUS_SKILL + "\ndescription: " + json.dumps(description, ensure_ascii=False) + "\n---\n\n" + body


def _toml(description, prompt):
    if "'''" in prompt:
        raise ValueError("the prompt would end the TOML string")
    return f"description = {json.dumps(description, ensure_ascii=False)}\nprompt = '''\n{prompt}\n'''\n"


def status_command_toml(description):
    """commands/anet-status.toml: the command delegates to the skill, which holds the runner path."""
    return _toml(description, (
        "Activate the `anet-status` skill with the `activate_skill` tool and follow its instructions to report "
        "the state of the local anet node, as the guide of the installed anet describes. Optional focus from the user: {{args}}"
    ))


def anet_command_toml(description):
    """commands/anet.toml: the @anet entry point. A TOML command is not expanded, so the runner comes from the skill."""
    return _toml(description, (
        "Carry out the following request for this machine's AgentNetwork (anet) node. First read the tool `guide` of "
        "the `anet` MCP server (or activate the `anet` skill with the `activate_skill` tool and run `<runner> guide` "
        "with the runner that it names) and follow its section on @anet requests.\n\n"
        "Request: {{args}}\n\n"
        "If the request is empty, show the tasks that other agents sent to this node and the state of the node, as "
        "the guide describes."
    ))


def build(ctx):
    meta = ctx.meta
    ctx.add_common(groups=("bin", "skills", "docs"))
    ctx.add(f"skills/{STATUS_SKILL}/SKILL.md", _status_skill(ctx))
    description, _ = ctx.command("anet-status")
    ctx.add(f"commands/{STATUS_SKILL}.toml", status_command_toml(description))
    description, _ = ctx.command("anet")
    ctx.add("commands/anet.toml", anet_command_toml(description))

    ctx.add_json("gemini-extension.json", {
        "name": meta["name"],
        "version": meta["version"],
        "description": meta["description"],
        "mcpServers": {
            meta["mcp"]["server_name"]: {
                # bin/anet-mcp on Linux and macOS, bin/anet-mcp.cmd on Windows (see the module docstring).
                "command": "${extensionPath}${/}bin${/}anet-mcp",
                "args": [],
                "cwd": "${extensionPath}",
                "timeout": meta["mcp"]["timeout_ms"],
            }
        },
        "settings": [
            {"name": s["title"], "description": s["description"], "envVar": s["env"], "sensitive": False}
            for s in meta["settings"]
        ],
    })


def site(ctx, site):
    """The release assets: Gemini CLI picks the asset whose name starts with <platform>. ."""
    name = ctx.meta["name"]
    names = [f"{platform}.{name}.tar.gz" for platform in ("linux", "darwin", "win32")]
    sums = [site.archive(n, kind="tar.gz")["sha256"] for n in names]
    if len(set(sums)) != 1:
        raise ValueError("the release assets must be identical")
    site.add("SHA256SUMS", "".join(f"{s}  {n}\n" for s, n in zip(sums, names)))
