---
description: Show the state of the local AgentNetwork (anet) node
---

Report the state of the local anet node to the user.

Read the tool `guide` of the `anet` MCP server with the topic `status` and follow it. When the
anet tools are not available in this session, run `<runner> guide status` instead, and run every
`anet` command that the guide names through the runner. `<runner>` is the runner of this plugin:

- Linux, macOS, or a POSIX shell on Windows: `sh "{{RUNNER_FROM_COMMAND}}"`
- Windows (PowerShell or cmd): `"{{RUNNER_CMD_FROM_COMMAND}}"`; in PowerShell, put `&` and a
  space before the quoted path.
{{#if RUNNER_COMMAND_NOTE}}

{{RUNNER_COMMAND_NOTE}}
{{#end}}

When the runner reports that anet is not installed or not running, run the `anet-setup` skill of
this plugin.

{{#if SETTINGS_STORE}}
Do not change any setting, and do not read or edit the {{TOOL}} configuration (`{{CONFIG_DIR}}`);
the runner reads the plugin settings itself. Optional focus from the user: {{ARGUMENTS}}
{{#else}}
Do not change any setting. Optional focus from the user: {{ARGUMENTS}}
{{#end}}
