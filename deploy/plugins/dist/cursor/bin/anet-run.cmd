@echo off
rem Runs the anet executable that the plugin settings select, for the identity
rem that they select: "anet-run.cmd <arguments>" is "anet [--id <name>]
rem <arguments>". The Windows counterpart of anet-run.sh; the selection is made
rem by anet-run.ps1 (see the comments there).
rem
rem "anet-run.cmd --resolve" prints the selected executable, version and
rem identity, and the command line of this script, and always exits 0. Errors
rem go to stderr with exit status 1.
rem
rem No setting is expanded on a command line of this file: anet-run.ps1 reads
rem the settings itself and returns only an absolute path of an .exe file
rem (which cannot contain a double quote) and a validated identity name.
rem PowerShell gets no standard input (0<NUL): the input of this script is the
rem input of anet, for example the MCP session of anet-mcp.cmd.
rem
rem Every program is named by its full path: PowerShell as
rem %SystemRoot%\System32\WindowsPowerShell\v1.0\powershell.exe, the scripts of
rem the plugin by the directory of this file. Nothing is looked up in the
rem working directory or on PATH, where cmd.exe would look in the working
rem directory (a workspace) first. SystemRoot is set by Windows for every
rem process and is not compared with the system directory here: cmd.exe cannot
rem ask Windows for that directory without running a program, which would have
rem to be found the same way, and a host that sets SystemRoot for its MCP
rem servers already chooses every program they run. anet-run.ps1 asks Windows
rem for the system directory ([Environment]::GetFolderPath) before it starts
rem the installer.
setlocal EnableExtensions DisableDelayedExpansion
set "ANET_RUN_SCRIPT=%~dp0anet-run.ps1"
set "ANET_RUN_SELF=%~f0"
rem Only the MCP launcher (anet-mcp.cmd, "anet-run.cmd mcp") installs anet.
if not "%~1"=="mcp" set "ANET_RUN_INSTALL="
if "%~1"=="--resolve" goto resolve
set "ANET_RUN_MODE=locate"
set "ANET_RUN_EXE="
set "ANET_RUN_ID="
for /f "tokens=1,* delims=|" %%A in ('%SystemRoot%\System32\WindowsPowerShell\v1.0\powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command "& ([scriptblock]::Create([IO.File]::ReadAllText($env:ANET_RUN_SCRIPT)))" 0^<NUL') do (
  if "%%A"=="E" set "ANET_RUN_EXE=%%B"
  if "%%A"=="I" set "ANET_RUN_ID=%%B"
)
if not defined ANET_RUN_EXE exit /b 1
set "ANET_RUN_MODE="
set "ANET_RUN_SCRIPT="
set "ANET_RUN_SELF="
set "ANET_RUN_INSTALL="
if defined ANET_RUN_ID goto with_identity
"%ANET_RUN_EXE%" %*
exit /b %ERRORLEVEL%

:with_identity
"%ANET_RUN_EXE%" --id "%ANET_RUN_ID%" %*
exit /b %ERRORLEVEL%

:resolve
set "ANET_RUN_MODE=resolve"
%SystemRoot%\System32\WindowsPowerShell\v1.0\powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command "& ([scriptblock]::Create([IO.File]::ReadAllText($env:ANET_RUN_SCRIPT)))" 0<NUL
exit /b 0
