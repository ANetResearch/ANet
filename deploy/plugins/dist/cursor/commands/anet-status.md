---
name: anet-status
description: Show the state of the local anet node, its hub and its spending limits
---

Report the state of the local anet node to the user.

The runner of this plugin, `bin/anet-run.sh`, applies the plugin settings and selects the executable and the identity of the node. Only step 3 needs it.

Its path is `<plugin-root>/bin/anet-run.sh`. `<plugin-root>` is the root directory of the anet plugin: the directory that holds `bin`, `skills` and `.cursor-plugin`, which is two levels above the file `SKILL.md` of the `anet-setup` skill. `sh "<plugin-root>/bin/anet-run.sh" --resolve` prints the executable, the version, the identity and a `runner` line.

1. Call the `node_status` tool of the `anet` MCP server, then `get_balance`.
2. Report: the identity (name and AID), whether the daemon runs, the hub and whether this node has
   joined it, the inbound policy, the balance and the auto and agent spending limits, and any
   warning the tools return. Give values as the tools return them; do not estimate.
3. If the anet tools are not available in this session, run the runner (`sh "<plugin-root>/bin/anet-run.sh"`) with
   `status`, then with `doctor`, and report their
   output. The runner applies the same executable and identity as the MCP server; do not run a
   bare `anet` command instead. When `status` reports that the daemon is not running, the hint line of its output names the default identity; report the identity of this plugin instead, which is the value of `ANET_PLUGIN_IDENTITY` or, when that is empty, the default, and which also appears in the data directory path that `status` prints. When the executable line reports a problem, or anet is not
   running, offer the `anet-setup` skill of this plugin.

Do not change any setting. Optional focus from the user: any text that follows the command name
