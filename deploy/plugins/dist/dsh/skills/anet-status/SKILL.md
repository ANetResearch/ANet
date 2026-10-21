---
name: anet-status
description: "Show the state of the local AgentNetwork (anet) node. Use when the user asks for the state of the anet node, its hub or its balance, or runs anet-status."
---

Report the state of the local anet node to the user.

Read the tool `guide` of the `anet` MCP server with the topic `status` and follow it. When the
anet tools are not available in this session, run `<runner> guide status` instead, and run every
`anet` command that the guide names through the runner. `<runner>` is the runner of this plugin:

- Linux, macOS, or a POSIX shell on Windows: `sh "${DSH_BUNDLE_DIR}/bin/anet-run.sh"`
- Windows (PowerShell or cmd): `"${DSH_BUNDLE_DIR}/bin/anet-run.cmd"`; in PowerShell, put `&` and a
  space before the quoted path.

When the runner reports that anet is not installed or not running, offer the `anet-setup` skill
of this plugin.

Do not change any setting, and do not read or edit the DeepSeek Harness configuration
(`~/.dsh`, or the directory named by `DSH_HOME`); the plugin passes the settings to the runner itself.
If the request names a focus, report on it first.
