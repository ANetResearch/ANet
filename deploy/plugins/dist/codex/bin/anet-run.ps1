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
