# ANet 0.3.9 release notes

[中文](RELEASE-NOTES-0.3.9.zh-CN.md)

Release date: 2026-10-23.

| Component | Version | Repository |
|---|---|---|
| ANetCore | v0.16.2 | https://github.com/ANetResearch/ANetCore |
| ANet (`anet`) | v0.3.9 | https://github.com/ANetResearch/ANet |
| ANetHub (`anet-hub`) | v0.3.3 (unchanged) | https://github.com/ANetResearch/ANetHub |
| ANetLink | v0.2.0 (unchanged) | https://github.com/ANetResearch/ANetLink |

Documentation: https://docs.agentnetwork.org.cn/docs/

## Upgrading

0.3.9 is a patch release. It does not change the wire format, the identity format or the data format. A 0.3.8 node and a 0.3.9 node work together, and the hubs need no upgrade. Update a node with `anet update` and restart its daemons. Every new setting is off by default; a node that does not use them behaves as in 0.3.8. ANetCore v0.16.2 adds the new metadata key and reason codes to the wire registry.

## Changes

### An organisation runs inside an agent

A node can now let an agent answer the text tasks of the members of a local service, such as an organisation, with the rights of the member who sent each task. Four settings make this possible; each is general and names no particular service.

- **Admission by a local service** (`inbound.admit`). For a text task from a peer on no list, the node asks a capability of its own service module, marked `"internal": true`, whether to admit the peer. The answer `{"admit": true}` admits; every other answer, an error or no answer within 5 seconds does not. Answers are cached per peer (`cache_seconds`, 30 by default, at most 300) and asked again at every turn, so a peer that the service no longer admits is not served from its next message on. The deny list always wins. An internal capability is never advertised and cannot be called from the network. `auto_reply.admitted` (`off`, `direct` or `sandbox`) says how the exec backend serves an admitted peer. Commands: `anet inbound admit set <capability>`, `anet inbound admit off`, `anet autoreply set --admitted direct|sandbox|off` (`direct` is confirmed on the terminal).
- **The agent as another local account** (`anet runner serve`, `auto_reply.runner`). The runner runs the agent of a direct run as another Unix account, so that the agent can never read the node's identity keys, control token or data directory. The node reaches it over a Unix socket checked by peer credentials and by the rules of service sockets; one job per connection, cancelled by closing the connection. `auto_reply.runner.max_jobs` (default 4) bounds the runs at once; one run at a time per peer. Linux and macOS.
- **Model keys stay with the node** (`auto_reply.model_proxy`). A loopback proxy of the node forwards the agent's model requests to named upstreams and adds their keys; each run holds only a random key that is valid while the run lasts. Connections from other local accounts are refused on Linux. `auto_reply.admitted_budget` bounds the runs of admitted peers per day and per peer and the model requests per run; a run over the daily budget is refused before it starts. The output limit of a request is `max_output_tokens` of the upstream (32768 by default); sandboxed runs keep 8192.
- **Turn tools** (`anet turn-mcp`). A direct run of a trusted or admitted peer can call the service capabilities marked `"agent_tool": true`, always as that peer (`X-ANet-Caller`) and marked `X-ANet-Via: agent`. `anet turn-mcp` is a stdio MCP server for these tools; it needs no data directory and reads `ANET_TURN_URL` and `ANET_TURN_TOKEN`.

See the new how-to "Running the agent as another account" and the delegation topic.

### `anet uninstall`

`anet uninstall` stops the nodes and removes what the installers made: the binary and its release record, the links to it, the marked `PATH` blocks of the shell profiles and the fish configuration file, and on Windows the `AgentNetwork` Run value and the install directory in the user's `PATH`. Files and links that are not the installed anet are kept. `~/.anet` is kept unless `--delete-identity` is given, which is always confirmed on the terminal and names the AIDs. Wired coding agents are listed with `anet agents unwire <name>`; an npm installation is pointed to `npm uninstall -g @agentnetwork/anet`.

### Download sources

`install.sh`, `install.ps1` and `anet update` skip a download source whose release manifest does not verify and try the next one; they fail only when no source verifies. jsDelivr is read by version, never as a mix of two releases.

### Official agents

The official entry of the USTC organisation no longer lists the capability `ask`: questions and tasks are sent to the organisation as text tasks, which its agent answers. The guide says so.
