"""Target trae: the anet bundle for Trae (ByteDance; Trae IDE and Trae CN).

Trae has no plugin manifest that a repository can provide, and no marketplace
that reads Git, an archive or an HTTPS catalogue. Its plugin marketplace is a
curated catalogue with no documented authoring or submission process. The
documented extension points are used instead: MCP servers (a JSON object
mcpServers), skills (<dir>/skills/<name>/SKILL.md) and commands
(<dir>/commands/<name>.md), at the user level in ~/.trae (Trae CN:
~/.trae-cn) and at the project level in <project>/.trae. The only variable that
Trae expands is ${workspaceFolder}, in MCP entries; there is no plugin root.

The bundle is therefore a directory that mirrors a configuration directory of
Trae, with an installer that copies it:

    install.sh              copies the parts into ~/.trae, ~/.trae-cn or <project>/.trae,
                            writes the settings, prints the MCP configuration and the
                            trae:// import link; it reads no file of Trae
    mcp.json                the MCP entry of a project (the output of install.sh --project
                            without settings, kept as a file for manual installation)
    skills/ commands/       the shared files
    anet/bin/               anet-run.sh and anet-mcp.sh (the runner and the MCP launcher)
    README.md README_CN.md LICENSE

The skills find the runner at <skill-dir>/../../anet/bin/anet-run.sh, which is
the same place in every scope. The MCP entry runs "/bin/sh <launcher>", so a
missing executable bit does not matter, and the launcher finds anet in
~/.local/bin although a desktop application does not inherit the PATH of a
login shell. A command of Trae must not contain spaces, so the launcher is the
argument and the command is /bin/sh.

Settings: Trae has no per-plugin settings. install.sh stores identity and
anetPath in anet/settings (read by the runner for the shell commands of the
agent) and in the env of the MCP entry (read by the launcher), from the same
options, so that both places agree. The runner never reads a file of Trae.

Not verified, because Trae is a graphical application with no command line or
headless mode: that Trae loads the skills and the command, whether it accepts a
command file with front matter, the path of its global MCP file (the installer
therefore prints the entry instead of writing it) and the install link of Trae
CN (only the link of Trae is printed). The generated JSON, the skills, the
installer and the launcher are tested by tests/test_trae.py.

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
_NAME_RE = re.compile(r"^[a-z0-9][a-z0-9-]{0,63}$")
# The launcher of a project entry: ${workspaceFolder} is the only variable of Trae.
PROJECT_LAUNCHER = "${workspaceFolder}/.trae/anet/bin/anet-mcp.sh"
COMMAND = "/bin/sh"

VARS = {
    "TOOL": "Trae",
    "RUNNER_FROM_SKILL": _RUNNER,
    "RUNNER_FROM_COMMAND": _RUNNER,
    # The runner reads the plugin's own settings file; the Trae configuration is off limits to the agent.
    "SETTINGS_STORE": True,
    "CONFIG_DIR": "mcp.json",
    "DOC_SLUG": "trae",
    "ARGUMENTS": {
        "en": "any text that follows the command name",
        "zh-CN": "命令之后的文字（如有）",
    },
    "RESOLVE_INVOCATION": {
        "en": (
            "`<skill-dir>` is the directory of the `anet-setup` skill of this bundle, which Trae loaded from "
            "`~/.trae/skills`, `~/.trae-cn/skills` or `.trae/skills`; the runner is `<skill-dir>/../../anet/bin/anet-run.sh`. "
            "`sh \"<skill-dir>/../../anet/bin/anet-run.sh\" --resolve` prints the executable, the version, the identity and a `runner` line."
        ),
        "zh-CN": (
            "`<skill-dir>` 是本插件包中 `anet-setup` skill 所在的目录，Trae 从 `~/.trae/skills`、`~/.trae-cn/skills` 或 "
            "`.trae/skills` 加载它；运行脚本为 `<skill-dir>/../../anet/bin/anet-run.sh`。"
            "`sh \"<skill-dir>/../../anet/bin/anet-run.sh\" --resolve` 会输出可执行文件、版本、身份和一行 `runner`。"
        ),
    },
    "INSTALL_STEP": {
        "en": (
            "Install the bundle with the script `install.sh` (see Installation and settings below): "
            "`sh install.sh` for the user level, `sh install.sh --cn` for Trae CN, or `sh install.sh --project <directory>` "
            "for one project. The script prints the MCP configuration; enter it in Trae. "
            "If you registered anet earlier with another tool, remove that entry first, so that Trae does not have two anet servers."
        ),
        "zh-CN": (
            "用脚本 `install.sh` 安装本插件包（见下文“安装与设置”）："
            "用户级执行 `sh install.sh`，Trae CN 执行 `sh install.sh --cn`，单个项目执行 `sh install.sh --project <目录>`。"
            "脚本会输出 MCP 配置，请在 Trae 中填入。"
            "如果此前用其他方式注册过 anet，请先删除该条目，以免 Trae 中出现两个 anet 服务。"
        ),
    },
    "SETTINGS_SECTION": {"en": _src("settings-section.md"), "zh-CN": _src("settings-section_CN.md")},
    "SETTINGS_READ_NOTE": {
        "en": (
            " The MCP server receives the two settings from the `env` of its entry in `mcp.json`; the setup skill and the status "
            "command run in a shell of the agent, where the script reads them, and nothing else, from the file `anet/settings` "
            "that `install.sh` wrote."
        ),
        "zh-CN": (
            "MCP 服务从 `mcp.json` 中其条目的 `env` 获得这两项设置；安装 skill 与状态命令在智能体的 shell 中运行，"
            "此时脚本只从 `install.sh` 写入的 `anet/settings` 文件中读取它们。"
        ),
    },
    "RUNNER_SETTINGS_COMMENT": _src("runner-settings-comment.txt"),
    "RUNNER_READ_SETTINGS": _src("runner-read-settings.sh"),
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


_SKILL_DIR_NOTE = {
    "en": (
        "In the command above, `<skill-dir>` is the directory that contains this `SKILL.md` file; "
        "use the absolute path under which Trae loaded the skill. The runner sits two levels above it, in `anet/bin`."
    ),
    "zh-CN": (
        "上面的命令中，`<skill-dir>` 是本 `SKILL.md` 文件所在的目录；请使用 Trae 加载该 skill 时的绝对路径。"
        "运行脚本位于它上两级的 `anet/bin` 中。"
    ),
}


def transform(ctx, rel, text, lang):
    if rel == "skills/anet/SKILL.md":
        # The tail of the guide (the part that is not generated from guide.go).
        return text.rstrip("\n") + "\n\n" + _SKILL_DIR_NOTE[lang] + "\n"
    if rel == "skills/anet-setup/SKILL.md":
        text = _replace_once(
            text,
            "The `runner` line of the output is the command to use;",
            "`<skill-dir>` is the directory that contains this `SKILL.md` file; use the absolute path under which "
            "Trae loaded the skill. The runner sits two levels above it, in `anet/bin`.\n\n"
            "The `runner` line of the output is the command to use;",
            rel,
        )
        return _replace_once(
            text,
            "it holds the user's model credentials,\nand the plugin settings in it belong to the user. The runner reads the two plugin settings itself.",
            "it can hold the credentials of other\nMCP servers. The plugin settings are in the file `anet/settings` of this bundle, which the runner reads itself;\n"
            "the user changes them with `install.sh`.",
            rel,
        )
    if rel == "README.md":
        return _replace_once(
            text,
            "The plugin writes no files.",
            "At run time the plugin writes no files; `install.sh` writes the files listed above once, when the user runs it, "
            "and creates `.trae/mcp.json` in a project that has none.",
            rel,
        )
    if rel == "README_CN.md":
        return _replace_once(
            text,
            "插件本身不写文件。",
            "插件在运行时不写文件；`install.sh` 仅在用户运行时写入上文所列的文件一次，并在尚无 `.trae/mcp.json` 的项目中创建该文件。",
            rel,
        )
    if rel == "commands/anet-status.md":
        text = _replace_once(
            text,
            "The plugin settings select this executable and identity (resolved when the command started):",
            "The runner of this bundle, `anet-run.sh`, applies the plugin settings and selects the executable and the identity of the node.",
            rel,
        )
        text = _replace_once(text, "the `runner` command shown above", "the runner (`sh \"<skill-dir>/../../anet/bin/anet-run.sh\"`)", rel)
        text = _replace_once(text, ", for example `sh <path>/anet-run.sh status`,", ",", rel)
        return text
    return text


def mcp_entry(meta, launcher, settings=None):
    """The MCP entry of the server: /bin/sh runs the launcher. settings maps ANET_PLUGIN_* names to non-empty values."""
    env = {"RUN_MCP_TIMEOUT_MS": str(meta["mcp"]["timeout_ms"])}
    env.update(settings or {})
    return {"command": COMMAND, "args": [launcher], "env": env}


def build(ctx):
    meta = ctx.meta
    ctx.add_common(dest={"bin": "anet/bin"})
    ctx.add("install.sh", ctx.render_text(
        (_SRC / "install.sh").read_text(encoding="utf-8"),
        extra={"SERVER_NAME": meta["mcp"]["server_name"], "MCP_TIMEOUT_MS": str(meta["mcp"]["timeout_ms"])},
    ))
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
        "install": "Unpack the archive and run: sh anet-trae/install.sh [--cn | --project DIR]",
    })
