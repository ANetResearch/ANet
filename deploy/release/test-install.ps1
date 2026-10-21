# deploy/release/test-install.ps1 - tests of the functions of install.ps1.
#
#   powershell -NoProfile -ExecutionPolicy Bypass -File deploy/release/test-install.ps1
#   pwsh -NoProfile -File deploy/release/test-install.ps1
#
# install.ps1 is loaded in its test mode (ANET_INSTALL_PS1_TEST=1: the last
# line does not call Install-Anet), first as the template, which must refuse
# to install, and then with the pins block replaced by pins of fixture
# builds made here. The tests call the functions that decide: parameter
# validation; the download sources, their order for given probe results and
# the npm package names; the reading of npm tarballs (an unsafe entry name is
# refused); a source that does not serve the release passed over and a file
# that fails the pins ending the installation (with the HTTP function
# replaced by one that serves fixtures); the pin check; the daemon of an
# earlier anet that holds the data directory; the log of a run; the summary
# (golden output, in English and Chinese); the privacy check of a directory
# before and after its access control list is replaced, and the replacement
# of a binary. Nothing is downloaded (the probe asks 127.0.0.1 only), and
# nothing outside a temporary directory is written.
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
$withoutAt = $template.IndexOf('Invoke-AnetCommand -Exe $S.Exe -ArgumentList @(''init'', ''--without'', $list)')
Check 'init --without runs with the installed binary, after the replacement' ($replaceAt -gt 0 -and $withoutAt -gt $replaceAt) ('' + $replaceAt + ' ' + $withoutAt)
Check 'the staged binary runs nothing but the verification' (([regex]::Matches($template, '-Exe \$Staged ')).Count -eq 1 -and -not $template.Contains('$S.Staged -ArgumentList'))

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
Check 'no parameters: default build, started, joins the default hub' ($o.Variant -eq 'default' -and $o.AssetPrefix -eq 'anet' -and $o.Start -and $o.Join -and $o.Hub -eq 'https://hub.agentnetwork.org.cn' -and $o.Controller -eq '')
$o = Get-AnetOptions -Given @{ Controller = $aid }
Check '-Controller: shell build and a join at the default hub' ($o.Variant -eq 'shell' -and $o.AssetPrefix -eq 'anet-shell' -and $o.Join -and $o.Hub -eq 'https://hub.agentnetwork.org.cn')
$o = Get-AnetOptions -Given @{ Controller = $aid; Hub = 'https://h.example' }
Check '-Controller with -Hub joins that hub' ($o.Hub -eq 'https://h.example' -and $o.Join)
$o = Get-AnetOptions -Given @{ Shell = [switch]$true }
Check '-Shell: shell build, started and joined' ($o.Variant -eq 'shell' -and $o.Start -and $o.Join)
$o = Get-AnetOptions -Given @{ Hub = 'https://h.example' }
Check '-Hub alone: default build and a join of that hub' ($o.Variant -eq 'default' -and $o.Join -and $o.Hub -eq 'https://h.example')
$o = Get-AnetOptions -Given @{}
Check 'no parameters: no agents are wired' (@($o.Agents).Count -eq 0)
$o = Get-AnetOptions -Given @{ Agents = [switch]$true }
Check '-Agents: every agent found (--all)' ((@($o.Agents) -join ' ') -eq '--all')
$o = Get-AnetOptions -Given @{ AgentList = @('claude,codex', 'claude', ' zcode ') }
Check '-AgentList: the agents named, once each' ((@($o.Agents) -join ' ') -eq 'claude codex zcode')
$o = Get-AnetOptions -Given @{ Agents = [switch]$true; AgentList = @('trae') }
Check '-AgentList with -Agents: the list' ((@($o.Agents) -join ' ') -eq 'trae')
foreach ($bad in @('', 'Claude', '--all', 'claude;calc', 'a b')) {
  Check ('-AgentList ''' + $bad + ''' is refused') (Refuses { Get-AnetOptions -Given @{ AgentList = @($bad) } } 'is not the name of an agent')
}
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
$o = Get-AnetOptions -Given @{ NoJoin = [switch]$true }
Check '-NoJoin: started, no hub, no join' ($o.Start -and $o.Hub -eq '' -and -not $o.Join)
$o = Get-AnetOptions -Given @{ NoStart = [switch]$true }
Check '-NoStart: neither started nor joined' (-not $o.Start -and $o.Hub -eq '' -and -not $o.Join)
$o = Get-AnetOptions -Given @{ Controller = $aid; NoStart = [switch]$true }
Check '-Controller -NoStart: shell build, not started' ($o.Variant -eq 'shell' -and -not $o.Start -and -not $o.Join)
Check '-NoStart with -Hub is refused' (Refuses { Get-AnetOptions -Given @{ NoStart = [switch]$true; Hub = 'https://h.example' } } 'contradict')
$o = Get-AnetOptions -Given @{ Elevated = [switch]$true }
Check '-Elevated is accepted and changes nothing' ($o.Variant -eq 'default' -and $o.Join -and $o.Start)
Check 'the staged binary is anet.new.exe (PowerShell runs only names in PATHEXT)' ((Split-Path -Leaf (Get-AnetStagedPath -Dir $here)) -ceq 'anet.new.exe')
if (Test-IsWindows) {
  $o = Get-AnetOptions -Given @{ Prefix = 'C:\Users\x\anet\' }
  Check '-Prefix is normalized' ($o.Prefix -eq 'C:\Users\x\anet') $o.Prefix
}
Check '-Base that is not https and not a directory is refused' (Refuses { Get-AnetOptions -Given @{ Base = 'http://example.org' } } 'neither')
Check '-Base that does not exist is refused' (Refuses { Get-AnetOptions -Given @{ Base = 'C:\no\such\anet-dir' } } 'neither')

# --- download sources, their order and the npm names ------------------------

Check 'npm package, default x64' ((Get-AnetNpmPackage -AssetPrefix 'anet' -Arch 'amd64') -eq '@agentnetwork/anet-win32-x64')
Check 'npm package, default arm64' ((Get-AnetNpmPackage -AssetPrefix 'anet' -Arch 'arm64') -eq '@agentnetwork/anet-win32-arm64')
Check 'npm package, shell x64' ((Get-AnetNpmPackage -AssetPrefix 'anet-shell' -Arch 'amd64') -eq '@agentnetwork/anet-shell-win32-x64')
Check 'npm package, shell arm64' ((Get-AnetNpmPackage -AssetPrefix 'anet-shell' -Arch 'arm64') -eq '@agentnetwork/anet-shell-win32-arm64')
$list = Get-AnetSourceList -AssetPrefix 'anet-shell' -Arch 'arm64' -Version '9.8.7'
Check 'npmmirror: the tarball of exactly this version' ($list['npmmirror'].Kind -eq 'tarball' -and $list['npmmirror'].Url -ceq 'https://registry.npmmirror.com/@agentnetwork/anet-shell-win32-arm64/-/anet-shell-win32-arm64-9.8.7.tgz' -and $list['npmmirror'].MetaUrl -ceq 'https://registry.npmmirror.com/@agentnetwork/anet-shell-win32-arm64/9.8.7') $list['npmmirror'].Url
Check 'npmjs: the tarball of exactly this version' ($list['npmjs'].Url -ceq 'https://registry.npmjs.org/@agentnetwork/anet-shell-win32-arm64/-/anet-shell-win32-arm64-9.8.7.tgz' -and $list['npmjs'].MetaUrl -ceq 'https://registry.npmjs.org/@agentnetwork/anet-shell-win32-arm64/9.8.7')
Check 'GitHub: the release of exactly this version, redirects to GitHub only' ($list['github'].Kind -eq 'files' -and $list['github'].Prefix -ceq 'https://github.com/ANetResearch/ANet/releases/download/v9.8.7/' -and $list['github'].Redirect -eq 'github')
Check 'jsDelivr: dl/ of the npm package of exactly this version' ($list['jsdelivr'].Prefix -ceq 'https://cdn.jsdelivr.net/npm/@agentnetwork/anet-shell-win32-arm64@9.8.7/dl/')
Check 'the project hosts: /dl/' ($list['host1'].Prefix -ceq 'https://agentnetwork.org.cn/dl/' -and $list['host2'].Prefix -ceq 'https://hub.agentnetwork.org.cn/dl/')
Check 'the probe of npmmirror reads the metadata of the version' ((Get-AnetProbeUrl -Source $list['npmmirror']) -ceq $list['npmmirror'].MetaUrl)
Check 'the probe of GitHub reads release.json of the version' ((Get-AnetProbeUrl -Source $list['github']) -ceq 'https://github.com/ANetResearch/ANet/releases/download/v9.8.7/release.json')
$tail = @('registry.npmjs.org', 'jsDelivr', 'agentnetwork.org.cn', 'hub.agentnetwork.org.cn')
foreach ($case in @(
    @{ Name = 'both probed sources answered, GitHub first'; Reachable = @('github', 'npmmirror'); Want = @('GitHub', 'npmmirror') + $tail },
    @{ Name = 'both probed sources answered, npmmirror first'; Reachable = @('npmmirror', 'github'); Want = @('npmmirror', 'GitHub') + $tail },
    @{ Name = 'only npmmirror answered'; Reachable = @('npmmirror'); Want = @('npmmirror') + $tail },
    @{ Name = 'only GitHub answered'; Reachable = @('github'); Want = @('GitHub') + $tail },
    @{ Name = 'neither answered'; Reachable = @(); Want = $tail },
    @{ Name = 'a name given twice or unknown is used once or not at all'; Reachable = @('github', 'github', 'host1', 'npmjs'); Want = @('GitHub') + $tail })) {
  $got = @(Get-AnetSourceOrder -List $list -Probe @{ Reachable = $case.Reachable } | ForEach-Object { $_.Label })
  Check ('source order: ' + $case.Name) (($got -join ' ') -ceq ($case.Want -join ' ')) ($got -join ' ')
}
$savedBase = $env:ANET_INSTALL_BASE
$env:ANET_INSTALL_BASE = $null
$got = @(Get-AnetSources -Options (Get-AnetOptions -Given @{ Controller = $aid }) -Arch 'arm64' -Version '9.8.7' -Probe @{ Reachable = @('github') } | ForEach-Object { $_.Label })
Check 'sources: the probe result, then the fixed order' (($got -join ' ') -ceq ((@('GitHub') + $tail) -join ' ')) ($got -join ' ')
$env:ANET_INSTALL_BASE = 'https://mirror.example/anet/'
$s = @(Get-AnetSources -Options (Get-AnetOptions -Given @{}) -Arch 'amd64' -Version '9.8.7' -Probe @{ Reachable = @('github') })
Check 'sources: ANET_INSTALL_BASE replaces them' ($s.Count -eq 1 -and $s[0].Kind -eq 'files' -and $s[0].Prefix -ceq 'https://mirror.example/anet/dl/' -and $s[0].Label -ceq 'https://mirror.example/anet') (($s | ForEach-Object { $_.Label }) -join ' ')
$env:ANET_INSTALL_BASE = 'http://mirror.example'
Check 'sources: an ANET_INSTALL_BASE that is not https is refused' (Refuses { Get-AnetSources -Options (Get-AnetOptions -Given @{}) -Arch 'amd64' -Version '9.8.7' -Probe @{ Reachable = @() } } 'ANET_INSTALL_BASE')
$env:ANET_INSTALL_BASE = 'https://mirror.example'
$s = @(Get-AnetSources -Options (Get-AnetOptions -Given @{ Base = 'https://one.example/' }) -Arch 'amd64' -Version '9.8.7' -Probe @{ Reachable = @() })
Check 'sources: -Base replaces them, and wins over ANET_INSTALL_BASE' ($s.Count -eq 1 -and $s[0].Prefix -ceq 'https://one.example/dl/') (($s | ForEach-Object { $_.Label }) -join ' ')
$env:ANET_INSTALL_BASE = $savedBase
Check 'GitHub hosts' ((Test-AnetGitHubHost 'github.com') -and (Test-AnetGitHubHost 'release-assets.githubusercontent.com') -and (Test-AnetGitHubHost 'objects.githubusercontent.com') -and
  -not (Test-AnetGitHubHost 'githubusercontent.com.evil.example') -and -not (Test-AnetGitHubHost 'evilgithub.com') -and -not (Test-AnetGitHubHost 'example.org'))
# The probe in parallel: two sources that refuse the connection at once are
# both unreachable, and the probe returns within its bound.
if ($null -eq ('System.Net.Http.HttpClient' -as [type])) { Add-Type -AssemblyName System.Net.Http }
$clock = [Diagnostics.Stopwatch]::StartNew()
$p = Invoke-AnetProbe -Sources @(@{ Name = 'npmmirror'; Kind = 'tarball'; MetaUrl = 'https://127.0.0.1:9/x'; Redirect = '' }, @{ Name = 'github'; Kind = 'files'; Prefix = 'https://127.0.0.1:9/'; Redirect = 'github' }) -Version '9.8.7' -TimeoutMs 1500 -TotalMs 2000
Check 'the probe of sources that do not answer finds none, within its bound' (@($p.Reachable).Count -eq 0 -and $clock.ElapsedMilliseconds -lt 6000) ('' + $clock.ElapsedMilliseconds + ' ms, ' + (@($p.Reachable) -join ','))

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
# .NET Framework writes the byte order mark of the console input encoding
# when it closes the standard input of a child (Windows PowerShell 5.1 with
# UTF-8 input), and the child decodes those three bytes in its own code
# page. What matters is that the child reads the end of its input at once
# instead of waiting for the console: it ends well within the time limit
# and has read at most that mark.
Invoke-AnetTimed -FilePath $self -ArgumentList @('-NoProfile', '-NonInteractive', '-Command', '[Console]::In.ReadToEnd().Length') -TimeoutSeconds 60 -Result $r
$read = @($r['Output'] | Where-Object { ([string]$_) -match '^\d+$' })
Check 'a timed program reads an empty standard input, not the console' ($r['Code'] -eq 0 -and -not $r['TimedOut'] -and $read.Count -eq 1 -and [int]$read[0] -le 3) ((@($r['Output'])) -join '|')
$r = @{}
Invoke-AnetTimed -FilePath $self -ArgumentList @('-NoProfile', '-NonInteractive', '-Command', 'Start-Sleep -Seconds 60') -TimeoutSeconds 2 -Result $r
Check 'a timed program that runs too long is ended and reported' ($r['TimedOut'] -and $r['Code'] -eq -1)
$r = @{}
Invoke-AnetTimed -FilePath (Join-Path $here 'no-such-program.exe') -Result $r
Check 'a program that does not start is code -1' ($r['Code'] -eq -1 -and -not $r['TimedOut'])
if (Test-IsWindows) {
  $r = @{}
  Invoke-AnetTimed -FilePath $self -ArgumentList @('-NoProfile', '-NonInteractive', '-Command', 'Write-Output $env:PATH') -PathVariable 'X-PATH-PROBE' -TimeoutSeconds 60 -Result $r
  # PowerShell 7 puts its own directory in front of the PATH it starts with.
  Check 'a timed program gets the PATH it is given' (@(@($r['Output']) -join ';' -split ';') -contains 'X-PATH-PROBE') ((@($r['Output'])) -join '|')
}

# Start-AnetNode returns the exit status of anet up alone: its output goes to
# the console, not into the value (a POSIX stand-in for anet.exe).
if (-not (Test-IsWindows)) {
  $fake = Join-Path ([IO.Path]::GetTempPath()) ('anet-up-' + [Guid]::NewGuid().ToString('N'))
  [IO.File]::WriteAllText($fake, "#!/bin/sh`necho started `$1`nexit 0`n")
  & chmod 755 $fake
  $start = Start-AnetNode -Exe $fake -Elevation @{ Elevated = $false; Restricted = $false; NoStandardToken = ''; Account = 'me' } 6>$null
  Check 'Start-AnetNode returns the exit status alone' ($start.Code -is [int] -and $start.Code -eq 0 -and -not $start.Deferred) ([string]$start.Code)
  Remove-Item -LiteralPath $fake -Force
}

# --- earlier installations ---------------------------------------------------

foreach ($v in @('1.1.15', '0.3.0', '0.4.0-rc.1', '999999.0.0')) { Check ('a version: ' + $v) (Test-AnetVersion $v) }
foreach ($v in @('', 'x', '1.0', '1.0.0\..\..\Public\x', '1.0.0 ', '1.0.0-' + ('a' * 41), '1234567.0.0')) { Check ('not a version: ''' + $v + '''') (-not (Test-AnetVersion $v)) }
Check 'version order' ((Compare-AnetVersion '0.3.4' '0.3.5') -eq -1 -and (Compare-AnetVersion '1.1.15' '0.3.0') -eq 1 -and (Compare-AnetVersion '0.3.5-rc.1' '0.3.5') -eq 0 -and (Compare-AnetVersion '0.10.0' '0.9.9') -eq 1)
$repo1x = '{"name": "@agentnetwork/anet", "version": "1.1.15", "repository": {"url": "git+https://github.com/ChatChatTech/AgentNetwork.git"}}'
$repoAnet = '{"name": "@agentnetwork/anet", "version": "1.2.0", "repository": {"url": "git+https://github.com/ANetResearch/ANet.git"}}'
Check 'the 1.x package by its repository' (Test-AnetLegacyPackage -Text $repo1x -Version '1.1.15')
Check 'a 1.x package without a repository field' (Test-AnetLegacyPackage -Text '{"name": "@agentnetwork/anet", "version": "1.1.15"}' -Version '1.1.15')
Check 'a 0.2 package' (Test-AnetLegacyPackage -Text '{}' -Version '0.2.1')
Check 'anet 0.3.4 as the npm launcher is not legacy' (-not (Test-AnetLegacyPackage -Text '{"name": "@agentnetwork/anet", "version": "0.3.4"}' -Version '0.3.4'))
Check 'a future anet 1.x that names ANetResearch/ANet is not legacy' (-not (Test-AnetLegacyPackage -Text $repoAnet -Version '1.2.0'))
Check 'a file that is not there has no family' ((Get-AnetBinaryFamily -Exe (Join-Path $here 'no-such.exe')) -eq '')

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
  # --- npm tarballs: what is read, and what is refused -----------------------

  function New-TarHeader {
    param([string]$Name, [long]$Size, [string]$Type = '0')
    $h = New-Object byte[] 512
    $nb = [Text.Encoding]::UTF8.GetBytes($Name)
    if ($nb.Length -gt 100) { throw 'name too long for the test header' }
    [Array]::Copy($nb, 0, $h, 0, $nb.Length)
    foreach ($f in @(@(100, '0000644'), @(108, '0000000'), @(116, '0000000'), @(124, [Convert]::ToString($Size, 8).PadLeft(11, '0')), @(136, '00000000000'))) {
      $b = [Text.Encoding]::ASCII.GetBytes([string]$f[1])
      [Array]::Copy($b, 0, $h, [int]$f[0], $b.Length)
    }
    $h[156] = [byte][char]$Type
    $magic = [Text.Encoding]::ASCII.GetBytes('ustar')
    [Array]::Copy($magic, 0, $h, 257, $magic.Length)
    $h[263] = 0x30; $h[264] = 0x30
    for ($i = 148; $i -lt 156; $i++) { $h[$i] = 32 }
    $sum = 0
    foreach ($x in $h) { $sum += $x }
    $c = [Text.Encoding]::ASCII.GetBytes([Convert]::ToString($sum, 8).PadLeft(6, '0'))
    [Array]::Copy($c, 0, $h, 148, 6)
    $h[154] = 0; $h[155] = 32
    return ,$h
  }
  # New-Tar: entries are @(name, bytes[, type]); a type x entry's bytes are
  # its pax records.
  function New-Tar {
    param([object[]]$Entries, [switch]$NoEnd)
    $ms = New-Object System.IO.MemoryStream
    foreach ($e in $Entries) {
      $data = [byte[]]$e[1]
      $type = '0'
      if ($e.Count -gt 2) { $type = [string]$e[2] }
      $h = New-TarHeader -Name ([string]$e[0]) -Size $data.Length -Type $type
      $ms.Write($h, 0, 512)
      $ms.Write($data, 0, $data.Length)
      $pad = (512 - ($data.Length % 512)) % 512
      if ($pad -gt 0) { $ms.Write((New-Object byte[] $pad), 0, $pad) }
    }
    if (-not $NoEnd) { $ms.Write((New-Object byte[] 1024), 0, 1024) }
    return ,$ms.ToArray()
  }
  function New-Pax {
    param([string]$Key, [string]$Value)
    $rec = ' ' + $Key + '=' + $Value + "`n"
    $n = $rec.Length + 2
    if (($n.ToString() + $rec).Length -ne $n) { $n++ }
    return ,([Text.Encoding]::UTF8.GetBytes($n.ToString() + $rec))
  }
  $manifest = [Text.Encoding]::ASCII.GetBytes('{"schema": "anet-release/1", "version": "9.8.7"}')
  $signature = [Text.Encoding]::ASCII.GetBytes("-----BEGIN SSH SIGNATURE-----`nAAAA`n-----END SSH SIGNATURE-----`n")
  $want = @('package/dl/release.json', 'package/dl/release.json.sig', 'package/dl/anet-windows-amd64.gz')
  $goodEntries = @(@('package/package.json', [Text.Encoding]::ASCII.GetBytes('{"name":"x"}')), @('package/dl/release.json', $manifest),
                   @('package/dl/release.json.sig', $signature), @('package/dl/anet-windows-amd64.gz', $f.Gz), @('package/README.md', [byte[]](65, 10)))
  $goodTgz = New-Gzip -Bytes (New-Tar -Entries $goodEntries)
  $files = Read-AnetTarball -Tgz $goodTgz -Want $want -Limit 10485760
  Check 'an npm tarball gives exactly the wanted files' ($files.Count -eq 3 -and (Get-Hex $files['package/dl/anet-windows-amd64.gz']) -eq (Get-Hex $f.Gz) -and (Get-Hex $files['package/dl/release.json']) -eq (Get-Hex $manifest))
  $files = Read-AnetTarball -Tgz (New-Gzip -Bytes (New-Tar -Entries @(@('PaxHeader/x', (New-Pax 'path' 'package/dl/release.json'), 'x'), @('short', $manifest)))) -Want $want -Limit 10485760
  Check 'a pax header names the next entry' ($files.ContainsKey('package/dl/release.json') -and -not $files.ContainsKey('short'))
  foreach ($bad in @('package/../../evil.txt', '../evil', '/etc/evil', 'package/dl/../../x', 'package\..\evil', 'C:evil', 'package/./dl/x', 'package//dl/x', ('package/dl/' + [char]1 + 'x'))) {
    $tgz = New-Gzip -Bytes (New-Tar -Entries (@($goodEntries) + ,@($bad, [byte[]](1, 2, 3))))
    Check ('a tarball with the entry ''' + ($bad -replace '[\x00-\x1f]', '?') + ''' is refused whole') (Refuses { Read-AnetTarball -Tgz $tgz -Want $want -Limit 10485760 } 'unsafe name')
  }
  foreach ($case in @(@{ Name = 'a pax path'; Entries = @(@('PaxHeader/x', (New-Pax 'path' '../../evil'), 'x'), @('ok', [byte[]](1))) },
                      @{ Name = 'a GNU long name'; Entries = @(@('././@LongLink', [Text.Encoding]::ASCII.GetBytes("package/../../evil`0"), 'L'), @('ok', [byte[]](1))) })) {
    $tgz = New-Gzip -Bytes (New-Tar -Entries $case.Entries)
    Check ('a traversal in ' + $case.Name + ' is refused') (Refuses { Read-AnetTarball -Tgz $tgz -Want $want -Limit 10485760 } 'unsafe name')
  }
  $tgz = New-Gzip -Bytes (New-Tar -Entries (,@('package/dl/release.json', [byte[]]@(), '2')))
  Check 'a wanted name that is a link is refused' (Refuses { Read-AnetTarball -Tgz $tgz -Want $want -Limit 10485760 } 'not a regular file')
  $tgz = New-Gzip -Bytes (New-Tar -Entries @(@('package/dl/release.json', $manifest), @('package/dl/release.json', $manifest)))
  Check 'a wanted name that comes twice is refused' (Refuses { Read-AnetTarball -Tgz $tgz -Want $want -Limit 10485760 } 'twice')
  $tar = New-Tar -Entries $goodEntries -NoEnd
  $tgz = New-Gzip -Bytes ([byte[]]$tar[0..($tar.Length - 700)])
  Check 'a tarball that ends early is refused' (Refuses { Read-AnetTarball -Tgz $tgz -Want $want -Limit 10485760 } 'ends')
  $tar = New-Tar -Entries $goodEntries
  $tar[0] = $tar[0] -bxor 0x20
  Check 'a header with a wrong checksum is refused' (Refuses { Read-AnetTarball -Tgz (New-Gzip -Bytes $tar) -Want $want -Limit 10485760 } 'checksum')
  Check 'a tarball that is not gzip is refused' (Refuses { Read-AnetTarball -Tgz ([Text.Encoding]::ASCII.GetBytes('<html></html>')) -Want $want -Limit 10485760 } 'not a gzip')
  Check 'a tarball that decompresses beyond the limit is refused' (Refuses { Read-AnetTarball -Tgz $goodTgz -Want $want -Limit 2048 } 'more than')
  foreach ($ok in @('package/dl/release.json', 'package/dl/', 'a')) { Check ('a safe entry name: ' + $ok) (Test-AnetTarName $ok) }

  # --- the sources in order: not serving moves on, a failed check ends -------

  # Get-AnetHttpBytes is replaced by one that serves $served (URL to bytes)
  # and answers anything else as not served, and records each request.
  $realHttp = ${function:Get-AnetHttpBytes}
  $served = @{}
  $requested = New-Object System.Collections.Generic.List[string]
  ${function:Get-AnetHttpBytes} = {
    param([string]$Url, [long]$Limit, [int]$TimeoutSeconds, [string]$Redirect = '', [hashtable]$Progress = $null)
    $requested.Add($Url)
    if (-not $served.ContainsKey($Url)) { throw (New-AnetNotServing ('HTTP 404 from ' + ([Uri]$Url).Host)) }
    $b = [byte[]]$served[$Url]
    if ($b.Length -gt $Limit) { throw (New-AnetNotServing 'more than expected') }
    if ($null -ne $Progress) { Update-AnetProgress -Progress $Progress -Done $b.Length }
    return ,$b
  }
  $srcs = @(Get-AnetSourceOrder -List (Get-AnetSourceList -AssetPrefix 'anet' -Arch 'amd64' -Version '9.8.7') -Probe @{ Reachable = @('npmmirror', 'github') })
  $mm = $srcs[0]; $gh = $srcs[1]; $h1 = $srcs[4]
  function Set-Files { param([string]$Prefix, [byte[]]$M = $manifest, [byte[]]$Gz = $f.Gz) $served[$Prefix + 'release.json'] = $M; $served[$Prefix + 'release.json.sig'] = $signature; $served[$Prefix + 'anet-windows-amd64.gz'] = $Gz }
  function Use { $requested.Clear(); return (Get-AnetRelease -Sources $srcs -Asset 'anet-windows-amd64' -Pin $pin -Platform 'windows-amd64' 6>$null) }

  $served.Clear(); Set-Files $gh.Prefix
  $rel = Use
  Check 'npmmirror without the version: GitHub is used' ($rel.Source.Label -eq 'GitHub' -and (Get-Hex $rel.Binary) -eq (Get-Hex $f.Bin) -and $requested[0] -ceq $mm.MetaUrl) ($requested -join ' ')
  $served.Clear()
  $served[$mm.MetaUrl] = [Text.Encoding]::ASCII.GetBytes('{"name": "@agentnetwork/anet-win32-x64", "version": "9.8.7"}')
  $served[$mm.Url] = $goodTgz
  Set-Files $gh.Prefix
  $rel = Use
  Check 'npmmirror with the version: its tarball is used' ($rel.Source.Label -eq 'npmmirror' -and (Get-Hex $rel.Binary) -eq (Get-Hex $f.Bin) -and @($requested | Where-Object { $_ -like 'https://github.com/*' }).Count -eq 0) ($requested -join ' ')
  $served[$mm.MetaUrl] = [Text.Encoding]::ASCII.GetBytes('{"version": "9.8.6"}')
  $rel = Use
  Check 'npmmirror that lags behind: the next source' ($rel.Source.Label -eq 'GitHub' -and -not ($requested -ccontains $mm.Url)) ($requested -join ' ')
  $served[$mm.MetaUrl] = [Text.Encoding]::ASCII.GetBytes('{"version": "9.8.7"}')
  $served[$mm.Url] = New-Gzip -Bytes (New-Tar -Entries (@($goodEntries) + ,@('package/../evil', [byte[]](1))))
  $rel = Use
  Check 'a tarball with an unsafe entry: the next source' ($rel.Source.Label -eq 'GitHub') ($requested -join ' ')
  $served.Clear()
  Set-Files $gh.Prefix -M ([Text.Encoding]::ASCII.GetBytes('<!doctype html><title>sign in</title>'))
  Set-Files $h1.Prefix
  $rel = Use
  Check 'a page instead of release.json: the next source' ($rel.Source.Label -eq 'agentnetwork.org.cn') ($requested -join ' ')
  $served.Clear()
  Set-Files $gh.Prefix -M ([Text.Encoding]::ASCII.GetBytes('{"version": "9.9.0"}')) -Gz $other
  Set-Files $h1.Prefix
  $rel = Use
  Check 'a source of another release: the next source, its .gz not read' ($rel.Source.Label -eq 'agentnetwork.org.cn' -and -not ($requested -ccontains ($gh.Prefix + 'anet-windows-amd64.gz'))) ($requested -join ' ')
  $served.Clear()
  Set-Files $gh.Prefix -Gz ([byte[]](0x3C, 0x68, 0x74, 0x6D, 0x6C))
  Set-Files $h1.Prefix
  $rel = Use
  Check 'a .gz that is a page: the next source' ($rel.Source.Label -eq 'agentnetwork.org.cn') ($requested -join ' ')
  $served.Clear()
  Set-Files $gh.Prefix -Gz $other
  Set-Files $h1.Prefix
  Check 'a .gz of this release that fails the pins ends the installation' (Refuses { Use } 'release pins')
  Check 'and no later source is asked' (-not ($requested -ccontains ($h1.Prefix + 'release.json'))) ($requested -join ' ')
  $served.Clear()
  $served[$mm.MetaUrl] = [Text.Encoding]::ASCII.GetBytes('{"version": "9.8.7"}')
  $served[$mm.Url] = New-Gzip -Bytes (New-Tar -Entries @(@('package/dl/release.json', $manifest), @('package/dl/release.json.sig', $signature), @('package/dl/anet-windows-amd64.gz', $other)))
  Set-Files $gh.Prefix
  Check 'a tarball whose .gz fails the pins ends the installation' (Refuses { Use } 'release pins')
  $served.Clear()
  Check 'no source serves the release: refused, naming every source tried' (Refuses { Use } 'no download source served anet 9.8.7 (tried: npmmirror, GitHub, registry.npmjs.org, jsDelivr, agentnetwork.org.cn, hub.agentnetwork.org.cn)')
  ${function:Get-AnetHttpBytes} = $realHttp

  # A local release directory (-Base): the same checks.
  $dist = Join-Path $work 'dist'
  $null = [IO.Directory]::CreateDirectory($dist)
  [IO.File]::WriteAllBytes((Join-Path $dist 'release.json'), $manifest)
  [IO.File]::WriteAllBytes((Join-Path $dist 'release.json.sig'), $signature)
  [IO.File]::WriteAllBytes((Join-Path $dist 'anet-windows-amd64.gz'), $f.Gz)
  $empty = Join-Path $work 'empty'
  $null = [IO.Directory]::CreateDirectory($empty)
  $rel = Get-AnetRelease -Sources @((New-AnetBaseSource $empty), (New-AnetBaseSource $dist)) -Asset 'anet-windows-amd64' -Pin $pin 6>$null
  Check 'a directory that serves nothing is passed over, the next one used' ($rel.Source.Label -eq $dist -and (Get-Hex $rel.Binary) -eq (Get-Hex $f.Bin))
  [IO.File]::WriteAllBytes((Join-Path $dist 'anet-windows-amd64.gz'), $other)
  Check 'a directory whose .gz does not match the pins ends the installation' (Refuses { Get-AnetRelease -Sources @((New-AnetBaseSource $dist)) -Asset 'anet-windows-amd64' -Pin $pin 6>$null } 'release pins')
  [IO.File]::WriteAllText((Join-Path $dist 'release.json'), '<!doctype html>')
  [IO.File]::WriteAllBytes((Join-Path $dist 'anet-windows-amd64.gz'), $f.Gz)
  Check 'a directory that holds a page instead of a manifest is not used' (Refuses { Get-AnetRelease -Sources @((New-AnetBaseSource $dist)) -Asset 'anet-windows-amd64' -Pin $pin 6>$null } 'no download source')
  [IO.File]::WriteAllBytes((Join-Path $dist 'release.json'), $manifest)

  # --- earlier installations: npm prefixes and the data directory -----------

  $plain = @{ Elevated = $false; Restricted = $false; NoStandardToken = ''; Account = 'me' }
  $restricted = @{ Elevated = $true; Restricted = $true; NoStandardToken = ''; Account = 'me' }
  # Stand-ins for binaries: a file that carries the Go module path of
  # AgentNetwork 1.x, one that carries anet's, and one with neither.
  $v1Bytes = [Text.Encoding]::ASCII.GetBytes('MZ' + ('x' * 3000) + 'github.com/ChatChatTech/AgentNetwork/cmd/anet.main' + ('y' * 3000))
  $anetBytes = [Text.Encoding]::ASCII.GetBytes('MZ' + ('x' * 3000) + 'github.com/ANetResearch/ANet/internal/daemon.New' + ('y' * 3000))
  $otherBytes = [Text.Encoding]::ASCII.GetBytes('MZ' + ('x' * 6000))
  $bins = Join-Path $work 'bins'
  $null = [IO.Directory]::CreateDirectory($bins)
  [IO.File]::WriteAllBytes((Join-Path $bins 'v1.exe'), $v1Bytes)
  [IO.File]::WriteAllBytes((Join-Path $bins 'anet.exe'), $anetBytes)
  [IO.File]::WriteAllBytes((Join-Path $bins 'other.exe'), $otherBytes)
  Check 'a binary with the module path of AgentNetwork 1.x is of family v1' ((Get-AnetBinaryFamily -Exe (Join-Path $bins 'v1.exe')) -eq 'v1')
  Check 'a binary with the module path of this project is of family anet' ((Get-AnetBinaryFamily -Exe (Join-Path $bins 'anet.exe')) -eq 'anet')
  Check 'a binary with neither is of no family' ((Get-AnetBinaryFamily -Exe (Join-Path $bins 'other.exe')) -eq '')
  # A text that straddles the block boundary of the block-wise search.
  $straddle = New-Object byte[] (1048576 + 20)
  $marker = [Text.Encoding]::ASCII.GetBytes('ChatChatTech/AgentNetwork')
  [Array]::Copy($marker, 0, $straddle, 1048576 - 10, $marker.Length)
  [IO.File]::WriteAllBytes((Join-Path $bins 'straddle.bin'), $straddle)
  Check 'a text across a block boundary is found' (Test-AnetFileContains -Path (Join-Path $bins 'straddle.bin') -Text 'ChatChatTech/AgentNetwork')
  Check 'a text that is not there is not found' (-not (Test-AnetFileContains -Path (Join-Path $bins 'straddle.bin') -Text 'ChatChatTech/AgentNetworks'))
  $info = Get-AnetVersionInfo -Exe (Join-Path $bins 'other.exe') -Elevation $plain
  Check 'an anet.exe of neither project is Unknown and not run' ($info.Class -eq 'Unknown' -and $info.Version -eq '')
  $info = Get-AnetVersionInfo -Exe (Join-Path $bins 'v1.exe') -Elevation $restricted
  Check 'from a restricted window, a 1.x binary is Legacy without being run' ($info.Class -eq 'Legacy' -and $info.Version -eq '')
  [IO.File]::WriteAllText((Join-Path $bins 'anet.exe.release.json'), '{"schema": "anet-release/1", "version": "0.3.4"}')
  $info = Get-AnetVersionInfo -Exe (Join-Path $bins 'anet.exe') -Elevation $restricted
  Check 'from a restricted window, an anet.exe is Current with the version of its release record' ($info.Class -eq 'Current' -and $info.Version -eq '0.3.4')
  [IO.File]::WriteAllText((Join-Path $bins 'anet.exe.release.json'), '{"version": "0.3.4\\..\\x"}')
  Check 'a release record with a version that is not one is ignored' ((Get-AnetRecordVersion -Exe (Join-Path $bins 'anet.exe')) -eq '')
  $dedicated = Join-Path $work 'dedicated'
  $null = [IO.Directory]::CreateDirectory($dedicated)
  foreach ($n in @('anet.exe', 'anet.exe.release.json', 'anet.exe.release.json.sig', 'anet.exe.old-1700000000000', 'anet.new.exe')) { [IO.File]::WriteAllText((Join-Path $dedicated $n), 'x') }
  Check 'a directory with anet files only is dedicated' (Test-AnetDedicatedDir -Dir $dedicated)
  [IO.File]::WriteAllText((Join-Path $dedicated 'tool.exe'), 'x')
  Check 'a directory with another program is not dedicated' (-not (Test-AnetDedicatedDir -Dir $dedicated))
  Check 'a directory with other files is not dedicated' (-not (Test-AnetDedicatedDir -Dir $bins))

  function New-FakeNpm {
    param([string]$Prefix, [string]$Version, [string]$Repository = '', [byte[]]$Binary)
    $pkg = [IO.Path]::Combine($Prefix, 'node_modules', '@agentnetwork', 'anet')
    $null = [IO.Directory]::CreateDirectory($pkg)
    $repo = ''
    if ($Repository -ne '') { $repo = ', "repository": {"type": "git", "url": "' + $Repository + '"}' }
    [IO.File]::WriteAllText([IO.Path]::Combine($pkg, 'package.json'), '{"name": "@agentnetwork/anet", "version": "' + $Version + '"' + $repo + '}')
    foreach ($n in @('anet.cmd', 'anet.ps1', 'anet')) { [IO.File]::WriteAllText((Join-Path $Prefix $n), 'shim') }
    if ($null -ne $Binary) {
      $bin = [IO.Path]::Combine($pkg, 'node_modules', '@agentnetwork', 'anet-win32-x64', 'bin')
      $null = [IO.Directory]::CreateDirectory($bin)
      [IO.File]::WriteAllBytes([IO.Path]::Combine($bin, 'anet.exe'), $Binary)
    }
  }
  $npmOld = Join-Path $work 'npm-old'
  New-FakeNpm -Prefix $npmOld -Version '1.1.15' -Repository 'git+https://github.com/ChatChatTech/AgentNetwork.git' -Binary $v1Bytes
  $npmNew = Join-Path $work 'npm-new'
  New-FakeNpm -Prefix $npmNew -Version '0.3.4' -Repository 'git+https://github.com/ANetResearch/ANet.git'
  $npmOdd = Join-Path $work 'npm-odd'
  New-FakeNpm -Prefix $npmOdd -Version '1.1.15' -Binary $otherBytes
  $npmBad = Join-Path $work 'npm-bad'
  New-FakeNpm -Prefix $npmBad -Version '1.0.0\..\..\Public\x'
  $inst = Join-Path $work 'inst'
  $e = @(Find-AnetOthers -InstallDir $inst -NpmPrefix $npmOld -Elevation $plain | Where-Object { Test-AnetSameDir $_.Dir $npmOld })
  Check 'an npm install of anet 1.1.15 in the npm prefix is legacy, with the binary that stops it' ($e.Count -eq 1 -and $e[0].Class -eq 'Legacy' -and $e[0].Npm -and $e[0].Version -eq '1.1.15' -and $e[0].StopExe -like '*anet-win32-x64*anet.exe' -and @($e[0].Names) -contains 'anet.cmd')
  $e = @(Find-AnetOthers -InstallDir $inst -NpmPrefix $npmNew -Elevation $plain | Where-Object { Test-AnetSameDir $_.Dir $npmNew })
  Check 'an npm launcher of anet 0.3.4 is current, with nothing to stop' ($e.Count -eq 1 -and $e[0].Class -eq 'Current' -and $e[0].Npm -and $e[0].StopExe -eq '')
  $e = @(Find-AnetOthers -InstallDir $inst -NpmPrefix $npmOdd -Elevation $plain | Where-Object { Test-AnetSameDir $_.Dir $npmOdd })
  Check 'a 1.x package whose binary lacks the module path has no stop binary' ($e.Count -eq 1 -and $e[0].Class -eq 'Legacy' -and $e[0].StopExe -eq '' -and $e[0].StopNote -like '*not a binary of AgentNetwork 1.x*') ([string]$e[0].StopNote)
  $e = @(Find-AnetOthers -InstallDir $inst -NpmPrefix $npmBad -Elevation $plain | Where-Object { Test-AnetSameDir $_.Dir $npmBad })
  Check 'a package.json whose version is not one is not an npm installation of anet' ($e.Count -eq 1 -and -not $e[0].Npm -and $e[0].Class -eq 'Unknown')
  Check 'the install directory itself is never reported' (@(Find-AnetOthers -InstallDir $npmOld -NpmPrefix $npmOld -Elevation $plain | Where-Object { Test-AnetSameDir $_.Dir $npmOld }).Count -eq 0)

  # --- the data directory rule: the cases of TestInstallShDataDirKindAgrees --
  function New-Kel {
    param([string]$Tag)
    $t = [Text.Encoding]::ASCII.GetBytes($Tag)
    return [byte[]](@(0xA2, 0x00, (0x60 + $t.Length)) + $t + @(0x01, 0x40))
  }
  $kel03 = New-Kel -Tag 'anet.identity/2'
  $kel02 = [byte[]](0xA3, 0x01, 0x40, 0x02, 0x40, 0x03, 0x80)
  function Write-V1Layout {
    param([string]$Root)
    foreach ($n in @('api_token', 'agent_token', 'scoped_tokens.json', 'peer-journal.json', 'daemon.log', 'anet.db')) { [IO.File]::WriteAllText((Join-Path $Root $n), 'x') }
    $null = [IO.Directory]::CreateDirectory((Join-Path $Root 'anet'))
    [IO.File]::WriteAllText([IO.Path]::Combine($Root, 'anet', 'identity.json'), '{"did":"did:key:z6Mk"}')
    $null = [IO.Directory]::CreateDirectory([IO.Path]::Combine($Root, 'cas', 'pebble'))
    [IO.File]::WriteAllText([IO.Path]::Combine($Root, 'cas', 'pebble', 'LOCK'), '')
  }
  function Write-Init034 {
    param([string]$Root)
    [IO.File]::WriteAllText((Join-Path $Root 'config.json'), '{"control_addr": "127.0.0.1:0"}')
    foreach ($n in @('peers.allow', 'peers.deny', 'peers.trust', 'payees.allow')) { [IO.File]::WriteAllText((Join-Path $Root $n), '') }
  }
  $kinds = @(
    @{ Name = 'missing'; Want = 'none'; Make = { param($r) [IO.Directory]::Delete($r) } },
    @{ Name = 'empty'; Want = 'none'; Make = { param($r) } },
    @{ Name = '0.3 identity'; Want = 'current'; Make = { param($r) Write-Init034 $r; [IO.File]::WriteAllBytes((Join-Path $r 'identity.kel'), $kel03) } },
    @{ Name = '0.3 init only'; Want = 'current'; Make = { param($r) Write-Init034 $r } },
    @{ Name = '0.2 identity'; Want = 'older'; Make = { param($r) [IO.File]::WriteAllBytes((Join-Path $r 'identity.kel'), $kel02) } },
    @{ Name = 'identity tag 1'; Want = 'older'; Make = { param($r) [IO.File]::WriteAllBytes((Join-Path $r 'identity.kel'), (New-Kel -Tag 'anet.identity/1')) } },
    @{ Name = 'identity tag 3'; Want = 'newer'; Make = { param($r) [IO.File]::WriteAllBytes((Join-Path $r 'identity.kel'), (New-Kel -Tag 'anet.identity/3')) } },
    @{ Name = 'identity tag 03'; Want = 'invalid'; Make = { param($r) [IO.File]::WriteAllBytes((Join-Path $r 'identity.kel'), (New-Kel -Tag 'anet.identity/03')) } },
    @{ Name = 'identity not cbor'; Want = 'invalid'; Make = { param($r) [IO.File]::WriteAllText((Join-Path $r 'identity.kel'), 'not an identity') } },
    @{ Name = 'identity empty'; Want = 'invalid'; Make = { param($r) [IO.File]::WriteAllText((Join-Path $r 'identity.kel'), '') } },
    @{ Name = '1.x'; Want = 'v1'; Make = { param($r) Write-V1Layout $r } },
    @{ Name = '1.x and anet init of 0.3.4'; Want = 'v1'; Make = { param($r) Write-V1Layout $r; Write-Init034 $r } },
    @{ Name = '1.x token only'; Want = 'v1'; Make = { param($r) $null = [IO.Directory]::CreateDirectory((Join-Path $r 'anet')); [IO.File]::WriteAllText([IO.Path]::Combine($r, 'anet', 'api_token'), 'x') } },
    @{ Name = '1.x and a 0.3 identity'; Want = 'mixed'; Make = { param($r) Write-V1Layout $r; Write-Init034 $r; [IO.File]::WriteAllBytes((Join-Path $r 'identity.kel'), $kel03) } },
    @{ Name = '1.x and a named 0.3 identity'; Want = 'mixed'; Make = { param($r) Write-V1Layout $r; $n = [IO.Path]::Combine($r, 'ids', 'work'); $null = [IO.Directory]::CreateDirectory($n); [IO.File]::WriteAllBytes((Join-Path $n 'identity.kel'), $kel03) } },
    @{ Name = '1.x and a named 0.2 identity'; Want = 'v1'; Make = { param($r) Write-V1Layout $r; $n = [IO.Path]::Combine($r, 'ids', 'work'); $null = [IO.Directory]::CreateDirectory($n); [IO.File]::WriteAllBytes((Join-Path $n 'identity.kel'), $kel02) } },
    @{ Name = '1.x and an empty named identity'; Want = 'v1'; Make = { param($r) Write-V1Layout $r; $n = [IO.Path]::Combine($r, 'ids', 'work'); $null = [IO.Directory]::CreateDirectory($n); [IO.File]::WriteAllText((Join-Path $n 'identity.kel'), '') } },
    @{ Name = '1.x and a 0.2 identity'; Want = 'older'; Make = { param($r) Write-V1Layout $r; [IO.File]::WriteAllBytes((Join-Path $r 'identity.kel'), $kel02) } }
  )
  $k = 0
  foreach ($c in $kinds) {
    $k++
    # Pattern characters in the path are read literally.
    $root = [IO.Path]::Combine($work, ('kind' + $k + '[1]'), '.anet')
    $null = [IO.Directory]::CreateDirectory($root)
    & $c.Make $root
    $got = Get-AnetDataDirKind -Root $root
    Check ('data directory kind: ' + $c.Name + ' is ' + $c.Want) ($got -eq $c.Want) $got
  }

  function Get-Asides {
    param([string]$Root)
    return @(Get-ChildItem -LiteralPath (Split-Path -Parent $Root) -Force | Where-Object { $_.Name -like ((Split-Path -Leaf $Root) + '-legacy-*') })
  }
  $root = [IO.Path]::Combine($work, 'home-1x', '.anet')
  $null = [IO.Directory]::CreateDirectory($root)
  Write-V1Layout $root
  $m = Move-AnetLegacyData -Root $root -Version '1.1.15' -Kind 'v1' -Aside (Get-AnetLegacyAside -Root $root -Version '1.1.15')
  $aside = @(Get-Asides $root)
  Check 'a data directory of anet 1.x alone is renamed aside as a whole' (-not $m.Held -and -not (Test-Path -LiteralPath $root) -and $aside.Count -eq 1 -and $aside[0].Name -match '^\.anet-legacy-1\.1\.15-[0-9]{8}T[0-9]{6}Z$' -and (Test-Path -LiteralPath (Join-Path $aside[0].FullName 'api_token'))) $m.Line
  $root = [IO.Path]::Combine($work, 'home-mixed', '.anet')
  $null = [IO.Directory]::CreateDirectory($root)
  Write-V1Layout $root
  Write-Init034 $root
  [IO.File]::WriteAllBytes((Join-Path $root 'identity.kel'), $kel03)
  foreach ($d in @('orgs', 'state')) { $null = [IO.Directory]::CreateDirectory((Join-Path $root $d)) }
  [IO.File]::WriteAllText([IO.Path]::Combine($root, 'state', 'last.json'), '{}')
  [IO.File]::WriteAllText((Join-Path $root 'daemon.pid'), '1')
  $m = Move-AnetLegacyData -Root $root -Version '1.x' -Kind 'mixed' -Aside (Get-AnetLegacyAside -Root $root -Version '1.x')
  $aside = @(Get-Asides $root)
  $stay = @('identity.kel', 'config.json', 'peers.allow', 'daemon.log') | Where-Object { -not (Test-Path -LiteralPath (Join-Path $root $_)) }
  $gone = @('anet', 'anet.db', 'api_token', 'agent_token', 'scoped_tokens.json', 'peer-journal.json', 'cas', 'orgs', 'state', 'daemon.pid') | Where-Object { Test-Path -LiteralPath (Join-Path $root $_) }
  Check 'with an identity of 0.3, exactly the entries of anet 1.x are moved' (-not $m.Held -and @($stay).Count -eq 0 -and @($gone).Count -eq 0 -and $aside.Count -eq 1 -and $aside[0].Name -like '.anet-legacy-1.x-*' -and (Test-Path -LiteralPath ([IO.Path]::Combine($aside[0].FullName, 'state', 'last.json')))) ($m.Line + ' stay=' + ($stay -join ',') + ' gone=' + ($gone -join ','))
  $root = [IO.Path]::Combine($work, 'home-02', '.anet')
  $null = [IO.Directory]::CreateDirectory($root)
  [IO.File]::WriteAllBytes((Join-Path $root 'identity.kel'), $kel02)
  $m = Move-AnetLegacyData -Root $root -Version '0.2' -Kind 'older' -Aside (Get-AnetLegacyAside -Root $root -Version '0.2')
  $aside = @(Get-Asides $root)
  Check 'a data directory of anet 0.2 is renamed aside as a whole' (-not $m.Held -and -not (Test-Path -LiteralPath $root) -and $aside.Count -eq 1 -and $aside[0].Name -like '.anet-legacy-0.2-*') $m.Line
  $cmd = Get-AnetMoveCommand -Root 'C:\Users\me\.anet' -Aside 'C:\Users\me\.anet-legacy-1.x-20260101T000000Z' -Kind 'v1'
  Check 'the move command for a 1.x directory renames it' ($cmd -ceq 'Rename-Item -LiteralPath ''C:\Users\me\.anet'' -NewName ''.anet-legacy-1.x-20260101T000000Z''') $cmd
  $cmd = Get-AnetMoveCommand -Root 'C:\Users\O''Brien\.anet' -Aside 'C:\Users\O''Brien\.anet-legacy-1.x-20260101T000000Z' -Kind 'v1'
  Check 'the move command quotes an apostrophe in the profile path' ($cmd -ceq 'Rename-Item -LiteralPath ''C:\Users\O''''Brien\.anet'' -NewName ''.anet-legacy-1.x-20260101T000000Z''') $cmd
  $cmd = Get-AnetMoveCommand -Root 'C:\Users\me\.anet' -Aside 'C:\Users\me\.anet-legacy-1.x-20260101T000000Z' -Kind 'mixed'
  Check 'the move command for a mixed directory moves the 1.x entries' ($cmd -like 'New-Item -ItemType Directory -Force -Path ''C:\Users\me\.anet-legacy-1.x-20260101T000000Z''; foreach ($n in ''anet'', ''anet.db'', *''daemon.pid'') { if (Test-Path -LiteralPath (Join-Path ''C:\Users\me\.anet'' $n)) { Move-Item -LiteralPath (Join-Path ''C:\Users\me\.anet'' $n) -Destination ''C:\Users\me\.anet-legacy-1.x-20260101T000000Z'' } }') $cmd

  # The takeover with nothing found to stop, as install.sh: a data directory
  # of an earlier anet that no daemon holds is moved (whole for 1.x, the 1.x
  # entries out of a mixed one); a directory of 0.3, a newer one or one
  # whose identity file does not read is left alone; nothing holds the
  # installation.
  $root = [IO.Path]::Combine($work, 'home-free', '.anet')
  $null = [IO.Directory]::CreateDirectory($root)
  Write-V1Layout $root
  [IO.File]::WriteAllText((Join-Path $root 'daemon.pid'), '2000000000')
  $t = Invoke-AnetTakeover -InstallDir $inst -DataDir $root -Elevation $plain 6>$null
  $aside = @(Get-Asides $root)
  Check 'a 1.x data directory that no daemon holds is moved aside whole, with nothing found to stop' (-not $t.Held -and -not (Test-Path -LiteralPath $root) -and $aside.Count -eq 1 -and $aside[0].Name -like '.anet-legacy-1.x-*' -and (Test-Path -LiteralPath (Join-Path $aside[0].FullName 'api_token')) -and @($t.Remaining).Count -eq 0) (($t.Report -join ' | ') + ' ' + ($t.Remaining -join ' | '))
  $root = [IO.Path]::Combine($work, 'home-free-02', '.anet')
  $null = [IO.Directory]::CreateDirectory($root)
  [IO.File]::WriteAllBytes((Join-Path $root 'identity.kel'), $kel02)
  $t = Invoke-AnetTakeover -InstallDir $inst -DataDir $root -Elevation $plain 6>$null
  $aside = @(Get-Asides $root)
  Check 'a 0.2 data directory is moved aside whole under the label 0.2' (-not $t.Held -and -not (Test-Path -LiteralPath $root) -and $aside.Count -eq 1 -and $aside[0].Name -like '.anet-legacy-0.2-*') ($t.Report -join ' | ')
  foreach ($case in @(@{ Name = '0.3'; Kel = $kel03 }, @{ Name = 'newer'; Kel = (New-Kel -Tag 'anet.identity/3') }, @{ Name = 'invalid'; Kel = [byte[]]@() })) {
    $root = [IO.Path]::Combine($work, ('home-' + $case.Name), '.anet')
    $null = [IO.Directory]::CreateDirectory($root)
    Write-V1Layout $root
    [IO.File]::WriteAllBytes((Join-Path $root 'identity.kel'), $case.Kel)
    $before = Get-AnetDataDirKind -Root $root
    $t = Invoke-AnetTakeover -InstallDir $inst -DataDir $root -Elevation $plain 6>$null
    if ($case.Name -eq '0.3') {
      $aside = @(Get-Asides $root)
      Check 'a mixed data directory keeps its 0.3 identity and loses the 1.x entries, with nothing found to stop' (-not $t.Held -and $before -eq 'mixed' -and (Test-Path -LiteralPath (Join-Path $root 'identity.kel')) -and -not (Test-Path -LiteralPath (Join-Path $root 'api_token')) -and $aside.Count -eq 1 -and (Test-Path -LiteralPath (Join-Path $aside[0].FullName 'api_token')) -and (Get-AnetDataDirKind -Root $root) -eq 'current') ($t.Report -join ' | ')
    } else {
      Check ('a data directory of kind ' + $before + ' is left alone and does not hold the installation') (-not $t.Held -and (Test-Path -LiteralPath (Join-Path $root 'identity.kel')) -and (Test-Path -LiteralPath (Join-Path $root 'api_token')) -and @(Get-Asides $root).Count -eq 0 -and @($t.Remaining).Count -eq 0) ($t.Report -join ' | ')
    }
  }
  $t = Invoke-AnetTakeover -InstallDir $inst -DataDir ([IO.Path]::Combine($work, 'none', '.anet')) -Elevation $plain 6>$null
  Check 'no data directory: nothing to do' (-not $t.Held -and @($t.Remaining).Count -eq 0)

  # --- a daemon that holds the data directory --------------------------------

  # A process ID in daemon.pid is a daemon of AgentNetwork 1.x only when its
  # program carries the module path of 1.x: this test's own process (as a
  # running anet 0.3 daemon, or a program that reused the ID of a stale
  # daemon.pid) is not one, and the directory is not held.
  $root = [IO.Path]::Combine($work, 'home-reused-pid', '.anet')
  $null = [IO.Directory]::CreateDirectory($root)
  Write-V1Layout $root
  Write-Init034 $root
  [IO.File]::WriteAllBytes((Join-Path $root 'identity.kel'), $kel03)
  [IO.File]::WriteAllText((Join-Path $root 'daemon.pid'), [string]$PID)
  $l = Get-AnetLegacyDaemons -Root $root
  Check 'a daemon.pid that names a process of another program is no daemon of 1.x' (@($l.Daemons).Count -eq 0)
  $t = Invoke-AnetTakeover -InstallDir $inst -DataDir $root -Elevation $plain 6>$null
  Check 'a directory with a 0.3 identity and a process of 0.3 is not held; only the 1.x entries move' (-not $t.Held -and (Test-Path -LiteralPath (Join-Path $root 'identity.kel')) -and -not (Test-Path -LiteralPath (Join-Path $root 'api_token')) -and $null -ne $t.Taken -and $t.Taken.Aside -like '*.anet-legacy-1.x-*') ($t.Report -join ' | ')

  # A program that carries the module path of 1.x and runs: a copy of a
  # system program that waits, with the module path appended (the loader
  # ignores what follows the image).
  $sleeper = $null
  $proc = $null
  if (Test-IsWindows) {
    $sleeper = Join-Path $work 'legacy-daemon.exe'
    [IO.File]::Copy((Join-Path ([Environment]::SystemDirectory) 'PING.EXE'), $sleeper)
    # Its messages, under the new name, for the languages Windows has them in.
    foreach ($mui in @(Get-ChildItem -Path (Join-Path ([Environment]::SystemDirectory) '*\PING.EXE.mui') -ErrorAction SilentlyContinue)) {
      $to = Join-Path $work $mui.Directory.Name
      $null = [IO.Directory]::CreateDirectory($to)
      [IO.File]::Copy($mui.FullName, (Join-Path $to 'legacy-daemon.exe.mui'), $true)
    }
    $args1 = @('-n', '120', '127.0.0.1')
  } elseif (Test-Path -LiteralPath '/bin/sleep') {
    $sleeper = Join-Path $work 'legacy-daemon'
    [IO.File]::Copy('/bin/sleep', $sleeper)
    & chmod 755 $sleeper
    $args1 = @('120')
  }
  if ($null -ne $sleeper) {
    $fs = [IO.File]::Open($sleeper, [IO.FileMode]::Append)
    $mark = [Text.Encoding]::ASCII.GetBytes('github.com/ChatChatTech/AgentNetwork/cmd/anet.main')
    $fs.Write($mark, 0, $mark.Length)
    $fs.Dispose()
    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName = $sleeper
    $psi.Arguments = ($args1 -join ' ')
    $psi.UseShellExecute = $false
    $psi.RedirectStandardOutput = $true
    $psi.CreateNoWindow = $true
    $proc = [System.Diagnostics.Process]::Start($psi)
    try {
      $root = [IO.Path]::Combine($work, 'home-held', '.anet')
      $null = [IO.Directory]::CreateDirectory($root)
      Write-V1Layout $root
      [IO.File]::WriteAllText((Join-Path $root 'daemon.pid'), [string]$proc.Id)
      $l = Get-AnetLegacyDaemons -Root $root
      Check 'a running program of 1.x named by daemon.pid holds the directory' (@($l.Daemons).Count -eq 1 -and $l.Daemons[0].Pid -eq $proc.Id) (@($l.Daemons | ForEach-Object { $_.Pid }) -join ',')
      $t = Invoke-AnetTakeover -InstallDir $inst -DataDir $root -Elevation $plain 6>$null
      $proc.Refresh()
      $aside = @(Get-Asides $root)
      Check 'the takeover ends that daemon by its process ID and moves the data' ($proc.HasExited -and -not $t.Held -and $aside.Count -eq 1 -and $t.Taken.Label -eq 'AgentNetwork 1.x') ($t.Report -join ' | ')
    } finally {
      if (-not $proc.HasExited) { $proc.Kill() }
      $proc.Dispose()
    }
  } else {
    Write-Host 'skip  no program to stand in for a daemon of 1.x'
  }

  # --- the log of a run --------------------------------------------------------

  Start-AnetLog
  Write-AnetLine 'line one of the log'
  $current = [IO.Path]::Combine($work, 'home-log', '.anet')
  $null = [IO.Directory]::CreateDirectory($current)
  Write-Init034 $current
  [IO.File]::WriteAllBytes((Join-Path $current 'identity.kel'), $kel03)
  $why = @{}
  $log = Save-AnetLog -DataDir $current -Why $why
  Check 'the log goes to logs\ of a data directory of this version' ($log -like ((Join-Path (Join-Path $current 'logs') 'install-') + '*.log') -and ([IO.File]::ReadAllText($log)).Contains('line one of the log')) ($log + ' ' + $why['Text'])
  if (Test-IsWindows) { Check 'the logs directory is private to this user' ($null -eq (Get-AnetPrivacyProblem -Path (Join-Path $current 'logs'))) }
  $log2 = Save-AnetLog -DataDir $current
  Check 'a second log of the same second gets a new name' ($log2 -ne $log -and (Test-Path -LiteralPath $log2))
  $legacyRoot = [IO.Path]::Combine($work, 'home-log-1x', '.anet')
  $null = [IO.Directory]::CreateDirectory($legacyRoot)
  Write-V1Layout $legacyRoot
  $log = Save-AnetLog -DataDir $legacyRoot
  Check 'with data of an earlier anet in the data directory, the log goes to the temporary directory' ($log -like ((Join-Path ([IO.Path]::GetTempPath()) 'anet-install') + '*') -and -not (Test-Path -LiteralPath (Join-Path $legacyRoot 'logs'))) $log
  Remove-Item -LiteralPath $log -Force -ErrorAction SilentlyContinue
  $AnetOut.Lines = $null

  # --- the summary: golden output ----------------------------------------------

  $node = 'bafyrei' + ('n' * 52)
  $ctl = 'bafyrei' + ('c' * 26) + ('7' * 26)
  $exe = 'C:\Users\ink\AppData\Local\Programs\anet\anet.exe'
  $logPath = 'C:\Users\ink\.anet\logs\install-20261020T151644Z.log'
  $saved = $env:ANET_LANG
  $env:ANET_LANG = $null
  $base = @{ Version = '0.3.6'; Running = $true; Aid = $node; Hub = 'https://hub.agentnetwork.org.cn'; JoinState = 'registered'; Error = '';
             Taken = $null; Controller = ''; Notes = @(); Run = 'new'; Exe = $exe }
  function Golden {
    param([hashtable]$Change, [string[]]$Want, [string]$Name)
    $facts = $base.Clone()
    foreach ($k in $Change.Keys) { $facts[$k] = $Change[$k] }
    $got = @(Get-AnetSummary -F $facts) + @(Get-AnetDetailsLine -Log $logPath -Why '')
    Check ('summary: ' + $Name) (($got -join "`n") -ceq ($Want -join "`n")) ("`n" + ($got -join "`n"))
    Check ('summary: ' + $Name + ', at most 10 lines') ($got.Count -le 10)
  }
  Golden -Name 'a fresh installation with -Controller' -Change @{ Controller = $ctl } -Want @(
    'anet 0.3.6 is installed and running.',
    ('  AID   ' + $node),
    '  Hub   https://hub.agentnetwork.org.cn (joined)',
    ('Remote control: allowed for ' + $ctl + ' only; revoke with: anet peers remove ' + $ctl),
    "Run 'anet' in a new window to get started.",
    ('Details: ' + $logPath))
  Golden -Name 'an installation that takes over AgentNetwork 1.1.15' -Change @{ Taken = @{ Label = 'AgentNetwork 1.1.15'; Aside = 'C:\Users\ink\.anet-legacy-1.1.15-20261020T151644Z' } } -Want @(
    'anet 0.3.6 is installed and running.',
    ('  AID   ' + $node),
    '  Hub   https://hub.agentnetwork.org.cn (joined)',
    'Replaced AgentNetwork 1.1.15; its data is kept in C:\Users\ink\.anet-legacy-1.1.15-20261020T151644Z',
    "Run 'anet' in a new window to get started.",
    ('Details: ' + $logPath))
  Golden -Name 'a hub that admits first, anet already on the PATH of this window' -Change @{ JoinState = 'pending_admission'; Run = 'here' } -Want @(
    'anet 0.3.6 is installed and running.',
    ('  AID   ' + $node),
    ('  Hub   https://hub.agentnetwork.org.cn (waiting for admission; the operator runs: anet-hub admit ' + $node + ')'),
    "Run 'anet' to get started.",
    ('Details: ' + $logPath))
  Golden -Name '-NoJoin' -Change @{ Hub = ''; JoinState = '' } -Want @(
    'anet 0.3.6 is installed and running.',
    ('  AID   ' + $node),
    "Run 'anet' in a new window to get started.",
    ('Details: ' + $logPath))
  Golden -Name '-NoStart -NoPath, with an npm package left' -Change @{ Running = $false; Aid = ''; Hub = ''; JoinState = ''; Run = 'path'
      Notes = @((Get-AnetText 'note_nostart'), (Get-AnetText 'note_npm' @{ version = '1.1.15'; command = 'npm uninstall -g @agentnetwork/anet' })) } -Want @(
    'anet 0.3.6 is installed.',
    'Note: the node was not started (-NoStart); start it with: anet up',
    'Note: the npm package @agentnetwork/anet 1.1.15 is still installed; remove it with: npm uninstall -g @agentnetwork/anet',
    ('Run "' + $exe + '" to get started; it is not on the PATH (-NoPath).'),
    ('Details: ' + $logPath))
  Golden -Name 'a hub that refused' -Change @{ Error = (Get-AnetText 'join_refused' @{ hub = 'https://hub.agentnetwork.org.cn'; reason = 'this hub needs anet 0.4.0 or later' }) } -Want @(
    'anet 0.3.6 is installed and running.',
    ('  AID   ' + $node),
    'Error: the hub https://hub.agentnetwork.org.cn refused the registration: this hub needs anet 0.4.0 or later',
    "Run 'anet' in a new window to get started.",
    ('Details: ' + $logPath))
  $env:ANET_LANG = 'zh_CN.UTF-8'
  $zh = @(Get-AnetSummary -F ($base.Clone())) + @(Get-AnetDetailsLine -Log $logPath -Why '')
  $wantZh = @([regex]::Unescape('anet 0.3.6 \u5df2\u5b89\u88c5\u5e76\u6b63\u5728\u8fd0\u884c\u3002'), ('  AID   ' + $node),
              [regex]::Unescape('  Hub   https://hub.agentnetwork.org.cn\uff08\u5df2\u52a0\u5165\uff09'),
              [regex]::Unescape("\u5728\u65b0\u7a97\u53e3\u4e2d\u8fd0\u884c 'anet' \u5f00\u59cb\u4f7f\u7528\u3002"),
              ([regex]::Unescape('\u8be6\u7ec6\u4fe1\u606f\uff1a') + $logPath))
  Check 'summary in Chinese with ANET_LANG=zh_CN.UTF-8' (($zh -join "`n") -ceq ($wantZh -join "`n")) ("`n" + ($zh -join "`n"))
  foreach ($k in @($AnetMessages.Keys)) {
    $text = [regex]::Unescape([string]$AnetMessages[$k][1])
    $names = @([regex]::Matches([string]$AnetMessages[$k][0], '\{[a-z]+\}') | ForEach-Object { $_.Value } | Sort-Object -Unique)
    $zhNames = @([regex]::Matches($text, '\{[a-z]+\}') | ForEach-Object { $_.Value } | Sort-Object -Unique)
    Check ('message ' + $k + ': the Chinese text has the same arguments') (($names -join ',') -ceq ($zhNames -join ',')) (($names -join ',') + ' / ' + ($zhNames -join ','))
  }
  $env:ANET_LANG = $saved
  Check 'sizes and times of the download line' ((Format-AnetMB 9437184) -eq '9.0' -and (Format-AnetMB 0) -eq '0.0' -and (Format-AnetDuration 125) -eq '2:05' -and (Format-AnetDuration -1) -eq '-:--')
  $p = New-AnetProgress -Label 'anet 9.8.7 from x' -Total 100 -Show
  Update-AnetProgress -Progress $p -Done 50
  Check 'a download line is not drawn without an interactive console' (-not $p.Interactive -and $p.Done -eq 50 -and -not $p.Open)

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
      @{ Name = '(OI)(CI)M for a local group (CodexSandboxUsers)'; Sid = 'S-1-5-21-1000-2000-3000-1005'; Propagation = 'None'; Private = $false; Why = '*S-1-5-21-1000-2000-3000-1005 (unresolved)*read, write, delete*' },
      @{ Name = '(OI)(CI)F for an app container capability'; Grant = '*S-1-15-3-2968813833-1-2-3:(OI)(CI)F'; Private = $false; Why = '*full control*' },
      @{ Name = 'inherit-only (OI)(CI)(IO)M for a local group'; Sid = 'S-1-5-21-1000-2000-3000-1005'; Propagation = 'InheritOnly'; Private = $false; Why = '*S-1-5-21-1000-2000-3000-1005*' },
      @{ Name = '(OI)(CI)(RX) for Users'; Grant = '*S-1-5-32-545:(OI)(CI)RX'; Private = $false; Why = '*Users*read*' }
    )
    $n = 0
    foreach ($shape in $shapes) {
      $n++
      $sd = Join-Path $work ('shape' + $n)
      $null = [IO.Directory]::CreateDirectory($sd)
      Set-AnetPrivateAcl -Path $sd
      if ($shape.ContainsKey('Sid')) {
        # icacls refuses a SID that no account maps to (as on a PC whose
        # local group is not on this runner); .NET writes it as it is.
        $ok = $true
        try {
          $acl = Get-Acl -LiteralPath $sd
          $rule = New-Object System.Security.AccessControl.FileSystemAccessRule -ArgumentList (New-Object System.Security.Principal.SecurityIdentifier -ArgumentList $shape.Sid),
            ([System.Security.AccessControl.FileSystemRights]::Modify),
            ([System.Security.AccessControl.InheritanceFlags]'ContainerInherit, ObjectInherit'),
            ([System.Security.AccessControl.PropagationFlags]$shape.Propagation),
            ([System.Security.AccessControl.AccessControlType]::Allow)
          $acl.AddAccessRule($rule)
          Set-Acl -LiteralPath $sd -AclObject $acl
        } catch {
          $ok = $false
        }
        Check ('an access rule grants ' + $shape.Name) $ok
      } else {
        $r = @{}
        Invoke-AnetNative -FilePath $icacls -ArgumentList @($sd, '/grant', $shape.Grant) -Capture -Result $r
        Check ('icacls grants ' + $shape.Name) ($r['Code'] -eq 0)
      }
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
