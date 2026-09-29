# ADR-XXX: A `SecurityScheme` variant for sender-signature authentication

> **NOT SUBMITTED — on hold per product owner (more testing first).** Every external submission also
> needs the product owner's approval, one by one. Route (`docs/notes/0032` step 6): a comment on
> A2A #1829 first; a `[Feat]:` issue with this text only after a2a-go A10 is fixed or accepted.
>
> **License: Apache-2.0**, the A2A project's license, under which changes to the A2A specification
> are contributed: this proposal is licensed under the Apache License 2.0 alone (ANet `LICENSE`,
> condition 3).

**Status:** Proposed (draft; the ADR number is assigned on submission)

**Date:** YYYY-MM-DD (set on submission)

**Decision Makers:** A2A Technical Steering Committee

**Technical Story:** sender-authenticated custom bindings, first the anet relay binding
(`relay-binding.md` §9, `submissions/adr-relay-binding.md`); affects `specification/a2a.proto`
(`SecurityScheme`) and spec §4.5, §5.7, §7. Related: A2A #1829; `issue-a2a-go.md` A10.

This document follows the A2A ADR template (`adrs/adr-template.md`).

## Context

A2A describes how a client authenticates in the Agent Card: `securitySchemes` (named
`SecurityScheme` objects) and `securityRequirements` (which schemes, and scopes, a request must
satisfy). Clients discover requirements there (§7.3), servers authenticate every request against them
(§7.4), per-skill requirements refine them, and the extended card is gated by them (§13.3).

`SecurityScheme` is a closed `oneof` modelled on the OpenAPI 3.2 Security Scheme Object: API key,
HTTP authentication, OAuth 2.0, OpenID Connect, mutual TLS. All five authenticate the **connection or
HTTP request** to the endpoint in `AgentInterface.url`, using a credential the server (or an issuer it
trusts) established beforehand.

Some agents authenticate callers differently: **every message is signed by the sender**, with a key
bound to an identifier the sender controls, and the signature travels with the message. Examples:

- The anet relay binding (`relay-binding.md`): messages pass through a store-and-forward relay; the
  relay is not trusted with content; the recipient authenticates the sender from an Ed25519 signature
  inside the end-to-end encrypted envelope, resolving the key from the sender's key event log. There
  is no connection between client and agent at all.
- HTTP Message Signatures (RFC 9421) with keys resolved from `did:web` or `did:key` identifiers, used
  by agent platforms that want request-level non-repudiation or that sit behind TLS-terminating
  gateways.
- Future bindings over message buses (MQTT, NATS) where the "connection" is to a broker, not to the
  agent.

Today such an agent can only omit `securitySchemes`. A client reading the card then concludes that no
authentication is required, which is wrong: the agent authorizes by the caller's identifier (in anet,
by default, it refuses everyone not on an allow list), and a caller without a suitable identity cannot
talk to it at all. The agent also cannot offer an extended card, cannot state per-skill requirements,
and cannot list this binding next to a standard one without violating §5.1 ("Equivalent
Authentication").

### Why the existing schemes do not fit

| Scheme | Why it does not describe sender-signature authentication |
|---|---|
| `APIKeySecurityScheme` | A bearer secret issued by the agent in advance: requires enrolment; anyone who sees it (a relay, a TLS-terminating gateway, a log) can replay it; it does not identify the caller verifiably to anyone but the issuer; it does not protect the message content. |
| `HTTPAuthSecurityScheme` (Basic, Bearer, …) | Same as above for Basic and Bearer. The `scheme` field names an RFC 7235 `Authorization` scheme; RFC 9421 message signatures do not use `Authorization` and are not a registered authentication scheme, so there is no value to put there. And in a relay binding there is no HTTP request from client to agent to put a header on. |
| `OAuth2SecurityScheme` / `OpenIdConnectSecurityScheme` | Need an authorization server both parties trust. That server learns every caller–agent pair and becomes the de-facto gatekeeper; in a relay deployment the obvious operator of such a server is the relay itself, which is exactly the party the design keeps away from content and relationships. Access tokens are bearer credentials bound to an audience, not to a message; their lifetime (minutes) does not fit mailbox delivery (days). |
| `MutualTlsSecurityScheme` | Authenticates the TLS peer. Through a relay the TLS peer is the relay, not the caller; in store-and-forward delivery there is no TLS session between caller and agent. X.509 identities also require a CA, or reduce to pinning. |

What is missing is not a stronger credential but a different **place** for it: in the message, signed
by the sender, verifiable by the recipient without anyone else.

## Decision Drivers

- The card must state truthfully what a caller needs.
- No shared secret and no prior enrolment: parties meet for the first time.
- No central issuer: the authenticating party must not become an intermediary that learns who talks
  to whom.
- Authentication must survive intermediaries and store-and-forward delivery (hours or days).
- Compatibility with clients that do not implement the new scheme.

## Considered Options

1. **Keep omitting `securitySchemes`; define authentication inside the binding** (anet today).
   Works on the wire, but the card misrepresents the requirement, per-skill requirements and the
   extended card are unavailable, and §5.1 cannot be satisfied across bindings.
2. **Declare authentication with an `AgentExtension`** (e.g. `required: true`). Extensions are the
   mechanism for protocol additions, but clients look for authentication in `securitySchemes`, SDK
   authentication plumbing (for example a2a-go's `AuthInterceptor`/`CredentialsService`) is keyed by
   scheme name, and `securityRequirements` cannot reference an extension.
3. **Add a `SecurityScheme` variant for sender signatures** (proposed). Generic, profile-based, so one
   variant covers several concrete signature formats.
4. **Add one variant per concrete format** (an "anet relay" variant, an "RFC 9421" variant, …). Simple
   to validate, but the closed `oneof` would grow with every binding.

## Decision Outcome

**Chosen option:** "3. Add a `SecurityScheme` variant for sender signatures"

It is the only option that states the requirement where clients and SDKs already look for it
(`securitySchemes` / `securityRequirements`), keeps per-skill requirements and the extended card
available, and lets one agent satisfy §5.1 across a message-signed binding and a standard binding.
Making it profile-based (rather than option 4) keeps the closed `oneof` from growing with every
binding: the variant says "the sender signs each message", and a profile URI says how.

### Consequences

#### Positive

- Cards state authentication truthfully for message-signed bindings; clients can decide before
  contacting the agent whether they can satisfy it.
- Per-skill `securityRequirements` and the extended agent card become available to such agents.
- An agent can list a message-signed binding next to a standard binding and satisfy §5.1 by requiring
  the same scheme on both (with an HTTP signature profile on the standard binding).
- Authentication that survives intermediaries and gives recipients a verifiable, attributable record
  of who asked for what.

#### Negative

- A new `oneof` member: SDKs must add a type, and SDKs that reject unknown variants break on cards
  that use it until they are fixed; agents should keep offering an alternative requirement where
  they can during the transition. Checked on 2026-09-29 with a card that carries a Bearer scheme and
  an unknown variant: a2a-go v2.6.0 fails to parse the whole card, and its card resolver fails
  (`issue-a2a-go.md` A10, `submissions/repro/a2a-go/a10-unknown-securityscheme`); a2a-python 1.1.5
  (`parse_agent_card`) and @a2a-js/sdk 1.2.1 (`AgentCard.fromJSON`) parse the card and keep the
  unknown entry as an empty `SecurityScheme`, so the Bearer requirement stays usable.
- The meaning is delegated to profile documents; validation of a card cannot check more than the
  profile URI syntax.

#### Neutral

- No change to the operation semantics or to the other schemes.

## Implementation

### Protocol buffer

```proto
message SecurityScheme {
  oneof scheme {
    APIKeySecurityScheme api_key_security_scheme = 1;
    HTTPAuthSecurityScheme http_auth_security_scheme = 2;
    OAuth2SecurityScheme oauth2_security_scheme = 3;
    OpenIdConnectSecurityScheme open_id_connect_security_scheme = 4;
    MutualTlsSecurityScheme mtls_security_scheme = 5;
    // Authentication by a signature, made by the sender, over each message or request.
    SenderSignatureSecurityScheme sender_signature_security_scheme = 6;
  }
}

// Defines authentication by a per-message (or per-request) signature made with a key
// bound to a verifiable identifier of the sender.
message SenderSignatureSecurityScheme {
  // An optional description for the security scheme.
  string description = 1;
  // A URI identifying the signature profile. The profile specification defines what is
  // signed, where the signature and the sender identifier are carried, how the sender's
  // key is resolved from its identifier, and the replay and freshness rules.
  string profile = 2 [(google.api.field_behavior) = REQUIRED];
  // Identifier methods the agent accepts for senders, as URI scheme or DID method
  // prefixes, e.g. "did:anet", "did:key", "did:web". Empty means the profile's default.
  repeated string identifier_methods = 3;
  // JOSE algorithm names the agent accepts, e.g. "EdDSA", "ES256". Empty means the
  // profile's default.
  repeated string algorithms = 4;
}
```

Scopes in `SecurityRequirement` are not defined for this scheme; a requirement lists it with an
empty list.

### JSON example (anet relay agent)

```json
{
  "securitySchemes": {
    "anetSender": {
      "senderSignatureSecurityScheme": {
        "description": "Every message is signed by the sender's current KEL key inside the sealed envelope.",
        "profile": "https://agentnetwork.org.cn/a2a/bindings/anet-relay/v1#sender-signature",
        "identifierMethods": ["did:anet"],
        "algorithms": ["EdDSA"]
      }
    }
  },
  "securityRequirements": [{"schemes": {"anetSender": {"list": []}}}]
}
```

### Specification text (additions)

**§4.5.x `SenderSignatureSecurityScheme`.** (table generated from the proto)

**§7.3, new paragraph.** "When the selected security scheme is a `SenderSignatureSecurityScheme`, the
client obtains no credential from the server. It uses a key bound to an identifier of one of the
listed `identifierMethods`, and signs each message as the `profile` specifies."

**§7.4, new paragraph.** "An agent that declares a `SenderSignatureSecurityScheme` MUST verify the
signature on every message it accepts, as the profile specifies, and MUST identify the caller by the
verified sender identifier for authorization (§7.5)."

**§5.7 or §7, forward compatibility.** "A client that does not recognize a `SecurityScheme` variant
MUST treat security requirements that reference it as unsatisfiable by that client and MUST NOT fail
to process the rest of the Agent Card." (Today at least one SDK rejects the whole card; see
`issue-a2a-go.md` A10.)

**Profile requirements.** A profile specification MUST define: (1) the exact signature preimage and
which message parts it covers — at least the recipient, the operation or message type, the task id
when present, a message identifier, a timestamp, and the payload; (2) where the signature, the key
reference and the sender identifier are carried; (3) how the verifier resolves the key from the
identifier, including key rotation and revocation; (4) freshness and replay rules; (5) how errors are
reported.

### Profiles

- `https://agentnetwork.org.cn/a2a/bindings/anet-relay/v1#sender-signature` — `relay-binding.md` §7.4
  and §9: Ed25519 over a CoreDet-CBOR preimage covering sender, key-state sequence, recipient, message
  type, task id, message id, send and expiry time, body, the sender's KEL and key set, and the HPKE
  encryption parameters; key resolved by replaying the sender's key event log, which the message
  carries; replay table keyed by (sender, message id) until expiry.
- A profile for RFC 9421 HTTP Message Signatures with `did:web`/`did:key` resolution would make the
  scheme usable with the standard HTTP bindings. It is out of scope for this draft; we would welcome
  co-authors.

## Relation to open proposals (checked 2026-09-29)

| Proposal | Relation |
|---|---|
| **A2A #1829** minimal Ed25519 + RFC 9421 signing extension for A2A messages (open since 2026-05-09; 143 comments; no Maintainer or TSC position seen) | Complementary layers. #1829 defines a wire format: a per-request RFC 9421 signature on the standard HTTP bindings, with `keyid` resolved as `{inline, cache, resolver}` (did:key inline, did:web or an HTTPS URL by one screened fetch). This ADR defines how an Agent Card *declares* that the sender signs each message, with a `profile` URI naming the wire format. #1829's extension would be one profile (the "RFC 9421" profile this draft leaves out of scope), anet's relay envelope another. The comment on #1829 should say exactly that, and ask whether the thread's authors would co-author the RFC 9421 profile. The key-source model discussed there maps onto `identifier_methods`; the single-hop resolution rule argued there belongs in each profile's key-resolution section (profile requirement 3). |
| A2A #1575 (running implementation of agent identity, delegation and enforcement; addresses #1497, #1472, #1501), #1672, #1497 | Identity and delegation proposals that would also need the card to state "authenticate by a sender-held key"; none defines a `SecurityScheme` variant. |
| A2A #1964, #2191, #2185 (custom bindings) | Bindings where there is no HTTP request from client to agent to carry today's schemes; each would need a message-level scheme, as the relay binding does. |

## Related Decisions

- ADR-001 (ProtoJSON serialization): the JSON form of the new message (`senderSignatureSecurityScheme`,
  `identifierMethods`) follows it.

## References

- A2A specification §4.5 (Security Objects), §5.1 (Functional Equivalence), §5.7 (Field Presence),
  §7.3–§7.5 (authentication and authorization), §12.6 (custom bindings), §13.3 (extended card).
- `relay-binding.md` (the anet relay binding, the first profile).
- `issue-a2a-go.md` A10 (unknown `SecurityScheme` variants).
- RFC 9421 (HTTP Message Signatures); W3C DID Core (`did:web`, `did:key`).

## Notes

**anet today.** Network cards omit `securitySchemes`; the relay binding authenticates inside the
envelope (A2A-DESIGN §10.1 decides not to define a new scheme type unilaterally). This proposal is
how that gap would be closed upstream.

**Open questions.**

1. Should `identifier_methods` be DID-only, or also allow non-DID identifier schemes?
2. Should the scheme carry a key-discovery hint (like `jku` for card signatures), or is that always
   the profile's business?
3. Is an agent-level "the caller identifier must be on my allow list" statement useful in the card
   (so clients know they need to be introduced first), or should that stay out of band?
