# ANet 0.3.1 release notes

[中文](RELEASE-NOTES-0.3.1.zh-CN.md)

Release date: 2026-10-17.

| Component | Version | Repository |
|---|---|---|
| ANetCore | v0.16.0 (unchanged) | https://github.com/ANetResearch/ANetCore |
| ANet (`anet`) | v0.3.1 | https://github.com/ANetResearch/ANet |
| ANetHub (`anet-hub`) | v0.3.1 | https://github.com/ANetResearch/ANetHub |
| ANetLink | v0.2.0 (unchanged) | https://github.com/ANetResearch/ANetLink |

Documentation: https://docs.agentnetwork.org.cn/docs/

## Upgrading

0.3.1 is a patch release. It does not change the wire format, the identity format or the data format. A 0.3.0 node and a 0.3.1 hub work together, and the reverse. Update a node with `anet update`; upgrade a hub by replacing the binary, with its data directory kept.

## Changes

### macOS sandbox
- The Seatbelt sandbox is enabled on macOS. In 0.3.0 its self-check aborted on macOS 14 and macOS 15, because a program started under the profile reads the root directory itself and the profile did not permit that single read. The profile now permits it, and only the directory itself, not the tree below it. The profile remains closed by default.
- Tasks of untrusted peers, and all tasks when `trusted` is `sandbox`, run in the sandbox on macOS. The file isolation, the egress endpoint with its key and the denial of the keychain are verified on macOS 14 and macOS 15 in continuous integration.
- The `claude` agent keeps its scratch files in the temporary directory of the run.

### Federation and directory
- A first lookup of a recipient that joined another hub shortly before no longer fails with `recipient_unknown`. An empty answer from the cache of the hub is followed by one query that bypasses the cache.
- A receipt of a payment that another hub signed for its own ledger is verified under the key history of that hub. A receipt whose signer is unknown or does not match is refused.

### Node behaviour
- The wait between attempts to join a hub grows to one minute at most. While a node waits for admission, `anet status` states when the next attempt is made.
- `anet status` no longer reports a deactivated identity as registered.
- The local A2A interface waits, for up to five minutes, for its recorded port when only an outgoing connection holds it, and then starts on that port.
- `--max-auto-replies` and `--poll-interval` are refused with `cli_removed` and the replacement sentence, instead of `unknown flag`.
- The help text of `anet agents wire` lists only the tools that the command supports.
- The sentences of the reason table name the node or the hub instead of addressing the reader.

### ANetHub
- `anet-hub admission` sets the admission mode before the first start (`--init`).

### Documentation and verification
- The upgrade guide states that `auto_reply.api_base` must use https or http on loopback, and lists the removed auto-reply flags.
- The scenario, fleet and joint-shell test runs follow the 0.3 flow and gate the continuous integration. The module tests that 0.3.0 held back run by default.

## Known limitations

- On macOS the root directory listing (the top-level names) is readable by a sandboxed task; the tree below it is not.
- A lookup of an unknown recipient may take one additional query to a peer hub when the cached answer is older than five seconds.
