# A2A Agent Registry API (proposal)

> **NOT SUBMITTED — on hold per product owner (more testing first).** Every external submission also
> needs the product owner's approval, one by one. ADR-format summary for the proposal:
> `submissions/adr-registry-api.md`.
>
> **License: Apache-2.0**: this proposal is licensed under the Apache License 2.0 alone (ANet
> `LICENSE`, condition 3). Registry code contributed upstream from ANetHub is contributed under
> Apache-2.0.

| | |
|---|---|
| Status | Draft. Everything in §3 and §5 is implemented in ANetHub and ANetCore `a2acard` as released with anet v0.2.0 and v0.2.1 (2026-09-28) and answers on two federated public hubs; field spellings, limits, `ETag` format and `card_status` values below were aligned with that code on 2026-09-29 (they had been proposals). Remaining open points are in §8. |
| Intended venue | Discussion issue in `a2aproject/A2A` (the discovery guide states that "the current A2A specification does not prescribe a standard API for curated registries"). |

Keywords MUST, SHOULD, MAY are to be read as in RFC 2119 / RFC 8174.

---

## 1. Summary

A small HTTP API for a registry ("hub") that stores **signed** A2A Agent Cards submitted by the
agents themselves and lets clients search them by skill and tag. The design separates two kinds of
statements:

- **The card** is the agent's statement. The registry stores and serves it byte for byte and never
  rewrites it. Its signature (A2A §8.4) is made by the agent's own key.
- **The wrapper** around each card (verification status, liveness, rating, home registry) is the
  registry's statement. It is labelled as such and clients are told not to confuse the two.

Clients are expected to verify the card signature themselves. A registry that returns only cards it
has verified saves them work; it does not replace their check.

## 2. Motivation

- A2A defines three discovery mechanisms (well-known URI, registries, direct configuration) but
  specifies only the first. Agents that cannot host an HTTPS server have no well-known URI, so for
  them a registry is the only discovery mechanism.
- Without a common API every registry defines its own listing format, and the card an agent signed
  is often re-serialized into it, which breaks the signature (JCS re-canonicalizes, but field
  omission, default values and number formatting do not survive every round trip; see
  `issue-a2a-go.md` A1–A2).
- Clients need to know which fields are the agent's claims and which are the registry's. Mixing
  "rating 4.8" into the card would make the registry's opinion look signed by the agent.

## 3. Endpoints

All responses are JSON (`application/json`) unless stated otherwise. Member names are camelCase, as
in A2A. AIDs are the agent identifiers of the anet relay binding (`relay-binding.md` §5); a
registry for other identifier schemes would use its own identifier in the same positions.

### 3.1 Search: `GET /a2a/v1/agents`

Query parameters (all optional):

| Parameter | Meaning |
|---|---|
| `skill` | exact match on an `AgentSkill.id` in the card |
| `tag` | exact match on one of the skill tags |
| `q` | free-text substring match over name, description and skill names. Intended for web UIs; see §7.2 |
| `cursor` | opaque pagination cursor from a previous response |
| `limit` | page size; default 50, maximum 200 (a larger value is clamped; 0, negative or non-numeric is 400) |

Response:

```json
{
  "agents": [
    {
      "aid": "bafyrei…",
      "card": { "name": "…", "supportedInterfaces": [ … ], "signatures": [ … ] },
      "cardVerification": "ok",
      "verifiedAt": "2026-09-27T08:00:00.000Z",
      "homeHub": "https://hub.example.org",
      "lastSeen": "2026-09-27T09:12:00.000Z",
      "quiet": false,
      "reviewCount": 12,
      "avgRating": 4.5
    }
  ],
  "nextCursor": "YmFmeXJlaS4uLg"
}
```

| Member | Whose statement | Meaning |
|---|---|---|
| `aid` | registry (verified binding) | the identifier the card's signing key belongs to |
| `card` | **agent** | the stored card, embedded verbatim as a JSON value |
| `cardVerification` | registry | result of the registry's own verification at `verifiedAt` |
| `verifiedAt` | registry | when the registry last verified the card against the KEL it held |
| `homeHub` | registry | base URL of the registry that holds the agent's registration (this registry's own URL for local entries); the same value as `home` in §3.5 |
| `lastSeen` | registry | when the agent last collected its mailbox (liveness); absent if never |
| `quiet` | registry | `true` if the agent has not been seen for a long time |
| `reviewCount`, `avgRating` | registry | aggregates of signed reviews the registry holds; the signed reviews themselves are available separately |

Rules:

- Only entries whose card passed verification (§5) are listed, and only for agents that chose to be
  listed. A registry MUST NOT list a card it has not verified or whose verification failed.
- `card` is written into the response verbatim (the exact bytes the agent submitted). Signature
  verification is over the RFC 8785 canonical form, so a client may parse the response with any
  conforming JSON parser; but the parser MUST reject duplicate member names (§7.3).
- `nextCursor` is present only when more entries follow; pass it back as `cursor`. (A2A ListTasks
  instead always returns `nextPageToken`, empty on the last page; aligning the two is §8 Q4.)
- `skill` or `tag` given but empty is 400 (omit the parameter to list everything); `q` is limited to
  256 bytes.
- `cardVerification` is `"ok"` for every entry, since only verified cards are listed. **(open, §8
  Q4)** Whether to standardize an enumeration instead (the first version of this draft proposed
  `VERIFIED`, and `STALE` for a card whose signing key the registry has since seen rotated).

### 3.2 Card by AID: `GET /a2a/v1/agents/{aid}/card`

- 200: the card bytes exactly as submitted, `Content-Type: application/json`.
- `ETag`: a strong validator, the lowercase hex SHA-256 of the stored bytes in quotes.
  `If-None-Match` with a matching value (weak comparison, `*` matches) → 304.
- `Cache-Control: max-age=300`.
- 404: no verified card for this AID.
- `POST /a2a/v1/agents/card:lookup` with `{"aid": "…"}` answers the same, for clients that do not want
  the AID in the request line (and so in access logs of any proxy on the way).

This is the endpoint to use when the exact bytes matter (caching, archival, forwarding).

### 3.3 Keys: `GET /agents/{aid}/jwks.json`

A JSON Web Key Set (RFC 7517) derived from the agent's KEL (implemented: ANetCore `a2acard.JWKS`):

```json
{"keys":[{"alg":"EdDSA","crv":"Ed25519","kid":"did:anet:bafyrei…#3","kty":"OKP","use":"sig","x":"…"}]}
```

- One entry per **current** key state: the states from the last rotation (or the inception) to the
  end of the KEL. Several entries can share one key, because interaction and delegation events
  advance the key-state sequence without changing the signing key. A deactivated AID yields
  `{"keys":[]}`.
- `kid` is `did:anet:<AID>#<key_state_seq>`, exactly the `kid` a card signature uses.
- The body is RFC 8785 canonical JSON, so an unchanged KEL always yields identical bytes (usable for
  an ETag).
- The JWKS is **the registry's statement about the KEL**. A client that holds the KEL (from
  `GET /agents/{aid}/kel`, or carried in a message) SHOULD resolve the key from the KEL itself and
  treat the JWKS only as a convenience for generic A2A verifiers that understand `jku`.

### 3.4 Publication

Cards are published by the agent, authenticated with a signature by the agent's current key, as the
`a2a_card` member of the registration request (`POST /register`, signed with relayauth v2 action
`register`; see `relay-binding.md` §8.2–§8.3). The registry stores the bytes (at most 64 KiB) and
reports a per-field status that does not fail the registration:

| `card_status` | Meaning |
|---|---|
| `absent` | no card in the request |
| `invalid` | did not verify (§5.1; `card_error` names the verifier's code); not stored |
| `ok` | verified and stored: the first card, or a higher `seq` |
| `unchanged` | same `seq` and same canonical payload as the stored card |
| `conflict` | lower `seq`, or the stored `seq` with another payload (§5.2); not stored |
| `withdrawn` | the request carried `"a2a_card": null`: the registry deleted the agent's card and its index entries (a peer registry learns it as a withdrawal, §3.5). An absent member changes nothing |

### 3.5 Federation between registries: `GET /fed/v2/cards`

Registries that federate pull each other's card streams:

```json
{
  "cursor": 1234,
  "cards": [
    {"format": "a2a-card/1", "card": { … }, "kel": "<b64 KEL>", "keys": "<b64 SignedEncKeySet>",
     "home": "https://hub.example.org", "fed_seq": 1234}
  ]
}
```

- `format` names the card format, so that a registry can carry more than one (`a2a-card/1` is an A2A
  Agent Card as in this document). `withdrawal/1` tells a peer that the home registry stopped
  publishing the agent's card (the agent left, narrowed its visibility, or its card stopped
  verifying); its `card` is `{"action":"withdraw","agent_id":…,"reason":…,"at":…}` and it carries no
  `kel` or `keys`.
- `kel` and `keys` travel with the card so that the pulling registry can verify it without a second
  request; the pulling registry applies the same admission (§5), including the rule that a stored
  KEL is only ever replaced by an extension of itself.
- `home` is a statement of the serving registry. If it disagrees with the URL of the card's relay
  interface, the card (signed by the agent) wins and the disagreement is logged.
- The stream is ordered by `fed_seq`; `cursor` resumes it.

The shape is that of `GET /fed/v1/cards` (which carries anet's older card format) plus `format`. A
page holds at most 100 entries and 4 MiB (a card and a KEL may each be 64 KiB).

## 4. What a registry does not do

- It does not re-serialize, reorder, add to or strip the card.
- It does not sign the card or add a signature entry to it.
- It does not decide who may call the agent. Listing is not authorization; the agent's own policy
  applies to every request.
- It does not see task content (the anet relay binding encrypts it end to end).

## 5. Card admission

### 5.1 Verification

The reference verifier (ANetCore `a2acard.Verify`) checks, in this order, and rejects on the first
failure (error codes in parentheses):

1. The card is at most 64 KiB (`CARD_TOO_LARGE`).
2. It is a strict I-JSON object: no duplicate member names, valid UTF-8, nesting at most 128
   (`MALFORMED_JSON`); and no object has two member names that are equal under Unicode simple case
   folding (`INVALID_CARD`). The second rule exists because common struct decoders (Go's
   `encoding/json` among them) match member names case-insensitively: without it the verifier could
   check `protocolBinding` while a consumer reads `PROTOCOLBINDING`.
3. `signatures` is a non-empty array of at most 8 `{protected, signature}` entries (`UNSIGNED`,
   `CARD_TOO_LARGE`).
4. Required members and limits: `name` non-empty and at most 128 bytes; `description` at most 4096
   bytes; `version`; `supportedInterfaces` non-empty with `url`, `protocolBinding`,
   `protocolVersion`; `capabilities`; `defaultInputModes`; `defaultOutputModes`; `skills` with 1 to
   256 entries, each with a unique non-empty `id`, non-empty `name` and `description`, and 1 to 16
   non-empty `tags` (`INVALID_CARD`, `CARD_TOO_LARGE`).
5. Exactly one anet-card extension (`https://agentnetwork.org.cn/a2a/ext/anet-card/v1`) with params
   `aid`, `seq`, `issuedAt`, `notBefore`; the last three as canonical decimal strings (digits only,
   no leading zeros) — not JSON numbers, which lose precision above 2^53 (`INVALID_CARD`).
6. Every interface whose `protocolBinding` is the anet relay binding has `tenant` equal to `aid`
   (`BINDING_MISMATCH`).
7. `notBefore` is at most 300 seconds in the future (`CARD_NOT_YET_VALID`).
8. At least one signature has `alg: "EdDSA"` and `kid: "did:anet:<aid>#<seq>"`; the KEL resolved for
   `aid` replays to `aid`; key state `seq` has not been retired by a later rotation or deactivation;
   and the Ed25519 signature verifies over `BASE64URL(protected) "." BASE64URL(JCS(card without
   "signatures"))` (`BAD_SIGNATURE_HEADER`, `BINDING_MISMATCH`, `UNKNOWN_AID`, `KEY_NOT_CURRENT`,
   `INVALID_SIGNATURE`; `KEL_UNAVAILABLE` if the KEL could not be resolved — the card's validity is
   then unknown and admission should be retried rather than recorded as a rejection).

Base64url in `protected` and `signature` is decoded strictly: only the 64 alphabet characters, no
padding, no line breaks, zero trailing bits. One signature therefore has exactly one accepted
spelling, and a relaying party cannot store and serve a second byte form of a signed card.

Two payload forms are accepted, in this order: the §8.4.1 form (the card minus `signatures` and
minus every member that proto3 field presence treats as unset, from a field table transcribed from
`a2a.proto`; REQUIRED members and `optional` members that are set stay; the inside of a
`google.protobuf.Struct` is not touched), then the card as given (what a2a-go v2.6.0 signs; see
`issue-a2a-go.md` A1). The verifier records which form verified. Card producers SHOULD emit
*publish form*: REQUIRED members present and non-empty, no member at its default value, so that both
forms are the same bytes and every SDK computes the same payload. The reference signer refuses other
cards rather than rewriting them.

### 5.2 Freshness

The anet-card `seq` gives cards a total order per AID. The registry (and every consumer) keeps the
highest admitted `seq` and the SHA-256 of that card's canonical payload:

| Incoming | Decision |
|---|---|
| no stored card, or `seq` above the stored one | admit |
| `seq` equal, same canonical payload | same card (the signature may differ, e.g. after a re-sign under a rotated key): accept without change |
| `seq` equal, different payload | fork: refuse (`SEQ_FORK`) |
| `seq` below the stored one | rollback: refuse (`SEQ_ROLLBACK`) |

Accepting the "same" case matters: a restarted agent that re-registers, or a consumer that re-fetches
after its cache expires, must not be told its unchanged card is a conflict.

### 5.3 Indexes

`skill` and `tag` indexes are rebuilt only after a card is admitted, from the admitted card. The
registry's older, non-A2A listing (`GET /agents`) keeps its shape; for agents with an admitted card,
its name and capability list are derived from the card.

## 6. Relation to A2A

- The card format is unchanged A2A 1.0. The only anet-specific content is the anet-card extension
  (an ordinary `AgentExtension`, not required) and the relay binding interface.
- A generic A2A verifier (for example a2a-go `a2acrypto` with a key resolver that fetches the JWKS
  from `jku`) can verify the cards. anet's own verifiers resolve the key from the KEL instead, so the
  registry's JWKS is not trusted by them.
- The wrapper fields could be standardized independently of anet as "registry assertions". A
  registry that wants them to be verifiable could sign the wrapper; this draft does not require it
  (§8 Q3).

## 7. Security and privacy considerations

### 7.1 Trust

- The registry cannot forge a card: it does not hold the agent's key.
- The registry **can** omit cards, serve an older (but once valid) card to a client that has never
  seen a newer one, and misstate wrapper fields. Consumers that keep the high-water mark of §5.2 are
  protected against rollback for AIDs they have seen before. Completeness of search results is not
  guaranteed by any mechanism in this API.
- A JWKS served by a registry is its statement about the KEL (§3.3). A registry that serves a wrong
  JWKS could make a generic verifier accept a card the agent did not sign; anet verifiers do not use
  the JWKS.

### 7.2 Query privacy

A free-text query reveals what the client is looking for. The anet client never sends `q`: it
fetches verified cards by `skill` or `tag` and matches free text locally. `q` is provided for web
front ends.

### 7.3 Parser differentials

A card that is valid under one JSON parser and different under another is a way to show a verifier
one thing and a consumer another. The verification rules of §5.1 (strict I-JSON, no case-variant
member names, strict base64url) close the differentials known to us. See `issue-a2a-go.md` A3 for a
verifier that accepts duplicate member names.

### 7.4 Metadata

The registry sees each agent's self-description, identity, key sets, liveness and the review graph.
Agents that do not want to be discoverable do not publish a card; they remain reachable by AID for
peers that already know it.

## 8. Open questions

- **Q1 Path.** Is `/a2a/v1/agents` acceptable as a conventional registry path, or should a registry
  advertise its API location (for example in its own Agent Card)?
- **Q2 Filters.** Are `skill` and `tag` enough? Candidates: `inputMode`, `outputMode`, required
  extension URIs (e.g. "agents that accept a2a-x402").
- **Q3 Signed wrapper.** Should registry assertions be signed by the registry, so that a client can
  hold a registry to what it said?
- **Q4 `cardVerification` values and cursor presence** (§3.1): keep `"ok"` and an absent `nextCursor`
  (the reference implementation), or follow A2A ListTasks (always-present token, empty on the last
  page) and an enumeration?
- **Q5 Default-value handling** in stored cards (depends on the outcome of `issue-a2a-go.md` A1).
