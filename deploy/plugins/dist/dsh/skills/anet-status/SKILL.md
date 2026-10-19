---
name: anet-status
description: "Show the state of the local anet node, its hub and its spending limits. Use when the user asks for the state of the anet node, its hub, its balance or its spending limits, or runs anet-status."
---

Report the state of the local anet node to the user.

The plugin settings select the executable and the identity of every anet command of this skill.

First run `sh "${DSH_BUNDLE_DIR}/bin/anet-run.sh" --resolve`. Its output states the executable, the identity and the `runner` command.

1. Call the `node_status` tool of the `anet` MCP server, then `get_balance`.
2. Report: the identity (name and AID), whether the daemon runs, the hub and whether this node has
   joined it, the inbound policy, the balance and the auto and agent spending limits, and any
   warning the tools return. Give values as the tools return them; do not estimate.
3. If the anet tools are not available in this session, run the `runner` command shown above with
   `status`, then with `doctor`, for example `sh <path>/anet-run.sh status`, and report their
   output. The runner applies the same executable and identity as the MCP server; do not run a
   bare `anet` command instead. When the executable line reports a problem, or anet is not
   running, offer the `anet-setup` skill of this plugin.

Do not change any setting, and do not read or edit the DeepSeek Harness configuration
(`~/.dsh`, or the directory named by `DSH_HOME`); the plugin passes the settings to the runner itself.
If the request names a focus, report on it first.
