@echo off
rem Installs the anet command line on Windows with the official installer.
rem
rem "anet-install.cmd" runs https://agentnetwork.org.cn/install.ps1 with Windows
rem PowerShell, over TLS 1.2. The installer checks the signed release manifest
rem and the SHA-256 of the binary, stops without changing anything when a check
rem fails, installs into a directory of this user and puts it on the user PATH.
rem The Windows counterpart of anet-install.sh.
rem
rem Every program is named by its full path: PowerShell as
rem %SystemRoot%\System32\WindowsPowerShell\v1.0\powershell.exe, the scripts of
rem the plugin by the directory of this file. Nothing is looked up in the
rem working directory or on PATH, where cmd.exe would look in the working
rem directory (a workspace) first. SystemRoot is set by Windows for every
rem process and is not compared with the system directory here: cmd.exe cannot
rem ask Windows for that directory without running a program, which would have
rem to be found the same way, and a host that sets SystemRoot for its MCP
rem servers already chooses every program they run. (The automatic installation
rem of the MCP launcher, in anet-run.ps1, asks Windows for the system directory
rem with [Environment]::GetFolderPath.)
setlocal EnableExtensions DisableDelayedExpansion
%SystemRoot%\System32\WindowsPowerShell\v1.0\powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command "[Net.ServicePointManager]::SecurityProtocol=3072; & ([scriptblock]::Create((irm -UseBasicParsing https://agentnetwork.org.cn/install.ps1)))" 0<NUL
exit /b %ERRORLEVEL%
