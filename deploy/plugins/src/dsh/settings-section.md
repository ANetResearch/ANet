## Installation

DSH installs a plugin into a profile with `dsh plugin`, which forwards to `pnpm`; `pnpm` must be on `PATH`. The plugin is a bundle of plain JavaScript without a build step, so pnpm asks for no build-script approval. Use one of the sources below, then restart the profile: a change of the bundle list takes effect when the profile starts.

| Source | Command |
| --- | --- |
| Local directory (this directory) | `dsh plugin --profile <profile> add ./<directory>` |
| Git subdirectory of the ANet repository | `dsh plugin --profile <profile> add "git+https://github.com/ANetResearch/ANet.git#<commit>&path:/deploy/plugins/dist/dsh"` |

`<commit>` is the full hash of a commit of the ANet repository that contains the plugin. The command `dsh plugin --profile <profile> remove dsh-anet` removes the plugin. The plugin declares the DSH releases `>=0.2.0-0 <0.3.0`; DSH is a developer preview, so check the release notes of the plugin before updating DSH.

## Settings

DSH has no declarative settings form for a plugin. The two settings are the configuration of the row `anet-plugin` in `cordis.patch.yml`. To change them, restate the row in the patch of the profile (`$DSH_HOME/profiles/<profile>/cordis.patch.yml`); the row replaces the whole configuration of the row of the plugin, so give both keys:

```yaml
- id: anet-plugin
  config:
    anetPath: /usr/local/bin/anet
    identity: work
```

| Setting | Default | Meaning |
| --- | --- | --- |
| `identity` | empty | A named identity (`anet id ls`). Empty selects the default identity in `~/.anet`. |
| `anetPath` | empty | Absolute path of the `anet` executable. Empty searches `PATH`, `~/.local/bin`, `/usr/local/bin` and `/opt/homebrew/bin`. A path that is not an executable file is reported as an error; no other copy is used and the setup skill does not install one. |

When a key is empty, the environment variables `ANET_PLUGIN_IDENTITY` and `ANET_PLUGIN_BIN` of the process that starts DSH are used. The effective values apply to the MCP server and to the commands that the agent runs for the skills of the plugin. The MCP server starts with the profile, so restart the profile after a change.

The MCP server `anet` waits at most 15 minutes for a tool call (`toolCallTimeoutMs`), so that `wait_task` can wait for a long task; DSH stops other MCP calls after 60 seconds by default. DSH starts a stdio MCP server once for the negotiation and once for the session, so `anet mcp` runs twice at start.
