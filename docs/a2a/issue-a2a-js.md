# Issue drafts for a2a-js (`@a2a-js/sdk`)

> **NOT SUBMITTED — on hold per product owner (more testing first).** Each item needs the product
> owner's approval; J1 is a private security advisory, not a public issue.
>
> **License: Apache-2.0**, a2a-js's own license: the text and the code in these drafts are licensed under
> the Apache License 2.0 alone (ANet `LICENSE`, condition 3).

Target: `@a2a-js/sdk` **1.2.1** (npm; still latest on 2026-09-29), Node.js v24.21.0. Found while
driving anet's local A2A interface with the SDK's own client (`docs/notes/0035` item 2). Standalone
reproductions, using @a2a-js/sdk from registry.npmjs.org and nothing from anet, are in
`submissions/repro/a2a-js/` (`j1-signature-skips-security-schemes.mjs`, `j2-resolver-base-path.mjs`,
`j3-blocking-call-300s.mjs`); every "Observed" below was re-run there on 2026-09-29.

**Upstream check, 2026-09-29.** J1 is the same defect as a2a-js #663 (2026-08-20, "canonicalizeAgentCard
is input-form dependent — AgentCard instance input silently drops securitySchemes"), which has an
open fix PR #664 (last touched 2026-09-09, not merged); #663's own repro is the sign/verify form
mismatch, and the reproduction here adds the resolve-then-verify client path and the tampered-OAuth
case. `canonicalizeAgentCard`'s `AgentCard.toJSON(AgentCard.fromJSON(card))` is unchanged from v1.0.1
through v1.2.1 (v1.0.0 did not have it). J2 and J3 have no matching issue found. Re-check #663/#664
right before filing J1; if #664 ships, re-run the repro and drop J1 if it no longer reproduces.

| # | Title | Area | Filing |
|---|---|---|---|
| J1 | `canonicalizeAgentCard` drops oneof members of a parsed card: signatures do not cover `securitySchemes` | `src/signature` (sign and verify) | **security advisory** (private report), not a public issue |
| J2 | The card resolver resolves the well-known path as a URL reference: a base URL with a path loses its last segment | `client/card-resolver` | public issue, or a spec discussion first |
| J3 | A blocking `SendMessage` longer than 300 s fails with Node's default `fetch` (`UND_ERR_HEADERS_TIMEOUT`) | client transports, docs | documentation issue |

---

## J1. `canonicalizeAgentCard` drops oneof members of a parsed card

**Code.** `canonicalizeAgentCard(agentCard)` (dist `index.cjs`, `function canonicalizeAgentCard`) computes
`AgentCard.toJSON(AgentCard.fromJSON(agentCard))`. `fromJSON` expects the JSON form. Given the SDK's own
in-memory form — what `DefaultAgentCardResolver.resolve()` / `normalizeAgentCard()` return, what
`Client.getAgentCard(options, verifySignature)` passes to the verifier, and what a server builds as an
`AgentCard` value — a oneof member is `{scheme: {$case: "httpAuthSecurityScheme", value: …}}`, which
`SecurityScheme.fromJSON` does not recognise; it yields `{}` and `cleanEmpty` then removes the whole
`securitySchemes` object. The same holds for every oneof in the card (`OAuthFlows.flow`).

**Observed.**

1. Verifying: a card signed by another implementation over the §8.4.1 payload (anet's proxy card, which has
   `securitySchemes.anetLocal.httpAuthSecurityScheme`; a2a-python verifies it) fails
   `verifyAgentCardSignature` when given the resolver's output, and `Client.getAgentCard(undefined, verifier)`
   throws `No valid signatures found on agent card.` The same card as JSON (`AgentCard.toJSON(card)`, or the
   parsed HTTP body) verifies.
2. Signing: `generateAgentCardSignature(key, header)(card)` with `card` in the SDK's form signs a payload
   without `securitySchemes`. a2a-python's `create_signature_verifier` rejects the served card
   (`InvalidSignaturesError`); the SDK's own verifier accepts it only in the parsed form — and then also
   accepts the card after its `securitySchemes` were replaced by different ones.

```js
const sdk = require('@a2a-js/sdk');
const card = { /* … */ securitySchemes: { bearer: { scheme: { $case: 'httpAuthSecurityScheme',
  value: { description: '', scheme: 'Bearer', bearerFormat: '' } } } } /* … */ };
sdk.canonicalizeAgentCard(card).includes('securitySchemes');   // false
```

**Impact.** Interoperability: cards with security schemes signed by a2a-js cannot be verified by other SDKs,
and cards signed elsewhere cannot be verified through the SDK's client API. Security: the signature of a
card signed by a2a-js does not cover its `securitySchemes` (nor any oneof), so they can be altered without
the SDK's verifier noticing.

**Suggested fix.** Canonicalize from the JSON form: `AgentCard.toJSON(card)` when given the in-memory form
(or accept only JSON and convert at the call sites: the resolver's raw body, the server's serialized card).
Add a cross-SDK test vector with `securitySchemes` and an OAuth2 flow.

**How anet copes.** Nothing to change on anet's side: its cards follow §8.4.1 and verify with a2a-python and
a2a-go. anet's guide tells JS users to verify `AgentCard.toJSON(card)`.

---

## J2. The card resolver resolves the well-known path as a URL reference

**Code.** `DefaultAgentCardResolver.resolve(baseUrl, path)` builds `new URL(path ?? '.well-known/agent-card.json', baseUrl)`.
By URL reference resolution, a base URL whose path does not end in `/` loses its last segment:
`http://127.0.0.1:43811/a2a/v1/agents/<aid>` resolves to `http://127.0.0.1:43811/a2a/v1/agents/.well-known/agent-card.json`.

**Observed.** `resolve('http://…/a2a/v1/agents/<aid>')` and `ClientFactory.createFromUrl` with that URL: `404`.
With a trailing slash (`…/<aid>/`) both work. Other clients given the same base URL: a2a-python
(`A2ACardResolver`) appends the path to `base_url.rstrip('/')`; Hermes' A2A client does the same; a2a-go's
resolver does not append the well-known path to a URL that has a path.

**Impact.** Agents served under a path prefix (several agents behind one host, a gateway, anet's local
interface) need a different base URL for each SDK.

**Spec reading.** A2A §8.2 (`docs/specification.md:1988`) names `https://{server_domain}/.well-known/agent-card.json`; how a base URL with
a path combines with the well-known path is not said. Worth raising with the spec before changing any SDK;
appending (as a2a-python does) is the least surprising for the `createFromUrl(baseUrl)` API.

---

## J3. A blocking `SendMessage` longer than 300 s fails with Node's default `fetch`

**Observed.** `client.sendMessage(…)` without `returnImmediately`, against a task that takes 420 s: after
301 s, `TypeError: fetch failed` (cause `UND_ERR_HEADERS_TIMEOUT`). Node's `fetch` (undici) waits at most
300 s for response headers (`headersTimeout`) and 300 s between body chunks (`bodyTimeout`); a blocking
`SendMessage` sends nothing until the task is terminal or interrupted. `sendMessageStream` on the same kind
of task stays open (the server's SSE keep-alives count as body data) and delivers the answer; a2a-python's
blocking call waits as long as its configured timeout.

**Impact.** A2A §3.2.2 (`docs/specification.md:446`) says a blocking `SendMessage` MUST wait for a terminal or interrupted state, with no
upper bound; with the SDK's defaults a Node client cannot wait more than five minutes, and the error does
not say that the task goes on.

**Suggested fix.** Document it in the client section and show how to pass a `fetchImpl` with an undici
`Agent({headersTimeout: 0, bodyTimeout: 0})`, or recommend streaming / `returnImmediately` + `getTask`
for long tasks.
