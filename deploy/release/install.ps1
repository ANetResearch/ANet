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
# these shells would interpret. Install only (the default build):
#
#   powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command "[Net.ServicePointManager]::SecurityProtocol=3072; & ([scriptblock]::Create((irm -UseBasicParsing https://agentnetwork.org.cn/install.ps1)))"
#
# Install the shell build and give the agent CONTROLLER_AID (replace it with
# that agent's AID) the right to run commands on this computer as this user,
# start the node, register it at the hub and start it at every logon. Giving
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
#   -Controller AID  Install the shell build, give the agent with this AID the
#                    right to run commands on this machine as this user, start
#                    the node, register it at the hub and start it at every
#                    logon.
#   -Shell           Install the shell build without giving anyone access.
#   -Hub URL         Start the node and join this hub (default with
#                    -Controller: https://hub.agentnetwork.org.cn).
#   -NoJoin          Do not register at a hub (with -Controller, the node is
#                    still started); register later with anet hub-register.
#   -Name NAME       The name to join under (default: the computer name).
#   -Prefix DIR      Install into DIR instead of the directory the installer
#                    chooses. No directory above DIR may let another account
#                    rename it or change its access control list; DIR must not
#                    contain ; or %.
#   -Without LIST    Turn these modules off in config.json.
#   -Base URL|DIR    Download base, https only, or a local copy of a release
#                    directory; replaces the list of download hosts.
#   -NoAutostart     Do not start the node at logon.
#   -NoPath          Do not put the install directory on the user's PATH.
#   -Elevated        Accepted for compatibility; it changes nothing.
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
# prefix, is stopped with its own stop command, the installer waits for its
# daemon to exit, its data in %USERPROFILE%\.anet is renamed aside (never
# deleted; a directory whose identity file is this version's keeps it, and
# only the 1.x entries move), and only then is the npm package removed with
# npm uninstall -g @agentnetwork/anet; a data directory of an earlier anet
# is moved the same way when no earlier installation was found, once no
# daemon holds it. When the data cannot be moved (the daemon still runs, or
# the move failed), the installation stops before anet init with exit
# status 3 and the exact commands, and the npm package stays. Another anet 0.3 or later is left in
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
#      download host that serves anything else is skipped.
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
# shell = powershell; the AID in peers.allow; the defaults of `anet init`
# (inbound closed, no public capabilities, spending limits 0); the hub
# registration (not with -NoJoin); the Run value AgentNetwork under
# HKCU\Software\Microsoft\Windows\CurrentVersion\Run; the user's PATH. Nothing
# else: no auto-reply, no other peer, no public capability, no listener beyond
# the node's loopback ones. The installer prints what was granted, and how to
# revoke it, at the end.
#
# Everything below is a parameter block, the release pins and function
# definitions; the last line calls Install-Anet. A download cut short does
# not parse, and runs nothing. The script ends the PowerShell process only
# when that process was started for this one command (powershell.exe
# -Command, as in the lines above), with the result as its exit status.

param(
  [string]$Controller, [switch]$Shell, [string]$Hub, [string]$Name, [string]$Prefix,
  [string[]]$Without, [string]$Base, [switch]$NoAutostart, [switch]$NoPath, [switch]$Elevated,
  [switch]$NoJoin
)

# BEGIN RELEASE PINS (written by deploy/release/build-release.sh; do not edit)
$AnetRelease = $null
# END RELEASE PINS

# The fingerprint of the release key, which the binary reports after it has
# verified the manifest with the key compiled into it.
$AnetReleaseKeyFingerprint = 'SHA256:/4FMm/jgZcBII3z3O3r81Y8SxFfugdLRu3zj2gnclD4'

function Write-AnetLine {
  param([string]$Text)
  Write-Host $Text
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

# Invoke-AnetNative is the one place this script runs a program: anet.exe,
# icacls.exe and taskkill.exe. The arguments are an array (no command line is assembled from
# text), stderr is never redirected (Windows PowerShell 5.1 would turn its
# lines into errors), and a non-zero exit status is reported in $Result.Code,
# not thrown. With -Capture, stdout is returned as lines in $Result.Output;
# without it, the program writes to the console directly. A caller that wants
# the output on the console must not assign the call.
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

# Get-AnetOptions validates the parameters (a hashtable of what was given)
# and returns what the install will do. Nothing is read from or written to
# the machine except $env:COMPUTERNAME, ANET_INSTALL_BASE and, for -Base and
# -Prefix, whether a directory exists. Without -Prefix, Prefix is empty: the
# install directory is chosen later (Select-AnetInstallDir).
function Get-AnetOptions {
  param([Parameter(Mandatory = $true)][hashtable]$Given)
  $o = @{
    Controller = ''; Variant = 'default'; AssetPrefix = 'anet'; Hub = ''; Join = $false; Name = '';
    Prefix = ''; PrefixGiven = $false; Without = @(); Base = ''; NoAutostart = [bool]$Given['NoAutostart'];
    NoPath = [bool]$Given['NoPath']; Elevated = [bool]$Given['Elevated']; NoJoin = [bool]$Given['NoJoin']
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
  if ($Given.ContainsKey('Hub')) {
    $hub = [string]$Given['Hub']
    if (-not (Test-AnetHttpsUrl -Text $hub -AllowLoopbackHttp)) {
      throw (New-AnetFailure ('-Hub ' + $hub + ' is not an absolute https:// URL (http:// only for 127.0.0.1 or localhost)'))
    }
    $o.Hub = $hub
  } elseif ($o.Controller -ne '' -and -not $o.NoJoin) {
    $o.Hub = 'https://hub.agentnetwork.org.cn'
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

# Get-AnetBases is the list of download bases, in the order they are tried:
# ANET_INSTALL_BASE, the project's two hosts, then the npm package of the
# build on three content delivery networks for npm (the order of
# release.MirrorBases in `anet update`). -Base replaces the list.
function Get-AnetBases {
  param([Parameter(Mandatory = $true)][hashtable]$Options, [Parameter(Mandatory = $true)][string]$Arch)
  if ($Options.Base -ne '') { return @($Options.Base) }
  $bases = New-Object System.Collections.Generic.List[string]
  if (-not [string]::IsNullOrEmpty($env:ANET_INSTALL_BASE)) {
    $bases.Add((Get-AnetBaseValue -Text $env:ANET_INSTALL_BASE -What 'ANET_INSTALL_BASE'))
  }
  $bases.Add('https://agentnetwork.org.cn')
  $bases.Add('https://hub.agentnetwork.org.cn')
  $pkg = Get-AnetNpmPackage -AssetPrefix $Options.AssetPrefix -Arch $Arch
  $bases.Add('https://registry.npmmirror.com/' + $pkg + '/latest/files')
  $bases.Add('https://cdn.jsdelivr.net/npm/' + $pkg + '@latest')
  $bases.Add('https://unpkg.com/' + $pkg + '@latest')
  return $bases.ToArray()
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

# Get-AnetHttpBytes fetches an https URL and returns its body, at most $Limit
# bytes (exactly $Limit when -Exact): redirects are followed by hand, at
# most five, and only to https.
function Get-AnetHttpBytes {
  param([string]$Url, [long]$Limit, [switch]$Exact, [int]$TimeoutSeconds)
  $socketsHandler = 'System.Net.Http.SocketsHttpHandler' -as [type]
  if ($null -ne $socketsHandler) {
    $handler = New-Object System.Net.Http.SocketsHttpHandler
    $handler.ConnectTimeout = [TimeSpan]::FromSeconds(15)
  } else {
    $handler = New-Object System.Net.Http.HttpClientHandler
  }
  $handler.AllowAutoRedirect = $false
  $client = New-Object System.Net.Http.HttpClient -ArgumentList $handler
  try {
    $client.Timeout = [TimeSpan]::FromSeconds($TimeoutSeconds)
    $client.MaxResponseContentBufferSize = $Limit
    $uri = New-Object System.Uri $Url
    for ($hop = 0; $hop -le 5; $hop++) {
      if ($uri.Scheme -cne 'https') { throw ('refusing ' + $uri.AbsoluteUri + ': downloads are https only') }
      $req = New-Object System.Net.Http.HttpRequestMessage -ArgumentList ([System.Net.Http.HttpMethod]::Get), $uri
      $req.Headers.Add('Cache-Control', 'no-cache')
      $resp = $client.SendAsync($req, [System.Net.Http.HttpCompletionOption]::ResponseContentRead).GetAwaiter().GetResult()
      try {
        $code = [int]$resp.StatusCode
        if ($code -eq 301 -or $code -eq 302 -or $code -eq 303 -or $code -eq 307 -or $code -eq 308) {
          if ($null -eq $resp.Headers.Location) { throw ('HTTP ' + $code + ' without a Location') }
          $uri = New-Object System.Uri -ArgumentList $uri, $resp.Headers.Location
          continue
        }
        if ($code -ne 200) { throw ('HTTP ' + $code) }
        $body = $resp.Content.ReadAsByteArrayAsync().GetAwaiter().GetResult()
        if ($body.Length -gt $Limit -or ($Exact -and $body.Length -ne $Limit)) {
          throw ($body.Length.ToString() + ' bytes, expected ' + $(if ($Exact) { 'exactly ' } else { 'at most ' }) + $Limit)
        }
        return ,$body
      } finally {
        $resp.Dispose()
      }
    }
    throw 'more than 5 redirects'
  } finally {
    $client.Dispose()
  }
}

# Get-AnetBaseFile reads one release file from a base: <base>/dl/<name> for
# an https base, <dir>\<name> for a local release directory.
function Get-AnetBaseFile {
  param([string]$Base, [string]$Name, [long]$Limit, [switch]$Exact, [int]$TimeoutSeconds)
  if ($Base -like 'https://*') {
    return ,(Get-AnetHttpBytes -Url ($Base + '/dl/' + $Name) -Limit $Limit -Exact:$Exact -TimeoutSeconds $TimeoutSeconds)
  }
  $path = Join-Path $Base $Name
  $item = Get-Item -LiteralPath $path -Force -ErrorAction Stop
  if ($item.PSIsContainer) { throw ($path + ' is a directory') }
  if ($item.Length -gt $Limit -or ($Exact -and $item.Length -ne $Limit)) {
    throw ($path + ' is ' + $item.Length + ' bytes, expected ' + $(if ($Exact) { 'exactly ' } else { 'at most ' }) + $Limit)
  }
  return ,([IO.File]::ReadAllBytes($path))
}

# Get-AnetRelease tries the bases in order and returns the first that serves
# release.json, its signature and a .gz that matches the pins.
function Get-AnetRelease {
  param([string[]]$Bases, [string]$Asset, [hashtable]$Pin)
  $tried = New-Object System.Collections.Generic.List[string]
  foreach ($b in $Bases) {
    Write-AnetLine ('-> download base: ' + $b)
    try {
      $manifest = Get-AnetBaseFile -Base $b -Name 'release.json' -Limit 1048576 -TimeoutSeconds 15
      if ($manifest.Length -eq 0 -or $manifest[0] -ne 0x7B) { throw 'release.json is not a manifest' }
      $sig = Get-AnetBaseFile -Base $b -Name 'release.json.sig' -Limit 16384 -TimeoutSeconds 15
      $first = ([Text.Encoding]::ASCII.GetString($sig) -split "`r?`n", 2)[0]
      if ($first -cne '-----BEGIN SSH SIGNATURE-----') { throw 'release.json.sig is not an SSH signature' }
      $gz = Get-AnetBaseFile -Base $b -Name ($Asset + '.gz') -Limit ([long]$Pin['GzSize']) -Exact -TimeoutSeconds 600
      $bin = Test-AnetPin -Gz $gz -Pin $Pin
      Write-AnetLine ('OK ' + $Asset + '.gz and the binary inside it match the release pins (SHA-256 and size)')
      return @{ Base = $b; Manifest = $manifest; Signature = $sig; Binary = $bin }
    } catch {
      $why = $_.Exception.Message
      if ($null -ne $_.Exception.InnerException) { $why = $_.Exception.InnerException.Message }
      Write-AnetLine ('   not used: ' + $why)
      $tried.Add($b)
    }
  }
  throw (New-AnetFailure ('no download base served release ' + $AnetRelease['Version'] + ' (tried: ' + ($tried -join ', ') + ')'))
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
  $r = @{}
  Invoke-AnetNative -FilePath $Staged -ArgumentList $argv -Capture -Result $r
  if ($r['Code'] -ne 0) {
    throw (New-AnetFailure ('the downloaded binary did not verify the release (exit status ' + $r['Code'] + '); its reason is above'))
  }
  $lines = @($r['Output'] | Where-Object { ([string]$_).Trim() -ne '' })
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
# reported, rather than one that is believed.
function Save-AnetRecord {
  param([string]$Exe, [string]$TmpDir)
  foreach ($pair in @(@('release.json.sig', '.release.json.sig'), @('release.json', '.release.json'))) {
    $dst = $Exe + $pair[1]
    try {
      if (Test-Path -LiteralPath $dst) { Remove-Item -LiteralPath $dst -Force -ErrorAction Stop }
      [IO.File]::Copy((Join-Path $TmpDir $pair[0]), $dst, $false)
    } catch {
      Write-AnetLine ('Warning: could not write ' + $dst + '; anet doctor will report the release signature as unknown')
      return
    }
  }
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

# Invoke-AnetTimed runs a program that is not the anet this script installs
# (an earlier anet, npm, a fresh powershell.exe): its standard input is
# closed at once, so it cannot wait for the console; its output is
# captured; and after $TimeoutSeconds it is ended, with every process it
# started in turn (Stop-AnetProcessTree), and reported as timed out. With
# $PathVariable, the program gets that PATH instead of this session's. A
# program that cannot start sets Code to -1.
function Invoke-AnetTimed {
  param([Parameter(Mandatory = $true)][string]$FilePath, [string[]]$ArgumentList = @(), [int]$TimeoutSeconds = 30,
        [string]$PathVariable = '', [Parameter(Mandatory = $true)][hashtable]$Result)
  $Result['Code'] = -1
  $Result['Output'] = @()
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
    $text = ''
    if ($out.Wait(5000)) { $text = [string]$out.Result }
    if ($err.Wait(1000)) { $text += "`n" + [string]$err.Result }
    $Result['Output'] = @($text -split "`r?`n" | Where-Object { ([string]$_).Trim() -ne '' })
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

# Test-AnetDataDirBusy: the daemon of AgentNetwork 1.x may still hold the
# data directory. 1.x writes its process ID to daemon.pid and serves
# 127.0.0.1:3998; the directory is busy while that process runs under a name
# that contains anet, or while that address accepts connections. Any doubt
# counts as busy.
function Test-AnetDataDirBusy {
  param([string]$Root)
  $pidFile = Join-Path $Root 'daemon.pid'
  if (Test-Path -LiteralPath $pidFile -PathType Leaf) {
    try {
      $text = [IO.File]::ReadAllText($pidFile)
      if ($text.Length -gt 32) { $text = $text.Substring(0, 32) }
      $digits = $text -replace '[^0-9]', ''
      if ($digits.Length -gt 0 -and $digits.Length -le 9) {
        $proc = Get-Process -Id ([int]$digits) -ErrorAction SilentlyContinue
        if ($null -ne $proc -and $proc.ProcessName -match 'anet') { return $true }
      }
    } catch { }
  }
  return (Test-AnetLoopbackListens -Port $AnetV1Port)
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
# stopped with its own stop command; the data directory $DataDir, when it is
# an earlier anet's and its daemon has exited, is moved aside
# (Move-AnetLegacyData); only then is the npm package of 1.x uninstalled.
# A data directory of an earlier anet is moved whether or not an earlier
# installation was found, as install.sh does, once it is free. When it
# cannot be moved (its daemon still runs, or the move failed), Held is set,
# the package stays, and Remaining lists the exact commands to run. Another anet 0.3 or later
# outside npm is reported, for its directory to be taken off the PATH; an
# npm launcher of 0.3 or later, and anything that is not an anet of either
# line, are left alone. From a Restricted window (elevated, while Windows has
# a standard token for the account), nothing found on the machine is run:
# the stop and npm commands go to Remaining instead, and the data is moved
# only when its daemon is not running.
function Invoke-AnetTakeover {
  param([string]$InstallDir, [string]$DataDir, [hashtable]$Elevation)
  $report = New-Object System.Collections.Generic.List[string]
  $current = New-Object System.Collections.Generic.List[string]
  $remaining = New-Object System.Collections.Generic.List[string]
  $restricted = [bool]$Elevation.Restricted
  $npm = Find-AnetNpm
  $npmPrefix = ''
  if ($restricted) {
    $npmPrefix = Get-AnetNpmDefaultPrefix
  } elseif ($npm -ne '') {
    $r = @{}
    Invoke-AnetTimed -FilePath $npm -ArgumentList @('prefix', '-g') -TimeoutSeconds 60 -Result $r
    $lines = @($r['Output'])
    if ($r['Code'] -eq 0 -and $lines.Count -gt 0 -and [IO.Path]::IsPathRooted(([string]$lines[0]).Trim())) {
      $npmPrefix = ([string]$lines[0]).Trim()
    }
  }
  $others = @(Find-AnetOthers -InstallDir $InstallDir -NpmPrefix $npmPrefix -Elevation $Elevation)
  $legacy = New-Object System.Collections.Generic.List[hashtable]
  $legacyVersion = ''
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
    Write-AnetLine ('-> taking over from anet ' + $label + ' in ' + $e.Dir)
    if ($e.StopExe -eq '') {
      $note = 'anet ' + $label + ' in ' + $e.Dir + ' has no binary of AgentNetwork 1.x this installer can run to stop it'
      if ($e.StopNote -ne '') { $note += ' (' + $e.StopNote + ')' }
      $report.Add($note)
      continue
    }
    $stopCommand = '& ''' + $e.StopExe + ''' stop'
    if ($restricted) {
      $report.Add('not run from this elevated window: ' + $e.StopExe + ' stop')
      $remaining.Add($stopCommand)
      continue
    }
    $r = @{}
    Invoke-AnetTimed -FilePath $e.StopExe -ArgumentList @('stop') -TimeoutSeconds 30 -Result $r
    if ($r['TimedOut']) {
      $report.Add('Warning: ' + $e.StopExe + ' stop did not finish within 30 seconds and was ended')
      $remaining.Add($stopCommand)
    } elseif ($r['Code'] -ne 0) {
      $report.Add('Warning: ' + $e.StopExe + ' stop ended with exit status ' + $r['Code'])
    } else {
      $report.Add('stopped anet ' + $label + ' with its own stop command (' + $e.StopExe + ' stop)')
    }
  }

  # The data directory, after the daemon that may hold it has exited;
  # found or not, an earlier anet's data is moved as install.sh moves it.
  $held = $false
  $kind = Get-AnetDataDirKind -Root $DataDir
  $version = $legacyVersion
  if ($kind -eq 'older') { $version = '0.2' } elseif ($version -eq '') { $version = '1.x' }
  if ($kind -eq 'older' -or $kind -eq 'v1' -or $kind -eq 'mixed') {
    $aside = Get-AnetLegacyAside -Root $DataDir -Version $version
    $moveCommand = Get-AnetMoveCommand -Root $DataDir -Aside $aside -Kind $kind
    if (-not (Wait-AnetDataDirFree -Root $DataDir -Seconds 15)) {
      $held = $true
      $report.Add('Warning: ' + $DataDir + ' is still in use by the daemon of the earlier anet (the process of its daemon.pid runs, or 127.0.0.1:' + $AnetV1Port + ' answers), so it was not moved; the new anet does not start on it')
      $remaining.Add($moveCommand)
    } else {
      $m = Move-AnetLegacyData -Root $DataDir -Version $version -Kind $kind -Aside $aside
      $report.Add($m.Line)
      if ($m.Held) {
        $held = $true
        $remaining.Add($moveCommand)
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
        $remaining.Add($typed)
        continue
      }
      $r = @{}
      Invoke-AnetTimed -FilePath $npm -ArgumentList $argv -TimeoutSeconds 180 -Result $r
      if ($r['Code'] -eq 0 -and -not (Test-Path -LiteralPath (Join-AnetPath $e.Dir 'node_modules\@agentnetwork\anet\package.json'))) {
        $report.Add('removed the npm package @agentnetwork/anet ' + $e.Version + ' (' + $typed + ')')
      } else {
        $report.Add('Warning: ' + $typed + ' did not remove the package (exit status ' + $r['Code'] + ')')
        $remaining.Add($typed)
      }
    } else {
      $report.Add('left in place: ' + $e.Dir + '\anet.exe (anet ' + $e.Version + '); the new anet comes before it on your PATH unless that directory is on the machine PATH')
    }
  }
  return @{ Report = $report.ToArray(); Current = $current.ToArray(); Held = $held; Remaining = $remaining.ToArray() }
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
# anet it runs, and reports in one line whether that is $Exe.
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
  } elseif ($got -ne '') {
    Write-AnetLine ('Warning: a new window runs ' + $got + ' for anet, not ' + $Exe + '; its directory comes first on the machine PATH: remove that anet, or run ' + $Exe + ' by its full path')
  } else {
    Write-AnetLine ('Warning: a new powershell.exe could not say which anet it runs; check with: (Get-Command anet).Source')
  }
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
  $res = @{ Code = 0; Deferred = $false; Why = '' }
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
  }
  # The lines of anet up go to the console, not into the value returned.
  $r = @{}
  Invoke-AnetNative -FilePath $Exe -ArgumentList @('up') -Result $r | Out-Host
  $res.Code = $r['Code']
  return $res
}

# Write-AnetGrant prints what -Controller granted, read back from the data
# directory, and the two steps that revoke it.
function Write-AnetGrant {
  param([string]$DataDir, [string]$Aid)
  $config = Join-Path $DataDir 'config.json'
  $allowFile = Join-Path $DataDir 'peers.allow'
  $shellText = '(config.json could not be read)'
  try {
    $cfg = [IO.File]::ReadAllText($config) | ConvertFrom-Json
    $sh = $cfg.modules.shell
    $parts = @('allow = [' + (@($sh.allow) -join ', ') + ']', ('allow_arbitrary = ' + ([string]$sh.allow_arbitrary).ToLowerInvariant()))
    if (@($sh.PSObject.Properties.Name) -contains 'shell') { $parts += ('shell = ' + [string]$sh.shell) }
    $shellText = $parts -join ', '
  } catch { }
  $listed = 'no'
  try {
    if (@([IO.File]::ReadAllLines($allowFile) | Where-Object { $_.Trim() -ceq $Aid }).Count -gt 0) { $listed = 'yes' }
  } catch { }
  Write-AnetLine ''
  Write-AnetLine ('Granted, as -Controller on the command line requested (that parameter is the consent; nothing was asked):')
  Write-AnetLine ('  modules.shell in ' + $config + ': ' + $shellText)
  Write-AnetLine ('  peers.allow in ' + $allowFile + ': ' + $Aid + ' (listed: ' + $listed + ')')
  Write-AnetLine ('The agent ' + $Aid + ' can run any command on this computer as this user. To revoke it:')
  Write-AnetLine ('  1. anet peers remove ' + $Aid)
  Write-AnetLine ('  2. remove the modules.shell block from ' + $config + ', then restart the node: anet stop, then anet up')
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

# Write-AnetInstalledFailure ends a run after the binary is installed: what
# went wrong, what is installed, and the commands that remain, in order.
function Write-AnetInstalledFailure {
  param([hashtable]$State, [string]$Message, [string[]]$Steps = @())
  Write-AnetLine ('Error: ' + $Message)
  Write-AnetLine ('anet ' + $AnetRelease['Version'] + ' is installed at ' + $State.Exe + '.')
  if (@($Steps).Count -gt 0) {
    Write-AnetLine 'Run by hand, in a normal window of this account, in this order:'
    foreach ($s in @($Steps)) { Write-AnetLine ('  ' + $s) }
  }
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
  $r = @{}
  Invoke-AnetNative -FilePath $Exe -ArgumentList @('version') -Capture -Result $r
  $lines = @($r['Output'])
  if ($r['Code'] -eq 0 -and $lines.Count -gt 0 -and ([string]$lines[0]) -cmatch '^anet (\S+) ') {
    if (Test-AnetVersion $Matches[1]) { return $Matches[1] }
  }
  Write-AnetLine ('Warning: cannot read the version of the existing ' + $Exe + '; it will be replaced')
  return ''
}

function Install-Anet {
  param(
    [string]$Controller, [switch]$Shell, [string]$Hub, [string]$Name, [string]$Prefix,
    [string[]]$Without, [string]$Base, [switch]$NoAutostart, [switch]$NoPath, [switch]$Elevated,
    [switch]$NoJoin
  )
  $previousEAP = $ErrorActionPreference
  $previousProgress = $ProgressPreference
  Set-StrictMode -Version 3
  $ErrorActionPreference = 'Stop'
  $ProgressPreference = 'SilentlyContinue'
  $given = @{}
  foreach ($k in $PSBoundParameters.Keys) { $given[$k] = $PSBoundParameters[$k] }
  $S = @{ Installed = $false; Exe = ''; Tmp = ''; Staged = ''; Guard = $null }
  $global:LASTEXITCODE = 0
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
    # started with standard user rights when Windows has them (step 13). An
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
    $pin = $AnetRelease['Assets'][$asset]
    Write-AnetLine ('-> Installing anet ' + $AnetRelease['Version'] + ' (' + $o.Variant + ' build, windows-' + $arch + ') for ' + $elevation.Account)
    if ($elevation.Elevated) {
      Write-AnetLine '   This window is elevated. anet is installed for this account all the same: its own directories, PATH and Run value.'
      if ($elevation.Restricted) {
        Write-AnetLine '   Programs already on this computer are not run from this window; the commands that need one are printed instead.'
      }
    }
    if ($o.Variant -eq 'shell') {
      Write-AnetLine '   The shell build CAN run commands on this machine for the agents its configuration allows.'
    }

    # 3. and 4. Bases, download and pin check.
    if ($PSVersionTable.PSEdition -ne 'Core') {
      # Windows PowerShell 5.1: add TLS 1.2 to what the session allows.
      [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor 3072
    }
    if ($null -eq ('System.Net.Http.HttpClient' -as [type])) { Add-Type -AssemblyName System.Net.Http }
    $bases = @(Get-AnetBases -Options $o -Arch $arch)
    $S.Tmp = Join-Path ([IO.Path]::GetTempPath()) ('anet-install-' + [Guid]::NewGuid().ToString('N'))
    $null = [IO.Directory]::CreateDirectory($S.Tmp)
    $rel = Get-AnetRelease -Bases $bases -Asset $asset -Pin $pin
    [IO.File]::WriteAllBytes((Join-Path $S.Tmp 'release.json'), $rel.Manifest)
    [IO.File]::WriteAllBytes((Join-Path $S.Tmp 'release.json.sig'), $rel.Signature)

    # 5. Install directory: -Prefix, or the first candidate that passes.
    $existing = @(Get-AnetExistingInstalls -Elevation $elevation)
    if ($o.PrefixGiven) {
      $why = Get-AnetDirectoryProblem -Dir $o.Prefix
      if ($null -ne $why) {
        Write-AnetLine ('   ' + $why)
        throw (New-AnetFailure ('-Prefix ' + $o.Prefix + ' cannot be used: it is not private to this user; choose another directory, or leave -Prefix out and let the installer choose'))
      }
    } else {
      $o.Prefix = Select-AnetInstallDir -Existing $existing
    }
    Write-AnetLine ('-> install directory: ' + $o.Prefix)
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
    if ($installed -ne '') {
      Write-AnetLine ('   ' + $S.Exe + ': ' + $installed + ' -> ' + $AnetRelease['Version'])
    }

    # 7. -Without: the names are checked against the verified module set
    # here, and config.json is changed only after the binary is installed
    # (step 9), so that a failure before then leaves the machine unchanged.
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
    Save-AnetRecord -Exe $S.Exe -TmpDir $S.Tmp
    Write-AnetLine ''
    Write-AnetLine ('OK Installed anet ' + $AnetRelease['Version'] + ' -> ' + $S.Exe)
    if ($aside -ne '') {
      Write-AnetLine ('   The previous binary is ' + $aside + ' until it is no longer in use; a node still running it keeps it until restarted.')
    }

    # 9. Earlier installations: stop an earlier anet, move its data aside,
    # remove its npm package, and find other installations of 0.3 or later,
    # before the data directory is initialized for this one.
    $profileDir = [Environment]::GetFolderPath('UserProfile')
    $dataDir = [string]$env:ANET_DATA_DIR
    if ($dataDir -eq '') { $dataDir = Join-Path $profileDir '.anet' }
    $takeover = Invoke-AnetTakeover -InstallDir $o.Prefix -DataDir $dataDir -Elevation $elevation
    $otherCurrent = New-Object System.Collections.Generic.List[string]
    foreach ($d in @($takeover.Current)) { $otherCurrent.Add($d) }
    foreach ($e in $existing) {
      if (-not (Test-AnetSameDir $e.Dir $o.Prefix) -and @($otherCurrent | Where-Object { Test-AnetSameDir $_ $e.Dir }).Count -eq 0) { $otherCurrent.Add($e.Dir) }
    }
    foreach ($line in @($takeover.Report)) { Write-AnetLine ('   ' + $line) }

    # 10. PATH: this directory first; another 0.3 installation off it when
    # its directory holds nothing else; a new powershell.exe asked which
    # anet it runs.
    $removable = New-Object System.Collections.Generic.List[string]
    foreach ($d in $otherCurrent) {
      if (Test-AnetDedicatedDir -Dir $d) {
        $removable.Add($d)
      } else {
        Write-AnetLine ('   ' + $d + ' stays on your PATH: it holds another anet 0.3 installation beside other files; the new anet comes before it')
      }
    }
    if (-not $o.NoPath) {
      Set-AnetUserPath -Dir $o.Prefix -Remove $removable.ToArray()
      Test-AnetPathOrder -Exe $S.Exe
    }
    foreach ($d in $otherCurrent) {
      Write-AnetLine ('   Another anet 0.3 installation stays in ' + $d + '; delete that directory when no node runs from it.')
    }

    # The commands that complete the installation when it stops here.
    $completion = New-Object System.Collections.Generic.List[string]
    if (@($o.Without).Count -gt 0) { $completion.Add('anet init --without ' + ($o.Without -join ',')) }
    $completion.Add('anet init')
    if ($o.Controller -ne '') { $completion.Add('anet init --shell-controller ' + $o.Controller + ' --yes') }
    if ($o.Join -or $o.Controller -ne '') { $completion.Add('anet up') }
    if ($o.Join) { $completion.Add('anet hub-register ' + $o.Hub + ' --name ' + (ConvertTo-AnetArgument -Text $o.Name) + ' --passphrase-later') }
    if ($takeover.Held) {
      Write-AnetInstalledFailure -State $S -Message ('the data directory ' + $dataDir + ' still holds the data of an earlier anet (the lines above say why); the new anet does not start on it, so the installation stops here and nothing in that directory was changed') -Steps (@($takeover.Remaining) + $completion.ToArray())
      $global:LASTEXITCODE = $AnetExitRemaining
      return
    }

    # 11. -Without.
    if (@($o.Without).Count -gt 0) {
      $list = $o.Without -join ','
      Write-AnetLine ('-> anet init --without ' + $list)
      $r = @{}
      Invoke-AnetNative -FilePath $S.Exe -ArgumentList @('init', '--without', $list) -Result $r
      if ($r['Code'] -ne 0) {
        Write-AnetInstalledFailure -State $S -Message ('anet init --without ' + $list + ' refused (exit status ' + $r['Code'] + '); its reason is above') -Steps $completion.ToArray()
        $global:LASTEXITCODE = $r['Code']
        return
      }
      $completion.RemoveAt(0)
    }

    # 12. Defaults.
    Write-AnetLine ''
    Write-AnetLine '-> anet init (explicit safe defaults: inbound closed, no public capabilities, spending limits 0)'
    $r = @{}
    Invoke-AnetNative -FilePath $S.Exe -ArgumentList @('init') -Result $r
    if ($r['Code'] -ne 0) {
      Write-AnetInstalledFailure -State $S -Message ('anet init did not complete (exit status ' + $r['Code'] + ')') -Steps $completion.ToArray()
      $global:LASTEXITCODE = $r['Code']
      return
    }
    $completion.RemoveAt(0)

    # 13. Controller: -Controller on the command line is the consent, given
    # to anet init with --yes; nothing is asked.
    $stopped = $false
    if ($o.Controller -ne '') {
      $r = @{}
      Invoke-AnetNative -FilePath $S.Exe -ArgumentList @('status') -Capture -Result $r
      if ($r['Code'] -eq 0) {
        Write-AnetLine '-> stopping the running node to change its configuration'
        $r = @{}
        Invoke-AnetNative -FilePath $S.Exe -ArgumentList @('stop') -Result $r
        if ($r['Code'] -ne 0) {
          Write-AnetInstalledFailure -State $S -Message ('anet stop did not complete (exit status ' + $r['Code'] + ')') -Steps (@('anet stop') + $completion.ToArray())
          $global:LASTEXITCODE = $r['Code']
          return
        }
        $stopped = $true
      }
      Write-AnetLine ('-> anet init --shell-controller ' + $o.Controller + ' --yes')
      $r = @{}
      Invoke-AnetNative -FilePath $S.Exe -ArgumentList @('init', '--shell-controller', $o.Controller, '--yes') -Result $r
      if ($r['Code'] -ne 0) {
        Write-AnetInstalledFailure -State $S -Message ('anet init --shell-controller refused (exit status ' + $r['Code'] + '); its reason is above; the controller was not given access') -Steps @()
        if ($stopped) { Write-AnetLine 'The node was stopped to change its configuration; start it again with: anet up' }
        $global:LASTEXITCODE = $r['Code']
        return
      }
      $completion.RemoveAt(0)
    }

    # 14. Start.
    $started = $false
    if ($o.Join -or $o.Controller -ne '') {
      Write-AnetLine ''
      Write-AnetLine '-> Starting the node'
      $start = Start-AnetNode -Exe $S.Exe -Elevation $elevation
      if ($start.Deferred) {
        # Not started with the rights of this window: it starts at the next
        # logon through the Run value, or now by hand in a normal window.
        $autostart = (-not $o.NoAutostart)
        if ($autostart) { Set-AnetAutostart -Exe $S.Exe }
        $message = 'the node was not started with standard user rights (' + $start.Why + '), and it is not started with the administrator rights of this window.'
        if ($autostart) { $message += ' It starts at the next logon of ' + $elevation.Account + '.' }
        Write-AnetInstalledFailure -State $S -Message $message -Steps $completion.ToArray()
        $global:LASTEXITCODE = $AnetExitRemaining
        return
      }
      if ($start.Code -ne 0) {
        Write-AnetInstalledFailure -State $S -Message ('the node did not start (exit status ' + $start.Code + ')') -Steps $completion.ToArray()
        $global:LASTEXITCODE = $start.Code
        return
      }
      $started = $true
      $completion.RemoveAt(0)
      for ($i = 0; $i -lt 20; $i++) {
        $r = @{}
        Invoke-AnetNative -FilePath $S.Exe -ArgumentList @('status') -Capture -Result $r
        if ($r['Code'] -eq 0) { break }
        Start-Sleep -Seconds 1
      }
    }

    # 15. Autostart.
    # A Run value an earlier installation wrote is pointed at this one.
    if (-not $o.NoAutostart -and ($started -or (Get-AnetRunValueExe) -ne '')) { Set-AnetAutostart -Exe $S.Exe }

    # 16. Report.
    Write-AnetLine ''
    $r = @{}
    Invoke-AnetNative -FilePath $S.Exe -ArgumentList @('doctor') -Result $r
    Write-AnetLine ''
    Write-AnetLine 'Next:'
    if (-not $started) {
      Write-AnetLine '  anet up                      start the node in the background'
    }
    if (-not $o.Join) {
      Write-AnetLine '  anet hub-register https://hub.agentnetwork.org.cn --name NAME'
    }
    Write-AnetLine '  anet status                  this node''s identity (AID), data directory and console address'
    Write-AnetLine '  anet doctor                  check again later: signature, hub, what this node lets in'
    Write-AnetLine '  anet update                  update in place, checked against the release key'
    if ($o.Controller -ne '') {
      Write-AnetLine '  anet audit                   every command the controller ran, and every refusal, is on the evidence chain'
      Write-AnetLine '  anet identity protect        set a recovery passphrase for this node''s identity'
      Write-AnetGrant -DataDir $dataDir -Aid $o.Controller
    }
    if ($o.NoPath) {
      Write-AnetLine ''
      Write-AnetLine ('  ' + $o.Prefix + ' is not on your PATH (-NoPath); run anet as "' + $S.Exe + '"')
    }

    # 17. Join, last: its lines as they are, and its exit status.
    if ($o.Join) {
      Write-AnetLine ''
      Write-AnetLine ('-> anet hub-register ' + $o.Hub + ' --name ' + $o.Name)
      $r = @{}
      Invoke-AnetNative -FilePath $S.Exe -ArgumentList @('hub-register', $o.Hub, '--name', $o.Name, '--passphrase-later') -Result $r
      $global:LASTEXITCODE = $r['Code']
      return
    }
    $global:LASTEXITCODE = 0
  } catch {
    $e = $_.Exception
    $code = 1
    if ($e.Data['AnetFailure']) { $code = [int]$e.Data['AnetCode'] }
    if ($S.Installed) {
      Write-AnetInstalledFailure -State $S -Message $e.Message -Steps @()
    } else {
      Write-AnetLine ('Error: ' + $e.Message)
      Write-AnetLine 'Nothing was installed or changed.'
    }
    $global:LASTEXITCODE = $code
  } finally {
    Close-AnetGuard -State $S
    if ($S.Staged -ne '' -and (Test-Path -LiteralPath $S.Staged)) {
      Remove-Item -LiteralPath $S.Staged -Force -ErrorAction SilentlyContinue
    }
    if ($S.Tmp -ne '' -and (Test-Path -LiteralPath $S.Tmp)) {
      Remove-Item -LiteralPath $S.Tmp -Recurse -Force -ErrorAction SilentlyContinue
    }
    $ProgressPreference = $previousProgress
    $ErrorActionPreference = $previousEAP
  }
}

if ($env:ANET_INSTALL_PS1_TEST -ne '1') { Install-Anet @PSBoundParameters; if (Test-AnetOneShotHost) { exit $global:LASTEXITCODE } }
