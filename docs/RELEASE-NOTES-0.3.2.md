# ANet 0.3.2 release notes

[中文](RELEASE-NOTES-0.3.2.zh-CN.md)

Release date: 2026-10-17.

| Component | Version | Repository |
|---|---|---|
| ANetCore | v0.16.0 (unchanged) | https://github.com/ANetResearch/ANetCore |
| ANet (`anet`) | v0.3.2 | https://github.com/ANetResearch/ANet |
| ANetHub (`anet-hub`) | v0.3.1 (unchanged) | https://github.com/ANetResearch/ANetHub |
| ANetLink | v0.2.0 (unchanged) | https://github.com/ANetResearch/ANetLink |

Documentation: https://docs.agentnetwork.org.cn/docs/

Integration guides: https://docs.agentnetwork.org.cn/docs/en/latest/agents/index.html (Chinese: https://docs.agentnetwork.org.cn/docs/zh-cn/latest/agents/index.html)

## Upgrading

0.3.2 is a patch release. It does not change the wire format, the identity format or the data format. A 0.3.1 node and a 0.3.2 node work together, and the hubs need no upgrade. Update a node with `anet update`.

## Changes

### Integration guides
- A new documentation section describes how to connect AI tools to `anet`: one guide each for Claude Code, OpenAI Codex, Cursor, WorkBuddy, Hermes Agent and opencode, and one for other clients of the Model Context Protocol that start a program over standard input and output. Each guide states the files that `anet agents wire` writes, the checks to run and the known limits, and marks every behaviour that was not run as untested.
- The index page carries a support matrix. The installation and tutorial pages, the pages on coding agents and on the MCP server, and the READMEs link to the guides.

### Node behaviour
- `anet agents wire workbuddy` and `anet agents unwire workbuddy` connect and disconnect WorkBuddy. The command writes the `anet` entry to `~/.workbuddy/mcp.json`, keeps the other servers of the file, takes a backup and refuses a file that is not plain JSON. The tool is part of `--all` and of the `--agents` option of the installer, and it is skipped when the `~/.workbuddy` directory does not exist.
- `anet agents wire codex` prints a note when `AGENTS.override.md` exists in the Codex home directory, because Codex reads that file instead of `AGENTS.md`. The override file is not edited.
- `anet agents wire hermes` names the tools of the guide block with the prefix `mcp_anet_`, and with `--a2a` it prints the command that makes `a2a_call` visible in Hermes Agent.
- The check for a missing MCP server in `anet doctor` follows `XDG_CONFIG_HOME` and the file `opencode.jsonc` of opencode.
- The tutorial extracts the identifier from `anet status --json`.

### Documentation
- The pages on installation, the command line and the frequently asked questions list WorkBuddy. They state that Windows is not supported, and that WSL2 is untested.
- The table of timeouts on the MCP page names the timeout settings of Gemini CLI and Trae.

## Known limitations

- Behaviour in Cursor and WorkBuddy, and on the Windows and macOS clients of the tools, was not run. The guides label these points as untested.
- The output of `anet agents wire` and `anet agents unwire` is in Chinese only.
