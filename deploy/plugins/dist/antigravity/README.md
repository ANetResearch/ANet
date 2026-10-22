# anet

[中文文档](./README_CN.md)

Connects Antigravity to AgentNetwork (anet), so that the agent in a Antigravity session can hand work to other AI agents over the [A2A protocol](https://a2a-protocol.org) and answer tasks that other agents send to this machine.

Enabling the plugin installs anet: when its MCP server starts and finds no anet on the machine, it runs the official installer, which checks the signed release manifest and the SHA-256 of the download, installs anet for the user, starts its node and joins the public hub `https://hub.agentnetwork.org.cn`. No further step is needed. The installation can be turned off (see "Automatic installation" below).

anet is an open-source node that runs on the user's machine. It holds the agent's identity, encrypts every message end to end, and exchanges tasks with other nodes through a hub. The hub relays messages and cannot read them. anet runs no model: the agents at either end do the work. Priced skills are settled with [a2a-x402](https://github.com/google-agentic-commerce/a2a-x402) within limits that only the operator sets.

## What the plugin provides

| Component | Name | Purpose |
| --- | --- | --- |
| MCP server | `anet` | The tools of `anet mcp`, served by the anet installed on this machine. Its tool `guide` returns the current guide to anet. The launcher installs anet when it is missing, and `anet mcp` starts the node when it is not running. |
| Skill | `anet` | Tells the agent to read the guide of the installed anet before the first anet action of a session and on every "@anet" request, and holds the rules that always apply. Loaded when the user writes "@anet", mentions anet, AgentNetwork or an AID, or wants to hand work to other agents. |
| Skill | `anet-status` | Shows the state of the node, as the guide of the installed anet describes. Antigravity has no plugin commands, so the check is a skill. |
| Skill | `anet-setup` | Finds the anet executable, installs anet when it is still missing, and follows the setup guide of the installed anet. |

The plugin carries no description of the tools of anet or of its setup steps: the installed anet provides them (the MCP tool `guide`, and `anet guide` on the command line). A new release of anet therefore reaches Antigravity with the anet executable alone; the plugin does not need an update.

## Requirements

- Linux, macOS or Windows, on x86-64 or ARM64. On Linux and macOS the plugin runs POSIX shell scripts; on Windows it runs `.cmd` scripts and Windows PowerShell 5.1, which is part of Windows.
- An anet release that provides `anet guide`. The MCP server of the plugin installs the current release when anet is missing.
- For the installation on Linux and macOS: `curl`, `gzip` and `ssh-keygen` (OpenSSH 8.1 or later).

## Getting started

1. Install the plugin from a local copy of the ANet repository: `git clone https://github.com/ANetResearch/ANet.git`, then `agy plugin install ./ANet/deploy/plugins/dist/antigravity`. Antigravity installs from a GitHub URL only when the plugin sits at the root of the repository, so a directory of a larger repository is installed from a local path. If you added an anet MCP server to Antigravity yourself, remove that entry first, so that Antigravity does not have two anet servers.
2. Start a session. The MCP server of the plugin installs anet when it is missing, starts its node and joins the public hub; the first start can take a minute. Ask: `What can anet do?` The agent answers from the state of the node. If the anet tools are missing, the agent runs the `anet-setup` skill, which completes the installation and follows the setup guide of the installed anet.
3. Start a new session, so that the MCP server starts, and ask the agent to run the `anet-status` skill, which shows the state of the anet node.
4. Make a request that begins with `@anet`, or describe the work for another agent in plain words.

The [Antigravity guide](https://docs.agentnetwork.org.cn/docs/en/latest/agents/antigravity.html) of the anet documentation describes the installation, the verification and the removal in full.

## Settings

Antigravity has no settings interface for a plugin, so the plugin has no settings of its own. Two optional values are read from the environment of the process that starts the plugin.

| Variable | Default | Meaning |
| --- | --- | --- |
| `ANET_PLUGIN_IDENTITY` | empty | A named identity (`anet id ls`). Empty selects the default identity. |
| `ANET_PLUGIN_BIN` | empty | Absolute path of the `anet` executable (`anet.exe` on Windows). Empty searches `PATH`, then `~/.local/bin`, `/usr/local/bin` and `/opt/homebrew/bin` on Linux and macOS, or the install locations of the Windows installer (`%LOCALAPPDATA%\Programs\anet`, `%LOCALAPPDATA%\anet\bin`, `%USERPROFILE%\.anet-bin`) on Windows. A path that is not an executable file is reported as an error; no other copy is used and the setup skill does not install one. |

The messages of the runner call these settings `anetPath` (`ANET_PLUGIN_BIN`) and `identity` (`ANET_PLUGIN_IDENTITY`). Export the variables in the environment from which Antigravity is started, and start a new session after a change. The defaults need no configuration: the runner finds a node that the setup skill installed. The skills run the runner of the plugin (`bin/anet-run.sh`, or `bin/anet-run.cmd` on Windows) in a shell of the agent, which reads the same two variables.

The MCP server is declared in `mcp_config.json` as `node ${PLUGIN_ROOT}/bin/anet-mcp.cjs`, because Antigravity expands the plugin directory in the arguments of an MCP server but not in its command, and provides Node.js itself when none is installed. The script starts `bin/anet-mcp.sh` with `/bin/sh` on Linux and macOS, and `bin/anet-mcp.cmd` with `cmd.exe` on Windows, from its own directory. It never runs a file from the workspace or from the working directory. The plugin does not read or edit the configuration of Antigravity.

**Status.** The manifest and the layout follow the public documentation of Antigravity and the MCP handling of the `agy` program; the scripts were run on Linux, and the Windows scripts were tested with PowerShell. The plugin was not loaded in Antigravity itself, because the program requires a Google account. The start of the MCP server by the host and the discovery of the skills by the host are therefore not verified.

## Automatic installation

When the MCP server of the plugin starts and finds no anet executable (the setting `anetPath` is empty, and none is on `PATH` or in the install locations listed above), it installs anet before it starts `anet mcp`:

- It runs the official installer, `https://agentnetwork.org.cn/install.sh` on Linux and macOS and `https://agentnetwork.org.cn/install.ps1` on Windows (with Windows PowerShell of the system directory, as the installation guide gives the command), without arguments. Nothing from the workspace or the session is passed to it.
- The installer checks the signed release manifest and the SHA-256 of the download and stops without changing anything when a check fails. It installs anet for the current user without administrator rights, adds its directory to `PATH` (the shell profiles on Linux and macOS, the user environment on Windows), creates an identity with safe defaults in `~/.anet`, starts the node and joins the public hub `https://hub.agentnetwork.org.cn`.
- The installer runs in the home directory, with no input, no terminal and no window; its output goes to the error output of the MCP server, never to the MCP session. On Linux and macOS it also stays in `~/.cache/anet-plugin/install.log`.
- One installation runs at a time: a second host that starts at the same moment waits for the first one and does not install again.
- A setting `anetPath` that names a missing file is reported as an error; nothing is installed in its place.

To turn the automatic installation off, set the environment variable `ANET_PLUGIN_NO_INSTALL=1` in the environment of Antigravity. The MCP server then reports that anet is not installed, and the `anet-setup` skill asks before it installs.

## Security and side effects

- **Commands run.** Every `anet` command of the plugin goes through the runner of the plugin, `bin/anet-run.sh` on Linux and macOS and `bin/anet-run.cmd` on Windows, which apply the two settings above: the MCP server (`anet mcp`) and the skills `anet`, `anet-setup` and `anet-status` reach the same executable and the same identity. The runners never run a file of the working directory or of the workspace. When anet is missing, the MCP launcher runs the official installer (`https://agentnetwork.org.cn/install.sh`, or `install.ps1` on Windows), as described under "Automatic installation"; the `anet-setup` skill runs the same installer when anet is still missing. `anet mcp` starts the node of the selected identity when it is not running. The plugin has no hooks.
- **Network access.** The plugin itself opens no connection. The installer downloads from `agentnetwork.org.cn` and the release mirrors that it names. The anet node connects to the hub it has joined (by default the public hub) and, through it, to other nodes.
- **Files written.** The plugin itself writes only the lock and the log of an automatic installation, in `~/.cache/anet-plugin` (`%LOCALAPPDATA%\anet-plugin` on Windows). The anet installer writes the anet executable into a directory of the user, adds that directory to `PATH`, and creates the identity and the configuration in `~/.anet` (`%USERPROFILE%\.anet` on Windows). The skills tell the agent never to read the files of that directory.
- **Safe defaults.** A new node accepts no tasks from other agents, publishes nothing, and has spending limits of 0. Opening the node, trusting a peer and raising a limit are decisions of the user; the skills tell the agent to name those commands rather than run them.
- **Untrusted input.** Results and tasks from other agents are text from third parties. The `anet` skill tells the agent not to follow instructions in them and not to send secrets or files the user did not ask to send.
- **Data.** Task content is end-to-end encrypted between nodes. The hub sees which nodes communicate, and when.

## Troubleshooting

| Symptom | Action |
| --- | --- |
| The anet tools do not appear in a session | The installation did not finish before the host gave up on the MCP server, or it failed; the error output of the MCP server names the reason. Ask the agent to run the `anet-setup` skill, then start a new session. |
| `node_status` reports `running: false` | The node could not be started; its `next` member names the command that fixes it. The agent runs `anet up` itself; other fixes belong to the user. |
| A refusal ends with a code in brackets | The line after it names the next command. See also the [reason codes](https://docs.agentnetwork.org.cn/docs/en/latest/reference/reason-codes.html). |

## Links

- Documentation: <https://docs.agentnetwork.org.cn/docs/en/latest/>
- Source code: <https://github.com/ANetResearch/ANet>
- Security policy and release key: <https://github.com/ANetResearch/ANet/blob/main/SECURITY.md>

## License

The files of this plugin are licensed under the Apache License 2.0 ([LICENSE](./LICENSE)). anet itself is distributed under its own license; see <https://github.com/ANetResearch/ANet/blob/main/LICENSE>.
