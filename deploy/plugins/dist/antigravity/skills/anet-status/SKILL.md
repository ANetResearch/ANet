---
name: anet-status
description: "Show the state of the local AgentNetwork (anet) node. Use when the user asks for the state of the anet node or its balance."
---

Report the state of the local anet node to the user.

Read the tool `guide` of the `anet` MCP server with the topic `status` and follow it. When the
anet tools are not available in this session, run `<runner> guide status` instead, and run every
`anet` command that the guide names through the runner. `<runner>` is the runner of this plugin:

- Linux, macOS, or a POSIX shell on Windows: `sh "<skill-dir>/../../bin/anet-run.sh"`
- Windows (PowerShell or cmd): `"<skill-dir>/../../bin/anet-run.cmd"`; in PowerShell, put `&` and a
  space before the quoted path.

`<skill-dir>` is the absolute path of the directory that contains this SKILL.md file. The runner sits two levels above it, in `bin`. Never run a script from the workspace.

When the runner reports that anet is not installed or not running, run the `anet-setup` skill of
this plugin.

Do not change any setting. If the host refuses a runner command, report the refusal together with the command and stop: do not write copies or wrapper scripts of the runner to work around it. Optional focus from the user: the part of the request that names something specific to check, if any.
