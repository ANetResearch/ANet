## Installation and settings

Trae has no plugin package format that a repository can provide. This bundle is therefore installed with the script `install.sh`, which copies the files into a configuration directory of Trae and prints the MCP configuration. The script starts no process, opens no connection and reads no file of Trae. The only file of Trae that it creates is a project `mcp.json` that does not exist yet.

```sh
sh install.sh                    # user scope: ~/.trae
sh install.sh --cn               # user scope for Trae CN: ~/.trae-cn
sh install.sh --project DIR      # project scope: DIR/.trae
```

| File in the bundle | Installed as | Purpose |
| --- | --- | --- |
| `skills/anet`, `skills/anet-setup` | `<trae-dir>/skills/` | The two skills. Trae loads the skills of `~/.trae/skills` (Trae CN: `~/.trae-cn/skills`) in every project, and those of `.trae/skills` in the project. |
| `commands/anet-status.md` | `<trae-dir>/commands/` | The command `/anet-status`. |
| `anet/bin/anet-run.sh`, `anet/bin/anet-mcp.sh` | `<trae-dir>/anet/bin/` | The runner and the launcher of the MCP server. The skills reach the runner at `<skill-dir>/../../anet/bin/anet-run.sh`. |
| `mcp.json` | project scope only | The MCP configuration of a project, written when `.trae/mcp.json` does not exist. |

To install by hand, copy these directories to the same places, and use the content of `mcp.json` (project scope) or the output of `install.sh` (user scope) for the MCP server.

**MCP server.** Trae starts the server with `/bin/sh` and the launcher, so that a missing executable bit does not matter and the launcher finds `anet` in `~/.local/bin` although a desktop application does not always inherit the `PATH` of a login shell.

- User scope: the script prints the configuration. Enter it under Settings, MCP, Add, Create Manually (or Raw Config), or open the `trae://` link that the script prints (for Trae only), and confirm the prompts of Trae. The path of the global MCP file of Trae is not documented, so the script does not write it. Trae documents that the command of an MCP server must not contain spaces and says nothing about spaces in its arguments, so a home directory with a space is refused.
- Project scope: the script writes `.trae/mcp.json`, which names the launcher through `${workspaceFolder}`. Turn on "Project MCP" in the settings of Trae and confirm the prompt. An existing `.trae/mcp.json` is not changed; the script prints the member to add.

**Settings.** The plugin has two optional settings. Give them to `install.sh` when needed; an option that is not given keeps the value of the earlier installation.

| Option | Default | Meaning |
| --- | --- | --- |
| `--identity NAME` | empty | A named identity (`anet id ls`). Empty selects the default identity in `~/.anet`. |
| `--anet PATH` | empty | Absolute path of the `anet` executable. Empty searches `PATH`, `~/.local/bin`, `/usr/local/bin` and `/opt/homebrew/bin`. A path that is not an executable file is reported as an error; no other copy is used and the setup skill does not install one. |

The script stores the values in the file `anet/settings` of the installed bundle, which `bin/anet-run.sh` reads for the setup skill and the status command, and in the `env` of the printed MCP entry (`ANET_PLUGIN_IDENTITY`, `ANET_PLUGIN_BIN`), which the launcher reads. After a change, run the script again and update the MCP entry of Trae with the new output, so that both places agree.

**Removal.** `sh install.sh --remove` (with the same `--cn` or `--project`) deletes the installed files. Delete the MCP server `anet` in Trae as well.

**Status.** The locations and formats follow the Trae documentation. The scripts were run on Linux, and the MCP launcher was driven through a generic MCP client. The files were not loaded in the Trae application itself.
