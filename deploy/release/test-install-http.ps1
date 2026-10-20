# deploy/release/test-install-http.ps1 -- the download path of install.ps1
# over the real network, in the PowerShell that runs it (Windows PowerShell
# 5.1 or PowerShell 7). test-install.ps1 replaces Get-AnetHttpBytes; this
# script calls it as installations do: the GitHub release through its
# redirects to *.githubusercontent.com, and the npm tarball of the build on
# npmmirror read with Read-AnetTarball. Both bodies are compared with the
# published checksums.txt of the release named by -Version. CI runs it on
# windows-latest; it needs network access and nothing else.

param([string]$Version = '0.3.5')

$ErrorActionPreference = 'Stop'
$here = Split-Path -Parent $MyInvocation.MyCommand.Path
$env:ANET_INSTALL_PS1_TEST = '1'
. ([scriptblock]::Create([IO.File]::ReadAllText((Join-Path $here 'install.ps1'))))

if ($PSVersionTable.PSEdition -ne 'Core') {
  # As Install-Anet does in Windows PowerShell 5.1.
  [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor 3072
}

$failed = 0
function Check {
  param([string]$Name, [bool]$Ok, [string]$Detail = '')
  if ($Ok) { Write-Output ('ok    ' + $Name) } else { Write-Output ('FAIL  ' + $Name + ': ' + $Detail); $script:failed++ }
}
function Get-Sha256Hex {
  param([byte[]]$Bytes)
  $h = [System.Security.Cryptography.SHA256]::Create()
  try { return (($h.ComputeHash($Bytes) | ForEach-Object { $_.ToString('x2') }) -join '') } finally { $h.Dispose() }
}

$gh = 'https://github.com/ANetResearch/ANet/releases/download/v' + $Version + '/'
$asset = 'anet-windows-amd64.gz'
$sums = [Text.Encoding]::ASCII.GetString((Get-AnetHttpBytes -Url ($gh + 'checksums.txt') -Limit 65536 -TimeoutSeconds 30 -Redirect github))
$want = ''
foreach ($l in ($sums -split "`n")) {
  $f = $l.Trim() -split '\s+'
  if ($f.Count -eq 2 -and $f[1].TrimStart('*') -eq $asset) { $want = $f[0].ToLowerInvariant() }
}
Check 'checksums.txt of the release names the asset' ($want -ne '') $sums

$sw = [Diagnostics.Stopwatch]::StartNew()
$gz = Get-AnetHttpBytes -Url ($gh + $asset) -Limit 67108864 -TimeoutSeconds 30 -Redirect github
Check ('GitHub, through its redirects: ' + $gz.Length + ' bytes in ' + [int]$sw.Elapsed.TotalSeconds + ' s match checksums.txt') ((Get-Sha256Hex $gz) -eq $want) (Get-Sha256Hex $gz)

$sw = [Diagnostics.Stopwatch]::StartNew()
$tgzUrl = 'https://registry.npmmirror.com/@agentnetwork/anet-win32-x64/-/anet-win32-x64-' + $Version + '.tgz'
$tgz = Get-AnetHttpBytes -Url $tgzUrl -Limit 67108864 -TimeoutSeconds 30
$files = Read-AnetTarball -Tgz $tgz -Want @(('package/dl/' + $asset)) -Limit 67108864
$fromNpm = $files[('package/dl/' + $asset)]
Check ('npmmirror tarball: ' + $tgz.Length + ' bytes in ' + [int]$sw.Elapsed.TotalSeconds + ' s, its ' + $asset + ' matches checksums.txt') ($null -ne $fromNpm -and (Get-Sha256Hex $fromNpm) -eq $want)

Write-Output ''
Write-Output ('{0} failed' -f $failed)
if ($failed -ne 0) { exit 1 }
exit 0
