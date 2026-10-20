# deploy/release/test-install-e2e.ps1 - the end-to-end check of install.ps1
# in the windows-install job of CI (.github/workflows/ci.yml). It is not
# part of a release and is not published.
#
#   -Plant -LegacyExe PATH   before the first installation: an earlier anet
#                            as the npm package @agentnetwork/anet 1.1.15
#                            would leave it (the package in the global npm
#                            prefix, its anet, anet.cmd and anet.ps1 shims,
#                            and the binary of its platform package, PATH:
#                            a stand-in built from testdata/legacy-anet),
#                            and its data in %USERPROFILE%\.anet.
#   -Verify -Aid AID         after each installation: what -Controller
#                            grants, the Run value, the PATH, a private
#                            install directory, and anet version.
#   -Takeover                with -Verify: the earlier anet was stopped with
#                            its own stop command, its npm package removed,
#                            and its data moved aside.
#   -Cleanup                 stop the node this job started.
#
# It runs in Windows PowerShell 5.1 and in PowerShell 7. It contacts no
# host: the installation reads the release from a local directory and does
# not join a hub (-NoJoin). The exit status is the number of failed checks.

param([switch]$Plant, [string]$LegacyExe, [switch]$Verify, [string]$Aid, [switch]$Takeover, [switch]$Cleanup)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 3
$script:Failures = 0

function Check {
  param([string]$What, [bool]$Ok, [string]$Detail = '')
  if ($Ok) {
    Write-Host ('ok    ' + $What)
  } else {
    $script:Failures++
    Write-Host ('FAIL  ' + $What + $(if ($Detail -ne '') { ': ' + $Detail } else { '' }))
  }
}

function Get-NpmPrefix {
  $p = @(& npm.cmd prefix -g)
  if ($LASTEXITCODE -ne 0 -or $p.Count -eq 0) { throw 'npm prefix -g failed' }
  return ([string]$p[0]).Trim()
}

$profileDir = [Environment]::GetFolderPath('UserProfile')
$dataDir = Join-Path $profileDir '.anet'

if ($Plant) {
  if ([string]::IsNullOrEmpty($LegacyExe) -or -not (Test-Path -LiteralPath $LegacyExe -PathType Leaf)) { throw '-Plant needs -LegacyExe' }
  if (Test-Path -LiteralPath $dataDir) { throw ($dataDir + ' exists already; the plant expects a fresh account') }
  # The npm package, packed and installed globally as npm installs it.
  $src = Join-Path ([IO.Path]::GetTempPath()) ('legacy-npm-' + [Guid]::NewGuid().ToString('N'))
  $null = New-Item -ItemType Directory -Path (Join-Path $src 'bin')
  [IO.File]::WriteAllText((Join-Path $src 'package.json'), '{"name": "@agentnetwork/anet", "version": "1.1.15", "bin": {"anet": "bin/anet"}, "license": "MIT", "repository": {"type": "git", "url": "git+https://github.com/ChatChatTech/AgentNetwork.git"}}')
  [IO.File]::WriteAllText((Join-Path $src 'bin\anet'), "#!/usr/bin/env node`nconsole.log('AgentNetwork v1.1.15');`n")
  Push-Location $src
  try {
    $tgz = @(& npm.cmd pack --silent)
    if ($LASTEXITCODE -ne 0) { throw 'npm pack failed' }
    & npm.cmd install -g --no-audit --no-fund (Join-Path $src ([string]$tgz[-1]).Trim())
    if ($LASTEXITCODE -ne 0) { throw 'npm install -g failed' }
  } finally {
    Pop-Location
  }
  $prefix = Get-NpmPrefix
  # The binary of the platform package, where the legacy shim resolves it.
  $bin = Join-Path $prefix 'node_modules\@agentnetwork\anet\node_modules\@agentnetwork\anet-win32-x64\bin'
  $null = New-Item -ItemType Directory -Force -Path $bin
  [IO.File]::WriteAllText((Join-Path (Split-Path -Parent $bin) 'package.json'), '{"name": "@agentnetwork/anet-win32-x64", "version": "1.1.15"}')
  Copy-Item -LiteralPath $LegacyExe -Destination (Join-Path $bin 'anet.exe')
  foreach ($n in @('anet.cmd', 'anet.ps1', 'node_modules\@agentnetwork\anet\package.json')) {
    Check ('planted ' + (Join-Path $prefix $n)) (Test-Path -LiteralPath (Join-Path $prefix $n))
  }
  # The data of anet 1.x, as its daemon leaves it after one run, with a
  # daemon.pid of a process that no longer exists.
  $null = New-Item -ItemType Directory -Path $dataDir
  foreach ($n in @('api_token', 'agent_token', 'scoped_tokens.json', 'peer-journal.json', 'daemon.log', 'anet.db')) { [IO.File]::WriteAllText((Join-Path $dataDir $n), 'legacy') }
  foreach ($d in @('anet', 'orgs', 'state')) { $null = New-Item -ItemType Directory -Path (Join-Path $dataDir $d) }
  [IO.File]::WriteAllText((Join-Path $dataDir 'anet\identity.json'), '{"did": "did:key:z6Mk"}')
  [IO.File]::WriteAllText((Join-Path $dataDir 'state\last.json'), '{}')
  [IO.File]::WriteAllText((Join-Path $dataDir 'daemon.pid'), '4000000')
  Check 'planted the data of anet 1.x' (Test-Path -LiteralPath (Join-Path $dataDir 'state\last.json'))
}

if ($Verify) {
  $src = Join-Path (Split-Path -Parent $MyInvocation.MyCommand.Path) 'install.ps1'
  $env:ANET_INSTALL_PS1_TEST = '1'
  . ([scriptblock]::Create([IO.File]::ReadAllText($src)))

  $runExe = Get-AnetRunValueExe
  Check 'the Run value AgentNetwork names an anet.exe' ($runExe -ne '' -and (Test-Path -LiteralPath $runExe -PathType Leaf)) $runExe
  $dir = Split-Path -Parent $runExe
  Check 'the install directory is private to this user' ($null -eq (Get-AnetPrivacyProblem -Path $dir)) ([string](Get-AnetPrivacyProblem -Path $dir))
  Check 'no directory above it lets another account move it' ($null -eq (Get-AnetAncestorProblem -Dir $dir)) ([string](Get-AnetAncestorProblem -Dir $dir))
  $userPath = @(Get-AnetPathEntries -Scope User)
  Check 'the install directory is first on the user PATH' ($userPath.Count -gt 0 -and (Test-AnetSameDir $userPath[0] $dir)) ($userPath -join ';')
  $v = @(& $runExe version)
  Check 'anet version works' ($LASTEXITCODE -eq 0 -and ([string]$v[0]) -match '^anet [0-9]+\.[0-9]+\.[0-9]+' -and (($v -join "`n") -match '(?m)^variant: shell$')) ($v -join ' | ')

  $cfg = [IO.File]::ReadAllText((Join-Path $dataDir 'config.json')) | ConvertFrom-Json
  $sh = $cfg.modules.shell
  Check 'modules.shell allow is exactly the controller' ((@($sh.allow) -join ',') -ceq $Aid) (@($sh.allow) -join ',')
  Check 'modules.shell allow_arbitrary is true' ($sh.allow_arbitrary -eq $true)
  Check 'modules.shell shell is powershell' ([string]$sh.shell -ceq 'powershell')
  $allow = @([IO.File]::ReadAllLines((Join-Path $dataDir 'peers.allow')) | ForEach-Object { $_.Trim() })
  Check 'peers.allow contains the controller' ($allow -ccontains $Aid) ($allow -join ',')
  $st = @(& $runExe status)
  Check 'the node runs' ($LASTEXITCODE -eq 0) ($st -join ' | ')

  if ($Takeover) {
    $mark = [string]$env:LEGACY_ANET_MARK
    Check 'the earlier anet was stopped with its own stop command' ($mark -ne '' -and (Test-Path -LiteralPath $mark)) $mark
    $prefix = Get-NpmPrefix
    Check 'the npm package @agentnetwork/anet 1.1.15 was removed' (-not (Test-Path -LiteralPath (Join-Path $prefix 'node_modules\@agentnetwork\anet')))
    Check 'its anet.cmd shim was removed' (-not (Test-Path -LiteralPath (Join-Path $prefix 'anet.cmd')))
    $aside = @(Get-ChildItem -LiteralPath $profileDir -Force -Directory | Where-Object { $_.Name -like '.anet-legacy-1.1.15-*' })
    Check 'the data of anet 1.x was renamed aside' ($aside.Count -eq 1 -and (Test-Path -LiteralPath (Join-Path $aside[0].FullName 'api_token'))) (($aside | ForEach-Object { $_.Name }) -join ',')
    Check 'the 0.3 data directory holds none of it' (-not (Test-Path -LiteralPath (Join-Path $dataDir 'api_token')) -and -not (Test-Path -LiteralPath (Join-Path $dataDir 'orgs')) -and -not (Test-Path -LiteralPath (Join-Path $dataDir 'daemon.pid')))
    Check 'the data directory of 0.3 is of this version' ((Get-AnetDataDirKind -Root $dataDir) -eq 'current') (Get-AnetDataDirKind -Root $dataDir)
    Check 'the install directory holds anet files only' (Test-AnetDedicatedDir -Dir $dir)
  }

  # A new window, with the PATH Windows gives it, runs the new anet.
  $machine = [string][Environment]::GetEnvironmentVariable('Path', 'Machine')
  $user = [string][Environment]::GetEnvironmentVariable('Path', 'User')
  $r = @{}
  Invoke-AnetTimed -FilePath (Join-Path ([Environment]::SystemDirectory) 'WindowsPowerShell\v1.0\powershell.exe') -ArgumentList @('-NoProfile', '-NonInteractive', '-Command', '(Get-Command anet).Source') -PathVariable ($machine.TrimEnd(';') + ';' + $user) -TimeoutSeconds 60 -Result $r
  $got = ''
  if (@($r['Output']).Count -gt 0) { $got = ([string]@($r['Output'])[0]).Trim() }
  Check 'a new window runs the installed anet' ($got -ieq $runExe) $got
}

if ($Cleanup) {
  $runExe = ''
  $key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Software\Microsoft\Windows\CurrentVersion\Run')
  if ($null -ne $key) {
    $v = [string]$key.GetValue('AgentNetwork', '')
    $key.Close()
    if ($v -match '^"([^"]+)"') { $runExe = $Matches[1] }
  }
  if ($runExe -ne '') { & $runExe stop --all }
}

Write-Host ('{0} failed' -f $script:Failures)
exit $script:Failures
