# ANet 0.3.8 release notes

[中文](RELEASE-NOTES-0.3.8.zh-CN.md)

Release date: not yet released. This file collects the changes while 0.3.8 is prepared.

Documentation: https://docs.agentnetwork.org.cn/docs/

## Changes

### The MCP server brings the node up
- `anet mcp` starts the node of the selected identity when no daemon answers and the data directory is set up. It starts the daemon exactly as `anet up` does (detached, with the same checks, and without sending the control token to a port that another process holds), in the home directory of the user rather than in the working directory of the session, and waits up to 20 seconds for it. Two sessions that start at the same moment start one daemon. Everything it reports goes to standard error; standard output carries only the MCP session.
- When the node cannot run, `anet mcp` serves anyway instead of exiting with status 1. Every tool is listed; `guide` works as before; `node_status` answers `running: false` with the reason code, the sentence and, in `next`, the command that fixes it; every other tool answers with the same reason as a tool error; and the instructions of the server begin with it. The server looks for the daemon again at each call, so the tools work in the same session once the node runs.
- The reasons are existing codes: `daemon_not_running`, `daemon_not_answering`, and the code of a refusal of the data directory or the configuration (for example `config_key_removed`). A data directory without a node gets the new sentence key `daemon_not_running.not_set_up` and the command `anet up` (`anet --id <name> up` for a named identity). `anet mcp` does not create an identity itself: it starts with every session of a coding agent, and a misspelt identity name in a plugin setting must not create and start a new identity.
- `anet mcp` with the `mcp` module turned off in `config.json` still refuses with `module_absent` and exit status 2.
- The guide tells the agent to answer a question about anet itself ("what can anet do") by calling `node_status`, showing the real state of the node and offering concrete next actions, and to run `anet up` when `node_status` names it.

### Plugins for coding agents
- The plugins move to version 0.3.0, their final version. Enabling a plugin is enough to install anet and run its node: when the MCP launcher finds no anet executable and the setting `anetPath` is empty, it runs the official installer without arguments (`install.sh` on Linux and macOS; on Windows the documented `install.ps1` command with Windows PowerShell of the system directory, which also covers ZCode on Windows). The installer checks the signed release manifest and the SHA-256, installs anet for the user, starts the node and joins the public hub.
- The installer runs in the home directory, with its input closed and its output on the error output of the MCP server. On Linux and macOS it runs in a session of its own without a controlling terminal, so it never prompts on the terminal of the host and finishes when the host gives up on a slow start; its log stays in `~/.cache/anet-plugin/install.log`. One installation runs at a time for a user (a lock in `~/.cache/anet-plugin`, or `%LOCALAPPDATA%\anet-plugin` on Windows); a second host waits for it and does not install again.
- `ANET_PLUGIN_NO_INSTALL=1` in the environment of the tool turns the automatic installation off. A configured `anetPath` that is not an executable is still an error, and nothing is installed in its place. Only the MCP launcher installs; the runner, the skills and the commands do not.
- The skills treat the installation of the plugin as the consent of the user: when the anet tools are missing, the agent runs the setup without asking, and a question about anet is answered from the guide and the state of the node rather than from the text of the plugin.
