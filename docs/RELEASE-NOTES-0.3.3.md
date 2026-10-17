# ANet 0.3.3 release notes

[中文](RELEASE-NOTES-0.3.3.zh-CN.md)

Release date: 2026-10-17.

| Component | Version | Repository |
|---|---|---|
| ANetCore | v0.16.1 | https://github.com/ANetResearch/ANetCore |
| ANet (`anet`) | v0.3.3 | https://github.com/ANetResearch/ANet |
| ANetHub (`anet-hub`) | v0.3.3 | https://github.com/ANetResearch/ANetHub |
| ANetLink | v0.2.0 (unchanged) | https://github.com/ANetResearch/ANetLink |

Documentation: https://docs.agentnetwork.org.cn/docs/

Reason codes: https://docs.agentnetwork.org.cn/docs/en/latest/reference/reason-codes.html

## Upgrading

0.3.3 is a patch release. It does not change the wire format, the identity format or the data format. A 0.3.2 node and a 0.3.3 node work together. Update a node with `anet update`; a hub is upgraded in place and keeps its data. ANetHub skips the number 0.3.2, so that the versions of `anet` and ANetHub are equal again.

One behaviour changes: output is English on every system. Before 0.3.3, `anet` and `anet-hub` printed Chinese when `LC_ALL`, `LC_MESSAGES` or `LANG` began with `zh`. Since 0.3.3 only `ANET_LANG` selects Chinese (a value that begins with `zh`, for example `ANET_LANG=zh-CN`), and the locale variables are not consulted. A script that matches the Chinese text of a sentence needs the variable set, or needs to match the English text or the reason code.

## Changes

### Language
- English is the default language of every sentence that `anet` and `anet-hub` print and of every text that is handed to a model. Chinese is an opt-in through `ANET_LANG`.
- The prompt of the exec backend, the self-check prompt of auto-reply and the audit verdict are English in both languages. The administration interface of the hub answers in English.

### Agent integration
- The output of `anet agents`, `anet agents wire` and `anet agents unwire` comes from the reason table, in English by default and in Chinese under `ANET_LANG=zh-CN`. The skill, persona and guide files that `wire` writes for a tool are English. The exit statuses are unchanged.
- The refusals of these commands carry 22 new codes with the prefix `wire_`, for example `wire_unknown_tool`, `wire_conflict` and `wire_io_failed`.
- `anet agents unwire` no longer prints an entry to add by hand when it cannot parse a JSON file; it names the file and leaves it unchanged.

### Reason codes
- The command line and the MCP tools show the reason code after the sentence of every refusal, for example `... send it again in 2 minutes. [busy]`. A refusal that the hub returned with a code of its own shows `[hub_refused:<hub code>]`. The JSON output is unchanged.
- Reason codes are managed as a stable interface. The register `internal/a2ashape/codes.txt` (and `internal/wire3/codes.txt` in ANetHub) lists every published code with the version that first emitted it, and a test holds it against the reason table. The meaning of a code never changes, and a code is never renamed or reused. The pages of the reason tables have a Since column, an anchor for every code and a table of retired codes; none is retired in this release.

### Fixes
- The task event stream no longer sends a preview frame twice when the frame arrives while the stream starts.

### ANetCore v0.16.1
- The wire registry lists the 22 `wire_` codes. The comments of the `coredet` package are in English. There is no change in behaviour.

## Known limitations

- Behaviour in Cursor and WorkBuddy, and on the Windows and macOS clients of the tools, was not run. The guides label these points as untested.
