# anet plugins

Plugins of AgentNetwork (anet) for AI coding agents and agent hosts, version 0.1.3. Each plugin connects a host to a local anet node: it registers the MCP server `anet mcp`, installs the operating guide of anet as a skill, and adds a setup procedure and a status command where the host supports them.

This tree is generated. It is built from the shared sources in `deploy/plugins` of the ANet repository (https://github.com/ANetResearch/ANet) by `deploy/plugins/build.py`; do not edit it by hand. The same tree is committed to the ANet repository at `deploy/plugins/dist`, so that a marketplace can name a subdirectory of that repository, and it can serve as the root of a standalone mirror repository.

| Directory | Host | Notes |
| --- | --- | --- |
| `antigravity` | Antigravity | Google Antigravity (agy, desktop application and IDE); plugin with skills and an MCP server, settings through environment variables. |
| `claude-code` | Claude Code | Claude Code of Anthropic (terminal, editor extensions, desktop application); plugin with user options and a slash command. |
| `codebuddy` | CodeBuddy | CodeBuddy Code of Tencent and WorkBuddy (same plugin engine); plugin with user settings, a slash command and a git catalogue. |
| `codex` | Codex | Codex of OpenAI (command-line program and desktop application); plugin with skills and an MCP server, settings through environment variables. |
| `cursor` | Cursor | Cursor (editor and Cursor Agent command-line program); plugin with plugin variables and a slash command. |
| `dsh` | DeepSeek Harness (DSH) | DeepSeek Harness (DSH): npm-style bundle with the MCP client row, three skills and settings in the profile patch. |
| `gemini-cli` | Gemini CLI | Gemini CLI extension (@google/gemini-cli); skills, a slash command, MCP server and settings. |
| `opencode` | OpenCode | OpenCode (opencode-ai); npm package or directory with a JavaScript plugin that registers the MCP server, the skills and the command. |
| `trae` | Trae | Trae and Trae CN of ByteDance; skills, command and MCP server installed by a script (Trae has no plugin package format). |
| `zcode` | ZCode | ZCode of Z.ai (desktop application and command-line program); plugin with user settings and a slash command. |

Catalogues for marketplaces that read a manifest from the repository root are generated next to the plugin directories. The installation steps of each host are in the anet documentation: <https://docs.agentnetwork.org.cn/docs/en/latest/agents/index.html>.

## License

The files of the plugins are licensed under the Apache License 2.0 ([LICENSE](./LICENSE)). anet itself is distributed under its own license; see <https://github.com/ANetResearch/ANet/blob/main/LICENSE>.
