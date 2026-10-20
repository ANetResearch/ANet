# ANet 0.3.6 release notes (draft)

[中文](RELEASE-NOTES-0.3.6.zh-CN.md)

Draft: not released. Release date: to be set.

| Component | Version | Repository |
|---|---|---|
| ANetCore | v0.16.1 (unchanged) | https://github.com/ANetResearch/ANetCore |
| ANet (`anet`) | v0.3.6 | https://github.com/ANetResearch/ANet |
| ANetHub (`anet-hub`) | v0.3.3 (unchanged) | https://github.com/ANetResearch/ANetHub |
| ANetLink | v0.2.0 (unchanged) | https://github.com/ANetResearch/ANetLink |

Documentation: https://docs.agentnetwork.org.cn/docs/

Installing on Linux and macOS: https://docs.agentnetwork.org.cn/docs/en/latest/intro/install.html#intro-install-script

## Upgrading

0.3.6 is a patch release. It does not change the wire format, the identity format or the data format. A 0.3.5 node and a 0.3.6 node work together, and the hubs need no upgrade. Update a node with `anet update` and restart its daemons. On Linux and macOS, the installation command is now the single line `curl --proto '=https' --tlsv1.2 -fsSL https://agentnetwork.org.cn/install.sh | sh`: the installer starts the node and joins `https://hub.agentnetwork.org.cn` itself, so `anet up` and `anet hub-register` no longer follow it. A script or an agent that must not join a hub passes `--no-join`. The section `Upgraded from anet <version>` is now in the installation log, not in the output. `ANET_INSTALL_BASE` now replaces every download source of `anet update` instead of being tried first. See "Upgrading to 0.3.6" in the documentation.

## Changes

### Windows installation
<!-- TODO (Windows installer change): the changes to install.ps1. -->

### Linux and macOS installation
- The installation is one line, and the installer never reads its input:

  ```shell
  curl --proto '=https' --tlsv1.2 -fsSL https://agentnetwork.org.cn/install.sh | sh
  ```

  By default it verifies the release and installs `anet` as `~/.local/bin/anet`, takes over earlier releases, runs `anet init`, starts the node with `anet up` and waits at most 30 seconds until the node answers, and joins `https://hub.agentnetwork.org.cn` with `anet hub-register <hub> --name <host name> --passphrase-later`, which asks nothing; the recovery passphrase is set afterwards with `anet identity protect`. A node that was running is restarted with the new binary. A node that has already joined a hub keeps it unless `--hub` names another one. On a hub that admits by AID, the summary names the `anet-hub admit <AID>` command for the hub's operator, and the join completes on its own once the operator has run it.
- New options: `--no-join` starts the node without joining a hub; `--no-start` neither starts the node nor joins a hub. `--hub URL` and `--name NAME` select the hub and the name of the join. The options `--system`, `--user`, `--prefix`, `--base`, `--shell`, `--without`, `--agents[=LIST]`, `--no-modify-path` and `--help` are unchanged.
- The output is one short line per phase (the download, the takeover of an earlier release, the start of the node) and a summary:

  ```text
  Downloaded anet 0.3.6 for darwin-arm64 from npmmirror (8.6 MB in 2 s)
  Starting the node and joining https://hub.agentnetwork.org.cn

  anet 0.3.6 is installed and running.
    AID   bafyrei...
    Hub   https://hub.agentnetwork.org.cn (joined)
  Run 'anet' to get started.  Details: ~/.anet/logs/install-20261020T151644Z.log
  ```

  When standard error is a terminal, the download is shown with a progress line (bar, megabytes received and expected, speed, remaining time), redrawn about five times a second. The bar is red; `NO_COLOR` removes the colour, and outside a UTF-8 locale the bar is drawn with ASCII characters. Without a terminal, one `Downloaded ...` line takes its place. A takeover adds the lines `Replacing <release>` and `Replaced <release>; its data is kept in <directory>`. Each step left to the user is one `Note:` line with the exact command. With `--no-join`, the hub line reads `Hub   not joined (--no-join)`; with `--no-start`, the summary reads `anet 0.3.6 is installed.` and `Run 'anet up' to get started.`.
- Everything else, including the sources tried, every check, each step of a takeover, the shell profiles that were changed, and the output of `anet init` and `anet hub-register`, is written to `<data directory>/logs/install-<UTC time>.log`. The data directory is `ANET_DATA_DIR`, else `ANET_HOME`, else `~/.anet`; the directory `logs` has mode `0700` and the log mode `0600`. The last line of the output names the log. With `ANET_LANG` beginning with `zh`, the lines are Chinese.
- A failure prints `Error:` with the reason and what to do, then `Nothing was installed or changed.` (or, during a takeover, the steps already carried out), then `Details:` with the log.
- In a terminal, the installer may ask for the password of `sudo` once: when an earlier release (AgentNetwork 1.x, anet 0.1 or 0.2) sits where only the superuser can remove it, for example `/usr/local/bin/anet` owned by `root` or a 1.x npm package under a prefix owned by `root`, when a system unit of AgentNetwork 1.x is to be disabled, or when the command link below needs the superuser. It first prints one line with the reason, for example `Removing AgentNetwork 1.1.5 from /usr/local/bin requires administrator rights; macOS will ask for your password once.`, and then runs `sudo -v`, which reads the password from the terminal and never from the input of the script. The steps that need the superuser then run with `sudo -n` within that session. When `sudo` is declined or fails, the installation continues and the summary has one `Note` line with the command. Without a terminal, as in the tool call of a coding agent or a CI job, nothing is asked and `sudo` is used only as `sudo -n`. `--system` is unchanged and uses `sudo -n` only.
- The binary stays in `~/.local/bin`, where the user can replace it, so that `anet update` needs no administrator rights. When `~/.local/bin` is not on the search path of the shell that runs the installer, `/usr/local/bin` is on it, and a terminal is available, the installer creates the command link `/usr/local/bin/anet` to `~/.local/bin/anet` (with `sudo` when `/usr/local/bin` belongs to `root`, within the same session; an earlier release there is replaced), so that `anet` runs in that terminal at once. Without a terminal, the last line says to run `anet` in a new terminal, or in the current one after `export PATH="$HOME/.local/bin:$PATH"`. The marked block in the shell profiles is written as before, unless `--no-modify-path` is given.
- A daemon of AgentNetwork 1.x that still holds the data directory although its binary is gone, for example because the binary was deleted by hand or because an MCP client started the daemon, is stopped by the installer: first through the shutdown interface of 1.x on `127.0.0.1:3998`, with the API token from the data directory passed in a private `curl` configuration file and never on a command line; then, if the daemon still runs, with `SIGTERM` to the process of `daemon.pid`, only after its executable has been identified as AgentNetwork 1.x. The data is then moved aside as before; from a directory that also holds an identity of 0.3, only the 1.x entries move. A daemon of this release that runs on the identity of 0.3 is never taken for an earlier release and never signalled. The manual `mv` instruction is removed; only a 1.x daemon that cannot be stopped leaves a `Note` line.
- A systemd unit named `anet-daemon` whose `ExecStart` runs a binary of AgentNetwork 1.x (the unit that 1.x prints for its users to install, with `Restart=always`) is disabled before the daemon is stopped: a user unit with `systemctl --user disable --now anet-daemon`, a system unit with `sudo -n systemctl disable --now anet-daemon` within the `sudo` session, or otherwise a `Note` line with the command. Unit files are not deleted. The npm packages of AgentNetwork 1.x register no service and no launchd agent themselves.

### Download sources and `anet update`
- `install.sh` and `anet update` use the same download sources in the same order. npmmirror (`registry.npmmirror.com`; the npm package `@agentnetwork/<anet|anet-shell>-<linux|darwin>-<x64|arm64>`, downloaded as the tarball of the registry, whose directory `package/dl/` holds `release.json`, `release.json.sig` and the `.gz` of the build) and GitHub (`github.com/ANetResearch/ANet/releases/latest/download/<file>`; redirects only to `github.com` and `*.githubusercontent.com`) are asked at the same time with a limit of three seconds and tried fastest first; a source that does not answer is skipped. Then come `registry.npmjs.org` (the same tarball), jsDelivr (`cdn.jsdelivr.net/npm/<package>@latest/dl/<file>`), and last the project hosts `agentnetwork.org.cn/dl` and `hub.agentnetwork.org.cn/dl`. The unpkg route and the per-file route of npmmirror are no longer used.
- `--base URL` and `ANET_INSTALL_BASE` replace every source, for `anet update` too: `ANET_INSTALL_BASE` now overrides the sources instead of being tried before them.
- Trust is unchanged: the signed `release.json` (release key) and the SHA-256 digests of the `.gz` and of the binary decide. A source that does not serve a release (an HTTP error, a time-out, an HTML page, a missing file, a redirect to a host it does not use, a malformed tarball) is passed over; a source that serves a release that fails verification ends the installation or the update. A source whose release is older than the installer's own version, or than the installed `anet`, is a mirror that lags behind and is passed over while another source serves the current release.
- Redirects are followed by the installer itself, at most five, over `https://` only and to the hosts of that source.
- `anet update` reads the npm tarball with bounded sizes, uses only regular files under `package/dl/`, refuses entry names that are absolute or contain `..`, and checks the sha512 integrity of the package metadata when it is present. Its output names the source, for example `source    npmmirror (https://registry.npmmirror.com/@agentnetwork/anet-darwin-arm64)`.

### Continuous integration
- The job `unix-upgrade` runs `install.sh` of the commit with `--no-join` on macOS and Ubuntu over a real installation of AgentNetwork 1.1.15 with its daemon running. It checks that the output has at most 12 lines with the download line, the takeover lines, the summary and the log named last, that nothing is written to standard error, that the log has mode `0600` in a directory of mode `0700` and holds the checks and the takeover, and that a second run reports no takeover.
- A test runs the whole `install.sh`, piped to `sh`, in a temporary home directory against a release signed with a key made for the test and served over TLS by local stand-ins for the download sources. It contacts no hub.

## Known limitations

- The limitations listed for 0.3.5 remain.
