@echo off
rem Starts the MCP server of the local anet node ("anet mcp", stdio) for the
rem executable and identity of the plugin settings, which the host passes in
rem ANET_PLUGIN_BIN and ANET_PLUGIN_IDENTITY; see anet-run.cmd. The Windows
rem counterpart of anet-mcp.sh.
rem
rem ANET_RUN_INSTALL=missing asks anet-run.ps1 to install anet with the
rem official installer (install.ps1, which also starts the node and joins the
rem public hub) when it finds no anet.exe, unless ANET_PLUGIN_NO_INSTALL=1 is
rem set; all of its output goes to standard error, because standard output is
rem the MCP session. "anet mcp" then starts the node of the selected identity
rem when it is not running, and serves the tools even when the node cannot
rem run: node_status then names the reason and the command that fixes it.
set "ANET_PLUGIN_SETTINGS=env"
set "ANET_RUN_INSTALL=missing"
"%~dp0anet-run.cmd" mcp
