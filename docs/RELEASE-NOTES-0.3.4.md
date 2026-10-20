# ANet 0.3.4 release notes

[中文](RELEASE-NOTES-0.3.4.zh-CN.md)

Release date: 2026-10-20.

| Component | Version | Repository |
|---|---|---|
| ANetCore | v0.16.1 (unchanged) | https://github.com/ANetResearch/ANetCore |
| ANet (`anet`) | v0.3.4 | https://github.com/ANetResearch/ANet |
| ANetHub (`anet-hub`) | v0.3.3 (unchanged) | https://github.com/ANetResearch/ANetHub |
| ANetLink | v0.2.0 (unchanged) | https://github.com/ANetResearch/ANetLink |

Documentation: https://docs.agentnetwork.org.cn/docs/

Installing on Windows: https://docs.agentnetwork.org.cn/docs/en/latest/intro/install.html#intro-install-windows

## Upgrading

0.3.4 is a patch release. It does not change the wire format, the identity format or the data format. A 0.3.3 node and a 0.3.4 node work together, and the hubs need no upgrade. Update a node with `anet update` and restart its daemons; the data directory, the identity and the configuration are kept. An installation from npm is updated with `npm update --global @agentnetwork/anet`.

One check changes on Linux and macOS: `anet doctor` reports the recovery file under `identity.recovery` only when another account can read it, so a recovery file with mode 0400 is no longer reported. On Windows, `anet` is installed with the PowerShell script `install.ps1`.

## Changes

### Windows
- `anet` runs natively on Windows 10 version 1809 or later and on Windows Server 2019 or later, on amd64 and arm64, in the default build and in the build with the `shell` module. WSL2 is not required.
- The PowerShell script `install.ps1` installs `anet` with Windows PowerShell 5.1 or PowerShell 7 and no other tool. The script is signed (`install.ps1.sig`) and carries the SHA-256 digests of the four Windows builds. A downloaded binary must match its digest, and then verifies the signed release manifest itself, before anything is installed. The installer refuses an elevated window unless `-Elevated` is given, checks that the installation directory is private to the user, adds it to the user's `PATH` and starts the node at logon through the `Run` value of the user. With `-Controller <AID>` it installs the build with the `shell` module, gives exactly that AID access to it after one confirmation on the console, and joins `https://hub.agentnetwork.org.cn` unless `-Hub` is given.
- The daemon keeps its data in `%USERPROFILE%\.anet` and its runtime files in `%LOCALAPPDATA%\anet\run`. Private files and directories carry an access control list that admits only the user, `SYSTEM` and `Administrators`. Secrets are private from the moment they are created, and files are opened without following reparse points. `anet up` starts the daemon detached from the console; it runs as the logged-on user and never as a Windows service.
- The `shell` module runs commands with Windows PowerShell by default; `pwsh`, `cmd` and the absolute path of a supported interpreter can be configured. A command runs as a process tree in a job object and ends as a whole. An argument that contains one of the characters `" % ! ^ & | < > ( )`, a carriage return or a line feed is refused, because a batch file would interpret it again.
- `anet update` replaces a running `anet.exe` by renaming it aside, puts it back when a later step fails, and removes the copies that were moved aside on a later run.
- `anet pull` marks received files as downloaded from the Internet (`Zone.Identifier`) and adds the suffix `.txt` to names on which Explorer acts, such as `.lnk`, `.url`, `.scf` and `desktop.ini`.
- Auto-reply has no sandbox on Windows and refuses every run that needs one. It also refuses an agent program that is a batch file.
- Backends on a Unix socket, TCP backends on the same host, the `p2p` module and the ANetLink sockets are refused with a reason, because the node cannot identify the process at the far end. Backends on another host over HTTPS work.
- The installation, shell, sandbox and security pages, the READMEs and `SECURITY.md` describe the Windows build in English and Chinese.

### Distribution
- Each release is published on npm. `@agentnetwork/anet` is a launcher for Node.js 18 or later. Before the first run of a version it verifies the signature of the release manifest and the SHA-256 digests of the archive and of the binary, and it runs the binary from the cache directory of the user. The builds are in the packages `@agentnetwork/anet-<os>-<cpu>` and `@agentnetwork/anet-shell-<os>-<cpu>`, with `win32` for Windows.
- `install.sh` and `anet update` try the npm package of the build through npmmirror, jsDelivr and unpkg after the project hosts. Every file is checked against the signed manifest, whichever host serves it.
- The `anet` plugin is generated from one source for Claude Code, OpenAI Codex, Cursor, CodeBuddy, opencode, DeepSeek Harness, Trae, Google Antigravity, Gemini CLI and ZCode. The catalogues and archives are served below `https://agentnetwork.org.cn/plugins/` and `https://agentnetwork.org.cn/zcode/`. Antigravity and Gemini CLI install from the mirror repository `ANetResearch/anet-plugins`, and opencode installs `@agentnetwork/opencode-anet` from the npm registry. New guides describe CodeBuddy, DeepSeek Harness, Trae, Antigravity, Gemini CLI and ZCode.
- Plugin version 0.1.3 states that `anet` runs on Windows. The launchers of the plugins are POSIX shell scripts and run on Linux and macOS only; on Windows, `anet agents wire` registers the node.

### Command line
- `anet init --shell-controller <AID>` makes the AID the only caller of the `shell` module and adds it to `peers.allow`, after one confirmation on the terminal and while no daemon runs. It requires the build with the `shell` module.
- `anet hub-register <url> --passphrase-later` joins a hub without asking for the recovery passphrase. The passphrase is set afterwards with `anet identity protect`. The option is not combined with `--recovery-file` or `--new-recovery`.
- `anet update --verify-dir <dir>` fetches and writes nothing. It verifies the release manifest in the directory with the release key that is compiled into the binary, and checks that the running binary is the build of that release for its variant and platform: its SHA-256 digest, its size, its version and its modules. With `--installed <version>`, a release older than that version is refused. `install.ps1` uses the command to check the binary that it is about to install.

### Fixes
- `anet doctor` reports the recovery file under `identity.recovery` only when another account can read it. A recovery file with mode 0400 is no longer reported.
- The sentence of `identity.recovery_perm` names the command of the platform that makes the recovery file private: `chmod 600` on Linux and macOS, an `icacls` command on Windows. The code is unchanged.
- Apart from these two points, the download hosts of `anet update` and sentences that now also name the Windows command, the behaviour on Linux and macOS does not change.

## Known limitations

- The Windows build differs from the Linux and macOS builds in the points listed at https://docs.agentnetwork.org.cn/docs/en/latest/reference/known-limitations.html#reference-known-limitations-windows: there is no sandbox for auto-reply; local backends, the `p2p` module and the ANetLink sockets are refused; the node runs only while the user is logged on; auto-reply refuses agent launchers ending in `.cmd` or `.bat`; the private access control lists carry no mandatory integrity label; the plugin launchers do not run; and the binaries carry no Authenticode signature, so that Smart App Control or an endpoint policy may block them.
- The Windows build is tested by unit tests and, in continuous integration on the `windows-latest` runner, by the Windows tests of the packages that the port changes and by the tests of `install.ps1` in Windows PowerShell 5.1 and PowerShell 7. It has not yet been tested on a physical Windows machine. End-to-end installation on a physical Windows machine is verified after publication.
- The steps of the integration guides were not run on Windows. Each guide labels the points that were not tested.
