# Issue drafts for a2a-go

> **DRAFT — not submitted; requires product owner approval before any external submission.**
>
> **License: Apache-2.0**, a2a-go's own license: the text and the code in these drafts
> (reproductions and suggested fixes) are licensed under the Apache License 2.0 alone (ANet
> `LICENSE`, Section 3). Check a2a-go's contribution requirements before opening a pull request.

Target: `github.com/a2aproject/a2a-go`, version **v2.6.0** (commit `ebf17c5`, "chore(main): release
2.6.0"). Every "Observed" result below was reproduced on 2026-09-27 with the snippets shown, built
against that commit with `replace github.com/a2aproject/a2a-go/v2 => <checkout>`.

Two drafts (A3, A9) describe verification bypasses. a2a-go's `SECURITY.md` asks for security issues
to be reported through GitHub Security Advisories, not public issues; they are marked accordingly.

| # | Title | Area | Filing |
|---|---|---|---|
| A1 | Card signing/verification skips the §8.4.1 default-value removal | `a2acrypto` | public issue |
| A2 | `a2a.AgentCard` JSON: `null` for required lists, inconsistent presence of optional booleans | `a2a` | public issue |
| A3 | Card verifier accepts duplicate member names and invalid UTF-8 | `a2acrypto` | **security advisory** |
| A4 | Card verifier accepts base64url with line breaks | `a2acrypto` | public issue (hardening) |
| A5 | Servers ignore `A2A-Version` | `a2asrv` | public issue |
| A6 | Comma-separated `A2A-Extensions` is not split | `a2asrv`, `a2aclient` | public issue |
| A7 | v1 handlers ignore `X-A2A-Extensions` | `a2asrv` | public issue |
| A8 | JSON-RPC and REST handlers do not echo activated extensions | `a2asrv` | public issue |
| A9 | Card resolver with a `Verifier` accepts unsigned cards | `a2aclient/agentcard` | **security advisory** |
| A10 | An unknown `SecurityScheme` variant makes the whole Agent Card unparseable | `a2a` | public issue |
| A11 | HTTP+JSON binding: `TaskNotCancelable` and `UnsupportedContentType` answered 400 (spec: 409, 415) | `a2asrv`, `internal/rest` | public issue |
| A12 | Streaming calls: an error before the first event is sent inside an already-opened SSE stream; the JSON-RPC client cannot read an error that is not | `a2asrv`, `a2aclient` | public issue |
| A13 | JSON-RPC params: proto field names (`history_length`, `context_id`) are silently ignored | `a2asrv`, `a2a` | public issue (confirm the spec reading first) |

---

## A1. `a2acrypto`: Agent Card signatures are computed without the default-value removal of spec §8.4.1

**Spec.** A2A §8.4.1 rule 1 and §8.4.2/§8.4.3: before JCS, "Fields with default values MUST be
omitted unless the field is marked as REQUIRED or has the `optional` keyword"; verifiers "remove
properties with default values from the received Agent Card".

**Code.** `a2acrypto/canonical.go` `canonicalizeJSON` decodes the raw bytes into `map[string]any`,
drops only the top-level `signatures` member and applies JCS. `Signer.Sign` and `Verifier.Verify`
both call it on the bytes as given (their doc comments say so).

**Reproduction.**

```go
pub, priv, _ := ed25519.GenerateKey(rand.Reader)
s, _ := a2acrypto.NewSigner(a2acrypto.SignerConfig{PrivateKey: priv, KeyID: "k"})
v := a2acrypto.NewVerifier(a2acrypto.VerifierConfig{KeyResolver: staticKey{pub}})

withDefault := []byte(`{"name":"n","capabilities":{"extensions":[{"uri":"u","required":false}]}}`)
stripped    := []byte(`{"name":"n","capabilities":{"extensions":[{"uri":"u"}]}}`)
sig, _ := s.Sign(ctx, withDefault)

v.Verify(ctx, withDefault, sig) // nil
v.Verify(ctx, stripped, sig)    // signature verification failed

var c a2a.AgentCard
json.Unmarshal(withDefault, &c)
rt, _ := json.Marshal(&c)       // "required":false is dropped (omitempty)
v.Verify(ctx, rt, sig)          // signature verification failed
```

**Observed.** A card whose JSON carries a default value (`"required": false`, `"description": ""` on
an extension, `"examples": []`, …) is signed over a payload that a §8.4.1-conformant verifier does not
compute, and vice versa. The card also stops verifying after a round trip through a2a-go's own
`a2a.AgentCard` type.

**Expected.** Signer and verifier compute the same payload for every JSON rendering of the same
AgentCard message.

**Impact.**
- Cross-SDK: cards signed by an SDK that strips defaults fail in a2a-go when the served JSON contains
  a default, and cards signed by a2a-go over JSON containing a default fail in such an SDK.
- Registries and proxies that parse a card into `a2a.AgentCard` and serve it again invalidate its
  signature whenever the original JSON contained a default.
- A card author cannot tell from a2a-go alone whether a card will verify elsewhere: a2a-go's own
  Sign→Verify round trip always succeeds.

**Suggested fix.**
1. Implement the removal step in `canonicalizeJSON`, driven by the AgentCard schema (the `a2apb`
   descriptors already carry `REQUIRED` field behaviour and proto3 `optional` presence): drop a member
   whose field is neither REQUIRED nor `optional` when its value is the proto default (`false`, `0`,
   `""`, `[]`, `{}`); recurse into message-typed members; leave `google.protobuf.Struct` contents
   (e.g. `AgentExtension.params`) untouched.
2. During a transition, verify both forms (stripped first, then as-given) and report which one
   matched, so deployments can find cards that need re-signing.
3. Sign only the stripped form.
4. Add a cross-SDK golden vector whose card contains defaults in several places (extension
   `required: false`, empty `examples`, empty `description` on an extension) and a variant of the same
   card without them; both must verify.

Related spec clarification (could be a separate issue in `a2aproject/A2A`): the §8.4.1 example keeps
`"skills": []` because `skills` is REQUIRED, while §5.7 says a REQUIRED array must contain at least
one element. The example would be clearer with a non-empty `skills`.

**How anet copes today.** Its card producer never emits a non-required field at its default value
(no `"required": false`, all required lists non-empty), so the as-given and stripped forms coincide.
Its own verifier (ANetCore `a2acard`) canonicalizes as given, like a2a-go, and will accept both forms
once this is settled.

---

## A2. `a2a.AgentCard` JSON: `null` for REQUIRED lists; `optional` presence cannot be expressed

**Reproduction.**

```go
card := &a2a.AgentCard{Name: "n", Description: "d", Version: "1",
    SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface("https://x", a2a.TransportProtocolJSONRPC)},
    Skills:       []a2a.AgentSkill{{ID: "s", Name: "s", Description: "d"}},
    Capabilities: a2a.AgentCapabilities{Extensions: []a2a.AgentExtension{{URI: "u"}}},
}
b, _ := json.Marshal(card)
```

**Observed.**

```json
{"supportedInterfaces":[…],"capabilities":{"extensions":[{"uri":"u"}],"pushNotifications":false,"streaming":false},
 "defaultInputModes":null,"defaultOutputModes":null,"description":"d","name":"n",
 "skills":[{"description":"d","id":"s","name":"s","tags":null}],"version":"1"}
```

1. REQUIRED repeated fields left nil (`defaultInputModes`, `defaultOutputModes`, `skills[].tags`,
   and `supportedInterfaces`/`skills` themselves) are written as `null`. ProtoJSON never writes
   `null` for a repeated field, and the spec requires REQUIRED arrays to be present and non-empty.
   The JCS payload therefore contains `null` where a ProtoJSON-based signer has `[]` or a value.
2. `AgentCapabilities.streaming` and `pushNotifications` are `optional bool` in the proto but plain
   `bool` without `omitempty` in Go: an unset value is written as `false` (presence invented).
   `extendedAgentCard`, also `optional bool`, is `bool` **with** `omitempty`: an explicit `false` is
   dropped (presence lost). §8.4.1 makes presence significant for exactly these fields.

**Impact.** Signatures over `json.Marshal(card)` (which `a2asrv.NewSignedCardProducer` computes)
differ from a ProtoJSON-based signer's for the same card, and a card with nil required lists is not a
valid card at all but is signed and served without complaint.

**Suggested fix.**
- `MarshalJSON` on `AgentCard`/`AgentSkill` that writes `[]` for nil required lists, or better,
  validation in `NewSignedCardProducer` / `NewStaticAgentCardHandler` that rejects a card with empty
  REQUIRED fields.
- Represent proto `optional` scalars with presence (`*bool`), or keep presence out of band, so that
  set-to-false and unset marshal differently. This is an API change; it may belong in a major version,
  with the canonicalization fix of A1 covering the signature aspect in the meantime.

---

## A3. `a2acrypto`: verifier accepts duplicate member names and invalid UTF-8 in the signed card — *report via Security Advisory*

**Spec.** RFC 8785 §3.1 requires the input to be I-JSON (RFC 7493), which forbids duplicate member
names (§2.3) and requires valid UTF-8 without lone surrogates (§2.1). A2A §8.4.1 requires RFC 8785.

**Code.** `canonicalizeJSON` decodes with `encoding/json` into `map[string]any`. For duplicate names
the last value wins; invalid UTF-8 and lone surrogates are silently replaced by U+FFFD (documented
`encoding/json` behaviour).

**Reproduction.**

```go
orig := []byte(`{"name":"A","description":"d"}`)
sig, _ := signer.Sign(ctx, orig)
tampered := []byte(`{"name":"EVIL","name":"A","description":"d"}`)
verifier.Verify(ctx, tampered, sig) // nil: accepted
```

**Observed.** A party without the signing key (a registry, cache or proxy) can insert members before
the signed ones, and the signature still verifies.

**Impact.** Parsers disagree on duplicates: some keep the first value, some the last, some reject.
A consumer that verified with a2a-go and then reads the card with a first-wins parser, or passes the
bytes on to one, acts on content the agent never signed (a different name, URL, interface list or
security scheme). The U+FFFD replacement lets two different byte strings verify under one signature,
so an intermediary can alter string content that a consumer decodes differently.

**Suggested fix.** Reject, in `canonicalizeJSON` (for both signing and verification): duplicate member
names at any depth, invalid UTF-8, lone surrogates in `\u` escapes, and numbers outside binary64.
This needs a token-level decoder (`json.Decoder.Token` with a per-object key set is enough for
duplicates; UTF-8 must be checked on the raw bytes). Optionally also reject member names that differ
only by case, since `encoding/json` matches struct fields case-insensitively: a card can carry
`protocolBinding` and `PROTOCOLBINDING`, the map-based check sees one and `a2a.AgentCard` decoding
the other.

**How anet copes.** ANetCore `a2acard` parses with its own strict I-JSON parser and rejects all of
the above, including case-variant member names.

---

## A4. `a2acrypto`: base64url decoding accepts line breaks in `protected` and `signature`

**Reproduction.**

```go
sig2 := *sig
sig2.Signature = sig.Signature[:10] + "\n" + sig.Signature[10:]
verifier.Verify(ctx, orig, &sig2) // nil: accepted
```

**Observed.** `base64.RawURLEncoding.DecodeString` skips `\r` and `\n`. A signature therefore has many
accepted spellings.

**Impact.** Low. The signature is still over the same content, but a relaying party can store and
serve byte-different copies of one signed card, which defeats deduplication, caching by hash, and
"serve the exact bytes the agent submitted" policies.

**Suggested fix.** Use `base64.RawURLEncoding.Strict()` and reject any character outside the
base64url alphabet before decoding (strict mode alone still skips line breaks).

---

## A5. `a2asrv`: `A2A-Version` is neither checked nor defaulted

**Spec.** §3.6.2: "Agents MUST process requests using the semantics of the requested `A2A-Version`
… If the version is not supported by the interface, agents MUST return a `VersionNotSupportedError`.
Agents MUST interpret empty value as 0.3 version." §3.6.1 also allows the version as a request
parameter (`?A2A-Version=1.0`).

**Code.** Nothing in `a2asrv` reads `a2a.SvcParamVersion`; `ErrVersionNotSupported` is defined but
never returned by the handlers.

**Reproduction.**

```go
srv := httptest.NewServer(a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(nopExecutor{})))
// POST {"jsonrpc":"2.0","id":1,"method":"GetTask","params":{"id":"nope"}}
// with A2A-Version: 9.9, with 0.3, and without the header
```

**Observed.** All three requests are served with v1.0 semantics and answer `-32001`
(`TASK_NOT_FOUND`). None answers `-32009` (`VERSION_NOT_SUPPORTED`).

**Impact.** A v1 server silently serves requests that declare another major version, and serves
header-less requests (which the spec says are 0.3) with 1.0 semantics. Servers cannot implement the
MUST in §3.6.2 without writing their own middleware.

**Suggested fix.** A transport option, for example

```go
a2asrv.WithVersionPolicy(a2asrv.VersionPolicy{
    Supported: []a2a.ProtocolVersion{"1.0"},   // Major.Minor, patch ignored
    Empty:     "0.3",                          // spec default; configurable
})
```

reading the header and the query parameter, returning `VersionNotSupportedError` otherwise, and
exposing the negotiated version in the `CallContext`.

**Discussion point for the spec.** With "empty means 0.3", a server that implements only 1.0 must
reject every client that omits the header (curl, hand-written clients, several agent frameworks
today). anet's local A2A interface treats an empty value as 1.0 and returns `VersionNotSupportedError`
for any explicit non-1.x value, and documents this as a deviation. A configurable `Empty` would let
such servers be explicit about the choice.

---

## A6. `a2asrv`/`a2aclient`: comma-separated `A2A-Extensions` values are not split

**Spec.** §3.2.6: `A2A-Extensions` is a "comma-separated list of extension URIs". §9.2 and §11.2:
"Multiple values for the same service parameter (e.g., `A2A-Extensions`) SHOULD be comma-separated in
a single header field."

**Code.** `a2asrv.NewServiceParams(req.Header)` stores header values as received;
`Extensions.RequestedURIs()` returns them unchanged and `Requested()` does `slices.Contains`. On the
client side, `a2aext.NewActivator` appends each URI as a separate value and the JSON-RPC transport
sends one header line per value (`Header.Add`).

**Reproduction.**

```go
ctx, _ := a2asrv.NewCallContext(context.Background(), a2asrv.NewServiceParams(map[string][]string{
    "A2A-Extensions": {"https://ex.com/a/v1,https://ex.com/b/v1"},
}))
ext, _ := a2asrv.ExtensionsFrom(ctx)
ext.Requested(&a2a.AgentExtension{URI: "https://ex.com/a/v1"}) // false
ext.RequestedURIs() // ["https://ex.com/a/v1,https://ex.com/b/v1"]
```

**Observed.** A client that follows the spec and activates two extensions in one header activates
neither on an a2a-go server. If either is declared `required`, the request fails with
`ExtensionSupportRequiredError` (`checkRequiredExtensions` in `a2asrv/intercepted_handler.go` uses the
same unsplit list), although the client did request it. Proxies that fold repeated header lines into
one comma-separated line (permitted by RFC 9110 §5.3) have the same effect on a2a-go's own clients.

**Suggested fix.** Split every `A2A-Extensions` value on `,` and trim optional whitespace (RFC 9110
list syntax) when building `ServiceParams`, or at least in `RequestedURIs()`; on the client, send one
comma-separated header line.

---

## A7. `a2asrv`: v1 handlers ignore the legacy `X-A2A-Extensions` header

**Code.** Only `a2acompat/a2av0.ToServiceParams` maps `x-a2a-extensions` to `a2a-extensions`; the v1
JSON-RPC and REST handlers do not.

**Reproduction.** As in A6 with header `X-A2A-Extensions: https://ex.com/a/v1`: `Requested()` returns
`false`.

**Impact.** Extension specifications written for A2A 0.3 still tell clients to use `X-A2A-Extensions`.
The a2a-x402 payments extension v0.2 §8 says: "Clients MUST request activation of this extension by
including its URI in the `X-A2A-Extensions` HTTP header." Such clients talking to an a2a-go v2
server get no activation, silently: if the server declares the extension `required` (a2a-x402
recommends `required: true`), `checkRequiredExtensions` fails the request with
`ExtensionSupportRequiredError` although the client did request it; a non-required one is just not
applied.

**Suggested fix.** In v1 handlers, merge `X-A2A-Extensions` into `A2A-Extensions` (deduplicated,
after the splitting of A6), optionally logging a deprecation notice. (A matching issue for the a2a-x402
text is in `issue-a2a-x402.md` X1.)

---

## A8. `a2asrv`: JSON-RPC and REST handlers do not return the activated extensions

**Spec.** `docs/topics/extensions.md`, "Extension Activation", step 3: "the response SHOULD include
the `A2A-Extensions` header, listing all extensions that were successfully activated for that
request."

**Code.** `CallContext.Extensions().ActivatedURIs()` is written into response metadata only by the
gRPC handlers (`a2agrpc/v1/handler.go`). `a2asrv/jsonrpc.go` and `a2asrv/rest.go` set only
`Content-Type`.

**Impact.** HTTP clients cannot learn whether an extension they requested was applied, which matters
for extensions that change the meaning of metadata (payments, for example).

**Suggested fix.** Set `A2A-Extensions: <comma-separated ActivatedURIs()>` on the response before the
body is written; for SSE, before the first event. Since activation can happen during execution,
document that for streaming responses the header reflects activation at stream start.

---

## A9. `a2aclient/agentcard`: a configured `Verifier` does not require a signature — *report via Security Advisory*

**Code.** `Resolver.parseCard`:

```go
if r.Verifier != nil && len(card.Signatures) > 0 {
    // verify, fail if none verifies
}
return card, nil
```

**Observed.** With a `Verifier` configured, a card with **no** `signatures` member is returned as
valid. Only a card that carries signatures, none of which verifies, is rejected.

**Impact.** Anyone who can modify the card in transit or at rest (a registry, a cache, a compromised
host) removes the `signatures` array and edits the card at will; a client that configured
verification believes it is protected and accepts it. Signature stripping is the standard downgrade
against optional signatures.

**Suggested fix.** When a `Verifier` is set, require at least one verifying signature (or add an
explicit `RequireSignature` field and make the "verify if present" behaviour opt-in, with a doc
comment explaining the downgrade risk).

**How anet copes.** Its verifier rejects a card with no signatures (`UNSIGNED`).

---

## A10. `a2a`: an unknown `SecurityScheme` variant makes the whole Agent Card unparseable

**Spec.** §5.7: "Implementations SHOULD ignore unrecognized fields in messages, allowing for forward
compatibility as the protocol evolves." `SecurityScheme` is a `oneof`; a future protocol version (or
a proposal such as `proposal-securityscheme.md`) adds variants.

**Code.** `NamedSecuritySchemes.UnmarshalJSON` (`a2a/auth.go`) returns an error when an entry has
none of the five known variants.

**Reproduction.**

```go
card := []byte(`{"name":"n","description":"d","version":"1",
  "supportedInterfaces":[{"url":"https://x","protocolBinding":"JSONRPC","protocolVersion":"1.0"}],
  "capabilities":{},"defaultInputModes":["text/plain"],"defaultOutputModes":["text/plain"],
  "skills":[{"id":"s","name":"s","description":"d","tags":["t"]}],
  "securitySchemes":{"bearer":{"httpAuthSecurityScheme":{"scheme":"Bearer"}},
                     "sig":{"senderSignatureSecurityScheme":{"profile":"https://example.org/p"}}}}`)
var c a2a.AgentCard
err := json.Unmarshal(card, &c)
// unknown security scheme type for sig: [senderSignatureSecurityScheme]
```

**Observed.** The whole card fails to parse, including its known `bearer` scheme and everything else.
The card resolver therefore fails as well.

**Impact.** No new security scheme can be introduced to A2A without breaking every deployed a2a-go
client that reads a card using it, even when the card also offers a scheme the client knows. This
blocks protocol evolution in exactly the place §5.7 is meant to protect.

**Suggested fix.** Keep unknown variants as an opaque value (e.g. `UnknownSecurityScheme{Raw
json.RawMessage}`) that round-trips unchanged; treat a security requirement that names only unknown
schemes as unsatisfiable by this client, and let the client pick another requirement if one is
satisfiable.

---

## A11. HTTP+JSON binding: `TaskNotCancelable` → 400 (spec 409), `UnsupportedContentType` → 400 (spec 415)

**Spec.** A2A §5.4, error code mappings: `TaskNotCancelableError` maps to HTTP `409 Conflict` and
`ContentTypeNotSupportedError` to HTTP `415 Unsupported Media Type` for the HTTP+JSON binding.

**Code.** `internal/rest/rest.go` `errorMappings` (v2.6.0, line 199 and line 202):

```go
{a2a.ErrTaskNotCancelable, http.StatusBadRequest, "FAILED_PRECONDITION"},
{a2a.ErrUnsupportedContentType, http.StatusBadRequest, "INVALID_ARGUMENT"},
```

`a2asrv/rest.go` `writeRESTError` writes that status (`errResp.HTTPStatus()`).

**Reproduction.**

```go
srv := httptest.NewServer(a2asrv.NewRESTHandler(handlerThatCompletesTasks))
// 1. create a task and let it complete, then:
// POST /tasks/<id>:cancel
```

**Observed** (a2a-tck and curl against a server on a2a-go v2.6.0, 2026-09-27). `400 Bad Request` with
`{"error":{"code":400,"status":"FAILED_PRECONDITION",…,"reason":"TASK_NOT_CANCELABLE"}}`.
a2a-tck CORE-CANCEL-002 (HTTP+JSON) fails on the status. A handler returning
`a2a.ErrUnsupportedContentType` is answered `400` likewise. The JSON-RPC binding's codes (`-32002`,
`-32005`) are right.

**Impact.** Clients and proxies that act on the HTTP status (retry policies, HTTP-level metrics, the
TCK) see a generic bad request where the spec gives a distinct status. Clients built on a2a-go read
the `reason` and are not affected.

**Suggested fix.** Map `ErrTaskNotCancelable` to `409` (`FAILED_PRECONDITION` can stay as the
google.rpc status) and `ErrUnsupportedContentType` to `415`; keep the table the client uses to turn a
status back into an error (`rest.FromRESTError`, which goes by `reason`) unchanged.

**How anet copes.** Its own middleware answers a wrong `Content-Type` with `415` before a2a-go runs;
`CancelTask` on a terminal task still goes through a2a-go and is answered `400`.

---

## A12. Streaming calls: errors before the first event arrive inside an SSE stream

**Code.** `a2asrv/jsonrpc.go` `handleStreamingRequest` (v2.6.0, line 150) and `a2asrv/rest.go`
`handleStreamingRequest` (line 277) call `sseWriter.WriteHeaders()` — HTTP 200,
`Content-Type: text/event-stream` — and only then ask the handler's iterator for its first event. An
error the handler returns before any event (`SubscribeToTask` on a task that does not exist or has
ended, `SendStreamingMessage` with a `taskId` that does not exist, invalid params) is therefore sent as
the stream's one event.

**Reproduction.**

```go
// A handler whose SubscribeToTask yields a2a.ErrTaskNotFound at once.
// POST /  {"jsonrpc":"2.0","id":1,"method":"SubscribeToTask","params":{"id":"nope"}}
```

**Observed** (a2a-tck and curl against a server on a2a-go v2.6.0, 2026-09-27). `200 OK`,
`text/event-stream`, one event `data: {"jsonrpc":"2.0","id":1,"error":{"code":-32001,…}}`.
a2a-tck STREAM-SUB-003 (terminal task) and STREAM-SUB-004 (unknown task) report the call as a
success: the response is a stream.

**Client side.** `a2aclient/jsonrpc.go` `sendStreamingRequest` treats any `200` answer as SSE and
`parseSSEStream` skips every line that is not `data:`. A server that answers a streaming call it
cannot start with an ordinary JSON-RPC error (`200`, `application/json`) is read by a2a-go as an
empty stream with no error (checked on 2026-09-28: `SubscribeToTask` yields no event and no error);
with a non-200 status the client reports only `unexpected HTTP status: 400 Bad Request`, not the A2A
error in the body. The REST client does read the error (`rest.FromRESTError`).

**Impact.** Servers cannot report "this stream cannot start" in the form the spec's error handling and
the TCK expect, and a server that does so anyway is misread by a2a-go's own JSON-RPC client.

**Suggested fix.**
1. Server: pull the first item from the iterator before writing the stream headers; if it is an
   error, answer with the binding's ordinary error response (JSON-RPC error object; HTTP+JSON
   `google.rpc.Status` with the mapped status) and do not open the stream. Keep-alives start after
   the first item, so a handler that blocks before its first event still gets its stream.
2. JSON-RPC client: when the answer to a streaming call is not `text/event-stream`, parse it as a
   JSON-RPC response and return its error (`jsonrpc.FromJSONRPCError`); for a non-200 status, try
   the body the same way before falling back to the status.

**How anet copes.** Its local A2A interface checks a streaming call before a2a-go runs (existence,
scope, state; a streaming send is carried out there and its task handed to the stream) and answers a
call that cannot start with the binding's ordinary error and the HTTP+JSON binding's status for it
(non-200, so that a2a-go's JSON-RPC client reports an error at all).

---

## A13. JSON-RPC params: proto field names are silently ignored

**Code.** `a2asrv/jsonrpc.go` decodes `params` with `encoding/json` into the `a2a` request types,
whose tags are the lowerCamelCase JSON names only — e.g. `a2a/core.go` line 830,
``HistoryLength *int `json:"historyLength,omitempty"` ``. `encoding/json` matches keys
case-insensitively but not across the underscore, so `history_length` never reaches the field, and an
unknown key is not an error.

**Spec reading (to confirm before filing).** The canonical proto3 JSON mapping, which the spec's JSON
forms follow, says parsers accept both the lowerCamelCase name and the original proto field name.
a2a-tck's JSON-RPC client relies on that: it sends `{"id": …, "history_length": 1}` for GetTask and
`context_id` for ListTasks.

**Reproduction.**

```sh
# GetTask of a task with four history messages, through a server on a2a-go v2.6.0
{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{"id":"<task>","history_length":1}}  # 4 messages
{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{"id":"<task>","historyLength":1}}   # 1 message
```

**Observed** (anet's local A2A interface on a2a-go v2.6.0, 2026-09-28, docs/notes/0029 §4.3): the
snake_case parameter is dropped without an error; a2a-tck CORE-HIST-001 (`historyLength=0` omits the
history) and CORE-HIST-002 (history not longer than `historyLength`) fail on JSON-RPC and pass on
HTTP+JSON, whose query parameters a2a-go reads by their camelCase names.

**Impact.** A client written against the proto field names gets full histories with no sign that its
parameter was ignored. By the same decoding (read from the code, not reproduced) ListTasks's
`context_id` is dropped and the list comes back unfiltered.

**Suggested fix.** Decode `params` with protojson semantics (accept both names), or at least refuse
unknown keys with `InvalidParams` so the mismatch is visible.

**How anet copes.** It does not, yet: module/a2a hands `params` to a2a-go unchanged. Normalizing the
keys in its pre-check is an open decision (docs/notes/0029 §9).

---

## Appendix: helper used in the reproductions

```go
type staticKey struct{ pub ed25519.PublicKey }

func (k staticKey) ResolveKey(ctx context.Context, kid, jku string) (crypto.PublicKey, error) {
    return k.pub, nil
}

type nopExecutor struct{}

func (nopExecutor) Execute(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
    return func(yield func(a2a.Event, error) bool) {}
}
func (nopExecutor) Cancel(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
    return func(yield func(a2a.Event, error) bool) {}
}
```
