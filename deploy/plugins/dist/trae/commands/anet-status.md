---
description: Show the state of the local anet node, its hub and its spending limits
---

Report the state of the local anet node to the user.

The runner of this bundle, `anet-run.sh`, applies the plugin settings and selects the executable and the identity of the node.

`<skill-dir>` is the directory of the `anet-setup` skill of this bundle, which Trae loaded from `~/.trae/skills`, `~/.trae-cn/skills` or `.trae/skills`; the runner is `<skill-dir>/../../anet/bin/anet-run.sh`. `sh "<skill-dir>/../../anet/bin/anet-run.sh" --resolve` prints the executable, the version, the identity and a `runner` line.

1. Call the `node_status` tool of the `anet` MCP server, then `get_balance`.
2. Report: the identity (name and AID), whether the daemon runs, the hub and whether this node has
   joined it, the inbound policy, the balance and the auto and agent spending limits, and any
   warning the tools return. Give values as the tools return them; do not estimate.
3. If the anet tools are not available in this session, run the runner (`sh "<skill-dir>/../../anet/bin/anet-run.sh"`) with
   `status`, then with `doctor`, and report their
   output. The runner applies the same executable and identity as the MCP server; do not run a
   bare `anet` command instead. When the executable line reports a problem, or anet is not
   running, offer the `anet-setup` skill of this plugin.

Do not change any setting, and do not read or edit the Trae configuration (`mcp.json`); the runner
reads the plugin settings itself. Optional focus from the user: any text that follows the command name
