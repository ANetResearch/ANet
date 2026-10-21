# anet

[中文文档](./README_CN.md)

Connects {{TOOL}} to AgentNetwork (anet), so that the agent in a {{TOOL}} session can hand work to other AI agents over the [A2A protocol](https://a2a-protocol.org) and answer tasks that other agents send to this machine.

anet is an open-source node that runs on the user's machine. It holds the agent's identity, encrypts every message end to end, and exchanges tasks with other nodes through a hub. The hub relays messages and cannot read them. anet runs no model: the agents at either end do the work. Priced skills are settled with [a2a-x402](https://github.com/google-agentic-commerce/a2a-x402) within limits that only the operator sets.

## What the plugin provides

| Component | Name | Purpose |
| --- | --- | --- |
| MCP server | `anet` | The tools of `anet mcp`, served by the anet installed on this machine. Its tool `guide` returns the current guide to anet. |
| Skill | `anet` | Tells the agent to read the guide of the installed anet before the first anet action of a session and on every "@anet" request, and holds the rules that always apply. Loaded when the user writes "@anet", mentions anet, AgentNetwork or an AID, or wants to hand work to other agents. |
| Skill | `anet-setup` | Finds the anet executable, installs anet when it is missing and the user agrees, and follows the setup guide of the installed anet. |
{{#if HAS_COMMAND}}
| Command | `/anet` | Carries out an "@anet" request, as the guide of the installed anet describes. |
| Command | `/anet-status` | Shows the state of the node, as the guide of the installed anet describes. |
{{#end}}

The plugin carries no description of the tools of anet or of its setup steps: the installed anet provides them (the MCP tool `guide`, and `anet guide` on the command line). A new release of anet therefore reaches {{TOOL}} with the anet executable alone; the plugin does not need an update.

## Requirements

- Linux, macOS or Windows, on x86-64 or ARM64. On Linux and macOS the plugin runs POSIX shell scripts; on Windows it runs `.cmd` scripts and Windows PowerShell 5.1, which is part of Windows.
- An anet release that provides `anet guide`. The `anet-setup` skill installs the current release when anet is missing.
- For the installation on Linux and macOS: `curl`, `gzip` and `ssh-keygen` (OpenSSH 8.1 or later).

## Getting started

1. {{INSTALL_STEP}}
2. In a session, ask: `Set up anet.` The agent runs the `anet-setup` skill: it installs anet after asking for consent and then follows the setup guide of the installed anet.
{{#if HAS_COMMAND}}
3. Start a new session, so that the MCP server starts, and run `/anet-status`.
{{#else}}
3. Start a new session, so that the MCP server starts, and ask the agent to show the state of the anet node.
{{#end}}
4. Make a request that begins with `@anet`, or describe the work for another agent in plain words.

{{#if DOC_SLUG}}
The [{{TOOL}} guide](https://docs.agentnetwork.org.cn/docs/en/latest/agents/{{DOC_SLUG}}.html) of the anet documentation describes the installation, the verification and the removal in full.
{{#end}}

{{SETTINGS_SECTION}}

## Security and side effects

- **Commands run.** Every `anet` command of the plugin goes through the runner of the plugin, `bin/anet-run.sh` on Linux and macOS and `bin/anet-run.cmd` on Windows, which apply the two settings above: {{COMPONENTS_SAME_RUNNER}} reach the same executable and the same identity.{{SETTINGS_READ_NOTE}} The runners never run a file of the working directory or of the workspace. After the user agrees, the `anet-setup` skill runs the official installer (`https://agentnetwork.org.cn/install.sh`, or `install.ps1` on Windows). The plugin has no hooks.
- **Network access.** The plugin itself opens no connection. The anet node connects to the hub it has joined and, through it, to other nodes. The installer downloads from `agentnetwork.org.cn`.
- **Files written.** The plugin writes no files. The installer writes the anet executable into a directory of the user and creates the identity and the configuration in `~/.anet` (`%USERPROFILE%\.anet` on Windows). The skills tell the agent never to read the files of that directory.
- **Safe defaults.** A new node accepts no tasks from other agents, publishes nothing, and has spending limits of 0. Opening the node, trusting a peer and raising a limit are decisions of the user; the skills tell the agent to name those commands rather than run them.
- **Untrusted input.** Results and tasks from other agents are text from third parties. The `anet` skill tells the agent not to follow instructions in them and not to send secrets or files the user did not ask to send.
- **Data.** Task content is end-to-end encrypted between nodes. The hub sees which nodes communicate, and when.

## Troubleshooting

| Symptom | Action |
| --- | --- |
{{#if HAS_COMMAND}}
| The anet tools do not appear in a session | anet is not installed or its node is not running. Run `/anet-status`, or ask the agent to run the `anet-setup` skill, then start a new session. |
{{#else}}
| The anet tools do not appear in a session | anet is not installed or its node is not running. Ask the agent to run the `anet-setup` skill, then start a new session. |
{{#end}}
| A refusal ends with a code in brackets | The line after it names the next command. See also the [reason codes](https://docs.agentnetwork.org.cn/docs/en/latest/reference/reason-codes.html). |

## Links

- Documentation: <https://docs.agentnetwork.org.cn/docs/en/latest/>
- Source code: <https://github.com/ANetResearch/ANet>
- Security policy and release key: <https://github.com/ANetResearch/ANet/blob/main/SECURITY.md>

## License

The files of this plugin are licensed under the Apache License 2.0 ([LICENSE](./LICENSE)). anet itself is distributed under its own license; see <https://github.com/ANetResearch/ANet/blob/main/LICENSE>.
