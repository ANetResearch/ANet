# ADR-XXX: An experimental custom protocol binding for relayed, end-to-end encrypted, store-and-forward delivery (`anet-relay/v1`)

> **NOT SUBMITTED — on hold per product owner (more testing first).**
> Draft in the format of A2A `adrs/adr-template.md`. Nothing here has been posted.
>
> **License: Apache-2.0** (ANet `LICENSE`, condition 3a).

**Status:** Proposed (draft; the ADR number is assigned on submission)

**Date:** YYYY-MM-DD (set on submission)

**Decision Makers:** A2A Technical Steering Committee and a sponsoring A2A Maintainer (experimental
bindings, `docs/topics/extension-and-binding-governance.md`)

**Technical Story:** proposal issue in `a2aproject/A2A` (to be opened); full binding text:
`../relay-binding.md` (binding URI `https://agentnetwork.org.cn/a2a/bindings/anet-relay/v1`).

How this document is meant to be used: the governance route for a custom binding is a proposal issue
("summary, motivation, initial approach or draft", governance :72-84), then a sponsoring Maintainer
and an `experimental-cpb-*` repository. This ADR-shaped text is the body of that issue, written so that
the TSC can take it as an ADR unchanged if it asks for one. The normative text stays in
`relay-binding.md`.

## Context

A2A's standard bindings (JSON-RPC, gRPC, HTTP+JSON) assume that the agent accepts inbound
connections at a stable URL. Many agents cannot: an agent on a laptop or phone, behind NAT, inside a
CLI tool, or one that is online for a few minutes a day. The usual workaround, a tunnel or reverse
proxy, makes the tunnel operator a TLS endpoint that reads every task.

anet runs agents like these. Each agent's local node holds a self-certifying identity (an Ed25519 key
event log, KEL); a hub keeps one mailbox per agent and stores opaque envelopes until the recipient
collects them. Every A2A message travels as an envelope signed by the sender and encrypted to the
recipient (HPKE, RFC 9180): the hub checks only the outer structure and never sees task content.
Get Task, List Tasks and Subscribe to Task are answered by the requester's own node from its mirror
of the task, because under store-and-forward it is the only party that can answer promptly.

A2A §12 and `docs/topics/custom-protocol-bindings.md` define what a custom binding must specify. The
binding text covers each item: operation mapping (§10.1), data types (§13), service parameters
(§10.4), errors (§11), streaming (§12), authentication (§9), card declaration (§4), interoperability
tests (§14).

## Decision Drivers

* Reach agents that cannot accept inbound connections, including agents that are offline when the
  task is sent (mailbox retention up to 14 days).
* Keep task content away from the relay operator: the relay is a third party to the conversation.
* Authenticate the sender without prior enrolment, shared secrets or a central identity provider.
* Change nothing in A2A's data model or operation semantics; a client talks A2A to its own node.
* Say plainly what the relay still learns, so that "end-to-end encrypted" is not over-read.

## Considered Options

* **1. Standard bindings through a tunnel or reverse proxy.** No new binding; the tunnel operator
  terminates TLS and reads every task, and an offline agent is simply unreachable.
* **2. Standard bindings plus an offline-reachability card extension** (the approach of A2A #2095).
  Declares that an agent is store-and-forward, but leaves the wire (who holds the queue, how it is
  protected) to a separate specification.
* **3. A mail-based or federated-messaging binding** (A2A #2191 MailA2A over RFC 5322 with
  S/MIME or OpenPGP; A2A #2185 fmsg). Store-and-forward over infrastructure that exists, with the
  security properties of that infrastructure.
* **4. A metadata-private binding** (A2A #1964). Hides the communication graph from network
  observers and relays with constant-rate cover traffic; identity and authentication are out of scope.
* **5. A relay binding with end-to-end sealed envelopes and a requester-side task mirror**
  (`anet-relay/v1`, this proposal), incubated as `experimental-cpb-anet-relay`.

## Decision Outcome

**Chosen option:** "5. A relay binding with end-to-end sealed envelopes and a requester-side task
mirror", as an **experimental** binding.

It is the only option that meets all five drivers with a running implementation: the relay
authenticates senders for rate limiting but never sees content (driver 2), identities are
self-certifying (driver 3), the A2A surface is unchanged because the client's node speaks the binding
(driver 4), and the binding text lists what the relay does learn (§15.2). Options 2–4 address
neighbouring problems and can be combined with it rather than replace it (see "Relation to open
proposals" below).

### Consequences

#### Positive

* A2A clients reach agents that have no inbound connectivity, with content protected from the relay.
* A worked, tested example of the §12 custom-binding checklist for a store-and-forward carrier,
  which the other store-and-forward proposals (#2191, #2185) can compare against.
* Sender authentication inside every message, independent of transport, is exercised end to end;
  that is the use case of the `SenderSignatureSecurityScheme` proposal
  (`../proposal-securityscheme.md`).

#### Negative

* A client needs a node (identity, key sets, a hub registration) to use the binding; a plain A2A client
  uses its own node's local A2A interface instead.
* Get Task reports the last state the requester's node received, which can lag the provider (§10.1).
* Delivery order between two nodes is not guaranteed; streaming is from the local mirror, not end to
  end (§12). The binding's cards therefore declare `capabilities.streaming: false` (§12, §17 Q4).
* The hub learns who sends to whom, when and how much (rounded to a Padmé size), and the client IP
  address (§15.2 item 1). Hiding that is option 4's problem, not solved here.
* Until A2A has a way to declare sender-signature authentication, network cards omit
  `securitySchemes` (§4, §9).

#### Neutral

* Hubs may federate; a hub forwards unchanged envelope bytes to the hub that holds the recipient.
* The same envelope can travel over other transports (the reference node also has a direct
  peer-to-peer path); only the hub transport is specified.

## Pros and Cons of the Options

### 1. Tunnel or reverse proxy

**Pros:** no new specification; any SDK works.
**Cons:** the tunnel operator reads every task; no offline delivery; sender authentication is
whatever the agent's HTTP stack does.

### 2. Offline-reachability card extension (#2095)

**Pros:** small, card-only; useful vocabulary for any store-and-forward agent.
**Cons:** does not define the queue, its protection, or sender authentication; the wire lives in a
separate Internet-Draft.

### 3. Mail or federated-messaging binding (#2191, #2185)

**Pros:** reuses deployed infrastructure and its operational boundaries; organisations already govern
mailboxes.
**Cons:** S/MIME / OpenPGP key distribution and mail-header metadata; latency and ordering of mail;
end-to-end protection depends on the profile each deployment chooses.

### 4. Metadata-private binding (#1964)

**Pros:** protects the communication graph, which option 5 does not.
**Cons:** cover traffic has a bandwidth and latency cost; identity and authentication are out of
scope by design.

### 5. `anet-relay/v1`

**Pros:** content hidden from the relay; sender authentication in every message; offline delivery;
unchanged A2A surface; running code and test vectors.
**Cons:** the negatives listed above; a node is required on both sides; the payload inside the
envelope is anet's task format today (§17 Q1 asks whether a v2 should carry A2A ProtoJSON instead).

## Implementation

Reference implementation: anet v0.2.0 (released 2026-09-28) and v0.2.1 (same day), source in
`ANetResearch/ANet`, `ANetCore`, `ANetHub` (map in `relay-binding.md` §16). Two federated public hubs
run it. Evidence gathered before any submission (anet `docs/notes`, summarised in 0032 §9):

| Evidence | What it shows |
|---|---|
| a2a-tck against the requester node's local A2A interface, every task relayed through a hub (0023, 0029) | 76 requirements pass, 8 fail. Six failures trace to the TCK (pinned to v1.0.0: a2a-tck #231, #240; snake_case JSON-RPC params: #242; one messageId reused for several messages; a camelCase check that descends into `Struct` metadata), two to anet (a MAY-level `Last-Modified`; file and data artifacts, a declared projection choice) |
| Official SDK clients through the binding (0035) | a2a-python 1.1.5: 82/0; @a2a-js/sdk 1.2.1: 76/0; Hermes' A2A client: 15/0; tasks relayed through a hub, answered by the provider's agent |
| Two-hub testnet soak, 4.25 h (0036) | 6 966 operations, 6 942 succeeded; the 24 failures all explained by scheduled restarts; 10 daemon/hub restarts, no task lost; 16 586 tasks, none left non-terminal after draining |
| Content never reaches the hub (0028, 0036) | 9 583 canaries in task text, attachments and skill arguments: 0 hits in either hub's data directory and logs, while found in the endpoints' own stores (control) |
| Fuzzing (0033) | envelope, key-set and relay-authentication parsers and the hub's relay routes; findings fixed before release |

Before the proposal issue is opened (from `docs/notes/0032` §4 step 5): the (designed) items left in
`relay-binding.md` removed or implemented; the §14 test-vector set for all message bodies and a
fixed-key end-to-end transcript; the binding URI answering with the specification; a decision by
the product owner on an Apache-2.0 reference implementation if an official (non-experimental) binding
is ever sought.

## Relation to open proposals

| Proposal | Relation |
|---|---|
| **#1964** metadata-private binding (open since 2026-06-21; last comment 2026-07-29) | Complementary threat models. #1964 hides *who talks to whom* from network observers and relays and leaves identity out of scope; `anet-relay/v1` hides *what is said* from the relay and authenticates the sender in every message, but the relay still sees the communication graph (§15.2 item 1). The two could be layered: sealed envelopes carried over a graph-private transport. Worth stating in #1964 that the relay's view should be written down per binding (our §15.1–§15.2 is one such list). |
| **#2191** MailA2A (open since 2026-08-30, no comments) and **#2185** fmsg (open, seeking sponsorship) | Same problem class, store-and-forward, other carriers. Points to align on: how streaming is presented when there is no stream (#2191 "ordered sequence of email events"; ours: local mirror, §12); deduplication and expiry (ours: `(from, mid)` until `exp`, §7.7); which operations are answered locally (ours: Get Task, List Tasks, Subscribe to Task, §10.1) against §12.1's "implement all core operations". A shared note on store-and-forward bindings would help all three. |
| **#2095** offline delivery / reachability card extension (open since 2026-08-01, no comments) | Complementary. #2095 describes on the card that an agent is store-and-forward; `anet-relay/v1` is one wire for it. anet's network cards could declare #2095's extension (`required: false`, `params.mode`) instead of leaving reachability implicit. |
| **#1829** Ed25519 + RFC 9421 message signatures (open, 143 comments, no Maintainer position seen) | Different layer: #1829 signs HTTP requests of the standard bindings; this binding signs inside the envelope. Both need a card-level way to say "the sender signs each message", which `../proposal-securityscheme.md` proposes (one profile each). |
| **#2239** discovery of public third-party A2A servers | An agent reachable only through this binding has no well-known URI; it is discovered through a registry (`adr-registry-api.md`). |

## Related Decisions

* ADR-001 (ProtoJSON serialization): the A2A surface of the reference node follows it; inside the
  envelope the binding uses deterministic CBOR (§13).

## References

* A2A specification v1.0.1 §5.8 (custom binding identification), §12 (custom binding requirements);
  `docs/topics/custom-protocol-bindings.md`; `docs/topics/extension-and-binding-governance.md`.
* `../relay-binding.md` (normative draft), `../proposal-securityscheme.md`, `adr-registry-api.md`.
* RFC 9180 (HPKE), RFC 8032 (Ed25519), RFC 8949 (CBOR).

## Notes

Open questions carried from `relay-binding.md` §17: Q1 generic ProtoJSON payload in v2; Q2 the
service-parameter key (`a2a.serviceParameters` here, `a2a-service-parameters` in the custom-binding
guide's example); Q3 data parts; Q4 `capabilities.streaming` for relay-only cards (the reference
implementation chose `false`); Q5 in-task authorization over a relay; Q6 the security scheme.

Name: the project is "ANet"; it is not "A2A Net" (already on the partners page) nor the "Agent
Network Protocol" (#2239 comment). Say so in the issue.
