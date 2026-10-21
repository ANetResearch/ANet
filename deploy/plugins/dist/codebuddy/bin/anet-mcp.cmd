@echo off
rem Starts the MCP server of the local anet node ("anet mcp", stdio) for the
rem executable and identity of the plugin settings, which the host passes in
rem ANET_PLUGIN_BIN and ANET_PLUGIN_IDENTITY; see anet-run.cmd. The Windows
rem counterpart of anet-mcp.sh. The server forwards every call to the running
rem daemon of the selected identity and exits with status 1, with the reason on
rem stderr, when no daemon runs for it.
set "ANET_PLUGIN_SETTINGS=env"
"%~dp0anet-run.cmd" mcp
