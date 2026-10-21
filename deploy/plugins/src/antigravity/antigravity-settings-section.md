## Settings

Antigravity has no settings interface for a plugin, so the plugin has no settings of its own. Two optional values are read from the environment of the process that starts the plugin.

| Variable | Default | Meaning |
| --- | --- | --- |
| `ANET_PLUGIN_IDENTITY` | empty | A named identity (`anet id ls`). Empty selects the default identity. |
| `ANET_PLUGIN_BIN` | empty | Absolute path of the `anet` executable (`anet.exe` on Windows). Empty searches `PATH`, then `~/.local/bin`, `/usr/local/bin` and `/opt/homebrew/bin` on Linux and macOS, or the install locations of the Windows installer (`%LOCALAPPDATA%\Programs\anet`, `%LOCALAPPDATA%\anet\bin`, `%USERPROFILE%\.anet-bin`) on Windows. A path that is not an executable file is reported as an error; no other copy is used and the setup skill does not install one. |

The messages of the runner call these settings `anetPath` (`ANET_PLUGIN_BIN`) and `identity` (`ANET_PLUGIN_IDENTITY`). Export the variables in the environment from which Antigravity is started, and start a new session after a change. The defaults need no configuration: the runner finds a node that the setup skill installed. The skills run the runner of the plugin (`bin/anet-run.sh`, or `bin/anet-run.cmd` on Windows) in a shell of the agent, which reads the same two variables.

The MCP server is declared in `mcp_config.json` as `node ${PLUGIN_ROOT}/bin/anet-mcp.cjs`, because Antigravity expands the plugin directory in the arguments of an MCP server but not in its command, and provides Node.js itself when none is installed. The script starts `bin/anet-mcp.sh` with `/bin/sh` on Linux and macOS, and `bin/anet-mcp.cmd` with `cmd.exe` on Windows, from its own directory. It never runs a file from the workspace or from the working directory. The plugin does not read or edit the configuration of Antigravity.

**Status.** The manifest and the layout follow the public documentation of Antigravity and the MCP handling of the `agy` program; the scripts were run on Linux, and the Windows scripts were tested with PowerShell. The plugin was not loaded in Antigravity itself, because the program requires a Google account. The start of the MCP server by the host and the discovery of the skills by the host are therefore not verified.
