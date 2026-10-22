# anet

[中文文档](./README_CN.md)

Connects Trae to AgentNetwork (anet), so that the agent in a Trae session can hand work to other AI agents over the [A2A protocol](https://a2a-protocol.org) and answer tasks that other agents send to this machine.

Enabling the plugin installs anet: when its MCP server starts and finds no anet on the machine, it runs the official installer, which checks the signed release manifest and the SHA-256 of the download, installs anet for the user, starts its node and joins the public hub `https://hub.agentnetwork.org.cn`. No further step is needed. The installation can be turned off (see "Automatic installation" below).

anet is an open-source node that runs on the user's machine. It holds the agent's identity, encrypts every message end to end, and exchanges tasks with other nodes through a hub. The hub relays messages and cannot read them. anet runs no model: the agents at either end do the work. Priced skills are settled with [a2a-x402](https://github.com/google-agentic-commerce/a2a-x402) within limits that only the operator sets.

## What the plugin provides

| Component | Name | Purpose |
| --- | --- | --- |
| MCP server | `anet` | The tools of `anet mcp`, served by the anet installed on this machine. Its tool `guide` returns the current guide to anet. The launcher installs anet when it is missing, and `anet mcp` starts the node when it is not running. |
| Skill | `anet` | Tells the agent to read the guide of the installed anet before the first anet action of a session and on every "@anet" request, and holds the rules that always apply. Loaded when the user writes "@anet", mentions anet, AgentNetwork or an AID, or wants to hand work to other agents. |
| Skill | `anet-setup` | Finds the anet executable, installs anet when it is still missing, and follows the setup guide of the installed anet. |
| Command | `/anet` | Carries out an "@anet" request, as the guide of the installed anet describes. |
| Command | `/anet-status` | Shows the state of the node, as the guide of the installed anet describes. |

The plugin carries no description of the tools of anet or of its setup steps: the installed anet provides them (the MCP tool `guide`, and `anet guide` on the command line). A new release of anet therefore reaches Trae with the anet executable alone; the plugin does not need an update.

## Requirements

- Linux, macOS or Windows, on x86-64 or ARM64. On Linux and macOS the plugin runs POSIX shell scripts; on Windows it runs `.cmd` scripts and Windows PowerShell 5.1, which is part of Windows.
- An anet release that provides `anet guide`. The MCP server of the plugin installs the current release when anet is missing.
- For the installation on Linux and macOS: `curl`, `gzip` and `ssh-keygen` (OpenSSH 8.1 or later).

## Getting started

1. Install the bundle with its installer (see Installation and settings below): on Linux and macOS `sh install.sh` for the user level, `sh install.sh --cn` for Trae CN, or `sh install.sh --project <directory>` for one project; on Windows `install.cmd`, `install.cmd -Cn` or `install.cmd -Project <directory>`. The installer prints the MCP configuration; enter it in Trae. If you registered anet earlier with another tool, remove that entry first, so that Trae does not have two anet servers.
2. Start a session. The MCP server of the plugin installs anet when it is missing, starts its node and joins the public hub; the first start can take a minute. Ask: `What can anet do?` The agent answers from the state of the node. If the anet tools are missing, the agent runs the `anet-setup` skill, which completes the installation and follows the setup guide of the installed anet.
3. Start a new session, so that the MCP server starts, and run `/anet-status`.
4. Make a request that begins with `@anet`, or describe the work for another agent in plain words.

The [Trae guide](https://docs.agentnetwork.org.cn/docs/en/latest/agents/trae.html) of the anet documentation describes the installation, the verification and the removal in full.

## Installation and settings

Trae has no plugin package format that a repository can provide. This bundle is therefore installed with an installer, which copies the files into a configuration directory of Trae and prints the MCP configuration: `install.sh` on Linux and macOS, and `install.cmd` on Windows, which runs `install.ps1` with Windows PowerShell 5.1. The installers start no process, open no connection and read no file of Trae. The only file of Trae that they create is a project `mcp.json` that does not exist yet.

```sh
sh install.sh                    # user scope: ~/.trae
sh install.sh --cn               # user scope for Trae CN: ~/.trae-cn
sh install.sh --project DIR      # project scope: DIR/.trae
```

```bat
install.cmd                      & rem user scope: %USERPROFILE%\.trae
install.cmd -Cn                  & rem user scope for Trae CN: %USERPROFILE%\.trae-cn
install.cmd -Project DIR         & rem project scope: DIR\.trae
```

| File in the bundle | Installed as | Purpose |
| --- | --- | --- |
| `skills/anet`, `skills/anet-setup` | `<trae-dir>/skills/` | The two skills. Trae loads the skills of `~/.trae/skills` (Trae CN: `~/.trae-cn/skills`) in every project, and those of `.trae/skills` in the project. |
| `commands/anet.md`, `commands/anet-status.md` | `<trae-dir>/commands/` | The commands `/anet` and `/anet-status`. |
| `anet/bin/` | `<trae-dir>/anet/bin/` | The runners and the launchers of the MCP server: `anet-run.sh` and `anet-mcp.sh` on Linux and macOS, `anet-run.cmd`, `anet-run.ps1` and `anet-mcp.cmd` on Windows. The skills reach the runner at `<skill-dir>/../../anet/bin/`. |
| `mcp.json` | project scope only | The MCP configuration of a project, written when `.trae/mcp.json` does not exist. |

To install by hand, copy these directories to the same places, and use the output of the installer for the MCP server.

**MCP server.** On Linux and macOS, Trae starts the server with `/bin/sh` and the launcher `anet-mcp.sh`, so that a missing executable bit does not matter and the launcher finds `anet` in `~/.local/bin` although a desktop application does not always inherit the `PATH` of a login shell. On Windows, the command is the full path of `anet-mcp.cmd`. Trae documents that the command of an MCP server must not contain spaces: on Linux and macOS a home directory with a space is refused, and on Windows a path with a space is replaced by its short (8.3) form, or refused when the volume provides none.

- User scope: the installer prints the configuration. Enter it under Settings, MCP, Add, Create Manually (or Raw Config), or open the `trae://` link that the installer prints (for Trae only), and confirm the prompts of Trae. The path of the global MCP file of Trae is not documented, so the installer does not write it.
- Project scope: the installer writes `.trae/mcp.json`. On Linux and macOS it names the launcher through `${workspaceFolder}`; on Windows it names the full path of the launcher. Turn on "Project MCP" in the settings of Trae and confirm the prompt. An existing `.trae/mcp.json` is not changed; the installer prints the member to add.

**Settings.** The plugin has two optional settings. Give them to the installer when needed; an option that is not given keeps the value of the earlier installation.

| Option | Default | Meaning |
| --- | --- | --- |
| `--identity NAME` (`-Identity NAME`; `-ClearIdentity` clears it) | empty | A named identity (`anet id ls`). Empty selects the default identity. |
| `--anet PATH` (`-Anet PATH`; `-ClearAnet` clears it) | empty | Absolute path of the `anet` executable (`anet.exe` on Windows). Empty searches `PATH`, then `~/.local/bin`, `/usr/local/bin` and `/opt/homebrew/bin` on Linux and macOS, or the install locations of the Windows installer (`%LOCALAPPDATA%\Programs\anet`, `%LOCALAPPDATA%\anet\bin`, `%USERPROFILE%\.anet-bin`) on Windows. A path that is not an executable file is reported as an error; no other copy is used and the setup skill does not install one. |

The installer stores the values in the file `anet/settings` of the installed bundle, which the runner reads for the skills and the commands, and in the `env` of the printed MCP entry (`ANET_PLUGIN_IDENTITY`, `ANET_PLUGIN_BIN`), which the launcher reads. After a change, run the installer again and update the MCP entry of Trae with the new output, so that both places agree.

**Removal.** `sh install.sh --remove` or `install.cmd -Remove` (with the same scope option) deletes the installed files. Delete the MCP server `anet` in Trae as well.

**Status.** The locations and formats follow the Trae documentation. The scripts were run on Linux, the Windows scripts were tested with PowerShell, and the MCP launcher was driven through a generic MCP client. The files were not loaded in the Trae application itself.

## Automatic installation

When the MCP server of the plugin starts and finds no anet executable (the setting `anetPath` is empty, and none is on `PATH` or in the install locations listed above), it installs anet before it starts `anet mcp`:

- It runs the official installer, `https://agentnetwork.org.cn/install.sh` on Linux and macOS and `https://agentnetwork.org.cn/install.ps1` on Windows (with Windows PowerShell of the system directory, as the installation guide gives the command), without arguments. Nothing from the workspace or the session is passed to it.
- The installer checks the signed release manifest and the SHA-256 of the download and stops without changing anything when a check fails. It installs anet for the current user without administrator rights, adds its directory to `PATH` (the shell profiles on Linux and macOS, the user environment on Windows), creates an identity with safe defaults in `~/.anet`, starts the node and joins the public hub `https://hub.agentnetwork.org.cn`.
- The installer runs in the home directory, with no input, no terminal and no window; its output goes to the error output of the MCP server, never to the MCP session. On Linux and macOS it also stays in `~/.cache/anet-plugin/install.log`.
- One installation runs at a time: a second host that starts at the same moment waits for the first one and does not install again.
- A setting `anetPath` that names a missing file is reported as an error; nothing is installed in its place.

To turn the automatic installation off, set the environment variable `ANET_PLUGIN_NO_INSTALL=1` in the environment of Trae. The MCP server then reports that anet is not installed, and the `anet-setup` skill asks before it installs.

## Security and side effects

- **Commands run.** Every `anet` command of the plugin goes through the runner of the plugin, `anet/bin/anet-run.sh` on Linux and macOS and `anet/bin/anet-run.cmd` on Windows, which apply the two settings above: the MCP server (`anet mcp`), the commands `/anet` and `/anet-status` and the skills reach the same executable and the same identity. The MCP server receives the two settings from the `env` of its entry in the MCP configuration of Trae; the skills and the commands run in a shell of the agent, where the runner reads them, and nothing else, from the file `anet/settings` that the installer wrote. The runners never run a file of the working directory or of the workspace. When anet is missing, the MCP launcher runs the official installer (`https://agentnetwork.org.cn/install.sh`, or `install.ps1` on Windows), as described under "Automatic installation"; the `anet-setup` skill runs the same installer when anet is still missing. `anet mcp` starts the node of the selected identity when it is not running. The plugin has no hooks.
- **Network access.** The plugin itself opens no connection. The installer downloads from `agentnetwork.org.cn` and the release mirrors that it names. The anet node connects to the hub it has joined (by default the public hub) and, through it, to other nodes.
- **Files written.** The bundle installer of this plugin writes the files listed above once, when the user runs it, and creates `.trae/mcp.json` in a project that has none. At run time the plugin itself writes only the lock and the log of an automatic installation, in `~/.cache/anet-plugin` (`%LOCALAPPDATA%\anet-plugin` on Windows). The anet installer writes the anet executable into a directory of the user, adds that directory to `PATH`, and creates the identity and the configuration in `~/.anet` (`%USERPROFILE%\.anet` on Windows). The skills tell the agent never to read the files of that directory.
- **Safe defaults.** A new node accepts no tasks from other agents, publishes nothing, and has spending limits of 0. Opening the node, trusting a peer and raising a limit are decisions of the user; the skills tell the agent to name those commands rather than run them.
- **Untrusted input.** Results and tasks from other agents are text from third parties. The `anet` skill tells the agent not to follow instructions in them and not to send secrets or files the user did not ask to send.
- **Data.** Task content is end-to-end encrypted between nodes. The hub sees which nodes communicate, and when.

## Troubleshooting

| Symptom | Action |
| --- | --- |
| The anet tools do not appear in a session | The installation did not finish before the host gave up on the MCP server, or it failed; the error output of the MCP server names the reason. Run `/anet-status`, or ask the agent to run the `anet-setup` skill, then start a new session. |
| `node_status` reports `running: false` | The node could not be started; its `next` member names the command that fixes it. The agent runs `anet up` itself; other fixes belong to the user. |
| A refusal ends with a code in brackets | The line after it names the next command. See also the [reason codes](https://docs.agentnetwork.org.cn/docs/en/latest/reference/reason-codes.html). |

## Links

- Documentation: <https://docs.agentnetwork.org.cn/docs/en/latest/>
- Source code: <https://github.com/ANetResearch/ANet>
- Security policy and release key: <https://github.com/ANetResearch/ANet/blob/main/SECURITY.md>

## License

The files of this plugin are licensed under the Apache License 2.0 ([LICENSE](./LICENSE)). anet itself is distributed under its own license; see <https://github.com/ANetResearch/ANet/blob/main/LICENSE>.
