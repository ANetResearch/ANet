"""Target trae: the anet bundle for Trae (ByteDance; Trae IDE and Trae CN).

Trae has no plugin manifest that a repository can provide, and no marketplace
that reads Git, an archive or an HTTPS catalogue. Its plugin marketplace is a
curated catalogue with no documented authoring or submission process. The
documented extension points are used instead: MCP servers (a JSON object
mcpServers), skills (<dir>/skills/<name>/SKILL.md) and commands
(<dir>/commands/<name>.md), at the user level in ~/.trae (Trae CN:
~/.trae-cn) and at the project level in <project>/.trae. The only variable that
Trae documents is ${workspaceFolder}, in MCP entries; there is no plugin root.

The bundle is therefore a directory that mirrors a configuration directory of
Trae, with an installer for each platform that copies it:

    install.sh              Linux and macOS: copies the parts into ~/.trae, ~/.trae-cn or
                            <project>/.trae, writes the settings, prints the MCP
                            configuration and the trae:// import link; reads no file of Trae
    install.cmd install.ps1 Windows: the same, with the options -Project, -Cn, -Identity,
                            -ClearIdentity, -Anet, -ClearAnet and -Remove
    mcp.json                the MCP entry of a project on Linux and macOS (the output of
                            install.sh --project without settings, kept for manual installation)
    skills/ commands/       the shared files; the commands are /anet and /anet-status
    anet/bin/               the runners and the MCP launchers of both platforms
    README.md README_CN.md LICENSE

The skills find the runner at <skill-dir>/../../anet/bin/anet-run.sh (or
anet-run.cmd), the same place in every scope; the commands name it as
<trae-dir>/anet/bin/anet-run.sh, where <trae-dir> is the configuration
directory that holds the command.

Start of the MCP server. Linux and macOS: the entry runs "/bin/sh <launcher>",
so a missing executable bit does not matter, and the launcher finds anet in
~/.local/bin although a desktop application does not inherit the PATH of a
login shell. A command of Trae must not contain spaces
(https://docs.trae.ai/ide/add-mcp-servers), so the launcher is the argument
and the command is /bin/sh. Windows: install.ps1 writes the absolute path of
anet-mcp.cmd as the command, with the extension, because it is not known
whether Trae looks a command up with PATHEXT. The tool host of Trae 2.3 is
written in Rust and starts MCP servers with rmcp 0.12 (TokioChildProcess);
the Rust standard library runs a .cmd file through cmd.exe /e:ON /v:OFF /d /c
with escaped arguments, and the entry has no arguments. A path with a space is
replaced by its 8.3 form, or refused. The shell of the agent on Windows is
PowerShell (TRAE_NATIVE_RUN_COMMAND_SHELL, else pwsh.exe, else
powershell.exe), and Trae blocks "cmd /c" in it, so the texts name the .cmd
runner as a path that PowerShell starts with &.

Settings: Trae has no per-plugin settings. The installers store identity and
anetPath in anet/settings (read by the runners for the shell commands of the
agent) and in the env of the MCP entry (read by the launchers), from the same
options, so that both places agree. The runners never read a file of Trae.

Not verified, because Trae is a graphical application with no command line or
headless mode: that Trae loads the skills and the commands, whether it accepts
a command file with front matter, the path of its global MCP file (the
installers therefore print the entry instead of writing it), the install link
of Trae CN (only the link of Trae is printed), and the start of a .cmd command
on Windows. Trae 2.3 also reads plugin directories with .trae-plugin/plugin.json,
whose format is not documented. The generated JSON, the skills, the installers
and the launchers are tested by tests/test_trae.py and
tests/test_windows_runner.py.

The site() hook publishes the bundle as an archive for download; Trae does not
install from it.

The target sources are in deploy/plugins/src/trae/.
"""

import re
from pathlib import Path

TITLE = "Trae"
HELP = "Trae and Trae CN of ByteDance; skills, command and MCP server installed by a script (Trae has no plugin package format)."
MARKERS = ["Trae", ".trae", "trae://", "workspaceFolder"]

_SRC = Path(__file__).resolve().parent.parent / "src" / "trae"


def _src(name):
    text = (_SRC / name).read_text(encoding="utf-8")
    return text[:-1] if text.endswith("\n") else text


_RUNNER = "<skill-dir>/../../anet/bin/anet-run.sh"
_COMMAND_RUNNER = "<trae-dir>/anet/bin/anet-run.sh"
_NAME_RE = re.compile(r"^[a-z0-9][a-z0-9-]{0,63}$")
# The launcher of a project entry: ${workspaceFolder} is the only variable of Trae.
PROJECT_LAUNCHER = "${workspaceFolder}/.trae/anet/bin/anet-mcp.sh"
COMMAND = "/bin/sh"

VARS = {
    "TOOL": "Trae",
    "RUNNER_FROM_SKILL": _RUNNER,
    "RUNNER_FROM_COMMAND": _COMMAND_RUNNER,
    # The runner reads the plugin's own settings file; the Trae configuration is off limits to the agent.
    "SETTINGS_STORE": True,
    "CONFIG_DIR": "mcp.json",
    "DOC_SLUG": "trae",
    "ARGUMENTS": {
        "en": "any text that follows the command name",
        "zh-CN": "命令之后的文字（如有）",
    },
    "RUNNER_NOTE": (
        "`<skill-dir>` is the directory that contains this `SKILL.md` file; use the absolute path under which Trae "
        "loaded the skill. The runner sits two levels above it, in `anet/bin`."
    ),
    "RUNNER_COMMAND_NOTE": (
        "`<trae-dir>` is the configuration directory of Trae that holds this command and the anet bundle: "
        "`~/.trae`, `~/.trae-cn` or the `.trae` directory of the project (on Windows `%USERPROFILE%\\.trae` or "
        "`%USERPROFILE%\\.trae-cn`)."
    ),
    "INSTALL_STEP": {
        "en": (
            "Install the bundle with its installer (see Installation and settings below): on Linux and macOS "
            "`sh install.sh` for the user level, `sh install.sh --cn` for Trae CN, or `sh install.sh --project <directory>` "
            "for one project; on Windows `install.cmd`, `install.cmd -Cn` or `install.cmd -Project <directory>`. "
            "The installer prints the MCP configuration; enter it in Trae. "
            "If you registered anet earlier with another tool, remove that entry first, so that Trae does not have two anet servers."
        ),
        "zh-CN": (
            "用安装脚本安装本插件包（见下文“安装与设置”）：在 Linux 和 macOS 上，用户级执行 `sh install.sh`，"
            "Trae CN 执行 `sh install.sh --cn`，单个项目执行 `sh install.sh --project <目录>`；在 Windows 上执行 "
            "`install.cmd`、`install.cmd -Cn` 或 `install.cmd -Project <目录>`。"
            "安装脚本会输出 MCP 配置，请在 Trae 中填入。"
            "如果此前用其他方式注册过 anet，请先删除该条目，以免 Trae 中出现两个 anet 服务。"
        ),
    },
    "SETTINGS_SECTION": {"en": _src("settings-section.md"), "zh-CN": _src("settings-section_CN.md")},
    "SETTINGS_READ_NOTE": {
        "en": (
            " The MCP server receives the two settings from the `env` of its entry in the MCP configuration of Trae; the "
            "skills and the commands run in a shell of the agent, where the runner reads them, and nothing else, from the "
            "file `anet/settings` that the installer wrote."
        ),
        "zh-CN": (
            "MCP 服务从 Trae 的 MCP 配置中其条目的 `env` 获得这两项设置；skill 与命令在智能体的 shell 中运行，"
            "此时运行脚本只从安装脚本写入的 `anet/settings` 文件中读取它们。"
        ),
    },
    "RUNNER_SETTINGS_COMMENT": _src("runner-settings-comment.txt"),
    "RUNNER_READ_SETTINGS": _src("runner-read-settings.sh"),
    "RUNNER_READ_SETTINGS_PS": _src("runner-read-settings.ps1"),
}


def validate(ctx):
    meta = ctx.meta
    if not _NAME_RE.match(meta["name"]):
        raise ValueError(f"name {meta['name']!r} is not usable as the name of a Trae skill directory")
    if not isinstance(meta["mcp"]["timeout_ms"], int) or meta["mcp"]["timeout_ms"] <= 0:
        raise ValueError("mcp.timeout_ms must be a positive integer")
    if re.search(r"\s", COMMAND) or re.search(r"\s", PROJECT_LAUNCHER.split("}")[0]):
        raise ValueError("Trae rejects an MCP command with spaces")


def _replace_once(text, old, new, rel):
    if text.count(old) != 1:
        raise ValueError(f"{rel}: expected exactly one occurrence of {old!r}; the shared source changed, update targets/trae.py")
    return text.replace(old, new)


_CONFIG_RULE_OLD = (
    "it holds the user's\n  credentials, and the plugin settings in it belong to the user. The runner reads the two\n"
    "  plugin settings itself."
)
_CONFIG_RULE_NEW = (
    "it can hold the credentials\n  of other MCP servers. The runner reads the plugin settings from the file `anet/settings`\n"
    "  of the bundle; the user changes them with the installer of the bundle."
)


def transform(ctx, rel, text, lang):
    if rel in ("skills/anet/SKILL.md", "skills/anet-setup/SKILL.md"):
        return _replace_once(text, _CONFIG_RULE_OLD, _CONFIG_RULE_NEW, rel)
    if rel == "README.md":
        text = _replace_once(
            text,
            "`bin/anet-run.sh` on Linux and macOS and `bin/anet-run.cmd` on Windows",
            "`anet/bin/anet-run.sh` on Linux and macOS and `anet/bin/anet-run.cmd` on Windows",
            rel,
        )
        return _replace_once(
            text,
            "The plugin itself writes only",
            "The bundle installer of this plugin writes the files listed above once, when the user runs it, "
            "and creates `.trae/mcp.json` in a project that has none. At run time the plugin itself writes only",
            rel,
        )
    if rel == "README_CN.md":
        text = _replace_once(
            text,
            "Linux 和 macOS 上为 `bin/anet-run.sh`，Windows 上为 `bin/anet-run.cmd`",
            "Linux 和 macOS 上为 `anet/bin/anet-run.sh`，Windows 上为 `anet/bin/anet-run.cmd`",
            rel,
        )
        return _replace_once(
            text,
            "插件本身只写入",
            "本插件的安装脚本仅在用户运行时写入上文所列的文件一次，并在尚无 `.trae/mcp.json` 的项目中创建该文件。插件在运行时只写入",
            rel,
        )
    return text


def mcp_entry(meta, launcher, settings=None):
    """The MCP entry of the server: /bin/sh runs the launcher. settings maps ANET_PLUGIN_* names to non-empty values."""
    env = {"RUN_MCP_TIMEOUT_MS": str(meta["mcp"]["timeout_ms"])}
    env.update(settings or {})
    return {"command": COMMAND, "args": [launcher], "env": env}


def build(ctx):
    meta = ctx.meta
    ctx.add_common(dest={"bin": "anet/bin"})
    extra = {"SERVER_NAME": meta["mcp"]["server_name"], "MCP_TIMEOUT_MS": str(meta["mcp"]["timeout_ms"])}
    ctx.add("install.sh", ctx.render_text((_SRC / "install.sh").read_text(encoding="utf-8"), extra=extra), executable=True)
    ctx.add("install.ps1", ctx.render_text((_SRC / "install.ps1").read_text(encoding="utf-8"), extra=extra))
    ctx.add("install.cmd", ctx.crlf((_SRC / "install.cmd").read_text(encoding="utf-8")))
    ctx.add_json("mcp.json", {"mcpServers": {meta["mcp"]["server_name"]: mcp_entry(meta, PROJECT_LAUNCHER)}})


def site(ctx, site):
    if not site.base.startswith("https://"):
        raise ValueError("the site base must be an https:// URL")
    meta = ctx.meta
    archive = site.archive(f"anet-trae-{meta['version']}.zip", wrapper="anet-trae", kind="zip")
    site.add_json("trae.json", {
        "name": meta["name"],
        "version": meta["version"],
        "description": meta["description"],
        "license": meta["license"],
        "archive": archive,
        "install": "Unpack the archive and run: sh anet-trae/install.sh [--cn | --project DIR] (Linux, macOS), or anet-trae\\install.cmd [-Cn | -Project DIR] (Windows)",
    })
