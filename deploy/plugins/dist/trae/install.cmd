@echo off
rem Installs the anet bundle for Trae and Trae CN on Windows: runs install.ps1
rem of this directory with Windows PowerShell 5.1 (powershell.exe of the system
rem directory). "install.cmd -Help" shows the options.
setlocal EnableExtensions DisableDelayedExpansion
%SystemRoot%\System32\WindowsPowerShell\v1.0\powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "%~dp0install.ps1" %* 0<NUL
exit /b %ERRORLEVEL%
