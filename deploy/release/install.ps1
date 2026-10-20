# AgentNetwork (anet) installer for Windows - https://agentnetwork.org.cn
#
# Installs the prebuilt anet.exe for this machine (Windows 10 1809 or
# Windows Server 2019 and later, x64 or arm64) into
# %LOCALAPPDATA%\Programs\anet, after checking it. Runs in Windows
# PowerShell 5.1 and in PowerShell 7, in a window that is not elevated.
#
# Usage (one line):
#   [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor 3072; & ([scriptblock]::Create((Invoke-RestMethod -UseBasicParsing -Uri 'https://agentnetwork.org.cn/install.ps1'))) -Controller '<controller AID>'
#
# Without -Controller, the same line with no arguments installs the default
# build and does nothing else.
#
# Parameters:
#   -Controller AID  Install the shell build, give the agent with this AID the
#                    right to run commands on this machine as this user (one
#                    confirmation on the console), start the node, register it
#                    at the hub and start it at every logon.
#   -Shell           Install the shell build without giving anyone access.
#   -Hub URL         Start the node and join this hub (default with
#                    -Controller: https://hub.agentnetwork.org.cn).
#   -Name NAME       The name to join under (default: the computer name).
#   -Prefix DIR      Install into DIR (default %LOCALAPPDATA%\Programs\anet).
#                    No directory above DIR may let another account rename
#                    it or change its access control list; DIR must not
#                    contain ; or %.
#   -Without LIST    Turn these modules off in config.json.
#   -Base URL|DIR    Download base, https only, or a local copy of a release
#                    directory; replaces the list of download hosts.
#   -NoAutostart     Do not start the node at logon.
#   -NoPath          Do not add the install directory to the user's PATH.
#   -Elevated        Allow running in an elevated window.
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
# Verify-first form (ssh-keygen -Y verify needs OpenSSH 8.1 or later; ssh -V
# shows the version, and the check can run on another machine):
#
#   Invoke-WebRequest -UseBasicParsing https://agentnetwork.org.cn/install.ps1 -OutFile install.ps1
#   Invoke-WebRequest -UseBasicParsing https://agentnetwork.org.cn/install.ps1.sig -OutFile install.ps1.sig
#   # write the line above into a file named allowed_signers, then:
#   #   ssh-keygen -Y verify -f allowed_signers -I anet-release@agentnetwork.org.cn -n anet-release@agentnetwork.org.cn -s install.ps1.sig < install.ps1
#   & ([scriptblock]::Create((Get-Content -Raw .\install.ps1))) -Controller '<AID>'
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
# registration; the Run value AgentNetwork under
# HKCU\Software\Microsoft\Windows\CurrentVersion\Run; the user's PATH. Nothing
# else: no auto-reply, no other peer, no public capability, no listener beyond
# the node's loopback ones.
#
# Everything below is a parameter block, the release pins and function
# definitions; the last line calls Install-Anet. A download cut short does
# not parse, and runs nothing. This script contains no call to exit: the
# window it runs in stays open, and $LASTEXITCODE holds the result.

param(
  [string]$Controller, [switch]$Shell, [string]$Hub, [string]$Name, [string]$Prefix,
  [string[]]$Without, [string]$Base, [switch]$NoAutostart, [switch]$NoPath, [switch]$Elevated
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

# Invoke-AnetNative is the one place this script runs a program: anet.exe and
# icacls.exe. The arguments are an array (no command line is assembled from
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

# Get-AnetOptions validates the parameters (a hashtable of what was given)
# and returns what the install will do. Nothing is read from or written to
# the machine except $env:COMPUTERNAME, ANET_INSTALL_BASE and, for -Base and
# -Prefix, whether a directory exists.
function Get-AnetOptions {
  param([Parameter(Mandatory = $true)][hashtable]$Given)
  $o = @{
    Controller = ''; Variant = 'default'; AssetPrefix = 'anet'; Hub = ''; Join = $false; Name = '';
    Prefix = ''; PrefixGiven = $false; Without = @(); Base = ''; NoAutostart = [bool]$Given['NoAutostart'];
    NoPath = [bool]$Given['NoPath']; Elevated = [bool]$Given['Elevated']
  }
  $controller = [string]$Given['Controller']
  if ($Given.ContainsKey('Controller')) {
    if (-not ($controller -cmatch '^bafyrei[a-z2-7]{52}$')) {
      throw (New-AnetFailure ('-Controller ' + $controller + ' is not an AID (bafyrei followed by 52 characters a-z and 2-7)'))
    }
    $o.Controller = $controller
  }
  if ($o.Controller -ne '' -or [bool]$Given['Shell']) {
    $o.Variant = 'shell'
    $o.AssetPrefix = 'anet-shell'
  }
  if ($Given.ContainsKey('Hub')) {
    $hub = [string]$Given['Hub']
    if (-not (Test-AnetHttpsUrl -Text $hub -AllowLoopbackHttp)) {
      throw (New-AnetFailure ('-Hub ' + $hub + ' is not an absolute https:// URL (http:// only for 127.0.0.1 or localhost)'))
    }
    $o.Hub = $hub
  } elseif ($o.Controller -ne '') {
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
  } else {
    $local = [Environment]::GetFolderPath('LocalApplicationData')
    if ([string]::IsNullOrEmpty($local)) {
      throw (New-AnetFailure 'this account has no local application data folder; give -Prefix')
    }
    $o.Prefix = Join-Path (Join-Path $local 'Programs') 'anet'
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

# Get-AnetPrivacyProblem says why a directory is not private to this user,
# or returns $null when it is: it is not a reparse point, its owner is this
# user, SYSTEM or Administrators, and every Allow rule names one of them.
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
  if ($allowed -notcontains $owner) { return 'it is owned by ' + $owner }
  foreach ($r in $rules) {
    if ($r.AccessControlType -ne [System.Security.AccessControl.AccessControlType]::Allow) { continue }
    $sid = $r.IdentityReference.Value
    if ($allowed -notcontains $sid) { return 'access is granted to ' + $sid }
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
      if ($trusted -notcontains $owner) { return $ancestor + ': it is owned by ' + $owner }
      foreach ($r in $rules) {
        if ($r.AccessControlType -ne [System.Security.AccessControl.AccessControlType]::Allow) { continue }
        if (($r.PropagationFlags -band [System.Security.AccessControl.PropagationFlags]::InheritOnly) -ne 0) { continue }
        $sid = $r.IdentityReference.Value
        if ($trusted -contains $sid -or $sid -eq 'S-1-3-0' -or $sid -eq 'S-1-3-4') { continue }
        if (([long][int]$r.FileSystemRights -band $moving) -ne 0) {
          return $ancestor + ': ' + $sid + ' may rename it or change its access control list'
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
# Administrators, inherited by everything created inside.
function Set-AnetPrivateAcl {
  param([Parameter(Mandatory = $true)][string]$Path)
  $user = [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value
  $icacls = Join-Path ([Environment]::SystemDirectory) 'icacls.exe'
  $r = @{}
  Invoke-AnetNative -FilePath $icacls -Capture -Result $r -ArgumentList @(
    $Path, '/inheritance:r', '/grant:r',
    ('*{0}:(OI)(CI)F' -f $user), '*S-1-5-18:(OI)(CI)F', '*S-1-5-32-544:(OI)(CI)F')
  if ($r['Code'] -ne 0) {
    throw (New-AnetFailure ('icacls could not set the access control list of ' + $Path + ' (exit status ' + $r['Code'] + ')'))
  }
}

# Initialize-AnetDirectory makes sure the install directory exists and is
# private to this user. A missing directory is created only below an
# existing directory that is private to this user; the new directory then
# gets its own access control list, and must be empty. With
# -CheckAncestors (a directory given with -Prefix), no directory above it
# may let another account move it away (Get-AnetAncestorProblem).
function Initialize-AnetDirectory {
  param([Parameter(Mandatory = $true)][string]$Dir, [switch]$CheckAncestors)
  $refusal = $Dir + ' is not private to this user; choose -Prefix inside your profile'
  if ($CheckAncestors) {
    $why = Get-AnetAncestorProblem -Dir $Dir
    if ($null -ne $why) {
      Write-AnetLine ('   ' + $why)
      throw (New-AnetFailure $refusal)
    }
  }
  if (Test-Path -LiteralPath $Dir) {
    $why = Get-AnetPrivacyProblem -Path $Dir
    if ($null -ne $why) {
      Write-AnetLine ('   ' + $Dir + ': ' + $why)
      throw (New-AnetFailure $refusal)
    }
    return
  }
  $ancestor = Split-Path -Parent $Dir
  while (-not [string]::IsNullOrEmpty($ancestor) -and -not (Test-Path -LiteralPath $ancestor)) {
    $ancestor = Split-Path -Parent $ancestor
  }
  if ([string]::IsNullOrEmpty($ancestor)) {
    throw (New-AnetFailure ($Dir + ' cannot be created: no part of the path exists'))
  }
  $why = Get-AnetPrivacyProblem -Path $ancestor
  if ($null -ne $why) {
    Write-AnetLine ('   ' + $ancestor + ': ' + $why)
    throw (New-AnetFailure $refusal)
  }
  Write-AnetLine ('-> creating ' + $Dir)
  $null = [IO.Directory]::CreateDirectory($Dir)
  Set-AnetPrivateAcl -Path $Dir
  $why = Get-AnetPrivacyProblem -Path $Dir
  if ($null -ne $why) {
    Write-AnetLine ('   ' + $Dir + ': ' + $why)
    throw (New-AnetFailure $refusal)
  }
  if (@(Get-ChildItem -LiteralPath $Dir -Force).Count -ne 0) {
    Write-AnetLine ('   ' + $Dir + ': it was created by this installer and is not empty')
    throw (New-AnetFailure $refusal)
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
# most two seconds while another process holds one of them.
function Move-AnetFile {
  param([string]$From, [string]$To)
  $deadline = [DateTime]::UtcNow.AddSeconds(2)
  $wait = 10
  while ($true) {
    try {
      [IO.File]::Move($From, $To)
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

# Get-AnetInstalledVersion is the version the anet.exe at $Exe reports, or ''.
function Get-AnetInstalledVersion {
  param([string]$Exe)
  if (-not (Test-Path -LiteralPath $Exe -PathType Leaf)) { return '' }
  $r = @{}
  Invoke-AnetNative -FilePath $Exe -ArgumentList @('version') -Capture -Result $r
  $lines = @($r['Output'])
  if ($r['Code'] -eq 0 -and $lines.Count -gt 0 -and ([string]$lines[0]) -cmatch '^anet (\S+) ') {
    $v = $Matches[1]
    if ($v -cmatch '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$') { return $v }
  }
  Write-AnetLine ('Warning: cannot read the version of the existing ' + $Exe + '; it will be replaced')
  return ''
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

# Add-AnetToPath appends the install directory to the user's PATH in the
# registry, keeping the value's kind, tells running programs that the
# environment changed, and puts the directory first in this session's PATH.
function Add-AnetToPath {
  param([string]$Dir)
  $key = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey('Environment')
  try {
    $raw = [string]$key.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
    $kind = [Microsoft.Win32.RegistryValueKind]::String
    if (@($key.GetValueNames()) -contains 'Path') { $kind = $key.GetValueKind('Path') }
    $present = $false
    foreach ($e in $raw.Split(';')) {
      if ($e.Trim().TrimEnd('\') -ieq $Dir.TrimEnd('\')) { $present = $true }
    }
    if ($present) {
      Write-AnetLine ('OK ' + $Dir + ' is already on your PATH')
    } else {
      $new = $Dir
      if ($raw.Trim() -ne '') { $new = $raw.TrimEnd(';') + ';' + $Dir }
      if ($kind -ne [Microsoft.Win32.RegistryValueKind]::ExpandString -and $new.Contains('%')) {
        $kind = [Microsoft.Win32.RegistryValueKind]::ExpandString
      }
      $key.SetValue('Path', $new, $kind)
      [Environment]::SetEnvironmentVariable('ANET_INSTALL_PATH_REFRESH', $null, 'User')
      Write-AnetLine ('-> added ' + $Dir + ' to your PATH (new windows see it)')
    }
  } finally {
    $key.Close()
  }
  $here = $false
  foreach ($e in ([string]$env:Path).Split(';')) {
    if ($e.Trim().TrimEnd('\') -ieq $Dir.TrimEnd('\')) { $here = $true }
  }
  if (-not $here) { $env:Path = $Dir + ';' + $env:Path }
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

# Write-AnetInstalledFailure ends a failure after the binary is installed:
# what is installed, and what to run by hand.
function Write-AnetInstalledFailure {
  param([hashtable]$State, [string]$Message, [string]$Next)
  Write-AnetLine ('Error: ' + $Message)
  Write-AnetLine ('anet ' + $AnetRelease['Version'] + ' is installed at ' + $State.Exe + '.')
  if ($Next -ne '') { Write-AnetLine ('Run by hand: ' + $Next) }
}

function Install-Anet {
  param(
    [string]$Controller, [switch]$Shell, [string]$Hub, [string]$Name, [string]$Prefix,
    [string[]]$Without, [string]$Base, [switch]$NoAutostart, [switch]$NoPath, [switch]$Elevated
  )
  $previousEAP = $ErrorActionPreference
  $previousProgress = $ProgressPreference
  Set-StrictMode -Version 3
  $ErrorActionPreference = 'Stop'
  $ProgressPreference = 'SilentlyContinue'
  $given = @{}
  foreach ($k in $PSBoundParameters.Keys) { $given[$k] = $PSBoundParameters[$k] }
  $S = @{ Installed = $false; Exe = ''; Tmp = ''; Staged = '' }
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
    $principal = New-Object System.Security.Principal.WindowsPrincipal ([System.Security.Principal.WindowsIdentity]::GetCurrent())
    if ($principal.IsInRole([System.Security.Principal.WindowsBuiltInRole]::Administrator) -and -not $Elevated) {
      throw (New-AnetFailure 'Run this in a PowerShell window that is not elevated; the node and the commands it runs for the controller run as the user that installs it. To install for an administrator account anyway, add -Elevated.')
    }

    # 2. Parameters.
    $o = Get-AnetOptions -Given $given
    $asset = $o.AssetPrefix + '-windows-' + $arch
    $pin = $AnetRelease['Assets'][$asset]
    $S.Exe = Join-Path $o.Prefix 'anet.exe'
    Write-AnetLine ('-> Installing anet ' + $AnetRelease['Version'] + ' (' + $o.Variant + ' build, windows-' + $arch + ') into ' + $o.Prefix)
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

    # 5. Install directory.
    Initialize-AnetDirectory -Dir $o.Prefix -CheckAncestors:$o.PrefixGiven

    # 6. Stage and verify.
    $S.Staged = Get-AnetStagedPath -Dir $o.Prefix
    Write-AnetStaged -Path $S.Staged -Bytes $rel.Binary
    $installed = Get-AnetInstalledVersion -Exe $S.Exe
    $mods = Confirm-AnetStaged -Staged $S.Staged -TmpDir $S.Tmp -Installed $installed -Variant $o.Variant -Asset $asset
    if ($installed -ne '') {
      Write-AnetLine ('   ' + $S.Exe + ': ' + $installed + ' -> ' + $AnetRelease['Version'])
    }

    # 7. -Without: the names are checked against the verified module set
    # here, and config.json is changed only after the binary is installed
    # (step 8), so that a failure before then leaves the machine unchanged.
    foreach ($m in @($o.Without)) {
      if ($m -ne 'shell' -and @($mods) -notcontains $m) {
        throw (New-AnetFailure ('-Without ' + $m + ': the ' + $o.Variant + ' build has no module of that name (its modules: ' + (@($mods) -join ',') + ')'))
      }
    }

    # 8. Replace.
    $aside = Install-AnetStaged -Staged $S.Staged -Exe $S.Exe
    $S.Staged = ''
    $S.Installed = $true
    Remove-AnetAsides -Dir $o.Prefix -Keep $aside
    Save-AnetRecord -Exe $S.Exe -TmpDir $S.Tmp
    Write-AnetLine ''
    Write-AnetLine ('OK Installed anet ' + $AnetRelease['Version'] + ' -> ' + $S.Exe)
    if ($aside -ne '') {
      Write-AnetLine ('   The previous binary is ' + $aside + ' until it is no longer in use; a node still running it keeps it until restarted.')
    }
    if (@($o.Without).Count -gt 0) {
      $list = $o.Without -join ','
      Write-AnetLine ('-> anet init --without ' + $list)
      $r = @{}
      Invoke-AnetNative -FilePath $S.Exe -ArgumentList @('init', '--without', $list) -Result $r
      if ($r['Code'] -ne 0) {
        Write-AnetInstalledFailure -State $S -Message ('anet init --without ' + $list + ' refused (exit status ' + $r['Code'] + '); its reason is above') -Next ('anet init --without ' + $list)
        $global:LASTEXITCODE = $r['Code']
        return
      }
    }

    # 9. PATH.
    if (-not $o.NoPath) { Add-AnetToPath -Dir $o.Prefix }

    # 10. Defaults.
    Write-AnetLine ''
    Write-AnetLine '-> anet init (explicit safe defaults: inbound closed, no public capabilities, spending limits 0)'
    $r = @{}
    Invoke-AnetNative -FilePath $S.Exe -ArgumentList @('init') -Result $r
    if ($r['Code'] -ne 0) {
      Write-AnetInstalledFailure -State $S -Message ('anet init did not complete (exit status ' + $r['Code'] + ')') -Next 'anet init'
      $global:LASTEXITCODE = $r['Code']
      return
    }

    # 11. Controller.
    $stopped = $false
    if ($o.Controller -ne '') {
      $r = @{}
      Invoke-AnetNative -FilePath $S.Exe -ArgumentList @('status') -Capture -Result $r
      if ($r['Code'] -eq 0) {
        Write-AnetLine '-> stopping the running node to change its configuration'
        $r = @{}
        Invoke-AnetNative -FilePath $S.Exe -ArgumentList @('stop') -Result $r
        if ($r['Code'] -ne 0) {
          Write-AnetInstalledFailure -State $S -Message ('anet stop did not complete (exit status ' + $r['Code'] + ')') -Next ('anet stop, then run this installer again')
          $global:LASTEXITCODE = $r['Code']
          return
        }
        $stopped = $true
      }
      Write-AnetLine ('-> anet init --shell-controller ' + $o.Controller)
      $r = @{}
      Invoke-AnetNative -FilePath $S.Exe -ArgumentList @('init', '--shell-controller', $o.Controller) -Result $r
      if ($r['Code'] -ne 0) {
        if ($r['Code'] -eq 1) {
          Write-AnetLine ('The controller was not given access; nothing was opened. anet ' + $AnetRelease['Version'] + ' is installed.')
        } else {
          Write-AnetInstalledFailure -State $S -Message ('anet init --shell-controller refused (exit status ' + $r['Code'] + '); its reason is above') -Next ''
        }
        if ($stopped) { Write-AnetLine 'The node was stopped to change its configuration; start it again with: anet up' }
        $global:LASTEXITCODE = $r['Code']
        return
      }
    }

    # 12. Start.
    $started = $false
    if ($o.Join -or $o.Controller -ne '') {
      Write-AnetLine ''
      Write-AnetLine '-> Starting the node'
      $r = @{}
      Invoke-AnetNative -FilePath $S.Exe -ArgumentList @('up') -Result $r
      if ($r['Code'] -ne 0) {
        Write-AnetInstalledFailure -State $S -Message ('the node did not start (exit status ' + $r['Code'] + ')') -Next 'anet up'
        $global:LASTEXITCODE = $r['Code']
        return
      }
      $started = $true
      for ($i = 0; $i -lt 20; $i++) {
        $r = @{}
        Invoke-AnetNative -FilePath $S.Exe -ArgumentList @('status') -Capture -Result $r
        if ($r['Code'] -eq 0) { break }
        Start-Sleep -Seconds 1
      }
    }

    # 13. Autostart.
    if ($started -and -not $o.NoAutostart) { Set-AnetAutostart -Exe $S.Exe }

    # 14. Report.
    Write-AnetLine ''
    $r = @{}
    Invoke-AnetNative -FilePath $S.Exe -ArgumentList @('doctor') -Result $r
    Write-AnetLine ''
    Write-AnetLine 'Next:'
    if (-not $o.Join) {
      Write-AnetLine '  anet up                      start the node in the background'
      Write-AnetLine '  anet hub-register https://hub.agentnetwork.org.cn --name <name>'
    }
    Write-AnetLine '  anet status                  this node''s identity (AID), data directory and console address'
    Write-AnetLine '  anet doctor                  check again later: signature, hub, what this node lets in'
    Write-AnetLine '  anet update                  update in place, checked against the release key'
    if ($o.Controller -ne '') {
      Write-AnetLine ''
      Write-AnetLine ('The agent ' + $o.Controller + ' can now run any command on this machine as this user.')
      Write-AnetLine ('  anet peers remove ' + $o.Controller + '   revokes it; remove the modules.shell block of config.json as well')
      Write-AnetLine '  anet audit                   every command it ran, and every refusal, is on the evidence chain'
      Write-AnetLine '  anet identity protect        set a recovery passphrase for this node''s identity'
    }
    if ($o.NoPath) {
      Write-AnetLine ''
      Write-AnetLine ('  ' + $o.Prefix + ' is not on your PATH (-NoPath); run anet as "' + $S.Exe + '"')
    }

    # 15. Join, last: its lines as they are, and its exit status.
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
      Write-AnetInstalledFailure -State $S -Message $e.Message -Next ''
    } else {
      Write-AnetLine ('Error: ' + $e.Message)
      Write-AnetLine 'Nothing was installed or changed.'
    }
    $global:LASTEXITCODE = $code
  } finally {
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

if ($env:ANET_INSTALL_PS1_TEST -ne '1') { Install-Anet @PSBoundParameters }
