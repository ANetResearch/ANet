# deploy/release/test-install.ps1 - tests of the functions of install.ps1.
#
#   powershell -NoProfile -ExecutionPolicy Bypass -File deploy/release/test-install.ps1
#   pwsh -NoProfile -File deploy/release/test-install.ps1
#
# install.ps1 is loaded in its test mode (ANET_INSTALL_PS1_TEST=1: the last
# line does not call Install-Anet), first as the template, which must refuse
# to install, and then with the pins block replaced by pins of fixture
# builds made here. The tests call the functions that decide: parameter
# validation, the order of the download bases and the npm package names, the
# pin check, the privacy check of a directory before and after its access
# control list is replaced, and the replacement of a binary. Nothing is
# downloaded, and nothing outside a temporary directory is written.
#
# The exit status is the number of failed checks (0: all passed).

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 3

$script:Failures = 0
$script:Checks = 0

function Check {
  param([string]$What, [bool]$Ok, [string]$Detail = '')
  $script:Checks++
  if ($Ok) {
    Write-Host ('ok    ' + $What)
  } else {
    $script:Failures++
    Write-Host ('FAIL  ' + $What + $(if ($Detail -ne '') { ': ' + $Detail } else { '' }))
  }
}

# Refuses runs $Block and reports whether it threw a failure whose message
# contains $Text.
function Refuses {
  param([scriptblock]$Block, [string]$Text)
  try {
    & $Block | Out-Null
    return $false
  } catch {
    return ($_.Exception.Message -like ('*' + $Text + '*'))
  }
}

function Test-IsWindows {
  return ([Environment]::OSVersion.Platform -eq [PlatformID]::Win32NT)
}

$here = Split-Path -Parent $MyInvocation.MyCommand.Path
$template = [IO.File]::ReadAllText((Join-Path $here 'install.ps1'))
$nullLine = '$AnetRelease = $null'
$env:ANET_INSTALL_PS1_TEST = '1'
if (-not (Test-IsWindows) -and [string]::IsNullOrEmpty($env:COMPUTERNAME)) {
  # The default -Name; Windows always has one.
  $env:COMPUTERNAME = 'TESTBOX'
}

# --- the file ----------------------------------------------------------------

$bytes = [IO.File]::ReadAllBytes((Join-Path $here 'install.ps1'))
Check 'install.ps1 is ASCII' (@($bytes | Where-Object { $_ -gt 127 }).Count -eq 0)
$tokens = $null
$errors = $null
[void][System.Management.Automation.Language.Parser]::ParseInput($template, [ref]$tokens, [ref]$errors)
Check 'install.ps1 parses' (@($errors).Count -eq 0) ((@($errors) | ForEach-Object { $_.Message }) -join '; ')
Check 'install.ps1 has one pins line' (([regex]::Matches($template, [regex]::Escape($nullLine))).Count -eq 1)
# -Without changes config.json with the installed binary, after the
# replacement: a failure before it leaves the machine unchanged.
$replaceAt = $template.IndexOf('$aside = Install-AnetStaged -Staged $S.Staged -Exe $S.Exe')
$withoutAt = $template.IndexOf('Invoke-AnetNative -FilePath $S.Exe -ArgumentList @(''init'', ''--without'', $list)')
Check 'init --without runs with the installed binary, after the replacement' ($replaceAt -gt 0 -and $withoutAt -gt $replaceAt) ('' + $replaceAt + ' ' + $withoutAt)
Check 'the staged binary runs nothing but the verification' (-not $template.Contains('-FilePath $S.Staged -ArgumentList @(''init'''))

# --- the template refuses to run ---------------------------------------------

. ([scriptblock]::Create($template))
$global:LASTEXITCODE = 0
$out = (Install-Anet 6>&1 | Out-String)
Check 'the template refuses' ($out -like '*this copy of install.ps1 is the unreleased template*') $out
Check 'the template says nothing was changed' ($out -like '*Nothing was installed or changed.*') $out
Check 'the template sets exit status 1' ($global:LASTEXITCODE -eq 1) ([string]$global:LASTEXITCODE)

# --- fixture builds and their pins -------------------------------------------

function New-Gzip {
  param([byte[]]$Bytes)
  $ms = New-Object System.IO.MemoryStream
  $z = New-Object System.IO.Compression.GZipStream -ArgumentList $ms, ([System.IO.Compression.CompressionMode]::Compress)
  $z.Write($Bytes, 0, $Bytes.Length)
  $z.Dispose()
  return ,$ms.ToArray()
}

function Get-Hex {
  param([byte[]]$Bytes)
  $h = [System.Security.Cryptography.SHA256]::Create()
  return ([BitConverter]::ToString($h.ComputeHash($Bytes)) -replace '-', '').ToLowerInvariant()
}

$fixtures = @{}
$pinsText = "`$AnetRelease = @{`n  Version = '9.8.7'`n  Assets = @{`n"
foreach ($n in @('anet-windows-amd64', 'anet-windows-arm64', 'anet-shell-windows-amd64', 'anet-shell-windows-arm64')) {
  $bin = [Text.Encoding]::ASCII.GetBytes(('MZ fixture build ' + $n + ' ') * 50)
  $gz = New-Gzip -Bytes $bin
  $fixtures[$n] = @{ Bin = $bin; Gz = $gz }
  $pinsText += ("    '{0}' = @{{ GzSha256 = '{1}'; GzSize = {2}; Sha256 = '{3}'; Size = {4} }}`n" -f $n, (Get-Hex $gz), $gz.Length, (Get-Hex $bin), $bin.Length)
}
$pinsText += "  }`n}"
. ([scriptblock]::Create($template.Replace($nullLine, $pinsText)))
Check 'released pins are accepted' (-not (Refuses { Assert-AnetReleased } ''))

# --- parameters --------------------------------------------------------------

$aid = 'bafyrei' + ('a' * 26) + ('2' * 26)
$o = Get-AnetOptions -Given @{}
Check 'no parameters: default build, no join' ($o.Variant -eq 'default' -and $o.AssetPrefix -eq 'anet' -and -not $o.Join -and $o.Controller -eq '')
$o = Get-AnetOptions -Given @{ Controller = $aid }
Check '-Controller: shell build and a join at the default hub' ($o.Variant -eq 'shell' -and $o.AssetPrefix -eq 'anet-shell' -and $o.Join -and $o.Hub -eq 'https://hub.agentnetwork.org.cn')
$o = Get-AnetOptions -Given @{ Controller = $aid; Hub = 'https://h.example' }
Check '-Controller with -Hub joins that hub' ($o.Hub -eq 'https://h.example' -and $o.Join)
$o = Get-AnetOptions -Given @{ Shell = [switch]$true }
Check '-Shell: shell build, no join' ($o.Variant -eq 'shell' -and -not $o.Join)
$o = Get-AnetOptions -Given @{ Hub = 'https://h.example' }
Check '-Hub alone: default build and a join' ($o.Variant -eq 'default' -and $o.Join)
foreach ($bad in @('', 'bafyrei', ('bafyrei' + ('a' * 51)), ('bafyrei' + ('a' * 53)), ('BAFYREI' + ('a' * 52)),
                  ('bafyrei' + ('a' * 51) + '1'), ('bafyrei' + ('a' * 51) + 'A'), (' ' + $aid), ($aid + ';calc'))) {
  Check ('-Controller ''' + $bad + ''' is refused') (Refuses { Get-AnetOptions -Given @{ Controller = $bad } } 'is not an AID')
}
foreach ($good in @('https://hub.agentnetwork.org.cn', 'https://h.example:8443/x/', 'http://127.0.0.1:8080', 'http://localhost')) {
  Check ('-Hub ' + $good + ' is accepted') (-not (Refuses { Get-AnetOptions -Given @{ Hub = $good } } ''))
}
foreach ($bad in @('http://h.example', 'ftp://h.example', 'h.example', 'https://', 'https://u:p@h.example', 'https://h.example/?a=b',
                  'https://h.example/#f', 'https://h .example', 'https://h.example/"x', '/relative', 'http://127.0.0.2')) {
  Check ('-Hub ' + $bad + ' is refused') (Refuses { Get-AnetOptions -Given @{ Hub = $bad } } 'is not an absolute https:// URL')
}
$o = Get-AnetOptions -Given @{ Name = 'build box_1.a-b' }
Check '-Name with spaces, dots, underscores and hyphens' ($o.Name -eq 'build box_1.a-b')
foreach ($bad in @('', '-x', ' x', 'x;y', 'x"y', ('a' * 65), 'name/x')) {
  Check ('-Name ''' + $bad + ''' is refused') (Refuses { Get-AnetOptions -Given @{ Name = $bad } } 'is not a name')
}
$o = Get-AnetOptions -Given @{ Name = ('a' * 64) }
Check '-Name of 64 characters' ($o.Name.Length -eq 64)
$o = Get-AnetOptions -Given @{ Without = @('autoreply,x402', 'x402', 'preview') }
Check '-Without: names collected once each, in order' (($o.Without -join ',') -eq 'autoreply,x402,preview') ($o.Without -join ',')
foreach ($bad in @('', 'AutoReply', 'a;b', 'auto reply', 'a,,b')) {
  Check ('-Without ''' + $bad + ''' is refused') (Refuses { Get-AnetOptions -Given @{ Without = @($bad) } } 'is not a module name')
}
Check '-Shell with -Without shell is refused' (Refuses { Get-AnetOptions -Given @{ Shell = [switch]$true; Without = @('shell') } } 'use one of them')
Check '-Controller with -Without shell is refused' (Refuses { Get-AnetOptions -Given @{ Controller = $aid; Without = @('x402,shell') } } 'use one of them')
$o = Get-AnetOptions -Given @{ Without = @('shell') }
Check '-Without shell on the default build is accepted' (($o.Without -join ',') -eq 'shell')
foreach ($bad in @('anet', '.\anet', '\anet', 'C:', 'C:anet', 'C:\', 'C:\a*b', '\\server\share\anet')) {
  Check ('-Prefix ' + $bad + ' is refused') (Refuses { Get-AnetOptions -Given @{ Prefix = $bad } } '-Prefix')
}
foreach ($bad in @('C:\Users\me\a;b', 'C:\Users\me\%TEMP%\anet', 'C:\x%y')) {
  Check ('-Prefix ' + $bad + ' is refused (; and % corrupt PATH)') (Refuses { Get-AnetOptions -Given @{ Prefix = $bad } } 'contains ; or %')
}
Check 'without -Prefix the directory is chosen later' (-not (Get-AnetOptions -Given @{}).PrefixGiven -and (Get-AnetOptions -Given @{}).Prefix -eq '')
foreach ($ph in @('CONTROLLER_AID', 'controller_aid', ' CONTROLLER_AID ')) {
  Check ('-Controller ''' + $ph + ''' is refused as the placeholder') (Refuses { Get-AnetOptions -Given @{ Controller = $ph } } 'is the placeholder of the published command')
}
$o = Get-AnetOptions -Given @{ Controller = $aid; NoJoin = [switch]$true }
Check '-Controller -NoJoin: shell build, no hub, no join' ($o.Variant -eq 'shell' -and $o.Hub -eq '' -and -not $o.Join)
Check '-NoJoin with -Hub is refused' (Refuses { Get-AnetOptions -Given @{ NoJoin = [switch]$true; Hub = 'https://h.example' } } 'contradict')
$o = Get-AnetOptions -Given @{ Elevated = [switch]$true }
Check '-Elevated is accepted and changes nothing' ($o.Variant -eq 'default' -and -not $o.Join)
Check 'the staged binary is anet.new.exe (PowerShell runs only names in PATHEXT)' ((Split-Path -Leaf (Get-AnetStagedPath -Dir $here)) -ceq 'anet.new.exe')
if (Test-IsWindows) {
  $o = Get-AnetOptions -Given @{ Prefix = 'C:\Users\x\anet\' }
  Check '-Prefix is normalized' ($o.Prefix -eq 'C:\Users\x\anet') $o.Prefix
}
Check '-Base that is not https and not a directory is refused' (Refuses { Get-AnetOptions -Given @{ Base = 'http://example.org' } } 'neither')
Check '-Base that does not exist is refused' (Refuses { Get-AnetOptions -Given @{ Base = 'C:\no\such\anet-dir' } } 'neither')

# --- download bases and npm names --------------------------------------------

Check 'npm package, default x64' ((Get-AnetNpmPackage -AssetPrefix 'anet' -Arch 'amd64') -eq '@agentnetwork/anet-win32-x64')
Check 'npm package, default arm64' ((Get-AnetNpmPackage -AssetPrefix 'anet' -Arch 'arm64') -eq '@agentnetwork/anet-win32-arm64')
Check 'npm package, shell x64' ((Get-AnetNpmPackage -AssetPrefix 'anet-shell' -Arch 'amd64') -eq '@agentnetwork/anet-shell-win32-x64')
Check 'npm package, shell arm64' ((Get-AnetNpmPackage -AssetPrefix 'anet-shell' -Arch 'arm64') -eq '@agentnetwork/anet-shell-win32-arm64')
$savedBase = $env:ANET_INSTALL_BASE
$env:ANET_INSTALL_BASE = $null
$b = @(Get-AnetBases -Options (Get-AnetOptions -Given @{ Controller = $aid }) -Arch 'arm64')
$want = @('https://agentnetwork.org.cn', 'https://hub.agentnetwork.org.cn',
          'https://registry.npmmirror.com/@agentnetwork/anet-shell-win32-arm64/latest/files',
          'https://cdn.jsdelivr.net/npm/@agentnetwork/anet-shell-win32-arm64@latest',
          'https://unpkg.com/@agentnetwork/anet-shell-win32-arm64@latest')
Check 'bases: project hosts, then the npm mirrors (the order of anet update)' (($b -join ' ') -eq ($want -join ' ')) ($b -join ' ')
$env:ANET_INSTALL_BASE = 'https://mirror.example/anet/'
$b = @(Get-AnetBases -Options (Get-AnetOptions -Given @{}) -Arch 'amd64')
Check 'bases: ANET_INSTALL_BASE first' ($b.Count -eq 6 -and $b[0] -eq 'https://mirror.example/anet' -and $b[3] -like '*@agentnetwork/anet-win32-x64/latest/files') ($b -join ' ')
$env:ANET_INSTALL_BASE = 'http://mirror.example'
Check 'bases: an ANET_INSTALL_BASE that is not https is refused' (Refuses { Get-AnetBases -Options (Get-AnetOptions -Given @{}) -Arch 'amd64' } 'ANET_INSTALL_BASE')
$env:ANET_INSTALL_BASE = $savedBase
$b = @(Get-AnetBases -Options (Get-AnetOptions -Given @{ Base = 'https://one.example/' }) -Arch 'amd64')
Check 'bases: -Base replaces the list' ($b.Count -eq 1 -and $b[0] -eq 'https://one.example') ($b -join ' ')

# --- the published commands, argument quoting, rights ------------------------

$universal = 'powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command "[Net.ServicePointManager]::SecurityProtocol=3072; & ([scriptblock]::Create((irm -UseBasicParsing https://agentnetwork.org.cn/install.ps1)))"'
$universalController = $universal.Substring(0, $universal.Length - 1) + ' -Controller CONTROLLER_AID"'
foreach ($line in @($universal, $universalController)) {
  Check ('the header gives the published command ' + $line.Substring($line.Length - 24)) ($template.Contains("`n#   " + $line + "`n"))
  Check 'the published command has no character a shell interprets' (-not ($line -match '[$%!^`''<>]'))
}
Check 'quoting: a plain argument is unchanged' ((ConvertTo-AnetArgument -Text 'uninstall') -ceq 'uninstall')
Check 'quoting: an argument with a space is quoted' ((ConvertTo-AnetArgument -Text 'C:\Users\A B\npm') -ceq '"C:\Users\A B\npm"')
Check 'quoting: a trailing backslash is doubled inside quotes' ((ConvertTo-AnetArgument -Text 'C:\A B\') -ceq '"C:\A B\\"')
Check 'quoting: a double quote is escaped' ((ConvertTo-AnetArgument -Text 'a"b') -ceq '"a\"b"')
Check 'quoting: an empty argument is two quotes' ((ConvertTo-AnetArgument -Text '') -ceq '""')
Check 'quoting: the PATH probe of a new powershell.exe' ((ConvertTo-AnetArgument -Text '(Get-Command anet).Source') -ceq '"(Get-Command anet).Source"')
foreach ($ok in @('uninstall', '-g', '@agentnetwork/anet', 'C:\Users\me\AppData\Roaming\npm', 'D:\a b\npm')) {
  Check ('batch-safe: ' + $ok) (Test-AnetBatchSafe $ok)
}
foreach ($bad in @('C:\50%\npm', 'a&b', 'a|b', 'a"b', 'a^b', 'a!b', 'C:\x (y)\npm', "a`nb")) {
  Check ('not batch-safe: ' + ($bad -replace "`n", '<LF>')) (-not (Test-AnetBatchSafe $bad))
}
# The access masks of the field: (S,X) for an app container capability on the
# profile directory passes; Modify for CodexSandboxUsers, Full control for an
# app container, READ_CONTROL and every generic right fail.
foreach ($m in @(0, 0x100000, 0x20, 0x80, 0x1000A0, 0x100020)) {
  Check ('harmless mask 0x{0:X8}' -f $m) (Test-AnetHarmlessMask ([long]$m))
}
foreach ($m in @(0x1301BF, 0x1F01FF, 0x20000, 0x1, 0x2, 0x4, 0x8, 0x10, 0x40, 0x100, 0x10000, 0x40000, 0x80000,
                 0x10000000, 0x20000000, 0x40000000, [long]2147483648, 0x1200A9)) {
  Check ('mask 0x{0:X8} is not harmless' -f [long]$m) (-not (Test-AnetHarmlessMask ([long]$m)))
}
Check 'rights: Modify is read, write, delete' ((Get-AnetRightsText 0x1301BF) -eq 'read, write, delete, read permissions') (Get-AnetRightsText 0x1301BF)
Check 'rights: Full control' ((Get-AnetRightsText 0x1F01FF) -eq 'full control')
Check 'rights: GENERIC_ALL is full control' ((Get-AnetRightsText 0x10000000) -eq 'full control')
Check 'rights: GENERIC_READ is read' ((Get-AnetRightsText ([long]2147483648)) -eq 'read')
Check 'an unresolvable SID is named as unresolved' ((Get-AnetAccountName 'S-1-5-21-1-2-3-4242') -like '*S-1-5-21-1-2-3-4242*') (Get-AnetAccountName 'S-1-5-21-1-2-3-4242')
Check 'this script run with -File does not end its process' (-not (Test-AnetOneShotHost))

# --- programs run with a time limit ------------------------------------------

$self = (Get-Process -Id $PID).Path
$r = @{}
Invoke-AnetTimed -FilePath $self -ArgumentList @('-NoProfile', '-NonInteractive', '-Command', 'Write-Output ''a b''; exit 3') -TimeoutSeconds 60 -Result $r
Check 'a timed program: its exit status and output' ($r['Code'] -eq 3 -and @($r['Output']) -contains 'a b' -and -not $r['TimedOut']) ('' + $r['Code'] + ' ' + (@($r['Output']) -join '|'))
$r = @{}
Invoke-AnetTimed -FilePath $self -ArgumentList @('-NoProfile', '-NonInteractive', '-Command', '[Console]::In.ReadToEnd().Length') -TimeoutSeconds 60 -Result $r
Check 'a timed program reads an empty standard input, not the console' ($r['Code'] -eq 0 -and @($r['Output']) -contains '0') ((@($r['Output'])) -join '|')
$r = @{}
Invoke-AnetTimed -FilePath $self -ArgumentList @('-NoProfile', '-NonInteractive', '-Command', 'Start-Sleep -Seconds 60') -TimeoutSeconds 2 -Result $r
Check 'a timed program that runs too long is ended and reported' ($r['TimedOut'] -and $r['Code'] -eq -1)
$r = @{}
Invoke-AnetTimed -FilePath (Join-Path $here 'no-such-program.exe') -Result $r
Check 'a program that does not start is code -1' ($r['Code'] -eq -1 -and -not $r['TimedOut'])
if (Test-IsWindows) {
  $r = @{}
  Invoke-AnetTimed -FilePath $self -ArgumentList @('-NoProfile', '-NonInteractive', '-Command', 'Write-Output $env:PATH') -PathVariable 'X-PATH-PROBE' -TimeoutSeconds 60 -Result $r
  Check 'a timed program gets the PATH it is given' (@($r['Output']) -contains 'X-PATH-PROBE') ((@($r['Output'])) -join '|')
}

# Start-AnetNode returns the exit status of anet up alone: its output goes to
# the console, not into the value (a POSIX stand-in for anet.exe).
if (-not (Test-IsWindows)) {
  $fake = Join-Path ([IO.Path]::GetTempPath()) ('anet-up-' + [Guid]::NewGuid().ToString('N'))
  [IO.File]::WriteAllText($fake, "#!/bin/sh`necho started `$1`nexit 0`n")
  & chmod 755 $fake
  $code = Start-AnetNode -Exe $fake -Elevation @{ Elevated = $false; NoStandardToken = ''; Account = 'me' } 6>$null
  Check 'Start-AnetNode returns the exit status alone' ($code -is [int] -and $code -eq 0) ([string]$code)
  Remove-Item -LiteralPath $fake -Force
}

# --- earlier installations ---------------------------------------------------

foreach ($v in @('1.1.15', '1.0.0', '0.2.1', '0.1.10', '2.0.0')) { Check ('legacy version ' + $v) (Test-AnetLegacyVersion $v) }
foreach ($v in @('0.3.0', '0.3.5', '0.4.0-rc.1', '', 'x')) { Check ('not a legacy version: ''' + $v + '''') (-not (Test-AnetLegacyVersion $v)) }

# --- the pin check -----------------------------------------------------------

$pin = $AnetRelease['Assets']['anet-windows-amd64']
$f = $fixtures['anet-windows-amd64']
$got = Test-AnetPin -Gz $f.Gz -Pin $pin
Check 'a .gz that matches its pin gives the binary' ($got -is [byte[]] -and (Get-Hex $got) -eq (Get-Hex $f.Bin))
Check 'a .gz with a byte more is refused' (Refuses { Test-AnetPin -Gz ([byte[]]($f.Gz + [byte]0)) -Pin $pin } 'bytes, the release pins')
$other = $fixtures['anet-windows-arm64'].Gz
Check 'another .gz is refused' (Refuses { Test-AnetPin -Gz $other -Pin $pin } 'release pins')
$p2 = $pin.Clone(); $p2['GzSha256'] = '0' * 64
Check 'a .gz whose SHA-256 differs from the pin is refused' (Refuses { Test-AnetPin -Gz $f.Gz -Pin $p2 } 'SHA-256')
$p2 = $pin.Clone(); $p2['Size'] = $f.Bin.Length - 1
Check 'a binary larger than its pin is refused' (Refuses { Test-AnetPin -Gz $f.Gz -Pin $p2 } 'larger than')
$p2 = $pin.Clone(); $p2['Size'] = $f.Bin.Length + 1
Check 'a binary smaller than its pin is refused' (Refuses { Test-AnetPin -Gz $f.Gz -Pin $p2 } 'bytes, the release pins')
$p2 = $pin.Clone(); $p2['Sha256'] = '0' * 64
Check 'a binary whose SHA-256 differs from the pin is refused' (Refuses { Test-AnetPin -Gz $f.Gz -Pin $p2 } 'the binary has SHA-256')
$broken = [byte[]]$f.Gz.Clone()
$broken[20] = $broken[20] -bxor 0xFF
$p2 = $pin.Clone(); $p2['GzSha256'] = Get-Hex $broken
Check 'a .gz that does not decompress is refused' (Refuses { Test-AnetPin -Gz $broken -Pin $p2 } '')

# A local release directory as a base: the same checks as any other base.
$work = Join-Path ([IO.Path]::GetTempPath()) ('anet-install-test-' + [Guid]::NewGuid().ToString('N'))
$null = [IO.Directory]::CreateDirectory($work)
try {
  $dist = Join-Path $work 'dist'
  $null = [IO.Directory]::CreateDirectory($dist)
  [IO.File]::WriteAllText((Join-Path $dist 'release.json'), '{"schema": "anet-release/1"}')
  [IO.File]::WriteAllText((Join-Path $dist 'release.json.sig'), "-----BEGIN SSH SIGNATURE-----`nAAAA`n-----END SSH SIGNATURE-----`n")
  [IO.File]::WriteAllBytes((Join-Path $dist 'anet-windows-amd64.gz'), $f.Gz)
  $empty = Join-Path $work 'empty'
  $null = [IO.Directory]::CreateDirectory($empty)
  $rel = Get-AnetRelease -Bases @($empty, $dist) -Asset 'anet-windows-amd64' -Pin $pin 6>$null
  Check 'a base that serves nothing is skipped, the next one used' ($rel.Base -eq $dist -and (Get-Hex $rel.Binary) -eq (Get-Hex $f.Bin))
  [IO.File]::WriteAllBytes((Join-Path $dist 'anet-windows-amd64.gz'), $other)
  Check 'a base whose .gz does not match the pins is not used' (Refuses { Get-AnetRelease -Bases @($dist) -Asset 'anet-windows-amd64' -Pin $pin 6>$null } 'no download base served release 9.8.7')
  [IO.File]::WriteAllText((Join-Path $dist 'release.json'), '<!doctype html>')
  [IO.File]::WriteAllBytes((Join-Path $dist 'anet-windows-amd64.gz'), $f.Gz)
  Check 'a base that serves a page instead of a manifest is not used' (Refuses { Get-AnetRelease -Bases @($dist) -Asset 'anet-windows-amd64' -Pin $pin 6>$null } 'no download base')

  # --- earlier installations: npm prefixes and the data directory -----------

  function New-FakeNpm {
    param([string]$Prefix, [string]$Version, [switch]$Binary)
    $pkg = [IO.Path]::Combine($Prefix, 'node_modules', '@agentnetwork', 'anet')
    $null = [IO.Directory]::CreateDirectory($pkg)
    [IO.File]::WriteAllText([IO.Path]::Combine($pkg, 'package.json'), '{"name": "@agentnetwork/anet", "version": "' + $Version + '"}')
    foreach ($n in @('anet.cmd', 'anet.ps1', 'anet')) { [IO.File]::WriteAllText((Join-Path $Prefix $n), 'shim') }
    if ($Binary) {
      $bin = [IO.Path]::Combine($pkg, 'node_modules', '@agentnetwork', 'anet-win32-x64', 'bin')
      $null = [IO.Directory]::CreateDirectory($bin)
      [IO.File]::WriteAllText([IO.Path]::Combine($bin, 'anet.exe'), 'MZ')
    }
  }
  $npmOld = Join-Path $work 'npm-old'
  New-FakeNpm -Prefix $npmOld -Version '1.1.15' -Binary
  $npmNew = Join-Path $work 'npm-new'
  New-FakeNpm -Prefix $npmNew -Version '0.3.4'
  $inst = Join-Path $work 'inst'
  $e = @(Find-AnetOthers -InstallDir $inst -NpmPrefix $npmOld | Where-Object { Test-AnetSameDir $_.Dir $npmOld })
  Check 'an npm install of anet 1.1.15 in the npm prefix is legacy, with the binary that stops it' ($e.Count -eq 1 -and $e[0].Class -eq 'Legacy' -and $e[0].Npm -and $e[0].Version -eq '1.1.15' -and $e[0].StopExe -like '*anet-win32-x64*anet.exe' -and @($e[0].Names) -contains 'anet.cmd')
  $e = @(Find-AnetOthers -InstallDir $inst -NpmPrefix $npmNew | Where-Object { Test-AnetSameDir $_.Dir $npmNew })
  Check 'an npm launcher of anet 0.3.4 is current, with nothing to stop' ($e.Count -eq 1 -and $e[0].Class -eq 'Current' -and $e[0].Npm -and $e[0].StopExe -eq '')
  Check 'the install directory itself is never reported' (@(Find-AnetOthers -InstallDir $npmOld -NpmPrefix $npmOld | Where-Object { Test-AnetSameDir $_.Dir $npmOld }).Count -eq 0)

  $kel03 = [byte[]](@(0xA2, 0x00, 0x6F) + [Text.Encoding]::ASCII.GetBytes('anet.identity/2') + @(0x01, 0x40))
  $kel02 = [byte[]](0xA3, 0x01, 0x40, 0x02, 0x40, 0x03, 0x80)
  function New-DataDir {
    param([string]$Root, [switch]$Legacy1x, [byte[]]$Kel)
    $null = [IO.Directory]::CreateDirectory($Root)
    if ($Legacy1x) {
      [IO.File]::WriteAllText((Join-Path $Root 'api_token'), 'secret')
      foreach ($d in @('anet', 'orgs', 'state')) { $null = [IO.Directory]::CreateDirectory((Join-Path $Root $d)) }
      [IO.File]::WriteAllText([IO.Path]::Combine($Root, 'state', 'last.json'), '{}')
    }
    if ($null -ne $Kel) {
      [IO.File]::WriteAllBytes((Join-Path $Root 'identity.kel'), $Kel)
      [IO.File]::WriteAllText((Join-Path $Root 'config.json'), '{}')
    }
  }
  function Get-Asides {
    param([string]$Root)
    return @(Get-ChildItem -LiteralPath (Split-Path -Parent $Root) -Force | Where-Object { $_.Name -like ((Split-Path -Leaf $Root) + '-legacy-*') })
  }
  $h = Join-Path $work 'home-1x'
  $root = Join-Path $h '.anet'
  New-DataDir -Root $root -Legacy1x
  $line = Move-AnetLegacyData -Root $root -Version '1.1.15'
  $aside = @(Get-Asides $root)
  Check 'a data directory of anet 1.x alone is renamed aside as a whole' (-not (Test-Path -LiteralPath $root) -and $aside.Count -eq 1 -and $aside[0].Name -match '^\.anet-legacy-1\.1\.15-[0-9]{8}T[0-9]{6}Z$' -and (Test-Path -LiteralPath (Join-Path $aside[0].FullName 'api_token'))) $line
  $h = Join-Path $work 'home-mixed'
  $root = Join-Path $h '.anet'
  New-DataDir -Root $root -Legacy1x -Kel $kel03
  $line = Move-AnetLegacyData -Root $root -Version ''
  $aside = @(Get-Asides $root)
  Check 'with an identity of 0.3, only the entries of anet 1.x are moved' ((Test-Path -LiteralPath (Join-Path $root 'identity.kel')) -and (Test-Path -LiteralPath (Join-Path $root 'config.json')) -and
    -not (Test-Path -LiteralPath (Join-Path $root 'api_token')) -and -not (Test-Path -LiteralPath (Join-Path $root 'state')) -and
    $aside.Count -eq 1 -and $aside[0].Name -like '.anet-legacy-1.x-*' -and (Test-Path -LiteralPath ([IO.Path]::Combine($aside[0].FullName, 'state', 'last.json')))) $line
  $h = Join-Path $work 'home-02'
  $root = Join-Path $h '.anet'
  New-DataDir -Root $root -Kel $kel02
  $line = Move-AnetLegacyData -Root $root -Version ''
  $aside = @(Get-Asides $root)
  Check 'a data directory of anet 0.2 is renamed aside as a whole' (-not (Test-Path -LiteralPath $root) -and $aside.Count -eq 1 -and $aside[0].Name -like '.anet-legacy-0.2-*') $line
  $h = Join-Path $work 'home-03'
  $root = Join-Path $h '.anet'
  New-DataDir -Root $root -Kel $kel03
  $line = Move-AnetLegacyData -Root $root -Version '1.1.15'
  Check 'a data directory of anet 0.3 is never moved' ($line -eq '' -and (Test-Path -LiteralPath (Join-Path $root 'identity.kel')) -and @(Get-Asides $root).Count -eq 0) $line
  Check 'no data directory: nothing to do' ((Move-AnetLegacyData -Root (Join-Path $work 'none\.anet') -Version '') -eq '')

  # --- private directories, and the replacement of a binary (Windows) ----------

  if (Test-IsWindows) {
    $user = [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value
    $icacls = Join-Path ([Environment]::SystemDirectory) 'icacls.exe'
    $d = Join-Path $work 'shared'
    $null = [IO.Directory]::CreateDirectory($d)
    $r = @{}
    Invoke-AnetNative -FilePath $icacls -ArgumentList @($d, '/grant', '*S-1-5-32-545:(OI)(CI)R') -Capture -Result $r
    Check 'icacls grants Users read access to a test directory' ($r['Code'] -eq 0)
    $why = Get-AnetPrivacyProblem -Path $d
    Check 'a directory Users can read is not private, and Users is named' ($why -like '*Users*') ([string]$why)
    Set-AnetPrivateAcl -Path $d
    $why = Get-AnetPrivacyProblem -Path $d
    Check 'after Set-AnetPrivateAcl a directory with an explicit rule for Users is private' ($null -eq $why) ([string]$why)
    $acl = Get-Acl -LiteralPath $d
    $sids = @($acl.GetAccessRules($true, $true, [System.Security.Principal.SecurityIdentifier]) | ForEach-Object { $_.IdentityReference.Value } | Sort-Object -Unique)
    Check 'the private directory names only the user, SYSTEM and Administrators' ((($sids | Sort-Object) -join ',') -eq ((@($user, 'S-1-5-18', 'S-1-5-32-544') | Sort-Object) -join ',')) ($sids -join ',')
    Check 'the private directory inherits nothing' ($acl.AreAccessRulesProtected)

    $j = Join-Path $work 'junction'
    New-Item -ItemType Junction -Path $j -Value $d | Out-Null
    $why = Get-AnetPrivacyProblem -Path $j
    Check 'a junction is not private' ($why -like '*reparse point*') ([string]$why)

    $fresh = Join-Path $d 'Programs\anet'
    Initialize-AnetDirectory -Dir $fresh 6>$null
    Check 'a missing install directory below a private one is created private' ((Test-Path -LiteralPath $fresh) -and $null -eq (Get-AnetPrivacyProblem -Path $fresh))
    $exposed = Join-Path $work 'exposed'
    $null = [IO.Directory]::CreateDirectory($exposed)
    $r = @{}
    Invoke-AnetNative -FilePath $icacls -ArgumentList @($exposed, '/grant', '*S-1-1-0:(OI)(CI)M') -Capture -Result $r
    Check 'an install directory below a directory Everyone can change is refused' (Refuses { Initialize-AnetDirectory -Dir (Join-Path $exposed 'anet') 6>$null } 'is not private to this user')
    Check 'nothing was created below it' (-not (Test-Path -LiteralPath (Join-Path $exposed 'anet')))
    Check 'an existing directory Everyone can change is refused' (Refuses { Initialize-AnetDirectory -Dir $exposed 6>$null } 'is not private to this user')

    $exe = Join-Path $fresh 'anet.exe'
    $staged = Get-AnetStagedPath -Dir $fresh
    [IO.File]::WriteAllText($exe, 'old')
    Write-AnetStaged -Path $staged -Bytes ([Text.Encoding]::ASCII.GetBytes('new'))
    $aside = Install-AnetStaged -Staged $staged -Exe $exe
    Check 'the staged binary takes the name of anet.exe' ([IO.File]::ReadAllText($exe) -eq 'new')
    Check 'the previous binary is kept aside' ($aside -match '\\anet\.exe\.old-[0-9]+$' -and [IO.File]::ReadAllText($aside) -eq 'old') $aside
    [IO.File]::WriteAllText(($exe + '.old-1'), 'stale')
    [IO.File]::WriteAllText(($exe + '.old-x'), 'kept')
    Remove-AnetAsides -Dir $fresh -Keep $aside
    Check 'older aside files are removed, the newest and look-alikes kept' (-not (Test-Path -LiteralPath ($exe + '.old-1')) -and (Test-Path -LiteralPath $aside) -and (Test-Path -LiteralPath ($exe + '.old-x')))
    $target = Join-Path $work 'target'
    [IO.File]::WriteAllText($target, 'x')
    New-Item -ItemType Junction -Path $staged -Value $d | Out-Null
    Check 'a staged path that is a junction is refused' (Refuses { Write-AnetStaged -Path $staged -Bytes ([byte[]](1, 2)) } 'reparse point')
    Check 'the junction is left alone' ((Test-Path -LiteralPath $staged) -and (Test-Path -LiteralPath $d))

    # The staged binary must start as a program: PowerShell hands a file
    # whose extension is not in PATHEXT to ShellExecute as a document, which
    # captures nothing. A system program under the staged name runs and
    # its output is captured.
    $probeDir = Join-Path $work 'probe'
    $null = [IO.Directory]::CreateDirectory($probeDir)
    $probe = Get-AnetStagedPath -Dir $probeDir
    [IO.File]::Copy((Join-Path ([Environment]::SystemDirectory) 'whoami.exe'), $probe)
    $r = @{}
    Invoke-AnetNative -FilePath $probe -Capture -Result $r
    $lines = @($r['Output'] | Where-Object { ([string]$_).Trim() -ne '' })
    Check 'a program under the staged name runs, and its output is captured' ($r['Code'] -eq 0 -and $lines.Count -eq 1) ('exit ' + $r['Code'] + ', ' + $lines.Count + ' lines')

    # The access control lists found on a Windows PC with the OpenAI Codex
    # sandbox installed, rebuilt on test directories: (S,X), not inherited,
    # for an app container capability (as on the profile directory) passes;
    # Modify for a local group such as CodexSandboxUsers, and Full control
    # for an app container capability, fail; an inherit-only Modify still
    # fails (it reaches what is created inside). The accounts are named, an
    # unresolvable SID as such.
    $shapes = @(
      @{ Name = '(S,X) for an app container capability'; Grant = '*S-1-15-3-65536-1888954469-739942743-1668119174-2468466756-4239452838-1296943325-355587736-700089176:(S,X)'; Private = $true },
      @{ Name = '(OI)(CI)M for a local group (CodexSandboxUsers)'; Grant = '*S-1-5-21-1000-2000-3000-1005:(OI)(CI)M'; Private = $false; Why = '*S-1-5-21-1000-2000-3000-1005 (unresolved)*read, write, delete*' },
      @{ Name = '(OI)(CI)F for an app container capability'; Grant = '*S-1-15-3-2968813833-1-2-3:(OI)(CI)F'; Private = $false; Why = '*full control*' },
      @{ Name = 'inherit-only (OI)(CI)(IO)M for a local group'; Grant = '*S-1-5-21-1000-2000-3000-1005:(OI)(CI)(IO)M'; Private = $false; Why = '*S-1-5-21-1000-2000-3000-1005*' },
      @{ Name = '(OI)(CI)(RX) for Users'; Grant = '*S-1-5-32-545:(OI)(CI)RX'; Private = $false; Why = '*Users*read*' }
    )
    $n = 0
    foreach ($shape in $shapes) {
      $n++
      $sd = Join-Path $work ('shape' + $n)
      $null = [IO.Directory]::CreateDirectory($sd)
      Set-AnetPrivateAcl -Path $sd
      $r = @{}
      Invoke-AnetNative -FilePath $icacls -ArgumentList @($sd, '/grant', $shape.Grant) -Capture -Result $r
      Check ('icacls grants ' + $shape.Name) ($r['Code'] -eq 0)
      $why = Get-AnetPrivacyProblem -Path $sd
      if ($shape.Private) {
        Check ('private with ' + $shape.Name) ($null -eq $why) ([string]$why)
        Check ('an install directory below a directory with ' + $shape.Name + ' passes') ($null -eq (Get-AnetDirectoryProblem -Dir (Join-Path $sd 'anet') | Where-Object { ([string]$_).StartsWith($work) })) 
      } else {
        Check ('not private with ' + $shape.Name) ($null -ne $why -and $why -like $shape.Why) ([string]$why)
      }
    }

    # Each missing level is created private: <dir>\anet, then <dir>\anet\bin,
    # as on the way to %LOCALAPPDATA%\anet\bin.
    $levels = Join-Path $work 'levels'
    $null = [IO.Directory]::CreateDirectory($levels)
    Set-AnetPrivateAcl -Path $levels
    $deep = Join-Path (Join-Path $levels 'anet') 'bin'
    Initialize-AnetDirectory -Dir $deep 6>$null
    foreach ($lv in @((Join-Path $levels 'anet'), $deep)) {
      $acl = Get-Acl -LiteralPath $lv
      Check ('a created level is private and inherits nothing: ' + $lv) ($null -eq (Get-AnetPrivacyProblem -Path $lv) -and $acl.AreAccessRulesProtected)
    }

    # -Prefix: no directory above it may let another account move it away.
    $anc = Join-Path $work 'ancestor'
    $mine = Join-Path $anc 'me'
    $null = [IO.Directory]::CreateDirectory($mine)
    Set-AnetPrivateAcl -Path $mine
    $why = Get-AnetAncestorProblem -Dir (Join-Path $mine 'anet')
    Check 'ancestors that grant no one else a moving right pass' ($null -eq $why -or -not ([string]$why).StartsWith($work)) ([string]$why)
    $r = @{}
    Invoke-AnetNative -FilePath $icacls -ArgumentList @($anc, '/grant', '*S-1-1-0:(D)') -Capture -Result $r
    Check 'icacls grants Everyone DELETE on a test directory' ($r['Code'] -eq 0)
    $why = Get-AnetAncestorProblem -Dir (Join-Path $mine 'anet')
    $everyone = Get-AnetAccountName 'S-1-1-0'
    Check 'an ancestor that Everyone may delete is reported, by name' ($why -like ('*' + $anc + '*' + $everyone + '*')) ([string]$why)
    Check 'a -Prefix below it is refused' ((Get-AnetDirectoryProblem -Dir (Join-Path $mine 'anet')) -like ('*' + $anc + '*' + $everyone + '*rename*'))
    Check 'nothing was created below it' (-not (Test-Path -LiteralPath (Join-Path $mine 'anet')))
    $r = @{}
    Invoke-AnetNative -FilePath $icacls -ArgumentList @($anc, '/remove:g', '*S-1-1-0') -Capture -Result $r
    Invoke-AnetNative -FilePath $icacls -ArgumentList @($anc, '/grant', '*S-1-1-0:(OI)(CI)(IO)(D)') -Capture -Result $r
    $why = Get-AnetAncestorProblem -Dir (Join-Path $mine 'anet')
    Check 'a rule for the children only (inherit-only) does not count' ($null -eq $why -or -not ([string]$why).StartsWith($work)) ([string]$why)
  } else {
    Write-Host 'skip  the access control list and replacement checks need Windows'
  }
} finally {
  Remove-Item -LiteralPath $work -Recurse -Force -ErrorAction SilentlyContinue
}

Write-Host ''
Write-Host ('{0} checks, {1} failed' -f $script:Checks, $script:Failures)
exit $script:Failures
