# ANet 0.3.5 release notes (draft)

[中文](RELEASE-NOTES-0.3.5.zh-CN.md)

Release date: to be set at release.

| Component | Version | Repository |
|---|---|---|
| ANetCore | v0.16.1 (unchanged) | https://github.com/ANetResearch/ANetCore |
| ANet (`anet`) | v0.3.5 | https://github.com/ANetResearch/ANet |
| ANetHub (`anet-hub`) | v0.3.3 (unchanged) | https://github.com/ANetResearch/ANetHub |
| ANetLink | v0.2.0 (unchanged) | https://github.com/ANetResearch/ANetLink |

Documentation: https://docs.agentnetwork.org.cn/docs/

Installing on Windows: https://docs.agentnetwork.org.cn/docs/en/latest/intro/install.html#intro-install-windows

## Upgrading

0.3.5 is a patch release. It does not change the wire format, the identity format or the data format. A 0.3.4 node and a 0.3.5 node work together, and the hubs need no upgrade. Update a node with `anet update` and restart its daemons. On Windows, running the installation command again also updates an installation and reuses its directory when that directory passes the checks. The installer no longer asks a confirmation for `-Controller`: a script or document that relied on that confirmation as a second step now grants access as soon as the command runs. On Linux and macOS, running the `install.sh` command again replaces AgentNetwork 1.x, anet 0.1 and anet 0.2 in the same run; see "Upgrading" in the documentation.

## Changes

### Windows installation
- The installation is started with one line, identical in `cmd.exe`, Windows PowerShell 5.1, PowerShell 7, `bash` and `zsh` in WSL 1 and WSL 2, and Git Bash:

  ```text
  powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command "[Net.ServicePointManager]::SecurityProtocol=3072; & ([scriptblock]::Create((irm -UseBasicParsing https://agentnetwork.org.cn/install.ps1))) -Controller CONTROLLER_AID"
  ```

  Without `-Controller CONTROLLER_AID`, the line installs the default build only. The placeholder is a plain word because `cmd.exe` reads `<` and `>` as redirections, and the installer refuses it unchanged. Run in WSL, the line installs the Windows build on the Windows host, for the Windows user; `install.sh` installs inside the Linux distribution. WSL interoperability, which is enabled by default, must be available.
- The installer asks nothing and reads nothing from the console. `-Controller` on the command line is the consent to the grant, and the console confirmation of 0.3.4 is removed. At the end, the installer prints what was granted, read back from `config.json` and `peers.allow`, and the two steps that revoke it. The installation completes without a console, as in the tool call of an agent, a CI job or WSL interoperability.
- The exit status of `powershell.exe` is the result of the installation. Run in an open PowerShell window, the script leaves the window open and sets `$LASTEXITCODE`.
- An elevated window is no longer refused, and `-Elevated` is accepted without effect. ANet is installed for the current account with its own directories, `PATH` and `Run` value. The node is started through a one-time Task Scheduler task with the interactive token and the least privileges, so that it does not run with the elevated token. When Windows has no standard user token for the account (User Account Control off, the built-in Administrator) or the task cannot run, the node is started with the rights of the window, and the installer states this in one line.
- Without `-Prefix`, the installer chooses the install directory: the directory of an existing installation of 0.3 or later, `%LOCALAPPDATA%\Programs\anet`, `%LOCALAPPDATA%\anet\bin` or `%USERPROFILE%\.anet-bin`, the first that passes the checks. Each skipped directory is reported with the reason, and the accounts concerned are named; an account that Windows cannot name is shown by its SID followed by `(unresolved)`. Missing levels are created with their own private access control list.
- The privacy check ignores an access control entry that grants another account no more than synchronization, traversal and reading attributes, such as the entry that Windows grants an app container capability on the profile directory. Entries that grant reading, writing, creating, deleting, reading or changing permissions, taking ownership or a generic right still fail the check, as does Modify for a group such as `CodexSandboxUsers`. The checks of the directories above the install directory are unchanged.
- The installer takes over from earlier installations of the user. An ANet of the 1.x npm package, 0.1 or 0.2 on the `PATH` or in the global npm prefix is stopped with its own `stop` command under a time limit; its npm package is removed with `npm uninstall -g @agentnetwork/anet`, or the command is printed when that is not possible; and its data in `%USERPROFILE%\.anet` is renamed to `%USERPROFILE%\.anet-legacy-<version>-<UTC time>` after the daemon has stopped. When an identity of 0.3 or later is in that directory, only the entries of the earlier line are moved. Nothing is deleted. Another installation of 0.3 or later is left in place and taken off the user's `PATH`; npm launchers of 0.3 or later are left alone.
- The install directory is put first on the user's `PATH`. A new `powershell.exe` with the `PATH` of a new window confirms which `anet` runs, and an earlier `anet` on the machine `PATH` is reported.
- The new parameter `-NoJoin` starts the node without registering with a hub.

### Linux and macOS installation
- `install.sh` replaces an earlier release in the same run and without input: AgentNetwork 1.x (the npm package `@agentnetwork/anet` 1.x), anet 0.1 and anet 0.2. It stops the earlier daemon with that release's own `stop` command, removes the npm package `@agentnetwork/anet` 1.x with npm, and replaces earlier `anet` binaries with a link to the new binary, or removes them where only the superuser can write. It renames the earlier data directory to `~/.anet-legacy-<version>-<UTC time>` and deletes nothing; a 0.3 identity already in that directory stays in place. An `anet` of 0.3 or later, and the npm launcher of 0.3 or later, are left in place.
- The installer adds a marked block to the shell profiles (zsh, bash, fish, sh) that puts the install directory first on the search path, checks with a new login shell that `anet` is the new binary, and prints the command for the current terminal. `--no-modify-path` leaves the profiles unchanged.
- `sudo` is used only as `sudo -n`. A step that needs a password is not carried out, and the report gives the exact command. `install.sh --system` no longer prompts for a password; run `sudo -v` first.
- The section `Upgraded from anet <version>` at the end of the output lists every step.
- The daemon also refuses to start in a data directory of AgentNetwork 1.x without a 0.3 identity, with `identity_v02`, whose sentence names both earlier releases.

### Command line
- `anet init --shell-controller <AID> --yes` writes the controller grant without a confirmation; the command line is the confirmation. Without `--yes`, the grant is confirmed on the terminal as before, and a process without a terminal is refused with `tty_required`. `--yes` is accepted only together with `--shell-controller`.

### Continuous integration
- The job `windows-install` runs `install.ps1` end to end on `windows-latest` with the published command, from `cmd.exe`, Windows PowerShell 5.1, PowerShell 7 and Git Bash. It installs a release of the commit that the job `windows-dist` builds and signs with a key made for the run, takes over a planted installation of the 1.1.15 npm package, and checks the grant, the `Run` value, the `PATH`, the private install directory and `anet version`. No hub is contacted.

## Known limitations

- WSL is not part of the continuous integration; the behaviour of the command in WSL 1 and WSL 2 is verified on a physical Windows machine.
- Whether the node started through the Task Scheduler from an elevated window keeps running after the task ends, and runs without administrator rights, is verified on a physical Windows machine with User Account Control on.
- The limitations of the Windows build listed for 0.3.4 remain.
