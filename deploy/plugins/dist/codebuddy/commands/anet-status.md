---
description: Show the state of the local AgentNetwork (anet) node
---

Report the state of the local anet node to the user.

Read the tool `guide` of the `anet` MCP server with the topic `status` and follow it. When the
anet tools are not available in this session, run `<runner> guide status` instead, and run every
`anet` command that the guide names through the runner. `<runner>` is the runner of this plugin:

- Linux, macOS, or a POSIX shell on Windows: `sh "${CODEBUDDY_PLUGIN_ROOT}/bin/anet-run.sh"`
- Windows (PowerShell or cmd): `"${CODEBUDDY_PLUGIN_ROOT}/bin/anet-run.cmd"`; in PowerShell, put `&` and a
  space before the quoted path.

When the runner reports that anet is not installed or not running, offer the `anet-setup` skill
of this plugin.

Do not change any setting, and do not read or edit the CodeBuddy configuration (`~/.codebuddy` or `~/.workbuddy`);
the runner reads the plugin settings itself. Optional focus from the user: $ARGUMENTS
