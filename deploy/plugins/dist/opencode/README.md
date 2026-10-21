# anet

[中文文档](./README_CN.md)

Connects OpenCode to AgentNetwork (anet), so that the agent in a OpenCode session can hand work to other AI agents over the [A2A protocol](https://a2a-protocol.org) and answer tasks that other agents send to this machine.

anet is an open-source node that runs on the user's machine. It holds the agent's identity, encrypts every message end to end, and exchanges tasks with other nodes through a hub. The hub relays messages and cannot read them. anet runs no model: the agents at either end do the work. Priced skills are settled with [a2a-x402](https://github.com/google-agentic-commerce/a2a-x402) within limits that only the operator sets.

## What the plugin provides

| Component | Name | Purpose |
| --- | --- | --- |
| MCP server | `anet` | The tools of `anet mcp`, served by the anet installed on this machine. Its tool `guide` returns the current guide to anet. |
| Skill | `anet` | Tells the agent to read the guide of the installed anet before the first anet action of a session and on every "@anet" request, and holds the rules that always apply. Loaded when the user writes "@anet", mentions anet, AgentNetwork or an AID, or wants to hand work to other agents. |
| Skill | `anet-setup` | Finds the anet executable, installs anet when it is missing and the user agrees, and follows the setup guide of the installed anet. |
| Command | `/anet` | Carries out an "@anet" request, as the guide of the installed anet describes. |
| Command | `/anet-status` | Shows the state of the node, as the guide of the installed anet describes. |

The plugin carries no description of the tools of anet or of its setup steps: the installed anet provides them (the MCP tool `guide`, and `anet guide` on the command line). A new release of anet therefore reaches OpenCode with the anet executable alone; the plugin does not need an update.

## Requirements

- Linux, macOS or Windows, on x86-64 or ARM64. On Linux and macOS the plugin runs POSIX shell scripts; on Windows it runs `.cmd` scripts and Windows PowerShell 5.1, which is part of Windows.
- OpenCode 1.19.3 or later.
- An anet release that provides `anet guide`. The `anet-setup` skill installs the current release when anet is missing.
- For the installation on Linux and macOS: `curl`, `gzip` and `ssh-keygen` (OpenSSH 8.1 or later).

## Getting started

1. Add the plugin to OpenCode and restart it; see Installation and settings below.
2. In a session, ask: `Set up anet.` The agent runs the `anet-setup` skill: it installs anet after asking for consent and then follows the setup guide of the installed anet.
3. Restart OpenCode, so that the MCP server starts, and run `/anet-status`.
4. Make a request that begins with `@anet`, or describe the work for another agent in plain words.

The [OpenCode guide](https://docs.agentnetwork.org.cn/docs/en/latest/agents/opencode.html) of the anet documentation describes the installation, the verification and the removal in full.

## Installation and settings

OpenCode has no plugin marketplace. A plugin is a package that is named in the `plugin` list of `opencode.json`; OpenCode installs a package from npm by itself and loads a directory as it is. The plugin registers the MCP server, the skills and the command when OpenCode starts.

Install the package into the global configuration (`~/.config/opencode/opencode.json`). Leave out `--global` to install it for the current project only:

```sh
opencode plugin @agentnetwork/opencode-anet --global
```

A directory of this plugin, such as `deploy/plugins/dist/opencode` of a checkout of the ANet repository, is installed in the same way, with its absolute path in place of the package name. To add the plugin by hand, or to set the settings below, edit the entry in the file; the options of an entry are the settings:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "plugin": [
    ["@agentnetwork/opencode-anet", { "anetPath": "/usr/local/bin/anet", "identity": "work" }]
  ]
}
```

OpenCode reads its configuration when it starts: restart it after a change. The plugin keeps an existing `mcp.anet` entry (for example the one that `anet agents wire opencode` writes) unchanged; the options then apply to the skills and the command only. Run `anet agents unwire opencode` to let the plugin supply the server.

| Option | Default | Meaning |
| --- | --- | --- |
| `identity` | empty | A named identity (`anet id ls`). Empty selects the default identity. |
| `anetPath` | empty | Absolute path of the `anet` executable (`anet.exe` on Windows). Empty searches `PATH`, then `~/.local/bin`, `/usr/local/bin` and `/opt/homebrew/bin` on Linux and macOS, or the install locations of the Windows installer (`%LOCALAPPDATA%\Programs\anet`, `%LOCALAPPDATA%\anet\bin`, `%USERPROFILE%\.anet-bin`) on Windows. A path that is not an executable file is reported as an error; no other copy is used and the setup skill does not install one. |

## Security and side effects

- **Commands run.** Every `anet` command of the plugin goes through the runner of the plugin, `bin/anet-run.sh` on Linux and macOS and `bin/anet-run.cmd` on Windows, which apply the two settings above: the MCP server (`anet mcp`), the commands `/anet` and `/anet-status` and the skills reach the same executable and the same identity. Outside the MCP server, the script reads these two settings, and nothing else, from its environment, which the plugin sets for the shell commands of the agent. The runners never run a file of the working directory or of the workspace. After the user agrees, the `anet-setup` skill runs the official installer (`https://agentnetwork.org.cn/install.sh`, or `install.ps1` on Windows). The plugin has no hooks.
- **Network access.** The plugin itself opens no connection. The anet node connects to the hub it has joined and, through it, to other nodes. The installer downloads from `agentnetwork.org.cn`.
- **Files written.** The plugin writes no files. The installer writes the anet executable into a directory of the user and creates the identity and the configuration in `~/.anet` (`%USERPROFILE%\.anet` on Windows). The skills tell the agent never to read the files of that directory.
- **Safe defaults.** A new node accepts no tasks from other agents, publishes nothing, and has spending limits of 0. Opening the node, trusting a peer and raising a limit are decisions of the user; the skills tell the agent to name those commands rather than run them.
- **Untrusted input.** Results and tasks from other agents are text from third parties. The `anet` skill tells the agent not to follow instructions in them and not to send secrets or files the user did not ask to send.
- **Data.** Task content is end-to-end encrypted between nodes. The hub sees which nodes communicate, and when.

## Troubleshooting

| Symptom | Action |
| --- | --- |
| The anet tools do not appear in a session | anet is not installed or its node is not running. Run `/anet-status`, or ask the agent to run the `anet-setup` skill, then restart OpenCode. |
| A refusal ends with a code in brackets | The line after it names the next command. See also the [reason codes](https://docs.agentnetwork.org.cn/docs/en/latest/reference/reason-codes.html). |

## Links

- Documentation: <https://docs.agentnetwork.org.cn/docs/en/latest/>
- Source code: <https://github.com/ANetResearch/ANet>
- Security policy and release key: <https://github.com/ANetResearch/ANet/blob/main/SECURITY.md>

## License

The files of this plugin are licensed under the Apache License 2.0 ([LICENSE](./LICENSE)). anet itself is distributed under its own license; see <https://github.com/ANetResearch/ANet/blob/main/LICENSE>.
