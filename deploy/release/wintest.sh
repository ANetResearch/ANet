#!/usr/bin/env bash
# deploy/release/wintest.sh - build the Windows test binaries of anet for a
# Windows PC, and the script that runs them there.
#
#   deploy/release/wintest.sh <amd64|arm64> <outdir>
#
# Cross-compiles, with GOOS=windows CGO_ENABLED=0 `go test -c`, one test
# binary per package that has Windows-specific behaviour:
#
#   fsperm.test.exe      ./internal/fsperm
#   procgroup.test.exe   ./internal/procgroup
#   pathsafe.test.exe    ./internal/pathsafe
#   anethome.test.exe    ./internal/anethome
#   loopguard.test.exe   ./internal/loopguard
#   localpeer.test.exe   ./internal/localpeer
#   agentwire.test.exe   ./internal/agentwire
#   backendconn.test.exe ./internal/backendconn
#   daemon.test.exe      ./internal/daemon
#   autoreply.test.exe   ./module/autoreply
#   p2p.test.exe         ./module/p2p
#   service.test.exe     ./module/service
#   shell.test.exe       ./module/shell           (-tags shell)
#   release.test.exe     ./internal/release
#   anet.test.exe        ./cmd/anet
#   anet-shell.test.exe  ./cmd/anet               (-tags shell)
#   official.test.exe    ./cmd/anet-official
#
# and writes <outdir>/run-tests.ps1, which runs each of them as
# `<binary> -test.run Windows -test.v -test.count 1` in a fresh directory
# under %TEMP%, prints a PASS/FAIL table and then the manual checks of the
# Windows verification plan. Copy <outdir> to the PC and run, once in a normal
# and once in an elevated window:
#
#   powershell -NoProfile -ExecutionPolicy Bypass -File .\run-tests.ps1
#
# The tests are self-contained: they use temporary directories only, need no
# network and no repository files. Tests that need a privilege the window
# does not have (creating symbolic links) skip and say so.
set -euo pipefail

die() { printf 'wintest.sh: %s\n' "$*" >&2; exit 1; }

[ $# -eq 2 ] || die "usage: wintest.sh <amd64|arm64> <outdir>"
ARCH="$1"
case "$ARCH" in amd64|arm64) ;; *) die "architecture '$ARCH': amd64 or arm64" ;; esac
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
mkdir -p "$2"
OUT="$(cd "$2" && pwd)"

# name|tags|package, in the order run-tests.ps1 runs them.
TESTS="fsperm||./internal/fsperm
procgroup||./internal/procgroup
pathsafe||./internal/pathsafe
anethome||./internal/anethome
loopguard||./internal/loopguard
localpeer||./internal/localpeer
agentwire||./internal/agentwire
backendconn||./internal/backendconn
daemon||./internal/daemon
autoreply||./module/autoreply
p2p||./module/p2p
service||./module/service
shell|shell|./module/shell
release||./internal/release
anet||./cmd/anet
anet-shell|shell|./cmd/anet
official||./cmd/anet-official"

NAMES=""
while IFS='|' read -r name tags pkg; do
  printf '== %s.test.exe (%s%s)\n' "$name" "$pkg" "${tags:+, -tags $tags}"
  (cd "$ROOT" && CGO_ENABLED=0 GOOS=windows GOARCH="$ARCH" go test -c -tags "$tags" -o "$OUT/$name.test.exe" "$pkg") \
    || die "building $name.test.exe failed"
  NAMES="$NAMES'$name.test.exe', "
done <<EOF
$TESTS
EOF
NAMES="${NAMES%, }"

# run-tests.ps1 is ASCII, and runs in Windows PowerShell 5.1 and PowerShell 7.
cat > "$OUT/run-tests.ps1" <<EOF
# run-tests.ps1 - runs the Windows test binaries of anet that
# deploy/release/wintest.sh built (windows/$ARCH), and lists the manual checks.
#
#   powershell -NoProfile -ExecutionPolicy Bypass -File .\\run-tests.ps1
#
# Run it once in a normal window and once in an elevated window: the tests that
# need the privilege to create symbolic links skip without it; the junction
# tests always run.

\$ErrorActionPreference = 'Stop'
\$here = Split-Path -Parent \$MyInvocation.MyCommand.Path
\$binaries = @($NAMES)
\$principal = New-Object System.Security.Principal.WindowsPrincipal ([System.Security.Principal.WindowsIdentity]::GetCurrent())
\$elevated = \$principal.IsInRole([System.Security.Principal.WindowsBuiltInRole]::Administrator)
Write-Host ('Windows ' + [Environment]::OSVersion.Version + ', PowerShell ' + \$PSVersionTable.PSVersion + ', ' + \$env:PROCESSOR_ARCHITECTURE + ', elevated: ' + \$elevated)
\$results = @()
foreach (\$b in \$binaries) {
  \$exe = Join-Path \$here \$b
  \$work = Join-Path \$env:TEMP ('anet-wintest-' + [Guid]::NewGuid().ToString('N'))
  [void][IO.Directory]::CreateDirectory(\$work)
  Write-Host ''
  Write-Host ('== ' + \$b)
  Push-Location -LiteralPath \$work
  \$code = -1
  try {
    \$ErrorActionPreference = 'Continue'
    \$global:LASTEXITCODE = 0
    & \$exe -test.run Windows -test.v -test.count 1
    \$code = \$global:LASTEXITCODE
  } catch {
    Write-Host ('cannot run ' + \$exe + ': ' + \$_.Exception.Message)
  } finally {
    \$ErrorActionPreference = 'Stop'
    Pop-Location
  }
  Remove-Item -LiteralPath \$work -Recurse -Force -ErrorAction SilentlyContinue
  \$state = 'FAIL'
  if (\$code -eq 0) { \$state = 'PASS' }
  \$results += New-Object PSObject -Property ([ordered]@{ Binary = \$b; Exit = \$code; Result = \$state })
}
Write-Host ''
\$results | Format-Table -AutoSize | Out-String | Write-Host
\$failed = @(\$results | Where-Object { \$_.Result -ne 'PASS' }).Count
Write-Host ('{0} of {1} test binaries passed (elevated: {2}).' -f (\$results.Count - \$failed), \$results.Count, \$elevated)

Write-Host @'

MANUAL CHECKS (Windows verification plan, end-to-end on this PC, in order)

Before publication, build a signed release candidate on Linux
(ANET_RELEASE_KEY=... deploy/release/build-release.sh), copy dist\ to this PC and run:
  & ([scriptblock]::Create((Get-Content -Raw .\dist\install.ps1))) -Base .\dist -Controller '<AID>'
After publication, run the one-line command:
  [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor 3072; & ([scriptblock]::Create((Invoke-RestMethod -UseBasicParsing -Uri 'https://agentnetwork.org.cn/install.ps1'))) -Controller '<AID>'

 1. Record [Environment]::OSVersion, \$PSVersionTable.PSVersion, ssh -V (informational),
    the architecture, and the Smart App Control state.
 2. Exactly one question is asked (the controller confirmation). Answer no once: the
    output says nothing was opened, config.json has no modules.shell, and peers.allow
    lacks the AID. Run again and answer yes.
 3. icacls \$env:USERPROFILE\.anet, icacls \$env:LOCALAPPDATA\anet\run and
    icacls "\$env:LOCALAPPDATA\Programs\anet" show only the user, SYSTEM and
    Administrators, with no inherited extra entries.
 4. anet doctor reports no FAIL; release signature verified; files private.
 5. From the controller node: shell.exec with whoami returns this user; with
    Write-Output ([string][char]0xE9 + [char]0x4E2D + [char]0x6587) the output is the same
    three characters in correct UTF-8; with exit 3 the result is FAILED exit 3.
 6. From a second node (AID not allowed): shell.exec is refused, and anet audit shows
    anet.shell.refused.
 7. Timeout: timeout_s 5 and the command Start-Process notepad; Start-Sleep 60 gives
    FAILED "killed after 5s", and no notepad remains (Get-Process notepad is empty).
    Normal completion: Start-Process notepad alone returns OK and notepad stays open.
 8. Close the PowerShell window: the node stays up (anet status in a new window).
 9. anet update while the daemon runs (after publishing a newer test release, or with
    --base pointing at a second release): anet.exe is new, anet.exe.old-* exists,
    anet stop then anet up runs the new version, and the next anet update removes
    the aside file.
10. Log off and on: the node is running again (Run value AgentNetwork); no console
    window remains visible.
11. Second user (create a standard user anettest, then runas /user:anettest powershell):
    Get-Content C:\Users\<owner>\.anet\control_token.txt is denied;
    Invoke-WebRequest http://127.0.0.1:<control port>/status answers 401; and binding
    the control port with SO_REUSEADDR from that account fails:
      \$s = New-Object System.Net.Sockets.Socket ([System.Net.Sockets.AddressFamily]::InterNetwork), ([System.Net.Sockets.SocketType]::Stream), ([System.Net.Sockets.ProtocolType]::Tcp)
      \$s.SetSocketOption([System.Net.Sockets.SocketOptionLevel]::Socket, [System.Net.Sockets.SocketOptionName]::ReuseAddress, \$true)
      \$s.Bind((New-Object System.Net.IPEndPoint ([System.Net.IPAddress]::Parse('127.0.0.1')), <control port>))
    The Bind call must fail with an access error (WSAEACCES); then \$s.Close().
12. Run the one-line command again (re-install) with the daemon running: no question is
    asked (unchanged), the node is restarted, and the version is the same or newer.
'@

if (\$failed -ne 0) { exit 1 }
exit 0
EOF

if LC_ALL=C grep -q '[^ -~]' "$OUT/run-tests.ps1"; then
  die "run-tests.ps1 is not ASCII"
fi
printf 'wrote %s (%d test binaries and run-tests.ps1)\n' "$OUT" "$(printf '%s\n' "$TESTS" | wc -l)"
