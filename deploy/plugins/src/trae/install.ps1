# Installs the anet bundle for Trae and Trae CN on Windows (on Linux and
# macOS: install.sh).
#
# Trae has no plugin package format. This script copies the parts of the
# bundle into a configuration directory of Trae (the skills, the commands and
# the launcher of the MCP server) and prints the MCP configuration that Trae
# needs. It starts no process, opens no connection, and reads no file of Trae.
# The only file of Trae that it creates is a project mcp.json that does not
# exist yet; it never changes an existing one.
#
# install.cmd of this bundle starts this script with Windows PowerShell 5.1.
# Run "install.cmd -Help" for the options.
param(
  [string]$Project = '',
  [switch]$Cn,
  [string]$Identity = '',
  [switch]$ClearIdentity,
  [string]$Anet = '',
  [switch]$ClearAnet,
  [switch]$Remove,
  [switch]$Help
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 2.0

$Server = '{{SERVER_NAME}}'
$RunTimeoutMs = '{{MCP_TIMEOUT_MS}}'

$Usage = @'
Usage: install.cmd [-Project DIR] [-Cn] [-Identity NAME | -ClearIdentity]
                   [-Anet PATH | -ClearAnet] [-Remove]

Installs the anet bundle for Trae into the user directory %USERPROFILE%\.trae
(with -Cn, for Trae CN: %USERPROFILE%\.trae-cn) or, with -Project, into
DIR\.trae.

  -Project DIR     install into the project DIR (project scope)
  -Cn              use the user directory .trae-cn of Trae CN
  -Identity NAME   the named anet identity (anet id ls) that the plugin uses
  -ClearIdentity   select the default identity again
  -Anet PATH       absolute path of anet.exe
  -ClearAnet       search PATH and the install locations of the anet
                   installer again
  -Remove          remove the files that this script installed
  -Help            show this text

Without -Identity, -ClearIdentity, -Anet and -ClearAnet, the settings of an
earlier installation are kept.
'@

function Stop-Install([string]$Message) {
  [Console]::Error.WriteLine('install.ps1: ' + $Message)
  exit 1
}

if ($Help) {
  [Console]::Out.WriteLine($Usage)
  exit 0
}

$OnWindows = ([Environment]::OSVersion.Platform -eq [PlatformID]::Win32NT)
if (-not $OnWindows) { Stop-Install 'this script installs the bundle on Windows; on Linux and macOS, run: sh install.sh' }

$Here = Split-Path -Parent $MyInvocation.MyCommand.Path
$SetId = $PSBoundParameters.ContainsKey('Identity') -or $ClearIdentity
$SetBin = $PSBoundParameters.ContainsKey('Anet') -or $ClearAnet
if ($PSBoundParameters.ContainsKey('Identity') -and $ClearIdentity) { Stop-Install '-Identity and -ClearIdentity exclude each other' }
if ($PSBoundParameters.ContainsKey('Anet') -and $ClearAnet) { Stop-Install '-Anet and -ClearAnet exclude each other' }

# Scope and target directory.
if ($Project -ne '') {
  if ($Cn) { Stop-Install '-Cn selects the user directory of Trae CN and cannot be combined with -Project' }
  if (-not (Test-Path -LiteralPath $Project -PathType Container)) { Stop-Install ('the project directory does not exist: ' + $Project) }
  $Project = (Resolve-Path -LiteralPath $Project).ProviderPath
  $Root = Join-Path $Project '.trae'
} else {
  $UserHome = [Environment]::GetEnvironmentVariable('USERPROFILE')
  if ([string]::IsNullOrEmpty($UserHome) -or -not [IO.Path]::IsPathRooted($UserHome)) { Stop-Install 'USERPROFILE is not set to an absolute path' }
  if ($Cn) { $Root = Join-Path $UserHome '.trae-cn' } else { $Root = Join-Path $UserHome '.trae' }
}

$Files = @(
  'skills/anet/SKILL.md', 'skills/anet-setup/SKILL.md', 'commands/anet.md', 'commands/anet-status.md',
  'anet/bin/anet-run.sh', 'anet/bin/anet-mcp.sh', 'anet/bin/anet-mcp', 'anet/bin/anet-install.sh',
  'anet/bin/anet-run.cmd', 'anet/bin/anet-run.ps1', 'anet/bin/anet-mcp.cmd', 'anet/bin/anet-install.cmd'
)
# The text that marks a file of this bundle: a file of the same name without it is neither replaced nor removed.
$Owners = @{
  'skills/anet/SKILL.md' = 'name: anet'; 'skills/anet-setup/SKILL.md' = 'name: anet-setup'
  'commands/anet.md' = 'AgentNetwork (anet) node'; 'commands/anet-status.md' = 'local anet node'
  'anet/bin/anet-run.sh' = 'Runs the anet executable'; 'anet/bin/anet-mcp.sh' = 'Starts the MCP server of the local anet node'
  'anet/bin/anet-mcp' = 'Starts the MCP server of the local anet node'; 'anet/bin/anet-install.sh' = 'Installs the anet command line'
  'anet/bin/anet-run.cmd' = 'Runs the anet executable'; 'anet/bin/anet-run.ps1' = 'The Windows runner of the anet plugin'
  'anet/bin/anet-mcp.cmd' = 'Starts the MCP server of the local anet node'; 'anet/bin/anet-install.cmd' = 'Installs the anet command line'
}

function Get-Target([string]$Rel) {
  return Join-Path $Root ($Rel -replace '/', [IO.Path]::DirectorySeparatorChar)
}

# Nothing is written or removed through a symbolic link or a junction: a
# project can hold one under .trae, and it could lead out of the directory.
function Assert-NoLink([string]$Rel) {
  $path = $Root
  $parts = @()
  if ($Rel -ne '') { $parts = $Rel -split '/' }
  for ($i = -1; $i -lt $parts.Count; $i++) {
    if ($i -ge 0) { $path = Join-Path $path $parts[$i] }
    if ($i -lt 0 -and $Project -eq '') { continue }
    $item = Get-Item -LiteralPath $path -Force -ErrorAction SilentlyContinue
    if ($null -ne $item -and ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) {
      Stop-Install ($path + ' is a symbolic link or a junction; this script does not write or remove through links')
    }
  }
}
Assert-NoLink ''
foreach ($f in $Files + @('anet/settings')) { Assert-NoLink $f }
if ($Project -ne '') { Assert-NoLink 'mcp.json' }

foreach ($f in $Files) {
  $t = Get-Target $f
  if (Test-Path -LiteralPath $t -PathType Leaf) {
    if (-not ([IO.File]::ReadAllText($t).Contains($Owners[$f]))) {
      Stop-Install ($t + ' exists and is not part of the anet bundle; move it away first')
    }
  }
}

if ($Remove) {
  if ($SetId -or $SetBin) { Stop-Install '-Remove takes no settings' }
  foreach ($f in $Files + @('anet/settings')) {
    $t = Get-Target $f
    if (Test-Path -LiteralPath $t -PathType Leaf) { Remove-Item -LiteralPath $t -Force }
  }
  # Only an empty directory is removed; a directory that holds other files stays.
  foreach ($d in @('skills/anet', 'skills/anet-setup', 'anet/bin', 'anet', 'commands', 'skills', '')) {
    $t = $Root
    if ($d -ne '') { $t = Get-Target $d }
    if ((Test-Path -LiteralPath $t -PathType Container) -and -not (Get-ChildItem -LiteralPath $t -Force | Select-Object -First 1)) {
      Remove-Item -LiteralPath $t -Force
    }
  }
  [Console]::Out.WriteLine('Removed the anet files from ' + $Root + '.')
  $line = 'Delete the MCP server "' + $Server + '" in Trae (Settings, MCP)'
  if ($Project -ne '') { $line += ' or remove it from ' + (Join-Path $Root 'mcp.json') }
  [Console]::Out.WriteLine($line + ', and start a new session.')
  exit 0
}

foreach ($f in $Files) {
  if (-not (Test-Path -LiteralPath (Join-Path $Here ($f -replace '/', [IO.Path]::DirectorySeparatorChar)) -PathType Leaf)) {
    Stop-Install ('the bundle is incomplete: ' + $f + ' is missing next to install.ps1')
  }
}

# Settings: an option that is not given keeps the value of the earlier installation.
$Settings = Get-Target 'anet/settings'
$AnetPath = $Anet
if ($ClearAnet) { $AnetPath = '' }
if ($ClearIdentity) { $Identity = '' }
if (Test-Path -LiteralPath $Settings -PathType Leaf) {
  $oldBin = $null
  $oldId = $null
  foreach ($line in [IO.File]::ReadAllLines($Settings)) {
    $line = $line.TrimEnd("`r")
    if ($null -eq $oldBin -and $line.StartsWith('anetPath=')) { $oldBin = $line.Substring(9) }
    elseif ($null -eq $oldId -and $line.StartsWith('identity=')) { $oldId = $line.Substring(9) }
  }
  if (-not $SetBin -and $null -ne $oldBin) { $AnetPath = $oldBin }
  if (-not $SetId -and $null -ne $oldId) { $Identity = $oldId }
}
if ($Identity -ne '' -and -not ($Identity -cmatch '^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$')) {
  Stop-Install ('the identity name may contain only letters, digits, dot, underscore and hyphen: ' + $Identity)
}
if ($AnetPath -ne '') {
  if (-not ($AnetPath -match '^[A-Za-z]:\\' -or $AnetPath -match '^\\\\[^\\?.]')) { Stop-Install ('-Anet needs an absolute path: ' + $AnetPath) }
  if ($AnetPath -match '[\x00-\x1f"]' -or -not $AnetPath.EndsWith('.exe', [StringComparison]::OrdinalIgnoreCase)) {
    Stop-Install ('-Anet needs the path of anet.exe, without quotes or control characters: ' + $AnetPath)
  }
}

# The command of the MCP entry. Trae documents that it must not contain a
# space; a path with one is replaced by its short (8.3) form when the volume
# provides one.
$Launcher = Get-Target 'anet/bin/anet-mcp.cmd'
function Get-CommandPath {
  if ($Launcher -notmatch '\s') { return $Launcher }
  $short = ''
  try { $short = [string](New-Object -ComObject Scripting.FileSystemObject).GetFile($Launcher).ShortPath } catch { $short = '' }
  if ($short -ne '' -and $short -notmatch '\s') { return $short }
  Stop-Install ('the path ' + $Launcher + ' contains a space and has no short form without one; Trae rejects an MCP command with spaces. Use -Project with a directory whose path has no space.')
}

foreach ($f in $Files) {
  $t = Get-Target $f
  $dir = Split-Path -Parent $t
  if (-not (Test-Path -LiteralPath $dir -PathType Container)) { [void](New-Item -ItemType Directory -Path $dir -Force) }
  Copy-Item -LiteralPath (Join-Path $Here ($f -replace '/', [IO.Path]::DirectorySeparatorChar)) -Destination $t -Force
}
if ($AnetPath -ne '' -or $Identity -ne '' -or (Test-Path -LiteralPath $Settings)) {
  [IO.File]::WriteAllText($Settings, 'anetPath=' + $AnetPath + "`n" + 'identity=' + $Identity + "`n", (New-Object Text.UTF8Encoding($false)))
}
$Command = Get-CommandPath

# The MCP configuration.
function ConvertTo-JsonString([string]$Value) {
  return '"' + ($Value -replace '\\', '\\' -replace '"', '\"') + '"'
}
$envPairs = @(, @('RUN_MCP_TIMEOUT_MS', $RunTimeoutMs))
if ($AnetPath -ne '') { $envPairs += , @('ANET_PLUGIN_BIN', $AnetPath) }
if ($Identity -ne '') { $envPairs += , @('ANET_PLUGIN_IDENTITY', $Identity) }
$envPretty = ($envPairs | ForEach-Object { '        ' + (ConvertTo-JsonString $_[0]) + ': ' + (ConvertTo-JsonString $_[1]) }) -join (",`n")
$envCompact = ($envPairs | ForEach-Object { (ConvertTo-JsonString $_[0]) + ':' + (ConvertTo-JsonString $_[1]) }) -join ','
$Pretty = "{`n  `"mcpServers`": {`n    " + (ConvertTo-JsonString $Server) + ": {`n      `"command`": " + (ConvertTo-JsonString $Command) +
  ",`n      `"args`": [],`n      `"env`": {`n" + $envPretty + "`n      }`n    }`n  }`n}"

[Console]::Out.WriteLine('Installed the anet bundle into ' + $Root + ':')
foreach ($f in $Files) { [Console]::Out.WriteLine('  ' + $f) }
if (Test-Path -LiteralPath $Settings) {
  $shownBin = $AnetPath
  if ($shownBin -eq '') { $shownBin = '<search>' }
  $shownId = $Identity
  if ($shownId -eq '') { $shownId = '<default>' }
  [Console]::Out.WriteLine('  anet/settings (anetPath=' + $shownBin + ', identity=' + $shownId + ')')
}
[Console]::Out.WriteLine('')

if ($Project -ne '') {
  $mcpFile = Join-Path $Root 'mcp.json'
  if (Test-Path -LiteralPath $mcpFile) {
    [Console]::Out.WriteLine($mcpFile + ' exists and was not changed. Add the member "' + $Server + '" of this object to its "mcpServers":')
    [Console]::Out.WriteLine('')
    [Console]::Out.WriteLine($Pretty)
  } else {
    [IO.File]::WriteAllText($mcpFile, $Pretty + "`n", (New-Object Text.UTF8Encoding($false)))
    [Console]::Out.WriteLine('Wrote ' + $mcpFile + ':')
    [Console]::Out.WriteLine('')
    [Console]::Out.WriteLine($Pretty)
  }
  [Console]::Out.WriteLine('')
  [Console]::Out.WriteLine('In Trae, open the project, turn on "Project MCP" in Settings, MCP, and confirm the prompt.')
} else {
  [Console]::Out.WriteLine('Register the MCP server in Trae. Open Settings, MCP, Add, Create Manually (or Raw Config) and enter:')
  [Console]::Out.WriteLine('')
  [Console]::Out.WriteLine($Pretty)
  [Console]::Out.WriteLine('')
  if (-not $Cn) {
    $compact = '{"command":' + (ConvertTo-JsonString $Command) + ',"args":[],"env":{' + $envCompact + '}}'
    $encoded = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($compact)) -replace '\+', '%2B' -replace '/', '%2F' -replace '=', '%3D'
    [Console]::Out.WriteLine('Alternatively open this link in a browser and confirm the prompts of Trae:')
    [Console]::Out.WriteLine('')
    [Console]::Out.WriteLine('  trae://trae.ai-ide/mcp-import?type=stdio&name=' + $Server + '&config=' + $encoded)
    [Console]::Out.WriteLine('')
  }
}
[Console]::Out.WriteLine('Then start a new session in Trae and ask: Set up anet.')
exit 0
