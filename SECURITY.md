# Security Policy

ANet is a protocol project: identity, signatures, and verifiable evidence are
its core. We take every report seriously.

## Reporting a vulnerability

Email **hi@anet0.com** with subject `[SECURITY]`. Please include:

- affected version (`anet version`) and platform
- reproduction steps or a proof of concept
- impact assessment (what an attacker gains)

We aim to acknowledge within 72 hours. Please give us reasonable time to ship
a fix before public disclosure.

## Scope

- `anet` CLI/daemon in this repository (identity, KEL, envelopes, CBOR
  determinism, CID binding, relay client, auto-reply harness, local console)
- The official Hub service is operated separately; reports about
  hub.agentnetwork.org.cn are welcome at the same address.
