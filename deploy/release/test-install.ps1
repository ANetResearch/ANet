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
Check 'the default prefix is not checked as one given with -Prefix' (-not (Get-AnetOptions -Given @{}).PrefixGiven)
Check 'the staged binary is anet.new.exe (PowerShell runs only names in PATHEXT)' ((Split-Path -Leaf (Get-AnetStagedPath -Dir $here)) -ceq 'anet.new.exe')
if (Test-IsWindows) {
  $o = Get-AnetOptions -Given @{ Prefix = 'C:\Users\x\anet\' }
  Check '-Prefix is normalized' ($o.Prefix -eq 'C:\Users\x\anet') $o.Prefix
  $o = Get-AnetOptions -Given @{}
  Check 'the default prefix is %LOCALAPPDATA%\Programs\anet' ($o.Prefix -eq (Join-Path ([Environment]::GetFolderPath('LocalApplicationData')) 'Programs\anet')) $o.Prefix
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
    Check 'a directory Users can read is not private' ($why -like '*S-1-5-32-545*') ([string]$why)
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
    Check 'an ancestor that Everyone may delete is reported' ($why -like ('*' + $anc + '*S-1-1-0*')) ([string]$why)
    Check 'a -Prefix below it is refused' (Refuses { Initialize-AnetDirectory -Dir (Join-Path $mine 'anet') -CheckAncestors 6>$null } 'is not private to this user')
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
