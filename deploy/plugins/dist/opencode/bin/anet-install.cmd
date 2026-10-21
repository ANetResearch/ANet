@echo off
rem Installs the anet command line on Windows with the official installer.
rem
rem "anet-install.cmd" runs https://agentnetwork.org.cn/install.ps1 with Windows
rem PowerShell, over TLS 1.2. The installer checks the signed release manifest
rem and the SHA-256 of the binary, stops without changing anything when a check
rem fails, installs into a directory of this user and puts it on the user PATH.
rem The Windows counterpart of anet-install.sh.
setlocal EnableExtensions DisableDelayedExpansion
%SystemRoot%\System32\WindowsPowerShell\v1.0\powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command "[Net.ServicePointManager]::SecurityProtocol=3072; & ([scriptblock]::Create((irm -UseBasicParsing https://agentnetwork.org.cn/install.ps1)))" 0<NUL
exit /b %ERRORLEVEL%
