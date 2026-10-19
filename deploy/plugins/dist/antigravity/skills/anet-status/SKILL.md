---
name: anet-status
description: "Show the state of the local anet node, its hub and its spending limits. Use when the user asks for the state of the anet node, the balance or the spending limits."
---

Report the state of the local anet node to the user.

The plugin settings select an executable and an identity. Resolve them first:

```bash
sh "<skill-dir>/../../bin/anet-run.sh" --resolve
```

`<skill-dir>` is the absolute path of the directory that contains this SKILL.md file.

1. Call the `node_status` tool of the `anet` MCP server, then `get_balance`.
2. Report: the identity (name and AID), whether the daemon runs, the hub and whether this node has
   joined it, the inbound policy, the balance and the auto and agent spending limits, and any
   warning the tools return. Give values as the tools return them; do not estimate.
3. If the anet tools are not available in this session, run the `runner` command shown above with
   `status`, then with `doctor`, for example `sh <path>/anet-run.sh status`, and report their
   output. The runner applies the same executable and identity as the MCP server; do not run a
   bare `anet` command instead. When the executable line reports a problem, or anet is not
   running, offer the `anet-setup` skill of this plugin.

Do not change any setting. If the host refuses a runner command, report the refusal together with the command and stop: do not write copies or wrapper scripts of the runner to work around it. Optional focus from the user: the part of the request that names something specific to check, if any.
