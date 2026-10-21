## Installation and settings

OpenCode has no plugin marketplace. A plugin is a package that is named in the `plugin` list of `opencode.json`; OpenCode installs a package from npm by itself and loads a directory as it is. The plugin registers the MCP server, the skills and the command when OpenCode starts.

Install the package into the global configuration (`~/.config/opencode/opencode.json`). Leave out `--global` to install it for the current project only:

```sh
opencode plugin @agentnetwork/opencode-anet --global
```

A directory of this plugin, such as `deploy/plugins/dist/opencode` of a checkout of the ANet repository, is installed in the same way, with its absolute path in place of the package name. To add the plugin by hand, or to set the settings below, edit the entry in the file; the options of an entry are the settings:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "plugin": [
    ["@agentnetwork/opencode-anet", { "anetPath": "/usr/local/bin/anet", "identity": "work" }]
  ]
}
```

OpenCode reads its configuration when it starts: restart it after a change. The plugin keeps an existing `mcp.anet` entry (for example the one that `anet agents wire opencode` writes) unchanged; the options then apply to the skills and the command only. Run `anet agents unwire opencode` to let the plugin supply the server.

| Option | Default | Meaning |
| --- | --- | --- |
| `identity` | empty | A named identity (`anet id ls`). Empty selects the default identity. |
| `anetPath` | empty | Absolute path of the `anet` executable (`anet.exe` on Windows). Empty searches `PATH`, then `~/.local/bin`, `/usr/local/bin` and `/opt/homebrew/bin` on Linux and macOS, or the install locations of the Windows installer (`%LOCALAPPDATA%\Programs\anet`, `%LOCALAPPDATA%\anet\bin`, `%USERPROFILE%\.anet-bin`) on Windows. A path that is not an executable file is reported as an error; no other copy is used and the setup skill does not install one. |
