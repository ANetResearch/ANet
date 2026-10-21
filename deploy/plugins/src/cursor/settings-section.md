## Settings

The plugin declares two optional variables. Cursor passes their values to the MCP server `anet` as environment variables of the same names. A variable without a value is empty, and an empty variable selects the default.

| Variable | Default | Meaning |
| --- | --- | --- |
| `ANET_PLUGIN_IDENTITY` | empty | A named identity (`anet id ls`). Empty selects the default identity. |
| `ANET_PLUGIN_BIN` | empty | Absolute path of the `anet` executable (`anet.exe` on Windows). Empty searches `PATH`, then `~/.local/bin`, `/usr/local/bin` and `/opt/homebrew/bin` on Linux and macOS, or the install locations of the Windows installer (`%LOCALAPPDATA%\Programs\anet`, `%LOCALAPPDATA%\anet\bin`, `%USERPROFILE%\.anet-bin`) on Windows. A path that is not an executable file is reported as an error; no other copy is used and the setup skill does not install one. |

Cursor takes the values of plugin variables from the plugin configuration, which a team administrator sets in the Cursor dashboard. Cursor has no per-user settings file for a plugin, and the plugin does not read the configuration of Cursor. The skills and the commands run the runner of the plugin (`bin/anet-run.sh`, or `bin/anet-run.cmd` on Windows) in a shell of the agent, which reads the same two variable names from the environment of that shell. Where a non-default value is needed, set it in both places. The defaults need no configuration: the runner finds a node that the setup skill installed.
