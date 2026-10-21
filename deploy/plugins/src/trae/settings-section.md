## Installation and settings

Trae has no plugin package format that a repository can provide. This bundle is therefore installed with an installer, which copies the files into a configuration directory of Trae and prints the MCP configuration: `install.sh` on Linux and macOS, and `install.cmd` on Windows, which runs `install.ps1` with Windows PowerShell 5.1. The installers start no process, open no connection and read no file of Trae. The only file of Trae that they create is a project `mcp.json` that does not exist yet.

```sh
sh install.sh                    # user scope: ~/.trae
sh install.sh --cn               # user scope for Trae CN: ~/.trae-cn
sh install.sh --project DIR      # project scope: DIR/.trae
```

```bat
install.cmd                      & rem user scope: %USERPROFILE%\.trae
install.cmd -Cn                  & rem user scope for Trae CN: %USERPROFILE%\.trae-cn
install.cmd -Project DIR         & rem project scope: DIR\.trae
```

| File in the bundle | Installed as | Purpose |
| --- | --- | --- |
| `skills/anet`, `skills/anet-setup` | `<trae-dir>/skills/` | The two skills. Trae loads the skills of `~/.trae/skills` (Trae CN: `~/.trae-cn/skills`) in every project, and those of `.trae/skills` in the project. |
| `commands/anet.md`, `commands/anet-status.md` | `<trae-dir>/commands/` | The commands `/anet` and `/anet-status`. |
| `anet/bin/` | `<trae-dir>/anet/bin/` | The runners and the launchers of the MCP server: `anet-run.sh` and `anet-mcp.sh` on Linux and macOS, `anet-run.cmd`, `anet-run.ps1` and `anet-mcp.cmd` on Windows. The skills reach the runner at `<skill-dir>/../../anet/bin/`. |
| `mcp.json` | project scope only | The MCP configuration of a project, written when `.trae/mcp.json` does not exist. |

To install by hand, copy these directories to the same places, and use the output of the installer for the MCP server.

**MCP server.** On Linux and macOS, Trae starts the server with `/bin/sh` and the launcher `anet-mcp.sh`, so that a missing executable bit does not matter and the launcher finds `anet` in `~/.local/bin` although a desktop application does not always inherit the `PATH` of a login shell. On Windows, the command is the full path of `anet-mcp.cmd`. Trae documents that the command of an MCP server must not contain spaces: on Linux and macOS a home directory with a space is refused, and on Windows a path with a space is replaced by its short (8.3) form, or refused when the volume provides none.

- User scope: the installer prints the configuration. Enter it under Settings, MCP, Add, Create Manually (or Raw Config), or open the `trae://` link that the installer prints (for Trae only), and confirm the prompts of Trae. The path of the global MCP file of Trae is not documented, so the installer does not write it.
- Project scope: the installer writes `.trae/mcp.json`. On Linux and macOS it names the launcher through `${workspaceFolder}`; on Windows it names the full path of the launcher. Turn on "Project MCP" in the settings of Trae and confirm the prompt. An existing `.trae/mcp.json` is not changed; the installer prints the member to add.

**Settings.** The plugin has two optional settings. Give them to the installer when needed; an option that is not given keeps the value of the earlier installation.

| Option | Default | Meaning |
| --- | --- | --- |
| `--identity NAME` (`-Identity NAME`; `-ClearIdentity` clears it) | empty | A named identity (`anet id ls`). Empty selects the default identity. |
| `--anet PATH` (`-Anet PATH`; `-ClearAnet` clears it) | empty | Absolute path of the `anet` executable (`anet.exe` on Windows). Empty searches `PATH`, then `~/.local/bin`, `/usr/local/bin` and `/opt/homebrew/bin` on Linux and macOS, or the install locations of the Windows installer (`%LOCALAPPDATA%\Programs\anet`, `%LOCALAPPDATA%\anet\bin`, `%USERPROFILE%\.anet-bin`) on Windows. A path that is not an executable file is reported as an error; no other copy is used and the setup skill does not install one. |

The installer stores the values in the file `anet/settings` of the installed bundle, which the runner reads for the skills and the commands, and in the `env` of the printed MCP entry (`ANET_PLUGIN_IDENTITY`, `ANET_PLUGIN_BIN`), which the launcher reads. After a change, run the installer again and update the MCP entry of Trae with the new output, so that both places agree.

**Removal.** `sh install.sh --remove` or `install.cmd -Remove` (with the same scope option) deletes the installed files. Delete the MCP server `anet` in Trae as well.

**Status.** The locations and formats follow the Trae documentation. The scripts were run on Linux, the Windows scripts were tested with PowerShell, and the MCP launcher was driven through a generic MCP client. The files were not loaded in the Trae application itself.
