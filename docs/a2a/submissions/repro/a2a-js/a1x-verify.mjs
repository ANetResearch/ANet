// A1 (cross-SDK part): verify the cards written by
// ../a2a-go/a1x-cross-sdk-vectors with @a2a-js/sdk's own verifier.
//
// Usage: node a1x-verify.mjs <dir written by the Go program>
//
// The cards are passed to the verifier in their JSON form, as served; see J1
// for what happens with the SDK's parsed form. Informational: prints the rows
// where the SDK differs from A2A §8.4.1 rule 1, exit 0 (2 on a setup error).
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { importJWK } from 'jose';
import { verifyAgentCardSignature } from '@a2a-js/sdk';

console.debug = () => {}; // the verifier logs every failed entry

const EXPECTED = {
  base: 'VERIFIED',
  'ext-required-false': 'FAILED',
  'ext-description-empty': 'FAILED',
  'skill-examples-empty': 'FAILED',
  'caps-streaming-false': 'VERIFIED',
  'caps-push-false': 'VERIFIED',
  'streaming-and-required-false': 'FAILED',
  'base-reserved-by-a2a-go': 'FAILED',
  'card-description-empty': 'VERIFIED',
  'ext-params-empty-string': 'VERIFIED',
};

const dir = process.argv[2];
if (!dir) {
  console.log('usage: node a1x-verify.mjs <dir written by the Go program>');
  process.exit(2);
}
const jwks = JSON.parse(readFileSync(join(dir, 'jwks.json'), 'utf8'));
const keys = new Map();
for (const k of jwks.keys) keys.set(k.kid, await importJWK(k, 'EdDSA'));
const verify = verifyAgentCardSignature(async (kid) => keys.get(kid));

const differs = [];
for (const entry of JSON.parse(readFileSync(join(dir, 'index.json'), 'utf8'))) {
  const name = entry.file.replace(/\.json$/, '');
  const card = JSON.parse(readFileSync(join(dir, entry.file), 'utf8'));
  let got = 'VERIFIED';
  try {
    await verify(card);
  } catch {
    got = 'FAILED';
  }
  const want = EXPECTED[name];
  if (got !== want) differs.push(name);
  console.log(`${name.padEnd(30)} @a2a-js/sdk: ${got.padEnd(8)} rule 1 expects: ${want.padEnd(8)} (${entry.note})`);
}
console.log(`@a2a-js/sdk differs from rule 1 on: ${differs.join(', ') || 'none'}`);
