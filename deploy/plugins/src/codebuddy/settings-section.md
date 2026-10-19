## Settings

The plugin has two options, which CodeBuddy keeps in the user settings file together with the other settings of the user. They are optional. Add them to the `pluginConfigs` entry of the plugin in `~/.codebuddy/settings.json` (WorkBuddy: `~/.workbuddy/settings.json`; the directory named by `CODEBUDDY_CONFIG_DIR` or `WORKBUDDY_CONFIG_DIR` when one is set):

```json
{
  "pluginConfigs": {
    "anet@agentnetwork": {
      "options": { "identity": "work", "anetPath": "/opt/anet/bin/anet" }
    }
  }
}
```

A change applies to the MCP server when a new session starts.

| Option | Default | Meaning |
| --- | --- | --- |
| `identity` | empty | A named identity (`anet id ls`). Empty selects the default identity in `~/.anet`. |
| `anetPath` | empty | Absolute path of the `anet` executable. Empty searches `PATH`, `~/.local/bin`, `/usr/local/bin` and `/opt/homebrew/bin`. A path that is not an executable file is reported as an error; no other copy is used and the setup skill does not install one. |
