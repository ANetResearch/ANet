# Step 1 — a2a-go: security advisories, issues, small pull requests

> **NOT SUBMITTED — on hold per product owner (more testing first).** Product-owner approval is also
> required for each item, one by one.
> Every block below is a separate submission and needs its own approval (docs/notes/0032 §3,
> docs/notes/0027 G10). Nothing here has been posted anywhere.

Plan and reasoning: `docs/notes/0032-计划-向A2A社区贡献.md` §3 step 1. Full analysis behind each
item: `docs/a2a/issue-a2a-go.md` (A1–A13).

## Before pasting anything

- Target: <https://github.com/a2aproject/a2a-go>. Checked on 2026-09-28: upstream `main` is still
  `ebf17c5` ("chore(main): release 2.6.0"), the commit every reproduction below was run on.
  Re-check `main` and the open issues right before submitting; drop an item that has been fixed or
  reported in the meantime.
- a2a-go asks for an issue before a significant change (`CONTRIBUTING.md:5-8`). Open the issue,
  wait for a maintainer reply, then open the pull request with `Fixes #<issue>`.
- PR titles must follow Conventional Commits (`.github/workflows/validate-pr-title.yaml`). CI runs
  `lint`, `test`, `ITK` and `ACTS Conformance`. No CLA or DCO check was seen on recent external PRs
  (#442); confirm again at submission time.
- Security issues go through GitHub Security Advisories only (`SECURITY.md`): A9 and A3 below are
  **never** filed as public issues, and neither is mentioned publicly until the maintainers publish.
- Pace: at most two or three public submissions per week to this repository. Wave 1 = S1–S6,
  wave 2 = S7–S9.
- The optional context line at the end of some issues mentions anet. Keep or delete it as the
  product owner decides; it is never a requirement for the report.
- Pull request patches are **not** included here. Each PR block describes the exact change; the
  patch and its tests are written in a fork once the issue has a maintainer reply.

Not submitted, on purpose:

| Draft | Why not |
|---|---|
| A11 (REST 400 instead of 409/415) | Withdrawn. A2A v1.0.1 (#1627, 2026-04-14) maps `TaskNotCancelableError` and `ContentTypeNotSupportedError` to `400 Bad Request` (A2A `docs/specification.md:1183,1186`). a2a-go is right; the TCK still expects the v1.0.0 values (a2a-tck #231, #240). |
| A13 (`history_length` ignored) | The spec requires camelCase in every JSON serialization (A2A `docs/specification.md:1204`); the non-conformant side is the TCK's JSON-RPC client (a2a-tck #242, fix PR #243). Handled in step 4. |
| A1 (default-value removal) | Already reported as a2a-go #445 (2026-09-26); spec side is A2A #2122. Added as a comment (S6), not a new issue. |
| A12 (errors inside an opened SSE stream) | The spec does not say how a stream that cannot start must be refused. Hold until the TCK/spec position is clear. |
| A2 (`null` lists, `optional` presence) | Public API change; wait for the maintainers' answer on #445 first. |

---

## S1 — Security advisory (private): card resolver with a `Verifier` accepts unsigned cards (A9)

Where: <https://github.com/a2aproject/a2a-go/security/advisories/new>

**Title**

```
agentcard.Resolver with a Verifier accepts Agent Cards that carry no signature
```

**Affected**: Go module `github.com/a2aproject/a2a-go/v2`, package `a2aclient/agentcard`.
Checked: v2.6.0 (the first release containing Agent Card JWS verification, #368). Patched: none.

**Suggested severity**: Moderate (maintainers to assess). **Suggested CWE**: CWE-347 (Improper
Verification of Cryptographic Signature).

**Description**

````markdown
### Summary

When `agentcard.Resolver.Verifier` is set, a card with no `signatures` member is returned as valid.
Only a card that carries signatures, none of which verifies, is rejected. Removing the `signatures`
array therefore turns any modified card into an accepted one.

### Details

`a2aclient/agentcard/resolver.go:210` (v2.6.0):

```go
if r.Verifier != nil && len(card.Signatures) > 0 {
    // verify, fail if none verifies
}
return card, nil
```

### PoC

1. Serve a signed card; configure a `Resolver` with a `Verifier` that resolves the signing key.
   Resolution succeeds, as expected.
2. Serve the same card with `"name"` (or `supportedInterfaces[0].url`) changed and the `signatures`
   member removed. Resolution succeeds; the modified card is returned without an error.

### Impact

Anyone who can alter the card in transit or at rest (a registry, a cache, a compromised host, a TLS
terminating proxy) strips the signatures and edits the card at will. A client that configured a
`Verifier` believes it is protected and accepts the card. Signature stripping is the standard
downgrade against optional signatures.

### Suggested fix

When a `Verifier` is set, require at least one verifying signature. If "verify only when present"
must stay available, make it an explicit opt-in (for example `AllowUnsigned bool`) with a doc comment
that describes the downgrade.
````

---

## S2 — Security advisory (private): verifier accepts duplicate member names and invalid UTF-8 (A3)

Where: <https://github.com/a2aproject/a2a-go/security/advisories/new>

**Title**

```
a2acrypto: Agent Card signatures verify over JSON with duplicate member names or invalid UTF-8
```

**Affected**: Go module `github.com/a2aproject/a2a-go/v2`, package `a2acrypto`. Checked: v2.6.0.

**Suggested severity**: Moderate (maintainers to assess). **Suggested CWE**: CWE-347; CWE-436
(Interpretation Conflict).

**Description**

````markdown
### Summary

`canonicalizeJSON` (`a2acrypto/canonical.go:31`) decodes the card with `encoding/json` into
`map[string]any`. For a duplicated member name the last value wins, and invalid UTF-8 or lone
surrogates are replaced with U+FFFD. RFC 8785 (required by A2A §8.4.1) is defined over I-JSON
(RFC 7493), which forbids both. A party without the signing key can therefore add members to a
signed card and the signature still verifies.

### PoC

```go
orig := []byte(`{"name":"A","description":"d"}`)
sig, _ := signer.Sign(ctx, orig)
tampered := []byte(`{"name":"EVIL","name":"A","description":"d"}`)
err := verifier.Verify(ctx, tampered, sig) // nil: accepted
```

### Impact

JSON parsers disagree on duplicates (first wins, last wins, or reject). A consumer that verifies with
a2a-go and then reads the card with a first-wins parser, or passes the bytes on to one, acts on
content the agent never signed: another name, interface URL or security scheme. The U+FFFD
replacement lets two different byte strings verify under one signature.

### Suggested fix

Reject, for both signing and verification: duplicate member names at any depth, invalid UTF-8, lone
surrogate escapes, and numbers outside IEEE 754 binary64. A token-level pass (`json.Decoder.Token`
with a key set per object) finds duplicates; UTF-8 has to be checked on the raw bytes. Consider also
rejecting member names that differ only by case: `encoding/json` matches struct fields
case-insensitively, so a card with both `protocolBinding` and `PROTOCOLBINDING` is read one way by
the map-based canonicalizer and another way by `a2a.AgentCard`.
````

---

## S3 — Issue: comma-separated `A2A-Extensions` values are not split (A6)

**Title**

```
a2asrv: comma-separated A2A-Extensions values are not split, so spec-conformant clients activate nothing
```

**Body**

````markdown
**What happened**

The spec defines `A2A-Extensions` as a comma-separated list and says multiple values SHOULD be sent
comma-separated in one header field (A2A `docs/specification.md` §3.2.6 and §11.2; also
`docs/topics/extensions.md`, "Extension Activation"). a2a-go stores the header value as received:
`Extensions.RequestedURIs()` (`a2asrv/extensions.go:64`) returns it unchanged and `Requested()` does
an exact `slices.Contains`.

```go
ctx, _ := a2asrv.NewCallContext(context.Background(), a2asrv.NewServiceParams(map[string][]string{
    "A2A-Extensions": {"https://ex.com/a/v1,https://ex.com/b/v1"},
}))
ext, _ := a2asrv.ExtensionsFrom(ctx)
ext.Requested(&a2a.AgentExtension{URI: "https://ex.com/a/v1"}) // false
ext.RequestedURIs() // ["https://ex.com/a/v1,https://ex.com/b/v1"]
```

**Consequences**

- A client that activates two extensions the way the spec describes activates neither.
- If one of them is declared `required`, `checkRequiredExtensions` (`a2asrv/intercepted_handler.go:328`)
  rejects the request with `ExtensionSupportRequiredError` although the client requested it.
- Proxies may fold repeated header lines into one comma-separated line (RFC 9110 §5.3), which has the
  same effect on a2a-go's own clients (`a2aext.NewActivator` sends one header line per URI).

**Expected**

`RequestedURIs()` returns `["https://ex.com/a/v1", "https://ex.com/b/v1"]`.

**Proposed fix** (happy to send a PR)

Split every `A2A-Extensions` value on `,`, trim optional whitespace, drop empty elements, and
de-duplicate while keeping order, in `RequestedURIs()` (so `Requested()` and the required-extension
check both benefit). Optionally, have the client send one comma-joined header line.

Checked on v2.6.0 (`ebf17c5`).
````

**Pull request (after a maintainer reply)**

Title:

```
fix(a2asrv): split comma-separated A2A-Extensions values
```

Body:

````markdown
Fixes #<issue>.

`A2A-Extensions` is a comma-separated list (spec §3.2.6, §11.2). `Extensions.RequestedURIs()`
returned header values unchanged, so `A2A-Extensions: a,b` activated neither `a` nor `b`, and a
required extension requested that way failed with `ExtensionSupportRequiredError`.

Changes:
- `a2asrv/extensions.go`: `RequestedURIs()` splits each value on `,`, trims optional whitespace,
  drops empty elements and removes duplicates, keeping first-seen order. `Requested()` and
  `checkRequiredExtensions` use it unchanged.
- Tests: one value with two URIs; spaces around commas; two header lines; a mix of both; empty
  elements (`a,,b`, trailing comma); a duplicated URI.

No change to the wire format of a2a-go clients in this PR.
````

---

## S4 — Issue: JSON-RPC and REST responses do not report activated extensions (A8)

**Title**

```
a2asrv: JSON-RPC and HTTP+JSON handlers do not return the A2A-Extensions response header
```

**Body**

````markdown
**What happened**

`docs/topics/extensions.md` ("Extension Activation", step 3): "the response SHOULD include the
`A2A-Extensions` header, listing all extensions that were successfully activated for that request."

`CallContext.Extensions().ActivatedURIs()` is written to response metadata only by the gRPC handler
(`a2agrpc/v1/handler.go`). `a2asrv/jsonrpc.go` and `a2asrv/rest.go` set only `Content-Type`, so an
HTTP client cannot tell whether an extension it requested was applied.

**Why it matters**

Extensions that change the meaning of message metadata (payments, for example the a2a-x402 extension)
need the client to know whether the server applied them.

**Proposed fix** (happy to send a PR)

Set `A2A-Extensions: <comma-separated ActivatedURIs()>` on the response before the body is written,
and for SSE responses before the stream headers are written. For streams, document that the header
reflects activation at stream start.

Checked on v2.6.0 (`ebf17c5`).
````

**Pull request (after a maintainer reply)**

Title:

```
fix(a2asrv): return activated extensions in the A2A-Extensions response header
```

Body:

````markdown
Fixes #<issue>.

- `a2asrv/jsonrpc.go`, `a2asrv/rest.go`: when `ActivatedURIs()` is non-empty, set
  `A2A-Extensions` (comma-separated) before writing a unary response, and before
  `sseWriter.WriteHeaders()` for streaming responses.
- Tests: an executor that activates one and two extensions; unary JSON-RPC, unary REST, and both
  streaming paths; no header when nothing was activated.
````

---

## S5 — Issue: an unknown `SecurityScheme` variant makes the whole Agent Card unparseable (A10)

**Title**

```
a2a: an unknown SecurityScheme variant makes the whole AgentCard fail to unmarshal
```

**Body**

````markdown
**What happened**

`NamedSecuritySchemes.UnmarshalJSON` (`a2a/auth.go:167`, error at line 200) returns an error when an
entry has none of the five known variants. The whole card then fails to parse, including schemes the
client does know, and so does card resolution.

```go
card := []byte(`{"name":"n","description":"d","version":"1",
  "supportedInterfaces":[{"url":"https://x","protocolBinding":"JSONRPC","protocolVersion":"1.0"}],
  "capabilities":{},"defaultInputModes":["text/plain"],"defaultOutputModes":["text/plain"],
  "skills":[{"id":"s","name":"s","description":"d","tags":["t"]}],
  "securitySchemes":{"bearer":{"httpAuthSecurityScheme":{"scheme":"Bearer"}},
                     "future":{"someFutureSecurityScheme":{"x":"y"}}}}`)
var c a2a.AgentCard
err := json.Unmarshal(card, &c)
// unknown security scheme type for future: [someFutureSecurityScheme]
```

**Why it matters**

Spec §5.7: "Implementations SHOULD ignore unrecognized fields in messages, allowing for forward
compatibility as the protocol evolves." `SecurityScheme` is a `oneof`; any variant added in a later
protocol version would break every deployed a2a-go client that reads a card using it, even when the
card also offers a scheme the client supports.

**Proposed fix**

Keep an unknown variant as an opaque value (e.g. `UnknownSecurityScheme{Raw json.RawMessage}`) that
round-trips unchanged. Treat a security requirement that names only unknown schemes as
unsatisfiable by this client, and let the client choose another requirement if one is satisfiable.
Happy to send a PR once the shape of the type is agreed.

Checked on v2.6.0 (`ebf17c5`).
````

---

## S6 — Comment on a2a-go #445 (A1)

Where: <https://github.com/a2aproject/a2a-go/issues/445>

````markdown
One more case in the same area, independent of the JS SDK: default values.

§8.4.1 rule 1 says fields with default values MUST be omitted before canonicalization unless they are
REQUIRED or `optional`. `canonicalizeJSON` works on the bytes as given, so a card whose JSON carries a
default is signed over a payload that a §8.4.1-conformant verifier does not compute, and the card
stops verifying after a round trip through a2a-go's own type:

```go
withDefault := []byte(`{"name":"n","capabilities":{"extensions":[{"uri":"u","required":false}]}}`)
stripped    := []byte(`{"name":"n","capabilities":{"extensions":[{"uri":"u"}]}}`)
sig, _ := signer.Sign(ctx, withDefault)

verifier.Verify(ctx, withDefault, sig) // nil
verifier.Verify(ctx, stripped, sig)    // signature verification failed

var c a2a.AgentCard
json.Unmarshal(withDefault, &c)
rt, _ := json.Marshal(&c)             // "required":false dropped by omitempty
verifier.Verify(ctx, rt, sig)         // signature verification failed
```

(v2.6.0, `ebf17c5`.) A registry or proxy that parses a card into `a2a.AgentCard` and serves it again
invalidates the signature whenever the original JSON contained a default.

A shared golden vector would help whichever way the spec question (A2A #2122) is settled: one card
with defaults in several places (extension `required: false`, empty `examples`, empty `description`
on an extension) and the same card without them, both expected to verify.
````

---

## S7 (wave 2) — Issue: `A2A-Version` is neither checked nor defaulted (A5)

**Title**

```
a2asrv: A2A-Version is not checked; VersionNotSupportedError is never returned
```

**Body**

````markdown
**What happened**

Spec §3.6.2 (`docs/specification.md:737-739`): "Agents MUST process requests using the semantics of the
requested `A2A-Version` … If the version is not supported by the interface, agents MUST return a
`VersionNotSupportedError`. Agents MUST interpret empty value as 0.3 version." §3.6.1 also allows the
version as a query parameter.

Nothing in `a2asrv` reads `a2a.SvcParamVersion`; `ErrVersionNotSupported` is defined but never returned
by the handlers. A `GetTask` for an unknown task sent with `A2A-Version: 9.9`, with `0.3`, and with no
header is answered `-32001` (`TASK_NOT_FOUND`) in all three cases; none gets `-32009`.

**Why it matters**

A v1 server silently serves requests that declare another version, and servers cannot meet the MUST
without writing their own middleware.

**Proposal**

A transport option, e.g.

```go
a2asrv.WithVersionPolicy(a2asrv.VersionPolicy{
    Supported: []a2a.ProtocolVersion{"1.0"}, // Major.Minor
    Empty:     "0.3",                        // spec default; configurable
})
```

that reads the header and the query parameter, returns `VersionNotSupportedError` otherwise, and
exposes the negotiated version in the `CallContext`. A configurable `Empty` lets a 1.0-only server
decide explicitly how to treat clients that omit the header (curl, hand-written clients), instead of
rejecting all of them.

Checked on v2.6.0 (`ebf17c5`).
````

---

## S8 (wave 2) — Issue: v1 handlers ignore the legacy `X-A2A-Extensions` header (A7)

**Title**

```
a2asrv: v1 handlers ignore X-A2A-Extensions, which extension specs written for 0.3 still prescribe
```

**Body**

````markdown
**What happened**

Only `a2acompat/a2av0` maps `x-a2a-extensions` to `a2a-extensions` (`a2acompat/a2av0/conversions.go:37`).
The v1 JSON-RPC and REST handlers do not, so `Requested()` is false for an extension requested with
`X-A2A-Extensions`.

**Why it matters**

Extension specifications written for A2A 0.3 still tell clients to use the `X-` header. The a2a-x402
payments extension v0.2 §8: "Clients MUST request activation of this extension by including its URI in
the `X-A2A-Extensions` HTTP header." Such a client talking to an a2a-go v2 server gets no activation,
silently. a2a-x402 recommends `required: true`; with that, `checkRequiredExtensions` rejects the
request with `ExtensionSupportRequiredError` although the client did request the extension.

**Proposal**

In the v1 handlers, merge `X-A2A-Extensions` into `A2A-Extensions` (after splitting, see #<S3 issue>,
de-duplicated), optionally with a deprecation log line. The extension spec itself should also move to
the 1.0 header; that is being raised with a2a-x402 separately.

Checked on v2.6.0 (`ebf17c5`).
````

---

## S9 (wave 2) — Issue and PR: base64url decoding accepts line breaks (A4)

**Title**

```
a2acrypto: JWS protected header and signature accept base64url with CR/LF
```

**Body**

````markdown
`a2acrypto/verify.go:61` and `:94` decode with `base64.RawURLEncoding.DecodeString`, which skips
`\r` and `\n`. A signature therefore has many accepted spellings:

```go
sig2 := *sig
sig2.Signature = sig.Signature[:10] + "\n" + sig.Signature[10:]
verifier.Verify(ctx, orig, &sig2) // nil: accepted
```

The signed content is unchanged, so this is hardening rather than a bypass. It matters to registries
and caches that deduplicate or address cards by hash, or that promise to serve the exact bytes an
agent submitted: one signed card can be stored and served in many byte-different forms.

Proposed fix: reject any byte outside the base64url alphabet before decoding. `Strict()` alone is not
enough; per the `encoding/base64` documentation it still ignores CR and LF.

Checked on v2.6.0 (`ebf17c5`).
````

PR title:

```
fix(a2acrypto): reject non-alphabet characters in JWS base64url parts
```

PR body:

````markdown
Fixes #<issue>.

- `a2acrypto/verify.go`: a helper that rejects any byte outside `[A-Za-z0-9_-]`, then decodes with
  `base64.RawURLEncoding.Strict()`; used for `protected` and `signature`.
- Tests: CR, LF, space and `=` padding in either part are rejected; the unmodified signature verifies.
````
