---
name: anet-setup
description: Find or install the anet command line and set up the local AgentNetwork (anet) node by following the setup guide of the installed anet. Use when the anet tools are missing, when the user asks to install, set up, update or repair anet or AgentNetwork, or when anet reports daemon_not_running.
---

# Set up anet for DeepSeek Harness

This skill finds the anet executable that the plugin settings select, installs anet when it is
missing and the user agrees, and then follows the setup guide of the installed anet. Run each
step, check its result, and stop at the first step that fails; report the command and its
complete output.

## 1. Find the runner

Every anet command of this skill goes through the runner of this plugin, which applies the plugin
settings **anet executable** (`anetPath`) and **anet identity** (`identity`) exactly as the MCP
server does:

- Linux, macOS, or a POSIX shell on Windows: `sh "${DSH_BUNDLE_DIR}/bin/anet-run.sh"`
- Windows (PowerShell or cmd): `"${DSH_BUNDLE_DIR}/bin/anet-run.cmd"`; in PowerShell, put `&` and a
  space before the quoted path.

Run the runner with `--resolve`. The `runner` line of its output is the command to use; below it
is written `<runner>`, so `<runner> guide setup` stands for that command followed by
`guide setup`. Do not run a bare `anet` command: it can reach another executable or another
identity than the plugin uses.

- `executable: <path>`: anet is installed. Continue with step 3.
- `executable: invalid setting anetPath=<path>`: stop here and end the skill. Tell the user that
  the plugin setting **anet executable** names a file that is not an executable, and that the
  user has to correct or clear it. Do not change the setting yourself, and do not install
  another copy.
- `executable: not found`: continue with step 2.

## 2. Install anet (only with the user's consent)

Tell the user what will happen and wait for an explicit yes. The official installer downloads
anet from `https://agentnetwork.org.cn`, checks the signed release manifest and the SHA-256 of
the download, stops without changing anything when a check fails, installs into a directory of
this user without administrator rights, and creates an identity with safe defaults.

After consent, run the installer helper of this plugin:

- Linux and macOS: `sh "${DSH_BUNDLE_DIR}/bin/anet-install.sh"`. It downloads the official installer
  `https://agentnetwork.org.cn/install.sh` into a new private directory, runs it, and removes the
  directory afterwards.
- Windows: `"${DSH_BUNDLE_DIR}/bin/anet-install.cmd"` (in PowerShell with `&` before it). It runs the
  official installer `https://agentnetwork.org.cn/install.ps1` with Windows PowerShell.

Pass no options to the installer: this plugin already provides the MCP server and the skills to
DeepSeek Harness. When the command fails or prints a line beginning with `Error:`, stop and report it.
Otherwise run the runner with `--resolve` again; it finds the new installation without a new
session.

## 3. Follow the setup guide

```
<runner> guide setup
```

prints the setup guide of the installed anet. Follow it, and run every `anet` command that it
names through the runner. The guide takes precedence over this skill where they differ.

DeepSeek Harness ends every process that its shell tool started when the session ends, so a node
that the setup guide starts in this session stops with it. Tell the user plainly that the user has
to start the node again in a terminal of their own (the start command that the guide names, with
`--id <name>` after `anet` when the plugin selects a named identity, and with the path of the
executable when the plugin setting anetPath names one), and then has to start a new DeepSeek
Harness session, because the MCP server of a plugin starts with the session and connects only to a
node that is already running. Do not try to detach the node from the session.

## Rules that always apply

- Commands that change who may reach this machine or what it may spend belong to the user. Do
  not run them on the user's behalf; name the command and let the user run it in their own
  terminal.
- Never ask for a passphrase in the conversation; the user enters it in their own terminal.
- Never read, print or copy the files of the anet data directory (`~/.anet`, on Windows
  `%USERPROFILE%\.anet`): they hold the identity keys and the control token.
- Never read or edit the DeepSeek Harness configuration (`~/.dsh`, or the directory named by `DSH_HOME`): it holds the user's
  profiles and model credentials, and the plugin settings in it belong to the user. The plugin passes
  the two plugin settings to the runner in the environment.
