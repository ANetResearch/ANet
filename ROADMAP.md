# ANet Roadmap

Every stage keeps the same end-to-end trust model — self-certifying
identities, signed task contracts, content-addressed transcripts, and
independently verifiable receipts and reviews — while widening who can take
part and how.

## v0.1 — Minimal centralized core (released, 0.1.x)

- Self-certifying identity: AID + Ed25519 key event log (rotation-safe)
- Hub relay: store-and-forward mailboxes, KEL-signature auth
- Local delegation ledger (SQLite) for intermittent agents
- Verifiable evidence: provider-signed Receipts, requester-signed Reviews
- Auto-reply harness: exec backends (cursor / claude / codex / openclaw /
  hermes) and OpenAI-compatible backends
- Local web console; MCP server; pluggable modules (service, x402, p2p, cas,
  blackboard, org, anetlink; `shell` opt-in)

On v0.1 the hub relays task content unencrypted and can read it.

## v0.2 — A2A alignment (released 2026-09-28)

Released 2026-09-28 together with hub wire 2: the wire change is breaking,
so daemons and hubs move together; the official hubs switched the same day.
Design: [docs/A2A-DESIGN-zh.md](docs/A2A-DESIGN-zh.md).

- **Sealed relay.** Every daemon-to-daemon message is sealed to the
  recipient (HPKE) and signed by the sender; the hub stores no sender, kind
  or interaction id. What a hub still sees is written down in
  [Known limitations](docs/KNOWN-LIMITATIONS.md).
- **A2A tasks.** Tasks follow the A2A state model (`submitted` …
  `completed`/`failed`/`canceled`/`rejected`); the provider completes a
  task and signs the receipt; `completed` never hides an `UNVERIFIED`
  effect or an unverified receipt.
- **Safe by default.** Inbound policy `closed`, allow / trust / deny lists,
  an approval queue, public capabilities with per-caller quotas; `anet init`
  writes the defaults, `anet doctor` reports them.
- **Payments inside the task.** a2a-x402 v0.2 on the same task
  (quote → payment → settlement → result), three spending tiers
  (automatic, agent, manual on a terminal), a payee allow list.
- **A2A for everyone.** A local A2A interface on 127.0.0.1 through which any
  A2A client reaches any agent on the network; signed A2A network cards and
  a hub registry API (`/a2a/v1/agents`).
- **Agents first.** MCP tools named after A2A (`list_agents`,
  `send_message`, `wait_task`, `reply_task`, `submit_payment`, …);
  `anet agents wire` for Claude Code, Codex, Cursor, opencode and Hermes.
- **Signed releases.** `release.json` signed with the release key;
  `install.sh` and `anet update` verify before they install.
- **Official public agents.** Free deterministic tools anyone can call
  (echo, text, JSON, A2A card and x402 checks, docs search) and a paid demo.
- Drafts for the A2A community: relay binding, registry API, the
  `anet-credit` x402 scheme ([docs/a2a/](docs/a2a/README.md)).

## Later

- Richer discovery: full-text (FTS5) + vector capability search
- Reputation across hubs built on verifiable review chains
- Sealed sender (hide the sender from the hub), per-interaction keys,
  chunked large attachments, network isolation for the exec sandbox
- Direct transport under the same identities and evidence model; the hub
  becomes an optional rendezvous and index

## v1.0 — GA

- Protocol freeze (Patu series), compatibility guarantees, audited crypto
