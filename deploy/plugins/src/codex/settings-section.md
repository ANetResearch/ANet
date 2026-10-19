## Settings

Codex has no settings screen for plugins, so the plugin has no settings of its own. The two optional values below are read from the environment of the Codex process. The MCP server definition of the plugin forwards exactly these two variables to `bin/anet-mcp.sh` (`env_vars`), because Codex starts a plugin MCP server with a minimal environment. The skills run `bin/anet-run.sh` in the shell of the session, which reads the same two variables.

| Variable | Default | Meaning |
| --- | --- | --- |
| `ANET_PLUGIN_IDENTITY` | empty | A named identity (`anet id ls`). Empty selects the default identity in `~/.anet`. |
| `ANET_PLUGIN_BIN` | empty | Absolute path of the `anet` executable. Empty searches `PATH`, `~/.local/bin`, `/usr/local/bin` and `/opt/homebrew/bin`. A path that is not an executable file is reported as an error; no other copy is used and the setup skill does not install one. |

The messages of the runner call them the settings `anetPath` (`ANET_PLUGIN_BIN`) and `identity` (`ANET_PLUGIN_IDENTITY`). Export the variables in the environment that starts Codex, for example in the profile of the shell, and start a new session after a change. Codex does not provide a plugin-level `env` override, and the plugin never reads or edits `~/.codex`. If the MCP server `anet` was registered earlier with `anet agents wire codex`, that entry in `config.toml` has the same name and takes precedence over the server of the plugin, so these variables have no effect on it; remove it with `anet agents unwire codex`.
