// js_long_hold.mjs — long_hold.py with the JavaScript SDK: a held task open HOLD seconds as a
// SendStreamingMessage (JSON-RPC) and as a blocking SendMessage, answered on the provider's control
// plane afterwards. Node's fetch (undici) gives up on a response whose headers have not come in 300 s
// (headersTimeout) and on a body silent for 300 s (bodyTimeout); the SDK's default fetch is that one.
//
//   A2A_JS_DIR=… node js_long_hold.mjs ENV_JSON HOLD_SECONDS
import { createRequire } from 'node:module';
import { readFileSync } from 'node:fs';
import { randomUUID } from 'node:crypto';

const req = createRequire((process.env.A2A_JS_DIR || process.cwd()) + '/package.json');
const { TaskState, Role } = req('@a2a-js/sdk');
const { ClientFactory, JsonRpcTransportFactory, DefaultAgentCardResolver, createAuthenticatingFetchWithRetry } = req('@a2a-js/sdk/client');

const env = JSON.parse(readFileSync(process.argv[2], 'utf8'));
const hold = Number(process.argv[3]);
const token = readFileSync(env.req.a2a_token_file, 'utf8').trim();
const provTok = readFileSync(env.prov.ctl_token_file, 'utf8').trim();
const base = `http://${env.req.a2a}/a2a/v1/agents/${env.prov.aid}/`;
const nonce = randomUUID().slice(0, 8);
let fails = 0;
const out = (k, id, m) => process.stdout.write(`${k} ${id}: ${m}\n`);
const check = (c, id, m) => { if (!c) fails++; out(c ? 'PASS' : 'FAIL', id, m); };
const fetchImpl = createAuthenticatingFetchWithRetry(fetch, { headers: async () => ({ Authorization: `Bearer ${token}` }), shouldRetryWithHeaders: async () => undefined });
const msg = (text) => ({ messageId: randomUUID(), contextId: '', taskId: '', role: Role.ROLE_USER, parts: [{ content: { $case: 'text', value: text }, metadata: undefined, filename: '', mediaType: '' }], metadata: undefined, extensions: [], referenceTaskIds: [] });
const sreq = (m) => ({ tenant: '', message: m, metadata: undefined, configuration: undefined });
const ctl = async (path, body) => (await fetch(`http://${env.prov.ctl}${path}`, { method: 'POST', body: JSON.stringify(body), headers: { Authorization: `Bearer ${provTok}`, 'Content-Type': 'application/json' } })).json();
async function providerTask(marker) {
  for (let i = 0; i < 60; i++) {
    const r = await ctl('/tasks/list', { role: 'inbound', page_size: 50, history_length: 1 });
    const t = (r.tasks || []).find((x) => (x.history || []).some((m) => (m.parts || []).some((p) => (p.text || '').includes(marker))));
    if (t) return t.id;
    await new Promise((r2) => setTimeout(r2, 1000));
  }
  return null;
}

const card = await new DefaultAgentCardResolver({ fetchImpl }).resolve(base);
const client = await new ClientFactory({ transports: [new JsonRpcTransportFactory({ fetchImpl })] }).createFromAgentCard(card);
const opts = { signal: AbortSignal.timeout((hold + 300) * 1000) };

const streamJob = (async () => {
  const t0 = Date.now();
  try {
    let last; let n = 0;
    for await (const ev of client.sendMessageStream(sreq(msg(`[hold] long-jsstream-${nonce}`)), opts)) { n++; if (ev.payload?.$case === 'statusUpdate') last = ev.payload.value.status.state; }
    return ['stream', (Date.now() - t0) / 1000, last, `${n} events`];
  } catch (e) { return ['stream', (Date.now() - t0) / 1000, undefined, `${e.constructor.name}: ${String(e.message).slice(0, 120)} ${e.cause ? '(cause ' + (e.cause.code || e.cause.name) + ')' : ''}`]; }
})();
const blockJob = (async () => {
  const t0 = Date.now();
  try {
    const t = await client.sendMessage(sreq(msg(`[hold] long-jsblock-${nonce}`)), opts);
    return ['blocking', (Date.now() - t0) / 1000, t.status?.state, 'ok'];
  } catch (e) { return ['blocking', (Date.now() - t0) / 1000, undefined, `${e.constructor.name}: ${String(e.message).slice(0, 120)} ${e.cause ? '(cause ' + (e.cause.code || e.cause.name) + ')' : ''}`]; }
})();
const ids = { jsstream: await providerTask(`long-jsstream-${nonce}`), jsblock: await providerTask(`long-jsblock-${nonce}`) };
check(ids.jsstream && ids.jsblock, 'js-hold-arrived', JSON.stringify(ids));
await new Promise((r) => setTimeout(r, hold * 1000));
for (const [tag, id] of Object.entries(ids)) if (id) await ctl('/tasks/reply', { task_id: id, text: `answered after ${hold}s (${tag})`, state: 'completed' });
for (const [tag, took, st, detail] of await Promise.all([streamJob, blockJob])) {
  check(st === TaskState.TASK_STATE_COMPLETED && took >= hold, `js-hold-${tag}`, `open ${took.toFixed(0)} s, ended ${TaskState[st]} (${detail})`);
}
process.exitCode = fails ? 1 : 0;
