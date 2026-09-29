# ADR-XXX: A registry API for signed Agent Cards

> **NOT SUBMITTED — on hold per product owner (more testing first).**
> Draft in the format of A2A `adrs/adr-template.md`. Nothing here has been posted.
>
> **License: Apache-2.0** (ANet `LICENSE`, condition 3a).

**Status:** Proposed (draft; the ADR number is assigned on submission)

**Date:** YYYY-MM-DD (set on submission)

**Decision Makers:** A2A Technical Steering Committee

**Technical Story:** discussion first (A2A Discussions, and a comment on A2A #2239), then a feature
proposal issue if there is interest. Full API text: `../registry-api.md`.

Why an ADR shape: a registry API is neither an extension nor a binding, so the extension/binding
governance has no route for it; it would be a documentation or specification change
(`docs/topics/agent-discovery.md`). This text is written so that it can be the body of that proposal
and, if the TSC wants one, an ADR.

## Context

A2A names three ways to discover an agent: the well-known URI, curated registries and direct
configuration (`docs/topics/agent-discovery.md`). Only the first is specified. The discovery guide
says: "The current A2A specification does not prescribe a standard API for curated registries"
(:59), and that the community is exploring standardization (:112).

Two gaps follow.

1. **Agents without a well-known URI.** An agent that cannot run an HTTPS server (see
   `adr-relay-binding.md`) has no `/.well-known/agent-card.json`. A registry is its only discovery
   mechanism.
2. **Registries break signatures.** Each registry defines its own listing format, and the card an
   agent signed (§8.4) is typically parsed and re-serialized into that format. Whether the signature
   survives depends on canonicalization details on which SDKs disagree today (A2A #2122; a2a-go #445;
   our cross-SDK vectors, `repro/`). A registry that serves the card as the agent submitted it avoids
   the question.

A2A #2239 asks where a public, third-party A2A server should be listed; the answers so far are "the
well-known URI" and per-project catalogues.

## Decision Drivers

* A client must be able to tell the agent's statements (the card) from the registry's (verification
  status, liveness, ratings).
* Signed cards must reach the client byte for byte, so that any conformant verifier can check them.
* Small enough to implement in an afternoon on top of an existing card store; no registry-specific
  identity scheme required.
* Search by what agents do (skills, tags), with pagination that matches A2A's (`ListTasks`).
* Registries can federate without re-signing or rewriting cards.

## Considered Options

* **1. Keep registries unspecified** (status quo).
* **2. Put registry metadata into the Agent Card** (ratings, liveness, home registry as card fields or
  an extension).
* **3. A small HTTP API that serves cards verbatim inside a registry wrapper** (this proposal).
* **4. A general agent-discovery protocol** (DNS-based, DID-based, or a federated directory
  protocol) specified in the A2A specification.

## Decision Outcome

**Chosen option:** "3. A small HTTP API that serves cards verbatim inside a registry wrapper"

It closes the two gaps above with the least specification: the card is carried as an opaque, signed
JSON value; everything the registry adds is in a wrapper labelled as the registry's statement. It
does not preclude option 4 later: a discovery protocol can point at registries that implement this
API.

### Consequences

#### Positive

* One listing format for all registries; clients and UIs can query any of them.
* Signatures survive, because the card bytes are never rewritten (`GET /a2a/v1/agents/{aid}/card`
  returns exactly what the agent submitted, with a strong `ETag`).
* The trust boundary is explicit: a registry cannot forge a card; it can omit cards, serve an older
  one to a client that has never seen a newer one, and misstate its own wrapper fields
  (`registry-api.md` §7.1).

#### Negative

* The agent identifier in the path (`{aid}`) and the key endpoint (`/agents/{aid}/jwks.json`) come
  from anet's identity scheme; a registry for other identifier schemes would put its own identifier
  in the same positions, which the specification text must allow.
* Free-text search (`q`) reveals what the client is looking for (§7.2).

#### Neutral

* Federation between registries (`GET /fed/v2/cards`) is informative, not required.

## Pros and Cons of the Options

### 1. Status quo

**Pros:** nothing to agree on. **Cons:** every registry invents a format; signed cards are
re-serialized and stop verifying; agents without a well-known URI are undiscoverable in a common way.

### 2. Registry metadata in the card

**Pros:** one document. **Cons:** the registry's opinion (a rating) looks signed by the agent, or the
registry must rewrite the signed card.

### 3. This API

**Pros:** small; keeps signatures intact; separates statements; running implementation.
**Cons:** another HTTP API to maintain; identifier scheme to be generalised.

### 4. A discovery protocol in the specification

**Pros:** complete answer. **Cons:** large; the roadmap does not list it; this ADR does not need it.

## Implementation

Reference implementation: ANetHub, released with anet v0.2.0/v0.2.1 (2026-09-28); verification in
ANetCore `a2acard`. Two federated public hubs serve it. What is implemented, as of v0.2.1
(`registry-api.md` updated to match on 2026-09-29):

| Endpoint | Status |
|---|---|
| `GET /a2a/v1/agents` (`skill`, `tag`, `q`, `limit` default 50 max 200, `cursor`) → `{agents, nextCursor}` | implemented; only verified cards are listed; `cardVerification` is `"ok"` for every entry |
| `GET /a2a/v1/agents/{aid}/card`, and `POST /a2a/v1/agents/card:lookup` (the AID in the body, not the URL) | implemented; card bytes as submitted, `ETag` (hex SHA-256 of the bytes), `Cache-Control: max-age=300`, 304 on `If-None-Match` |
| `GET /agents/{aid}/jwks.json` | implemented; derived from the agent's KEL; the `jku` of the agent's card signatures |
| `POST /register` with `a2a_card` → `card_status` `ok` / `unchanged` / `conflict` / `invalid` / `absent` / `withdrawn` | implemented |
| `GET /fed/v2/cards` (formats `a2a-card/1`, `withdrawal/1`) | implemented |

Evidence: the official a2a-python and @a2a-js/sdk clients resolved and verified cards served this
way (anet `docs/notes/0035` §3.1, §4); the card verifier and the hub's card routes were fuzzed
(0033: `a2acard` Verify and publish-form targets, hub card-admission targets); the hubs' card indexes
were checked to hold only cards that re-verify against the stored KEL (0033 hub). Card verification
tries the §8.4.1 proto-stripped payload first and falls back to the payload as given (the form a2a-go
v2.6.0 signs), and records which form verified — the transition path discussed in a2a-go #445.

## Relation to open proposals

| Proposal | Relation |
|---|---|
| **#2239** recommended discovery surface for a public third-party A2A server (open since 2026-09-16; two community comments, no Maintainer answer seen on 2026-09-29) | A concrete answer to the gap it names: a registry that any operator can run, with a common listing API. Comment there first (link to this draft), before opening a proposal. |
| **#2264** optional settlement / accepted-rails attribute on agent cards (open since 2026-09-26) | Adjacent: the registry filter "agents I can pay" (`registry-api.md` §8 Q2 lists "required extension URIs, e.g. a2a-x402" as a candidate filter). A rails attribute would be a card field or extension; the registry would index it, not define it. |
| **#2122** §8.4.1 canonicalization under-determined; a2a-go **#445**; a2a-tck **#228**, **#245** | The registry's rule "serve the card bytes as submitted" makes it independent of how #2122 is settled; its verifier already accepts both payload forms during the transition. |
| **#1964**, **#2191**, **#2095**, **#1829** | Not about discovery. #2095's reachability extension, if adopted, is exactly the kind of card content a registry would index. |

## Related Decisions

* ADR-001 (ProtoJSON serialization) for everything in the wrapper; the card itself is carried as an
  opaque JSON value.

## References

* `../registry-api.md` (full text), `adr-relay-binding.md`, `../issue-a2a-go.md` A1, A2.
* A2A `docs/topics/agent-discovery.md` (:59, :112), specification §8.2 (well-known URI), §8.4
  (card signing).

## Notes

Open questions carried from `registry-api.md` §8: Q1 the conventional path, or advertisement in the
registry's own card; Q2 more filters; Q3 a signed wrapper; Q4 the `cardVerification` values (the
draft proposed `VERIFIED`/`STALE`, the implementation emits `ok`); Q5 default-value handling in stored
cards.
