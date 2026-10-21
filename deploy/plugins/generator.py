"""Core of the anet plugin generator (see README.md in this directory).

This module renders the shared sources in common/ once per target, collects the
files that a target module (targets/<key>.py) emits, and writes, checks or
packages them. It uses the Python standard library only. Target modules do not
import it: they receive a Context (the adapter interface) and, for the
catalogue of a website, a Site.
"""

import copy
import gzip
import hashlib
import importlib.util
import io
import json
import os
import re
import shutil
import stat
import sys
import tarfile
import zipfile
from pathlib import Path, PurePosixPath

HERE = Path(__file__).resolve().parent
COMMON = HERE / "common"
TARGETS = HERE / "targets"
DIST = HERE / "dist"
REPO_URL = "https://github.com/ANetResearch/ANet"
DIST_PATH_IN_REPO = "deploy/plugins/dist"
SITE_ROOT_URL = "https://agentnetwork.org.cn"
DOCS_URL = "https://docs.agentnetwork.org.cn/docs/en/latest/"
KEY_RE = re.compile(r"^[a-z0-9][a-z0-9-]{0,31}$")
ZIP_DATE = (1980, 1, 1, 0, 0, 0)
LANGS = ("en", "zh-CN")


class BuildError(Exception):
    """A problem in the sources, in a target module or in the arguments."""


# --------------------------------------------------------------------------
# Helpers that target modules may use through the Context

def sha256_hex(data):
    return hashlib.sha256(data).hexdigest()


def json_text(obj):
    """The JSON layout of every generated JSON file: two spaces, UTF-8, final newline."""
    return json.dumps(obj, ensure_ascii=False, indent=2) + "\n"


def split_frontmatter(text):
    """Split `---` front matter off a Markdown text.

    Returns (fields, body). fields maps each top-level `key: value` line to its
    value as text (one pair of surrounding double or single quotes removed); the
    front matter is not parsed as YAML. body is the text after the closing line,
    without the blank line that follows it.
    """
    if not text.startswith("---\n"):
        return {}, text
    end = text.find("\n---\n", 4)
    if end < 0:
        raise BuildError("front matter is not closed")
    fields = {}
    for line in text[4:end].split("\n"):
        m = re.match(r"^([A-Za-z0-9_-]+):\s*(.*)$", line)
        if m:
            v = m.group(2)
            if len(v) >= 2 and v[0] == v[-1] and v[0] in "\"'":
                v = v[1:-1]
            fields[m.group(1)] = v
    body = text[end + 5:]
    return fields, body[1:] if body.startswith("\n") else body


def _ordered(files):
    # Component-wise order, as pathlib sorts: "skills/anet/x" before "skills/anet-setup/x".
    return sorted(files, key=lambda rel: PurePosixPath(rel).parts)


def zip_bytes(files, wrapper=None, exec_bits=False):
    """Reproducible zip of {relative path: File}.

    Entries are sorted component-wise, dated 1980-01-01, created by "Unix" and
    stored deflated at level 6, with mode 0644. With exec_bits the entries
    carry the regular-file type and mode 0755 for executable files. wrapper,
    when given, is the single top-level directory of the archive.
    """
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w", zipfile.ZIP_DEFLATED) as zf:
        for rel in _ordered(files):
            f = files[rel]
            info = zipfile.ZipInfo(f"{wrapper}/{rel}" if wrapper else rel, date_time=ZIP_DATE)
            if exec_bits:
                info.external_attr = (stat.S_IFREG | (0o755 if f.executable else 0o644)) << 16
            else:
                info.external_attr = 0o644 << 16  # the layout of the ZCode marketplace archives
            info.create_system = 3
            info.compress_type = zipfile.ZIP_DEFLATED
            zf.writestr(info, f.data, compresslevel=6)
    return buf.getvalue()


def tar_gz_bytes(files, wrapper=None):
    """Reproducible .tar.gz of {relative path: File}: sorted, mtime 0, numeric owner 0, mode 0644/0755."""
    raw = io.BytesIO()
    with tarfile.open(fileobj=raw, mode="w", format=tarfile.PAX_FORMAT) as tf:
        for rel in _ordered(files):
            f = files[rel]
            info = tarfile.TarInfo(f"{wrapper}/{rel}" if wrapper else rel)
            info.size, info.mtime, info.uid, info.gid = len(f.data), 0, 0, 0
            info.uname = info.gname = ""
            info.mode = 0o755 if f.executable else 0o644
            tf.addfile(info, io.BytesIO(f.data))
    out = io.BytesIO()
    with gzip.GzipFile(fileobj=out, mode="wb", mtime=0, filename="") as gz:
        gz.write(raw.getvalue())
    return out.getvalue()


# --------------------------------------------------------------------------
# Templates

_TAG = re.compile(r"^\{\{#(?:(if) ([A-Z][A-Z0-9_]*)|(else|end))\}\}\n?$")
_BAD_TAG = re.compile(r"^\{\{#")
_VAR = re.compile(r"\{\{([A-Z][A-Z0-9_]*)\}\}")


def _pick(value, lang, name):
    if isinstance(value, dict):
        if lang not in value:
            raise BuildError(f"variable {name} has no value for language {lang}")
        value = value[lang]
    return value


def render(text, variables, lang="en", origin="template"):
    """Render a template.

    {{NAME}} is replaced by the text of the variable NAME. A line that consists
    only of {{#if NAME}}, {{#else}} (at most once per block) or {{#end}} opens,
    switches or closes a conditional block (it nests); the tag lines produce no
    output. Any other line that begins with {{# is an error. A variable is
    a string, a bool (conditions only) or a dict {"en": ..., "zh-CN": ...}. An
    unknown variable, a bool in a substitution and an unbalanced block are
    errors. Substituted text is not scanned again.
    """
    out, stack = [], []  # one [branch is active, else seen] per open block
    for n, line in enumerate(text.splitlines(keepends=True), 1):
        m = _TAG.match(line)
        if m:
            kind = m.group(1) or m.group(3)
            if kind == "if":
                name = m.group(2)
                if name not in variables:
                    raise BuildError(f"{origin}:{n}: unknown variable {name}")
                stack.append([bool(_pick(variables[name], lang, name)), False])
            elif kind == "else":
                if not stack or stack[-1][1]:
                    raise BuildError(f"{origin}:{n}: {{{{#else}}}} without {{{{#if}}}}, or a second {{{{#else}}}}")
                stack[-1] = [not stack[-1][0], True]
            else:
                if not stack:
                    raise BuildError(f"{origin}:{n}: {{{{#end}}}} without {{{{#if}}}}")
                stack.pop()
            continue
        if _BAD_TAG.match(line):
            raise BuildError(f"{origin}:{n}: malformed tag {line.strip()!r}")
        if not all(a for a, _ in stack):
            continue

        def sub(mm, n=n):
            name = mm.group(1)
            if name not in variables:
                raise BuildError(f"{origin}:{n}: unknown variable {name}")
            v = _pick(variables[name], lang, name)
            if not isinstance(v, str):
                raise BuildError(f"{origin}:{n}: variable {name} is not text")
            return v

        out.append(_VAR.sub(sub, line))
    if stack:
        raise BuildError(f"{origin}: {{{{#if}}}} block is not closed")
    return "".join(out)


# --------------------------------------------------------------------------
# Output files

class File:
    __slots__ = ("data", "executable")

    def __init__(self, data, executable=False):
        self.data = data.encode("utf-8") if isinstance(data, str) else bytes(data)
        self.executable = bool(executable)


def _check_path(path):
    p = PurePosixPath(path)
    if not path or p.is_absolute() or ".." in p.parts or "\\" in path or path.endswith("/") or ".git" in p.parts:
        raise BuildError(f"invalid output path {path!r}: use a relative path with / separators, no .. and no .git")
    return p.as_posix()


# The shared launchers and runners (common/bin). The .sh files and the
# extensionless anet-mcp run on Linux and macOS (and in a POSIX shell on
# Windows); the .cmd files, with anet-run.ps1, run on Windows.
BIN_FILES = ("anet-run.sh", "anet-mcp.sh", "anet-mcp", "anet-install.sh",
             "anet-run.cmd", "anet-run.ps1", "anet-mcp.cmd", "anet-install.cmd")
# The shared commands (common/commands): the @anet entry point and the status report.
COMMANDS = ("anet", "anet-status")


def crlf(text):
    """The text with CRLF line ends, as cmd.exe expects them in a batch file."""
    return text.replace("\r\n", "\n").replace("\n", "\r\n")


# Where each group of the shared sources goes inside a plugin by default.
DEFAULT_DEST = {"bin": "bin", "skills": "skills", "commands": "commands", "docs": "", "icon": "assets"}
GROUPS = tuple(DEFAULT_DEST)


class Context:
    """What build(ctx) of a target module receives: the adapter interface.

    Attributes
      key         the target key (the file stem of the module)
      module      the target module
      meta        the shared plugin metadata (common/meta.json), see README.md
      vars        the template variables that the target sets (VARS of the module
                  and the conventional files of targets/<key>.d/); defaults for
                  the others are added at every render, so build() may change
                  these before rendering
      files       {path inside the plugin: File}; written to <out>/<key>/
      root_files  {path at the output root: File}; for catalogues and manifests
                  that a platform reads from the repository root
      icon        bytes of the icon (common/icon.png)
      repo_url, dist_path, site_root_url, docs_url   constants of the project

    Methods are documented in README.md ("The adapter interface").
    """

    def __init__(self, module, meta, variables):
        self.module = module
        self.key = module.KEY
        self.meta = copy.deepcopy(meta)
        self.vars = dict(variables)
        self.files = {}
        self.root_files = {}
        self.icon = (COMMON / "icon.png").read_bytes()
        self.repo_url, self.dist_path = REPO_URL, DIST_PATH_IN_REPO
        self.site_root_url, self.docs_url = SITE_ROOT_URL, DOCS_URL

    # -- reading sources
    def target_file(self, rel):
        """Text of targets/<key>.d/<rel>, with the final newline removed."""
        p = TARGETS / f"{self.key}.d" / rel
        if not p.is_file():
            raise BuildError(f"{self.key}: missing target file {p.relative_to(HERE)}")
        t = p.read_text(encoding="utf-8")
        return t[:-1] if t.endswith("\n") else t

    def common_bytes(self, rel):
        return (COMMON / rel).read_bytes()

    @staticmethod
    def crlf(text):
        """text with CRLF line ends, for a batch file (.cmd) of the target."""
        return crlf(text)

    def render(self, rel, lang=None, extra=None):
        """Render the shared source common/<rel> for this target and return the text.

        lang defaults to zh-CN for files named *_CN.md and to en otherwise.
        extra adds or overrides variables for this call. The transform() hook of
        the module, if any, runs last.
        """
        lang = lang or ("zh-CN" if rel.endswith("_CN.md") else "en")
        variables = self.variables(extra)
        text = render((COMMON / rel).read_text(encoding="utf-8"), variables, lang, f"common/{rel}")
        hook = getattr(self.module, "transform", None)
        return hook(self, rel, text, lang) if hook else text

    def render_text(self, text, lang="en", extra=None, origin="inline template"):
        """Render a template given as text (for target-owned templates)."""
        return render(text, self.variables(extra), lang, origin)

    def variables(self, extra=None):
        """All template variables: those of the target, `extra`, and the defaults for the rest."""
        v = dict(self.vars)
        if extra:
            v.update(extra)
        return _default_vars(v)

    def skill(self, name="anet"):
        """The shared skill common/skills/<name>/SKILL.md, rendered: (front matter fields, body)."""
        return split_frontmatter(self.render(f"skills/{name}/SKILL.md"))

    def command(self, name="anet-status"):
        """The shared command common/commands/<name>.md, rendered: (description, body)."""
        fields, body = split_frontmatter(self.render(f"commands/{name}.md"))
        if "description" not in fields:
            raise BuildError(f"command {name} has no description")
        return fields["description"], body

    # -- emitting files
    def add(self, path, data, executable=False):
        """Add a file to the plugin tree (<out>/<key>/<path>). data is str or bytes."""
        path = _check_path(path)
        if path in self.files:
            raise BuildError(f"{self.key}: {path} is added twice")
        self.files[path] = File(data, executable)

    def add_json(self, path, obj, executable=False):
        self.add(path, json_text(obj), executable)

    def add_root(self, path, data, executable=False):
        """Add a file at the output root (<out>/<path>), e.g. a marketplace catalogue.

        The file is owned by this target. Two targets cannot write the same path.
        """
        path = _check_path(path)
        if path in self.root_files:
            raise BuildError(f"{self.key}: root file {path} is added twice")
        self.root_files[path] = File(data, executable)

    def add_root_json(self, path, obj):
        self.add_root(path, json_text(obj))

    def add_common(self, groups=None, dest=None, skip=()):
        """Add the shared files to the plugin tree, rendered for this target.

        groups: which of "bin" (BIN_FILES), "skills" (anet,
        anet-setup), "commands" (anet.md, anet-status.md), "docs" (README.md,
        README_CN.md, LICENSE) and "icon" (assets/icon.png) to add; the default
        (None) is every group except "icon", which is added only when named. dest maps a group to its directory in the plugin
        ("" is the plugin root). skip lists plugin paths to leave out.
        """
        if groups is None:
            groups = tuple(g for g in GROUPS if g != "icon")
        where = dict(DEFAULT_DEST, **(dest or {}))
        unknown = [g for g in groups if g not in DEFAULT_DEST]
        if unknown:
            raise BuildError(f"{self.key}: unknown groups {unknown}")

        def put(group, rel_in_group, src, text=True):
            d = where[group]
            path = f"{d}/{rel_in_group}" if d else rel_in_group
            if path in skip:
                return
            executable = bool((COMMON / src).stat().st_mode & stat.S_IXUSR)
            data = self.render(src) if text else self.common_bytes(src)
            if text and src.endswith(".cmd"):
                data = crlf(data)
            self.add(path, data, executable)

        if "bin" in groups:
            for name in BIN_FILES:
                put("bin", name, f"bin/{name}")
        if "skills" in groups:
            for name in ("anet", "anet-setup"):
                put("skills", f"{name}/SKILL.md", f"skills/{name}/SKILL.md")
        if "commands" in groups:
            for name in COMMANDS:
                put("commands", f"{name}.md", f"commands/{name}.md")
        if "docs" in groups:
            put("docs", "README.md", "README.md")
            put("docs", "README_CN.md", "README_CN.md")
            put("docs", "LICENSE", "LICENSE")
        if "icon" in groups:
            put("icon", "icon.png", "icon.png", text=False)


class Site:
    """What site(ctx, site) of a target receives: one directory of a website.

    Files added here are written to <site dir>/<module SITE_PATH> and are served
    at base (an https URL without trailing slash; attribute base).
    """

    def __init__(self, ctx, base):
        if not base.startswith("https://"):
            raise BuildError("the base URL of a site must be an https:// URL: the hosts fetch catalogues and archives over HTTPS only")
        self.ctx, self.base = ctx, base.rstrip("/")
        self.files = {}

    def url(self, path):
        return f"{self.base}/{_check_path(path)}"

    def add(self, path, data):
        path = _check_path(path)
        if path in self.files:
            raise BuildError(f"{self.ctx.key}: site file {path} is added twice")
        self.files[path] = File(data)

    def add_json(self, path, obj):
        self.add(path, json_text(obj))

    def archive(self, path, wrapper=None, kind="zip", exec_bits=False):
        """Archive the plugin tree of the target at path; returns {url, path, sha256, size}."""
        files = self.ctx.files
        data = zip_bytes(files, wrapper, exec_bits) if kind == "zip" else tar_gz_bytes(files, wrapper)
        self.add(path, data)
        return {"path": path, "url": self.url(path), "sha256": sha256_hex(data), "size": len(data)}


# --------------------------------------------------------------------------
# Loading

def load_meta():
    meta = json.loads((COMMON / "meta.json").read_text(encoding="utf-8"))
    for k in ("name", "version", "display_name", "description", "author", "homepage", "repository",
              "license", "keywords", "category", "settings", "mcp"):
        if k not in meta:
            raise BuildError(f"common/meta.json lacks {k}")
    if not re.match(r"^\d+\.\d+\.\d+$", meta["version"]):
        raise BuildError(f"version {meta['version']!r} is not MAJOR.MINOR.PATCH")
    if set(meta["description"]) != set(LANGS) or set(meta["display_name"]) != set(LANGS):
        raise BuildError("description and display_name must hold " + " and ".join(LANGS))
    meta["description_i18n"] = dict(meta["description"])
    meta["description"] = meta["description"]["en"]
    return meta


def check_common():
    for path in sorted(COMMON.rglob("*")):
        rel = path.relative_to(COMMON).as_posix()
        if path.is_symlink():
            raise BuildError(f"common/{rel} is a symbolic link")
        if path.is_file() and (rel.endswith((".pyc", ".DS_Store")) or "__pycache__" in rel):
            raise BuildError(f"common/{rel} is not a source file")


def _default_vars(tvars):
    """Fill the optional variables from the required ones and from common/."""
    v = dict(tvars)
    tool = v["TOOL"]

    def soft(name, value):
        v.setdefault(name, value)

    soft("ARGUMENTS", "$ARGUMENTS")
    soft("SETTINGS_STORE", False)
    soft("CONFIG_DIR", "")
    soft("HAS_COMMAND", True)
    soft("DOC_SLUG", "")
    soft("SETTINGS_READ_NOTE", "")
    soft("INSTALLER_FROM_SKILL", v["RUNNER_FROM_SKILL"].replace("anet-run.sh", "anet-install.sh"))
    # The Windows runner and installer sit next to the POSIX ones.
    soft("RUNNER_CMD_FROM_SKILL", _plain(v["RUNNER_FROM_SKILL"], "RUNNER_FROM_SKILL").replace("anet-run.sh", "anet-run.cmd"))
    soft("RUNNER_CMD_FROM_COMMAND", _plain(v["RUNNER_FROM_COMMAND"]).replace("anet-run.sh", "anet-run.cmd"))
    soft("INSTALLER_CMD_FROM_SKILL", _plain(v["RUNNER_FROM_SKILL"], "RUNNER_FROM_SKILL").replace("anet-run.sh", "anet-install.cmd"))
    soft("RUNNER_NOTE", "")
    soft("RUNNER_COMMAND_NOTE", "")
    soft("INSTALL_STEP", {"en": f"Install and enable the plugin in {tool}.", "zh-CN": f"在 {tool} 中安装并启用本插件。"})
    if "COMPONENTS_SAME_RUNNER" not in v:
        if v["HAS_COMMAND"]:
            v["COMPONENTS_SAME_RUNNER"] = {
                "en": "the MCP server (`anet mcp`), the commands `/anet` and `/anet-status` and the skills",
                "zh-CN": "MCP 服务（`anet mcp`）、`/anet` 与 `/anet-status` 命令以及各 skill ",
            }
        else:
            v["COMPONENTS_SAME_RUNNER"] = {
                "en": "the MCP server (`anet mcp`) and the skills",
                "zh-CN": "MCP 服务（`anet mcp`）与各 skill ",
            }
    soft("SETTINGS_SECTION", {
        "en": "## Settings\n\n"
              "The plugin has no settings of its own. The two optional values below are read from the environment of the process that starts the plugin.\n\n"
              "| Variable | Default | Meaning |\n| --- | --- | --- |\n"
              "| `ANET_PLUGIN_IDENTITY` | empty | A named identity (`anet id ls`). Empty selects the default identity. |\n"
              "| `ANET_PLUGIN_BIN` | empty | " + SEARCH_TEXT["en"] + " |",
        "zh-CN": "## 设置项\n\n"
                 "本插件没有自己的设置界面。下列两项为可选项，从启动插件的进程的环境变量中读取。\n\n"
                 "| 变量 | 默认值 | 含义 |\n| --- | --- | --- |\n"
                 "| `ANET_PLUGIN_IDENTITY` | 空 | 指定的命名身份（`anet id ls`）。为空时使用默认身份。 |\n"
                 "| `ANET_PLUGIN_BIN` | 空 | " + SEARCH_TEXT["zh-CN"] + " |",
    })
    for name, rel in (("RUNNER_SETTINGS_COMMENT", "runner-settings-env.txt"), ("RUNNER_READ_SETTINGS", "runner-read-settings-env.sh"),
                      ("RUNNER_READ_SETTINGS_PS", "runner-read-settings-env.ps1")):
        if name not in v:
            t = (COMMON / rel).read_text(encoding="utf-8")
            v[name] = t[:-1] if t.endswith("\n") else t
    return v


def _plain(value, name="RUNNER_FROM_COMMAND"):
    if not isinstance(value, str):
        raise BuildError(f"{name} must be text, not a per-language value")
    return value


# What the setting anetPath means, for the settings section of a README. The
# targets that write their own settings section use the same text.
SEARCH_TEXT = {
    "en": ("Absolute path of the `anet` executable (`anet.exe` on Windows). Empty searches `PATH`, then "
           "`~/.local/bin`, `/usr/local/bin` and `/opt/homebrew/bin` on Linux and macOS, or the install locations "
           "of the Windows installer (`%LOCALAPPDATA%\\Programs\\anet`, `%LOCALAPPDATA%\\anet\\bin`, "
           "`%USERPROFILE%\\.anet-bin`) on Windows. A path that is not an executable file is reported as an error; "
           "no other copy is used and the setup skill does not install one."),
    "zh-CN": ("`anet` 可执行文件（Windows 上为 `anet.exe`）的绝对路径。为空时先查找 `PATH`，然后在 Linux 和 macOS 上查找 "
              "`~/.local/bin`、`/usr/local/bin` 和 `/opt/homebrew/bin`，在 Windows 上查找 Windows 安装程序的安装位置"
              "（`%LOCALAPPDATA%\\Programs\\anet`、`%LOCALAPPDATA%\\anet\\bin`、`%USERPROFILE%\\.anet-bin`）。"
              "若该路径不是可执行文件，则报告错误；不会改用其他副本，安装 skill 也不会另行安装。"),
}


# Variables that a target supplies as files of targets/<key>.d/ (text; the
# final newline is removed). The README variable has one file per language.
DATA_VARS = {
    "RUNNER_SETTINGS_COMMENT": {"": "runner-settings-comment.txt"},
    "RUNNER_READ_SETTINGS": {"": "runner-read-settings.sh"},
    "RUNNER_READ_SETTINGS_PS": {"": "runner-read-settings.ps1"},
    "SETTINGS_SECTION": {"en": "settings-section.md", "zh-CN": "settings-section_CN.md"},
}


def _target_vars(mod):
    v = dict(mod.VARS)
    d = TARGETS / f"{mod.KEY}.d"
    for name, files in DATA_VARS.items():
        if name in v:
            continue
        texts = {}
        for lang, fn in files.items():
            if (d / fn).is_file():
                t = (d / fn).read_text(encoding="utf-8")
                texts[lang] = t[:-1] if t.endswith("\n") else t
        if len(texts) == len(files):
            v[name] = texts[""] if "" in texts else texts
        elif texts:
            raise BuildError(f"{mod.KEY}: targets/{mod.KEY}.d must hold all of {', '.join(files.values())} or none")
    return v


REQUIRED_VARS = ("TOOL", "RUNNER_FROM_SKILL", "RUNNER_FROM_COMMAND")


def discover(only=None):
    """Load the target modules: targets/<key>.py, every file whose name does not start with _.

    A module whose name starts with _ (an example, a draft) is built only when
    named in `only`.
    """
    sys.dont_write_bytecode = True
    wanted = list(only) if only else None
    found = {}
    available = []
    for path in sorted(TARGETS.glob("*.py")):
        stem = path.stem
        if stem.startswith("_") and not (wanted and stem in wanted):
            continue
        if not KEY_RE.match(stem.lstrip("_")):
            raise BuildError(f"targets/{path.name}: the file stem must be lowercase letters, digits and hyphens")
        available.append(stem)
        if wanted and stem not in wanted:
            # A subset build does not run the code of the other targets: a defect in one adapter
            # must not stop the build of another (for example the ZCode archive).
            continue
        spec = importlib.util.spec_from_file_location(f"anet_plugin_target_{stem.replace('-', '_')}", path)
        mod = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(mod)
        mod.KEY = stem
        for attr, kind in (("TITLE", str), ("VARS", dict)):
            if not isinstance(getattr(mod, attr, None), kind):
                raise BuildError(f"targets/{path.name}: {attr} is missing or has the wrong type")
        if not callable(getattr(mod, "build", None)):
            raise BuildError(f"targets/{path.name}: build(ctx) is missing")
        for name in REQUIRED_VARS:
            if name not in mod.VARS:
                raise BuildError(f"targets/{path.name}: VARS lacks {name}")
        mod.SITE_PATH = _check_path(getattr(mod, "SITE_PATH", f"plugins/{stem}"))
        mod.MARKERS = list(getattr(mod, "MARKERS", []))
        mod.HELP = getattr(mod, "HELP", "")
        found[stem] = mod
    if wanted:
        missing = [k for k in wanted if k not in found]
        if missing:
            raise BuildError(f"unknown target {missing}; available: {', '.join(sorted(available))}")
        found = {k: found[k] for k in wanted}
    if not found:
        raise BuildError("no target found in targets/")
    return found


def _check_collisions(tree):
    """No path may be both a file and a directory of another path."""
    paths = set(tree)
    for path in paths:
        parts = PurePosixPath(path).parts
        for i in range(1, len(parts)):
            if "/".join(parts[:i]) in paths:
                raise BuildError(f"{path} lies below the file {'/'.join(parts[:i])}")


class Result:
    """The output of a build: per-target contexts and the files of the output root."""

    def __init__(self):
        self.contexts = {}   # key -> Context
        self.root = {}       # path -> File (all owners)
        self.owner = {}      # root path -> key or None (core)

    def tree(self):
        """{path relative to the output root: File} for everything that --out writes."""
        t = {}
        for key, ctx in self.contexts.items():
            for rel, f in ctx.files.items():
                t[f"{key}/{rel}"] = f
        t.update(self.root)
        _check_collisions(t)
        return t


def build_all(only=None):
    check_common()
    meta = load_meta()
    modules = discover(only)
    res = Result()
    for key, mod in modules.items():
        ctx = Context(mod, meta, _target_vars(mod))
        validate = getattr(mod, "validate", None)
        if validate:
            validate(ctx)
        mod.build(ctx)
        if not ctx.files:
            raise BuildError(f"{key}: build(ctx) added no files")
        for rel, f in ctx.root_files.items():
            if rel in res.root:
                raise BuildError(f"{key}: root file {rel} is already written by {res.owner[rel]}")
            if rel.split("/", 1)[0] in modules:
                raise BuildError(f"{key}: root file {rel} would sit inside the directory of target {rel.split('/', 1)[0]}")
            res.root[rel] = f
            res.owner[rel] = key
        res.contexts[key] = ctx
    check_markers(res, modules)
    if not only:
        add_mirror_root(res, meta, modules)
    return res


def check_markers(res, modules):
    """No file of a target may contain a marker that another target declares."""
    for key, ctx in res.contexts.items():
        owned = dict(ctx.files)
        owned.update({f"<root>/{p}": f for p, f in ctx.root_files.items()})
        for other, mod in modules.items():
            if other == key:
                continue
            for rel, f in owned.items():
                try:
                    text = f.data.decode("utf-8")
                except UnicodeDecodeError:
                    continue
                for marker in mod.MARKERS:
                    if marker in text:
                        raise BuildError(f"{key}: {rel} contains {marker!r}, which belongs to target {other}")


def add_mirror_root(res, meta, modules):
    """README.md and LICENSE of the output root, so that it can be the root of a repository."""
    rows = "\n".join(f"| `{k}` | {m.TITLE} | {m.HELP} |" for k, m in sorted(modules.items()))
    text = render((COMMON / "MIRROR_README.md").read_text(encoding="utf-8"),
                  {"TARGET_TABLE": rows, "VERSION": meta["version"], "REPO_URL": REPO_URL,
                   "DIST_PATH": DIST_PATH_IN_REPO}, "en", "common/MIRROR_README.md")
    for path, data in (("README.md", text), ("LICENSE", (COMMON / "LICENSE").read_bytes())):
        if path in res.root:
            raise BuildError(f"root file {path} is reserved for the generator")
        res.root[path] = File(data)
        res.owner[path] = None


def build_site(res, base):
    """Run site(ctx, site) of every target that defines it.

    Returns {path relative to the web root: File}; the files of a target sit
    below its SITE_PATH.
    """
    out = {}
    for key, ctx in res.contexts.items():
        hook = getattr(ctx.module, "site", None)
        if not hook:
            continue
        sp = ctx.module.SITE_PATH
        site = Site(ctx, f"{base.rstrip('/')}/{sp}")
        hook(ctx, site)
        for rel, f in site.files.items():
            path = f"{sp}/{rel}"
            if path in out:
                raise BuildError(f"{key}: site file {path} is already written by another target")
            out[path] = f
    _check_collisions(out)
    return out


# --------------------------------------------------------------------------
# Writing and checking

def write_tree(tree, dest, owned_dirs=(), force=False):
    dest = Path(dest)
    if dest.is_symlink():
        raise BuildError(f"{dest} is a symbolic link")
    dest.mkdir(parents=True, exist_ok=True)
    for rel in list(tree) + [d + "/x" for d in owned_dirs]:
        cur = dest
        for part in PurePosixPath(rel).parts[:-1]:
            cur = cur / part
            if cur.is_symlink():
                raise BuildError(f"{cur} is a symbolic link; the generator does not write through links")
    clashes = []
    for d in owned_dirs:
        p = dest / d
        if p.exists() and any(p.iterdir()):
            clashes.append(d + "/")
    for rel in tree:
        if os.path.lexists(dest / rel) and not any(rel.startswith(d + "/") for d in owned_dirs):
            clashes.append(rel)
    if clashes and not force:
        raise BuildError("these paths exist in the output directory (use --force to replace them): " + ", ".join(sorted(clashes)))
    for d in owned_dirs:
        if (dest / d).exists():
            shutil.rmtree(dest / d)
    for rel, f in sorted(tree.items()):
        p = dest / rel
        if p.is_symlink() or p.exists():
            # Replace the entry; never write into an inode that another name (a hard link) shares.
            p.unlink()
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_bytes(f.data)
        os.chmod(p, 0o755 if f.executable else 0o644)


def prune_tree(tree, dest):
    """Delete the files of dest that the generator does not produce, and the directories left empty.

    Returns the deleted paths. Only for a full build of a directory that the
    generator owns (the committed dist/).
    """
    dest = Path(dest)
    removed = []
    for p in sorted(dest.rglob("*"), reverse=True):
        rel = p.relative_to(dest).as_posix()
        if p.is_symlink() or (p.is_file() and rel not in tree):
            p.unlink()
            removed.append(rel)
        elif p.is_dir() and not any(p.iterdir()):
            p.rmdir()
    return sorted(removed)


def compare_tree(tree, dest, owned_prefixes=None):
    """Differences between the generated tree and a directory, as a list of lines.

    With owned_prefixes (a subset of targets was generated), only paths below
    those prefixes are compared; otherwise the directory must hold exactly the
    generated files.
    """
    dest = Path(dest)
    problems = []
    existing = {}
    if dest.is_dir():
        for p in sorted(dest.rglob("*")):
            if p.is_symlink():
                problems.append(f"{p.relative_to(dest).as_posix()}: is a symbolic link")
            elif p.is_file():
                existing[p.relative_to(dest).as_posix()] = p
    else:
        problems.append(f"{dest} does not exist")

    def inside(rel):
        return owned_prefixes is None or any(rel.startswith(x + "/") for x in owned_prefixes)

    for rel, f in sorted(tree.items()):
        p = existing.get(rel)
        if p is None:
            problems.append(f"{rel}: missing")
            continue
        if p.read_bytes() != f.data:
            problems.append(f"{rel}: content differs")
        elif bool(p.stat().st_mode & stat.S_IXUSR) != f.executable:
            problems.append(f"{rel}: executable bit differs (expected {'755' if f.executable else '644'})")
    for rel in sorted(existing):
        if rel not in tree and inside(rel):
            problems.append(f"{rel}: not generated")
    return problems
