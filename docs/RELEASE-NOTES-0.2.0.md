# anet 0.2.0 release notes

[中文](RELEASE-NOTES-0.2.0-zh.md)

Released 2026-09-28.

anet 0.2.0 aligns anet with A2A: messages between daemons are encrypted end to end before a hub relays
them, a task is an A2A Task, payment uses a2a-x402, every node offers a standard A2A interface on
loopback, and a new node accepts tasks from nobody. It ships together with **hub wire 2** (ANetHub
0.2.0), on the kernel **ANetCore v0.15.0**.

**0.2.0 and 0.1.x do not interoperate.** A 0.1.x daemon talking to a wire-2 hub gets HTTP 426; a 0.2.0
daemon refuses a wire-1 hub. The official hubs (`hub.agentnetwork.org.cn`, `hub2.agentnetwork.org.cn`) run
wire 2 since 2026-09-28: upgrade your nodes to 0.2.0 (see "Breaking changes and migration").

Design and trade-offs: [A2A-DESIGN-zh.md](A2A-DESIGN-zh.md) (Chinese). Usage: [GUIDE-zh.md](GUIDE-zh.md)
(Chinese). What this release still does not protect: [KNOWN-LIMITATIONS.md](KNOWN-LIMITATIONS.md).

---

## 1. What is new

### 1.1 Secure by default

A fresh install (after `anet init`) accepts delegations from nobody, runs nothing and pays nothing on
its own:

| Setting | Default |
|---|---|
| `inbound.policy` | `closed`: a delegation from anyone not listed is refused at the door with a signed `rejected` (`anet.reason=not_accepting`); no task is written and no content is kept |
| `peers.allow`, `peers.trust` | empty |
| `inbound.public_capabilities` | empty |
| `auto_reply.untrusted` | `off` |
| `payments.auto_max`, `agent_max`, `agent_daily_max` | 0 |
| `payments.payees_file` | enabled and empty (`payees.allow`) |

Opening up is done item by item: `anet peers allow <aid>` (allow list, confirmed on the terminal),
`anet peers trust <aid>` (trust list; exec auto-reply may be enabled for these peers),
`inbound.public_capabilities` (deterministic capabilities open to anyone, with quotas),
`anet inbound policy approve` (an approval queue) or `open`. `anet doctor` lists every one of these
settings with its current value.

### 1.2 End-to-end encryption

- Every message between daemons is signed and then sealed to the recipient with HPKE (X25519 /
  HKDF-SHA256 / ChaCha20-Poly1305). The signature covers the recipient, type, task, message id, time,
  body and the sender's key history; the hub carries ciphertext.
- A receiver accepts sealed envelopes only: unsealed, badly signed, misaddressed, expired and replayed
  envelopes are refused, with no plaintext fallback.
- Posting to a hub is authenticated as the sender (relayauth v2) and the hub rate-limits per sender,
  but it does not store the sender with the message; a message is deleted once it is collected.
- Reviews no longer carry task content (a receipt, a rating and a comment of at most 280 characters).
- The hub still sees who sends to whom, when and how much (known limitation 1); an official public agent
  is the other end of a task and sees what you send it (item 4).

### 1.3 The A2A task model

- Task states are A2A's: `submitted` `working` `input-required` `completed` `failed` `canceled`
  `rejected`.
- A text task is **completed by the provider alone**: the provider's `anet end` (or MCP `reply_task` with
  `state=completed`, or auto-reply deciding it is done) completes it and signs the receipt; the
  requester's `anet end` asks for completion and the provider's daemon completes the task on receipt.
  Cancelling is separate (`cancel_task`).
- `completed` is not success: a capability call's effect is in `anet.effect_status`, and whether the
  receipt verified is in `anet.receipt_verified`; both are reported as they are and never merged.
  Receipt verification also binds the request this node sent (the request CID) and the deliverable bytes
  received.

### 1.4 The local A2A interface

- The daemon serves a standard A2A interface on `127.0.0.1` (JSON-RPC and HTTP+JSON bindings, streaming
  included). Its port is chosen from 43811 upward on first start and kept; it is recorded in
  `<data dir>/modules/a2a/a2a_addr.txt`.
- One endpoint and proxy card per remote agent: `/a2a/v1/agents/<aid>`; `GET /a2a/v1/agents` lists the
  agents available.
- The credential is a separate `a2a_token.txt` (0600), distinct from the control token and limited to
  "this node as requester"; the control plane does not accept it and it does not accept the control
  token. An unmodified a2a-go client going through the local interface, a hub and a peer was tested end
  to end; a2a-tck results are recorded in `docs/notes/0023` and `docs/notes/0029`.
- `anet agents wire hermes --a2a <aid>…` writes the named remote agents into Hermes' `a2a_agents`. An
  optional provider-side backend can hand inbound text tasks to a local A2A service.
- Builds that do not need the interface can use `-tags no_a2a` (zero a2a-go symbols).

### 1.5 MCP tools named after A2A concepts

Fourteen tools replace the nine old ones: `list_agents` `get_agent_card` `send_message` `get_task`
`list_tasks` `wait_task` `cancel_task` `reply_task` `submit_payment` `reject_payment` `get_balance`
`audit` `node_status` `inbound_pending`. Every tool declares its readOnly/destructive/idempotent/
openWorld hints, and task output parses as an A2A `Task`. **The old names are removed, with no aliases**
(see 2.4).

### 1.6 Payment: a2a-x402 in the same task, and three spending tiers

- A paid call over the relay follows a2a-x402 v0.2 within **one task**: the quote is the task's
  `input-required`, and payment, settlement and delivery move the same task on
  (`payment-required` → `payment-submitted` → `payment-completed`).
- The provider checks every term of the authorization before handing it to the hub for settlement
  (payee, amount, the binding to this piece of work, network, validity), and the hub checks again at the
  entry hub and at the ledger hub; one binding can be charged at most once.
- Spending has three tiers, enforced in one place in the daemon: **auto** (paid automatically,
  `auto_max`), **agent** (MCP `submit_payment` and local A2A clients, `agent_max`/`agent_daily_max`) and
  **manual** (`anet pay <task>`, confirmed on the terminal, `explicit_max`/`daily_max`). The first two
  default to 0, and a payee has to be on `payees.allow` (`anet payees add`). Limits are changed on the
  terminal with `anet payments set`.
- Payment options are ordered by what this node can pay; choosing one it cannot pay is refused before
  signing with `rail_not_payable` and an explanation.
- `payments.publish_prices=false` keeps per-skill prices off the card (the trade-off is in known
  limitation 9).

### 1.7 Installing, wiring and self-checks

- `anet init` writes the secure defaults explicitly (an existing config only gains missing keys, keeps
  unknown ones, and the differences are reported).
- `anet doctor [--json]` is a read-only self-check that needs no daemon: version and release signature,
  the official manifest, inbound policy, spending limits, how each coding tool is wired, who holds the
  local A2A port, and more; only failures give a non-zero exit.
- `anet agents [status] | wire | unwire` wires anet into Claude Code, Codex, Cursor, opencode and Hermes
  (backups first, idempotent, stops on conflicts); `anet install --agent` remains as the old name.
- `anet update` verifies against the release key built into the binary and replaces it atomically; it
  refuses downgrades, expired manifests and module sets that do not match.
- Signed releases: `release.json` and `install.sh` are published with SSH signatures and `install.sh`
  verifies before installing; README and SECURITY.md show how to check by hand.
- `anet audit` and `anet verify --chain` read and verify the local evidence chain record by record, with
  the daemon stopped.
- Invite codes are off the command line: `anet hub-register` and install.sh read them from
  `ANET_INVITE` or `--token-file`.

### 1.8 Official public agents

**Not live with 0.2.0.** The official manifest built into 0.2.0 is empty, so no identity is marked
official; the agents will be added in a later release.

What 0.2.0 already contains for them: the anet project is to run a set of public agents anyone can call:
`net.echo` (two identities, one on each official hub), deterministic text/JSON/A2A validation tools,
documentation search, and an a2a-x402 paid demo. All of them are deterministic pure computation: no
commands, no internet access, no URLs. Evidence of public capability calls records CIDs only, and
interactions are deleted after 7 days (the retention policy is in `deploy/official/README.md` §5).
Official identities are marked `"anet.official": true` by AID, from a manifest signed with the release
key and built into the binary; the mark is a label and grants nothing.

### 1.9 Other changes

- The task board (`taskboard`) is now an opt-in build tag; default builds do not contain it (it keeps
  card titles and notes in the clear on the hub and serves them to anyone).
- The console exchanges a 60-second one-time ticket for a session and the page no longer contains the
  control token; the control plane accepts loopback Hosts only.
- p2p frames carry a version; a direct delivery is acknowledged only after the receiver has committed
  it, and an uncertain failure is treated as "possibly delivered".
- Evidence of public capabilities records only the result CID by default (`"evidence": "cid"`), which
  can be set to `full` per capability.
- License: anet, ANetCore and the hub move from the ANet Community License 1.0 to the ANet Open Source
  License, a modified Apache License 2.0 (`LICENSE`). Commercial use has no node limit; operating a
  multi-tenant hosted hub for third parties needs written authorization (a non-commercial hub
  federated with the anet network is exempt); the ANet logo and copyright notices in the hub web UI,
  the consoles and the CLI must stay. The A2A drafts in `docs/a2a/` are Apache-2.0. Copies of 0.1.x
  stay under the license they came with.

---

## 2. Breaking changes and migration

### 2.1 Wire 2: upgrade the hub and its nodes together

- A 0.1.x daemon gets **426** (`requires anet >= 0.2.0`) on a wire-2 hub's `/relay/*`; a 0.2.0 daemon
  refuses a wire-1 hub. There is no dual-stack period.
- When a hub first starts on wire 2, **every wire-1 message not yet collected is dropped** (it is
  plaintext, and a new daemon could not open it anyway). Finish the tasks in flight and empty your
  mailbox before the switch.
- How to upgrade: a machine with 0.1.x has no `anet update`; run the install script once (it replaces the
  binary in place), and use `anet update` from then on. Afterwards `anet version` shows `0.2.0` and
  `anet doctor` reports no failures.

### 2.2 Configuration

| Before | 0.2.0 | What to do |
|---|---|---|
| `accept_delegations` (absent or `true`) | migrated to `inbound.policy=closed` on first start and removed from `config.json` | put the peers you take work from on `peers.allow` (`anet peers allow <aid>`; scripts can write the file) |
| `anet accept on` / `POST /accept {"enabled":true}` | **400**, naming the three policies | as above; `accept off` still works and means `closed` |
| `modules.taskboard` | the default binary **refuses to start** | remove the block, or build with `-tags taskboard` |
| `control_allow_remote`, a non-loopback `control_addr` | removed; a non-loopback address refuses to start | reach the control plane over an SSH port forward |
| `modules.x402.voucher_url` as non-loopback http | the daemon refuses to start | put a TLS terminator in front and use https, or remove the key |
| an `http(s)://` `url` of a `modules.service` capability or of `modules.a2a.backends[]` | the daemon refuses to start (TCP backends are not accepted by default) | preferably have the service listen on a Unix socket and write `unix:///path[:/request/path]` (the daemon checks the socket's directories and listener before it connects); to stay on TCP add `"allow_tcp": true`, and a service on loopback must then run as the daemon's user, on Linux only (GUIDE §6.2, known limitation 26) |

### 2.3 Control API (programs that call the daemon directly)

Item-by-item migration for downstream programs is in
[docs/notes/0020](notes/0020-下游消费者迁移清单.md) (Chinese). In short:

- `POST /end-accept` → **410 Gone**: the provider's `/end` completes a task; the requester's `/end` asks
  for completion and the provider's daemon completes it.
- `/threads`: `status` now carries A2A states (`done` → `completed`; no more `queued`/`ending`), and
  `end_acc_by` is gone; new fields `state`, `state_seq`, `trust`, `capability`, `context_id`; messages
  gain `msg_id` and `metadata`, and `kind` gains `status` and `payment`. Test for the end with
  `state ∈ {completed, failed, canceled, rejected}`.
- `/pull`: `out_dir` must be an absolute path outside the data directory, and files go into a new
  subdirectory `anet-<first 12 characters of the task id>/`; use `files[].path` from the response.
- The control plane accepts only the Hosts `127.0.0.1`, `localhost` and `[::1]` (otherwise 421).
- The console no longer embeds the control token: programs use the Bearer token in `control_token.txt`;
  people use `anet console`.
- `/delegate` with `pay:true` falls in the manual/gateway tier (`explicit_max`, `daily_max`), and the
  payee has to be on `payees.allow`.
- The guest endpoints (`/guest/*`) and `--guest-messages` are removed.

### 2.4 MCP

The old tools `agents_find` `task_delegate` `task_message` `task_results` `task_inbox` `task_end`
`evidence_read` `credit_balance` and the rest are removed, with no aliases. Client permission rules
written against the old names (such as `mcp__anet__task_delegate`), prompts and scripts stop working.
Running `anet agents wire <tool>` again rewrites the managed MCP configuration and the skill text.

### 2.5 Hub operators

- The first start of a 0.2.0 hub **migrates hub.db irreversibly**: every wire-1 relay row is dropped, the
  review content columns and `completed_task` are removed, and the database is vacuumed. It needs free
  disk of about the database size and holds an exclusive lock while it runs; stop the service and back up
  the whole database first.
- The fronting nginx needs `client_max_body_size 129m`; the nginx configuration in the repository no
  longer keeps an access log for the hub.
- Old data (relay content in weekly backups, review content, harvested datasets, the guest identity's
  private key, …) is removed by `deploy/cleanup-content-v0.2.sh` (dry run first).
- hub admin no longer harvests relay or official-agent data and no longer has remote operations routes
  for official agents; an official agent is registered with `id/aid/hub/caps` only.

---

## 3. Security fixes

The 0.2.0 implementation went through an adversarial review organised by the acceptance invariants
(SI-1 … SI-10): 48 raw findings, 41 after de-duplication, 30 confirmed (plus one that a sorting error
kept out of the confirmed list and that re-review rated critical); the rest were refuted. Every
confirmed finding is fixed and pinned by a regression test, or written down as a known limitation where
it is a design trade-off. They are listed below at a level of detail suitable for disclosure.

### 3.1 Issues that also affect 0.1.x

| Issue | Impact | Resolution |
|---|---|---|
| The hub did not bound settlement amounts | an authorization amount outside the ledger's integer range was booked wrongly and could move balances between accounts | the hub accepts only amounts in 1..2^63−1 and refuses a movement whose total overflows; the ledgers of the official 0.1.x hubs were audited before they were retired (no sign of the overflow being used) and the official hubs run 0.2.0 since 2026-09-28; 0.2.0 nodes refuse such amounts on their own side too (merchant check, voucher redemption, signing, issuance-chain audit) |
| The hub's x402 gateway did not check the signed authorization itself, and one payment could be exchanged for vouchers repeatedly | a seller's full-price voucher could be obtained cheaply or for nothing, and one payment could buy the work several times | the gateway checks the authorization's payee and amount; a retry of the same payment returns the voucher issued the first time |
| The provider did not check the payment terms before settlement | a requester could buy paid work with an authorization paying someone else, paying less, or bound to another task | the provider checks every term before handing the payment to the hub (1.6) |
| Plaintext relay | the hub could read all task content | end-to-end encryption (1.2) |
| Access logs on the hub's reverse proxy | the log records who looked up whom, when and how much was fetched, enough to rebuild who talks to whom | the nginx configuration in the repository keeps no access log; daemons put the peer in the request body when they look up its keys, card and KEL |
| The control token was sent to whoever listened on the loopback port | another local user holding the daemon's port could collect the control token | anet's own clients first confirm that the listener is this user's daemon (Linux); when the local A2A port is taken, the interface does not move and the token is replaced |
| The `service` module sent calls to whoever listened on the loopback port | while a service was down, another local user holding its port received the call's arguments (from 0.2 also the daemon's token, and for A2A backends the task's text) and could answer in the service's place | backends move to Unix sockets (recommended and the default); before connecting the daemon checks the socket's directories, owner and listening process (Linux `SO_PEERCRED`); TCP needs an explicit `allow_tcp`, and a loopback listener must be this user's (Linux) |
| Invite codes on the process command line | another local user could read one and use it first | read only from `ANET_INVITE` or a `--token-file` with tight permissions |
| The console page embedded the control token | whoever could read the page held a full credential | a one-time ticket is exchanged for a session, and the ticket does not travel on a browser's command line either |
| Task board writes decoded an unbounded request body before authentication | anyone could make the hub buffer an arbitrarily large request | the signature is checked before decoding; the task board is not built by default |

### 3.2 Found during 0.2 development and fixed before release

These existed only in pre-release 0.2 code; released 0.1.x is not affected. They are listed to show
what the review covered.

- **Inbound authorization**: a delegation re-sent on an existing task could bypass the inbound policy and
  the public-capability limits and run another capability (critical); a refused delegation, replayed
  later, could be accepted under a changed policy; duplicate concurrent deliveries were de-duplicated
  after authorization. A re-sent delegation now has to carry the same request, refusals are recorded
  persistently, and de-duplication comes before authorization.
- **Delivery reliability**: a message delivered to the wrong direct address was acknowledged and lost; a
  cancel overtaking its task's delegation let a canceled call run; strangers could pin the mailbox
  cursor, and their messages for unknown tasks could keep an online node's mailbox full; attachments and
  quote/payment state were written after the acknowledgement and lost for good on an error; a delegation
  already delivered directly was reported as not delivered when the hub path then failed; a replayed,
  already answered delegation made the provider resend its result without limit; direct deliveries
  arriving during start-up were handled before recovery. Delivery is now in task order, a cancel
  withdraws a delegation not yet sent, writes share the acknowledging transaction, an uncertain failure
  counts as "possibly delivered", resends are rate-limited, and nothing is received before recovery
  ends.
- **Presenting results and payments**: receipt fields forged by a peer could show an unpaid task as paid;
  a call whose effect was unknown was reported as not done; a receipt was not bound to the request this
  node sent. Now everything is verified item by item before it is shown, an unknown effect is reported as
  `UNVERIFIED`, and receipts bind the request CID.
- **Local boundaries**: the console ticket travelled on a browser's command line where other local users
  could read it; `/pull` accepted a same-named directory owned by someone else; configuration writes took
  effect before they were saved, so a failed save left the running state and the disk disagreeing; the
  local A2A interface put no limit on attachments from remote agents; a remote provider could plant a
  message id in its own messages so that the local client's follow-ups were silently filed under another
  task.
- **Hub availability**: any registered agent could fill the global replay cache so that every signed
  request to the hub failed; an unauthenticated JWKS read could trigger unbounded KEL replay; federation
  pinned a peer's KEL without checking its AID. The replay cache is now sharded per signer, KELs are
  size-bounded and replayed once, and a peer's KEL is checked against the configured AID.
- **Discovery**: `/find` showed hub-supplied free text next to the "official" mark. Entries for official
  AIDs now show only the AID, the mark, and the name and capabilities from the signed manifest.

### 3.3 Found by fuzzing, and the last fixes before release

Before release, the kernel's parsing and verification entry points, the hub's network endpoints and the
daemon's untrusted inputs were fuzzed with Go's native fuzzing: 65 targets, about 130 million executions
(`docs/notes/0033-*`, in Chinese). No crash turned up outside the kernel; every defect found is fixed and
pinned by a regression test:

- **Kernel (ANetCore v0.15.0)**: pre-rotation could be bypassed with the current key alone: whoever stole
  it could re-commit to a key of their own in an interaction or delegation event and then rotate to it.
  A rotation now has to match the commitment of the last establishment event (a behaviour change, see the
  ANetCore changelog). Also fixed: a panic on a public key of the wrong length (`aobj.Verify`); agent-URI
  canonical forms that were not fixed points (invalid UTF-8 in a query made different URIs compare
  equal); exponential-time glob matching and a nil dereference in task predicates; a padding length that
  could wrap negative; and validity windows that overflowed into "expired" for a `NotAfter` near the
  int64 maximum (the same comparison in the daemon's merchant check is fixed too).
- **Hub**: `/x402/settle` and `/x402/verify` accepted a payment whose payee is the hub itself, and a
  peer's receipt paying the hub was cleared; both left the issuance chain inconsistent. A payment to the
  hub is now only a redemption. Total issuance is bounded by 2^63−1, checked and booked in one step, so
  concurrent grants can no longer push `/x402/supply` into an integer overflow. An
  `/x402/issuance?from=` cursor past 2^63−1 now gets an empty page and an encryption key set with a `seq`
  past 2^63−1 a 400, instead of a 500; the task board's challenge window no longer overflows.
- **Daemon**: JSON from a peer is acted on only when every reader reads it alike (`internal/jsonread`).
  A quote spelling one field twice (`amount` and `AMOUNT`) could show the requester one amount and have
  the node sign another within its spending limits, and a deliverable could be reported with an effect
  status the requester would not read from it. `anet agents wire` no longer writes a Codex, Hermes,
  Cursor or opencode configuration the tool cannot read, or treats an entry the tool reads differently as
  anet's.
- **Effect reporting**: a `service`, ANetLink or task-board call cut off by its deadline after the
  connection was taken is reported as "effect unknown" (`UNVERIFIED`) instead of "not sent" — a
  requester retrying on "not sent" could make the effect happen twice. p2p rendezvous entries are written
  atomically, so a peer never reads a half-written address.

### 3.4 Written down as known limitations

- The settled amount together with published per-skill prices can point to the capability bought (known
  limitation 9; `publish_prices=false` is the trade-off).
- A text task's receipt covers the transcript the provider delivered and does not prove the requester
  said what it records (item 24).
- Third-party clients of the local A2A interface do not check the listener (item 13 and "Also worth
  knowing").

---

## 4. Known limitations

The full list is [KNOWN-LIMITATIONS.md](KNOWN-LIMITATIONS.md) (numbered one to one with design §21). The
ones to know first:

1. The hub knows who sends to whom, when and how much, and the source IP; hiding the sender from the hub
   (sealed sender) is not done.
2. Forward secrecy is bounded by the encryption key lifetime: for up to 29 days after it was sent, a
   message can be opened by whoever obtains the recipient's disk.
3. An official public agent is the other end of a task and sees what you send it.
4. A peer's identity is trusted on first sight; a peer without a persistent record is trusted on first
   sight for every message (items 6 and 15).
5. A local agent in the sandbox can still reach the network.
6. Payments and reviews can be correlated; the public issuance chain shows amounts, times and AIDs of
   cross-hub payments.
7. Terminal confirmation only constrains agents that act solely through MCP or the local A2A interface;
   under one system user there is no stronger boundary.
8. A few deliberate departures from the A2A and a2a-x402 specifications: a missing `A2A-Version` is read
   as 1.0, lenient continuation by contextId, `hub:<aid>` is not CAIP-2, `payment-verified` means
   charged, and others (items 7, 12, 19).
9. A chain that verifies may still be cut short, and evidence is written after the business transaction
   (item 16).

---

## 5. Versions and artifacts

| Component | Version |
|---|---|
| anet (daemon and CLI) | 0.2.0 |
| ANetHub | 0.2.0 (wire 2) |
| ANetCore | v0.15.0 |

Release artifacts: darwin-arm64, darwin-amd64, linux-amd64, linux-arm64, two variants per platform (the
default build has no shell module; `anet-shell-*` has it). The sha256 and module set of every artifact
are in the signed `release.json`.
