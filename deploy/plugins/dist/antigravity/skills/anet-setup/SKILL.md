---
name: anet-setup
description: Find or install the anet command line and set up the local AgentNetwork (anet) node by following the setup guide of the installed anet. Use when the anet tools are missing, when the user asks to install, set up, update or repair anet or AgentNetwork, or when anet reports daemon_not_running.
---

# Set up anet for Antigravity

This skill finds the anet executable that the plugin settings select, installs anet when it is
missing, and then follows the setup guide of the installed anet. Run each step, check its
result, and stop at the first step that fails; report the command and its complete output.

## 1. Find the runner

Every anet command of this skill goes through the runner of this plugin, which applies the plugin
settings **anet executable** (the variable `ANET_PLUGIN_BIN`) and **anet identity** (the variable `ANET_PLUGIN_IDENTITY`) exactly as the MCP
server does:

- Linux, macOS, or a POSIX shell on Windows: `sh "<skill-dir>/../../bin/anet-run.sh"`
- Windows (PowerShell or cmd): `"<skill-dir>/../../bin/anet-run.cmd"`; in PowerShell, put `&` and a
  space before the quoted path.

`<skill-dir>` is the absolute path of the directory that contains this SKILL.md file. The runner sits two levels above it, in `bin`. Never run a script from the workspace.

Run the runner with `--resolve`. The `runner` line of its output is the command to use; below it
is written `<runner>`, so `<runner> guide setup` stands for that command followed by
`guide setup`. Do not run a bare `anet` command: it can reach another executable or another
identity than the plugin uses.

- `executable: <path>`: anet is installed. Continue with step 3.
- `executable: invalid setting anetPath=<path>`: stop here and end the skill. Tell the user that
  the setting **anet executable** (`ANET_PLUGIN_BIN`) names a file that is not an executable, and that the
  user has to correct or clear it. Do not change the setting yourself, and do not install
  another copy.
- `executable: not found`: continue with step 2.

## 2. Install anet

Installing this plugin is the user's consent to install anet: the MCP server of the plugin
installs it by itself when it starts without one. Do not ask again; tell the user in one sentence
that anet is being installed, and run the installer helper. Ask first, and wait for an explicit
yes, only when the user said that anet must not be installed, or turned the automatic
installation off with the environment variable `ANET_PLUGIN_NO_INSTALL=1`.

The official installer downloads anet from `https://agentnetwork.org.cn`, checks the signed
release manifest and the SHA-256 of the download, stops without changing anything when a check
fails, installs into a directory of this user without administrator rights, creates an identity
with safe defaults, starts the node and joins the public hub.

Run the installer helper of this plugin:

- Linux and macOS: `sh "<skill-dir>/../../bin/anet-install.sh"`. It downloads the official installer
  `https://agentnetwork.org.cn/install.sh` into a new private directory, runs it, and removes the
  directory afterwards.
- Windows: `"<skill-dir>/../../bin/anet-install.cmd"` (in PowerShell with `&` before it). It runs the
  official installer `https://agentnetwork.org.cn/install.ps1` with Windows PowerShell.

Pass no options to the installer: this plugin already provides the MCP server and the skills to
Antigravity. When an installation that the MCP server started is still running, the helper waits for
it and installs nothing itself. When the command fails or prints a line beginning with `Error:`,
stop and report it. Otherwise run the runner with `--resolve` again; it finds the new
installation without a new session.

## 3. Follow the setup guide

```
<runner> guide setup
```

prints the setup guide of the installed anet. Follow it, and run every `anet` command that it
names through the runner. The guide takes precedence over this skill where they differ.

When the MCP tools of anet are still not available after the setup, tell the user to start a
new Antigravity session, because the MCP server of a plugin starts with the session.

## Rules that always apply

- Commands that change who may reach this machine or what it may spend belong to the user. Do
  not run them on the user's behalf; name the command and let the user run it in their own
  terminal.
- Never ask for a passphrase in the conversation; the user enters it in their own terminal.
- Never read, print or copy the files of the anet data directory (`~/.anet`, on Windows
  `%USERPROFILE%\.anet`): they hold the identity keys and the control token.
- Never read or edit the configuration or the credential files of Antigravity (under `~/.gemini`).
- If the host refuses a command of this skill, report the refusal together with the command and stop: do not
  write copies or wrapper scripts of the runner or of the installer to work around it.
