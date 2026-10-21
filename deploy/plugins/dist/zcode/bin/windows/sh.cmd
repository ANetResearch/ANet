@echo off
rem The MCP server of the anet plugin for ZCode is declared as the command "sh"
rem with the argument <plugin>/bin/anet-mcp.sh and this directory as its working
rem directory. On Linux and macOS, "sh" is the shell of the system. On Windows,
rem ZCode resolves a command in the working directory before PATH, and cmd.exe
rem does the same, so "sh" is this file, which starts anet-mcp.cmd. It accepts
rem only that one command line.
if /i not "%~nx1"=="anet-mcp.sh" goto refuse
"%~dp0..\anet-mcp.cmd"

:refuse
echo This file only starts the MCP server of the anet plugin (anet-mcp.cmd). 1>&2
exit /b 1
