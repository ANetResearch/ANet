## Settings

The extension declares two optional settings. Gemini CLI asks for them when the extension is installed (`--skip-settings` leaves them empty), stores them in the file `.env` of the extension directory, and passes them to the MCP server `anet` as environment variables. An empty answer selects the default.

| Setting | Variable | Default | Meaning |
| --- | --- | --- | --- |
| anet identity | `ANET_PLUGIN_IDENTITY` | empty | A named identity (`anet id ls`). Empty selects the default identity in `~/.anet`. |
| anet executable | `ANET_PLUGIN_BIN` | empty | Absolute path of the `anet` executable. Empty searches `PATH`, `~/.local/bin`, `/usr/local/bin` and `/opt/homebrew/bin`. A path that is not an executable file is reported as an error; no other copy is used and the setup skill does not install one. |

Change a value with `gemini extensions config anet`, then start a new session. The messages of the script call these settings `anetPath` (`ANET_PLUGIN_BIN`) and `identity` (`ANET_PLUGIN_IDENTITY`). The setup skill and the status skill run `bin/anet-run.sh` in a shell of the agent. There the script reads the two variables from its environment and, when they are not set, from the `.env` file of this extension, and nothing else. It does not read the settings or credentials of Gemini CLI. The defaults need no configuration: a node installed by the setup skill sits in `~/.local/bin`, where the script finds it.
