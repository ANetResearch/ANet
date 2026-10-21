"""Target opencode: the plugin for OpenCode (opencode-ai), the open-source coding agent.

OpenCode has no plugin bundle and no marketplace. A plugin is a JavaScript
module (an npm package or a directory) that is named in the "plugin" list of
opencode.json, optionally with options: ["<package>", {"anetPath": ...}]. Its
"config" hook receives the merged configuration and may add to it, which is how
this plugin registers what other hosts read from a manifest:

    mcp.anet            {type: local, command: [<root>/bin/anet-mcp], timeout}
    skills.paths        <root>/skills   (anet, anet-setup)
    command.anet        the @anet entry point (commands/anet.md)
    command.anet-status the status command; both with the plugin root written in

The plugin directory is an npm package. OpenCode installs a package by name from
npm (`opencode plugin <name>`) and loads a directory by absolute path, so one
directory of the ANet repository serves both; no repository of its own is needed.

Output (dist/opencode/ is the package root):

    package.json            name, version, main and exports["./server"], engines.opencode
    server.js lib.js        the plugin (src/opencode/); only server.js is an entry point
    bin/ skills/ commands/  the shared files
    README.md README_CN.md LICENSE

There is no --site output: OpenCode installs from npm or from a directory.

Settings are the options of the plugin entry. The plugin passes them to the MCP
server as ANET_PLUGIN_BIN and ANET_PLUGIN_IDENTITY and, through the "shell.env"
hook, to every shell command of the agent together with ANET_PLUGIN_ROOT, which
the skills and the command use to find bin/anet-run.sh (OpenCode has no
plugin-root variable). The runner therefore reads its settings from the
environment, as for any host without a settings file.

Launch on Windows and POSIX (read in OpenCode 1.19.3, the strings of its
Windows binary, which is built with Bun):

  * A local MCP server is spawned by the StdioClientTransport of the MCP
    TypeScript SDK with cross-spawn, shell:false; command[0] is the program and
    the rest are its arguments. A plugin that adds the entry in its config hook
    gets no variable expansion, so lib.js writes the absolute path.
  * The program is <root>/bin/anet-mcp without an extension. On Windows
    cross-spawn tries the path with each extension of PATHEXT (an extensionless
    file is not executable there), finds bin/anet-mcp.cmd and runs it through
    cmd.exe /d /s /c with verbatim arguments. On Linux and macOS bin/anet-mcp is
    run itself: a shell script that starts bin/anet-mcp.sh with sh.
  * The executable bit survives the installation: npm tarballs carry the mode,
    and the installer of OpenCode (arborist and pacote) keeps the executable
    bit; a directory is used in place.
  * The bash tool of OpenCode uses PowerShell, Git Bash or cmd on Windows. The
    shell.env hook sets ANET_PLUGIN_ROOT in every one of them; the skills tell
    the agent how to write it in each shell.

The target sources are in deploy/plugins/src/opencode/.
"""

import json
from pathlib import Path

TITLE = "OpenCode"
HELP = "OpenCode (opencode-ai); npm package or directory with a JavaScript plugin that registers the MCP server, the skills and the command."
MARKERS = ["ANET_PLUGIN_ROOT", "opencode.json"]

# The name under which the package is to be published to npm. The scope has to
# belong to the project; until the package is published, OpenCode installs the
# directory by its absolute path.
PACKAGE_NAME = "@agentnetwork/opencode-anet"
# The oldest OpenCode that the plugin was run with. The loader checks engines.opencode.
MIN_OPENCODE = "1.19.3"

SRC = Path(__file__).resolve().parent.parent / "src" / "opencode"

VARS = {
    "TOOL": "OpenCode",
    "RUNNER_FROM_SKILL": "${ANET_PLUGIN_ROOT}/bin/anet-run.sh",
    "RUNNER_FROM_COMMAND": "${ANET_PLUGIN_ROOT}/bin/anet-run.sh",
    # The settings are not read from a file by the runner, but the agent must be told
    # that the OpenCode configuration is off limits (it holds the model credentials).
    "SETTINGS_STORE": True,
    "CONFIG_DIR": "~/.config/opencode`, `opencode.json",
    "DOC_SLUG": "opencode",
    "INSTALL_STEP": {
        "en": "Add the plugin to OpenCode and restart it; see Installation and settings below.",
        "zh-CN": "将插件添加到 OpenCode 并重启；见下文“安装与设置”。",
    },
    "RUNNER_NOTE": (
        "`${ANET_PLUGIN_ROOT}` is the environment variable `ANET_PLUGIN_ROOT`, which the plugin sets for the shell "
        "commands of the agent: in PowerShell write it as `$env:ANET_PLUGIN_ROOT`, in cmd as `%ANET_PLUGIN_ROOT%`. "
        "If it is empty, the plugin is not loaded: stop and tell the user."
    ),
    "SETTINGS_READ_NOTE": {
        "en": " Outside the MCP server, the script reads these two settings, and nothing else, from its environment, which the plugin sets for the shell commands of the agent.",
        "zh-CN": "在 MCP 服务之外，该脚本只从自身环境变量中读取这两项设置，由插件为智能体的 shell 命令设置。",
    },
}


def _source(name):
    return (SRC / name).read_text(encoding="utf-8")


def _text(name):
    t = _source(name)
    return t[:-1] if t.endswith("\n") else t


def _sub(text, old, new, rel):
    if old not in text:
        raise ValueError(f"opencode: the text {old!r} is not in {rel}; the shared source changed, update targets/opencode.py")
    return text.replace(old, new)


def transform(ctx, rel, text, lang):
    """Wording that depends on how OpenCode loads configuration."""
    if rel == "skills/anet-setup/SKILL.md":
        text = _sub(
            text,
            "tell the user to start a\nnew OpenCode session, because the MCP server of a plugin starts with the session.",
            "tell the user to\nrestart OpenCode, because OpenCode reads its configuration and starts the MCP servers only\nwhen it starts.",
            rel,
        )
    elif rel == "README.md":
        text = _sub(text, "3. Start a new session, so that the MCP server starts, and", "3. Restart OpenCode, so that the MCP server starts, and", rel)
        text = _sub(text, "- An anet release that provides `anet guide`.", f"- OpenCode {MIN_OPENCODE} or later.\n- An anet release that provides `anet guide`.", rel)
        text = _sub(text, "`anet-setup` skill, then start a new session.", "`anet-setup` skill, then restart OpenCode.", rel)
    elif rel == "README_CN.md":
        text = _sub(text, "3. 新建一个会话，使 MCP 服务启动，", "3. 重启 OpenCode，使 MCP 服务启动，", rel)
        text = _sub(text, "- 提供 `anet guide` 命令的 anet 版本。", f"- OpenCode {MIN_OPENCODE} 或更高版本。\n- 提供 `anet guide` 命令的 anet 版本。", rel)
        text = _sub(text, "skill，然后新建会话。", "skill，然后重启 OpenCode。", rel)
    return text


def build(ctx):
    meta = ctx.meta
    # Files of src/opencode/ that supply template variables (see README.md, "Template variables").
    ctx.vars["RUNNER_SETTINGS_COMMENT"] = _text("runner-settings-comment.txt")
    read = ctx.common_bytes("runner-read-settings-env.sh").decode("utf-8")
    ctx.vars["RUNNER_READ_SETTINGS"] = read[:-1] if read.endswith("\n") else read
    ctx.vars["SETTINGS_SECTION"] = {"en": _text("settings-section.md"), "zh-CN": _text("settings-section_CN.md")}

    ctx.add_common()

    values = {"__SERVER_NAME__": meta["mcp"]["server_name"], "__TIMEOUT_MS__": str(int(meta["mcp"]["timeout_ms"]))}
    for name in ("server.js", "lib.js"):
        text = _source(name)
        for key, value in values.items():
            text = text.replace(key, json.dumps(value)[1:-1] if key == "__SERVER_NAME__" else value)
        if "__" in text.replace("__proto__", ""):
            raise ValueError(f"opencode: unresolved placeholder in src/opencode/{name}")
        ctx.add(name, text)

    ctx.add_json("package.json", {
        "name": PACKAGE_NAME,
        "version": meta["version"],
        "description": meta["description"],
        "keywords": meta["keywords"] + ["opencode", "opencode-plugin"],
        "homepage": meta["homepage"],
        "repository": {"type": "git", "url": "git+" + meta["repository"] + ".git", "directory": ctx.dist_path + "/" + ctx.key},
        "license": meta["license"],
        "author": {"name": meta["author"]["name"], "url": meta["author"]["url"]},
        "type": "module",
        "main": "./server.js",
        "exports": {"./server": "./server.js"},
        "engines": {"opencode": ">=" + MIN_OPENCODE},
        "files": ["server.js", "lib.js", "bin", "skills", "commands", "README.md", "README_CN.md", "LICENSE"],
        "publishConfig": {"access": "public"},
    })
