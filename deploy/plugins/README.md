# anet plugins

This directory holds the single source of the anet plugins for the hosts that the project supports through a plugin. A plugin connects a host to a local anet node. It registers the MCP server `anet mcp`, installs a skill that sends the agent to the guide of the installed anet, and adds a setup procedure, an `@anet` entry point and a status command where the host supports them.

Maintainer decision: the plugins are frozen. Each one is published once more and then does not change; every later feature of anet reaches the hosts through the installed anet executable alone. The skills and commands therefore carry nothing that depends on a release (no tool names, counts, versions or setup steps). They tell the agent to read the MCP tool `guide` (or `anet guide <topic>` through the runner) and to follow it; the topics `overview`, `setup` and `status` of that guide are permanent (`internal/agentwire/topics.go`). `test_generator.py` and the Go tests enforce both sides.

No host has a repository of its own. The shared files are written once, each host differs only in a small module, and `build.py` generates the plugin of every host from them.

| Path | Content |
| --- | --- |
| `common/` | The shared sources: `meta.json`; the POSIX runner `bin/anet-run.sh`, MCP launchers `bin/anet-mcp.sh` and `bin/anet-mcp` and installer helper `bin/anet-install.sh`; the Windows runner `bin/anet-run.cmd` with `bin/anet-run.ps1`, MCP launcher `bin/anet-mcp.cmd` and installer helper `bin/anet-install.cmd`; the skills `anet` and `anet-setup`; the commands `anet` and `anet-status`; `README.md`, `README_CN.md`, `LICENSE`, `icon.png`. |
| `targets/<key>.py` | One module per host. Discovered automatically; no shared file lists the targets. |
| `targets/<key>.d/` | Optional data files of that module (shell fragments, templates). |
| `generator.py` | The core: templates, file sets, archives, checks. Standard library only. |
| `build.py` | The command line of the generator. |
| `test_generator.py` | Unit tests of the core. |
| `dist/` | The generated tree, committed. Do not edit it by hand. |

## Build and check

```sh
python3 deploy/plugins/build.py --list                                   # the targets
python3 deploy/plugins/build.py --out deploy/plugins/dist --force --prune # regenerate the committed tree
python3 deploy/plugins/build.py --check                                  # fail if dist/ differs from a fresh generation
python3 deploy/plugins/build.py --out /tmp/plugins --target zcode        # a subset, into a scratch directory
python3 deploy/plugins/build.py --site /tmp/site                         # catalogues and archives for HTTPS hosting
python3 deploy/plugins/test_generator.py                                 # unit tests of the core
```

`--out DIR` writes `DIR/<key>/`, the plugin of each target, and the files that a target places at the root of the output (catalogues, manifests that a host reads from the root of a repository). When every target is built it also writes `README.md` and `LICENSE` at the root, so that the tree is complete as the root of a repository. `--force` is required to replace existing files, and `--prune` (full build only) also deletes files that the generator no longer produces, such as the files of a removed target. A module whose file name starts with an underscore (`targets/_example.py`) is built only when named with `--target`.

The Go tests of `internal/agentwire` run `--check`, the unit tests, the tests of every target and a build into a temporary directory, verify that every generated skill and command names the guide and only topics that the guide has, and run the POSIX runner of every generated target against the shared rules. `tests/test_windows_runner.py` runs the Windows runner of every target with PowerShell (`ANET_PWSH=<path of pwsh>`; skipped without it). Run them after every change of a source:

```sh
go test ./internal/agentwire ./cmd/anet
```

A change of the guide (`internal/agentwire/guide.go`, `topics.go`) needs no change here.

## The generated tree

`dist/` has this shape (more targets add their own directories and root files):

```text
dist/README.md, dist/LICENSE      generated, for the root of a repository
dist/<key>/                       the plugin of the target, exactly as the host installs it
dist/<root file>                  catalogues and manifests that a host reads from the repository root,
                                  for example .claude-plugin/marketplace.json
```

The tree is used in two ways. It is committed to the ANet repository, so that a marketplace can name the subdirectory `deploy/plugins/dist/<key>` of that repository (a sparse checkout of a subdirectory, which several hosts support). For the hosts that require the manifest at the root of a repository, the trees of those hosts are pushed unchanged to the mirror repository [ANetResearch/anet-plugins](https://github.com/ANetResearch/anet-plugins): its default branch holds `dist/antigravity`, its branch `gemini-cli` holds `dist/gemini-cli`, and each of its releases carries the Gemini CLI archives that `--site` writes (`linux.anet.tar.gz`, `darwin.anet.tar.gz`, `SHA256SUMS`). The generator creates and publishes no repository.

Maintainer decision: the plugins have one source, in the ANet repository (`common/`, `targets/`, `src/`). The generated `dist/` is committed next to it and is never edited by hand. The mirror repository has exactly one purpose: hosts that read a manifest only from the root of a repository can install from it. Its content is generated trees of `dist/` pushed unchanged, so no source exists in two places, and it is never edited by hand. Two catalogues for such hosts (Codex, Cursor) are also kept at the root of the ANet repository, as `.agents/plugins/marketplace.json` and `.cursor-plugin/marketplace.json`; they are copies of `src/codex/repo-root-marketplace.json` and `src/cursor/repo-root-marketplace.json`, and a Go test keeps them equal.

`--site DIR` writes the files that are served over HTTPS (marketplace catalogues, archives with their SHA-256) below the web root `https://agentnetwork.org.cn`: by default into `plugins/<key>/`, or into the `SITE_PATH` of the module. Archives are reproducible: entries are sorted, dated 1980-01-01, with fixed modes.

## Shared metadata

`common/meta.json` is read by every target: `name`, `version`, `display_name` and `description` (each with `en` and `zh-CN`), `author`, `homepage`, `repository`, `license`, `keywords`, `category`, `mcp` (`server_name`, `timeout_ms`) and `settings`. The two entries of `settings` (`identity`, `anetPath`) have a `key`, the environment variable `env` that the launcher reads (`ANET_PLUGIN_IDENTITY`, `ANET_PLUGIN_BIN`), `title`, `description`, `type` and `default`. There is one version for all targets. Hosts cache installed plugins by version, so raise `version` for every change of a generated file, then regenerate.

## The adapter interface

A target is the file `targets/<key>.py`. `<key>` (lowercase letters, digits and hyphens) is the name of the output directory and is stored in `module.KEY`. A module defines:

| Name | Required | Meaning |
| --- | --- | --- |
| `TITLE` | yes | Name of the host, for example `"Cursor"`. |
| `VARS` | yes | `dict` of template variables; see below. Must contain `TOOL`, `RUNNER_FROM_SKILL` and `RUNNER_FROM_COMMAND`. |
| `build(ctx)` | yes | Adds the files of the plugin to `ctx`. |
| `HELP` | no | One line for the table in `dist/README.md`. |
| `SITE_PATH` | no | Directory of the website tree for `--site`. Default `plugins/<key>`. |
| `MARKERS` | no | List of strings that occur only in files of this target (for example `"CURSOR_PLUGIN_ROOT"`). The generator fails when a file of another target contains one, which detects a template that leaked from one host into another. |
| `validate(ctx)` | no | Called before `build`; raise `ValueError` for a violation of the host's rules (name, version, field limits). |
| `transform(ctx, rel, text, lang)` | no | Called for every shared source after rendering; `rel` is the path in `common/` (for example `skills/anet-setup/SKILL.md`), `lang` is `en` or `zh-CN`. Return the text to emit. Use it for host-specific wording that the variables do not cover. |
| `site(ctx, site)` | no | Adds the files that are served over HTTPS; see below. |

### Context (`ctx`)

| Member | Meaning |
| --- | --- |
| `ctx.key`, `ctx.meta` | The target key and the shared metadata. `meta["description"]` is the English text, `meta["description_i18n"]` holds both languages. |
| `ctx.vars` | The template variables that the target sets (`VARS` and the conventional files of `targets/<key>.d/`, below). `build` may change them; the defaults of the others are added at every render. |
| `ctx.icon` | Bytes of `common/icon.png`. |
| `ctx.repo_url`, `ctx.dist_path`, `ctx.site_root_url`, `ctx.docs_url` | Constants: the ANet repository, `deploy/plugins/dist`, `https://agentnetwork.org.cn`, the documentation. |
| `ctx.add(path, data, executable=False)` | Adds a file to the plugin (`dist/<key>/<path>`). `data` is text or bytes. Paths are relative with `/` separators. |
| `ctx.add_json(path, obj)` | Same, as JSON: two-space indent, UTF-8, final newline. |
| `ctx.add_root(path, data)`, `ctx.add_root_json(path, obj)` | Adds a file at the root of the output (`dist/<path>`), for a catalogue or a manifest that the host reads from the root of a repository. The file belongs to the target. Two targets cannot write the same path, and a path may not sit inside the directory of a target. |
| `ctx.add_common(groups=None, dest=None, skip=())` | Adds the shared files, rendered for this target. `groups` selects `"bin"` (the POSIX and the Windows launchers and runners, `generator.BIN_FILES`), `"skills"` (`anet`, `anet-setup`), `"commands"` (`anet.md`, `anet-status.md`), `"docs"` (`README.md`, `README_CN.md`, `LICENSE`) and `"icon"` (`assets/icon.png`); `None` selects all but `"icon"`. `dest` maps a group to its directory in the plugin (defaults `bin`, `skills`, `commands`, `""`, `assets`). `skip` lists plugin paths to leave out. A `.cmd` file is written with CRLF line ends. |
| `ctx.render(rel, lang=None, extra=None)` | Renders `common/<rel>` and returns the text, for a target that places a shared source itself. `extra` overrides variables for the call. |
| `ctx.render_text(text, lang="en", extra=None)` | Renders a template given as text. |
| `ctx.command(name="anet-status")` | Returns `(description, body)` of the rendered shared command, for hosts that need another command format. |
| `ctx.skill(name="anet")` | Returns `(front matter fields, body)` of a rendered shared skill, for hosts that need another file layout or format. |
| `ctx.target_file(rel)` | Text of `targets/<key>.d/<rel>` without the final newline. |
| `ctx.crlf(text)` | `text` with CRLF line ends, for a batch file (`.cmd`) that the target adds itself. |
| `ctx.common_bytes(rel)` | Bytes of `common/<rel>`. |
| `ctx.files`, `ctx.root_files` | The files added so far (`File` objects with `data` and `executable`). |

### Site (`site` argument of `site(ctx, site)`)

| Member | Meaning |
| --- | --- |
| `site.base` | Public URL of this target's directory, without trailing slash. |
| `site.url(path)` | `site.base` and `path`. |
| `site.add(path, data)`, `site.add_json(path, obj)` | Adds a file below `SITE_PATH`. |
| `site.archive(path, wrapper=None, kind="zip", exec_bits=False)` | Archives the plugin tree of the target (`kind` is `"zip"` or `"tar.gz"`), stores it at `path` and returns `{"path", "url", "sha256", "size"}`. `wrapper` names a single top-level directory inside the archive. |

### Helpers of `generator.py`

Target modules receive everything through `ctx` and `site`, and do not import the generator. The functions behind them are `render`, `split_frontmatter`, `json_text`, `sha256_hex`, `zip_bytes` and `tar_gz_bytes`.

### Template variables

Shared sources contain `{{NAME}}` and, on lines of their own, `{{#if NAME}}`, `{{#else}}` and `{{#end}}`. A value is text, `True` or `False` (conditions only; an empty text is false), or `{"en": ..., "zh-CN": ...}` for text that differs by language. An unknown variable is an error.

| Variable | Default | Meaning |
| --- | --- | --- |
| `TOOL` | required | Name of the host in running text. |
| `RUNNER_FROM_SKILL` | required | The path of `bin/anet-run.sh` as a skill refers to it in the host's own syntax, for example `${CLAUDE_SKILL_DIR}/../../bin/anet-run.sh`. |
| `RUNNER_FROM_COMMAND` | required | The same path as a command refers to it, for example `${CLAUDE_PLUGIN_ROOT}/bin/anet-run.sh`. |
| `RUNNER_CMD_FROM_SKILL`, `RUNNER_CMD_FROM_COMMAND` | the paths above with `anet-run.cmd` | The Windows runner, as a skill and as a command refer to it. |
| `INSTALLER_FROM_SKILL`, `INSTALLER_CMD_FROM_SKILL` | the skill path with `anet-install.sh`, `anet-install.cmd` | The installer helpers for Linux and macOS, and for Windows. |
| `RUNNER_NOTE`, `RUNNER_COMMAND_NOTE` | empty | A paragraph after the runner paths of the skills and of the commands, for a placeholder such as `<skill-dir>` that the host does not replace. |
| `HAS_COMMAND` | `True` | `False` when the host has no command; the texts then do not mention `/anet-status`. |
| `SETTINGS_STORE` | `False` | `True` when the host keeps the plugin settings in a file that the runner reads. |
| `CONFIG_DIR` | empty | The configuration directory of the host (`~/.zcode`), named in the instructions that tell the agent not to read it. Used when `SETTINGS_STORE` is true. |
| `DOC_SLUG` | empty | Page `docs/agents/<slug>.rst` of the documentation; empty omits the link. |
| `INSTALL_STEP` | "Install and enable the plugin in {TOOL}." | First step of "Getting started" in the README. |
| `SETTINGS_SECTION` | environment variables | The whole "Settings" section of the README, with its heading. |
| `SETTINGS_READ_NOTE` | empty | A sentence (with a leading space) that says where the runner reads the settings outside the MCP server. |
| `COMPONENTS_SAME_RUNNER` | MCP server, command and setup skill | The list of components in the README sentence about the runner. |
| `ARGUMENTS` | `$ARGUMENTS` | How the command receives the optional focus from the user. |
| `RUNNER_SETTINGS_COMMENT` | environment | The comment of `anet-run.sh` that tells where the settings come from. |
| `RUNNER_READ_SETTINGS` | environment | The shell function `read_settings` of `anet-run.sh`, which prints two lines (`anetPath`, `identity`) from the settings store of the host. Without a store, the default reads `ANET_PLUGIN_BIN` and `ANET_PLUGIN_IDENTITY`. |
| `RUNNER_READ_SETTINGS_PS` | environment | The PowerShell function `Read-AnetSettings` of `anet-run.ps1`, which returns the same two values on Windows. |

### The runner

`bin/anet-run.sh` (Linux, macOS and a POSIX shell on Windows) and `bin/anet-run.cmd` with `bin/anet-run.ps1` (Windows) are the resolvers of the executable and the identity. Every anet command of a plugin goes through them: the MCP launcher, the skills and the commands. The rules are the same for every target and on both systems, and are tested for all of them:

1. The MCP launcher (`bin/anet-mcp.sh`, started by `bin/anet-mcp` or by `sh`; `bin/anet-mcp.cmd` on Windows) sets `ANET_PLUGIN_SETTINGS=env`, so the settings come from the MCP environment (`ANET_PLUGIN_BIN`, `ANET_PLUGIN_IDENTITY`).
2. Otherwise `read_settings` reads them from the settings store of the host; the host-specific code is the target's `RUNNER_READ_SETTINGS` fragment. It reads the two settings and nothing else, and ignores an ambiguous store.
3. A configured `anetPath` that is not an absolute path of an executable file (`.exe` on Windows) is an error. No other copy is used.
4. Without it, `PATH` (its absolute directories only), then `~/.local/bin`, `/usr/local/bin` and `/opt/homebrew/bin`, or on Windows `%LOCALAPPDATA%\Programs\anet`, `%LOCALAPPDATA%\anet\bin` and `%USERPROFILE%\.anet-bin`, are searched. The working directory is never searched.
5. `--resolve` prints the executable, version, identity and the runner command, and exits 0.
6. On Windows no setting is expanded on a command line of `cmd.exe`: `anet-run.ps1` reads the settings and hands back only a validated path and a validated identity name.

How each host starts the MCP launcher on Windows and on POSIX, with the evidence, is in the docstring of its module.

### Adding a target

1. Read the verified facts about the host: manifest, variables, component layout, how an MCP server is declared.
2. Copy `targets/_example.py` to `targets/<key>.py`. Set `TITLE`, `HELP`, `MARKERS` and `VARS`.
3. In `build(ctx)`, call `ctx.add_common()` (restrict `groups` or change `dest` where the layout differs), then add the manifest, the MCP configuration and any root catalogue. Derive every value from `ctx.meta` instead of writing it again.
4. Put host-specific shell fragments or templates into `targets/<key>.d/`.
5. Add `site(ctx, site)` only if the host installs from an HTTPS catalogue.
6. Run `python3 deploy/plugins/build.py --target <key> --out /tmp/<key>`, install the result in a sandbox of the host, and run the host's own validator when it has one.
7. Regenerate `dist/`, run the Go tests, and add the page `docs/agents/<key>.rst` for the host.

The generator gives each target its own copy of `meta`. Three variables can be supplied as files of `targets/<key>.d/` instead of in `VARS`: `runner-settings-comment.txt` (`RUNNER_SETTINGS_COMMENT`), `runner-read-settings.sh` (`RUNNER_READ_SETTINGS`), and the pair `settings-section.md` and `settings-section_CN.md` (`SETTINGS_SECTION`, English and Chinese; both files or neither).

Hosts that read a catalogue only from the root of a repository cannot use the ANet repository directly, because `dist/` is a subdirectory of it. For such a host the target writes its catalogue with `ctx.add_root`, and the mirror repository (the contents of `dist/`) is the way to install it from Git. A host that can install from a subdirectory (a sparse checkout, or a `git-subdir` source) is served by a catalogue in the `--site` tree that names `deploy/plugins/dist/<key>` in the ANet repository. Two targets cannot write the same root file; each host has its own catalogue file.

A module must not edit `common/` or another target's files. A wording difference that a variable does not cover is handled in `transform`; if several targets need the same change, add a variable to `common/` and its default to `generator.py` once.

## Publication

The generator publishes nothing. Pushing a mirror repository, submitting to a marketplace or a directory, publishing a package, and uploading the website tree to `agentnetwork.org.cn` each require the authorization of the project owner, and are done by hand from the output of `build.py`.

* ZCode: `python3 deploy/zcode/build.py --out DIR`, then `deploy/release/apex/publish.sh --zcode DIR`. This is the same output as `--site` produces below `zcode/`. A published version is never replaced; raise `version` for a change.
* Other hosts: the files under `dist/` and the output of `--site` are static; the steps of each host are in its page under `docs/agents/`.
