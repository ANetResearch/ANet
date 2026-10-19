## Settings

Antigravity has no settings interface for a plugin, so the plugin has no settings of its own. Two optional values are read from the environment of the process that starts the plugin.

| Variable | Default | Meaning |
| --- | --- | --- |
| `ANET_PLUGIN_IDENTITY` | empty | A named identity (`anet id ls`). Empty selects the default identity in `~/.anet`. |
| `ANET_PLUGIN_BIN` | empty | Absolute path of the `anet` executable. Empty searches `PATH`, `~/.local/bin`, `/usr/local/bin` and `/opt/homebrew/bin`. A path that is not an executable file is reported as an error; no other copy is used and the setup skill does not install one. |

The messages of the script call these settings `anetPath` (`ANET_PLUGIN_BIN`) and `identity` (`ANET_PLUGIN_IDENTITY`). Export the variables in the environment from which Antigravity is started, and start a new session after a change. The defaults need no configuration: a node installed by the setup skill sits in `~/.local/bin`, where the script finds it. The setup skill and the status skill run `bin/anet-run.sh` in a shell of the agent, which reads the same two variables.

The MCP server is declared in `mcp_config.json` without any path variable, because the documentation of Antigravity defines none for a plugin. The declaration starts a short `sh` program that looks for `bin/anet-mcp.sh` in the directory named by `ANTIGRAVITY_PLUGIN_ROOT` when the host sets it, then in `~/.gemini/config/plugins/anet` and in `~/.gemini/antigravity-cli/plugins/anet`. It never runs a file from the workspace or from the working directory, because a workspace can come from an untrusted source; a plugin installed at workspace scope is therefore not started unless `ANTIGRAVITY_PLUGIN_ROOT` names it. The plugin does not read or edit the configuration of Antigravity.

**Status.** The manifest and the layout follow the public documentation of Antigravity, and the scripts were run on Linux. The plugin was not loaded in Antigravity itself, because the program requires a Google account. The start of the MCP server by the host and the discovery of the skills by the host are therefore not verified.
