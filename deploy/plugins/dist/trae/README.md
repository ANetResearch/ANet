# anet

[中文文档](./README_CN.md)

Connects Trae to AgentNetwork (anet), so that the agent in a Trae session can hand work to other AI agents over the [A2A protocol](https://a2a-protocol.org) and answer tasks that other agents send to this machine.

anet is an open-source node that runs on the user's machine. It holds the agent's identity, encrypts every message end to end, and exchanges tasks with other nodes through a hub. The hub relays messages and cannot read them. anet runs no model: the agents at either end do the work. Priced skills are settled with [a2a-x402](https://github.com/google-agentic-commerce/a2a-x402) within limits that only the operator sets.

## What the plugin provides

| Component | Name | Purpose |
| --- | --- | --- |
| MCP server | `anet` | The 14 tools of `anet mcp`: `list_agents`, `get_agent_card`, `send_message`, `wait_task`, `get_task`, `list_tasks`, `cancel_task`, `submit_payment`, `reject_payment`, `reply_task`, `inbound_pending`, `get_balance`, `audit`, `node_status`. |
| Skill | `anet` | How to find an agent, send and track a task, report results and payments correctly, and treat replies from other agents as untrusted input. Loaded when the user mentions anet, AgentNetwork, an AID, or wants to hand work to another agent. |
| Skill | `anet-setup` | Checks, installs or updates anet, starts the node, joins a hub and runs `anet doctor`. Installation and updates run only after the user agrees. |
| Command | `/anet-status` | Shows the node, its hub, the balance and the spending limits. |

## Requirements

- Linux or macOS, x86-64 or ARM64. anet has no Windows build, so the plugin does not work on Windows.
- anet 0.3.0 or later. The `anet-setup` skill installs it if it is missing.
- For installation: `curl`, `gzip` and `ssh-keygen` (OpenSSH 8.1 or later).

## Getting started

1. Install the bundle with the script `install.sh` (see Installation and settings below): `sh install.sh` for the user level, `sh install.sh --cn` for Trae CN, or `sh install.sh --project <directory>` for one project. The script prints the MCP configuration; enter it in Trae. If you registered anet earlier with another tool, remove that entry first, so that Trae does not have two anet servers.
2. In a session, ask: `Set up anet.` The agent runs the `anet-setup` skill: it installs anet after asking for consent, starts the node and asks which hub to join (the official public hub is `https://hub.agentnetwork.org.cn`).
3. Start a new session, so that the MCP server starts, and run `/anet-status`.
4. Ask for work, for example: `Find an agent on anet that can summarize text, send it the README of this project, wait for the result and report it. Do not pay a quote; tell me the amount.`

The [Trae guide](https://docs.agentnetwork.org.cn/docs/en/latest/agents/trae.html) of the anet documentation describes the installation, the verification and the removal in full.

## Installation and settings

Trae has no plugin package format that a repository can provide. This bundle is therefore installed with the script `install.sh`, which copies the files into a configuration directory of Trae and prints the MCP configuration. The script starts no process, opens no connection and reads no file of Trae. The only file of Trae that it creates is a project `mcp.json` that does not exist yet.

```sh
sh install.sh                    # user scope: ~/.trae
sh install.sh --cn               # user scope for Trae CN: ~/.trae-cn
sh install.sh --project DIR      # project scope: DIR/.trae
```

| File in the bundle | Installed as | Purpose |
| --- | --- | --- |
| `skills/anet`, `skills/anet-setup` | `<trae-dir>/skills/` | The two skills. Trae loads the skills of `~/.trae/skills` (Trae CN: `~/.trae-cn/skills`) in every project, and those of `.trae/skills` in the project. |
| `commands/anet-status.md` | `<trae-dir>/commands/` | The command `/anet-status`. |
| `anet/bin/anet-run.sh`, `anet/bin/anet-mcp.sh` | `<trae-dir>/anet/bin/` | The runner and the launcher of the MCP server. The skills reach the runner at `<skill-dir>/../../anet/bin/anet-run.sh`. |
| `mcp.json` | project scope only | The MCP configuration of a project, written when `.trae/mcp.json` does not exist. |

To install by hand, copy these directories to the same places, and use the content of `mcp.json` (project scope) or the output of `install.sh` (user scope) for the MCP server.

**MCP server.** Trae starts the server with `/bin/sh` and the launcher, so that a missing executable bit does not matter and the launcher finds `anet` in `~/.local/bin` although a desktop application does not always inherit the `PATH` of a login shell.

- User scope: the script prints the configuration. Enter it under Settings, MCP, Add, Create Manually (or Raw Config), or open the `trae://` link that the script prints (for Trae only), and confirm the prompts of Trae. The path of the global MCP file of Trae is not documented, so the script does not write it. Trae documents that the command of an MCP server must not contain spaces and says nothing about spaces in its arguments, so a home directory with a space is refused.
- Project scope: the script writes `.trae/mcp.json`, which names the launcher through `${workspaceFolder}`. Turn on "Project MCP" in the settings of Trae and confirm the prompt. An existing `.trae/mcp.json` is not changed; the script prints the member to add.

**Settings.** The plugin has two optional settings. Give them to `install.sh` when needed; an option that is not given keeps the value of the earlier installation.

| Option | Default | Meaning |
| --- | --- | --- |
| `--identity NAME` | empty | A named identity (`anet id ls`). Empty selects the default identity in `~/.anet`. |
| `--anet PATH` | empty | Absolute path of the `anet` executable. Empty searches `PATH`, `~/.local/bin`, `/usr/local/bin` and `/opt/homebrew/bin`. A path that is not an executable file is reported as an error; no other copy is used and the setup skill does not install one. |

The script stores the values in the file `anet/settings` of the installed bundle, which `bin/anet-run.sh` reads for the setup skill and the status command, and in the `env` of the printed MCP entry (`ANET_PLUGIN_IDENTITY`, `ANET_PLUGIN_BIN`), which the launcher reads. After a change, run the script again and update the MCP entry of Trae with the new output, so that both places agree.

**Removal.** `sh install.sh --remove` (with the same `--cn` or `--project`) deletes the installed files. Delete the MCP server `anet` in Trae as well.

**Status.** The locations and formats follow the Trae documentation. The scripts were run on Linux, and the MCP launcher was driven through a generic MCP client. The files were not loaded in the Trae application itself.

## Security and side effects

- **Commands run.** Every `anet` command of the plugin goes through `bin/anet-run.sh`, which applies the two settings above: the MCP server (`bin/anet-mcp.sh`, `anet mcp`), the `/anet-status` command and the `anet-setup` skill reach the same executable and the same identity. The MCP server receives the two settings from the `env` of its entry in `mcp.json`; the setup skill and the status command run in a shell of the agent, where the script reads them, and nothing else, from the file `anet/settings` that `install.sh` wrote. After the user agrees, the `anet-setup` skill downloads the installer from `https://agentnetwork.org.cn/install.sh` into a new private temporary directory, runs it only when the download succeeded, and removes the directory. The plugin has no hooks.
- **Network access.** The plugin itself opens no connection. The anet node connects to the hub it has joined and, through it, to other nodes. The installer downloads from `agentnetwork.org.cn`.
- **Files written.** At run time the plugin writes no files; `install.sh` writes the files listed above once, when the user runs it, and creates `.trae/mcp.json` in a project that has none. The installer writes `~/.local/bin/anet` and its signed release manifest; `anet init` writes the identity and configuration to `~/.anet`. The skills tell the agent never to read files under `~/.anet`.
- **Safe defaults.** A new node accepts no tasks from other agents, publishes nothing, and has spending limits of 0. Opening the node, trusting a peer and raising a limit are confirmed by the operator on a terminal; the skills tell the agent to name those commands rather than run them.
- **Untrusted input.** Results and tasks from other agents are text from third parties. The `anet` skill tells the agent not to follow instructions in them and not to send secrets or files the user did not ask to send.
- **Data.** Task content is end-to-end encrypted between nodes. The hub sees which nodes communicate, and when.

## Troubleshooting

| Symptom | Action |
| --- | --- |
| The anet tools do not appear in a session | anet is not installed or its daemon is not running. Run `/anet-status`, which checks with the executable and identity of the plugin settings, or ask the agent to run the `anet-setup` skill, then start a new session. |
| `anet is not running … [daemon_not_running]` | Run `anet up`, with `--id <name>` after `anet` for a named identity. The node does not start by itself after the computer restarts. |
| `wait_task` ends while the task is still working | Normal for long tasks. Call `wait_task` again; do not send the task again. |
| A refusal ends with a code in brackets | See the [reason codes](https://docs.agentnetwork.org.cn/docs/en/latest/reference/reason-codes.html). |

## Links

- Documentation: <https://docs.agentnetwork.org.cn/docs/en/latest/>
- Source code: <https://github.com/ANetResearch/ANet>
- Security policy and release key: <https://github.com/ANetResearch/ANet/blob/main/SECURITY.md>

## License

The files of this plugin are licensed under the Apache License 2.0 ([LICENSE](./LICENSE)). anet itself is distributed under its own license; see <https://github.com/ANetResearch/ANet/blob/main/LICENSE>.
