# AgentNetwork (anet) installer for Windows - https://agentnetwork.org.cn
#
# Installs the prebuilt anet.exe for this machine (Windows 10 1809 or
# Windows Server 2019 and later, x64 or arm64) for the current account, after
# checking it. Runs in Windows PowerShell 5.1 and in PowerShell 7. It asks
# nothing: every decision is made from the parameters, and nothing reads the
# console, so it also runs where no console is visible (an agent's tool call,
# a CI job, WSL).
#
# Usage. The same line runs unchanged in cmd.exe, Windows PowerShell 5.1,
# PowerShell 7, bash or zsh in WSL 1 and WSL 2 (through WSL interoperability,
# which is on by default) and Git Bash. It contains no character that one of
# these shells would interpret. Install the default build, start the node,
# register it at https://hub.agentnetwork.org.cn and start it at every logon:
#
#   powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command "[Net.ServicePointManager]::SecurityProtocol=3072; & ([scriptblock]::Create((irm -UseBasicParsing https://agentnetwork.org.cn/install.ps1)))"
#
# Install the shell build and give the agent CONTROLLER_AID (replace it with
# that agent's AID) the right to run commands on this computer as this user;
# the node is started, registered and started at every logon as above. Giving
# -Controller on the command line is the consent; nothing else is asked:
#
#   powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command "[Net.ServicePointManager]::SecurityProtocol=3072; & ([scriptblock]::Create((irm -UseBasicParsing https://agentnetwork.org.cn/install.ps1))) -Controller CONTROLLER_AID"
#
# Run inside WSL, the line installs the Windows build on the Windows host, for
# the Windows user: that is the computer the controller is given. install.sh
# installs anet inside the Linux distribution instead.
#
# The exit status of powershell.exe is the result: 0 when everything was
# done; 3 when anet is installed but the installation stopped before its
# last step and the commands that complete it were printed (the data of an
# earlier anet could not be moved, or the node could not be started with
# standard user rights from an elevated window); 1 for a refusal or a
# failure before anything was installed; otherwise the exit status of the
# anet command that failed. In a window that was not started for this one
# command (the script pasted into an open PowerShell window), the script
# does not end the window; $LASTEXITCODE holds the result.
#
# Parameters:
#   -Controller AID  Install the shell build and give the agent with this AID
#                    the right to run commands on this machine as this user.
#   -Shell           Install the shell build without giving anyone access.
#   -Agents          After installing, give the coding agents found on this
#                    computer the tools and guide of anet (anet agents wire
#                    --all); every file it edits is backed up first.
#   -AgentList LIST  As -Agents, for these agents only (comma-separated, such
#                    as claude,codex). The names: claude, codex, cursor,
#                    opencode, hermes, workbuddy, trae (Trae and Trae CN) and
#                    zcode (through the zcode command-line program).
#   -Hub URL         Join this hub instead of https://hub.agentnetwork.org.cn.
#   -NoJoin          Start the node, but do not register it at a hub; register
#                    later with anet hub-register.
#   -NoStart         Neither start the node nor register it; start it later
#                    with anet up.
#   -Name NAME       The name to join under (default: the computer name).
#   -Prefix DIR      Install into DIR instead of the directory the installer
#                    chooses. No directory above DIR may let another account
#                    rename it or change its access control list; DIR must not
#                    contain ; or %.
#   -Without LIST    Turn these modules off in config.json.
#   -Base URL|DIR    Download base, https only, or a local copy of a release
#                    directory; replaces the download sources.
#   -NoAutostart     Do not start the node at logon.
#   -NoPath          Do not put the install directory on the user's PATH.
#   -Elevated        Accepted for compatibility; it changes nothing.
#
# DOWNLOAD SOURCES. This script installs exactly the release it was published
# with (its pins, below), so every source is asked for that version. The
# npm mirror of China (registry.npmmirror.com, the npm package of the build
# as a tarball) and the GitHub release of the project are probed in
# parallel, for at most 3 seconds each and 4 seconds in all; those that
# answered are tried first, the fastest first; then the npm registry
# (registry.npmjs.org), jsDelivr, and last the project's hosts
# (agentnetwork.org.cn and hub.agentnetwork.org.cn, under /dl/). A source
# that does not serve the release (no answer, an HTTP error, a page instead
# of a file, a missing file, another version) is passed over for the next.
# A source decides only where the bytes come from: what it serves is
# checked as below, and a file that fails a check ends the installation.
# -Base, or else ANET_INSTALL_BASE, replaces the sources.
#
# OUTPUT. While it works, the script prints one line per phase, and on an
# interactive console the download as one line that it updates (bar, size,
# speed, time left); NO_COLOR turns its colours off. At the end it prints a
# short summary and, last, the line Details: with the log of the run, which
# holds everything else (the sources tried, each check, every takeover step,
# the output of anet init, anet doctor and anet hub-register). The log is
# <data directory>\logs\install-<UTC time>.log, in a directory with the
# private access control list; when the data directory is not ready (a
# refusal, or data of an earlier anet left in it), %TEMP%\anet-install\
# instead. With ANET_LANG=zh..., the summary is in Chinese.
#
# THE INSTALL DIRECTORY, without -Prefix: the first of these that passes the
# checks below; each one skipped is reported in one line, with the reason and
# the accounts it names:
#
#   1. the directory of an anet 0.3 or later already installed for this
#      user (on the user's PATH or in the Run value below), when it holds
#      nothing but the files of that installation;
#   2. %LOCALAPPDATA%\Programs\anet;
#   3. %LOCALAPPDATA%\anet\bin (beside the node's runtime directory
#      %LOCALAPPDATA%\anet\run);
#   4. %USERPROFILE%\.anet-bin.
#
# A directory passes when no directory above it lets another account rename
# it or change its access control list, and it (or, when it does not exist
# yet, the nearest directory above it that exists) is private to this user:
# owned by this user, SYSTEM or Administrators, and granting no other account
# more than SYNCHRONIZE, FILE_TRAVERSE and FILE_READ_ATTRIBUTES. A directory
# the installer creates gets an access control list of its own: full control
# for this user, SYSTEM and Administrators, and nothing else.
#
# AN ELEVATED WINDOW: anet is installed for the current account all the same
# (per-user directories, the user's PATH and Run value). While Windows has a
# standard token for this account (User Account Control on), nothing that is
# already on this computer is run from the window, since the window has more
# rights than the account's programs: an earlier anet, npm and the stop
# command of an earlier anet are not run, and the commands are printed
# instead; the node is started with the standard token through a one-time
# task of the Task Scheduler, and when that fails it is not started at all:
# it starts at the next logon through the Run value, the commands to start
# it now are printed, and the exit status is 3. An elevated window of an
# account other than the one logged on to the session (another account's
# credentials given to the User Account Control prompt) is refused before
# anything is changed. When Windows has no standard token for the account
# (User Account Control off, the built-in Administrator), the window has the
# rights the account always has, and the node runs with them, said in one
# line.
#
# EARLIER ANET INSTALLATIONS of this user are taken over: an anet.exe found
# on the PATH is run only when it carries the Go module path of AgentNetwork
# 1.x (ChatChatTech/AgentNetwork) or of this project; such an anet of the
# earlier line, or its npm package @agentnetwork/anet 1.x in the global npm
# prefix, is stopped with its own stop command. A daemon of AgentNetwork 1.x
# that still holds the data directory (the process of its daemon.pid, or the
# process that listens on 127.0.0.1:3998, when its program carries the
# module path of AgentNetwork 1.x) is then asked to shut down over its local
# API, with the token from its data directory, and, when it has not exited
# within 10 seconds, ended by its process ID; no other process is ever
# ended, and none by name. A daemon of anet 0.3 or later never counts as
# one of the earlier line. Then its data in %USERPROFILE%\.anet is renamed
# aside (never deleted; a directory whose identity file is this version's
# keeps it, and only the 1.x entries move), and only then is the npm
# package removed with npm uninstall -g @agentnetwork/anet; a data directory
# of an earlier anet is moved the same way when no earlier installation was
# found. When the data cannot be moved (the daemon could not be stopped, or
# the move failed), the installation stops before anet init with exit
# status 3 and the exact commands, and the npm package stays. Another anet
# 0.3 or later is left in
# place; its directory is taken off the user's PATH only when it holds
# nothing else. npm launchers of 0.3 or later, and files named anet.exe that
# are not a program of either project, are left alone and never run.
#
# WHAT IS CHECKED, and every check stops the install before anything is
# installed or changed:
#
#   1. This script carries the release version and the SHA-256 and size of
#      the four Windows builds of that release, and of the binary inside
#      each (the release pins, written when the release is built). The
#      downloaded .gz and the binary inside it must match them exactly; a
#      .gz that does not ends the installation.
#   2. The pinned binary then verifies the signed release manifest itself
#      (anet update --verify-dir), with the release key compiled into it:
#      release.json is signed by the anet release key (namespace
#      anet-release@agentnetwork.org.cn), it has not expired, it names this
#      binary by its SHA-256 as the build of its variant for this platform,
#      the version and the module set the binary reports are the ones it
#      names (the default build must not contain the shell module), and it
#      is not older than the anet already installed.
#
# There is no parameter to skip any of these. The manifest and its signature
# are kept beside the installed binary (anet.exe.release.json and
# anet.exe.release.json.sig) so that `anet doctor` can check them, and the
# binary, again later.
#
# WHAT IS NOT: this script itself, when it is run straight from the
# download host. That trusts the host and its TLS certificate. To not trust
# the host, check the script before running it. The release public key is
# published in SECURITY.md and README.md of https://github.com/ANetResearch/ANet:
#
#   anet-release@agentnetwork.org.cn namespaces="anet-release@agentnetwork.org.cn,anet-official@agentnetwork.org.cn" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAMTUwPlzeKmU7qr+eicaQVuxmltc5mY1sTmwfhIJJEL
#
# Verify-first form, in PowerShell (ssh-keygen -Y verify needs OpenSSH 8.1 or
# later; ssh -V shows the version). It writes the line above into
# allowed_signers as UTF-8 without a byte order mark, checks the signature in
# cmd.exe (PowerShell has no input redirection), and stops before running the
# script when the check fails:
#
#   Invoke-WebRequest -UseBasicParsing https://agentnetwork.org.cn/install.ps1 -OutFile install.ps1
#   Invoke-WebRequest -UseBasicParsing https://agentnetwork.org.cn/install.ps1.sig -OutFile install.ps1.sig
#   [IO.File]::WriteAllText((Join-Path $PWD.ProviderPath 'allowed_signers'), 'anet-release@agentnetwork.org.cn namespaces="anet-release@agentnetwork.org.cn,anet-official@agentnetwork.org.cn" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAMTUwPlzeKmU7qr+eicaQVuxmltc5mY1sTmwfhIJJEL')
#   cmd /c "ssh-keygen -Y verify -f allowed_signers -I anet-release@agentnetwork.org.cn -n anet-release@agentnetwork.org.cn -s install.ps1.sig < install.ps1"; if ($LASTEXITCODE -ne 0) { throw 'install.ps1 did not verify' }; & ([scriptblock]::Create((Get-Content -Raw .\install.ps1))) -Controller CONTROLLER_AID
#
# With an older OpenSSH, run the check on another machine with OpenSSH 8.1 or
# later (the same command in cmd.exe and in a POSIX shell), and run the
# script only after it prints Good "anet-release@agentnetwork.org.cn" signature:
#
#   ssh-keygen -Y verify -f allowed_signers -I anet-release@agentnetwork.org.cn -n anet-release@agentnetwork.org.cn -s install.ps1.sig < install.ps1
#
# The default build, and what it can do (the same text as internal/a2ashape
# InstallHeader; a test keeps the two equal):
#
#   The default binary does not run remote commands (the shell module is not in it). Once the operator configures auto-reply, it runs a local agent for peers, and untrusted peers only inside a sandbox. If you do not want this feature: --without autoreply keeps it from ever starting; to leave the code out of the binary, build it yourself with build.sh --without autoreply as described at https://docs.agentnetwork.org.cn/docs/en/latest/topics/modules.html.
#
# On Windows there is no sandbox: auto-reply for untrusted peers is refused.
# -Without autoreply here is --without autoreply above.
#
# WHAT -Controller OPENS, exhaustively: the shell build of anet; in
# config.json, modules.shell with allow = [AID], allow_arbitrary = true and
# shell = powershell; the AID in peers.allow. Everything else is what every
# installation does: the defaults of `anet init` (inbound closed, no public
# capabilities, spending limits 0); the hub registration (not with -NoJoin
# or -NoStart); the Run value AgentNetwork under
# HKCU\Software\Microsoft\Windows\CurrentVersion\Run; the user's PATH. Nothing
# else: no auto-reply, no other peer, no public capability, no listener beyond
# the node's loopback ones. The summary names the agent and how to revoke
# its access; the log holds what was written.
#
# Everything below is a parameter block, the release pins and function
# definitions; the last line calls Install-Anet. A download cut short does
# not parse, and runs nothing. The script ends the PowerShell process only
# when that process was started for this one command (powershell.exe
# -Command, as in the lines above), with the result as its exit status.

param(
  [string]$Controller, [switch]$Shell, [string]$Hub, [string]$Name, [string]$Prefix,
  [string[]]$Without, [string]$Base, [switch]$NoAutostart, [switch]$NoPath, [switch]$Elevated,
  [switch]$NoJoin, [switch]$NoStart, [switch]$Agents, [string[]]$AgentList
)

# BEGIN RELEASE PINS (written by deploy/release/build-release.sh; do not edit)
$AnetRelease = $null
# END RELEASE PINS

# The fingerprint of the release key, which the binary reports after it has
# verified the manifest with the key compiled into it.
$AnetReleaseKeyFingerprint = 'SHA256:/4FMm/jgZcBII3z3O3r81Y8SxFfugdLRu3zj2gnclD4'

# The output of a run. While it works, the script prints one short line per
# phase (Write-AnetSay); everything else goes to the log of the run
# (Write-AnetLine), which is written at the end (Save-AnetLog) and named on
# the last line. Lines holds the log; Stamp is the UTC time of the run.
$AnetOut = @{ Lines = $null; Stamp = '' }

# Write-AnetLine adds a line to the log of the run (and prints nothing).
function Write-AnetLine {
  param([string]$Text)
  if ($null -ne $AnetOut.Lines) { $AnetOut.Lines.Add($Text) }
}

# Write-AnetSay prints a line on the console and adds it to the log.
function Write-AnetSay {
  param([string]$Text)
  Write-Host $Text
  Write-AnetLine $Text
}

# Start-AnetLog starts the log of a run.
function Start-AnetLog {
  $AnetOut.Lines = New-Object System.Collections.Generic.List[string]
  $AnetOut.Stamp = [DateTime]::UtcNow.ToString("yyyyMMdd'T'HHmmss'Z'", [Globalization.CultureInfo]::InvariantCulture)
}

# Test-AnetChinese: the summary is in Chinese when ANET_LANG begins with zh,
# as for anet itself; in English otherwise.
function Test-AnetChinese {
  return ([string]$env:ANET_LANG).ToLowerInvariant().StartsWith('zh')
}

# The lines of the summary, in English and in Chinese. The Chinese text is
# written with \u escapes, so that this file stays ASCII; Get-AnetText
# decodes it. {name} is replaced by the argument of that name.
$AnetMessages = @{
  dl_label = @('anet {version} from {source}', 'anet {version}\uff08{source}\uff09')
  dl_done = @('Downloaded anet {version} for {platform} from {source} ({size} MB in {secs} s)', '\u5df2\u4ece {source} \u4e0b\u8f7d anet {version}\uff08{platform}\uff0c{size} MB\uff0c\u7528\u65f6 {secs} \u79d2\uff09')
  dl_dir = @('Read anet {version} for {platform} from {source}', '\u5df2\u4ece {source} \u8bfb\u53d6 anet {version}\uff08{platform}\uff09')
  dl_skip = @('{source} did not serve anet {version} ({why}); trying the next source', '{source} \u672a\u63d0\u4f9b anet {version}\uff08{why}\uff09\uff1b\u6539\u7528\u4e0b\u4e00\u4e2a\u4e0b\u8f7d\u6e90')
  installed = @('Installed {exe} (signature verified)', '\u5df2\u5b89\u88c5 {exe}\uff08\u7b7e\u540d\u5df2\u9a8c\u8bc1\uff09')
  installed_up = @('Installed {exe} (signature verified; upgraded from {previous})', '\u5df2\u5b89\u88c5 {exe}\uff08\u7b7e\u540d\u5df2\u9a8c\u8bc1\uff1b\u7531 {previous} \u5347\u7ea7\uff09')
  running = @('anet {version} is installed and running.', 'anet {version} \u5df2\u5b89\u88c5\u5e76\u6b63\u5728\u8fd0\u884c\u3002')
  installed_only = @('anet {version} is installed.', 'anet {version} \u5df2\u5b89\u88c5\u3002')
  hub_joined = @('  Hub   {hub} (joined)', '  Hub   {hub}\uff08\u5df2\u52a0\u5165\uff09')
  hub_pending = @('  Hub   {hub} (waiting for admission; the operator runs: anet-hub admit {aid})', '  Hub   {hub}\uff08\u7b49\u5f85\u51c6\u5165\uff1b\u8bf7\u8fd0\u8425\u8005\u8fd0\u884c\uff1aanet-hub admit {aid}\uff09')
  hub_retrying = @('  Hub   {hub} (not reachable yet; the node keeps trying)', '  Hub   {hub}\uff08\u6682\u65f6\u65e0\u6cd5\u8fde\u63a5\uff1b\u8282\u70b9\u4f1a\u7ee7\u7eed\u91cd\u8bd5\uff09')
  hub_sent = @('  Hub   {hub} (registration sent)', '  Hub   {hub}\uff08\u5df2\u63d0\u4ea4\u6ce8\u518c\uff09')
  takeover_kept = @('Replaced {label}; its data is kept in {aside}', '\u5df2\u66ff\u6362 {label}\uff1b\u5176\u6570\u636e\u4fdd\u7559\u5728 {aside}')
  takeover = @('Replaced {label}.', '\u5df2\u66ff\u6362 {label}\u3002')
  remote = @('Remote control: allowed for {aid} only; revoke with: anet peers remove {aid}', '\u8fdc\u7a0b\u63a7\u5236\uff1a\u4ec5\u5141\u8bb8 {aid}\uff1b\u64a4\u9500\uff1aanet peers remove {aid}')
  run_here = @('Run ''anet'' to get started.', '\u8fd0\u884c ''anet'' \u5f00\u59cb\u4f7f\u7528\u3002')
  run_new = @('Run ''anet'' in a new window to get started.', '\u5728\u65b0\u7a97\u53e3\u4e2d\u8fd0\u884c ''anet'' \u5f00\u59cb\u4f7f\u7528\u3002')
  run_path = @('Run "{exe}" to get started; it is not on the PATH (-NoPath).', '\u8fd0\u884c "{exe}" \u5f00\u59cb\u4f7f\u7528\uff1b\u5b83\u4e0d\u5728 PATH \u4e2d\uff08-NoPath\uff09\u3002')
  details = @('Details: {path}', '\u8be6\u7ec6\u4fe1\u606f\uff1a{path}')
  details_none = @('Details: the log could not be written ({why})', '\u8be6\u7ec6\u4fe1\u606f\uff1a\u65e0\u6cd5\u5199\u5165\u65e5\u5fd7\uff08{why}\uff09')
  note = @('Note: {text}', '\u6ce8\u610f\uff1a{text}')
  note_nostart = @('the node was not started (-NoStart); start it with: anet up', '\u8282\u70b9\u672a\u542f\u52a8\uff08-NoStart\uff09\uff1b\u542f\u52a8\u547d\u4ee4\uff1aanet up')
  note_old_daemon = @('a node of anet {old} is still running; restart it with: anet stop --all, then anet up --all', 'anet {old} \u7684\u8282\u70b9\u4ecd\u5728\u8fd0\u884c\uff1b\u91cd\u542f\u547d\u4ee4\uff1aanet stop --all\uff0c\u7136\u540e anet up --all')
  note_path_other = @('a new window runs {other} for anet, because its directory comes first on the machine PATH; remove it, or run "{exe}"', '\u65b0\u7a97\u53e3\u4e2d\u7684 anet \u662f {other}\uff0c\u56e0\u4e3a\u5176\u76ee\u5f55\u5728\u7cfb\u7edf PATH \u4e2d\u9760\u524d\uff1b\u8bf7\u5220\u9664\u5b83\uff0c\u6216\u8fd0\u884c "{exe}"')
  note_npm = @('the npm package @agentnetwork/anet {version} is still installed; remove it with: {command}', 'npm \u5305 @agentnetwork/anet {version} \u4ecd\u5df2\u5b89\u88c5\uff1b\u5220\u9664\u547d\u4ee4\uff1a{command}')
  note_legacy_exe = @('{path} (AgentNetwork {version}) is no longer used; delete it with: {command}', '{path}\uff08AgentNetwork {version}\uff09\u5df2\u4e0d\u518d\u4f7f\u7528\uff1b\u5220\u9664\u547d\u4ee4\uff1a{command}')
  note_run = @('run in a normal window of this account: {command}', '\u8bf7\u5728\u6b64\u8d26\u6237\u7684\u666e\u901a\u7a97\u53e3\u4e2d\u8fd0\u884c\uff1a{command}')
  note_uac_off = @('the node runs with the rights of this elevated window, because User Account Control is off', '\u8282\u70b9\u4ee5\u6b64\u63d0\u5347\u6743\u9650\u7a97\u53e3\u7684\u6743\u9650\u8fd0\u884c\uff0c\u56e0\u4e3a\u7528\u6237\u8d26\u6237\u63a7\u5236\u5df2\u5173\u95ed')
  note_builtin_admin = @('the node runs with the rights of this elevated window, because this is the built-in Administrator account', '\u8282\u70b9\u4ee5\u6b64\u63d0\u5347\u6743\u9650\u7a97\u53e3\u7684\u6743\u9650\u8fd0\u884c\uff0c\u56e0\u4e3a\u8fd9\u662f\u5185\u7f6e Administrator \u8d26\u6237')
  note_record = @('the release record could not be written beside {exe}; anet doctor reports the signature as unknown', '\u65e0\u6cd5\u5728 {exe} \u65c1\u5199\u5165\u53d1\u5e03\u8bb0\u5f55\uff1banet doctor \u4f1a\u5c06\u7b7e\u540d\u62a5\u544a\u4e3a\u672a\u77e5')
  note_agents = @('anet agents wire did not complete (see the log); run it: anet agents wire {list}', 'anet agents wire \u672a\u5b8c\u6210\uff08\u89c1\u65e5\u5fd7\uff09\uff1b\u8bf7\u8fd0\u884c\uff1aanet agents wire {list}')
  note_grant = @('the grant could not be read back from {config}; check it with: anet doctor', '\u65e0\u6cd5\u4ece {config} \u8bfb\u56de\u6388\u6743\uff1b\u8bf7\u7528 anet doctor \u68c0\u67e5')
  error = @('Error: {text}', '\u9519\u8bef\uff1a{text}')
  nothing = @('Nothing was installed or changed.', '\u672a\u5b89\u88c5\u6216\u66f4\u6539\u4efb\u4f55\u5185\u5bb9\u3002')
  stopped_at = @('anet {version} is installed at {exe}, but the installation stopped before its last step.', 'anet {version} \u5df2\u5b89\u88c5\u5728 {exe}\uff0c\u4f46\u5b89\u88c5\u5728\u6700\u540e\u4e00\u6b65\u4e4b\u524d\u505c\u6b62\u3002')
  steps = @('Run in a normal window of this account, in this order:', '\u8bf7\u5728\u6b64\u8d26\u6237\u7684\u666e\u901a\u7a97\u53e3\u4e2d\u6309\u4ee5\u4e0b\u987a\u5e8f\u8fd0\u884c\uff1a')
  held = @('{dir} still holds the data of {label}, whose daemon could not be stopped; nothing in it was changed', '{dir} \u4ecd\u4fdd\u5b58\u7740 {label} \u7684\u6570\u636e\uff0c\u4e14\u5176\u5b88\u62a4\u8fdb\u7a0b\u65e0\u6cd5\u505c\u6b62\uff1b\u5176\u4e2d\u7684\u5185\u5bb9\u672a\u4f5c\u4efb\u4f55\u66f4\u6539')
  deferred = @('the node was not started: it is not started with the administrator rights of this window, and starting it with standard user rights failed ({why})', '\u8282\u70b9\u672a\u542f\u52a8\uff1a\u4e0d\u4f1a\u4ee5\u6b64\u7a97\u53e3\u7684\u7ba1\u7406\u5458\u6743\u9650\u542f\u52a8\uff0c\u800c\u4ee5\u6807\u51c6\u7528\u6237\u6743\u9650\u542f\u52a8\u5931\u8d25\uff08{why}\uff09')
  deferred_logon = @('It starts at the next logon.', '\u5b83\u5c06\u5728\u4e0b\u6b21\u767b\u5f55\u65f6\u542f\u52a8\u3002')
  join_refused = @('the hub {hub} refused the registration: {reason}', 'hub {hub} \u62d2\u7edd\u4e86\u6ce8\u518c\uff1a{reason}')
  start_failed = @('the node did not start (exit status {code})', '\u8282\u70b9\u672a\u80fd\u542f\u52a8\uff08\u9000\u51fa\u72b6\u6001 {code}\uff09')
  init_failed = @('anet init did not complete (exit status {code})', 'anet init \u672a\u5b8c\u6210\uff08\u9000\u51fa\u72b6\u6001 {code}\uff09')
}

# Get-AnetText is the message $Key in the language of the run, with its
# arguments put in.
function Get-AnetText {
  param([Parameter(Mandatory = $true)][string]$Key, [hashtable]$Arg = @{})
  $pair = $AnetMessages[$Key]
  if ($null -eq $pair) { throw ('no message ' + $Key) }
  $text = [string]$pair[0]
  if (Test-AnetChinese) { $text = [regex]::Unescape([string]$pair[1]) }
  foreach ($k in $Arg.Keys) { $text = $text.Replace('{' + $k + '}', [string]$Arg[$k]) }
  return $text
}

# Test-AnetWindows: this is Windows (the tests also run on other systems).
function Test-AnetWindows {
  return ([Environment]::OSVersion.Platform -eq [PlatformID]::Win32NT)
}

# New-AnetLogDirectory makes sure $Dir exists and, on Windows, is private
# to this user: a directory it creates gets the private access control
# list (Set-AnetPrivateAcl); an existing one must pass Get-AnetPrivacyProblem.
function New-AnetLogDirectory {
  param([Parameter(Mandatory = $true)][string]$Dir)
  if (-not (Test-Path -LiteralPath $Dir -PathType Container)) {
    $null = [IO.Directory]::CreateDirectory($Dir)
    if (Test-AnetWindows) { Set-AnetPrivateAcl -Path $Dir }
  }
  if (Test-AnetWindows) {
    $why = Get-AnetPrivacyProblem -Path $Dir
    if ($null -ne $why) { throw ($Dir + ': ' + $why) }
  }
}

# Save-AnetLog writes the log of the run to <data directory>\logs when the
# data directory is this version's (Get-AnetDataDirKind current), else to
# %TEMP%\anet-install, as install-<UTC time>.log, a new file. Returns its
# path, or '' with the reason in Why.
function Save-AnetLog {
  param([string]$DataDir, [hashtable]$Why = @{})
  $Why['Text'] = ''
  if ($null -eq $AnetOut.Lines) { $Why['Text'] = 'no log was started'; return '' }
  $dirs = New-Object System.Collections.Generic.List[string]
  if ($DataDir -ne '' -and (Get-AnetDataDirKind -Root $DataDir) -eq 'current') { $dirs.Add((Join-Path $DataDir 'logs')) }
  $dirs.Add((Join-Path ([IO.Path]::GetTempPath()) 'anet-install'))
  $text = ($AnetOut.Lines.ToArray() -join "`r`n") + "`r`n"
  $bytes = (New-Object System.Text.UTF8Encoding -ArgumentList $false).GetBytes($text)
  foreach ($d in $dirs) {
    try {
      New-AnetLogDirectory -Dir $d
      $path = Join-Path $d ('install-' + $AnetOut.Stamp + '.log')
      $n = 1
      while (Test-Path -LiteralPath $path) { $n++; $path = Join-Path $d ('install-' + $AnetOut.Stamp + '-' + $n + '.log') }
      $fs = New-Object System.IO.FileStream -ArgumentList $path, ([IO.FileMode]::CreateNew), ([IO.FileAccess]::Write), ([IO.FileShare]::None)
      try { $fs.Write($bytes, 0, $bytes.Length) } finally { $fs.Dispose() }
      return $path
    } catch {
      $Why['Text'] = $_.Exception.Message
    }
  }
  return ''
}

# New-AnetFailure is the exception every refusal throws: its message is the
# reason, and Code the exit status the script ends with.
function New-AnetFailure {
  param([string]$Message, [int]$Code = 1)
  $e = New-Object System.Exception $Message
  $e.Data['AnetFailure'] = $true
  $e.Data['AnetCode'] = $Code
  return $e
}

# $AnetExitRemaining is the exit status of a run in which anet was
# installed but the installation stopped before its last step, with the
# commands that complete it printed (the header says when).
$AnetExitRemaining = 3

# Assert-AnetReleased refuses the template in the repository, whose pins are
# empty, and pins that are not in the form build-release.sh writes.
function Assert-AnetReleased {
  if ($null -eq $AnetRelease) {
    throw (New-AnetFailure 'this copy of install.ps1 is the unreleased template; use the copy published with a release (https://agentnetwork.org.cn/install.ps1)')
  }
  if (-not ($AnetRelease -is [hashtable]) -or -not ($AnetRelease['Assets'] -is [hashtable]) -or
      -not ([string]$AnetRelease['Version'] -cmatch '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$')) {
    throw (New-AnetFailure 'the release pins of this script are malformed')
  }
  foreach ($n in @('anet-windows-amd64', 'anet-windows-arm64', 'anet-shell-windows-amd64', 'anet-shell-windows-arm64')) {
    $p = $AnetRelease['Assets'][$n]
    if (-not ($p -is [hashtable]) -or
        -not ([string]$p['GzSha256'] -cmatch '^[0-9a-f]{64}$') -or -not ([string]$p['Sha256'] -cmatch '^[0-9a-f]{64}$') -or
        -not ($p['GzSize'] -is [int] -or $p['GzSize'] -is [long]) -or -not ($p['Size'] -is [int] -or $p['Size'] -is [long]) -or
        $p['GzSize'] -le 0 -or $p['Size'] -le 0) {
      throw (New-AnetFailure ('the release pins of this script are malformed for ' + $n))
    }
  }
}

# Invoke-AnetNative runs icacls.exe and taskkill.exe (every other program
# runs through Invoke-AnetTimed). The arguments are an array (no command line
# is assembled from text), stderr is never redirected (Windows PowerShell
# 5.1 would turn its lines into errors), and a non-zero exit status is
# reported in $Result.Code, not thrown. With -Capture, stdout is returned as
# lines in $Result.Output; without it, the program writes to the console
# directly. A caller that wants the output on the console must not assign
# the call.
function Invoke-AnetNative {
  param([Parameter(Mandatory = $true)][string]$FilePath, [string[]]$ArgumentList = @(),
        [Parameter(Mandatory = $true)][hashtable]$Result, [switch]$Capture)
  $previous = $ErrorActionPreference
  $ErrorActionPreference = 'Continue'
  $Result['Code'] = -1
  $Result['Output'] = @()
  try {
    $global:LASTEXITCODE = 0
    if ($Capture) {
      $Result['Output'] = @(& $FilePath @ArgumentList)
    } else {
      & $FilePath @ArgumentList
    }
    $Result['Code'] = $global:LASTEXITCODE
  } catch {
    $Result['Code'] = -1
    Write-AnetLine ('   cannot run ' + $FilePath + ': ' + $_.Exception.Message)
  } finally {
    $ErrorActionPreference = $previous
  }
}

# Get-AnetArch is the architecture of Windows itself (not of this process,
# which may run under emulation): amd64 or arm64.
function Get-AnetArch {
  $a = $env:PROCESSOR_ARCHITEW6432
  if ([string]::IsNullOrEmpty($a)) { $a = $env:PROCESSOR_ARCHITECTURE }
  switch -exact ([string]$a) {
    'AMD64' { return 'amd64' }
    'ARM64' { return 'arm64' }
  }
  throw (New-AnetFailure ('unsupported processor architecture: ' + $a + ' (anet is built for x64 and arm64)'))
}

# Test-AnetHttpsUrl reports whether $Text is an absolute https URL without
# credentials, query or fragment.
function Test-AnetHttpsUrl {
  param([string]$Text, [switch]$AllowLoopbackHttp)
  if ([string]::IsNullOrEmpty($Text) -or $Text -match '[\s"''`<>]') { return $false }
  $u = $null
  if (-not [Uri]::TryCreate($Text, [UriKind]::Absolute, [ref]$u)) { return $false }
  if ($u.UserInfo -ne '' -or $u.Query -ne '' -or $u.Fragment -ne '' -or $u.Host -eq '') { return $false }
  if ($u.Scheme -ceq 'https') { return $true }
  if ($AllowLoopbackHttp -and $u.Scheme -ceq 'http' -and ($u.Host -eq '127.0.0.1' -or $u.Host -eq 'localhost')) { return $true }
  return $false
}

# The placeholder of the published command with -Controller. cmd.exe would
# read < and > as redirections, so the placeholder is a plain word, and the
# installer refuses it by name.
$AnetControllerPlaceholder = 'CONTROLLER_AID'

# The hub a node joins unless -Hub, -NoJoin or -NoStart says otherwise.
$AnetDefaultHub = 'https://hub.agentnetwork.org.cn'

# Get-AnetOptions validates the parameters (a hashtable of what was given)
# and returns what the install will do. Nothing is read from or written to
# the machine except $env:COMPUTERNAME, ANET_INSTALL_BASE and, for -Base and
# -Prefix, whether a directory exists. Without -Prefix, Prefix is empty: the
# install directory is chosen later (Select-AnetInstallDir).
function Get-AnetOptions {
  param([Parameter(Mandatory = $true)][hashtable]$Given)
  $o = @{
    Controller = ''; Variant = 'default'; AssetPrefix = 'anet'; Hub = ''; Join = $false; Start = $true; Name = '';
    Prefix = ''; PrefixGiven = $false; Without = @(); Base = ''; NoAutostart = [bool]$Given['NoAutostart'];
    NoPath = [bool]$Given['NoPath']; Elevated = [bool]$Given['Elevated']; NoJoin = [bool]$Given['NoJoin'];
    NoStart = [bool]$Given['NoStart']; Agents = @()
  }
  $controller = [string]$Given['Controller']
  if ($Given.ContainsKey('Controller')) {
    if ($controller.Trim() -ieq $AnetControllerPlaceholder) {
      throw (New-AnetFailure ('-Controller ' + $AnetControllerPlaceholder + ' is the placeholder of the published command; replace it with the AID of the agent that is to control this computer (bafyrei followed by 52 characters a-z and 2-7)'))
    }
    if (-not ($controller -cmatch '^bafyrei[a-z2-7]{52}$')) {
      throw (New-AnetFailure ('-Controller ' + $controller + ' is not an AID (bafyrei followed by 52 characters a-z and 2-7)'))
    }
    $o.Controller = $controller
  }
  if ($o.Controller -ne '' -or [bool]$Given['Shell']) {
    $o.Variant = 'shell'
    $o.AssetPrefix = 'anet-shell'
  }
  if ($o.NoJoin -and $Given.ContainsKey('Hub')) {
    throw (New-AnetFailure '-NoJoin and -Hub contradict each other; use one of them')
  }
  if ($o.NoStart -and $Given.ContainsKey('Hub')) {
    throw (New-AnetFailure '-NoStart and -Hub contradict each other (a node that is not started does not join); use one of them')
  }
  # The node is started and joins the official hub unless -NoStart (neither)
  # or -NoJoin (no join) says otherwise.
  $o.Start = (-not $o.NoStart)
  if ($Given.ContainsKey('Hub')) {
    $hub = [string]$Given['Hub']
    if (-not (Test-AnetHttpsUrl -Text $hub -AllowLoopbackHttp)) {
      throw (New-AnetFailure ('-Hub ' + $hub + ' is not an absolute https:// URL (http:// only for 127.0.0.1 or localhost)'))
    }
    $o.Hub = $hub.TrimEnd('/')
  } elseif ($o.Start -and -not $o.NoJoin) {
    $o.Hub = $AnetDefaultHub
  }
  $o.Join = ($o.Hub -ne '')
  $name = [string]$Given['Name']
  if (-not $Given.ContainsKey('Name')) { $name = [string]$env:COMPUTERNAME }
  if (-not ($name -cmatch '^[A-Za-z0-9][A-Za-z0-9 ._-]{0,63}$')) {
    throw (New-AnetFailure ('-Name ' + $name + ' is not a name to join under (a letter or digit, then up to 63 letters, digits, spaces, dots, underscores or hyphens)'))
  }
  $o.Name = $name
  $without = New-Object System.Collections.Generic.List[string]
  foreach ($item in @($Given['Without'])) {
    if ($null -eq $item -and -not $Given.ContainsKey('Without')) { continue }
    foreach ($m in ([string]$item).Split(',')) {
      $m = $m.Trim()
      if (-not ($m -cmatch '^[a-z0-9]+$')) {
        throw (New-AnetFailure ('-Without: ''' + $m + ''' is not a module name (lower-case letters and digits, comma-separated)'))
      }
      if (-not $without.Contains($m)) { $without.Add($m) }
    }
  }
  if ($o.Variant -eq 'shell' -and $without.Contains('shell')) {
    throw (New-AnetFailure 'the shell build is requested (-Shell or -Controller) and -Without shell turns it off; use one of them')
  }
  $o.Without = $without.ToArray()
  # -Agents: every agent found (--all); -AgentList: the agents named.
  if ($Given.ContainsKey('AgentList')) {
    $agents = New-Object System.Collections.Generic.List[string]
    foreach ($item in @($Given['AgentList'])) {
      foreach ($a in ([string]$item).Split(',')) {
        $a = $a.Trim()
        if (-not ($a -cmatch '^[a-z][a-z0-9-]{0,31}$')) {
          throw (New-AnetFailure ('-AgentList: ''' + $a + ''' is not the name of an agent (lower-case letters, digits and hyphens, comma-separated, such as claude,codex)'))
        }
        if (-not $agents.Contains($a)) { $agents.Add($a) }
      }
    }
    $o.Agents = $agents.ToArray()
  } elseif ([bool]$Given['Agents']) {
    $o.Agents = @('--all')
  }
  if ($Given.ContainsKey('Prefix')) {
    $p = [string]$Given['Prefix']
    if (-not ($p -match '^[A-Za-z]:[\\/]') -or $p -match '[\*\?"<>\|]' -or $p -match '^[A-Za-z]:[\\/]+$') {
      throw (New-AnetFailure ('-Prefix ' + $p + ' is not an absolute path on a drive (such as C:\Users\me\anet)'))
    }
    if ($p -match '[;%]') {
      throw (New-AnetFailure ('-Prefix ' + $p + ' contains ; or %, which the PATH variable cannot carry in a directory name; choose another directory'))
    }
    $o.Prefix = [IO.Path]::GetFullPath($p).TrimEnd('\')
    $o.PrefixGiven = $true
    if ($o.Prefix.Length -le 2) {
      throw (New-AnetFailure ('-Prefix ' + $p + ' is the root of a drive; give a directory'))
    }
  }
  if ($Given.ContainsKey('Base')) {
    $o.Base = Get-AnetBaseValue -Text ([string]$Given['Base']) -What '-Base'
  }
  return $o
}

# Get-AnetBaseValue accepts an https URL (returned without a trailing
# slash) or an existing directory (returned as a full path).
function Get-AnetBaseValue {
  param([string]$Text, [string]$What)
  if (Test-AnetHttpsUrl -Text $Text) { return $Text.TrimEnd('/') }
  if ($Text -match '^[A-Za-z]:[\\/]|^\\\\' -and (Test-Path -LiteralPath $Text -PathType Container)) {
    return [IO.Path]::GetFullPath($Text).TrimEnd('\')
  }
  throw (New-AnetFailure ($What + ' ' + $Text + ' is neither an https:// URL nor an existing directory'))
}

# Get-AnetNpmPackage is the npm package that carries a build, by npm's names
# of the platform: @agentnetwork/<asset prefix>-win32-<x64|arm64>.
function Get-AnetNpmPackage {
  param([string]$AssetPrefix, [string]$Arch)
  $cpu = $Arch
  if ($Arch -eq 'amd64') { $cpu = 'x64' }
  return '@agentnetwork/' + $AssetPrefix + '-win32-' + $cpu
}

function Get-AnetSha256Hex {
  param([byte[]]$Bytes)
  $h = [System.Security.Cryptography.SHA256]::Create()
  try {
    return ([BitConverter]::ToString($h.ComputeHash($Bytes)) -replace '-', '').ToLowerInvariant()
  } finally {
    $h.Dispose()
  }
}

# Test-AnetPin checks a downloaded .gz against its pin and returns the binary
# inside it: the .gz has exactly the pinned size and SHA-256, it decompresses
# to exactly the pinned size (never reading more than one byte past it), and
# the result has the pinned SHA-256.
function Test-AnetPin {
  param([byte[]]$Gz, [Parameter(Mandatory = $true)][hashtable]$Pin)
  if ($null -eq $Gz -or $Gz.Length -ne $Pin['GzSize']) {
    throw (New-AnetFailure ('the .gz is ' + $(if ($null -eq $Gz) { 0 } else { $Gz.Length }) + ' bytes, the release pins ' + $Pin['GzSize']))
  }
  $got = Get-AnetSha256Hex -Bytes $Gz
  if (-not ($got -ceq $Pin['GzSha256'])) {
    throw (New-AnetFailure ('the .gz has SHA-256 ' + $got + ', the release pins ' + $Pin['GzSha256']))
  }
  $limit = [long]$Pin['Size']
  $in = New-Object System.IO.MemoryStream -ArgumentList (,$Gz)
  $z = New-Object System.IO.Compression.GZipStream -ArgumentList $in, ([System.IO.Compression.CompressionMode]::Decompress)
  $out = New-Object System.IO.MemoryStream
  try {
    $buf = New-Object byte[] 65536
    $total = [long]0
    while ($true) {
      $n = $z.Read($buf, 0, $buf.Length)
      if ($n -le 0) { break }
      $total += $n
      if ($total -gt $limit) {
        throw (New-AnetFailure ('the binary inside the .gz is larger than the ' + $limit + ' bytes the release pins'))
      }
      $out.Write($buf, 0, $n)
    }
  } catch {
    if ($_.Exception.Data['AnetFailure']) { throw }
    throw (New-AnetFailure ('the .gz does not decompress: ' + $_.Exception.Message))
  } finally {
    $z.Dispose()
    $in.Dispose()
  }
  $bin = $out.ToArray()
  $out.Dispose()
  if ($bin.Length -ne $limit) {
    throw (New-AnetFailure ('the binary inside the .gz is ' + $bin.Length + ' bytes, the release pins ' + $limit))
  }
  $got = Get-AnetSha256Hex -Bytes $bin
  if (-not ($got -ceq $Pin['Sha256'])) {
    throw (New-AnetFailure ('the binary has SHA-256 ' + $got + ', the release pins ' + $Pin['Sha256']))
  }
  return ,$bin
}

# The download line. On an interactive console (output not redirected), a
# download is one line that is rewritten about five times a second: the
# label, a bar, megabytes done of the total, the speed and the time left;
# when it ends, the line stays with the size, the average speed and the time
# it took. The bar is the heavy horizontal line, red for the part done and
# dark gray for the rest, or # and - when the console's code page cannot
# show that character; NO_COLOR turns the colours off. Without an
# interactive console (a CI job, an agent's tool call, a pipe), nothing is
# shown while it runs, and one plain line when it is done.

# Test-AnetInteractive: the console shows a line that is rewritten.
function Test-AnetInteractive {
  try {
    return ($Host.Name -eq 'ConsoleHost' -and [Environment]::UserInteractive -and -not [Console]::IsOutputRedirected)
  } catch {
    return $false
  }
}

# Get-AnetBarGlyphs is the pair of characters of the bar: done, to do.
function Get-AnetBarGlyphs {
  param([switch]$Color)
  $heavy = [string][char]0x2501
  $light = [string][char]0x2500
  try {
    $enc = [Console]::OutputEncoding
    if ($enc.GetString($enc.GetBytes($heavy + $light)) -ceq ($heavy + $light)) {
      if ($Color) { return @($heavy, $heavy) }
      return @($heavy, $light)
    }
  } catch { }
  return @('#', '-')
}

# Format-AnetMB is a size in megabytes with one decimal.
function Format-AnetMB {
  param([long]$Bytes)
  return ([Math]::Round($Bytes / 1048576.0, 1)).ToString('0.0', [Globalization.CultureInfo]::InvariantCulture)
}

# Format-AnetDuration is m:ss.
function Format-AnetDuration {
  param([double]$Seconds)
  if ($Seconds -lt 0 -or [double]::IsInfinity($Seconds) -or [double]::IsNaN($Seconds)) { return '-:--' }
  $s = [long][Math]::Round($Seconds)
  return ('{0}:{1:00}' -f [long][Math]::Floor($s / 60), ($s % 60))
}

# New-AnetProgress starts the line of one download. -Show:$false (a local
# directory) shows nothing.
function New-AnetProgress {
  param([string]$Label, [long]$Total, [switch]$Show)
  $interactive = $Show -and (Test-AnetInteractive)
  $color = $interactive -and [string]::IsNullOrEmpty($env:NO_COLOR)
  return @{ Label = $Label; Total = $Total; Done = [long]0; Interactive = $interactive; Color = $color;
            Glyphs = (Get-AnetBarGlyphs -Color:$color); Clock = [Diagnostics.Stopwatch]::StartNew();
            Last = [long]-1000; Width = 0; Open = $false; RateBytes = [long]0; RateAt = [long]0; Rate = 0.0;
            FirstAt = [long]-1; FirstBytes = [long]0; Windowed = $false }
}

# Write-AnetProgressLine draws the line: the label, then the bar of $Width
# characters, then $Tail.
function Write-AnetProgressLine {
  param([hashtable]$Progress, [double]$Fraction, [string]$Tail)
  $cols = 80
  try { $cols = [Console]::WindowWidth } catch { }
  if ($cols -lt 20) { $cols = 80 }
  # The bar keeps its width while the numbers after it change.
  $bar = [Math]::Min(30, $cols - 1 - $Progress.Label.Length - [Math]::Max($Tail.Length, 34) - 4)
  $text = $Progress.Label + '  '
  [Console]::Write("`r" + $text)
  $len = $text.Length
  if ($bar -ge 5) {
    $done = [int][Math]::Floor($bar * [Math]::Min(1.0, [Math]::Max(0.0, $Fraction)))
    $previous = [Console]::ForegroundColor
    if ($Progress.Color) { [Console]::ForegroundColor = [ConsoleColor]::Red }
    [Console]::Write($Progress.Glyphs[0] * $done)
    if ($Progress.Color) { [Console]::ForegroundColor = [ConsoleColor]::DarkGray }
    [Console]::Write($Progress.Glyphs[1] * ($bar - $done))
    if ($Progress.Color) { [Console]::ForegroundColor = $previous }
    $len += $bar
  }
  [Console]::Write('  ' + $Tail)
  $len += 2 + $Tail.Length
  if ($len -lt $Progress.Width) { [Console]::Write(' ' * ($Progress.Width - $len)) }
  $Progress.Width = $len
  $Progress.Open = $true
}

# Update-AnetProgress records $Done bytes and redraws the line at most
# every 200 milliseconds. The speed is that of the last second, and before
# a second has passed, that since the first bytes arrived.
function Update-AnetProgress {
  param([hashtable]$Progress, [long]$Done)
  $Progress.Done = $Done
  if (-not $Progress.Interactive) { return }
  $now = $Progress.Clock.ElapsedMilliseconds
  if ($now - $Progress.Last -lt 200) { return }
  $Progress.Last = $now
  if ($Progress.FirstAt -lt 0) {
    $Progress.FirstAt = $now
    $Progress.FirstBytes = $Done
    $Progress.RateAt = $now
    $Progress.RateBytes = $Done
  } elseif ($now - $Progress.RateAt -ge 1000) {
    $Progress.Rate = ($Done - $Progress.RateBytes) * 1000.0 / ($now - $Progress.RateAt)
    $Progress.RateBytes = $Done
    $Progress.RateAt = $now
    $Progress.Windowed = $true
  } elseif (-not $Progress.Windowed -and $now -gt $Progress.FirstAt) {
    $Progress.Rate = ($Done - $Progress.FirstBytes) * 1000.0 / ($now - $Progress.FirstAt)
  }
  $total = [Math]::Max($Progress.Total, $Done)
  $eta = -1.0
  if ($Progress.Rate -gt 0) { $eta = ($total - $Done) / $Progress.Rate }
  $tail = (Format-AnetMB $Done) + '/' + (Format-AnetMB $total) + ' MB  ' + (Format-AnetMB ([long]$Progress.Rate)) + ' MB/s  eta ' + (Format-AnetDuration $eta)
  $fraction = 0.0
  if ($total -gt 0) { $fraction = $Done / [double]$total }
  try { Write-AnetProgressLine -Progress $Progress -Fraction $fraction -Tail $tail } catch { $Progress.Interactive = $false }
}

# Complete-AnetProgress ends the line of a download that succeeded: on an
# interactive console the full bar with the size, the average speed and the
# time it took; otherwise the line $Plain. Both go to the log as $Plain.
function Complete-AnetProgress {
  param([hashtable]$Progress, [long]$Done, [string]$Plain)
  $secs = $Progress.Clock.Elapsed.TotalSeconds
  if ($Progress.Interactive) {
    $avg = 0.0
    if ($secs -gt 0) { $avg = $Done / $secs }
    $tail = (Format-AnetMB $Done) + ' MB  ' + (Format-AnetMB ([long]$avg)) + ' MB/s  ' + (Format-AnetDuration $secs)
    try {
      Write-AnetProgressLine -Progress $Progress -Fraction 1.0 -Tail $tail
      [Console]::WriteLine()
      $Progress.Open = $false
      Write-AnetLine $Plain
      return
    } catch { }
  }
  Stop-AnetProgress -Progress $Progress
  Write-AnetSay $Plain
}

# Stop-AnetProgress ends a line that is still open (a download that failed),
# so that the next line starts at the left.
function Stop-AnetProgress {
  param([hashtable]$Progress)
  if ($null -ne $Progress -and $Progress.Open) {
    try { [Console]::WriteLine() } catch { }
    $Progress.Open = $false
  }
}

# The download sources of a release (DOWNLOAD SOURCES in the header). A
# source is a hashtable: Name (npmmirror, github, npmjs, jsdelivr, host,
# base), Label (as the summary names it), Kind, and where it reads from:
#   tarball  the npm package of the build: MetaUrl, its metadata for the
#            version (JSON with that version), and Url, its tarball, which
#            holds package/dl/release.json, release.json.sig and <asset>.gz;
#   files    the three files at Prefix + <name>;
#   dir      the three files in the local directory Dir.
# GitHub sets Redirect = 'github': redirects only to github.com and
# *.githubusercontent.com. The release version is part of every address
# except the project's hosts, which serve the current release under /dl/.
$AnetGitHubReleases = 'https://github.com/ANetResearch/ANet/releases/download/v'

# New-AnetBaseSource is the source of -Base or ANET_INSTALL_BASE: an https
# base (<base>/dl/<name>) or a local release directory.
function New-AnetBaseSource {
  param([string]$Base)
  if ($Base -like 'https://*') { return @{ Name = 'base'; Label = $Base; Kind = 'files'; Prefix = ($Base + '/dl/'); Redirect = '' } }
  return @{ Name = 'base'; Label = $Base; Kind = 'dir'; Dir = $Base }
}

# Get-AnetSourceList is every source of the build, by name: the two that are
# probed, npmmirror and github, and the four that follow them in this order:
# npmjs, jsdelivr, then the project's two hosts.
function Get-AnetSourceList {
  param([Parameter(Mandatory = $true)][string]$AssetPrefix, [Parameter(Mandatory = $true)][string]$Arch, [Parameter(Mandatory = $true)][string]$Version)
  $pkg = Get-AnetNpmPackage -AssetPrefix $AssetPrefix -Arch $Arch
  $leaf = $pkg.Substring($pkg.IndexOf('/') + 1)
  $tgz = '/-/' + $leaf + '-' + $Version + '.tgz'
  return @{
    npmmirror = @{ Name = 'npmmirror'; Label = 'npmmirror'; Kind = 'tarball'; MetaUrl = ('https://registry.npmmirror.com/' + $pkg + '/' + $Version); Url = ('https://registry.npmmirror.com/' + $pkg + $tgz); Redirect = '' }
    github = @{ Name = 'github'; Label = 'GitHub'; Kind = 'files'; Prefix = ($AnetGitHubReleases + $Version + '/'); Redirect = 'github' }
    npmjs = @{ Name = 'npmjs'; Label = 'registry.npmjs.org'; Kind = 'tarball'; MetaUrl = ('https://registry.npmjs.org/' + $pkg + '/' + $Version); Url = ('https://registry.npmjs.org/' + $pkg + $tgz); Redirect = '' }
    jsdelivr = @{ Name = 'jsdelivr'; Label = 'jsDelivr'; Kind = 'files'; Prefix = ('https://cdn.jsdelivr.net/npm/' + $pkg + '@' + $Version + '/dl/'); Redirect = '' }
    host1 = @{ Name = 'host'; Label = 'agentnetwork.org.cn'; Kind = 'files'; Prefix = 'https://agentnetwork.org.cn/dl/'; Redirect = '' }
    host2 = @{ Name = 'host'; Label = 'hub.agentnetwork.org.cn'; Kind = 'files'; Prefix = 'https://hub.agentnetwork.org.cn/dl/'; Redirect = '' }
  }
}

# Get-AnetProbeUrl is the small file a probe of a source fetches: the
# metadata of the version for an npm registry, release.json for GitHub.
function Get-AnetProbeUrl {
  param([hashtable]$Source)
  if ($Source.Kind -eq 'tarball') { return $Source.MetaUrl }
  return ($Source.Prefix + 'release.json')
}

# Get-AnetSourceOrder puts the sources in the order they are tried, from
# the result of the probe: Reachable lists the names of the probed sources
# (npmmirror, github) that answered, in the order they answered; those come
# first, and a probed source that did not answer is left out. Then npmjs,
# jsdelivr and the project's two hosts, always.
function Get-AnetSourceOrder {
  param([Parameter(Mandatory = $true)][hashtable]$List, [Parameter(Mandatory = $true)][hashtable]$Probe)
  $order = New-Object System.Collections.Generic.List[hashtable]
  foreach ($n in @($Probe['Reachable'])) {
    if (($n -eq 'npmmirror' -or $n -eq 'github') -and @($order | Where-Object { $_.Name -eq $n }).Count -eq 0) { $order.Add($List[$n]) }
  }
  foreach ($n in @('npmjs', 'jsdelivr', 'host1', 'host2')) { $order.Add($List[$n]) }
  return $order.ToArray()
}

# Get-AnetSources is the list of sources, in the order they are tried: the
# source of -Base, else of ANET_INSTALL_BASE, alone; otherwise
# Get-AnetSourceOrder after a probe of npmmirror and GitHub (Invoke-AnetProbe;
# a test passes its result as $Probe).
function Get-AnetSources {
  param([Parameter(Mandatory = $true)][hashtable]$Options, [Parameter(Mandatory = $true)][string]$Arch,
        [Parameter(Mandatory = $true)][string]$Version, [hashtable]$Probe = $null)
  if ($Options.Base -ne '') { return @(New-AnetBaseSource -Base $Options.Base) }
  if (-not [string]::IsNullOrEmpty($env:ANET_INSTALL_BASE)) {
    return @(New-AnetBaseSource -Base (Get-AnetBaseValue -Text $env:ANET_INSTALL_BASE -What 'ANET_INSTALL_BASE'))
  }
  $list = Get-AnetSourceList -AssetPrefix $Options.AssetPrefix -Arch $Arch -Version $Version
  if ($null -eq $Probe) { $Probe = Invoke-AnetProbe -Sources @($list['npmmirror'], $list['github']) -Version $Version }
  $order = @(Get-AnetSourceOrder -List $list -Probe $Probe)
  Write-AnetLine ('download sources, in order: ' + (@($order | ForEach-Object { $_.Label }) -join ', '))
  return $order
}

# New-AnetHttpClient is an HttpClient that follows no redirect by itself (a
# caller follows them, https only), with a connection timeout of 5 seconds
# where .NET has one (PowerShell 7) and $TimeoutSeconds for the answer.
function New-AnetHttpClient {
  param([int]$TimeoutSeconds, [switch]$Redirects)
  if ($null -eq ('System.Net.Http.HttpClient' -as [type])) { Add-Type -AssemblyName System.Net.Http }
  $socketsHandler = 'System.Net.Http.SocketsHttpHandler' -as [type]
  if ($null -ne $socketsHandler) {
    $handler = New-Object System.Net.Http.SocketsHttpHandler
    $handler.ConnectTimeout = [TimeSpan]::FromSeconds([Math]::Min(5, $TimeoutSeconds))
  } else {
    $handler = New-Object System.Net.Http.HttpClientHandler
  }
  $handler.AllowAutoRedirect = [bool]$Redirects
  if ($Redirects) { $handler.MaxAutomaticRedirections = 5 }
  $client = New-Object System.Net.Http.HttpClient -ArgumentList $handler
  $client.Timeout = [TimeSpan]::FromSeconds($TimeoutSeconds)
  return $client
}

# Invoke-AnetProbe asks the sources (npmmirror and GitHub) at the same time
# for their probe file (Get-AnetProbeUrl), each for at most $TimeoutMs, and
# waits at most $TotalMs in all. A source answers when it returns that file
# for this version: npm metadata whose version is $Version, or a
# release.json. Returns Reachable, the names of those that answered, the
# first to answer first. What a probe returns is used for nothing else.
function Invoke-AnetProbe {
  param([hashtable[]]$Sources, [string]$Version, [int]$TimeoutMs = 3000, [int]$TotalMs = 4000)
  $reachable = New-Object System.Collections.Generic.List[string]
  $client = New-AnetHttpClient -TimeoutSeconds ([Math]::Max(1, [int][Math]::Ceiling($TimeoutMs / 1000))) -Redirects
  $client.MaxResponseContentBufferSize = 1048576
  $cts = New-Object System.Threading.CancellationTokenSource
  $cts.CancelAfter($TimeoutMs)
  $clock = [Diagnostics.Stopwatch]::StartNew()
  $pending = New-Object System.Collections.Generic.List[object]
  try {
    foreach ($s in $Sources) {
      $req = New-Object System.Net.Http.HttpRequestMessage -ArgumentList ([System.Net.Http.HttpMethod]::Get), (Get-AnetProbeUrl -Source $s)
      $req.Headers.Add('Cache-Control', 'no-cache')
      $pending.Add(@{ Source = $s; Task = $client.SendAsync($req, [System.Net.Http.HttpCompletionOption]::ResponseContentRead, $cts.Token) })
    }
    while ($pending.Count -gt 0) {
      $left = $TotalMs - [int]$clock.ElapsedMilliseconds
      if ($left -le 0) { break }
      $tasks = [System.Threading.Tasks.Task[]]@($pending | ForEach-Object { $_.Task })
      $i = [System.Threading.Tasks.Task]::WaitAny($tasks, $left)
      if ($i -lt 0) { break }
      $p = $pending[$i]
      $pending.RemoveAt($i)
      $name = $p.Source.Name
      $ms = [int]$clock.ElapsedMilliseconds
      if ($p.Task.Status -ne [System.Threading.Tasks.TaskStatus]::RanToCompletion) {
        $why = 'no answer'
        if ($null -ne $p.Task.Exception) { $why = $p.Task.Exception.GetBaseException().Message }
        Write-AnetLine ('probe ' + $name + ': not reachable (' + $why + ')')
        continue
      }
      $resp = $p.Task.Result
      try {
        $code = [int]$resp.StatusCode
        $final = $resp.RequestMessage.RequestUri
        $ok = ($code -eq 200 -and $final.Scheme -ceq 'https')
        if ($ok -and $p.Source.Redirect -eq 'github') { $ok = (Test-AnetGitHubHost -HostName $final.Host) }
        if ($ok) {
          $body = $resp.Content.ReadAsStringAsync().GetAwaiter().GetResult()
          if ($p.Source.Kind -eq 'tarball') {
            $ok = ([string](($body | ConvertFrom-Json).version) -ceq $Version)
          } else {
            $ok = $body.TrimStart().StartsWith('{')
          }
        }
        if ($ok) {
          $reachable.Add($name)
          Write-AnetLine ('probe ' + $name + ': reachable in ' + $ms + ' ms')
        } else {
          Write-AnetLine ('probe ' + $name + ': does not serve ' + $Version + ' (HTTP ' + $code + ' from ' + $final.Host + ')')
        }
      } catch {
        Write-AnetLine ('probe ' + $name + ': does not serve ' + $Version + ' (' + $_.Exception.Message + ')')
      } finally {
        $resp.Dispose()
      }
    }
    foreach ($p in $pending) { Write-AnetLine ('probe ' + $p.Source.Name + ': no answer within ' + $TotalMs + ' ms') }
  } finally {
    $cts.Cancel()
    $client.Dispose()
    $cts.Dispose()
  }
  return @{ Reachable = $reachable.ToArray() }
}

# Test-AnetGitHubHost: a host a GitHub download may be redirected to.
function Test-AnetGitHubHost {
  param([string]$HostName)
  $h = $HostName.ToLowerInvariant()
  return ($h -ceq 'github.com' -or $h.EndsWith('.githubusercontent.com'))
}

# New-AnetNotServing is the exception of a source that does not serve the
# release: Get-AnetRelease passes over it for the next source.
function New-AnetNotServing {
  param([string]$Message)
  $e = New-Object System.Exception $Message
  $e.Data['AnetNotServing'] = $true
  return $e
}

# Get-AnetHttpBytes fetches an https URL and returns its body, at most
# $Limit bytes: redirects are followed by hand, at most five, only to https
# (and with -Redirect github only to GitHub's hosts). $TimeoutSeconds bounds
# the wait for the answer; the body must then keep coming (no 30 seconds
# without data). $Progress, when given, is updated as the body arrives
# (Update-AnetProgress). Every failure is New-AnetNotServing.
function Get-AnetHttpBytes {
  param([string]$Url, [long]$Limit, [int]$TimeoutSeconds, [string]$Redirect = '', [hashtable]$Progress = $null)
  $client = New-AnetHttpClient -TimeoutSeconds $TimeoutSeconds
  try {
    $uri = New-Object System.Uri $Url
    for ($hop = 0; $hop -le 5; $hop++) {
      if ($uri.Scheme -cne 'https') { throw (New-AnetNotServing ('redirected to ' + $uri.Scheme + '://' + $uri.Host + ': downloads are https only')) }
      if ($Redirect -eq 'github' -and -not (Test-AnetGitHubHost -HostName $uri.Host)) {
        throw (New-AnetNotServing ('redirected to ' + $uri.Host + ', which is not a host of GitHub'))
      }
      $req = New-Object System.Net.Http.HttpRequestMessage -ArgumentList ([System.Net.Http.HttpMethod]::Get), $uri
      $req.Headers.Add('Cache-Control', 'no-cache')
      try {
        $resp = $client.SendAsync($req, [System.Net.Http.HttpCompletionOption]::ResponseHeadersRead).GetAwaiter().GetResult()
      } catch {
        throw (New-AnetNotServing ($uri.Host + ': ' + $_.Exception.GetBaseException().Message))
      }
      try {
        $code = [int]$resp.StatusCode
        if ($code -eq 301 -or $code -eq 302 -or $code -eq 303 -or $code -eq 307 -or $code -eq 308) {
          if ($null -eq $resp.Headers.Location) { throw (New-AnetNotServing ('HTTP ' + $code + ' without a Location')) }
          $uri = New-Object System.Uri -ArgumentList $uri, $resp.Headers.Location
          continue
        }
        if ($code -ne 200) { throw (New-AnetNotServing ('HTTP ' + $code + ' from ' + $uri.Host)) }
        $declared = $resp.Content.Headers.ContentLength
        if ($null -ne $declared -and $declared -gt $Limit) { throw (New-AnetNotServing ('' + $declared + ' bytes, more than the ' + $Limit + ' expected')) }
        if ($null -ne $Progress -and $null -ne $declared -and $declared -gt 0) { $Progress.Total = [long]$declared }
        $stream = $resp.Content.ReadAsStreamAsync().GetAwaiter().GetResult()
        $out = New-Object System.IO.MemoryStream
        try {
          $buf = New-Object byte[] 65536
          $total = [long]0
          while ($true) {
            $t = $stream.ReadAsync($buf, 0, $buf.Length)
            try {
              if (-not $t.Wait(30000)) { throw (New-AnetNotServing ('no data for 30 seconds from ' + $uri.Host)) }
            } catch {
              if ($_.Exception.Data['AnetNotServing']) { throw }
              throw (New-AnetNotServing ($uri.Host + ': ' + $_.Exception.GetBaseException().Message))
            }
            $n = $t.Result
            if ($n -le 0) { break }
            $total += $n
            if ($total -gt $Limit) { throw (New-AnetNotServing ('more than the ' + $Limit + ' bytes expected')) }
            $out.Write($buf, 0, $n)
            if ($null -ne $Progress) { Update-AnetProgress -Progress $Progress -Done $total }
          }
          return ,$out.ToArray()
        } finally {
          $stream.Dispose()
          $out.Dispose()
        }
      } finally {
        $resp.Dispose()
      }
    }
    throw (New-AnetNotServing 'more than 5 redirects')
  } finally {
    $client.Dispose()
  }
}

# Read-AnetLocalFile reads one file of a local release directory, at most
# $Limit bytes.
function Read-AnetLocalFile {
  param([string]$Dir, [string]$Name, [long]$Limit)
  $path = Join-Path $Dir $Name
  if (-not (Test-Path -LiteralPath $path -PathType Leaf)) { throw (New-AnetNotServing ($path + ' does not exist')) }
  $item = Get-Item -LiteralPath $path -Force
  if ($item.Length -gt $Limit) { throw (New-AnetNotServing ($path + ' is ' + $item.Length + ' bytes, more than the ' + $Limit + ' expected')) }
  return ,([IO.File]::ReadAllBytes($path))
}

# Test-AnetTarName: an entry name of an npm tarball that is safe to read: a
# relative path with / between its parts, no empty part, no . or .., no
# backslash, colon or control character. A tarball with any other name is
# refused whole (nothing of it is used, and nothing is ever written under
# the name of an entry).
function Test-AnetTarName {
  param([string]$Name)
  if ([string]::IsNullOrEmpty($Name) -or $Name.Length -gt 1024) { return $false }
  if ($Name -match '[\\:\x00-\x1f]' -or $Name.StartsWith('/')) { return $false }
  foreach ($part in $Name.TrimEnd('/').Split('/')) {
    if ($part -eq '' -or $part -eq '.' -or $part -eq '..') { return $false }
  }
  return $true
}

# Get-AnetTarField reads a NUL-terminated text field of a tar header.
function Get-AnetTarField {
  param([byte[]]$Bytes, [int]$Offset, [int]$Length)
  $end = $Offset
  while ($end -lt $Offset + $Length -and $Bytes[$end] -ne 0) { $end++ }
  return [Text.Encoding]::UTF8.GetString($Bytes, $Offset, $end - $Offset)
}

# Read-AnetTarball reads an npm package tarball (a gzip of a ustar archive)
# in memory and returns the regular files whose names are in $Want, by
# name. The archive is refused (New-AnetNotServing) when it is not a gzip
# of a tar archive, when it decompresses to more than $Limit bytes, when an
# entry name is not safe (Test-AnetTarName), when a wanted name is not a
# regular file or comes twice, or when it ends early. Extended headers (pax
# x, GNU L) give the next entry its name and size; pax g is skipped.
function Read-AnetTarball {
  param([byte[]]$Tgz, [string[]]$Want, [long]$Limit)
  if ($null -eq $Tgz -or $Tgz.Length -lt 2 -or $Tgz[0] -ne 0x1F -or $Tgz[1] -ne 0x8B) { throw (New-AnetNotServing 'the package is not a gzip file') }
  $in = New-Object System.IO.MemoryStream -ArgumentList (,$Tgz)
  $z = New-Object System.IO.Compression.GZipStream -ArgumentList $in, ([System.IO.Compression.CompressionMode]::Decompress)
  $ms = New-Object System.IO.MemoryStream
  try {
    $buf = New-Object byte[] 65536
    while ($true) {
      $n = $z.Read($buf, 0, $buf.Length)
      if ($n -le 0) { break }
      if ($ms.Length + $n -gt $Limit) { throw (New-AnetNotServing ('the package decompresses to more than ' + $Limit + ' bytes')) }
      $ms.Write($buf, 0, $n)
    }
  } catch {
    if ($_.Exception.Data['AnetNotServing']) { throw }
    throw (New-AnetNotServing ('the package does not decompress: ' + $_.Exception.Message))
  } finally {
    $z.Dispose()
    $in.Dispose()
  }
  $tar = $ms.ToArray()
  $ms.Dispose()
  $out = @{}
  $pos = 0
  $nextName = ''
  $nextSize = [long]-1
  while ($true) {
    if ($pos + 512 -gt $tar.Length) { throw (New-AnetNotServing 'the package ends before the end of its archive') }
    $zero = $true
    for ($i = 0; $i -lt 512; $i++) { if ($tar[$pos + $i] -ne 0) { $zero = $false; break } }
    if ($zero) { break }
    $sum = 0
    for ($i = 0; $i -lt 512; $i++) { if ($i -ge 148 -and $i -lt 156) { $sum += 32 } else { $sum += $tar[$pos + $i] } }
    $check = (Get-AnetTarField -Bytes $tar -Offset ($pos + 148) -Length 8).Trim()
    if (-not ($check -match '^[0-7]{1,7}$') -or [Convert]::ToInt32($check, 8) -ne $sum) { throw (New-AnetNotServing 'the package is not a tar archive (header checksum)') }
    $sizeText = (Get-AnetTarField -Bytes $tar -Offset ($pos + 124) -Length 12).Trim()
    if (-not ($sizeText -match '^[0-7]{1,11}$')) { throw (New-AnetNotServing 'the package has an entry of unreadable size') }
    $size = [Convert]::ToInt64($sizeText, 8)
    $type = [char]$tar[$pos + 156]
    $name = Get-AnetTarField -Bytes $tar -Offset $pos -Length 100
    if ((Get-AnetTarField -Bytes $tar -Offset ($pos + 257) -Length 6) -eq 'ustar') {
      $prefix = Get-AnetTarField -Bytes $tar -Offset ($pos + 345) -Length 155
      if ($prefix -ne '') { $name = $prefix + '/' + $name }
    }
    $data = $pos + 512
    if ($type -eq 'x' -or $type -eq 'L' -or $type -eq 'g') {
      if ($size -gt 65536 -or $data + $size -gt $tar.Length) { throw (New-AnetNotServing 'the package has an extended header that does not read') }
      $ext = [Text.Encoding]::UTF8.GetString($tar, $data, [int]$size)
      if ($type -eq 'L') {
        $nextName = $ext.TrimEnd([char]0)
      } elseif ($type -eq 'x') {
        foreach ($m in [regex]::Matches($ext, '(?m)^[0-9]+ (path|size)=(.*)$')) {
          if ($m.Groups[1].Value -eq 'path') { $nextName = $m.Groups[2].Value }
          elseif ($m.Groups[2].Value -match '^[0-9]{1,15}$') { $nextSize = [long]$m.Groups[2].Value }
          else { throw (New-AnetNotServing 'the package has an extended header that does not read') }
        }
      }
      $pos = $data + [long][Math]::Ceiling($size / 512) * 512
      continue
    }
    if ($nextName -ne '') { $name = $nextName }
    if ($nextSize -ge 0) { $size = $nextSize }
    $nextName = ''
    $nextSize = [long]-1
    if (-not (Test-AnetTarName -Name $name)) { throw (New-AnetNotServing ('the package has an entry with an unsafe name: ' + ($name -replace '[\x00-\x1f]', '?'))) }
    if ($data + $size -gt $tar.Length) { throw (New-AnetNotServing ('the package ends inside ' + $name)) }
    if (@($Want) -ccontains $name) {
      if ($type -ne '0' -and $type -ne [char]0) { throw (New-AnetNotServing ($name + ' in the package is not a regular file')) }
      if ($out.ContainsKey($name)) { throw (New-AnetNotServing ($name + ' is in the package twice')) }
      $bytes = New-Object byte[] $size
      [Array]::Copy($tar, $data, $bytes, 0, $size)
      $out[$name] = $bytes
    }
    $pos = $data + [long][Math]::Ceiling($size / 512) * 512
  }
  return $out
}

# Get-AnetSourceFiles reads release.json, its signature and <asset>.gz from
# a source. Anything that keeps the source from serving them is
# New-AnetNotServing; the contents are checked by the caller.
function Get-AnetSourceFiles {
  param([hashtable]$Source, [string]$Asset, [hashtable]$Pin, [string]$Version, [hashtable]$Progress)
  $gzName = $Asset + '.gz'
  $gzLimit = [long]$Pin['GzSize'] + 1
  if ($Source.Kind -eq 'dir') {
    return @{
      Manifest = (Read-AnetLocalFile -Dir $Source.Dir -Name 'release.json' -Limit 1048576)
      Signature = (Read-AnetLocalFile -Dir $Source.Dir -Name 'release.json.sig' -Limit 16384)
      Gz = (Read-AnetLocalFile -Dir $Source.Dir -Name $gzName -Limit $gzLimit)
    }
  }
  if ($Source.Kind -eq 'files') {
    $manifest = Get-AnetHttpBytes -Url ($Source.Prefix + 'release.json') -Limit 1048576 -TimeoutSeconds 5 -Redirect $Source.Redirect
    Assert-AnetManifestServes -Manifest $manifest -Version $Version
    $sig = Get-AnetHttpBytes -Url ($Source.Prefix + 'release.json.sig') -Limit 16384 -TimeoutSeconds 15 -Redirect $Source.Redirect
    $Progress.Total = [long]$Pin['GzSize']
    $gz = Get-AnetHttpBytes -Url ($Source.Prefix + $gzName) -Limit $gzLimit -TimeoutSeconds 15 -Redirect $Source.Redirect -Progress $Progress
    return @{ Manifest = $manifest; Signature = $sig; Gz = $gz }
  }
  $meta = Get-AnetHttpBytes -Url $Source.MetaUrl -Limit 1048576 -TimeoutSeconds 5
  try {
    $v = [string](([Text.Encoding]::UTF8.GetString($meta)) | ConvertFrom-Json).version
  } catch {
    throw (New-AnetNotServing 'its package metadata does not read')
  }
  if (-not ($v -ceq $Version)) { throw (New-AnetNotServing ('it has no package of version ' + $Version)) }
  $Progress.Total = [long]$Pin['GzSize']
  $tgz = Get-AnetHttpBytes -Url $Source.Url -Limit ([long]$Pin['GzSize'] + 8388608) -TimeoutSeconds 15 -Progress $Progress
  $files = Read-AnetTarball -Tgz $tgz -Want @('package/dl/release.json', 'package/dl/release.json.sig', ('package/dl/' + $gzName)) -Limit ([long]$Pin['GzSize'] + 16777216)
  foreach ($n in @('release.json', 'release.json.sig', $gzName)) {
    if (-not $files.ContainsKey('package/dl/' + $n)) { throw (New-AnetNotServing ('the package has no dl/' + $n)) }
  }
  return @{ Manifest = $files['package/dl/release.json']; Signature = $files['package/dl/release.json.sig']; Gz = $files['package/dl/' + $gzName] }
}

# Get-AnetManifestVersion is the version a release.json names, without
# checking anything else ('' when it does not read): a source that serves
# another release than this script's is passed over before its files are
# compared with the pins.
function Get-AnetManifestVersion {
  param([byte[]]$Manifest)
  try {
    $v = [string](([Text.Encoding]::UTF8.GetString($Manifest)) | ConvertFrom-Json).version
    if (Test-AnetVersion $v) { return $v }
  } catch { }
  return ''
}

# Assert-AnetManifestServes passes over a source (New-AnetNotServing) whose
# release.json is not a manifest (a page, say) or names another release.
function Assert-AnetManifestServes {
  param([byte[]]$Manifest, [string]$Version)
  if ($null -eq $Manifest -or $Manifest.Length -eq 0 -or $Manifest[0] -ne 0x7B) { throw (New-AnetNotServing 'release.json is not a manifest') }
  $v = Get-AnetManifestVersion -Manifest $Manifest
  if (-not ($v -ceq $Version)) { throw (New-AnetNotServing ('it serves release ' + $(if ($v -eq '') { '(unreadable)' } else { $v }) + ', not ' + $Version)) }
}

# Get-AnetRelease tries the sources in order and returns the first that
# serves release.json of this release, its signature and the .gz, with
# the binary inside it. A source that does not serve them (no answer, an
# HTTP error, a page instead of a file, a missing file, another release) is
# passed over for the next one, in one line. A .gz that does not match the
# release pins ends the installation (New-AnetFailure from Test-AnetPin):
# a source only decides where the bytes come from.
function Get-AnetRelease {
  param([hashtable[]]$Sources, [string]$Asset, [hashtable]$Pin, [string]$Platform = '')
  $version = [string]$AnetRelease['Version']
  $tried = New-Object System.Collections.Generic.List[string]
  foreach ($s in $Sources) {
    Write-AnetLine ('source ' + $s.Label + ' (' + $s.Kind + ')')
    $progress = New-AnetProgress -Label (Get-AnetText 'dl_label' @{ version = $version; source = $s.Label }) -Total ([long]$Pin['GzSize']) -Show:($s.Kind -ne 'dir')
    try {
      $f = Get-AnetSourceFiles -Source $s -Asset $Asset -Pin $Pin -Version $version -Progress $progress
      Assert-AnetManifestServes -Manifest $f.Manifest -Version $version
      $first = ([Text.Encoding]::ASCII.GetString($f.Signature) -split "`r?`n", 2)[0]
      if ($first -cne '-----BEGIN SSH SIGNATURE-----') { throw (New-AnetNotServing 'release.json.sig is not an SSH signature') }
      if ($f.Gz.Length -lt 2 -or $f.Gz[0] -ne 0x1F -or $f.Gz[1] -ne 0x8B) { throw (New-AnetNotServing ($Asset + '.gz is not a gzip file')) }
    } catch {
      Stop-AnetProgress -Progress $progress
      if ($_.Exception.Data['AnetFailure']) { throw }
      $why = $_.Exception.Message
      if (-not $_.Exception.Data['AnetNotServing'] -and $null -ne $_.Exception.InnerException) { $why = $_.Exception.GetBaseException().Message }
      Write-AnetSay (Get-AnetText 'dl_skip' @{ source = $s.Label; version = $version; why = $why })
      $tried.Add($s.Label)
      continue
    }
    try {
      $bin = Test-AnetPin -Gz $f.Gz -Pin $Pin
    } catch {
      Stop-AnetProgress -Progress $progress
      throw
    }
    if ($s.Kind -eq 'dir') {
      Write-AnetSay (Get-AnetText 'dl_dir' @{ version = $version; platform = $Platform; source = $s.Label })
    } else {
      Complete-AnetProgress -Progress $progress -Done ([long]$progress.Done) -Plain (Get-AnetText 'dl_done' @{ version = $version; platform = $Platform; source = $s.Label;
        size = (Format-AnetMB ([long]$progress.Done)); secs = ([Math]::Round($progress.Clock.Elapsed.TotalSeconds, 1)).ToString([Globalization.CultureInfo]::InvariantCulture) })
    }
    Write-AnetLine ('OK ' + $Asset + '.gz and the binary inside it match the release pins (SHA-256 and size)')
    return @{ Source = $s; Manifest = $f.Manifest; Signature = $f.Signature; Binary = $bin }
  }
  throw (New-AnetFailure ('no download source served anet ' + $version + ' (tried: ' + ($tried -join ', ') + ')'))
}

# Get-AnetAccountName names an account for a message: DOMAIN\Name, or the
# SID followed by (unresolved) when Windows cannot translate it (a deleted
# account, an account of another computer, most capability SIDs).
function Get-AnetAccountName {
  param([string]$Sid)
  try {
    $n = (New-Object System.Security.Principal.SecurityIdentifier $Sid).Translate([System.Security.Principal.NTAccount]).Value
    if (-not [string]::IsNullOrEmpty($n)) { return $n }
  } catch { }
  return $Sid + ' (unresolved)'
}

# Get-AnetRuleMask is the access mask of a rule of Get-Acl as an unsigned
# 32-bit number (the generic rights are the high bits, which .NET shows as a
# negative FileSystemRights).
function Get-AnetRuleMask {
  param($Rule)
  return (([long][int]$Rule.FileSystemRights) -band [long]4294967295)
}

# Get-AnetRightsText says in words what an access mask grants.
function Get-AnetRightsText {
  param([long]$Mask)
  if (($Mask -band 0x10000000) -ne 0 -or ($Mask -band 0x1F01FF) -eq 0x1F01FF) { return 'full control' }
  $parts = New-Object System.Collections.Generic.List[string]
  if (($Mask -band ([long]2147483648 -bor 0x1 -bor 0x8)) -ne 0) { $parts.Add('read') }
  if (($Mask -band (0x40000000 -bor 0x2 -bor 0x4 -bor 0x10 -bor 0x100)) -ne 0) { $parts.Add('write') }
  if (($Mask -band (0x10000 -bor 0x40)) -ne 0) { $parts.Add('delete') }
  if (($Mask -band 0x40000) -ne 0) { $parts.Add('change permissions') }
  if (($Mask -band 0x80000) -ne 0) { $parts.Add('take ownership') }
  if (($Mask -band 0x20000000) -ne 0) { $parts.Add('execute') }
  if (($Mask -band 0x20000) -ne 0) { $parts.Add('read permissions') }
  if ($parts.Count -eq 0) { return ('access mask 0x{0:X8}' -f $Mask) }
  return ($parts -join ', ')
}

# $AnetHarmlessRights is what an allow rule may grant another account without
# making a directory less private: SYNCHRONIZE (0x100000), FILE_TRAVERSE
# (0x20, FILE_EXECUTE on a file) and FILE_READ_ATTRIBUTES (0x80). With these
# the account can pass through the directory to a path it already knows, and
# read its timestamps and attributes; it cannot list the directory, read or
# write a file in it, create, delete or rename anything, read its access
# control list, or change it. Windows gives such entries to the capability
# SIDs of app containers on the profile directory. READ_CONTROL is not in
# the set: it would let the account read who has access, which no reader of
# the directory needs. Every other right fails the check, the generic rights
# included.
$AnetHarmlessRights = [long]0x001000A0

# Test-AnetHarmlessMask: an access mask that grants nothing beyond
# $AnetHarmlessRights (an empty mask included).
function Test-AnetHarmlessMask {
  param([long]$Mask)
  return (($Mask -band (-bnot $AnetHarmlessRights) -band [long]4294967295) -eq 0)
}

# Get-AnetPrivacyProblem says why a directory is not private to this user,
# or returns $null when it is: it is not a reparse point, its owner is this
# user, SYSTEM or Administrators, and every Allow rule for any other account
# (inherit-only rules included, since they reach what is created inside)
# grants nothing beyond $AnetHarmlessRights. Accounts are named.
function Get-AnetPrivacyProblem {
  param([Parameter(Mandatory = $true)][string]$Path)
  $user = [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value
  $allowed = @($user, 'S-1-5-18', 'S-1-5-32-544')
  try {
    $item = Get-Item -LiteralPath $Path -Force -ErrorAction Stop
  } catch {
    return 'it cannot be read: ' + $_.Exception.Message
  }
  if (-not $item.PSIsContainer) { return 'it is not a directory' }
  if (($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) { return 'it is a reparse point (a link or junction)' }
  try {
    $acl = Get-Acl -LiteralPath $Path -ErrorAction Stop
    $owner = $acl.GetOwner([System.Security.Principal.SecurityIdentifier]).Value
    $rules = @($acl.GetAccessRules($true, $true, [System.Security.Principal.SecurityIdentifier]))
  } catch {
    return 'its access control list cannot be read: ' + $_.Exception.Message
  }
  if ($allowed -notcontains $owner) { return 'it is owned by ' + (Get-AnetAccountName $owner) }
  foreach ($r in $rules) {
    if ($r.AccessControlType -ne [System.Security.AccessControl.AccessControlType]::Allow) { continue }
    $sid = $r.IdentityReference.Value
    if ($allowed -contains $sid) { continue }
    $mask = Get-AnetRuleMask $r
    if (Test-AnetHarmlessMask $mask) { continue }
    return 'access is granted to ' + (Get-AnetAccountName $sid) + ' (' + (Get-AnetRightsText $mask) + ')'
  }
  return $null
}

# Get-AnetAncestorProblem says why a directory above $Dir lets another
# account move the install directory away, or returns $null when none does.
# Renaming a directory takes DELETE on it or FILE_DELETE_CHILD on its
# parent, and WRITE_DAC or WRITE_OWNER (or GENERIC_ALL) lets an account
# grant itself either; an account that owns a directory can change its
# access control list. Every existing directory above $Dir, up to the root
# of the drive, must be owned by this user, SYSTEM, Administrators or
# TrustedInstaller, and grant none of these rights to any other account.
# Rules that apply only to the children of a directory (inherit-only) do not
# apply to the directory itself and are not counted.
function Get-AnetAncestorProblem {
  param([Parameter(Mandatory = $true)][string]$Dir)
  $user = [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value
  $trusted = @($user, 'S-1-5-18', 'S-1-5-32-544', 'S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464')
  # DELETE, FILE_DELETE_CHILD, WRITE_DAC, WRITE_OWNER, GENERIC_ALL.
  $moving = 0x10000 -bor 0x40 -bor 0x40000 -bor 0x80000 -bor 0x10000000
  $ancestor = Split-Path -Parent $Dir
  while (-not [string]::IsNullOrEmpty($ancestor)) {
    if (Test-Path -LiteralPath $ancestor) {
      try {
        $acl = Get-Acl -LiteralPath $ancestor -ErrorAction Stop
        $owner = $acl.GetOwner([System.Security.Principal.SecurityIdentifier]).Value
        $rules = @($acl.GetAccessRules($true, $true, [System.Security.Principal.SecurityIdentifier]))
      } catch {
        return $ancestor + ': its access control list cannot be read: ' + $_.Exception.Message
      }
      if ($trusted -notcontains $owner) { return $ancestor + ': it is owned by ' + (Get-AnetAccountName $owner) }
      foreach ($r in $rules) {
        if ($r.AccessControlType -ne [System.Security.AccessControl.AccessControlType]::Allow) { continue }
        if (($r.PropagationFlags -band [System.Security.AccessControl.PropagationFlags]::InheritOnly) -ne 0) { continue }
        $sid = $r.IdentityReference.Value
        if ($trusted -contains $sid -or $sid -eq 'S-1-3-0' -or $sid -eq 'S-1-3-4') { continue }
        $mask = Get-AnetRuleMask $r
        if (($mask -band $moving) -ne 0) {
          return $ancestor + ': ' + (Get-AnetAccountName $sid) + ' may rename it or change its access control list (' + (Get-AnetRightsText $mask) + ')'
        }
      }
    }
    $next = Split-Path -Parent $ancestor
    if ($next -eq $ancestor) { break }
    $ancestor = $next
  }
  return $null
}

# Set-AnetPrivateAcl replaces the access control list of a directory this
# script created: no inherited rules, full control for this user, SYSTEM and
# Administrators, inherited by everything created inside. /inheritance:r
# removes the inherited rules and /grant:r replaces the rules of the three
# accounts that stay; every other account that has an explicit rule (allow
# or deny) is named after /remove, so that no other rule remains. The three
# accounts that stay are never named after /remove.
function Set-AnetPrivateAcl {
  param([Parameter(Mandatory = $true)][string]$Path)
  $user = [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value
  $keep = @($user, 'S-1-5-18', 'S-1-5-32-544')
  $others = @((Get-Acl -LiteralPath $Path).GetAccessRules($true, $false, [System.Security.Principal.SecurityIdentifier]) |
    ForEach-Object { $_.IdentityReference.Value } | Where-Object { $keep -notcontains $_ } | Sort-Object -Unique)
  $arguments = @($Path, '/inheritance:r')
  if ($others.Count -gt 0) {
    $arguments += '/remove'
    $arguments += @($others | ForEach-Object { '*' + $_ })
  }
  $arguments += @('/grant:r', ('*{0}:(OI)(CI)F' -f $user), '*S-1-5-18:(OI)(CI)F', '*S-1-5-32-544:(OI)(CI)F')
  $icacls = Join-Path ([Environment]::SystemDirectory) 'icacls.exe'
  $r = @{}
  Invoke-AnetNative -FilePath $icacls -Capture -Result $r -ArgumentList $arguments
  if ($r['Code'] -ne 0) {
    throw (New-AnetFailure ('icacls could not set the access control list of ' + $Path + ' (exit status ' + $r['Code'] + ')'))
  }
}

# Get-AnetDirectoryProblem says why $Dir cannot be the install directory, or
# returns $null: no directory above it may let another account move it
# (Get-AnetAncestorProblem), and it must be private to this user
# (Get-AnetPrivacyProblem), or, when it does not exist yet, the nearest
# directory above it that exists must be.
function Get-AnetDirectoryProblem {
  param([Parameter(Mandatory = $true)][string]$Dir)
  $why = Get-AnetAncestorProblem -Dir $Dir
  if ($null -ne $why) { return $why }
  if (Test-Path -LiteralPath $Dir) {
    $why = Get-AnetPrivacyProblem -Path $Dir
    if ($null -ne $why) { return $Dir + ': ' + $why }
    return $null
  }
  $ancestor = Split-Path -Parent $Dir
  while (-not [string]::IsNullOrEmpty($ancestor) -and -not (Test-Path -LiteralPath $ancestor)) {
    $ancestor = Split-Path -Parent $ancestor
  }
  if ([string]::IsNullOrEmpty($ancestor)) { return $Dir + ': no part of the path exists' }
  $why = Get-AnetPrivacyProblem -Path $ancestor
  if ($null -ne $why) { return $ancestor + ': ' + $why }
  return $null
}

# Initialize-AnetDirectory makes sure the install directory exists and is
# private to this user; the caller has checked it with
# Get-AnetDirectoryProblem. Each missing level is created on its own, below
# the previous one, gets the private access control list (Set-AnetPrivateAcl)
# and must then be private and empty; so %LOCALAPPDATA%\anet, created on the
# way to %LOCALAPPDATA%\anet\bin, is private as the node's runtime directory
# beside it requires.
function Initialize-AnetDirectory {
  param([Parameter(Mandatory = $true)][string]$Dir)
  $refusal = $Dir + ' is not private to this user'
  if (Test-Path -LiteralPath $Dir) {
    $why = Get-AnetPrivacyProblem -Path $Dir
    if ($null -ne $why) {
      Write-AnetLine ('   ' + $Dir + ': ' + $why)
      throw (New-AnetFailure $refusal)
    }
    return
  }
  $missing = New-Object System.Collections.Generic.List[string]
  $p = $Dir
  while (-not [string]::IsNullOrEmpty($p) -and -not (Test-Path -LiteralPath $p)) {
    $missing.Insert(0, $p)
    $p = Split-Path -Parent $p
  }
  if ([string]::IsNullOrEmpty($p)) {
    throw (New-AnetFailure ($Dir + ' cannot be created: no part of the path exists'))
  }
  $why = Get-AnetPrivacyProblem -Path $p
  if ($null -ne $why) {
    Write-AnetLine ('   ' + $p + ': ' + $why)
    throw (New-AnetFailure $refusal)
  }
  foreach ($level in $missing) {
    Write-AnetLine ('-> creating ' + $level)
    $null = [IO.Directory]::CreateDirectory($level)
    Set-AnetPrivateAcl -Path $level
    $why = Get-AnetPrivacyProblem -Path $level
    if ($null -ne $why) {
      Write-AnetLine ('   ' + $level + ': ' + $why)
      throw (New-AnetFailure $refusal)
    }
    if (@(Get-ChildItem -LiteralPath $level -Force).Count -ne 0) {
      Write-AnetLine ('   ' + $level + ': it was created by this installer and is not empty')
      throw (New-AnetFailure $refusal)
    }
  }
}

# Test-AnetRetryable: the rename failed because another process holds the
# file for a moment (access denied, sharing or lock violation).
function Test-AnetRetryable {
  param($Exception)
  if ($Exception -is [System.UnauthorizedAccessException]) { return $true }
  if ($Exception -is [System.IO.IOException]) {
    $code = $Exception.HResult -band 0xFFFF
    return ($code -eq 5 -or $code -eq 32 -or $code -eq 33)
  }
  return $false
}

# Move-AnetFile renames $From to $To (which must not exist), retrying for at
# most two seconds while another process holds one of them. With
# -Directory, $From is a directory.
function Move-AnetFile {
  param([string]$From, [string]$To, [switch]$Directory)
  $deadline = [DateTime]::UtcNow.AddSeconds(2)
  $wait = 10
  while ($true) {
    try {
      if ($Directory) { [IO.Directory]::Move($From, $To) } else { [IO.File]::Move($From, $To) }
      return
    } catch {
      $e = $_.Exception
      if ($e -is [System.Management.Automation.MethodInvocationException] -and $null -ne $e.InnerException) { $e = $e.InnerException }
      if (-not (Test-AnetRetryable $e) -or [DateTime]::UtcNow -ge $deadline) { throw $e }
      Start-Sleep -Milliseconds $wait
      $wait = [Math]::Min($wait * 2, 200)
    }
  }
}

function Test-AnetReparsePoint {
  param([string]$Path)
  $item = Get-Item -LiteralPath $Path -Force -ErrorAction Stop
  return (($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0)
}

# Get-AnetStagedPath is where the checked binary is staged before it takes
# the name anet.exe: <dir>\anet.new.exe. The name ends in .exe because
# PowerShell runs only a file whose extension is in PATHEXT as a program;
# any other file it hands to ShellExecute as a document, which would not
# run the binary (and might open it in another program).
function Get-AnetStagedPath {
  param([Parameter(Mandatory = $true)][string]$Dir)
  return (Join-Path $Dir 'anet.new.exe')
}

# Write-AnetStaged writes the checked binary to the staged path, a file that
# did not exist before.
function Write-AnetStaged {
  param([string]$Path, [byte[]]$Bytes)
  if (Test-Path -LiteralPath $Path) {
    if (Test-AnetReparsePoint -Path $Path) {
      throw (New-AnetFailure ($Path + ' is a reparse point (a link); remove it and run this again'))
    }
    Remove-Item -LiteralPath $Path -Force -ErrorAction Stop
  }
  $fs = New-Object System.IO.FileStream -ArgumentList $Path, ([IO.FileMode]::CreateNew), ([IO.FileAccess]::Write), ([IO.FileShare]::None)
  try {
    $fs.Write($Bytes, 0, $Bytes.Length)
    $fs.Flush($true)
  } finally {
    $fs.Dispose()
  }
}

# Confirm-AnetStaged runs the staged binary's own verification of the
# release (anet update --verify-dir) and checks what it reports.
function Confirm-AnetStaged {
  param([string]$Staged, [string]$TmpDir, [string]$Installed, [string]$Variant, [string]$Asset)
  $argv = @('update', '--verify-dir', $TmpDir)
  if ($Installed -ne '') { $argv += @('--installed', $Installed) }
  Write-AnetLine '-> anet update --verify-dir (the downloaded binary verifies the signed release manifest)'
  $r = Invoke-AnetCommand -Exe $Staged -ArgumentList $argv -TimeoutSeconds 120
  if ($r['Code'] -ne 0) {
    $reason = ''
    if (@($r['Stderr']).Count -gt 0) { $reason = ': ' + [string]@($r['Stderr'])[-1] }
    throw (New-AnetFailure ('the downloaded binary did not verify the release (exit status ' + $r['Code'] + ')' + $reason))
  }
  $lines = @($r['Stdout'])
  if ($lines.Count -ne 1) { throw (New-AnetFailure 'the verification printed no single answer line') }
  try {
    $v = [string]$lines[0] | ConvertFrom-Json
  } catch {
    throw (New-AnetFailure 'the verification printed an answer that is not JSON')
  }
  foreach ($k in @('version', 'commit', 'expires_at', 'key_fingerprint', 'variant', 'asset', 'modules')) {
    if (-not ($v.PSObject.Properties.Name -contains $k)) { throw (New-AnetFailure ('the verification answer lacks ' + $k)) }
  }
  if (-not ([string]$v.version -ceq $AnetRelease['Version'])) {
    throw (New-AnetFailure ('the verified release is ' + $v.version + ', this script installs ' + $AnetRelease['Version']))
  }
  if (-not ([string]$v.key_fingerprint -ceq $AnetReleaseKeyFingerprint)) {
    throw (New-AnetFailure ('the release is signed by ' + $v.key_fingerprint + ', not by the release key ' + $AnetReleaseKeyFingerprint))
  }
  if (-not ([string]$v.variant -ceq $Variant) -or -not ([string]$v.asset -ceq $Asset)) {
    throw (New-AnetFailure ('the verified build is ' + $v.asset + ' (' + $v.variant + '), expected ' + $Asset + ' (' + $Variant + ')'))
  }
  $mods = @($v.modules | ForEach-Object { [string]$_ })
  # PowerShell 7 reads a JSON date as a DateTime; show it as the manifest writes it.
  $expires = $v.expires_at
  if ($expires -is [DateTime]) {
    $expires = $expires.ToUniversalTime().ToString("yyyy-MM-dd'T'HH:mm:ss'Z'", [Globalization.CultureInfo]::InvariantCulture)
  }
  Write-AnetLine ('OK signature (release key ' + $v.key_fingerprint + '), release ' + $v.version + ', valid until ' + $expires)
  $shown = '(none)'
  if ($mods.Count -gt 0) { $shown = $mods -join ',' }
  Write-AnetLine ('OK modules: ' + $shown)
  return ,$mods
}

# Remove-AnetAsides removes binaries that earlier updates moved aside
# (anet.exe.old-<digits>), except $Keep; one still in use stays.
function Remove-AnetAsides {
  param([string]$Dir, [string]$Keep)
  foreach ($f in @(Get-ChildItem -LiteralPath $Dir -Force -ErrorAction SilentlyContinue)) {
    if ($f.PSIsContainer -or ($f.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) { continue }
    if (-not ($f.Name -match '^anet\.exe\.old-[0-9]+$') -or $f.FullName -eq $Keep) { continue }
    try { Remove-Item -LiteralPath $f.FullName -Force -ErrorAction Stop } catch { }
  }
}

# Install-AnetStaged puts the staged binary in place of anet.exe. A running
# anet.exe cannot be replaced, only renamed, so it is renamed aside first and
# renamed back if the staged file cannot take its name.
function Install-AnetStaged {
  param([string]$Staged, [string]$Exe)
  if (-not (Test-Path -LiteralPath $Exe)) {
    Move-AnetFile -From $Staged -To $Exe
    return ''
  }
  $aside = $Exe + '.old-' + [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds()
  try {
    Move-AnetFile -From $Exe -To $aside
  } catch {
    throw (New-AnetFailure ('cannot move the current ' + $Exe + ' aside: ' + $_.Exception.Message))
  }
  try {
    Move-AnetFile -From $Staged -To $Exe
  } catch {
    $first = $_.Exception.Message
    try {
      Move-AnetFile -From $aside -To $Exe
    } catch {
      throw (New-AnetFailure ('cannot put the new binary in place (' + $first + '), and the previous binary, now ' + $aside + ', could not be renamed back (' + $_.Exception.Message + '): rename it to ' + $Exe + ' by hand'))
    }
    throw (New-AnetFailure ('cannot put the new binary in place: ' + $first))
  }
  return $aside
}

# Save-AnetRecord keeps release.json and its signature beside the binary,
# where `anet doctor` verifies them again. The signature goes first: a
# failure between the two leaves a record that fails verification, which is
# reported, rather than one that is believed. Returns whether both were
# written.
function Save-AnetRecord {
  param([string]$Exe, [string]$TmpDir)
  foreach ($pair in @(@('release.json.sig', '.release.json.sig'), @('release.json', '.release.json'))) {
    $dst = $Exe + $pair[1]
    try {
      if (Test-Path -LiteralPath $dst) { Remove-Item -LiteralPath $dst -Force -ErrorAction Stop }
      [IO.File]::Copy((Join-Path $TmpDir $pair[0]), $dst, $false)
    } catch {
      Write-AnetLine ('Warning: could not write ' + $dst + '; anet doctor will report the release signature as unknown')
      return $false
    }
  }
  return $true
}

# ConvertTo-AnetArgument quotes one argument for a Windows command line the
# way the C runtime splits it again: unchanged when it has no space, tab or
# double quote; otherwise in double quotes, with each double quote and the
# backslashes before it (or before the closing quote) escaped.
function ConvertTo-AnetArgument {
  param([string]$Text)
  if ($Text -ne '' -and $Text -notmatch '[\s"]') { return $Text }
  $quoted = [regex]::Replace($Text, '(\\*)"', { param($m) $m.Groups[1].Value + $m.Groups[1].Value + '\"' })
  $quoted = [regex]::Replace($quoted, '(\\+)$', { param($m) $m.Groups[1].Value + $m.Groups[1].Value })
  return '"' + $quoted + '"'
}

# Invoke-AnetTimed runs a program other than icacls.exe and taskkill.exe:
# the anet this script installs, an earlier anet, npm, a fresh
# powershell.exe. Its standard input is closed at once, so it cannot wait
# for the console; its standard output and standard error are captured
# (Stdout, Stderr, and both in Output), for the log; and after
# $TimeoutSeconds it is ended, with every process it started in turn
# (Stop-AnetProcessTree), and reported as timed out. With $PathVariable, the
# program gets that PATH instead of this session's. A program that cannot
# start sets Code to -1.
function Invoke-AnetTimed {
  param([Parameter(Mandatory = $true)][string]$FilePath, [string[]]$ArgumentList = @(), [int]$TimeoutSeconds = 30,
        [string]$PathVariable = '', [Parameter(Mandatory = $true)][hashtable]$Result)
  $Result['Code'] = -1
  $Result['Output'] = @()
  $Result['Stdout'] = @()
  $Result['Stderr'] = @()
  $Result['TimedOut'] = $false
  $psi = New-Object System.Diagnostics.ProcessStartInfo
  $psi.FileName = $FilePath
  $psi.Arguments = (@($ArgumentList | ForEach-Object { ConvertTo-AnetArgument -Text ([string]$_) }) -join ' ')
  $psi.UseShellExecute = $false
  $psi.RedirectStandardInput = $true
  $psi.RedirectStandardOutput = $true
  $psi.RedirectStandardError = $true
  $psi.CreateNoWindow = $true
  if ($PathVariable -ne '') { $psi.EnvironmentVariables['Path'] = $PathVariable }
  $p = New-Object System.Diagnostics.Process
  $p.StartInfo = $psi
  try {
    $null = $p.Start()
  } catch {
    $Result['Output'] = @('cannot run ' + $FilePath + ': ' + $_.Exception.Message)
    $p.Dispose()
    return
  }
  try {
    $p.StandardInput.Close()
    $out = $p.StandardOutput.ReadToEndAsync()
    $err = $p.StandardError.ReadToEndAsync()
    if (-not $p.WaitForExit($TimeoutSeconds * 1000)) {
      $Result['TimedOut'] = $true
      Stop-AnetProcessTree -Process $p
      return
    }
    $p.WaitForExit()
    $Result['Code'] = $p.ExitCode
    $stdout = ''
    $stderr = ''
    if ($out.Wait(5000)) { $stdout = [string]$out.Result }
    if ($err.Wait(1000)) { $stderr = [string]$err.Result }
    $Result['Stdout'] = @($stdout -split "`r?`n" | Where-Object { ([string]$_).Trim() -ne '' })
    $Result['Stderr'] = @($stderr -split "`r?`n" | Where-Object { ([string]$_).Trim() -ne '' })
    $Result['Output'] = @($Result['Stdout']) + @($Result['Stderr'])
  } finally {
    $p.Dispose()
  }
}

# Stop-AnetProcessTree ends the process that Invoke-AnetTimed started, by
# its process object, and every process it started in turn (npm.cmd is a
# cmd.exe with node.exe below it): Kill(true) where .NET has it (PowerShell
# 7), else taskkill /T /PID with that one process ID, else the one process.
# No process is ever chosen by its name.
function Stop-AnetProcessTree {
  param([Parameter(Mandatory = $true)]$Process)
  $killTree = $Process.GetType().GetMethod('Kill', [type[]]@([bool]))
  if ($null -ne $killTree) {
    try { $Process.Kill($true); return } catch { }
  }
  if ([Environment]::OSVersion.Platform -eq [PlatformID]::Win32NT) {
    $r = @{}
    Invoke-AnetNative -FilePath (Join-Path ([Environment]::SystemDirectory) 'taskkill.exe') -ArgumentList @('/T', '/F', '/PID', [string]$Process.Id) -Capture -Result $r
    if ($r['Code'] -eq 0) { return }
  }
  try { $Process.Kill() } catch { }
}

# Invoke-AnetCommand runs an anet.exe (the one this script installs, or the
# staged one) through Invoke-AnetTimed and puts the command line, its output
# and its exit status in the log. Returns the result of Invoke-AnetTimed.
function Invoke-AnetCommand {
  param([Parameter(Mandatory = $true)][string]$Exe, [string[]]$ArgumentList = @(), [int]$TimeoutSeconds = 120)
  $r = @{}
  Invoke-AnetTimed -FilePath $Exe -ArgumentList $ArgumentList -TimeoutSeconds $TimeoutSeconds -Result $r
  Write-AnetLine ('$ anet ' + (@($ArgumentList | ForEach-Object { ConvertTo-AnetArgument -Text ([string]$_) }) -join ' '))
  foreach ($l in @($r['Output'])) { Write-AnetLine ('  ' + $l) }
  if ($r['TimedOut']) {
    Write-AnetLine ('  (ended after ' + $TimeoutSeconds + ' seconds)')
  } else {
    Write-AnetLine ('  (exit status ' + $r['Code'] + ')')
  }
  return $r
}

# Test-AnetOneShotHost reports whether this PowerShell process was started
# to run one command (powershell.exe -Command, -EncodedCommand or
# -CommandWithArgs, without -NoExit): then the script may end it with the
# result as the exit status, which is how the result reaches cmd.exe, a
# POSIX shell or the PowerShell that started it. Anything after the command
# option is the command itself and is not read.
function Test-AnetOneShotHost {
  foreach ($a in @([Environment]::GetCommandLineArgs() | Select-Object -Skip 1)) {
    $t = ([string]$a).ToLowerInvariant()
    if ($t -match '^[-/]noe(x(i(t)?)?)?$') { return $false }
    if ($t -match '^[-/](c(o(m(m(a(n(d)?)?)?)?)?)?|cwa|commandwithargs|e|ec|en(c(o(d(e(d(c(o(m(m(a(n(d)?)?)?)?)?)?)?)?)?)?)?)?)$') { return $true }
  }
  return $false
}

# Get-AnetPathEntries is the PATH of the user or of the machine as the
# registry holds it: each directory expanded, without a trailing backslash,
# empty entries left out.
function Get-AnetPathEntries {
  param([ValidateSet('User', 'Machine')][string]$Scope)
  $raw = [string][Environment]::GetEnvironmentVariable('Path', $Scope)
  $out = New-Object System.Collections.Generic.List[string]
  foreach ($e in $raw.Split(';')) {
    $d = [Environment]::ExpandEnvironmentVariables($e.Trim()).TrimEnd('\')
    if ($d -ne '') { $out.Add($d) }
  }
  return $out.ToArray()
}

# Test-AnetFileContains reports whether a file holds an ASCII text, read in
# blocks of one megabyte (a binary of tens of megabytes is never one
# string), with the file shared for reading only. A file that cannot be
# opened does not contain the text.
function Test-AnetFileContains {
  param([string]$Path, [string]$Text)
  try {
    $fs = New-Object System.IO.FileStream -ArgumentList $Path, ([IO.FileMode]::Open), ([IO.FileAccess]::Read), ([IO.FileShare]::Read)
  } catch {
    return $false
  }
  try {
    $latin1 = [Text.Encoding]::GetEncoding(28591)
    $buf = New-Object byte[] 1048576
    $tail = ''
    while ($true) {
      $n = $fs.Read($buf, 0, $buf.Length)
      if ($n -le 0) { break }
      $chunk = $tail + $latin1.GetString($buf, 0, $n)
      if ($chunk.IndexOf($Text, [StringComparison]::Ordinal) -ge 0) { return $true }
      if ($chunk.Length -ge $Text.Length) { $tail = $chunk.Substring($chunk.Length - $Text.Length + 1) } else { $tail = $chunk }
    }
    return $false
  } catch {
    return $false
  } finally {
    $fs.Dispose()
  }
}

# Get-AnetBinaryFamily says whose program an anet.exe is, by the Go module
# path in the file, as install.sh does: 'v1' for AgentNetwork 1.x, the
# earlier project (ChatChatTech/AgentNetwork); 'anet' for this project
# (github.com/ANetResearch/ANet; on Windows that is 0.3.4 or later, the
# first Windows build); '' for anything else, which this script never runs.
function Get-AnetBinaryFamily {
  param([string]$Exe)
  if (-not (Test-Path -LiteralPath $Exe -PathType Leaf)) { return '' }
  if (Test-AnetFileContains -Path $Exe -Text 'ChatChatTech/AgentNetwork') { return 'v1' }
  if (Test-AnetFileContains -Path $Exe -Text 'github.com/ANetResearch/ANet') { return 'anet' }
  return ''
}

# Test-AnetVersion: $Text is a version in the form x.y.z or x.y.z-pre and
# nothing else (the rule of install.sh's semver_ok); text read from a file
# or from the output of a program found on this machine is used only when
# it passes.
function Test-AnetVersion {
  param([string]$Text)
  return ($Text -cmatch '^[0-9]{1,6}\.[0-9]{1,6}\.[0-9]{1,6}(-[0-9A-Za-z.-]{1,40})?$')
}

# Compare-AnetVersion orders two versions that pass Test-AnetVersion by
# their three numbers: -1, 0 or 1.
function Compare-AnetVersion {
  param([string]$A, [string]$B)
  $x = @(($A -split '-', 2)[0].Split('.') | ForEach-Object { [long]$_ })
  $y = @(($B -split '-', 2)[0].Split('.') | ForEach-Object { [long]$_ })
  for ($i = 0; $i -lt 3; $i++) {
    if ($x[$i] -lt $y[$i]) { return -1 }
    if ($x[$i] -gt $y[$i]) { return 1 }
  }
  return 0
}

# Get-AnetRecordVersion is the version of the release manifest kept beside
# an installed anet.exe (<exe>.release.json, written by this installer and
# by anet update), or '' when there is none that reads.
function Get-AnetRecordVersion {
  param([string]$Exe)
  $record = $Exe + '.release.json'
  if (-not (Test-Path -LiteralPath $record -PathType Leaf)) { return '' }
  try {
    $m = [IO.File]::ReadAllText($record) | ConvertFrom-Json
    if ((@($m.PSObject.Properties.Name) -contains 'version') -and (Test-AnetVersion ([string]$m.version))) { return [string]$m.version }
  } catch { }
  return ''
}

# Get-AnetVersionInfo classifies an anet.exe found on this machine: Current
# for an anet of this project (0.3 or later), Legacy for AgentNetwork 1.x,
# Unknown for a file that carries neither module path (Get-AnetBinaryFamily),
# which is never run. The version comes from the release record beside a
# Current one, or, when that is missing, from `<exe> version` run with a
# time limit; a Legacy one is run for its version as well. From a window
# that has more rights than the account's programs (Restricted), nothing
# found on the machine is run, and a version that cannot be read stays ''.
function Get-AnetVersionInfo {
  param([string]$Exe, [hashtable]$Elevation)
  $info = @{ Class = 'Unknown'; Version = ''; Family = (Get-AnetBinaryFamily -Exe $Exe) }
  if ($info.Family -eq '') { return $info }
  if ($info.Family -eq 'v1') {
    $info.Class = 'Legacy'
  } else {
    $info.Class = 'Current'
    $info.Version = Get-AnetRecordVersion -Exe $Exe
  }
  if ($Elevation.Restricted -or $info.Version -ne '') { return $info }
  $r = @{}
  Invoke-AnetTimed -FilePath $Exe -ArgumentList @('version') -TimeoutSeconds 15 -Result $r
  $lines = @($r['Output'])
  if ($r['TimedOut'] -or $r['Code'] -ne 0 -or $lines.Count -eq 0) { return $info }
  $first = ([string]$lines[0]).Trim()
  $v = ''
  if ($info.Family -eq 'anet') {
    if ($first -cmatch '^anet (\S+)( |$)') { $v = $Matches[1] }
  } elseif ($first -cmatch '(^|\s)v?([0-9]+\.[0-9]+\.[0-9]+)(\s|$)') {
    $v = $Matches[2]
  }
  if (Test-AnetVersion $v) { $info.Version = $v }
  return $info
}

# Test-AnetSameDir compares two directory paths as Windows does: without
# case, and without a trailing backslash.
function Test-AnetSameDir {
  param([string]$A, [string]$B)
  return ($A.TrimEnd('\') -ieq $B.TrimEnd('\'))
}

# Get-AnetRunValueExe is the program the Run value AgentNetwork starts, or ''.
function Get-AnetRunValueExe {
  $key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Software\Microsoft\Windows\CurrentVersion\Run')
  if ($null -eq $key) { return '' }
  try {
    $v = [string]$key.GetValue('AgentNetwork', '')
  } finally {
    $key.Close()
  }
  if ($v -match '^"([^"]+)"') { return $Matches[1] }
  return ''
}

# Test-AnetDedicatedDir: the directory holds nothing but the files of an
# anet installation (anet.exe, its release record and signature, binaries
# moved aside by an update, a staged binary, the temporary file of an
# interrupted anet update), or nothing at all. Only such a
# directory is taken over as the install directory or taken off the PATH:
# one that holds other programs is someone else's.
function Test-AnetDedicatedDir {
  param([string]$Dir)
  foreach ($f in @(Get-ChildItem -LiteralPath $Dir -Force -ErrorAction SilentlyContinue)) {
    if ($f.PSIsContainer) { return $false }
    if (-not ($f.Name -match '^(anet\.exe|anet\.exe\.release\.json(\.sig)?|anet\.exe\.old-[0-9]+|anet\.new\.exe|\.anet-update-[0-9A-Za-z]+\.exe)$')) { return $false }
  }
  return $true
}

# Get-AnetExistingInstalls lists the directories of anet 0.3 or later that
# are already installed for this user: an anet.exe of this project in a
# directory of the user's PATH, or the program of the Run value. Each
# directory once, with whether it holds anet's files alone (Dedicated).
function Get-AnetExistingInstalls {
  param([hashtable]$Elevation)
  $found = New-Object System.Collections.Generic.List[hashtable]
  $dirs = New-Object System.Collections.Generic.List[string]
  $runExe = Get-AnetRunValueExe
  if ($runExe -ne '') { $dirs.Add((Split-Path -Parent $runExe)) }
  foreach ($d in @(Get-AnetPathEntries -Scope User)) { $dirs.Add($d) }
  $seen = New-Object System.Collections.Generic.List[string]
  foreach ($d in $dirs) {
    if ([string]::IsNullOrEmpty($d) -or @($seen | Where-Object { Test-AnetSameDir $_ $d }).Count -gt 0) { continue }
    $seen.Add($d)
    $exe = Join-Path $d 'anet.exe'
    if (-not (Test-Path -LiteralPath $exe -PathType Leaf)) { continue }
    $info = Get-AnetVersionInfo -Exe $exe -Elevation $Elevation
    if ($info.Class -eq 'Current') {
      $found.Add(@{ Dir = $d; Exe = $exe; Version = $info.Version; Dedicated = (Test-AnetDedicatedDir -Dir $d) })
    }
  }
  return $found.ToArray()
}

# Select-AnetInstallDir chooses the install directory when -Prefix is not
# given: the first candidate for which Get-AnetDirectoryProblem finds
# nothing. The candidates are an anet 0.3 or later already installed for
# this user in a directory of its own (Get-AnetExistingInstalls),
# %LOCALAPPDATA%\Programs\anet, %LOCALAPPDATA%\anet\bin and
# %USERPROFILE%\.anet-bin. Each candidate that is skipped is reported in one
# line, with the reason.
function Select-AnetInstallDir {
  param([hashtable[]]$Existing = @())
  $candidates = New-Object System.Collections.Generic.List[string]
  foreach ($e in $Existing) {
    if ($e.Dedicated) {
      $candidates.Add($e.Dir)
    } else {
      Write-AnetLine ('   skipped ' + $e.Dir + ': it holds an anet installation beside other files, so it is not anet''s directory')
    }
  }
  $local = [Environment]::GetFolderPath('LocalApplicationData')
  if (-not [string]::IsNullOrEmpty($local)) {
    $candidates.Add((Join-Path (Join-Path $local 'Programs') 'anet'))
    $candidates.Add((Join-Path (Join-Path $local 'anet') 'bin'))
  }
  $profileDir = [Environment]::GetFolderPath('UserProfile')
  if (-not [string]::IsNullOrEmpty($profileDir)) { $candidates.Add((Join-Path $profileDir '.anet-bin')) }
  $seen = New-Object System.Collections.Generic.List[string]
  foreach ($c in $candidates) {
    $c = [IO.Path]::GetFullPath($c).TrimEnd('\')
    if (@($seen | Where-Object { Test-AnetSameDir $_ $c }).Count -gt 0) { continue }
    $seen.Add($c)
    $why = Get-AnetDirectoryProblem -Dir $c
    if ($null -eq $why) { return $c }
    Write-AnetLine ('   skipped ' + $c + ': ' + $why)
  }
  throw (New-AnetFailure 'no install directory passed the checks (the reasons are above); give -Prefix with a directory that is private to this user')
}

# Join-AnetPath joins a directory and the parts of a relative path given as
# 'a\b\c', with the separator of this system.
function Join-AnetPath {
  param([string]$Dir, [string]$Relative)
  $p = $Dir
  foreach ($part in $Relative.Split('\')) { $p = [IO.Path]::Combine($p, $part) }
  return $p
}

# Test-AnetLegacyPackage: the package.json ($Text, with its version
# $Version) is the npm package @agentnetwork/anet of the earlier project,
# AgentNetwork 1.x, by the rule of install.sh: its repository names
# ChatChatTech/AgentNetwork, or its version is below 0.3.0, or it is 1.0 or
# later and does not name ANetResearch/ANet (anet's own npm packages exist
# from 0.3.4 on and do).
function Test-AnetLegacyPackage {
  param([string]$Text, [string]$Version)
  if ($Text.Contains('ChatChatTech/AgentNetwork')) { return $true }
  if ((Compare-AnetVersion $Version '0.3.0') -lt 0) { return $true }
  return ((Compare-AnetVersion $Version '1.0.0') -ge 0 -and -not $Text.Contains('ANetResearch/ANet'))
}

# Get-AnetNpmPackageInfo reads node_modules\@agentnetwork\anet\package.json
# under the npm prefix $Dir (whose bin shims are $Dir\anet.cmd and
# $Dir\anet.ps1): $null when the package is not there, or its package.json
# does not name @agentnetwork/anet with a version in x.y.z form; otherwise
# the version and whether it is the 1.x package (Test-AnetLegacyPackage).
function Get-AnetNpmPackageInfo {
  param([string]$Dir)
  $pj = Join-AnetPath $Dir 'node_modules\@agentnetwork\anet\package.json'
  if (-not (Test-Path -LiteralPath $pj -PathType Leaf)) { return $null }
  try {
    $text = [IO.File]::ReadAllText($pj)
    $j = $text | ConvertFrom-Json
  } catch {
    return $null
  }
  $names = @($j.PSObject.Properties.Name)
  if (-not ($names -contains 'name') -or -not (([string]$j.name) -ceq '@agentnetwork/anet')) { return $null }
  if (-not ($names -contains 'version') -or -not (Test-AnetVersion ([string]$j.version))) { return $null }
  return @{ Version = [string]$j.version; Legacy = (Test-AnetLegacyPackage -Text $text -Version ([string]$j.version)) }
}

# Find-AnetOthers lists every other anet of this user and machine: anet.exe,
# anet.cmd and anet.ps1 in the directories of the machine's and the user's
# PATH and in the global npm prefix, except in $InstallDir. Each entry has
# the directory, what was found there, its class (Current, Legacy, Unknown),
# its version, whether it is an npm installation of @agentnetwork/anet, and
# for a legacy one the binary that can stop it: only a binary that carries
# the module path of AgentNetwork 1.x (StopNote says why there is none).
function Find-AnetOthers {
  param([string]$InstallDir, [string]$NpmPrefix, [hashtable]$Elevation)
  $dirs = New-Object System.Collections.Generic.List[string]
  foreach ($d in @(Get-AnetPathEntries -Scope Machine)) { $dirs.Add($d) }
  foreach ($d in @(Get-AnetPathEntries -Scope User)) { $dirs.Add($d) }
  if ($NpmPrefix -ne '') { $dirs.Add($NpmPrefix.TrimEnd('\')) }
  $seen = New-Object System.Collections.Generic.List[string]
  $found = New-Object System.Collections.Generic.List[hashtable]
  foreach ($d in $dirs) {
    if (@($seen | Where-Object { Test-AnetSameDir $_ $d }).Count -gt 0) { continue }
    $seen.Add($d)
    if (Test-AnetSameDir $d $InstallDir) { continue }
    $names = @(@('anet.exe', 'anet.cmd', 'anet.ps1') | Where-Object { Test-Path -LiteralPath (Join-Path $d $_) -PathType Leaf })
    if ($names.Count -eq 0) { continue }
    $e = @{ Dir = $d; Names = $names; Class = 'Unknown'; Version = ''; Npm = $false; StopExe = ''; StopNote = ''; ExeOnPath = $false }
    $pkg = Get-AnetNpmPackageInfo -Dir $d
    if ($null -ne $pkg) {
      $e.Npm = $true
      $e.Version = $pkg.Version
      if ($pkg.Legacy) { $e.Class = 'Legacy' } else { $e.Class = 'Current' }
      if ($pkg.Legacy) {
        foreach ($cpu in @('x64', 'arm64')) {
          foreach ($rel in @(('node_modules\@agentnetwork\anet\node_modules\@agentnetwork\anet-win32-' + $cpu + '\bin\anet.exe'),
                             ('node_modules\@agentnetwork\anet-win32-' + $cpu + '\bin\anet.exe'))) {
            $candidate = Join-AnetPath $d $rel
            if (-not (Test-Path -LiteralPath $candidate -PathType Leaf)) { continue }
            if ((Get-AnetBinaryFamily -Exe $candidate) -eq 'v1') {
              $e.StopExe = $candidate
            } elseif ($e.StopNote -eq '') {
              $e.StopNote = $candidate + ' is not a binary of AgentNetwork 1.x, so it is not run'
            }
            break
          }
          if ($e.StopExe -ne '') { break }
        }
      }
    } elseif ($names -contains 'anet.exe') {
      $e.ExeOnPath = $true
      $info = Get-AnetVersionInfo -Exe (Join-Path $d 'anet.exe') -Elevation $Elevation
      $e.Class = $info.Class
      $e.Version = $info.Version
      if ($e.Class -eq 'Legacy') { $e.StopExe = Join-Path $d 'anet.exe' }
    }
    $found.Add($e)
  }
  return $found.ToArray()
}

# Find-AnetNpm is npm.cmd: on this session's PATH, or in a directory of the
# machine's or the user's PATH. '' when there is none.
function Find-AnetNpm {
  $c = @(Get-Command -Name 'npm.cmd' -CommandType Application -ErrorAction SilentlyContinue)
  if ($c.Count -gt 0) { return [string]$c[0].Source }
  foreach ($d in (@(Get-AnetPathEntries -Scope Machine) + @(Get-AnetPathEntries -Scope User))) {
    $p = Join-Path $d 'npm.cmd'
    if (Test-Path -LiteralPath $p -PathType Leaf) { return $p }
  }
  return ''
}

# Get-AnetNpmDefaultPrefix is where npm installs global packages for a user
# unless configured otherwise, %APPDATA%\npm, when it exists; '' otherwise.
# It stands in for `npm prefix -g` where npm is not run.
function Get-AnetNpmDefaultPrefix {
  $appData = [Environment]::GetFolderPath('ApplicationData')
  if ([string]::IsNullOrEmpty($appData)) { return '' }
  $p = Join-Path $appData 'npm'
  if (Test-Path -LiteralPath $p -PathType Container) { return $p }
  return ''
}

# Test-AnetBatchSafe: npm.cmd is a batch file, which cmd.exe runs and whose
# arguments it reads again with its own rules. An argument with one of
# these characters is not passed to it; the installer prints the command
# instead.
function Test-AnetBatchSafe {
  param([string]$Text)
  return -not ($Text -match '["%!^&|<>()\r\n]')
}

# The data directory rule, the same as the daemon's (internal/daemon
# ClassifyDataDir, V1Markers) and install.sh's (datadir_kind, V1_MARKERS,
# V1_ENTRIES); a test holds the three equal on the same directories.
#   $AnetV1Markers  entries AgentNetwork 1.x writes when its daemon first
#                   starts and that no release of anet writes
#   $AnetV1Entries  what 1.x keeps in its data directory, moved out of a
#                   mixed directory; not its daemon.log, a name 0.3 writes too
$AnetV1Markers = @('api_token', 'agent_token', 'scoped_tokens.json', 'peer-journal.json', 'anet\identity.json', 'anet\api_token')
$AnetV1Entries = @('anet', 'anet.db', 'anet.db-shm', 'anet.db-wal', 'api_token', 'agent_token', 'scoped_tokens.json', 'peer-journal.json', 'cas', 'orgs', 'state', 'daemon.pid')
$AnetV1Port = 3998

# Get-AnetIdentityFileAge: older, newer, current or invalid, the identity
# file's part of the rule. An identity file is a CBOR map: anet 0.2's has no
# key 0 (its first key is 1), this version's starts with key 0, the type tag
# anet.identity/2; a tag above 2 is a newer anet's, one below 2 an older
# one's; anything else, an empty or unreadable file included, is invalid.
function Get-AnetIdentityFileAge {
  param([string]$Path)
  try {
    $b = [IO.File]::ReadAllBytes($Path)
  } catch {
    return 'invalid'
  }
  if ($b.Length -lt 2) { return 'invalid' }
  $head = [int]$b[0]
  $key = [int]$b[1]
  # 0xA1-0xAF and 0xB0-0xB7: a map of 1 to 23 entries. Its first key is
  # then one byte: 0x00 is key 0, 0x01-0x17 are keys 1 to 23.
  if (-not (($head -ge 0xA1 -and $head -le 0xAF) -or ($head -ge 0xB0 -and $head -le 0xB7))) { return 'invalid' }
  if ($key -ge 0x01 -and $key -le 0x17) { return 'older' }
  if ($key -ne 0) { return 'invalid' }
  $text = [Text.Encoding]::GetEncoding(28591).GetString($b)
  $digits = ''
  foreach ($token in [regex]::Split($text, '[^a-z0-9./]+')) {
    if ($token -cmatch 'anet\.identity/([0-9]+)$') { $digits = $Matches[1]; break }
  }
  if (-not ($digits -cmatch '^(0|[1-9][0-9]{0,8})$')) { return 'invalid' }
  $n = [long]$digits
  if ($n -gt 2) { return 'newer' }
  if ($n -lt 2) { return 'older' }
  return 'current'
}

# Get-AnetDataDirKind: none, current, older, newer, invalid, v1 or mixed.
# The identity file decides first; without one, the 1.x markers do, and an
# identity of this version under ids\<name> makes the directory mixed
# rather than 1.x's. The installer moves a directory of kind older or v1
# aside whole, moves only the 1.x entries out of one of kind mixed, and
# leaves every other kind alone: a current directory, a newer anet's, and
# one whose identity file does not read (anet doctor says what is wrong).
function Get-AnetDataDirKind {
  param([string]$Root)
  if (-not (Test-Path -LiteralPath $Root -PathType Container)) { return 'none' }
  if (@(Get-ChildItem -LiteralPath $Root -Force -ErrorAction SilentlyContinue).Count -eq 0) { return 'none' }
  $v1 = $false
  foreach ($m in $AnetV1Markers) {
    if (Test-Path -LiteralPath (Join-AnetPath $Root $m)) { $v1 = $true; break }
  }
  $kel = Join-Path $Root 'identity.kel'
  if (Test-Path -LiteralPath $kel -PathType Leaf) {
    $age = Get-AnetIdentityFileAge -Path $kel
    if ($age -eq 'current' -and $v1) { return 'mixed' }
    return $age
  }
  if (-not $v1) { return 'current' }
  foreach ($d in @(Get-ChildItem -LiteralPath (Join-Path $Root 'ids') -Directory -Force -ErrorAction SilentlyContinue)) {
    $named = Join-Path $d.FullName 'identity.kel'
    if ((Test-Path -LiteralPath $named -PathType Leaf) -and (Get-AnetIdentityFileAge -Path $named) -eq 'current') { return 'mixed' }
  }
  return 'v1'
}

# Test-AnetLoopbackListens: something accepts connections on 127.0.0.1:$Port.
function Test-AnetLoopbackListens {
  param([int]$Port)
  $c = New-Object System.Net.Sockets.TcpClient
  try {
    $ar = $c.BeginConnect('127.0.0.1', $Port, $null, $null)
    if (-not $ar.AsyncWaitHandle.WaitOne(1000)) { return $false }
    $c.EndConnect($ar)
    return $true
  } catch {
    return $false
  } finally {
    $c.Close()
  }
}

# Get-AnetProcessPath is the program file of the process $Id, or '' when it
# cannot be read (no such process, or one of another account).
function Get-AnetProcessPath {
  param([int]$Id)
  try {
    $p = Get-Process -Id $Id -ErrorAction Stop
  } catch {
    return ''
  }
  $path = ''
  try { $path = [string]$p.Path } catch { }
  if ($path -eq '' -and $null -ne (Get-Command -Name 'Get-CimInstance' -ErrorAction SilentlyContinue)) {
    try {
      $c = Get-CimInstance -ClassName Win32_Process -Filter ('ProcessId = ' + $Id) -ErrorAction Stop
      if ($null -ne $c) { $path = [string]$c.ExecutablePath }
    } catch { }
  }
  return $path
}

# Get-AnetPortOwner is the process ID that listens on TCP port $Port: 0
# when none does, -1 when Windows cannot be asked (no Get-NetTCPConnection).
function Get-AnetPortOwner {
  param([int]$Port)
  if ($null -eq (Get-Command -Name 'Get-NetTCPConnection' -ErrorAction SilentlyContinue)) { return -1 }
  try {
    $c = @(Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction Stop)
    if ($c.Count -gt 0) { return [int]$c[0].OwningProcess }
  } catch { }
  return 0
}

# Get-AnetLegacyDaemons lists the daemons of AgentNetwork 1.x that may hold
# the data directory $Root: the process of its daemon.pid, and the process
# that listens on 127.0.0.1:3998, each only when its program carries the
# module path of AgentNetwork 1.x (Get-AnetBinaryFamily v1). A process of
# anet 0.3 or later, a process of another program that reuses the ID in a
# stale daemon.pid, and a process this account cannot read (another
# account's) are not such daemons. Unknown is set when something answers on
# 127.0.0.1:3998 and Windows cannot say which process it is.
function Get-AnetLegacyDaemons {
  param([string]$Root)
  $found = New-Object System.Collections.Generic.List[hashtable]
  $ids = New-Object System.Collections.Generic.List[int]
  $pidFile = Join-Path $Root 'daemon.pid'
  if (Test-Path -LiteralPath $pidFile -PathType Leaf) {
    try {
      $text = [IO.File]::ReadAllText($pidFile)
      if ($text.Length -gt 32) { $text = $text.Substring(0, 32) }
      $digits = $text -replace '[^0-9]', ''
      if ($digits.Length -gt 0 -and $digits.Length -le 9) { $ids.Add([int]$digits) }
    } catch { }
  }
  $unknown = $false
  if (Test-AnetLoopbackListens -Port $AnetV1Port) {
    $owner = Get-AnetPortOwner -Port $AnetV1Port
    if ($owner -lt 0) { $unknown = $true } elseif ($owner -gt 0 -and -not $ids.Contains($owner)) { $ids.Add($owner) }
  }
  foreach ($id in $ids) {
    if ($id -le 0) { continue }
    $exe = Get-AnetProcessPath -Id $id
    if ($exe -ne '' -and (Get-AnetBinaryFamily -Exe $exe) -eq 'v1') { $found.Add(@{ Pid = $id; Exe = $exe }) }
  }
  return @{ Daemons = $found.ToArray(); Unknown = $unknown }
}

# Test-AnetDataDirBusy: a daemon of AgentNetwork 1.x may still hold the
# data directory (Get-AnetLegacyDaemons found one, or cannot tell who
# answers on 127.0.0.1:3998).
function Test-AnetDataDirBusy {
  param([string]$Root)
  $l = Get-AnetLegacyDaemons -Root $Root
  return (@($l.Daemons).Count -gt 0 -or $l.Unknown)
}

# Wait-AnetDataDirFree waits, at most $Seconds, until Test-AnetDataDirBusy
# no longer says busy; returns whether the directory is free.
function Wait-AnetDataDirFree {
  param([string]$Root, [int]$Seconds)
  $deadline = [DateTime]::UtcNow.AddSeconds($Seconds)
  while (Test-AnetDataDirBusy -Root $Root) {
    if ([DateTime]::UtcNow -ge $deadline) { return $false }
    Start-Sleep -Seconds 1
  }
  return $true
}

# Get-AnetLegacyToken is the API token AgentNetwork 1.x keeps in its data
# directory (api_token, or anet\api_token), as its own stop command reads
# it; '' when there is none that reads. It is sent to 127.0.0.1 only and
# never written to the log.
function Get-AnetLegacyToken {
  param([string]$Root)
  foreach ($rel in @('api_token', 'anet\api_token')) {
    $f = Join-AnetPath $Root $rel
    try {
      if (-not (Test-Path -LiteralPath $f -PathType Leaf) -or (Get-Item -LiteralPath $f -Force).Length -gt 4096) { continue }
      $t = ([IO.File]::ReadAllText($f)).Trim()
      if ($t -cmatch '^[\x21-\x7e]{1,4096}$') { return $t }
    } catch { }
  }
  return ''
}

# Invoke-AnetLegacyShutdown asks the daemon of AgentNetwork 1.x on
# 127.0.0.1:3998 to shut down (POST /api/shutdown, what its stop command
# sends), with its token when there is one. Returns what it answered.
function Invoke-AnetLegacyShutdown {
  param([string]$Token)
  $client = New-AnetHttpClient -TimeoutSeconds 5
  try {
    $req = New-Object System.Net.Http.HttpRequestMessage -ArgumentList ([System.Net.Http.HttpMethod]::Post), ('http://127.0.0.1:' + $AnetV1Port + '/api/shutdown')
    if ($Token -ne '') { $req.Headers.Authorization = New-Object System.Net.Http.Headers.AuthenticationHeaderValue -ArgumentList 'Bearer', $Token }
    $req.Content = New-Object System.Net.Http.StringContent -ArgumentList '{}', ([Text.Encoding]::UTF8), 'application/json'
    $resp = $client.SendAsync($req).GetAwaiter().GetResult()
    try { return ('HTTP ' + [int]$resp.StatusCode) } finally { $resp.Dispose() }
  } catch {
    return $_.Exception.GetBaseException().Message
  } finally {
    $client.Dispose()
  }
}

# Stop-AnetLegacyDaemon stops a daemon of AgentNetwork 1.x that still holds
# the data directory $Root after the stop commands of the earlier
# installations ran (or none was found): first over its local API with the
# token from $Root, then, when it has not exited within 10 seconds, by its
# process ID, each ID checked again just before (its program carries the
# module path of AgentNetwork 1.x). No other process is ended, and none by
# name. Returns whether the directory is then free.
function Stop-AnetLegacyDaemon {
  param([string]$Root)
  $l = Get-AnetLegacyDaemons -Root $Root
  if (@($l.Daemons).Count -eq 0 -and -not $l.Unknown) { return $true }
  foreach ($d in @($l.Daemons)) { Write-AnetLine ('a daemon of AgentNetwork 1.x still holds ' + $Root + ': process ' + $d.Pid + ' (' + $d.Exe + ')') }
  if ($l.Unknown) { Write-AnetLine ('something answers on 127.0.0.1:' + $AnetV1Port + ', and Windows cannot say which process it is') }
  if (Test-AnetLoopbackListens -Port $AnetV1Port) {
    $token = Get-AnetLegacyToken -Root $Root
    if ($token -ne '') {
      $answer = Invoke-AnetLegacyShutdown -Token $token
      Write-AnetLine ('asked it to shut down (POST http://127.0.0.1:' + $AnetV1Port + '/api/shutdown with the token of ' + $Root + '): ' + $answer)
    } else {
      Write-AnetLine ($Root + ' holds no API token of AgentNetwork 1.x; its local API was not asked')
    }
  }
  if (Wait-AnetDataDirFree -Root $Root -Seconds 10) { return $true }
  foreach ($d in @((Get-AnetLegacyDaemons -Root $Root).Daemons)) {
    $exe = Get-AnetProcessPath -Id $d.Pid
    if ($exe -eq '' -or (Get-AnetBinaryFamily -Exe $exe) -ne 'v1') { continue }
    try {
      Stop-Process -Id $d.Pid -Force -ErrorAction Stop
      Write-AnetLine ('ended process ' + $d.Pid + ' (' + $exe + '), the daemon of AgentNetwork 1.x')
    } catch {
      Write-AnetLine ('could not end process ' + $d.Pid + ' (' + $exe + '): ' + $_.Exception.Message)
    }
  }
  return (Wait-AnetDataDirFree -Root $Root -Seconds 5)
}

# Get-AnetLegacyAside names the directory the data of an earlier anet is
# moved to: <Root>-legacy-<version>-<UTC time>.
function Get-AnetLegacyAside {
  param([string]$Root, [string]$Version)
  $stamp = [DateTime]::UtcNow.ToString("yyyyMMdd'T'HHmmss'Z'", [Globalization.CultureInfo]::InvariantCulture)
  return ($Root + '-legacy-' + $Version + '-' + $stamp)
}

# Get-AnetMoveCommand is the PowerShell command that does by hand what
# Move-AnetLegacyData does, for the report when the installer could not.
function Get-AnetMoveCommand {
  param([string]$Root, [string]$Aside, [string]$Kind)
  $r = ConvertTo-AnetPSLiteral $Root
  $a = ConvertTo-AnetPSLiteral $Aside
  if ($Kind -eq 'mixed') {
    $names = @($AnetV1Entries | ForEach-Object { ConvertTo-AnetPSLiteral $_ }) -join ', '
    return ('New-Item -ItemType Directory -Force -Path ' + $a + '; foreach ($n in ' + $names + ') { if (Test-Path -LiteralPath (Join-Path ' + $r + ' $n)) { Move-Item -LiteralPath (Join-Path ' + $r + ' $n) -Destination ' + $a + ' } }')
  }
  return ('Rename-Item -LiteralPath ' + $r + ' -NewName ' + (ConvertTo-AnetPSLiteral (Split-Path -Leaf $Aside)))
}

# ConvertTo-AnetPSLiteral is $Text as a single-quoted PowerShell literal,
# with an apostrophe in it doubled (a profile path such as C:\Users\O'Brien).
function ConvertTo-AnetPSLiteral {
  param([string]$Text)
  return ('''' + $Text.Replace('''', '''''') + '''')
}

# Move-AnetLegacyData moves the data of an earlier anet out of the data
# directory $Root, of kind older, v1 or mixed (Get-AnetDataDirKind), after
# its daemon has stopped: a directory of kind older or v1 is renamed to
# $Aside as a whole; of one of kind mixed, only the entries of 1.x
# ($AnetV1Entries) are moved into a new directory $Aside, and everything of
# 0.3 stays. Nothing is deleted. Returns the line for the report and, when
# something could not be moved, Held.
function Move-AnetLegacyData {
  param([string]$Root, [string]$Version, [string]$Kind, [string]$Aside)
  $res = @{ Line = ''; Held = $false }
  if ($Kind -ne 'mixed') {
    try {
      Move-AnetFile -From $Root -To $Aside -Directory
      $res.Line = 'moved the data directory of anet ' + $Version + ' aside: ' + $Root + ' -> ' + $Aside
    } catch {
      $why = $_.Exception.Message
      if ($null -ne $_.Exception.InnerException) { $why = $_.Exception.InnerException.Message }
      $res.Held = $true
      $res.Line = 'Warning: could not move the data directory of anet ' + $Version + ' aside (' + $why + ')'
    }
    return $res
  }
  try {
    $null = [IO.Directory]::CreateDirectory($Aside)
  } catch {
    $res.Held = $true
    $res.Line = 'Warning: could not create ' + $Aside + ' (' + $_.Exception.Message + '); the entries of anet ' + $Version + ' stay in ' + $Root
    return $res
  }
  $moved = New-Object System.Collections.Generic.List[string]
  $left = New-Object System.Collections.Generic.List[string]
  foreach ($n in $AnetV1Entries) {
    $from = Join-Path $Root $n
    if (-not (Test-Path -LiteralPath $from)) { continue }
    try {
      Move-AnetFile -From $from -To (Join-Path $Aside $n) -Directory:(Test-Path -LiteralPath $from -PathType Container)
      $moved.Add($n)
    } catch {
      $why = $_.Exception.Message
      if ($null -ne $_.Exception.InnerException) { $why = $_.Exception.InnerException.Message }
      $left.Add($n + ' (' + $why + ')')
    }
  }
  $movedText = '(none)'
  if ($moved.Count -gt 0) { $movedText = $moved -join ', ' }
  if ($left.Count -gt 0) {
    $res.Held = $true
    $res.Line = 'Warning: of the entries of anet ' + $Version + ' in ' + $Root + ', moved to ' + $Aside + ': ' + $movedText + '; not moved: ' + ($left -join ', ')
  } else {
    $res.Line = 'moved the entries of anet ' + $Version + ' (' + $movedText + ') from ' + $Root + ' to ' + $Aside + '; the anet 0.3 data stays'
  }
  return $res
}

# Invoke-AnetTakeover takes over from the other anet installations of this
# user (Find-AnetOthers), without asking, in an order that leaves a
# consistent state when a step fails: an earlier anet (AgentNetwork 1.x) is
# stopped with its own stop command; a daemon of 1.x that still holds the
# data directory $DataDir is stopped (Stop-AnetLegacyDaemon); the data
# directory, when it is an earlier anet's, is moved aside
# (Move-AnetLegacyData); only then is the npm package of 1.x uninstalled.
# A data directory of an earlier anet is moved whether or not an earlier
# installation was found, as install.sh does. When it cannot be moved (its
# daemon could not be stopped, or the move failed), Held is set, the
# package stays, and Remaining lists the exact commands to run. Another anet
# 0.3 or later outside npm is reported, for its directory to be taken off
# the PATH; an npm launcher of 0.3 or later, and anything that is not an
# anet of either line, are left alone. From a Restricted window (elevated,
# while Windows has a standard token for the account), nothing found on the
# machine is run: the stop and npm commands go to Remaining instead.
#
# Returns Report (lines for the log), Current, Held, Remaining, Taken (the
# summary line of the takeover: Label, and Aside when data was moved; $null
# when nothing was taken over), Label (what the earlier anet is called) and
# Notes (lines of the summary for what is left to the user).
function Invoke-AnetTakeover {
  param([string]$InstallDir, [string]$DataDir, [hashtable]$Elevation)
  $report = New-Object System.Collections.Generic.List[string]
  $current = New-Object System.Collections.Generic.List[string]
  $remaining = New-Object System.Collections.Generic.List[string]
  $notes = New-Object System.Collections.Generic.List[string]
  $restricted = [bool]$Elevation.Restricted
  $npm = Find-AnetNpm
  $npmPrefix = ''
  if ($restricted) {
    $npmPrefix = Get-AnetNpmDefaultPrefix
  } elseif ($npm -ne '') {
    $r = @{}
    Invoke-AnetTimed -FilePath $npm -ArgumentList @('prefix', '-g') -TimeoutSeconds 60 -Result $r
    $lines = @($r['Stdout'])
    if ($r['Code'] -eq 0 -and $lines.Count -gt 0 -and [IO.Path]::IsPathRooted(([string]$lines[0]).Trim())) {
      $npmPrefix = ([string]$lines[0]).Trim()
    }
  }
  $others = @(Find-AnetOthers -InstallDir $InstallDir -NpmPrefix $npmPrefix -Elevation $Elevation)
  $legacy = New-Object System.Collections.Generic.List[hashtable]
  $legacyVersion = ''
  $acted = $false
  foreach ($e in $others) {
    $what = $e.Dir + '\' + ($e.Names -join ', ')
    if ($e.Class -eq 'Current') {
      if ($e.Npm) {
        $report.Add('left alone: ' + $what + ' (npm launcher of anet ' + $e.Version + ')')
      } elseif ($e.ExeOnPath) {
        $current.Add($e.Dir)
      }
      continue
    }
    if ($e.Class -ne 'Legacy') {
      $report.Add('left alone: ' + $what + ' (not a program of AgentNetwork or anet: neither module path is in it; it was not run)')
      continue
    }
    $legacy.Add($e)
    if ($legacyVersion -eq '' -and $e.Version -ne '') { $legacyVersion = $e.Version }
    $label = $e.Version
    if ($label -eq '') { $label = '1.x' }
    $report.Add('taking over from AgentNetwork ' + $label + ' in ' + $e.Dir)
    if ($e.StopExe -eq '') {
      $note = 'AgentNetwork ' + $label + ' in ' + $e.Dir + ' has no binary of AgentNetwork 1.x this installer can run to stop it'
      if ($e.StopNote -ne '') { $note += ' (' + $e.StopNote + ')' }
      $report.Add($note)
      continue
    }
    $stopCommand = '& ' + (ConvertTo-AnetPSLiteral $e.StopExe) + ' stop'
    if ($restricted) {
      $report.Add('not run from this elevated window: ' + $e.StopExe + ' stop')
      $remaining.Add($stopCommand)
      continue
    }
    $r = @{}
    Invoke-AnetTimed -FilePath $e.StopExe -ArgumentList @('stop') -TimeoutSeconds 30 -Result $r
    foreach ($l in @($r['Output'])) { $report.Add('  ' + $l) }
    if ($r['TimedOut']) {
      $report.Add('Warning: ' + $e.StopExe + ' stop did not finish within 30 seconds and was ended')
    } elseif ($r['Code'] -ne 0) {
      $report.Add('Warning: ' + $e.StopExe + ' stop ended with exit status ' + $r['Code'])
    } else {
      $report.Add('stopped AgentNetwork ' + $label + ' with its own stop command (' + $e.StopExe + ' stop)')
      $acted = $true
    }
  }

  # The data directory, after the daemon that may hold it has exited;
  # found or not, an earlier anet's data is moved as install.sh moves it.
  $held = $false
  $aside = ''
  $kind = Get-AnetDataDirKind -Root $DataDir
  $version = $legacyVersion
  if ($kind -eq 'older') { $version = '0.2' } elseif ($version -eq '') { $version = '1.x' }
  $takenLabel = 'AgentNetwork ' + $version
  if ($kind -eq 'older') { $takenLabel = 'anet 0.2' }
  $report.Add('data directory ' + $DataDir + ': ' + $kind)
  if ($kind -eq 'older' -or $kind -eq 'v1' -or $kind -eq 'mixed') {
    $target = Get-AnetLegacyAside -Root $DataDir -Version $version
    $moveCommand = Get-AnetMoveCommand -Root $DataDir -Aside $target -Kind $kind
    $free = Wait-AnetDataDirFree -Root $DataDir -Seconds 5
    if (-not $free) {
      foreach ($l in @(Get-AnetLegacyDaemonLog -Root $DataDir)) { $report.Add($l) }
      $free = Stop-AnetLegacyDaemon -Root $DataDir
      if ($free) { $acted = $true }
    }
    if (-not $free) {
      $held = $true
      $report.Add('Warning: ' + $DataDir + ' is still in use by a daemon of AgentNetwork 1.x, which could not be stopped, so it was not moved; the new anet does not start on it')
      foreach ($d in @((Get-AnetLegacyDaemons -Root $DataDir).Daemons)) { $remaining.Add('Stop-Process -Id ' + $d.Pid) }
      $remaining.Add($moveCommand)
    } else {
      $m = Move-AnetLegacyData -Root $DataDir -Version $version -Kind $kind -Aside $target
      $report.Add($m.Line)
      if ($m.Held) {
        $held = $true
        $remaining.Add($moveCommand)
      } else {
        $aside = $target
        $acted = $true
      }
    }
  } elseif ($kind -eq 'newer') {
    $report.Add($DataDir + ' was written by a newer anet; it is left as it is')
  } elseif ($kind -eq 'invalid') {
    $report.Add($DataDir + ' has an identity file that does not read (' + (Join-Path $DataDir 'identity.kel') + '); it is left as it is; see anet doctor')
  }

  # The npm package of 1.x, last, and only when the takeover is complete:
  # its stop command is what stops a daemon that still runs.
  foreach ($e in $legacy) {
    if ($e.Npm) {
      $argv = @('uninstall', '-g')
      if (-not (Test-AnetSameDir $e.Dir $npmPrefix)) { $argv += @('--prefix', $e.Dir) }
      $argv += '@agentnetwork/anet'
      $typed = 'npm ' + (@($argv | ForEach-Object { ConvertTo-AnetArgument -Text $_ }) -join ' ')
      if ($held -or $restricted -or $npm -eq '' -or @($argv | Where-Object { -not (Test-AnetBatchSafe $_) }).Count -gt 0) {
        $report.Add('the npm package @agentnetwork/anet ' + $e.Version + ' was not removed; run: ' + $typed)
        if ($held) { $remaining.Add($typed) } else { $notes.Add((Get-AnetText 'note_npm' @{ version = $e.Version; command = $typed })) }
        continue
      }
      $r = @{}
      Invoke-AnetTimed -FilePath $npm -ArgumentList $argv -TimeoutSeconds 180 -Result $r
      foreach ($l in @($r['Output'])) { $report.Add('  ' + $l) }
      if ($r['Code'] -eq 0 -and -not (Test-Path -LiteralPath (Join-AnetPath $e.Dir 'node_modules\@agentnetwork\anet\package.json'))) {
        $report.Add('removed the npm package @agentnetwork/anet ' + $e.Version + ' (' + $typed + ')')
        $acted = $true
      } else {
        $report.Add('Warning: ' + $typed + ' did not remove the package (exit status ' + $r['Code'] + ')')
        $notes.Add((Get-AnetText 'note_npm' @{ version = $e.Version; command = $typed }))
      }
    } else {
      $exe = Join-Path $e.Dir 'anet.exe'
      $report.Add('left in place: ' + $exe + ' (AgentNetwork ' + $e.Version + ')')
      if (-not $held) {
        $v = $e.Version
        if ($v -eq '') { $v = '1.x' }
        $notes.Add((Get-AnetText 'note_legacy_exe' @{ path = $exe; version = $v; command = ('Remove-Item -LiteralPath ' + (ConvertTo-AnetPSLiteral $exe)) }))
      }
    }
  }
  # From a restricted window, a stop command that was not run is left to a
  # normal window (when the takeover is complete; otherwise it is a step).
  if (-not $held) {
    foreach ($c in @($remaining)) { $notes.Add((Get-AnetText 'note_run' @{ command = $c })) }
    $remaining.Clear()
  }
  $taken = $null
  if ($acted -and -not $held) { $taken = @{ Label = $takenLabel; Aside = $aside } }
  return @{ Report = $report.ToArray(); Current = $current.ToArray(); Held = $held; Remaining = $remaining.ToArray();
            Taken = $taken; Label = $takenLabel; Notes = $notes.ToArray() }
}

# Get-AnetLegacyDaemonLog says in lines for the log which daemons of
# AgentNetwork 1.x hold the data directory.
function Get-AnetLegacyDaemonLog {
  param([string]$Root)
  $l = Get-AnetLegacyDaemons -Root $Root
  $out = @()
  foreach ($d in @($l.Daemons)) { $out += ('daemon of AgentNetwork 1.x on ' + $Root + ': process ' + $d.Pid + ' (' + $d.Exe + ')') }
  if ($l.Unknown) { $out += ('127.0.0.1:' + $AnetV1Port + ' answers; Windows cannot say which process listens') }
  return $out
}

# Set-AnetUserPath puts the install directory first in the user's PATH in
# the registry and takes the directories in $Remove (other installations of
# anet 0.3 or later, in directories of their own) off it, keeping every
# other entry as written and the value's kind; it tells running programs
# that the environment changed and puts the directory first in this
# session's PATH. Windows builds the PATH of a new process from the
# machine's PATH followed by the user's, so a directory on the machine PATH
# still comes first (Test-AnetPathOrder reports it).
function Set-AnetUserPath {
  param([string]$Dir, [string[]]$Remove = @())
  $key = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey('Environment')
  try {
    $raw = [string]$key.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
    $kind = [Microsoft.Win32.RegistryValueKind]::String
    if (@($key.GetValueNames()) -contains 'Path') { $kind = $key.GetValueKind('Path') }
    $keep = New-Object System.Collections.Generic.List[string]
    $removed = New-Object System.Collections.Generic.List[string]
    $first = $false
    $i = 0
    foreach ($e in $raw.Split(';')) {
      $t = $e.Trim()
      if ($t -eq '') { continue }
      $x = [Environment]::ExpandEnvironmentVariables($t)
      if (Test-AnetSameDir $x $Dir) {
        if ($i -eq 0) { $first = $true }
        $i++
        continue
      }
      if (@($Remove | Where-Object { Test-AnetSameDir $_ $x }).Count -gt 0) {
        $removed.Add($t)
        continue
      }
      $keep.Add($t)
      $i++
    }
    if ($first -and $removed.Count -eq 0) {
      Write-AnetLine ('OK ' + $Dir + ' is first on your PATH')
    } else {
      $new = (@($Dir) + $keep.ToArray()) -join ';'
      if ($kind -ne [Microsoft.Win32.RegistryValueKind]::ExpandString -and $new.Contains('%')) {
        $kind = [Microsoft.Win32.RegistryValueKind]::ExpandString
      }
      $key.SetValue('Path', $new, $kind)
      [Environment]::SetEnvironmentVariable('ANET_INSTALL_PATH_REFRESH', $null, 'User')
      Write-AnetLine ('-> put ' + $Dir + ' first on your PATH (new windows see it)')
      foreach ($r in $removed) {
        Write-AnetLine ('-> took ' + $r + ' off your PATH: it holds another anet 0.3 installation and nothing else; that installation is left in place')
      }
    }
  } finally {
    $key.Close()
  }
  $parts = @(([string]$env:Path).Split(';') | Where-Object { $_.Trim() -ne '' -and -not (Test-AnetSameDir $_.Trim() $Dir) })
  $env:Path = (@($Dir) + $parts) -join ';'
}

# Test-AnetPathOrder asks a new powershell.exe, started with the PATH a new
# window gets (the machine's PATH, then the user's, from the registry), which
# anet it runs, and says in the log whether that is $Exe. Returns the other
# anet a new window runs, or ''.
function Test-AnetPathOrder {
  param([string]$Exe)
  $machine = [string][Environment]::GetEnvironmentVariable('Path', 'Machine')
  $user = [string][Environment]::GetEnvironmentVariable('Path', 'User')
  $path = $machine.TrimEnd(';') + ';' + $user
  $ps = Join-Path ([Environment]::SystemDirectory) 'WindowsPowerShell\v1.0\powershell.exe'
  $r = @{}
  Invoke-AnetTimed -FilePath $ps -ArgumentList @('-NoProfile', '-NonInteractive', '-Command', '(Get-Command anet).Source') -PathVariable $path -TimeoutSeconds 60 -Result $r
  $lines = @($r['Output'])
  $got = ''
  if ($r['Code'] -eq 0 -and $lines.Count -gt 0) { $got = ([string]$lines[0]).Trim() }
  if ($got -ne '' -and $got -ieq $Exe) {
    Write-AnetLine ('OK a new window runs ' + $Exe + ' for anet')
    return ''
  }
  if ($got -ne '') {
    Write-AnetLine ('Warning: a new window runs ' + $got + ' for anet, not ' + $Exe + '; its directory comes first on the machine PATH')
    return $got
  }
  Write-AnetLine ('Warning: a new powershell.exe could not say which anet it runs; check with: (Get-Command anet).Source')
  return ''
}

# Test-AnetPathHere reports whether the PATH this process started with (that
# of the window that ran the command) already finds $Exe as anet: the first
# directory of it that holds an anet program is the install directory.
function Test-AnetPathHere {
  param([string]$PathText, [string]$Exe)
  $dir = Split-Path -Parent $Exe
  foreach ($e in ([string]$PathText).Split(';')) {
    $d = $e.Trim().Trim('"')
    if ($d -eq '') { continue }
    try { $d = [Environment]::ExpandEnvironmentVariables($d).TrimEnd('\') } catch { continue }
    foreach ($n in @('anet.exe', 'anet.cmd', 'anet.bat', 'anet.ps1', 'anet.com')) {
      if (Test-Path -LiteralPath (Join-Path $d $n) -PathType Leaf) { return (Test-AnetSameDir $d $dir) }
    }
  }
  return $false
}

# Get-AnetSessionOwner is the account that owns explorer.exe in the session
# of this process, that is, the user logged on to it: its SID and name, or
# $null when there is no explorer.exe in the session (a service session,
# Server Core) or it cannot be asked.
function Get-AnetSessionOwner {
  if ([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT) { return $null }
  if ($null -eq (Get-Command -Name 'Get-CimInstance' -ErrorAction SilentlyContinue)) { return $null }
  try {
    $session = [int](Get-Process -Id $PID).SessionId
    foreach ($p in @(Get-CimInstance -ClassName Win32_Process -Filter "Name = 'explorer.exe'" -ErrorAction Stop)) {
      if ([int]$p.SessionId -ne $session) { continue }
      $owner = Invoke-CimMethod -InputObject $p -MethodName GetOwnerSid -ErrorAction Stop
      if ($null -ne $owner -and [int]$owner.ReturnValue -eq 0 -and -not [string]::IsNullOrEmpty([string]$owner.Sid)) {
        return @{ Sid = [string]$owner.Sid; Name = (Get-AnetAccountName ([string]$owner.Sid)) }
      }
    }
  } catch { }
  return $null
}

# Get-AnetElevation says whether this window is elevated, and when it is,
# why Windows has no standard user token for this account ('' when it has
# one): User Account Control is off, or this is the built-in Administrator
# without Admin Approval Mode. Restricted is the case in which this window
# has more rights than the programs of this account normally run with
# (elevated, and a standard token exists): then nothing found on the
# machine is run from it, and the node is started with the standard token
# or not at all. SessionOwner is the user logged on to this session
# (Get-AnetSessionOwner), asked only in an elevated window.
function Get-AnetElevation {
  $id = [System.Security.Principal.WindowsIdentity]::GetCurrent()
  $principal = New-Object System.Security.Principal.WindowsPrincipal $id
  $e = @{ Elevated = $principal.IsInRole([System.Security.Principal.WindowsBuiltInRole]::Administrator); NoStandardToken = '';
          Restricted = $false; Account = $id.Name; Sid = $id.User.Value; SessionOwner = $null }
  if (-not $e.Elevated) { return $e }
  $lua = 1
  $filter = 0
  $key = [Microsoft.Win32.Registry]::LocalMachine.OpenSubKey('SOFTWARE\Microsoft\Windows\CurrentVersion\Policies\System')
  if ($null -ne $key) {
    try {
      $lua = [int]$key.GetValue('EnableLUA', 1)
      $filter = [int]$key.GetValue('FilterAdministratorToken', 0)
    } finally {
      $key.Close()
    }
  }
  if ($lua -eq 0) {
    $e.NoStandardToken = 'User Account Control is off'
  } elseif ($id.User.Value -match '-500$' -and $filter -ne 1) {
    $e.NoStandardToken = 'this is the built-in Administrator account'
  }
  $e.Restricted = ($e.NoStandardToken -eq '')
  $e.SessionOwner = Get-AnetSessionOwner
  return $e
}

# Remove-AnetStaleTasks deletes the one-time start tasks of earlier runs of
# this installer for this account ($Account) that were ended before they
# could delete them (named AgentNetwork start <id>), except one that is
# running. The tasks of other accounts are theirs.
function Remove-AnetStaleTasks {
  param($Folder, [string]$Account)
  try {
    foreach ($t in @($Folder.GetTasks(1))) {
      $owner = ''
      try { $owner = [string]$t.Definition.Principal.UserId } catch { }
      if (([string]$t.Name) -like 'AgentNetwork start *' -and [int]$t.State -ne 4 -and $owner -ieq $Account) {
        try { $Folder.DeleteTask([string]$t.Name, 0) } catch { }
      }
    }
  } catch { }
}

# Start-AnetStandardUser starts `<Exe> up` with the standard user token of
# this account from an elevated window: a task of the Task Scheduler for
# this user, with the interactive token (TASK_LOGON_INTERACTIVE_TOKEN, the
# session the user is logged on to) and the least privileges
# (TASK_RUNLEVEL_LUA), run once and deleted. Returns '' when `anet up` ran
# that way and succeeded, or why it did not. A task that is still running
# after the wait is left for the next run to delete (Remove-AnetStaleTasks),
# since ending it could end the node it started.
function Start-AnetStandardUser {
  param([string]$Exe, [string]$Account)
  $name = 'AgentNetwork start ' + [Guid]::NewGuid().ToString('N')
  try {
    $svc = New-Object -ComObject Schedule.Service
    $svc.Connect()
    $folder = $svc.GetFolder('\')
  } catch {
    return 'the Task Scheduler is not available: ' + $_.Exception.Message
  }
  Remove-AnetStaleTasks -Folder $folder -Account $Account
  $registered = $false
  try {
    $def = $svc.NewTask(0)
    $def.RegistrationInfo.Description = 'Starts the AgentNetwork node once with standard user rights; the installer deletes it.'
    $def.Settings.Enabled = $true
    $def.Settings.Hidden = $true
    $def.Settings.DisallowStartIfOnBatteries = $false
    $def.Settings.StopIfGoingOnBatteries = $false
    $def.Settings.ExecutionTimeLimit = 'PT0S'
    $def.Principal.UserId = $Account
    $def.Principal.LogonType = 3
    $def.Principal.RunLevel = 0
    $action = $def.Actions.Create(0)
    $action.Path = $Exe
    $action.Arguments = 'up'
    $action.WorkingDirectory = Split-Path -Parent $Exe
    $task = $folder.RegisterTaskDefinition($name, $def, 6, $Account, $null, 3)
    $registered = $true
    $null = $task.Run($null)
    $deadline = [DateTime]::UtcNow.AddSeconds(30)
    while ([DateTime]::UtcNow -lt $deadline) {
      Start-Sleep -Milliseconds 500
      $t = $folder.GetTask($name)
      # State 4 is running; LastRunTime stays in 1899 until the task has run.
      if ($t.State -ne 4 -and ([DateTime]$t.LastRunTime).Year -gt 2000) {
        if ([int]$t.LastTaskResult -eq 0) { return '' }
        return ('anet up ended with exit status ' + [int]$t.LastTaskResult)
      }
    }
    $t = $folder.GetTask($name)
    if ($t.State -eq 4) {
      $registered = $false
      return 'anet up, started through the Task Scheduler, had not finished after 30 seconds; it may still complete, and the task ' + $name + ' is removed by the next run of this installer'
    }
    return 'the task did not run within 30 seconds (no one is logged on to this computer interactively as ' + $Account + ')'
  } catch {
    return $_.Exception.Message
  } finally {
    if ($registered) {
      try { $folder.DeleteTask($name, 0) } catch { }
    }
  }
}

# Start-AnetNode starts the node (anet up) and returns its exit status in
# Code. In a window without elevation, directly. In an elevated window
# while Windows has a standard token for the account (Restricted), only with
# that token (Start-AnetStandardUser), so that neither the node nor the
# commands it runs for a controller have administrator rights; when that
# fails, the node is not started at all, and Deferred says so with the
# reason in Why. When Windows has no standard token for the account, the
# node runs with the rights of this window, said in one line.
function Start-AnetNode {
  param([string]$Exe, [hashtable]$Elevation)
  $res = @{ Code = 0; Deferred = $false; Why = ''; Note = '' }
  if ($Elevation.Elevated) {
    if ($Elevation.Restricted) {
      $why = Start-AnetStandardUser -Exe $Exe -Account $Elevation.Account
      if ($why -eq '') {
        Write-AnetLine '   This window is elevated; the node was started with standard user rights through the Task Scheduler.'
        return $res
      }
      $res.Deferred = $true
      $res.Why = $why
      return $res
    }
    Write-AnetLine ('   This window is elevated and ' + $Elevation.NoStandardToken + ', so Windows has no standard user token for this account: the node runs with the rights of this window.')
    $res.Note = 'note_builtin_admin'
    if ($Elevation.NoStandardToken -eq 'User Account Control is off') { $res.Note = 'note_uac_off' }
  }
  $r = Invoke-AnetCommand -Exe $Exe -ArgumentList @('up') -TimeoutSeconds 120
  $res.Code = $r['Code']
  return $res
}

# Write-AnetGrant puts in the log what -Controller granted, read back from
# the data directory, and the two steps that revoke it. Returns whether the
# grant read back as written.
function Write-AnetGrant {
  param([string]$DataDir, [string]$Aid)
  $config = Join-Path $DataDir 'config.json'
  $allowFile = Join-Path $DataDir 'peers.allow'
  $shellText = '(config.json could not be read)'
  $ok = $false
  try {
    $cfg = [IO.File]::ReadAllText($config) | ConvertFrom-Json
    $sh = $cfg.modules.shell
    $parts = @(('allow = [' + (@($sh.allow) -join ', ') + ']'), ('allow_arbitrary = ' + ([string]$sh.allow_arbitrary).ToLowerInvariant()))
    if (@($sh.PSObject.Properties.Name) -contains 'shell') { $parts += ('shell = ' + [string]$sh.shell) }
    $shellText = $parts -join ', '
    $ok = ((@($sh.allow) -join ',') -ceq $Aid)
  } catch { }
  $listed = $false
  try {
    if (@([IO.File]::ReadAllLines($allowFile) | Where-Object { $_.Trim() -ceq $Aid }).Count -gt 0) { $listed = $true }
  } catch { }
  Write-AnetLine 'Granted, as -Controller on the command line requested (that parameter is the consent; nothing was asked):'
  Write-AnetLine ('  modules.shell in ' + $config + ': ' + $shellText)
  Write-AnetLine ('  peers.allow in ' + $allowFile + ': ' + $Aid + ' (listed: ' + $(if ($listed) { 'yes' } else { 'no' }) + ')')
  Write-AnetLine ('The agent ' + $Aid + ' can run any command on this computer as this user. To revoke it:')
  Write-AnetLine ('  1. anet peers remove ' + $Aid)
  Write-AnetLine ('  2. remove the modules.shell block from ' + $config + ', then restart the node: anet stop, then anet up')
  return ($ok -and $listed)
}

# Set-AnetAutostart starts the node at every logon of this user.
function Set-AnetAutostart {
  param([string]$Exe)
  $runKey = 'Software\Microsoft\Windows\CurrentVersion\Run'
  $key = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey($runKey)
  try {
    $key.SetValue('AgentNetwork', ('"' + $Exe + '" up'), [Microsoft.Win32.RegistryValueKind]::String)
  } finally {
    $key.Close()
  }
  Write-AnetLine ('-> the node starts at every logon: value AgentNetwork under HKCU\' + $runKey)
  Write-AnetLine ('   to stop that: Remove-ItemProperty -Path ''HKCU:\' + $runKey + ''' -Name AgentNetwork')
}

# Get-AnetInstalledFailure is what a run that ends after the binary is
# installed prints: what went wrong, what is installed, and the commands
# that remain, in order.
function Get-AnetInstalledFailure {
  param([hashtable]$State, [string]$Message, [string[]]$Steps = @())
  $out = @((Get-AnetText 'error' @{ text = $Message }),
           (Get-AnetText 'stopped_at' @{ version = $AnetRelease['Version']; exe = $State.Exe }))
  if (@($Steps).Count -gt 0) {
    $out += (Get-AnetText 'steps')
    foreach ($s in @($Steps)) { $out += ('  ' + $s) }
  }
  return $out
}

# Open-AnetGuard opens the binary at $Path for reading and shares it for
# reading only: while the handle is open, no process can write to the file,
# rename it or delete it, and so what this script runs is what it checked.
# Through that handle it checks that the file's SHA-256 is the pinned one.
# Returns the open stream; the caller disposes it when the last run ends.
function Open-AnetGuard {
  param([string]$Path, [string]$Sha256)
  $fs = New-Object System.IO.FileStream -ArgumentList $Path, ([IO.FileMode]::Open), ([IO.FileAccess]::Read), ([IO.FileShare]::Read)
  try {
    $h = [System.Security.Cryptography.SHA256]::Create()
    try {
      $got = ([BitConverter]::ToString($h.ComputeHash($fs)) -replace '-', '').ToLowerInvariant()
    } finally {
      $h.Dispose()
    }
    if (-not ($got -ceq $Sha256)) {
      throw (New-AnetFailure ($Path + ' has SHA-256 ' + $got + ', not the pinned ' + $Sha256 + ': the file was changed after it was written; remove it and run this again'))
    }
  } catch {
    $fs.Dispose()
    throw
  }
  return $fs
}

# Close-AnetGuard releases the handle of Open-AnetGuard held in $State.
function Close-AnetGuard {
  param([hashtable]$State)
  if ($null -ne $State.Guard) {
    try { $State.Guard.Dispose() } catch { }
    $State.Guard = $null
  }
}

# Get-AnetInstalledVersion is the version of the anet.exe already at $Exe,
# or '': from the release record beside it, else by running `<exe> version`
# (not from a Restricted window: the file is this account's to write, and
# nothing of the account's is run with more than its rights).
function Get-AnetInstalledVersion {
  param([string]$Exe, [hashtable]$Elevation)
  if (-not (Test-Path -LiteralPath $Exe -PathType Leaf)) { return '' }
  $v = Get-AnetRecordVersion -Exe $Exe
  if ($v -ne '') { return $v }
  if ($Elevation.Restricted) {
    Write-AnetLine ('Warning: the version of the existing ' + $Exe + ' is not read from an elevated window; it will be replaced')
    return ''
  }
  $r = Invoke-AnetCommand -Exe $Exe -ArgumentList @('version') -TimeoutSeconds 30
  $lines = @($r['Stdout'])
  if ($r['Code'] -eq 0 -and $lines.Count -gt 0 -and ([string]$lines[0]) -cmatch '^anet (\S+) ') {
    if (Test-AnetVersion $Matches[1]) { return $Matches[1] }
  }
  Write-AnetLine ('Warning: cannot read the version of the existing ' + $Exe + '; it will be replaced')
  return ''
}

# Get-AnetNodeStatus asks the node for its status (anet status --json):
# Running, Aid and Version. -Quiet keeps the answer out of the log.
function Get-AnetNodeStatus {
  param([string]$Exe, [switch]$Quiet)
  $st = @{ Running = $false; Aid = ''; Version = '' }
  if ($Quiet) {
    $r = @{}
    Invoke-AnetTimed -FilePath $Exe -ArgumentList @('status', '--json') -TimeoutSeconds 30 -Result $r
  } else {
    $r = Invoke-AnetCommand -Exe $Exe -ArgumentList @('status', '--json') -TimeoutSeconds 30
  }
  if ($r['Code'] -ne 0) { return $st }
  $st.Running = $true
  try {
    $j = (@($r['Stdout']) -join "`n") | ConvertFrom-Json
    if ([string]$j.aid -cmatch '^bafyrei[a-z2-7]{52}$') { $st.Aid = [string]$j.aid }
    if (Test-AnetVersion ([string]$j.version)) { $st.Version = [string]$j.version }
  } catch { }
  return $st
}

# Read-AnetJoinState is the state of the join of $Hub as the node keeps it
# in hub_join.json (registered, pending_admission, retrying, refused), or ''.
function Read-AnetJoinState {
  param([string]$DataDir, [string]$Hub)
  try {
    $j = [IO.File]::ReadAllText((Join-Path $DataDir 'hub_join.json')) | ConvertFrom-Json
    if (([string]$j.hub).TrimEnd('/') -ieq $Hub.TrimEnd('/')) { return [string]$j.state }
  } catch { }
  return ''
}

# Get-AnetLastLine is the last line a command printed that says why, for a
# message: of its standard error when it wrote there, else of its standard
# output; a line that gives the next command (it starts with an arrow) is
# left out. '' when there is none.
function Get-AnetLastLine {
  param([hashtable]$Result)
  $lines = @($Result['Stdout'])
  if (@($Result['Stderr']).Count -gt 0) { $lines = @($Result['Stderr']) }
  $lines = @($lines | Where-Object { $t = ([string]$_).Trim(); $t -ne '' -and -not $t.StartsWith([string][char]0x2192) -and -not $t.StartsWith('->') })
  if ($lines.Count -eq 0) { return '' }
  return ([string]$lines[-1]).Trim()
}

# Get-AnetSummary is the summary of a run that completed (OUTPUT in the
# header), from its facts: Version; Running; Aid; Hub and JoinState
# (registered, pending_admission, retrying, or '' when not known); Error
# (a reason for the line Error:, or ''); Taken (@{ Label; Aside } or
# $null); Controller; Notes (lines); Run (here, new or path) and Exe. The
# line Details: follows it (Get-AnetDetailsLine).
function Get-AnetSummary {
  param([Parameter(Mandatory = $true)][hashtable]$F)
  $out = New-Object System.Collections.Generic.List[string]
  if ($F.Running) {
    $out.Add((Get-AnetText 'running' @{ version = $F.Version }))
  } else {
    $out.Add((Get-AnetText 'installed_only' @{ version = $F.Version }))
  }
  if ($F.Running -and $F.Aid -ne '') { $out.Add('  AID   ' + $F.Aid) }
  if ($F.Running -and $F.Hub -ne '' -and $F.Error -eq '') {
    $key = 'hub_sent'
    switch -exact ([string]$F.JoinState) {
      'registered' { $key = 'hub_joined' }
      'pending_admission' { $key = 'hub_pending' }
      'retrying' { $key = 'hub_retrying' }
    }
    $out.Add((Get-AnetText $key @{ hub = $F.Hub; aid = $F.Aid }))
  }
  if ($F.Error -ne '') { $out.Add((Get-AnetText 'error' @{ text = $F.Error })) }
  if ($null -ne $F.Taken) {
    if ($F.Taken.Aside -ne '') {
      $out.Add((Get-AnetText 'takeover_kept' @{ label = $F.Taken.Label; aside = $F.Taken.Aside }))
    } else {
      $out.Add((Get-AnetText 'takeover' @{ label = $F.Taken.Label }))
    }
  }
  if ($F.Controller -ne '') { $out.Add((Get-AnetText 'remote' @{ aid = $F.Controller })) }
  foreach ($n in @($F.Notes)) { $out.Add((Get-AnetText 'note' @{ text = $n })) }
  switch -exact ([string]$F.Run) {
    'here' { $out.Add((Get-AnetText 'run_here')) }
    'path' { $out.Add((Get-AnetText 'run_path' @{ exe = $F.Exe })) }
    default { $out.Add((Get-AnetText 'run_new')) }
  }
  return $out.ToArray()
}

# Get-AnetDetailsLine is the last line of every run: where its log is.
function Get-AnetDetailsLine {
  param([string]$Log, [string]$Why)
  if ($Log -ne '') { return (Get-AnetText 'details' @{ path = $Log }) }
  return (Get-AnetText 'details_none' @{ why = $Why })
}

function Install-Anet {
  param(
    [string]$Controller, [switch]$Shell, [string]$Hub, [string]$Name, [string]$Prefix,
    [string[]]$Without, [string]$Base, [switch]$NoAutostart, [switch]$NoPath, [switch]$Elevated,
    [switch]$NoJoin, [switch]$NoStart, [switch]$Agents, [string[]]$AgentList
  )
  $previousEAP = $ErrorActionPreference
  $previousProgress = $ProgressPreference
  Set-StrictMode -Version 3
  $ErrorActionPreference = 'Stop'
  $ProgressPreference = 'SilentlyContinue'
  $given = @{}
  foreach ($k in $PSBoundParameters.Keys) { $given[$k] = $PSBoundParameters[$k] }
  $S = @{ Installed = $false; Exe = ''; Tmp = ''; Staged = ''; Guard = $null; DataDir = ''; Lines = @(); Error = $false }
  $global:LASTEXITCODE = 0
  $startPath = [string]$env:Path
  Start-AnetLog
  $relVersion = '(unreleased template)'
  if ($AnetRelease -is [hashtable]) { $relVersion = [string]$AnetRelease['Version'] }
  Write-AnetLine ('install.ps1 of anet ' + $relVersion + ', run at ' + $AnetOut.Stamp + ' (UTC)')
  Write-AnetLine ('PowerShell ' + $PSVersionTable.PSVersion + ' ' + [string]$PSVersionTable.PSEdition + ', ' + [Environment]::OSVersion.VersionString)
  Write-AnetLine ('parameters: ' + ((@($given.Keys | Sort-Object) | ForEach-Object { '-' + $_ + ' ' + (@($given[$_]) -join ',') }) -join ' '))
  try {
    Assert-AnetReleased

    # 1. Environment.
    if ($ExecutionContext.SessionState.LanguageMode -ne 'FullLanguage') {
      throw (New-AnetFailure ('PowerShell runs in ' + $ExecutionContext.SessionState.LanguageMode + ' mode here; the installer needs FullLanguage'))
    }
    $psv = $PSVersionTable.PSVersion
    if ($psv.Major -lt 5 -or ($psv.Major -eq 5 -and $psv.Minor -lt 1)) {
      throw (New-AnetFailure ('PowerShell ' + $PSVersionTable.PSVersion + ' is too old; 5.1 or later is required'))
    }
    if ([Environment]::OSVersion.Version.Build -lt 17763) {
      throw (New-AnetFailure ('Windows build ' + [Environment]::OSVersion.Version.Build + ' is too old; anet needs Windows 10 1809 or Windows Server 2019 (build 17763) or later'))
    }
    if (-not [Environment]::Is64BitOperatingSystem) {
      throw (New-AnetFailure 'this is a 32-bit Windows; anet is built for 64-bit Windows (x64 and arm64)')
    }
    $arch = Get-AnetArch
    # An elevated window is not refused (-Elevated changes nothing): anet is
    # installed for this account as in any other window, and the node is
    # started with standard user rights when Windows has them (step 14). An
    # elevated window of another account than the one logged on to this
    # session is refused: anet would be installed for that other account,
    # and its node would run as it in this user's session.
    $elevation = Get-AnetElevation
    if ($elevation.Elevated -and $null -ne $elevation.SessionOwner -and -not ($elevation.SessionOwner.Sid -ceq $elevation.Sid)) {
      throw (New-AnetFailure ('this elevated window runs as ' + $elevation.Account + ', but the user logged on to this session is ' + $elevation.SessionOwner.Name + '; anet would be installed for ' + $elevation.Account + ' and its node would run as that account. Run the command in a normal window of your own account.'))
    }

    # 2. Parameters.
    $o = Get-AnetOptions -Given $given
    $asset = $o.AssetPrefix + '-windows-' + $arch
    $platform = 'windows-' + $arch
    $pin = $AnetRelease['Assets'][$asset]
    $version = [string]$AnetRelease['Version']
    Write-AnetLine ('installing anet ' + $version + ' (' + $o.Variant + ' build, ' + $platform + ') for ' + $elevation.Account)
    if ($elevation.Elevated) {
      Write-AnetLine 'This window is elevated. anet is installed for this account all the same: its own directories, PATH and Run value.'
      if ($elevation.Restricted) {
        Write-AnetLine 'Programs already on this computer are not run from this window; the commands that need one are listed instead.'
      }
    }
    if ($o.Variant -eq 'shell') {
      Write-AnetLine 'The shell build CAN run commands on this machine for the agents its configuration allows.'
    }
    $profileDir = [Environment]::GetFolderPath('UserProfile')
    $dataDir = [string]$env:ANET_DATA_DIR
    if ($dataDir -eq '') { $dataDir = Join-Path $profileDir '.anet' }
    $S.DataDir = $dataDir
    $notes = New-Object System.Collections.Generic.List[string]

    # 3. and 4. Sources, download and pin check.
    if ($PSVersionTable.PSEdition -ne 'Core') {
      # Windows PowerShell 5.1: add TLS 1.2 to what the session allows.
      [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor 3072
    }
    if ($null -eq ('System.Net.Http.HttpClient' -as [type])) { Add-Type -AssemblyName System.Net.Http }
    $sources = @(Get-AnetSources -Options $o -Arch $arch -Version $version)
    $S.Tmp = Join-Path ([IO.Path]::GetTempPath()) ('anet-install-' + [Guid]::NewGuid().ToString('N'))
    $null = [IO.Directory]::CreateDirectory($S.Tmp)
    $rel = Get-AnetRelease -Sources $sources -Asset $asset -Pin $pin -Platform $platform
    [IO.File]::WriteAllBytes((Join-Path $S.Tmp 'release.json'), $rel.Manifest)
    [IO.File]::WriteAllBytes((Join-Path $S.Tmp 'release.json.sig'), $rel.Signature)

    # 5. Install directory: -Prefix, or the first candidate that passes.
    $existing = @(Get-AnetExistingInstalls -Elevation $elevation)
    if ($o.PrefixGiven) {
      $why = Get-AnetDirectoryProblem -Dir $o.Prefix
      if ($null -ne $why) {
        Write-AnetLine $why
        throw (New-AnetFailure ('-Prefix ' + $o.Prefix + ' cannot be used: ' + $why + '; choose a directory that is private to this user, or leave -Prefix out'))
      }
    } else {
      $o.Prefix = Select-AnetInstallDir -Existing $existing
    }
    Write-AnetLine ('install directory: ' + $o.Prefix)
    Initialize-AnetDirectory -Dir $o.Prefix
    $S.Exe = Join-Path $o.Prefix 'anet.exe'

    # 6. Stage and verify. The staged file is held open, shared for reading
    # only, from before its verification until it is renamed, so that what
    # was verified is what runs (Open-AnetGuard).
    $S.Staged = Get-AnetStagedPath -Dir $o.Prefix
    Write-AnetStaged -Path $S.Staged -Bytes $rel.Binary
    $S.Guard = Open-AnetGuard -Path $S.Staged -Sha256 ([string]$pin['Sha256'])
    $installed = Get-AnetInstalledVersion -Exe $S.Exe -Elevation $elevation
    $mods = Confirm-AnetStaged -Staged $S.Staged -TmpDir $S.Tmp -Installed $installed -Variant $o.Variant -Asset $asset

    # 7. -Without: the names are checked against the verified module set
    # here, and config.json is changed only after the binary is installed
    # (step 11), so that a failure before then leaves the machine unchanged.
    foreach ($m in @($o.Without)) {
      if ($m -ne 'shell' -and @($mods) -notcontains $m) {
        throw (New-AnetFailure ('-Without ' + $m + ': the ' + $o.Variant + ' build has no module of that name (its modules: ' + (@($mods) -join ',') + ')'))
      }
    }

    # 8. Replace, and hold the installed binary the same way for every run
    # of it below, checked again through the new handle.
    Close-AnetGuard -State $S
    $aside = Install-AnetStaged -Staged $S.Staged -Exe $S.Exe
    $S.Staged = ''
    $S.Installed = $true
    $S.Guard = Open-AnetGuard -Path $S.Exe -Sha256 ([string]$pin['Sha256'])
    Remove-AnetAsides -Dir $o.Prefix -Keep $aside
    if (-not (Save-AnetRecord -Exe $S.Exe -TmpDir $S.Tmp)) { $notes.Add((Get-AnetText 'note_record' @{ exe = $S.Exe })) }
    if ($installed -ne '' -and $installed -ne $version) {
      Write-AnetSay (Get-AnetText 'installed_up' @{ exe = $S.Exe; previous = $installed })
    } else {
      Write-AnetSay (Get-AnetText 'installed' @{ exe = $S.Exe })
    }
    if ($aside -ne '') { Write-AnetLine ('the previous binary is ' + $aside + ' until it is no longer in use') }

    # 9. Earlier installations: stop an earlier anet and a daemon of it that
    # still holds the data directory, move its data aside, remove its npm
    # package, and find other installations of 0.3 or later, before the
    # data directory is initialized for this one.
    $takeover = Invoke-AnetTakeover -InstallDir $o.Prefix -DataDir $dataDir -Elevation $elevation
    foreach ($line in @($takeover.Report)) { Write-AnetLine $line }
    foreach ($n in @($takeover.Notes)) { $notes.Add($n) }
    $otherCurrent = New-Object System.Collections.Generic.List[string]
    foreach ($d in @($takeover.Current)) { $otherCurrent.Add($d) }
    foreach ($e in $existing) {
      if (-not (Test-AnetSameDir $e.Dir $o.Prefix) -and @($otherCurrent | Where-Object { Test-AnetSameDir $_ $e.Dir }).Count -eq 0) { $otherCurrent.Add($e.Dir) }
    }

    # 10. PATH: this directory first; another 0.3 installation off it when
    # its directory holds nothing else; a new powershell.exe asked which
    # anet it runs.
    $removable = New-Object System.Collections.Generic.List[string]
    foreach ($d in $otherCurrent) {
      if (Test-AnetDedicatedDir -Dir $d) {
        $removable.Add($d)
      } else {
        Write-AnetLine ($d + ' stays on your PATH: it holds another anet 0.3 installation beside other files; the new anet comes before it')
      }
    }
    if (-not $o.NoPath) {
      Set-AnetUserPath -Dir $o.Prefix -Remove $removable.ToArray()
      $other = Test-AnetPathOrder -Exe $S.Exe
      if ($other -ne '') { $notes.Add((Get-AnetText 'note_path_other' @{ other = $other; exe = $S.Exe })) }
    }
    foreach ($d in $otherCurrent) {
      Write-AnetLine ('Another anet 0.3 installation stays in ' + $d + '; delete that directory when no node runs from it.')
    }

    # The commands that complete the installation when it stops here.
    $completion = New-Object System.Collections.Generic.List[string]
    if (@($o.Without).Count -gt 0) { $completion.Add('anet init --without ' + ($o.Without -join ',')) }
    $completion.Add('anet init')
    if ($o.Controller -ne '') { $completion.Add('anet init --shell-controller ' + $o.Controller + ' --yes') }
    if ($o.Start) { $completion.Add('anet up') }
    if ($o.Join) { $completion.Add('anet hub-register ' + $o.Hub + ' --name ' + (ConvertTo-AnetArgument -Text $o.Name) + ' --passphrase-later') }
    if ($takeover.Held) {
      $S.Lines = Get-AnetInstalledFailure -State $S -Message (Get-AnetText 'held' @{ dir = $dataDir; label = $takeover.Label }) -Steps (@($takeover.Remaining) + $completion.ToArray())
      $S.Error = $true
      $global:LASTEXITCODE = $AnetExitRemaining
      return
    }

    # 11. -Without.
    if (@($o.Without).Count -gt 0) {
      $list = $o.Without -join ','
      $r = Invoke-AnetCommand -Exe $S.Exe -ArgumentList @('init', '--without', $list) -TimeoutSeconds 120
      if ($r['Code'] -ne 0) {
        $S.Lines = Get-AnetInstalledFailure -State $S -Message ('anet init --without ' + $list + ' refused (exit status ' + $r['Code'] + '): ' + (Get-AnetLastLine $r)) -Steps $completion.ToArray()
        $S.Error = $true
        $global:LASTEXITCODE = $r['Code']
        return
      }
      $completion.RemoveAt(0)
    }

    # 12. Defaults: inbound closed, no public capabilities, spending limits 0.
    $r = Invoke-AnetCommand -Exe $S.Exe -ArgumentList @('init') -TimeoutSeconds 120
    if ($r['Code'] -ne 0) {
      $S.Lines = Get-AnetInstalledFailure -State $S -Message (Get-AnetText 'init_failed' @{ code = $r['Code'] }) -Steps $completion.ToArray()
      $S.Error = $true
      $global:LASTEXITCODE = $r['Code']
      return
    }
    $completion.RemoveAt(0)

    # 12b. -Agents or -AgentList: the coding agents get the tools and guide
    # of anet. A failure is a note; the installation goes on.
    if (@($o.Agents).Count -gt 0) {
      Write-AnetLine ('-> anet agents wire ' + ($o.Agents -join ' '))
      $r = Invoke-AnetCommand -Exe $S.Exe -ArgumentList (@('agents', 'wire') + @($o.Agents)) -TimeoutSeconds 120
      if ($r['Code'] -ne 0) { $notes.Add((Get-AnetText 'note_agents' @{ list = ($o.Agents -join ' ') })) }
    }

    # 13. A node that runs: stopped to change its configuration (-Controller)
    # or to run the new binary (when it is started again below).
    # -Controller on the command line is the consent, given to anet init
    # with --yes; nothing is asked.
    $st = Get-AnetNodeStatus -Exe $S.Exe
    $restart = $st.Running -and ($o.Controller -ne '' -or ($o.Start -and $st.Version -ne $version))
    if ($restart) {
      $r = Invoke-AnetCommand -Exe $S.Exe -ArgumentList @('stop') -TimeoutSeconds 120
      if ($r['Code'] -ne 0) {
        $S.Lines = Get-AnetInstalledFailure -State $S -Message ('anet stop did not complete (exit status ' + $r['Code'] + '): ' + (Get-AnetLastLine $r)) -Steps (@('anet stop') + $completion.ToArray())
        $S.Error = $true
        $global:LASTEXITCODE = $r['Code']
        return
      }
    } elseif ($st.Running -and -not $o.Start -and $st.Version -ne '' -and $st.Version -ne $version) {
      $notes.Add((Get-AnetText 'note_old_daemon' @{ old = $st.Version }))
    }
    if ($o.Controller -ne '') {
      $r = Invoke-AnetCommand -Exe $S.Exe -ArgumentList @('init', '--shell-controller', $o.Controller, '--yes') -TimeoutSeconds 120
      if ($r['Code'] -ne 0) {
        $steps = @()
        if ($restart) { $steps = @('anet up') }
        $S.Lines = Get-AnetInstalledFailure -State $S -Message ('anet init --shell-controller refused (exit status ' + $r['Code'] + '): ' + (Get-AnetLastLine $r) + '; the controller was not given access') -Steps $steps
        $S.Error = $true
        $global:LASTEXITCODE = $r['Code']
        return
      }
      $completion.RemoveAt(0)
    }

    # 14. Start.
    $started = $false
    if ($o.Start) {
      $start = Start-AnetNode -Exe $S.Exe -Elevation $elevation
      if ($start.Note -ne '') { $notes.Add((Get-AnetText $start.Note)) }
      if ($start.Deferred) {
        # Not started with the rights of this window: it starts at the next
        # logon through the Run value, or now by hand in a normal window.
        $autostart = (-not $o.NoAutostart)
        if ($autostart) { Set-AnetAutostart -Exe $S.Exe }
        $message = Get-AnetText 'deferred' @{ why = $start.Why }
        if ($autostart) { $message += ' ' + (Get-AnetText 'deferred_logon') }
        $S.Lines = Get-AnetInstalledFailure -State $S -Message $message -Steps $completion.ToArray()
        $S.Error = $true
        $global:LASTEXITCODE = $AnetExitRemaining
        return
      }
      if ($start.Code -ne 0) {
        $S.Lines = Get-AnetInstalledFailure -State $S -Message (Get-AnetText 'start_failed' @{ code = $start.Code }) -Steps $completion.ToArray()
        $S.Error = $true
        $global:LASTEXITCODE = $start.Code
        return
      }
      $started = $true
      $completion.RemoveAt(0)
      for ($i = 0; $i -lt 20; $i++) {
        if ((Get-AnetNodeStatus -Exe $S.Exe -Quiet).Running) { break }
        Start-Sleep -Seconds 1
      }
      $st = Get-AnetNodeStatus -Exe $S.Exe
    } else {
      $notes.Add((Get-AnetText 'note_nostart'))
    }

    # 15. Autostart. A Run value an earlier installation wrote is pointed
    # at this one.
    if (-not $o.NoAutostart -and ($started -or (Get-AnetRunValueExe) -ne '')) { Set-AnetAutostart -Exe $S.Exe }

    # 16. What the node is set up to do, and what -Controller granted, for
    # the log.
    $null = Invoke-AnetCommand -Exe $S.Exe -ArgumentList @('doctor') -TimeoutSeconds 120
    if ($o.Controller -ne '' -and -not (Write-AnetGrant -DataDir $dataDir -Aid $o.Controller)) {
      $notes.Add((Get-AnetText 'note_grant' @{ config = (Join-Path $dataDir 'config.json') }))
    }

    # 17. Join, last. --passphrase-later: nothing is asked; the passphrase
    # is set later with anet identity protect.
    $joinState = ''
    $joinError = ''
    $code = 0
    if ($o.Join -and $started) {
      $r = Invoke-AnetCommand -Exe $S.Exe -ArgumentList @('hub-register', $o.Hub, '--name', $o.Name, '--passphrase-later') -TimeoutSeconds 300
      $code = $r['Code']
      if ($code -ne 0) {
        $reason = Get-AnetLastLine $r
        if ($r['TimedOut']) { $reason = 'no answer within 300 seconds' }
        $joinError = Get-AnetText 'join_refused' @{ hub = $o.Hub; reason = $reason }
        $S.Error = $true
      } else {
        $joinState = Read-AnetJoinState -DataDir $dataDir -Hub $o.Hub
      }
    }

    $run = 'new'
    if ($o.NoPath) { $run = 'path' } elseif (Test-AnetPathHere -PathText $startPath -Exe $S.Exe) { $run = 'here' }
    $hubShown = ''
    if ($o.Join -and $started) { $hubShown = $o.Hub }
    $S.Lines = Get-AnetSummary -F @{ Version = $version; Running = $st.Running; Aid = $st.Aid; Hub = $hubShown; JoinState = $joinState;
      Error = $joinError; Taken = $takeover.Taken; Controller = $o.Controller; Notes = $notes.ToArray(); Run = $run; Exe = $S.Exe }
    $global:LASTEXITCODE = $code
  } catch {
    $e = $_.Exception
    $code = 1
    if ($e.Data['AnetFailure']) { $code = [int]$e.Data['AnetCode'] }
    Write-AnetLine ('failure: ' + $e.Message)
    if (-not $e.Data['AnetFailure']) { Write-AnetLine ([string]$_.ScriptStackTrace) }
    if ($S.Installed) {
      $S.Lines = Get-AnetInstalledFailure -State $S -Message $e.Message -Steps @()
    } else {
      $S.Lines = @((Get-AnetText 'error' @{ text = $e.Message }), (Get-AnetText 'nothing'))
    }
    $S.Error = $true
    $global:LASTEXITCODE = $code
  } finally {
    Close-AnetGuard -State $S
    if ($S.Staged -ne '' -and (Test-Path -LiteralPath $S.Staged)) {
      Remove-Item -LiteralPath $S.Staged -Force -ErrorAction SilentlyContinue
    }
    if ($S.Tmp -ne '' -and (Test-Path -LiteralPath $S.Tmp)) {
      Remove-Item -LiteralPath $S.Tmp -Recurse -Force -ErrorAction SilentlyContinue
    }
    Write-AnetLine ''
    foreach ($l in @($S.Lines)) { Write-AnetLine $l }
    Write-AnetLine ('exit status ' + $global:LASTEXITCODE)
    # Saving the log runs icacls, which sets $LASTEXITCODE; the result of the
    # installation is restored after it.
    $exitCode = $global:LASTEXITCODE
    $why = @{}
    $log = ''
    try { $log = Save-AnetLog -DataDir $S.DataDir -Why $why } catch { $why['Text'] = $_.Exception.Message }
    $errorPrefix = (Get-AnetText 'error' @{ text = '' })
    $red = (Test-AnetInteractive) -and [string]::IsNullOrEmpty($env:NO_COLOR)
    foreach ($l in @($S.Lines)) {
      if ($red -and ([string]$l).StartsWith($errorPrefix)) { Write-Host $l -ForegroundColor Red } else { Write-Host $l }
    }
    Write-Host (Get-AnetDetailsLine -Log $log -Why ([string]$why['Text']))
    $global:LASTEXITCODE = $exitCode
    $ProgressPreference = $previousProgress
    $ErrorActionPreference = $previousEAP
  }
}

if ($env:ANET_INSTALL_PS1_TEST -ne '1') { Install-Anet @PSBoundParameters; if (Test-AnetOneShotHost) { exit $global:LASTEXITCODE } }
