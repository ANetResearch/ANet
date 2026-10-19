---
description: Show the state of the local anet node, its hub and its spending limits
---

Report the state of the local anet node to the user.

1. Call the `node_status` tool of the `anet` MCP server, then `get_balance`.
2. Report: the identity (name and AID), whether the daemon runs, the hub and whether this node has
   joined it, the inbound policy, the balance and the auto and agent spending limits, and any
   warning the tools return. Give values as the tools return them; do not estimate.
3. If the anet tools are not available in this session, run `anet status` and `anet doctor` in the
   terminal instead and report their output. When anet is not installed or not running, offer the
   `anet-setup` skill of this plugin.

Do not change any setting. Optional focus from the user: $ARGUMENTS
