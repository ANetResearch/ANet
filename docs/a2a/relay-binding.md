# A2A Custom Protocol Binding: anet relay, version 1

> **DRAFT — not submitted; requires product owner approval before any external submission.**
>
> **License: to be decided.** The product owner has not chosen a license for this text or for the
> reference implementation. Note that an *official* A2A binding must be Apache-2.0
> (A2A `docs/topics/extension-and-binding-governance.md`, "Licensing").

| | |
|---|---|
| Binding URI (`protocolBinding`) | `https://agentnetwork.org.cn/a2a/bindings/anet-relay/v1` |
| A2A protocol version | `1.0` (the only version this binding defines) |
| Status | Draft, reference implementation in progress |
| Reference implementation | ANetCore `seal`, `relayauth`, `identity`, `delegation`; ANetHub `internal/aghub` (`relay.go`, `auth2.go`, `keys.go`, `limits.go`); ANet `internal/daemon` (`seal_send.go`, `receive.go`) |
| Intended venue | Proposal issue in `a2aproject/A2A`, then (with a maintainer sponsor) an `experimental-cpb-anet-relay` repository |

Keywords MUST, SHOULD, MAY etc. are to be read as in RFC 2119 / RFC 8174.

Every rule below that a reader could implement against was checked against the reference
implementation at the time of writing. Rules marked **(designed)** are specified in the anet
design (A2A-DESIGN r3) but not yet implemented; they are listed so that reviewers see the whole
binding, and they are candidates for change.

---

## 1. Abstract

This binding lets an A2A client reach an A2A agent that cannot run an HTTPS server: an agent on a
laptop, behind NAT, inside a CLI tool, or on a device. Both parties run a small local node that
holds a self-certifying identity. A relay ("hub") stores and forwards opaque, end-to-end encrypted
envelopes between the two nodes' mailboxes. The hub authenticates senders for rate limiting, but it
never sees task content, and it does not store who sent a message.

Relative to the standard bindings the binding differs in three ways:

1. **Store-and-forward.** There is no connection between client and agent. Requests and all task
   updates travel as independent messages through a mailbox.
2. **End-to-end sealing.** Every message is signed by its sender and then encrypted to the
   recipient (HPKE, RFC 9180). The relay can check only the outer envelope.
3. **Requester-side task state.** The client's node keeps a mirror of each task it started. Get
   Task, List Tasks and Subscribe to Task are answered from that mirror without a network round
   trip; only Send Message and Cancel Task travel to the agent.

## 2. Motivation

Why the standard bindings do not cover this case (as asked for by the A2A proposal process):

- **Reachability.** JSON-RPC, gRPC and HTTP+JSON all assume the agent accepts inbound
  connections at a stable URL. Most personal and on-device agents cannot. A reverse proxy or
  tunnel makes the operator of the tunnel a TLS endpoint, i.e. a party that reads every task.
- **Confidentiality from the intermediary.** In a relay deployment the operator of the relay is a
  third party to the conversation. TLS protects each hop, not the content from the relay. The
  binding therefore encrypts end to end, and the relay is designed so that its disk, logs and HTTP
  responses contain no task content.
- **Authentication without prior enrolment.** The two parties usually have no shared account,
  OAuth provider or PKI. Each party is identified by an AID (a hash of its key inception event),
  and every message is signed with the key that the sender's key event log (KEL) designates. No
  existing A2A `SecurityScheme` expresses this (see `proposal-securityscheme.md`).
- **Offline delivery.** A task may be sent while the agent is offline and answered hours later.
  The mailbox keeps undelivered envelopes for up to 14 days.

## 3. Architecture

```
 A2A client app                                                      agent (LLM, tool, service)
      |  local A2A (JSON-RPC / HTTP+JSON on 127.0.0.1)                        ^
      v                                                                       |  local
 +----------------+   POST /relay/send     +-----------+   POST /relay/poll  +----------------+
 | requester node | ---------------------> |    hub    | <------------------ | provider node  |
 | (binding client|   sealed envelope      |  mailbox  |   sealed envelopes  | (binding server|
 |  + task mirror)| <--------------------- | per AID   | ------------------> |  + policy)     |
 +----------------+   POST /relay/poll     +-----------+   POST /relay/send  +----------------+
```

- The **binding client** is a node that holds an AID, a KEL, an encryption key set, and a
  registration at some hub (needed to send; §8.2). A plain A2A client that has none of these does
  not implement this binding. It uses its own node's local interface (JSON-RPC or HTTP+JSON on
  127.0.0.1) and the node speaks this binding on its behalf. That local interface is an ordinary
  A2A server and is out of scope here.
- The **binding server** is the provider's node. It decrypts and authenticates each message, applies
  its inbound policy (by default: refuse everyone not on an allow list), and hands accepted tasks to
  the agent.
- The **hub** is transport. It keeps one mailbox per registered AID, checks the outer envelope
  structure, and deletes an envelope when the recipient acknowledges it. Hubs may federate: a hub
  that does not hold the recipient forwards the unchanged envelope bytes to a peer hub that does.

Sealed envelopes may also travel over other transports (the reference implementation has a
direct peer-to-peer path). This document specifies the envelope and the hub transport. Other
transports that carry the same envelope bytes are out of scope, except that a receiver MUST treat
an envelope identically whatever transport delivered it (§7.7, deduplication).

## 4. Agent Card declaration

An agent reachable through this binding lists one `AgentInterface` per hub mailbox:

```json
{
  "url": "https://hub.example.org",
  "protocolBinding": "https://agentnetwork.org.cn/a2a/bindings/anet-relay/v1",
  "protocolVersion": "1.0",
  "tenant": "bafyreicg3paeuo2nt4n575adgnovtr2y7aizti7643fxhk6zbmiaaa7q7y"
}
```

- `url` is the hub's base URL: an `https` origin, optionally followed by a path prefix, with no
  trailing slash. The endpoints in §8 are resolved relative to it (`{url}/relay/send`,
  `{url}/agents/{aid}/keys`, `{url}/hub/identity`, ...). **(designed; the reference card generator is not
  yet implemented.)**
- `tenant` MUST be the agent's AID. It is the routing key: the client addresses the envelope to
  this AID and the hub delivers it to this AID's mailbox. It is also a binding: a verifier MUST
  reject a card in which an interface with this `protocolBinding` names a `tenant` other than the
  card's own AID (implemented: ANetCore `a2acard.Verify`, check 6).
- `protocolVersion` MUST be `"1.0"`.
- The card MUST carry the `anet-card` extension
  (`https://agentnetwork.org.cn/a2a/ext/anet-card/v1`, params `{aid, seq, issuedAt, notBefore}`,
  numbers as canonical decimal strings, times in unix milliseconds) and MUST be signed (A2A §8.4)
  with `alg: "EdDSA"` and `kid: "did:anet:<AID>#<key_state_seq>"` under a key that is current in
  the AID's KEL. This is what lets a client check that the `tenant` it will seal to is the party
  that published the skills. See `registry-api.md` §5 for the verification rules.
- `securitySchemes` and `securityRequirements` are omitted. Authentication is performed inside the
  binding (§9). No A2A `SecurityScheme` variant describes it; `proposal-securityscheme.md` proposes
  one.
- A client that does not implement this binding skips the interface, as A2A §8.3.2 requires. If a
  card lists this binding next to a standard binding, A2A §5.1 ("Equivalent Authentication")
  cannot be met while the two use different authentication; anet network cards therefore list
  only relay (and direct peer) interfaces, and expose standard bindings only on the local node.

## 5. Identity

- **AID.** An AID is the CIDv1 (multicodec `dag-cbor`, multihash `sha2-256`, multibase base32
  lower-case, prefix `b`) of the CoreDet-CBOR encoding of the AID's inception event with the AID
  field empty. It is 59 characters, `[a-z2-7]` after the `b`. An AID is self-certifying: nobody
  can produce a valid inception event for an AID they did not create.
- **KEL.** A key event log is a CoreDet-CBOR array of signed key events (inception `icp`,
  rotation `rot`, interaction `ixn`, delegation `drt`, deactivation `dip`) with KERI-style
  pre-rotation: each event commits to the digest of the next key. Replaying a KEL yields, for
  each position `key_state_seq`, the signing key and its status (active, rotated, deactivated).
  The normative definition is ANetCore `identity` (`identity.go`); a CDDL summary is in Appendix A.
- **Signatures.** Every signature in this binding is Ed25519 over the exact preimage bytes, made
  with the key at a declared `key_state_seq` of the signer's KEL. A verifier replays the KEL,
  checks that it replays to the expected AID, and selects the key at the declared seq.
- **Encoding.** "CoreDet-CBOR" is RFC 8949 §4.2.1 Core Deterministic Encoding (shortest integer
  and length forms, definite lengths, map keys sorted bytewise by their encoding) with no floating
  point NaN/Infinity, no tags, and no duplicate map keys. All maps in this binding use unsigned
  integer keys. All times are unix milliseconds.

## 6. Encryption key sets

A node publishes its encryption public keys as a signed object, separate from its identity key.

### 6.1 Objects

```cddl
EncKey = {
  1 => bstr .size 16,     ; kid = SHA-256("anet-enc-kid/v1" || u8(suite) || pub)[0..16]
  2 => uint .le 255,      ; suite (§7.1); 0 is not assigned
  3 => bstr,              ; pub; 32 bytes for suite 1
  4 => uint,              ; not_before (unix ms)
  5 => uint,              ; not_after  (unix ms, exclusive); 0 < not_after - not_before <= 30 days
}

EncKeySet = {
  0 => "anet.enckeys/1",  ; type; a new field requires a new type string
  1 => tstr,              ; aid
  2 => uint,              ; seq = max(now_ms, previous_seq + 1); consumers keep a high-water mark on it
  3 => [1*4 EncKey],      ; ascending not_before, distinct kids
  4 => uint,              ; issued_at (unix ms)
}

SignedEncKeySet = {
  1 => bstr,              ; set: CoreDet-CBOR(EncKeySet); the signed bytes
  2 => uint,              ; key_state_seq of the signer
  3 => bstr .size 64,     ; Ed25519 over `set`
}
```

A decoder MUST reject `set` bytes that are not the CoreDet encoding of the decoded EncKeySet
(unknown fields, non-canonical encodings). Otherwise two byte strings could carry one set, and the
high-water rule in §6.3, which compares bytes, would misreport a fork.

### 6.2 Verification

`VerifyEncKeySet(signed, expectAID, kel, now)` succeeds only if all hold:

1. `kel` replays; the AID it replays to, `set.aid` and `expectAID` are all equal. `expectAID` is
   what the caller already believes: the intended recipient when sending, the authenticated sender
   when receiving. Without this check a relay could answer a key lookup for AID X with its own
   valid KEL and key set, and the sender would encrypt to the relay.
2. The AID is not deactivated, and `key_state_seq` names a key state that is active (the key in
   force at the head of the KEL). A key set is a present-tense statement; no rotation grace applies.
3. The Ed25519 signature over `set` verifies under that key.
4. The set is well formed (§6.1) and at least one key satisfies
   `not_before <= now + 5 min < not_after`.

### 6.3 Freshness (high-water mark)

A consumer stores, per AID, the highest `seq` it accepted and the exact `set` bytes. For an
incoming set with sequence `s`:

| Condition | Decision |
|---|---|
| nothing stored, or `s` > stored | verify (§6.2); on success replace |
| `s` < stored | ignore (rollback) |
| `s` == stored, identical `set` bytes | same object: verify again; on success refresh cache time |
| `s` == stored, different bytes | fork: ignore and count |

A hub accepting a publication applies the same rule: replace → 200, same → 200 unchanged,
rollback or fork → 409.

### 6.4 Key choice and key ring policy

- A sender seals to the key, among keys of a suite it implements with
  `not_before <= now < not_after`, that has the largest `not_before`. If none is valid now but one
  becomes valid within 5 minutes, the sender uses that one.
- The reference node creates a new key every 7 days, valid for 14 days, and keeps the private half
  for 15 days after `not_after`. A message is therefore decryptable by whoever obtains the
  recipient's key store for at most 29 days after it was sent (§15.2).

## 7. The sealed envelope

### 7.1 Suites

| suite | KEM | KDF | AEAD | Status |
|---|---|---|---|---|
| 1 | DHKEM(X25519, HKDF-SHA256) `0x0020` | HKDF-SHA256 `0x0001` | ChaCha20-Poly1305 `0x0003` | implemented |
| 2 | MLKEM768-X25519 `0x647a` | HKDF-SHA256 | ChaCha20-Poly1305 | reserved, not implemented |

HPKE mode is Base (RFC 9180 §5.1.1). An envelope naming an unimplemented suite is rejected. A key
set may list keys of an unimplemented suite; senders skip them.

### 7.2 Outer envelope (visible to the hub)

```cddl
SealedEnvelope = {
  1 => 1,                 ; v: envelope version
  2 => tstr,              ; to: recipient AID (non-empty)
  3 => uint .le 255,      ; suite
  4 => bstr .size 16,     ; kid of the recipient key sealed to
  5 => bstr,              ; enc: HPKE encapsulated key; 32 bytes for suite 1
  6 => bstr,              ; ct: HPKE ciphertext of CoreDet-CBOR(SealedInner); non-empty
  ; key 7 is reserved (delivery token for sealed-sender); v1 senders MUST NOT send it
  * ext => any            ; ext = uint .ge 64: ignorable
}
```

Receivers check `v` first and report an unknown version as such. Any key in 0..63 other than 1..6
is a must-understand key this version does not know: the envelope is rejected. Keys 64 and above
are ignored.

HPKE parameters:

```
info = CoreDet-CBOR({1: "anet-relay/v1", 2: to, 3: suite, 4: kid})
aad  = "" (empty)
```

Binding `to`, `suite` and `kid` into `info` means a ciphertext cannot be moved under a different
outer header without AEAD failure. Example: for `to = "bafyreigoldenrecipient"`, suite 1, the
`info` bytes begin `a4 01 6d 616e65742d72656c61792f7631 02 76 …` (vector VEC-SEALED-1, §14).

### 7.3 Inner message (inside the ciphertext)

```cddl
SealedInner = {
  1  => tstr,             ; from: sender AID
  2  => uint,             ; ksn: sender key_state_seq the signature is made under
  3  => tstr,             ; to: recipient AID; MUST equal outer key 2
  4  => tstr,             ; type (§10.2)
  5  => tstr,             ; ix: interaction id = A2A task id (§10.3)
  6  => bstr .size 16,    ; mid: message id; (from, mid) is the replay key
  7  => uint,             ; ts: send time (unix ms), non-zero
  8  => uint,             ; exp: expiry (unix ms); ts <= exp <= ts + 15 days
  9  => bstr,             ; body: type-specific CoreDet-CBOR (§10.2)
  10 => bstr,             ; kel: sender KEL (CoreDet-CBOR); <= 65536 bytes, 1..256 events
  12 => bstr,             ; keys: sender SignedEncKeySet (CoreDet-CBOR)
  20 => bstr .size 64,    ; sig: Ed25519 over the preimage (§7.4)
  ? 21 => bstr,           ; pad: zero bytes; not signed (§7.5)
  * ext => any            ; ext = uint .ge 64: ignorable, but inside the signature
}
```

- Keys 0..63 are must-understand. A receiver MUST reject an inner containing any key in 0..63 it
  does not know (in v1: anything other than 1–10, 12, 20, 21). Adding a must-understand field
  requires a new outer `v`.
- Every listed field except `pad` is required. Each field MUST have exactly the CBOR major type
  shown; in particular a CBOR `null` is not an empty string.
- **Every** inner message carries the sender's full KEL and current key set. The receive path
  therefore makes no network request (a design requirement: a hub that fails or lies about keys
  cannot block or redirect a reply), and a node that only ever sends requests still gives its peer
  what is needed to encrypt the answer.
- The sender sets `exp = ts + 14 days`, which equals the hub's undelivered-message TTL (§8.4).

### 7.4 Signature

The signature preimage is built from the inner map **as received**, not from a re-encoding of
decoded fields:

```
preimage = CoreDet-CBOR( inner_fields \ {20, 21}
                         ∪ {0:  "anet-relay-sig/v1",
                            30: outer.enc,          ; bstr
                            31: outer.kid,          ; bstr
                            32: outer.suite} )      ; uint
sig      = Ed25519(sk_at(from, ksn), preimage)
```

Values are copied as encoded bytes; CoreDet only sorts the keys. A receiver can therefore verify
extension keys (>= 64) it does not understand. Keys 0, 30, 31 and 32 are reserved for the preimage;
an inner that carries them fails the must-understand check before the preimage is built.

Covering the outer `enc`, `kid` and `suite` means a recipient that decrypts a message cannot
re-encrypt it to a third party and present it as addressed to that party: the new `enc` is not
what the sender signed, and `to` inside the signature names the original recipient.

The signer's clock and key rotation: a signature made under a key state that a later rotation
retired is accepted only if `ts < SupersededAt` (the retiring event's timestamp) **and**
`now − SupersededAt <= rotation_grace` (reference default: 1 hour). This bounds the damage of a
stolen pre-rotation key to back-dated messages within the grace window.

### 7.5 Padding

Before encryption, the sender sets `pad` to zero bytes such that the length of the CoreDet
encoding of the whole inner map is a Padmé length (Nikitin et al., PETS 2019): with
`E = floor(log2 n)` and `S = floor(log2 E) + 1`, `n` is rounded up so that its low `E − S` bits are
zero. When no pad length reaches a Padmé value exactly (because a CBOR byte-string header grows at
lengths 24, 256, 65536, 2^32), the next Padmé value is used. Receivers MUST reject a `pad` that
contains a non-zero byte and MUST accept an inner without `pad`.

### 7.6 Sending

1. Resolve the recipient key set: a stored set (§6.3) while it verifies and holds a usable key;
   otherwise `GET {hub}/agents/{to}/keys` (§8.3) and `VerifyEncKeySet(keyset, to, kel, now)` with
   the returned KEL, which MUST extend any KEL already stored for `to` (prefix check, event by
   event). If no key can be resolved the send fails; there is no unencrypted fallback.
2. Build the inner (fresh random `mid`, own KEL and key set), sign (§7.4), pad (§7.5), encrypt.
3. Submit the same envelope bytes to every transport tried. A retry of one logical message MUST
   resend the bytes from the first attempt, never a re-sealed copy, so that the receiver's replay
   table recognises it. After `exp` the sender stops retrying.

### 7.7 Receiving

Failures are either **permanent** (same bytes fail the same way on every attempt: acknowledge and
drop) or **temporary** (storage error, cancellation, rate limit, "message for a task that has not
arrived yet": do not acknowledge, so the hub keeps the envelope). After `exp` a temporary failure
becomes permanent.

| Step | Check | Failure |
|---|---|---|
| 1 | outer decodes; `v == 1`; no unknown key 0..63; `to` == own AID; suite implemented; kid 16 bytes; `enc` of suite length; `ct` non-empty | permanent |
| 2 | kid is a key the node holds (valid or within retention) | permanent (`sealed-to-unknown-key`) |
| 3 | HPKE open with `info` from §7.2 | permanent |
| 4 | inner decodes as an integer-keyed map; must-understand and type checks (§7.3); inner `to` == outer `to`; KEL within size caps; `pad` all zero | permanent |
| 5 | `exp >= ts`, `exp − ts <= 15 days`, `now <= exp`, `ts <= now + 5 min` | permanent |
| 6 | resolve sender KEL: the carried KEL must replay to `from`. If a KEL is stored for `from`: carried extends stored → candidate update; stored extends carried → use stored; neither → reject (fork). No record → use carried (first use) | permanent; storage error temporary |
| 7 | signature (§7.4) under the resolved KEL at `ksn`, with the rotation rule | permanent |
| 8 | attached key set: `VerifyEncKeySet(keys, from, kel, now)` and §6.3. Advisory: a failure here does not reject the message | — |
| 9 | authorization: inbound policy for new tasks; for all other types the task must exist, its peer must equal `from`, and the role must fit the type (§10) | permanent; see §10.5 for the unknown-task window |
| 10 | deduplication and processing: `(from, mid)` seen before → acknowledge, do not process again. Otherwise process, and record `(from, mid, exp)` in the same storage transaction as the business write | temporary on storage error |

Delivery is at least once. The hub deletes an envelope only when acknowledged (§8.3) or when the
undelivered TTL expires (§8.4); a node acknowledges only after step 10 committed or after a permanent failure. The same envelope arriving
over two transports is processed once.

## 8. Hub transport (HTTP)

### 8.1 Versioning

Every response of the endpoints in §8.3 carries `X-ANet-Wire: 2`, except `GET /hub/identity`, which
the reference hub serves outside its versioned router (a client learns the wire version from any
other response, for example the first `/relay/poll`). A request to `/relay/*` without
`X-ANet-Wire` or with a value below 2 receives **426 Upgrade Required** with a JSON body naming the
required version (`required_wire`). A request to any of these endpoints declaring a version above
the hub's receives 400. A node that finds a hub below wire 2 refuses to use it. There is no fallback
to the earlier, unencrypted relay.

### 8.2 Request authentication (relayauth v2)

Signed requests carry four headers, each exactly once:

| Header | Value |
|---|---|
| `X-ANet-AID` | signer AID |
| `X-ANet-TS` | signing time, unix ms, canonical decimal (no sign, no leading zeros) |
| `X-ANet-Seq` | signer `key_state_seq`, canonical decimal |
| `X-ANet-Sig` | Ed25519 signature, base64url without padding, exactly 86 characters (strict: no line breaks, zero trailing bits) |

The signature covers:

```
preimage = "anet-relay/v2/" action "/" aid "/" hubAID "/" decimal(ts) "/"
           base64url_nopad( SHA-256( method || 0x00 || pathAndQuery || 0x00 || body ) )
```

- `action` is one of `send`, `poll`, `ack`, `register`, `keys`, `profile`, `visibility`,
  `deregister`, `p2p`, `balance`, `ledger`, `redemptions`.
- `hubAID` is the AID of the hub addressed, obtained from `GET {url}/hub/identity`; a signature is
  valid at one hub only.
- `method` is the HTTP method as sent; `pathAndQuery` is the origin-form request target exactly as
  sent (escaped path, `?`, raw query); `body` is the raw request body (empty if none). The hub reads
  the body (under the endpoint's size cap) and hashes it before decoding it.
- The hub accepts `|now − ts| <= 5 min` and verifies the signature against the KEL it holds for the
  signer (identity check at `msgTime = ts`). For `register` the signer may not be known yet, so the
  hub verifies against the KEL in the request body, which must replay to the `aid` being registered
  and must extend any KEL the hub already holds for it (409 otherwise). It then records
  `(aid, sig)` until `ts + 5 min` and refuses a second use (401). If its replay cache is full it
  answers 503 rather than evict.

This authentication is between the sender and the hub. It exists so that the hub can rate-limit
and bound storage per sender (§8.4). It is **not** the A2A-level authentication of the sender to
the agent; that is the inner signature (§7.4, §9).

### 8.3 Endpoints

All request and response bodies are JSON. Binary values (`envelope`, `keyset`, `kel`) are
**standard** base64 with padding (RFC 4648 §4).

| Endpoint | Auth (action) | Request | Response |
|---|---|---|---|
| `GET /hub/identity` | none | — | `{"aid": "<hub AID>", "kel": "<b64 KEL>"}` |
| `POST /register` | `register` (signer = `aid` in body) | `{"aid", "name", "caps", "kel", "enc_keys"?, "a2a_card"?, …}` | `{"aid", "status", "keys_status", "keys_error"?, "card_status", "card_error"?}`; a key set or card that is refused is reported per field and does not fail the registration. 403 when the hub admits by invitation only and the AID is new to it; 409 when `kel` does not extend the stored KEL; 429 per client address |
| `GET /agents/{aid}/keys` | none | — | 200 `{"aid", "keyset", "kel"}`; 404 registered here without a key set, or held neither here nor at any peer hub; 429 (federated lookups, per client address); 502 federated lookup failed |
| `POST /agents/{aid}/keys` | `keys` (signer = path AID) | `{"keyset": "<b64>"}` | 200 `{"aid", "keys_status": "ok" \| "unchanged"}`; 400 does not verify; 401 signer is not the path AID or not registered; 409 rollback or fork (§6.3) |
| `POST /relay/send` | `send` (signer must be registered at this hub) | `{"to_aid": "<AID>", "envelope": "<b64>"}` | 200 `{"id": n, "status": "queued"}` or `{"status": "forwarded", "via_hub": "<peer hub AID>"}`, optionally `"recipient_quiet": true, "warning": "…"` |
| `POST /relay/poll` | `poll` (mailbox = signer) | `{"limit": n}` (optional; default 100) | `{"messages": [{"id": n, "envelope": "<b64>"}]}`, oldest first |
| `POST /relay/ack` | `ack` (mailbox = signer) | `{"ids": [n, …]}` | `{"acked": count}` |

`/relay/send` checks, in order: the sender's rate bucket (refused before the body is read if
empty), authentication, the envelope size, the outer structure (§7.2 step-1 checks, and outer `to`
== `to_aid`), then routing. It stores `id, to_aid, size, created_at, envelope` and nothing about the
sender.

`/relay/poll` returns envelopes up to a byte budget per response (default 48 MiB); the first
envelope is always returned even if it alone exceeds the budget. Polling is also the recipient's
liveness signal: a send to a recipient that has not polled for a long time is still queued but the
response says so (`recipient_quiet`).

`/relay/ack` deletes only rows in the caller's own mailbox; unknown ids are skipped; ids are never
reused.

### 8.4 Errors and limits

| Status | Meaning (`/relay/send` unless noted) |
|---|---|
| 400 | body malformed, envelope not base64, outer structure invalid, `to` ≠ `to_aid`; any endpoint: `X-ANet-Wire` above the hub's |
| 401 | authentication headers missing or invalid, signer not registered here, signature replayed |
| 404 | recipient not registered here and no peer hub accepted it |
| 413 | envelope above the size limit, or body above the endpoint cap |
| 426 | `/relay/*` without `X-ANet-Wire: 2` |
| 429 | sender's token bucket empty; `Retry-After` in seconds |
| 502 | forwarding to a peer hub failed |
| 503 | hub has no identity, or replay cache full |
| 507 | recipient mailbox full (locally or at the peer hub) |

Default limits of the reference hub (each a hub flag): envelope 96 MiB; per-sender bucket 20/s,
burst 200; per-recipient mailbox 5000 envelopes or 1 GiB; undelivered TTL 14 days; poll budget
48 MiB; `/register` 10/min, burst 20 per client address; federated key lookups 60/min, burst 30 per
client address. The hub deletes an acknowledged envelope immediately and uses SQLite
`secure_delete`, so deleted envelopes do not linger in the database file.

### 8.5 Federation (informative)

A hub that does not hold `to_aid` may forward the envelope to peer hubs (`POST /fed/v1/forward`,
forward envelope v2, signed by the origin hub; the payload is the unchanged sealed envelope with
its CID; there is no sender, message kind or task id in the forward envelope). A hub asked for the
keys of an AID it does not hold asks its peers (`GET /fed/v2/keys/{aid}`, hub-signed, exact AID
only, answer not stored or indexed). Both are hub-to-hub matters; the binding client sees only the
responses of §8.3. Because the key set is signed and verified with `expectAID`, a peer hub's answer
needs no trust.

## 9. Authentication and authorization (A2A §12.6)

- **How credentials are transmitted.** There are no bearer credentials. Each message proves its
  sender by the inner Ed25519 signature (§7.4) under the sender's KEL, which the message itself
  carries. The provider node identifies the A2A caller as the AID `from`.
- **What is authenticated.** Sender identity, recipient, message type, task id, message id, send
  and expiry time, body, the sender's KEL and key set, and the HPKE `enc`/`kid`/`suite` of this
  particular encryption.
- **Authorization** is the provider's policy (A2A §7.5). In the reference node the default is
  `closed`: a new task from an AID that is not on the operator's allow list is refused
  (`TASK_STATE_REJECTED`, §11), except calls to skills the operator declared public. Every later
  message in a task must come from the task's peer (step 9).
- **Challenges.** The binding has no challenge/response. A refused request is answered with a
  signed, encrypted status message (§11). Refusal notices are rate limited per peer and globally;
  when the limit is reached the request is dropped silently.
- **First use.** The first time a node sees an AID it accepts the KEL the message carries
  (trust on first use). After that it accepts only extensions of the stored KEL (§15.2, item 4).
- `TASK_STATE_AUTH_REQUIRED` (in-task authorization, A2A §7.6) is not used by the reference
  implementation; it is carried transparently if a provider emits it (**open question** §17).

## 10. Operations

### 10.1 Operation mapping

| A2A operation | How it is served | Wire message |
|---|---|---|
| SendMessage, new task (no `taskId`) | relayed | `anet.delegate/1` |
| SendMessage, existing task | relayed | `anet.message/1`, kind `text` |
| SendStreamingMessage | relayed as SendMessage; events from the local mirror (§12) | as SendMessage |
| GetTask | requester node, from its task mirror | none |
| ListTasks | requester node, from its task mirror | none |
| CancelTask | relayed; local state per §10.6 | `anet.message/1`, kind `cancel` |
| SubscribeToTask | requester node, from its task mirror (§12) | none |
| Create / Get / List / Delete push notification config | not supported: `PushNotificationNotSupportedError`; `capabilities.pushNotifications` is `false` | none |
| GetExtendedAgentCard | not supported: `capabilities.extendedAgentCard` is absent, so the call is `UnsupportedOperationError` (A2A §3.3.4) | none |

Every core operation is available to the client (A2A §12.1). Three of them are answered without a
round trip because, under store-and-forward, the client's node is the only party that can answer
promptly, and the provider's node already pushes every state change to it. The consequence is that
GetTask reports **the last state the requester's node has received**, which may lag the
provider's.

Local answers are scoped (A2A §13.1): the node answers only for tasks it started, with the AID of
the selected interface's `tenant`; any other id is `TaskNotFoundError`, checked before existence.

In the reference node the task mirror (the requester's interaction store) and the per-task event
bus that these local answers read are implemented; the local A2A interface that exposes them to
A2A clients, and therefore the exact local error codes of this section, are **(designed,
A2A-DESIGN §11)**.

### 10.2 Inner message types and bodies

| `type` | Direction | Body |
|---|---|---|
| `anet.delegate/1` | requester → provider | `DelegateReq` |
| `anet.message/1` | both | `ChatMsg`; kinds `text`, `end_request`, `cancel`, `stream_preview` |
| `anet.status/1` | provider → requester | `StatusMsg` |
| `anet.result/1` | provider → requester | `ResultResp` |

```cddl
DelegateReq = {
  1 => bstr,              ; task_doc: requester-signed anet TaskDoc (ANetCore tsir); the request CID anchor
  2 => AObjEnvelope/null, ; detached signature over task_doc
  3 => bstr,              ; kel: requester KEL as stated; empty, equal to, or a prefix of the resolved KEL
  4 => tstr,              ; interaction_id; MUST equal inner ix
  ? 5 => [* Attachment],  ; files sent with the request
  ? 6 => bstr,            ; payment: x402 PaymentPayload (JSON), prepaid variant
  ? 7 => tstr,            ; context_id: A2A contextId
  ? 8 => bstr,            ; metadata: JSON object (A2A Message.metadata + reserved keys, §10.4)
}

ChatMsg = {
  1 => tstr,              ; kind: "text" / "end_request" / "cancel" / "stream_preview"
  ? 2 => tstr,            ; body: text
  ? 3 => [* Attachment],
  ? 4 => uint,            ; stream_seq      (stream_preview only)
  ? 5 => int,             ; stream_at_ms    (stream_preview only)
  ? 6 => tstr,            ; reasoning_body  (stream_preview only)
  ? 7 => int,             ; reasoning_stream_at_ms
  ; key 8 is unassigned and stays unassigned
  ? 9 => tstr,            ; msg_id: A2A messageId as minted by the sending node
  ? 10 => bstr,           ; metadata: JSON object (§10.4)
}

StatusMsg = {
  1 => tstr,              ; state: "submitted" / "working" / "input-required" / "rejected" / "canceled" / "failed"
  ? 2 => tstr,            ; text: human-readable explanation
  ? 3 => bstr,            ; metadata: JSON object (§10.4)
  4 => uint,              ; at: when the provider entered the state (unix ms)
}

ResultResp = {
  1 => tstr,              ; status: "done" / "failed"
  ? 2 => bstr,            ; deliverable
  ? 3 => bstr,            ; receipt: provider-signed evidence receipt (ANetCore evidence)
  ? 4 => bstr,            ; kel: provider KEL, to verify the receipt
  ? 5 => bstr,            ; metadata: JSON object (§10.4)
}

Attachment   = { 1 => tstr, ? 2 => tstr, 3 => int, 4 => tstr, ? 5 => bstr }
               ; name, mime, size, cid = CIDv1(raw, sha2-256) of data, data
AObjEnvelope = { 1 => tstr, 2 => uint, 3 => -8, 4 => bstr .size 64, ? 5 => tstr }
               ; signer_aid, key_state_seq, alg (EdDSA), sig, preimage_ref
```

A receiver MUST drop a `StatusMsg` whose `state` is not one of the six values rather than map it to
a known state.

Unlike the inner envelope map (§7.3), the body maps are extensible without a version change: a
receiver ignores body keys it does not know. A new body field is therefore always optional, and a
field whose meaning a receiver must understand needs a new `type` string instead.

Note for reviewers: the request body is anet's own signed task document, not an A2A `Message`.
This is what the reference implementation carries today, because the provider signs a receipt over
the request CID. A generic profile that carries A2A ProtoJSON directly is an **open question**
(§17 Q1).

### 10.3 Identifiers

- **Task id.** The A2A task id is the interaction id `ix`. It is minted by the requester's node
  (reference format `ix_` followed by 32 hex digits) and sent in the first message. To the local A2A
  caller this is still server-generated: the requester's node is the A2A server it talks to.
  A provider MUST NOT accept an `anet.delegate/1` whose `ix` it already holds with a different
  peer or role (dropped silently as a collision); a repeat from the same peer is the idempotent
  redelivery path.
- **Context id.** `DelegateReq.context_id` carries the client's `contextId` unchanged; if the
  client gave none, the requester node mints one (`ctx_` + 32 hex digits).
- **Message id.** `ChatMsg.msg_id` is minted by the sending node and is the `messageId` under which
  the message appears in the task history on both sides. The local client's own `messageId` is kept
  by the requester's node (A2A-DESIGN §11.5: `metadata["a2a.messageId"]`, used to deduplicate
  client retries) and is not an identifier on the wire. The envelope `mid` is a separate,
  per-envelope replay key.

### 10.4 Metadata and service parameters (A2A §12.3)

The `metadata` field of each body is a JSON object. For requests it is the A2A
`Message.metadata`, to which the sending node adds reserved keys:

| Key | In | Meaning |
|---|---|---|
| `a2a.serviceParameters` | DelegateReq, ChatMsg | service parameters of the request (below) |
| `anet.state` | ChatMsg from provider | `"working"` if the message does not ask for input |
| `anet.state` | ResultResp | the state the result puts the task in: `completed`, `failed`, `rejected` or `input-required` (§10.5) |
| `anet.effect_status` | ResultResp of a structured skill call | the provider's statement about the call's effect (`OK`, `UNVERIFIED`, `FAILED`, `UNAVAILABLE`, `PAYMENT_REQUIRED`); `completed` does not imply `OK` |
| `anet.reason` | StatusMsg, ResultResp | machine-readable reason (§11) |
| `anet.retry_after_ms` | StatusMsg, ResultResp | when a refused request may succeed if retried |
| `anet.inbound` | StatusMsg | `"pending_approval"` when the task waits for the operator |
| `anet.a2aError` | StatusMsg | A2A error name (§11) |
| `x402.payment.*` | all | a2a-x402 v0.2 keys (see `x402-scheme-anet-credit.md`) |

**Service parameters.** The binding has no headers between client and agent (the hub's HTTP
headers are hub-level, §8.2). Service parameters are therefore carried as a JSON object under the
metadata key `a2a.serviceParameters`:

```json
{"a2a.serviceParameters": {"A2A-Version": "1.0",
                           "A2A-Extensions": ["https://github.com/google-agentic-commerce/a2a-x402/blob/main/spec/v0.2"]}}
```

- Names are compared case-insensitively; senders SHOULD use the spelling of A2A §3.2.6.
- A value is a string, or an array of strings for a parameter with several values. `A2A-Extensions`
  is an array of URIs (equivalent to the comma-separated header form).
- Values are UTF-8 strings. There is no size limit other than the message limit.
- The binding reserves no service parameter names of its own.
- Absent `A2A-Version` means `1.0`: this binding exists only for A2A 1.0, so the A2A §3.6.2 default
  of `0.3` for an empty value cannot apply to it. A provider that receives a version other than 1.x
  answers `VersionNotSupported` (§11).

**(designed)** The key and format come from A2A-DESIGN §3.4; the reference node does not yet write
or read `a2a.serviceParameters` (it will when its local A2A interface lands). The A2A
custom-binding guide suggests `a2a-service-parameters` as an example name; see §17 Q2.

### 10.5 Task lifecycle on the wire

| Event at the requester | Wire | Requester task state |
|---|---|---|
| client sends first message | `anet.delegate/1` | `submitted` |
| client sends further input | `anet.message/1` `text` | `working` |
| client cancels | `anet.message/1` `cancel` | `canceled` (see §10.6 for paid tasks) |
| client asks the provider to wrap up | `anet.message/1` `end_request` | unchanged; provider completes |

| Event at the provider | Wire | Requester task state |
|---|---|---|
| provider replies, needs input | `anet.message/1` `text` | `input-required` |
| provider replies, keeps working | `anet.message/1` `text`, `anet.state: "working"` | `working` |
| provider reports a state | `anet.status/1` | the state carried |
| provider answers with a result | `anet.result/1` | `anet.state` of the result when it is `completed`, `failed`, `rejected` or `input-required`; otherwise `failed` for status `failed`, `completed` for `done` |

- No transition leaves a terminal state (`completed`, `failed`, `canceled`, `rejected`). A message
  that would do so is recorded but does not change the state.
- A `text`, `cancel` or `end_request` message for a task the provider does not know may simply have
  overtaken its `anet.delegate/1` (different transports, retries). The provider treats it as a
  temporary failure for 10 minutes after the message `ts` (the envelope stays in the mailbox). After
  that it answers `status{failed, anet.a2aError: "TaskNotFound"}`, rate limited like refusal notices.
- `end_request` asks the provider to finish a conversational task; the provider node completes it
  on its own and returns `anet.result/1`. It has no A2A counterpart and is not needed by A2A clients.
- A result is normally final. The exception is a priced structured skill call answered with a
  quote (`anet.effect_status: PAYMENT_REQUIRED`, `anet.state: input-required`). The reference node
  currently pays by sending the same work again as a new, prepaid task (`DelegateReq.payment`) and
  leaves the quoted task in `input-required`. The a2a-x402 same-task flow, in which the payment and
  the final result arrive on the quoted task itself, is **(designed, `x402-scheme-anet-credit.md`)**.

### 10.6 Cancel Task

The requester node sends `cancel` and returns the task from its mirror:

- Not yet paid, or no payment involved: the local state becomes `canceled` immediately; the
  provider cancels its work if it can and confirms with `status{canceled}`. No receipt is issued.
- A payment was already submitted (a2a-x402 `payment-submitted`): the local state does not change
  (normally `working`); the cancel is recorded in the task's message log, and the outcome is
  whatever the provider reports next. A provider that has received or settled a payment does not
  cancel. **(designed, A2A-DESIGN §11.5)** The local interface marks such a task with metadata
  `anet.cancel_requested: true`.
- The task is already terminal: `TaskNotCancelableError`.

### 10.7 Message parts

Mapping from A2A `Part` to the wire in the reference node **(designed, A2A-DESIGN §11.5)**:

- text parts are concatenated into the request goal (`anet.delegate/1`) or the `ChatMsg.body`;
- a file part with inline bytes becomes an `Attachment` (reference cap 64 MiB each; `cid` is the
  CIDv1 `raw` of the bytes and is re-checked by the receiver);
- a part referring to a URL (any scheme) is refused with `InvalidParams`: nodes do not fetch;
- a data part `{"skill": …, "args": …}`, or `metadata["anet.skill"]`, selects a skill as a
  structured capability call.

Other data parts are not defined in v1 (§17 Q3).

## 11. Error mapping (A2A §12.4)

Errors surface in two places. Errors the requester's node can decide are returned synchronously to
its caller with the standard codes of the local binding. Errors only the provider can decide
travel back asynchronously as `anet.status/1` with metadata `anet.a2aError` = the A2A error name
**without** the `Error` suffix, and put the task in `failed` (or `rejected`, before the task was
accepted).

| A2A error | Where decided | Representation |
|---|---|---|
| `TaskNotFoundError` | requester (unknown or out-of-scope id) | local error |
| | provider (message for an unknown task, after the 10-minute window) | `status{failed, anet.a2aError: "TaskNotFound"}` — implemented |
| `TaskNotCancelableError` | requester (task terminal) | local error |
| `PushNotificationNotSupportedError` | requester | local error |
| `UnsupportedOperationError` | requester (input to a terminal task, subscribe to a terminal task, GetExtendedAgentCard) | local error; a provider drops input to a terminal task without reply |
| `ContentTypeNotSupportedError` | provider | `status{rejected, anet.a2aError: "ContentTypeNotSupported"}` — not emitted by the reference implementation |
| `InvalidAgentResponseError` | requester | not surfaced in v1: a malformed or unverifiable provider message is dropped and counted (§7.7), and the task keeps its last state |
| `ExtendedAgentCardNotConfiguredError` | — | not used: the binding offers no extended card and does not declare `capabilities.extendedAgentCard`, so A2A §3.3.4 requires `UnsupportedOperationError` instead |
| `ExtensionSupportRequiredError` | provider (a required extension not in `A2A-Extensions`) | `status{rejected, anet.a2aError: "ExtensionSupportRequired"}` — **(designed)** |
| `VersionNotSupportedError` | provider | `status{rejected, anet.a2aError: "VersionNotSupported"}` — **(designed)** |

Policy refusals are not A2A errors. They are `TASK_STATE_REJECTED` with `anet.reason`:

| `anet.reason` | Meaning |
|---|---|
| `not_accepting` | the provider accepts tasks only from its allow list (also what a denied peer sees under the default policy, so that denial is not observable) |
| `denied` | the sender is on the deny list (policies other than the default) |
| `capability_not_public` | a structured call to a skill that is not public |
| `pending_full`, `pending_expired`, `operator_rejected` | the task was queued for operator approval and then refused |
| `sandbox_unavailable` | the provider's agent may only run sandboxed for this peer and no sandbox is available |
| admission codes, with `anet.retry_after_ms` | a public skill's rate or concurrency limit |

A task queued for approval is reported as `status{submitted, anet.inbound: "pending_approval"}`.

Transport errors from the hub (§8.4) are not A2A errors. The requester's node reports them to its
caller as a system error (A2A §3.3.2, "System Errors"), with `Retry-After` guidance where the hub
gave one. What happens to a task whose first message never reached a hub is an implementation
choice; the reference node keeps its local record of the task in `submitted` and returns the error.

## 12. Streaming under store-and-forward (A2A §12.5)

- **Mechanism.** There is no stream between requester and provider. The requester's node keeps
  the task mirror and an in-process event bus per task. `SendStreamingMessage` relays the request
  and returns the local stream; `SubscribeToTask` returns the current Task snapshot first and then
  the stream, both obtained atomically (A2A §3.1.6). Each received status, provider message or
  result updates the mirror and emits a `TaskStatusUpdateEvent` or `TaskArtifactUpdateEvent`.
  (Reference node: the mirror and the event bus, with atomic snapshot-and-subscribe, are
  implemented; the A2A streaming responses built on them are **(designed)**, see §10.1.)
- **Previews.** `stream_preview` messages are ephemeral, replace-in-place snapshots of a reply
  being written. A node MAY surface them as artifact updates; they are never stored in history and
  clients MUST NOT rely on them.
- **Ordering.** Events on one node's stream follow the order in which that node recorded them:
  state changes carry the node's per-task state sequence number (`state_seq`) and messages their
  per-task message sequence number, so a subscriber sees each state change once and in order
  (A2A §3.5.2 for that stream). The binding does **not** guarantee that provider messages arrive in
  the order sent: a hub mailbox is FIFO per recipient, but a message retried after a failure, or
  sent over another transport, can overtake an earlier one. Receivers apply the rules of §10.5
  (terminal states are final; early follow-ups wait for their task). This is weaker than the
  "events in the order they were generated" of the A2A custom-binding guide and is listed in §15.2.
- **Reconnection.** A disconnected client re-subscribes; it receives the full current Task and
  then new events. Events that occurred while disconnected are not replayed individually, but
  history and status in the snapshot reflect them. A subscriber that falls behind (its event
  buffer is full) is disconnected rather than allowed to block the node, and recovers the same way.
- **Termination.** The stream closes when the task reaches a terminal state.
- **Latency.** Update latency is the provider's send path plus the requester's poll interval (the
  reference node polls its mailbox once per second, and additionally on interactive reads; the hub
  answers each poll immediately, it does not hold the request open). A2A §3.1.2 "immediate
  feedback" is met locally (the stream starts with the submitted Task) but not end to end.

Whether an agent card with only relay interfaces should declare `capabilities.streaming: true` is
§17 Q4; the recommendation of this draft is `true`, since streaming is always available through the
local mirror.

## 13. Data type mappings (A2A §12.2)

| A2A / protobuf | This binding |
|---|---|
| message structure | CoreDet-CBOR maps with integer keys inside the envelope; JSON only for `metadata` objects |
| `bytes` | CBOR byte strings inside the envelope; standard base64 in hub JSON |
| `google.protobuf.Timestamp` | unix milliseconds (`uint`) on the wire; ISO 8601 UTC with milliseconds at the A2A surface |
| `TaskState` | `StatusMsg.state` lowercase-hyphen names: `submitted` ↔ `TASK_STATE_SUBMITTED`, `working` ↔ `TASK_STATE_WORKING`, `input-required` ↔ `TASK_STATE_INPUT_REQUIRED`, `rejected` ↔ `TASK_STATE_REJECTED`, `canceled` ↔ `TASK_STATE_CANCELED`, `failed` ↔ `TASK_STATE_FAILED`; `TASK_STATE_COMPLETED` is carried by `anet.result/1` |
| `Role` | implied by direction: requester → `ROLE_USER`, provider → `ROLE_AGENT` |
| `google.protobuf.Struct` (metadata) | JSON object in a CBOR byte string |
| numbers in metadata | JSON; values that may exceed 2^53 are carried as decimal strings |

## 14. Interoperability testing (A2A §12.8)

Test vectors for independent implementations (all in ANetCore):

- `seal/testdata/vec-sealed-1.json` (VEC-SEALED-1): one envelope from the frozen conformance
  identity to a recipient key derived with RFC 9180 `DeriveKeyPair` from
  `SHA-256("anet-seal-golden-v1/recipient-enc")`. Pins the recipient public key and kid, the HPKE
  `info` bytes, the signature preimage and the envelope bytes. Sealing is randomized, so the vector
  is checked on the opening side.
- `seal/testdata/rfc9180-a2-1.json`: RFC 9180 Appendix A.2.1 (the suite-1 cipher suite), opening side.
- `relayauth/relayauth_test.go`: pinned `PreimageV2` bytes, header names and action names.
- Deterministic test keys: `aobj.SuiteSeed()` (`SHA-256("anet-suite-test-key-v1")`) and
  `identity.SuiteController()`.

Still to be written before submission: a vector set for `DelegateReq`/`ChatMsg`/`StatusMsg`/
`ResultResp` bodies with every field set, and an end-to-end transcript (request, status, result)
with fixed keys.

## 15. Security considerations

### 15.1 Properties

- **Confidentiality from the hub.** The hub sees only the outer envelope: recipient AID, suite,
  recipient key id, HPKE `enc`, and ciphertext length rounded to a Padmé size.
- **Sender authentication and integrity.** Every message is signed by the sender's current key and
  bound to one recipient, one task, one message id and one encryption. Neither the hub nor the
  recipient can forge a message from the sender or redirect one to a third party.
- **Recipient binding of key sets.** A key set is accepted only for the AID the caller expected, so
  a hub cannot substitute its own keys for a recipient's.
- **Replay.** `(from, mid)` is recorded until `exp`; a replayed envelope is acknowledged and not
  processed. Relay authentication signatures are single-use within their window.
- **Rollback.** A stored KEL is replaced only by an extension of itself; key sets and cards only
  by higher sequence numbers.
- **Resource limits.** Per-sender rate, per-recipient quota, TTL and registration rate bound what a
  sender (or a sender that mints fresh AIDs) can make a hub store.

### 15.2 Known limitations

These are stated so that nobody reads more into "end-to-end encrypted" than it gives.

1. The hub learns, at send time, who sends to whom, when, and how much (rounded), and the client IP
   address. It does not store the sender, but it could log it. Sealed sender with delivery tokens
   (outer key 7) is future work.
2. Forward secrecy is bounded by the key lifetime: a message can be decrypted by whoever obtains the
   recipient's key store for at most 29 days after sending. There are no per-task ephemeral keys.
3. Large attachments are buffered whole and sealed in one AEAD operation (envelope limit 96 MiB).
4. First use is trusted: the first KEL seen for an AID is accepted as carried. A hub or network
   attacker can present a stale (truncated) KEL, or an older key set or card, to a node that has
   never seen the AID; rollback protection applies only to AIDs with a stored record.
5. Rotation grace: a signature by a key a rotation retired is accepted for up to 1 hour after the
   rotation if its `ts` predates the rotation. Messages still in a mailbox after that are refused.
   The reference product has no user-facing key rotation yet.
6. The hub can delay or drop envelopes, or refuse service. Availability is not protected.
7. GetTask answers from the requester's mirror and can lag the provider's actual state.
8. Blocking sends: if the client waits for a terminal state and the provider is offline, the wait
   can be long; a client timeout does not cancel the task, and a retry creates a second task.
9. Payment metadata (payer, payee, amount, time) is visible to the hub that settles payments; see
   `x402-scheme-anet-credit.md`.
10. End-to-end encryption protects content from the relay, not from the agent being called. An agent
    (including a publicly operated one) reads every request sent to it; what it keeps is its
    operator's policy.
11. Key sets and KELs are public by exact AID. `GET /agents/{aid}/keys` is unauthenticated and, through
    federation, answers for an AID registered at any peer hub whatever its directory visibility.
    Anyone who knows an AID can therefore confirm that it is registered and fetch its key history.
    A registered sender also learns from `recipient_quiet` whether a recipient has polled recently.
12. Delivery order between two nodes is not guaranteed (§12): a retried or differently routed
    message can overtake an earlier one.
13. An absent `A2A-Version` means `1.0` in this binding, where A2A §3.6.2 says 0.3 (§10.4). This is
    a deliberate deviation: the binding exists only for 1.0.

## 16. Reference implementation map

| Section | Code |
|---|---|
| §5 identity, KEL | ANetCore `identity/identity.go`, `anetcid` |
| §6 key sets | ANetCore `seal/enckeys.go`, `seal/highwater.go`, `seal/limits.go` |
| §7 envelope | ANetCore `seal/envelope.go`, `seal/fields.go`, `seal/suite.go`, `seal/errors.go` |
| §8.2 relayauth v2 | ANetCore `relayauth/relayauth.go`; ANetHub `internal/aghub/auth2.go` |
| §8.3 endpoints | ANetHub `internal/aghub/server.go` (`hRelaySend`, `hRelayPoll`, `hRelayAck`), `keys.go`, `relay.go`, `limits.go`; `internal/hubid` (`/hub/identity`) |
| §8.5 federation | ANetHub `internal/federation/federation.go`, `keys.go` |
| §10 bodies | ANetCore `delegation/delegation.go`, `delegation/status.go` |
| §7.6–7.7, §10.5, §11 node behaviour | ANet `internal/daemon/seal_send.go`, `receive.go`, `inbound.go` |

## 17. Open questions for the community

- **Q1 Generic payload profile.** Should a v2 of the binding carry A2A ProtoJSON (`SendMessageRequest`,
  `Task`, `TaskStatusUpdateEvent`, …) inside the envelope, so that non-anet implementations need
  only the envelope and the hub API, and not anet's task document format?
- **Q2 Service parameter key.** This draft uses `a2a.serviceParameters` (object, array values). The
  custom-binding guide mentions `a2a-service-parameters` as an example. Would the community prefer
  one standard key for all header-less bindings?
- **Q3 Data parts.** Which A2A data parts should a relay binding carry verbatim?
- **Q4 `capabilities.streaming`** for agents reachable only through a store-and-forward binding:
  `true` (streaming is available from the local mirror) or `false` (no end-to-end stream)?
- **Q5 In-task authorization** (`TASK_STATE_AUTH_REQUIRED`) over a relay: should credentials be
  sealed to the agent in-band, as A2A §7.6.3 recommends for in-band exchange?
- **Q6 Security scheme.** See `proposal-securityscheme.md`.

## Appendix A. KEL encoding (summary)

Normative source: ANetCore `identity`.

```cddl
KEL         = [1*256 SignedEvent]                 ; as carried in SealedInner key 10
SignedEvent = { 1 => KeyEvent, 2 => bstr .size 64, 3 => tstr }   ; event, signature, event_id (CID of the event preimage)
KeyEvent    = {
  ? 1 => tstr,            ; aid; empty in the inception preimage that derives the AID
  2 => uint,              ; seq
  ? 3 => tstr,            ; prev event_id; absent for inception
  4 => "icp" / "rot" / "ixn" / "drt" / "dip",
  5 => [* bstr],          ; current Ed25519 public keys (32 bytes each)
  ? 6 => bstr,            ; next_digest = SHA-256(next public key)
  7 => uint,              ; threshold
  ? 8 => uint,            ; timestamp (unix ms); 0 or absent = unknown
}
```

An event's signature is Ed25519 over `CoreDet-CBOR(KeyEvent)` by the controlling key (for `rot`,
the prior key). A `rot` must reveal a key whose SHA-256 equals the prior `next_digest`.

## Appendix B. Example exchange (abbreviated)

```
requester R, provider P, both registered at hub H

R: GET  H/hub/identity                       -> {aid: H_AID, kel}
R: GET  H/agents/P/keys                      -> {aid: P, keyset, kel}
R: VerifyEncKeySet(keyset, P, kel, now); SelectKey
R: POST H/relay/send   X-ANet-Wire: 2, X-ANet-AID: R, X-ANet-TS, X-ANet-Seq, X-ANet-Sig
       {to_aid: P, envelope: b64(Seal(inner{from R, to P, type "anet.delegate/1", ix, mid, ts, exp,
                                            body DelegateReq{…, metadata {"a2a.serviceParameters": {…}}}}))}
                                              -> {id: 17, status: "queued"}
P: POST H/relay/poll   (signed, action poll) -> {messages: [{id: 17, envelope}]}
P: Open, CheckTime, resolve KEL, VerifyInnerSig, policy, store task + replay row
P: POST H/relay/ack    {ids: [17]}
P: … agent works …
P: POST H/relay/send   {to_aid: R, envelope: Seal(inner{type "anet.status/1", body StatusMsg{state "working"}})}
P: POST H/relay/send   {to_aid: R, envelope: Seal(inner{type "anet.result/1", body ResultResp{status "done", …}})}
R: poll, open, verify, update mirror (working -> completed), emit events, ack
```
