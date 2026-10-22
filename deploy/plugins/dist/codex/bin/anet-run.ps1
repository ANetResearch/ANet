# The Windows runner of the anet plugin: the logic behind anet-run.cmd and
# anet-mcp.cmd, which select the anet executable and the identity exactly as
# anet-run.sh does on Linux and macOS.
#
# Settings. The MCP server receives them in ANET_PLUGIN_BIN and
# ANET_PLUGIN_IDENTITY, with ANET_PLUGIN_SETTINGS=env. In any other context the
# script reads the same two variables from its own environment, and nothing
# else.
#
# The script is started by the two .cmd files with Windows PowerShell 5.1
# (powershell.exe of the system directory, never one found on PATH or in the
# working directory). It reads the file of this script as text and runs it as a
# script block, so that an execution policy for script files does not apply.
# It receives no argument: every input is an environment variable that the
# .cmd file set, so that no value passes through a command line of cmd.exe.
#
#   ANET_RUN_MODE      locate: print the selection as two lines for the .cmd
#                      file ("E|<executable>", and "I|<identity>" when one is
#                      selected) and exit 0, or print the reason on stderr and
#                      exit 1.
#                      resolve: print the executable, its version, the
#                      identity and the runner command, and exit 0.
#   ANET_RUN_SELF      the full path of anet-run.cmd, for the runner line.
#   ANET_RUN_INSTALL   missing: set by anet-mcp.cmd (the MCP launcher) and
#                      by no other caller. In the mode locate, when no
#                      anetPath is set and no anet.exe is found, anet is
#                      installed first (Install-AnetForMcp, below).
#
# Rules (the same as anet-run.sh):
#   * A configured anetPath is used as it is and never replaced by another
#     copy. It must be an absolute path of an existing .exe file; a relative
#     path would depend on the working directory of the session, which can be
#     an untrusted workspace.
#   * Without it, the directories of PATH that are absolute paths are searched
#     for anet.exe, then the install locations of the official installer
#     (install.ps1): %LOCALAPPDATA%\Programs\anet, %LOCALAPPDATA%\anet\bin and
#     %USERPROFILE%\.anet-bin. The working directory is never searched.
#   * An identity name must be a valid anet identity name (letters, digits,
#     dot, underscore and hyphen, at most 64 characters, starting with a letter
#     or a digit). Anything else is refused, so that the .cmd file only ever
#     passes a plain token to anet.

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 2.0

$AnetOnWindows = ([Environment]::OSVersion.Platform -eq [PlatformID]::Win32NT)
if ($AnetOnWindows) { $AnetExeName = 'anet.exe' } else { $AnetExeName = 'anet' }

function Get-AnetEnv([string]$Name) {
  $v = [Environment]::GetEnvironmentVariable($Name)
  if ($null -eq $v) { return '' }
  return [string]$v
}

# A value with a line break is not a valid path or name.
function Get-AnetSingleLine($Value) {
  if ($null -eq $Value -or -not ($Value -is [string])) { return '' }
  if ($Value.Contains("`n") -or $Value.Contains("`r")) { return '' }
  return $Value
}

function Get-AnetUserHome {
  $h = Get-AnetEnv 'USERPROFILE'
  if ($h -eq '') { $h = Get-AnetEnv 'HOME' }
  return $h
}

# Helpers for a settings store of the host in JSON. Only the two settings of
# this plugin are taken; a store that cannot be read selects nothing.
function Read-AnetJsonFile([string]$Path) {
  if ($Path -eq '' -or -not (Test-Path -LiteralPath $Path -PathType Leaf)) { return $null }
  try { return ([IO.File]::ReadAllText($Path) | ConvertFrom-Json) } catch { return $null }
}

function Get-AnetMember($Object, [string]$Name) {
  if ($null -eq $Object -or -not ($Object -is [Management.Automation.PSCustomObject])) { return $null }
  $p = $Object.PSObject.Properties[$Name]
  if ($null -eq $p) { return $null }
  return $p.Value
}

# The values of the members "anet@<marketplace>" of a map, sorted by name.
function Get-AnetOwnEntries($Map) {
  $out = @()
  if ($null -eq $Map -or -not ($Map -is [Management.Automation.PSCustomObject])) { return $out }
  foreach ($p in @($Map.PSObject.Properties | Sort-Object Name)) {
    if ($p.Name.StartsWith('anet@') -and $p.Value -is [Management.Automation.PSCustomObject]) { $out += ,$p.Value }
  }
  return $out
}

# anetPath and identity of an options object; anything but a string is empty.
function Get-AnetOptionValues($Options) {
  $out = @()
  foreach ($key in @('anetPath', 'identity')) {
    $out += (Get-AnetSingleLine (Get-AnetMember $Options $key))
  }
  return $out
}

# Read-AnetSettings returns anetPath and identity from the environment of the
# caller. A host without a plugin settings store passes the two values as
# ANET_PLUGIN_BIN and ANET_PLUGIN_IDENTITY.
function Read-AnetSettings {
  return @((Get-AnetEnv 'ANET_PLUGIN_BIN'), (Get-AnetEnv 'ANET_PLUGIN_IDENTITY'))
}

function Test-AnetAbsolutePath([string]$Path) {
  if ($AnetOnWindows) {
    # A drive path (C:\...) or a UNC path (\\server\share\...); not a
    # drive-relative path (C:anet.exe), a rooted path without a drive (\anet.exe)
    # or a device path (\\.\ or \\?\).
    return ($Path -match '^[A-Za-z]:[\\/]' -or $Path -match '^\\\\[^\\/?.][^\\/]*[\\/]')
  }
  return $Path.StartsWith('/')
}

function Test-AnetExecutable([string]$Path) {
  if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) { return $false }
  if ($AnetOnWindows) { return $Path.EndsWith('.exe', [StringComparison]::OrdinalIgnoreCase) }
  return $true
}

$AnetIdentityPattern = '^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$'

if ((Get-AnetEnv 'ANET_PLUGIN_SETTINGS') -eq 'env') {
  $AnetPath = Get-AnetSingleLine (Get-AnetEnv 'ANET_PLUGIN_BIN')
  $AnetIdentity = Get-AnetSingleLine (Get-AnetEnv 'ANET_PLUGIN_IDENTITY')
} else {
  $settings = @(Read-AnetSettings)
  $AnetPath = ''
  $AnetIdentity = ''
  if ($settings.Count -ge 1) { $AnetPath = Get-AnetSingleLine $settings[0] }
  if ($settings.Count -ge 2) { $AnetIdentity = Get-AnetSingleLine $settings[1] }
}

$AnetSearched = 'PATH, %LOCALAPPDATA%\Programs\anet, %LOCALAPPDATA%\anet\bin and %USERPROFILE%\.anet-bin'

# Find-AnetExecutable returns the path of the executable, or throws the reason.
function Find-AnetExecutable {
  if ($AnetPath -ne '') {
    if (-not (Test-AnetAbsolutePath $AnetPath)) {
      throw ('The plugin setting anetPath names ' + $AnetPath + ', which is not an absolute path. Correct or clear the setting.')
    }
    if (Test-AnetExecutable $AnetPath) { return $AnetPath }
    throw ('The plugin setting anetPath names ' + $AnetPath + ', which is not an executable file (' + $AnetExeName + '). Correct or clear the setting.')
  }
  if ($AnetOnWindows) { $sep = ';' } else { $sep = ':' }
  foreach ($dir in ((Get-AnetEnv 'PATH') -split $sep)) {
    $dir = $dir.Trim().Trim('"')
    if ($dir -eq '' -or -not (Test-AnetAbsolutePath $dir)) { continue }
    $candidate = Join-Path $dir $AnetExeName
    if (Test-AnetExecutable $candidate) { return $candidate }
  }
  $candidates = @()
  $local = Get-AnetEnv 'LOCALAPPDATA'
  if ($local -ne '' -and (Test-AnetAbsolutePath $local)) {
    $candidates += (Join-Path (Join-Path (Join-Path $local 'Programs') 'anet') $AnetExeName)
    $candidates += (Join-Path (Join-Path (Join-Path $local 'anet') 'bin') $AnetExeName)
  }
  $userHome = Get-AnetUserHome
  if ($userHome -ne '' -and (Test-AnetAbsolutePath $userHome)) {
    $candidates += (Join-Path (Join-Path $userHome '.anet-bin') $AnetExeName)
  }
  foreach ($candidate in $candidates) {
    if (Test-AnetExecutable $candidate) { return $candidate }
  }
  throw ('anet is not installed (searched ' + $AnetSearched + ').' + [Environment]::NewLine +
    'Ask the agent to run the anet-setup skill of the anet plugin, or see https://docs.agentnetwork.org.cn/docs/en/latest/intro/install.html')
}

# Install-AnetForMcp installs anet for the MCP launcher when it is missing,
# so that enabling the plugin is enough to have anet installed and its node
# running. It runs the official Windows installer exactly as the installation
# guide gives it, with Windows PowerShell of the system directory, which is
# asked of Windows ([Environment]::GetFolderPath('System')), so that no
# environment variable can move it (off Windows, where PowerShell 7 runs the
# tests of this script, SystemRoot stands in for it):
#
#   powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command
#     "[Net.ServicePointManager]::SecurityProtocol=3072; & ([scriptblock]::Create((irm -UseBasicParsing https://agentnetwork.org.cn/install.ps1)))"
#
# install.ps1 checks the signed release manifest and the SHA-256 of the
# binary, stops without changing anything when a check fails, installs anet
# for this user, starts the node and joins the public hub. It receives no
# argument and nothing from the workspace or the session: no input (its input
# is closed at once), no window, the user's home directory as its working
# directory. Its output and its errors are copied to standard error, line by
# line, after it ends: standard output is read by anet-run.cmd and is then the
# MCP session. Nothing is installed when ANET_PLUGIN_NO_INSTALL is set to a
# value other than 0.
#
# One installation at a time: the file anet-plugin\install.lock in
# %LOCALAPPDATA% is held open without sharing while the installer runs, so
# that a second host that starts at the same moment waits for it (at most 10
# minutes) and then installs nothing when an anet is there. Windows releases
# the file when the process ends, however it ends. The file also names the
# installer while it runs (its process ID and start time), and is emptied
# when it ends: when a host ends this script but not the installer (which is
# a process of its own), the next one still waits for that installer rather
# than start a second one. The installer is waited for at most 15 minutes;
# it is not ended when that passes, and it stays named in the file.
function Test-AnetInstallOptOut {
  $v = Get-AnetEnv 'ANET_PLUGIN_NO_INSTALL'
  return ($v -ne '' -and $v -ne '0')
}

function Find-AnetQuietly {
  try { return (Find-AnetExecutable) } catch { return $null }
}

function Write-AnetErrorLines([string]$Text) {
  foreach ($line in ($Text -split "`r?`n")) {
    if ($line.Trim() -ne '') { [Console]::Error.WriteLine($line) }
  }
}

# The system directory (System32): asked of Windows, never of the
# environment. Off Windows there is none; the tests of this script run it
# with PowerShell 7 and a SystemRoot of their own.
function Get-AnetSystemDirectory {
  if ($AnetOnWindows) { return [Environment]::GetFolderPath([Environment+SpecialFolder]::System) }
  $root = Get-AnetEnv 'SystemRoot'
  if ($root -eq '' -or -not (Test-AnetAbsolutePath $root)) { return '' }
  return (Join-Path $root 'System32')
}

# Whether the installer that the lock file names ("<process ID> <start time
# in UTC ticks>") still runs: a process with that ID that started then (within
# a second: off Windows, .NET derives the start time from the boot time,
# which varies slightly between two readings).
function Test-AnetLockHolderRuns($Stream) {
  try {
    $Stream.Position = 0
    $buf = New-Object byte[] 64
    $n = $Stream.Read($buf, 0, $buf.Length)
    $text = [Text.Encoding]::ASCII.GetString($buf, 0, $n).Trim()
    if ($text -notmatch '^([0-9]+) ([0-9]+)$') { return $false }
    $ticks = [long]$Matches[2]
    $proc = [Diagnostics.Process]::GetProcessById([int]$Matches[1])
    try { return ((-not $proc.HasExited) -and [Math]::Abs($proc.StartTime.ToUniversalTime().Ticks - $ticks) -le 10000000) } finally { $proc.Dispose() }
  } catch {
    return $false
  }
}

function Set-AnetLockHolder($Stream, [string]$Text) {
  $Stream.SetLength(0)
  $b = [Text.Encoding]::ASCII.GetBytes($Text)
  $Stream.Write($b, 0, $b.Length)
  $Stream.Flush()
}

function Install-AnetForMcp {
  $sys = Get-AnetSystemDirectory
  if ($sys -eq '' -or -not (Test-AnetAbsolutePath $sys)) { return }
  $ps = Join-Path (Join-Path (Join-Path $sys 'WindowsPowerShell') 'v1.0') 'powershell.exe'
  if (-not (Test-Path -LiteralPath $ps -PathType Leaf)) { return }
  $local = Get-AnetEnv 'LOCALAPPDATA'
  if ($local -eq '' -or -not (Test-AnetAbsolutePath $local)) { return }
  $userHome = Get-AnetUserHome
  if ($userHome -eq '' -or -not (Test-AnetAbsolutePath $userHome)) { $userHome = $local }
  $state = Join-Path $local 'anet-plugin'
  $lock = $null
  try {
    $null = [IO.Directory]::CreateDirectory($state)
    $lockPath = Join-Path $state 'install.lock'
    $deadline = [DateTime]::UtcNow.AddMinutes(10)
    $waited = $false
    while ($null -eq $lock) {
      $busy = $false
      try {
        $lock = [IO.File]::Open($lockPath, [IO.FileMode]::OpenOrCreate, [IO.FileAccess]::ReadWrite, [IO.FileShare]::None)
        if (Test-AnetLockHolderRuns $lock) {
          # The installer of a host that ended without it still runs.
          $lock.Dispose()
          $lock = $null
          $busy = $true
        }
      } catch [IO.IOException] {
        $busy = $true
      }
      if ($busy) {
        if ([DateTime]::UtcNow -ge $deadline) {
          [Console]::Error.WriteLine('Another installation of anet is still running; anet was not installed by this session.')
          return
        }
        if (-not $waited) { [Console]::Error.WriteLine('Another installation of anet is running; waiting for it to finish.') }
        $waited = $true
        Start-Sleep -Seconds 1
      }
    }
    if ($waited -and $null -ne (Find-AnetQuietly)) {
      [Console]::Error.WriteLine('Another installation of anet has finished.')
      return
    }
    [Console]::Error.WriteLine('anet is not installed. Installing it with the official installer (https://agentnetwork.org.cn/install.ps1),')
    [Console]::Error.WriteLine('which checks the signed release manifest, installs anet for this user, starts its node and joins the public hub.')
    [Console]::Error.WriteLine('Set the environment variable ANET_PLUGIN_NO_INSTALL=1 to turn this off.')
    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName = $ps
    $psi.Arguments = '-NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command "[Net.ServicePointManager]::SecurityProtocol=3072; & ([scriptblock]::Create((irm -UseBasicParsing https://agentnetwork.org.cn/install.ps1)))"'
    $psi.UseShellExecute = $false
    $psi.RedirectStandardInput = $true
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    $psi.CreateNoWindow = $true
    $psi.WorkingDirectory = $userHome
    $p = New-Object System.Diagnostics.Process
    $p.StartInfo = $psi
    try {
      $null = $p.Start()
    } catch {
      [Console]::Error.WriteLine('The anet installer could not be started: ' + $_.Exception.Message)
      return
    }
    try {
      try { Set-AnetLockHolder $lock ([string]$p.Id + ' ' + [string]$p.StartTime.ToUniversalTime().Ticks) } catch { }
      $p.StandardInput.Close()
      $out = $p.StandardOutput.ReadToEndAsync()
      $err = $p.StandardError.ReadToEndAsync()
      if (-not $p.WaitForExit(15 * 60 * 1000)) {
        [Console]::Error.WriteLine('The installation of anet has not finished; it goes on in the background.')
        return
      }
      $p.WaitForExit()
      try { Set-AnetLockHolder $lock '' } catch { }
      # The node that the installer started may hold the other end of the
      # pipes; what arrived within a few seconds is shown.
      if ($out.Wait(5000)) { Write-AnetErrorLines ([string]$out.Result) }
      if ($err.Wait(1000)) { Write-AnetErrorLines ([string]$err.Result) }
      [Console]::Error.WriteLine('The anet installer ended with status ' + [string]$p.ExitCode + '.')
    } finally {
      $p.Dispose()
    }
  } catch {
    [Console]::Error.WriteLine('anet could not be installed: ' + $_.Exception.Message)
  } finally {
    if ($null -ne $lock) { $lock.Dispose() }
  }
}

# The .cmd file reads the selection through the code page of the console. A
# path with a character that this code page cannot represent is replaced by its
# short (8.3) form; when that does not help either, the path is refused rather
# than passed on altered.
function ConvertTo-AnetConsolePath([string]$Path) {
  if (-not $AnetOnWindows) { return $Path }
  $enc = [Console]::OutputEncoding
  if ($enc.GetString($enc.GetBytes($Path)) -ceq $Path) { return $Path }
  $short = ''
  try { $short = [string](New-Object -ComObject Scripting.FileSystemObject).GetFile($Path).ShortPath } catch { $short = '' }
  if ($short -ne '' -and $enc.GetString($enc.GetBytes($short)) -ceq $short) { return $short }
  throw ('The path ' + $Path + ' contains characters that the code page of this console cannot represent. Set the plugin setting anetPath to a path without them.')
}

function Test-AnetIdentity {
  if ($AnetIdentity -ne '' -and -not ($AnetIdentity -cmatch $AnetIdentityPattern)) {
    throw ('The plugin setting identity names ' + $AnetIdentity + ', which is not a valid anet identity name. Correct or clear the setting.')
  }
}

# The runner line is a command: a path with a character that a shell would
# split or interpret is quoted. A Windows path cannot contain a double quote.
function Format-AnetRunner([string]$Path) {
  if ($Path -match '^[A-Za-z0-9_.:\\/-]+$') { return $Path }
  return '"' + $Path + '"'
}

$mode = Get-AnetEnv 'ANET_RUN_MODE'
if ($mode -eq 'resolve') {
  $exe = $null
  try { $exe = Find-AnetExecutable } catch { $exe = $null }
  if ($null -ne $exe) {
    [Console]::Out.WriteLine('executable: ' + $exe)
    $version = ''
    # A message of anet on stderr is not an error of this script.
    $ErrorActionPreference = 'Continue'
    try {
      $version = [string](& $exe --version 2>$null | Select-Object -First 1)
    } catch {
      $version = ''
    }
    [Console]::Out.WriteLine('version: ' + $version)
  } elseif ($AnetPath -ne '') {
    [Console]::Out.WriteLine('executable: invalid setting anetPath=' + $AnetPath)
  } else {
    [Console]::Out.WriteLine('executable: not found')
  }
  $shown = $AnetIdentity
  if ($shown -eq '') { $shown = 'default' }
  elseif (-not ($AnetIdentity -cmatch $AnetIdentityPattern)) { $shown = 'invalid setting identity=' + $AnetIdentity }
  [Console]::Out.WriteLine('identity: ' + $shown)
  [Console]::Out.WriteLine('runner: ' + (Format-AnetRunner (Get-AnetEnv 'ANET_RUN_SELF')))
  exit 0
}
if ($mode -eq 'locate') {
  if ((Get-AnetEnv 'ANET_RUN_INSTALL') -eq 'missing' -and (Get-AnetEnv 'ANET_PLUGIN_SETTINGS') -eq 'env' -and
      $AnetPath -eq '' -and -not (Test-AnetInstallOptOut) -and $null -eq (Find-AnetQuietly)) {
    Install-AnetForMcp
  }
  try {
    $exe = ConvertTo-AnetConsolePath (Find-AnetExecutable)
    Test-AnetIdentity
  } catch {
    [Console]::Error.WriteLine($_.Exception.Message)
    exit 1
  }
  [Console]::Out.WriteLine('E|' + $exe)
  if ($AnetIdentity -ne '') { [Console]::Out.WriteLine('I|' + $AnetIdentity) }
  exit 0
}
[Console]::Error.WriteLine('anet-run.ps1 is started by anet-run.cmd and anet-mcp.cmd; run anet-run.cmd instead.')
exit 1
