## Settings

The plugin has two options, which Claude Code stores with the other settings of the user. They are optional. Change them in Claude Code with `/plugin` (select `anet`, then the configuration), or from a terminal:

```sh
echo '{"identity": "work", "anetPath": "/opt/anet/bin/anet"}' | claude plugin configure anet@agentnetwork --values-stdin
```

A change applies to the MCP server when a new session starts. The node of the selected identity has to be running (`anet --id <name> up`); otherwise the `anet` server offers only the `connect` tool until it is.

| Option | Default | Meaning |
| --- | --- | --- |
| `identity` | empty | A named identity (`anet id ls`). Empty selects the default identity in `~/.anet`. |
| `anetPath` | empty | Absolute path of the `anet` executable. Empty searches `PATH`, `~/.local/bin`, `/usr/local/bin` and `/opt/homebrew/bin`. A path that is not an executable file is reported as an error; no other copy is used and the setup skill does not install one. |
