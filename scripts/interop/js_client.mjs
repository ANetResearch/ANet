// js_client.mjs — the official A2A JavaScript SDK (@a2a-js/sdk) against a requester's local A2A
// interface (docs/notes/0035 item 2; A2A-DESIGN §11, §8.7, §10.3). The same checks as py_client.py.
//
// Only the SDK's public client surface: DefaultAgentCardResolver, ClientFactory with
// JsonRpcTransportFactory / RestTransportFactory, createAuthenticatingFetchWithRetry for the Bearer,
// RequestOptions.serviceParameters with withA2AExtensions for a2a-x402, verifyAgentCardSignature for
// the cards. What the SDK hands back is compared with the raw JSON-RPC GetTask body and with the
// control plane's /tasks/get.
//
//   A2A_JS_DIR=<dir with node_modules/@a2a-js/sdk> node js_client.mjs ENV_JSON
//
// One line per check: PASS/FAIL/NOTE id: detail. Exit 0 / 1 (a check failed) / 2 (could not run).
import { createRequire } from 'node:module';
import { readFileSync } from 'node:fs';
import { createHash, randomUUID } from 'node:crypto';

const req = createRequire((process.env.A2A_JS_DIR || process.cwd()) + '/package.json');
const sdk = req('@a2a-js/sdk');
const cl = req('@a2a-js/sdk/client');
const { TaskState, Role, Task, AgentCard, verifyAgentCardSignature } = sdk;
const { ClientFactory, JsonRpcTransportFactory, RestTransportFactory, DefaultAgentCardResolver,
  createAuthenticatingFetchWithRetry, ServiceParameters, withA2AExtensions } = cl;

const X402 = 'https://github.com/google-agentic-commerce/a2a-x402/blob/main/spec/v0.2';
const ORIGIN = 'https://agentnetwork.org.cn/a2a/ext/anet-origin/v1';
const ECHO = 'echo: ';
const HOLD = '[hold]';

let fails = 0;
const line = (k, id, m) => process.stdout.write(`${k} ${id}: ${m}\n`);
const ok = (id, m = '') => line('PASS', id, m);
const fail = (id, m = '') => { fails++; line('FAIL', id, m); };
const note = (id, m = '') => line('NOTE', id, m);
const check = (c, id, m = '') => { (c ? ok : fail)(id, m); return c; };

const env = JSON.parse(readFileSync(process.argv[2], 'utf8'));
const token = readFileSync(env.req.a2a_token_file, 'utf8').trim();
const ctlToken = readFileSync(env.req.ctl_token_file, 'utf8').trim();
const provCtlToken = readFileSync(env.prov.ctl_token_file, 'utf8').trim();
const agent = env.prov.aid;
const base = `http://${env.req.a2a}/a2a/v1/agents/${agent}`;
const baseSlash = base + '/';
const nonce = 'js' + randomUUID().replaceAll('-', '').slice(0, 10);
const sha = (t) => createHash('sha256').update(t).digest('hex');
const stateName = (t) => (t && t.status ? TaskState[t.status.state] : 'none');
const TERMINAL = new Set([TaskState.TASK_STATE_COMPLETED, TaskState.TASK_STATE_FAILED, TaskState.TASK_STATE_CANCELED,
  TaskState.TASK_STATE_REJECTED, TaskState.TASK_STATE_INPUT_REQUIRED]);

// Every response the SDK's fetch sees, for the A2A-Extensions echo.
const respHeaders = [];
const recordingFetch = async (...a) => {
  const r = await fetch(...a);
  respHeaders.push(Object.fromEntries(r.headers.entries()));
  return r;
};
const authFetch = (tok) => createAuthenticatingFetchWithRetry(recordingFetch, {
  headers: async () => ({ Authorization: `Bearer ${tok}` }),
  shouldRetryWithHeaders: async () => undefined,
});
const fetchImpl = authFetch(token);

function textPart(text) { return { content: { $case: 'text', value: text }, metadata: undefined, filename: '', mediaType: '' }; }
function dataPart(v) { return { content: { $case: 'data', value: v }, metadata: undefined, filename: '', mediaType: '' }; }
function msg(parts, { ctx = '', taskId = '', md } = {}) {
  return { messageId: randomUUID().replaceAll('-', ''), contextId: ctx, taskId, role: Role.ROLE_USER, parts,
    metadata: md, extensions: [], referenceTaskIds: [] };
}
function sendReq(m, immediate = false) {
  return { tenant: '', message: m, metadata: undefined,
    configuration: immediate ? { acceptedOutputModes: [], taskPushNotificationConfig: undefined, returnImmediately: true } : undefined };
}
const meta = (t, k) => (t?.metadata && k in t.metadata ? t.metadata[k] : t?.status?.message?.metadata?.[k]);
const statusMeta = (t, k) => t?.status?.message?.metadata?.[k];
const artifact = (t, id) => (t?.artifacts || []).find((a) => a.artifactId === id || a.name === id);
const partsText = (parts) => (parts || []).filter((p) => p.content?.$case === 'text').map((p) => p.content.value).join('');
const replyText = (t) => { const a = artifact(t, 'anet.reply'); return a ? partsText(a.parts) : null; };
const isTask = (r) => r && typeof r === 'object' && 'status' in r && 'artifacts' in r;
const errName = (e) => `${e?.constructor?.name}: ${String(e?.message || e).slice(0, 140)}`;
const opts = (x402 = false, timeoutMs = 120000) => ({
  signal: AbortSignal.timeout(timeoutMs),
  serviceParameters: x402 ? ServiceParameters.create(withA2AExtensions(X402)) : undefined,
});
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
// The SDK's verifier logs every failed signature with console.debug; keep that out of the report.
async function quiet(fn) {
  const saved = [console.log, console.warn, console.error, console.debug];
  console.log = console.warn = console.error = console.debug = () => {};
  try { return await fn(); } finally { [console.log, console.warn, console.error, console.debug] = saved; }
}

async function ctl(path, body, prov = false) {
  const n = prov ? env.prov : env.req;
  const r = await fetch(`http://${n.ctl}${path}`, { method: 'POST', body: JSON.stringify(body),
    headers: { Authorization: `Bearer ${prov ? provCtlToken : ctlToken}`, 'Content-Type': 'application/json' } });
  return r.json();
}
async function rawRpc(method, params) {
  const r = await fetch(`${base}/jsonrpc`, { method: 'POST', body: JSON.stringify({ jsonrpc: '2.0', id: 1, method, params }),
    headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json', 'A2A-Version': '1.0' } });
  return r.json();
}

const TS = /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?Z$/;
function canon(v) {
  if (typeof v === 'string' && TS.test(v)) return new Date(v).toISOString();
  if (Array.isArray(v)) return v.map(canon);
  if (v && typeof v === 'object') {
    const out = {};
    for (const k of Object.keys(v).sort()) {
      const x = canon(v[k]);
      if (x === undefined || x === null || x === '' || x === false ||
        (Array.isArray(x) && !x.length) || (typeof x === 'object' && !Array.isArray(x) && !Object.keys(x).length)) continue;
      out[k] = x;
    }
    return out;
  }
  return v;
}
function diff(a, b, p = '') {
  if (typeof a !== typeof b || Array.isArray(a) !== Array.isArray(b)) return [`${p || '/'}: ${JSON.stringify(a)?.slice(0, 60)} vs ${JSON.stringify(b)?.slice(0, 60)}`];
  if (Array.isArray(a)) {
    if (a.length !== b.length) return [`${p}: ${a.length} vs ${b.length} items`];
    return a.flatMap((x, i) => diff(x, b[i], `${p}[${i}]`));
  }
  if (a && typeof a === 'object') {
    const ks = [...new Set([...Object.keys(a), ...Object.keys(b)])].sort();
    return ks.flatMap((k) => (!(k in a) || !(k in b) ? [`${p}/${k} only in ${k in a ? 'anet' : 'sdk'}`] : diff(a[k], b[k], `${p}/${k}`)));
  }
  return a === b ? [] : [`${p}: ${JSON.stringify(a)?.slice(0, 60)} vs ${JSON.stringify(b)?.slice(0, 60)}`];
}
async function sameAsProjection(id, t) {
  const raw = (await rawRpc('GetTask', { id: t.id })).result;
  const d = diff(canon(raw), canon(Task.toJSON(t)));
  const c = await ctl('/tasks/get', { task_id: t.id });
  const sdkMeta = t.metadata || {};
  const keys = (m) => Object.keys(m || {}).filter((k) => k.startsWith('anet.') || k.startsWith('x402.')).sort();
  const sameKeys = JSON.stringify(keys(c.metadata)) === JSON.stringify(keys(sdkMeta));
  const sameVals = keys(c.metadata).filter((k) => k !== 'anet.receipt')
    .every((k) => JSON.stringify(canon(c.metadata[k])) === JSON.stringify(canon(sdkMeta[k])));
  check(!d.length && c.status?.state === `TASK_STATE_${stateName(t).replace('TASK_STATE_', '')}` && sameKeys && sameVals, id,
    `SDK view == JSON-RPC bytes${d.length ? ' EXCEPT ' + d.slice(0, 4).join('; ') : ''}; control plane ${c.status?.state}, metadata ${sameKeys && sameVals ? 'equal' : 'differ'}`);
}

async function makeClient(card, binding) {
  const t = binding === 'JSONRPC' ? new JsonRpcTransportFactory({ fetchImpl }) : new RestTransportFactory({ fetchImpl });
  const f = new ClientFactory({ transports: [t], cardResolver: new DefaultAgentCardResolver({ fetchImpl }) });
  return f.createFromAgentCard(card);
}
async function waitTerminal(c, id, secs = 90) {
  const end = Date.now() + secs * 1000;
  let t;
  while (Date.now() < end) {
    t = await c.getTask({ tenant: '', id }, opts());
    if (TERMINAL.has(t.status.state)) return t;
    await sleep(300);
  }
  return t;
}
async function collect(gen) { const out = []; for await (const ev of gen) out.push(ev); return out; }
const kinds = (evs) => evs.map((e) => e.payload?.$case).reduce((a, k) => { const l = a[a.length - 1]; if (l && l[0] === k) l[1]++; else a.push([k, 1]); return a; }, []).map(([k, n]) => (n > 1 ? `${k}×${n}` : k)).join(',');

// -- 1. cards --------------------------------------------------------
const keyCache = new Map();
async function keyFor(kid, jku) {
  if (!kid?.startsWith('did:anet:')) throw new Error(`kid ${kid}`);
  const aid = kid.slice('did:anet:'.length).split('#')[0];
  const url = jku || `${env.hub}/agents/${aid}/jwks.json`;
  if (!url.startsWith(env.hub + '/')) throw new Error(`jku ${url} is not this hub's`);
  if (!keyCache.has(url)) keyCache.set(url, (await (await fetch(url)).json()).keys);
  const k = keyCache.get(url).find((x) => x.kid === kid);
  if (!k) throw new Error(`no key ${kid} at ${url}`);
  return k;
}
async function cards() {
  const verify = verifyAgentCardSignature(keyFor);
  // The resolver resolves the card path against the base URL as a URL reference (WHATWG): without a
  // trailing slash the last segment — the AID — is replaced. anet's base URL is written without one
  // (§11.2, Hermes appends the path itself), so a JS client has to add it.
  try {
    await new DefaultAgentCardResolver({ fetchImpl }).resolve(base);
    note('card-base-noslash', 'resolve(<base>) found the card');
  } catch (e) { note('card-base-noslash', `resolve(<base> without a trailing slash): ${errName(e)}`); }
  try {
    await new DefaultAgentCardResolver({ fetchImpl: recordingFetch }).resolve(baseSlash);
    fail('card-no-token', 'the card was served without the bearer');
  } catch (e) { check(/401/.test(String(e.message)), 'card-no-token', `resolve() without the token: ${errName(e)}`); }
  const card = await new DefaultAgentCardResolver({ fetchImpl }).resolve(baseSlash);
  check(card.name && card.supportedInterfaces.map((i) => i.protocolBinding).join() === 'JSONRPC,HTTP+JSON', 'card',
    `DefaultAgentCardResolver + Bearer: ${card.name}, ${card.supportedInterfaces.map((i) => i.protocolBinding)}`);
  // verifyAgentCardSignature canonicalizes with AgentCard.fromJSON(card): given the SDK's own parsed card
  // (what the resolver returns and Client.getAgentCard hands the verifier) the oneof members — here
  // securitySchemes.anetLocal.httpAuthSecurityScheme — are dropped from the payload, so a card with
  // securitySchemes never verifies that way (docs/a2a/issue-a2a-js.md J1). The JSON form verifies.
  try { await quiet(() => verify(AgentCard.toJSON(card))); ok('card-signature-proxy', 'verifyAgentCardSignature accepts the proxy card (as JSON)'); } catch (e) { fail('card-signature-proxy', errName(e)); }
  try { await quiet(() => verify(card)); ok('card-signature-proxy-parsed', 'the parsed card verifies as well'); } catch (e) {
    note('card-signature-proxy-parsed', `SDK defect J1: the parsed card (resolver output) does not verify: ${errName(e)}`);
  }
  const alias = await new DefaultAgentCardResolver({ fetchImpl }).resolve(base, '');  // GET <base>, the alias route
  check(JSON.stringify(AgentCard.toJSON(alias)) === JSON.stringify(AgentCard.toJSON(card)), 'card-alias', 'GET <base> serves the same card');
  try { await quiet(() => verify({ ...AgentCard.toJSON(card), name: card.name + ' (edited)' })); fail('card-signature-tamper', 'an edited card verified'); } catch { ok('card-signature-tamper', 'an edited proxy card does not verify'); }
  const raw = await (await fetch(`${env.hub}/a2a/v1/agents/${agent}/card`)).text();
  const net = new DefaultAgentCardResolver().normalizeAgentCard(JSON.parse(raw));
  try { await quiet(() => verify(net)); ok('card-signature-network', 'the provider network card verifies (jku at the hub)'); } catch (e) { fail('card-signature-network', errName(e)); }
  const origin = card.capabilities.extensions.find((e) => e.uri === ORIGIN)?.params;
  check(origin?.originVerification === 'VERIFIED' && Buffer.from(origin.originCard, 'base64url').toString() === raw, 'card-origin',
    'anet-origin: VERIFIED, originCard == the hub bytes');
  // createFromUrl: the SDK's one-call path, then Client.getAgentCard with the verifier.
  const f = new ClientFactory({ transports: [new JsonRpcTransportFactory({ fetchImpl })], cardResolver: new DefaultAgentCardResolver({ fetchImpl }) });
  try {
    const c = await f.createFromUrl(baseSlash);
    ok('card-create-from-url', 'ClientFactory.createFromUrl(<base>/)');
    try { await quiet(() => c.getAgentCard(undefined, verify)); ok('card-getagentcard-verify', 'Client.getAgentCard(verifier)'); } catch (e) {
      note('card-getagentcard-verify', `SDK defect J1: Client.getAgentCard(verifier) refuses the proxy card: ${errName(e)}`);
    }
  } catch (e) { fail('card-create-from-url', errName(e)); }
  return card;
}

// -- 2. text tasks ---------------------------------------------------
async function text(tag, binding, card) {
  const c = await makeClient(card, binding);
  const ctx = `ctx-js-${tag}-${randomUUID().slice(0, 8)}`;
  const t1 = `js ${tag} blocking ${nonce}`;
  const s0 = Date.now();
  const b = await c.sendMessage(sendReq(msg([textPart(t1)], { ctx })), opts());
  check(isTask(b) && b.status.state === TaskState.TASK_STATE_COMPLETED && replyText(b) === ECHO + t1 && b.contextId === ctx &&
    meta(b, 'anet.receipt_verified') === 'verified' && meta(b, 'anet.peer_aid') === agent, `${tag}-blocking`,
  `${stateName(b)} in ${((Date.now() - s0) / 1000).toFixed(1)}s, anet.reply ${JSON.stringify(replyText(b))}, contextId kept ${b.contextId === ctx}`);
  if (isTask(b)) await sameAsProjection(`${tag}-projection`, b);

  const t2 = `js ${tag} immediate ${nonce}`;
  const im = await c.sendMessage(sendReq(msg([textPart(t2)], { ctx }), true), opts());
  check(isTask(im) && [TaskState.TASK_STATE_SUBMITTED, TaskState.TASK_STATE_WORKING].includes(im.status.state), `${tag}-immediate`, `returnImmediately: ${stateName(im)}`);
  const done = await waitTerminal(c, im.id);
  check(done.status.state === TaskState.TASK_STATE_COMPLETED && replyText(done) === ECHO + t2, `${tag}-immediate-done`, `GetTask: ${stateName(done)}, ${JSON.stringify(replyText(done))}`);

  const t3 = `js ${tag} stream ${nonce}`;
  const evs = await collect(c.sendMessageStream(sendReq(msg([textPart(t3)], { ctx })), opts()));
  const ups = evs.filter((e) => e.payload?.$case === 'statusUpdate').map((e) => e.payload.value);
  const art = evs.filter((e) => e.payload?.$case === 'artifactUpdate').map((e) => e.payload.value.artifact)
    .filter((a) => a.artifactId === 'anet.reply' || a.name === 'anet.reply').map((a) => partsText(a.parts)).join('');
  check(evs[0]?.payload?.$case === 'task' && ups.at(-1)?.status.state === TaskState.TASK_STATE_COMPLETED && art === ECHO + t3,
    `${tag}-stream`, `events ${kinds(evs)}; anet.reply ${JSON.stringify(art)}`);
  const streamId = evs[0]?.payload?.value?.id;

  const g = await c.getTask({ tenant: '', id: b.id, historyLength: 1 }, opts());
  check(g.id === b.id && g.history.length === 1, `${tag}-get`, `GetTask(historyLength=1): ${stateName(g)}, ${g.history.length} history`);

  const lr = await c.listTasks({ tenant: '', contextId: ctx, status: 0, pageSize: 10, pageToken: '', statusTimestampAfter: undefined }, opts());
  const want = [streamId, im.id, b.id];
  check(JSON.stringify(lr.tasks.map((x) => x.id)) === JSON.stringify(want) && lr.totalSize === 3, `${tag}-list`,
    `ListTasks(contextId): ${lr.tasks.length} tasks, totalSize ${lr.totalSize}, newest first ${JSON.stringify(lr.tasks.map((x) => x.id)) === JSON.stringify(want)}`);
  const p1 = await c.listTasks({ tenant: '', contextId: ctx, status: 0, pageSize: 1, pageToken: '', statusTimestampAfter: undefined }, opts());
  const p2 = p1.nextPageToken ? await c.listTasks({ tenant: '', contextId: ctx, status: 0, pageSize: 1, pageToken: p1.nextPageToken, statusTimestampAfter: undefined }, opts()) : null;
  check(p1.tasks.length === 1 && p2?.tasks?.[0]?.id === want[1], `${tag}-list-page`, `pageSize 1 then nextPageToken: ${p1.tasks[0]?.id === want[0]}, ${p2?.tasks?.[0]?.id === want[1]}`);

  const h = await c.sendMessage(sendReq(msg([textPart(`js ${tag} ${HOLD} cancel me ${nonce}`)]), true), opts());
  await sleep(1000);
  const cx = await c.cancelTask({ tenant: '', id: h.id, metadata: undefined }, opts());
  check(cx.status.state === TaskState.TASK_STATE_CANCELED, `${tag}-cancel`, `CancelTask: ${stateName(cx)}`);
  let provOk = false;
  for (let i = 0; i < 60 && !provOk; i++) { provOk = (await ctl('/tasks/get', { task_id: h.id }, true)).status?.state === 'TASK_STATE_CANCELED'; if (!provOk) await sleep(500); }
  check(provOk, `${tag}-cancel-provider`, 'the provider side is canceled too');
  try { await c.cancelTask({ tenant: '', id: h.id, metadata: undefined }, opts()); fail(`${tag}-cancel-again`, 'no error'); } catch (e) { check(/cancel/i.test(errName(e)), `${tag}-cancel-again`, errName(e)); }
  try { await collect(c.resubscribeTask({ tenant: '', id: h.id }, opts())); fail(`${tag}-subscribe-terminal`, 'subscribed to a canceled task'); } catch (e) {
    check(/UnsupportedOperation|-32004|not supported/i.test(errName(e)), `${tag}-subscribe-terminal`, errName(e));
  }
  try { await c.getTask({ tenant: '', id: 'ix_' + '0'.repeat(32) }, opts()); fail(`${tag}-notfound`, 'found'); } catch (e) { check(/NotFound|-32001|not found/i.test(errName(e)), `${tag}-notfound`, errName(e)); }
  try {
    await c.createTaskPushNotificationConfig({ tenant: '', id: '', taskId: b.id, url: 'http://127.0.0.1:1/x', token: '', authentication: undefined }, opts());
    fail(`${tag}-push`, 'accepted');
  } catch (e) { check(/PushNotification|-32003/i.test(errName(e)), `${tag}-push`, errName(e)); }
  const n0 = respHeaders.length;
  try {
    const ec = await c.getAgentCard(opts());
    check(respHeaders.length === n0 && ec.name === card.name, `${tag}-extended-card-sdk`, 'Client.getAgentCard returns the card it has (no extendedAgentCard), no request');
  } catch (e) { fail(`${tag}-extended-card-sdk`, errName(e)); }
  const bad = await new ClientFactory({ transports: [binding === 'JSONRPC' ? new JsonRpcTransportFactory({ fetchImpl: authFetch('0'.repeat(64)) }) : new RestTransportFactory({ fetchImpl: authFetch('0'.repeat(64)) })] }).createFromAgentCard(card);
  try { await bad.sendMessage(sendReq(msg([textPart(`js wrong token ${nonce}`)])), opts()); fail(`${tag}-401`, 'accepted'); } catch (e) { check(/401|nauthenticated|auth|bearer/i.test(errName(e)) || e?.statusCode === 401 || e?.code === 401, `${tag}-401`, errName(e)); }
}

// -- 3. capability calls ---------------------------------------------
async function capability(tag, binding, card) {
  const c = await makeClient(card, binding);
  const free = env.skills.free;
  const tx = `js free ${nonce}`;
  for (const [form, m] of [['metadata', msg([dataPart({ text: tx })], { md: { 'anet.skill': free } })],
    ['datapart', msg([dataPart({ skill: free, args: { text: tx } })])]]) {
    const t = await c.sendMessage(sendReq(m), opts());
    const res = artifact(t, 'anet.result');
    check(t.status.state === TaskState.TASK_STATE_COMPLETED && JSON.stringify(res || {}).includes(sha(tx)) && meta(t, 'anet.effect_status') === 'OK',
      `${tag}-cap-${form}`, `${form} → ${stateName(t)}, anet.result carries sha256(text), effect ${meta(t, 'anet.effect_status')}`);
    if (form === 'datapart') await sameAsProjection(`${tag}-cap-projection`, t);
  }
}

// -- 5. what else a real client sends ----------------------------------
async function providerReply(marker, text) {
  for (let i = 0; i < 120; i++) {
    const r = await ctl('/tasks/list', { role: 'inbound', page_size: 50, history_length: 1 }, true);
    const t = (r.tasks || []).find((x) => (x.history || []).some((m) => (m.parts || []).some((pp) => (pp.text || '').includes(marker))));
    if (t) {
      const r2 = await fetch(`http://${env.prov.ctl}/tasks/reply`, { method: 'POST', body: JSON.stringify({ task_id: t.id, text, state: 'completed' }),
        headers: { Authorization: `Bearer ${provCtlToken}`, 'Content-Type': 'application/json' } });
      return r2.status;
    }
    await sleep(500);
  }
  return null;
}
async function extras(tag, binding, card) {
  const c = await makeClient(card, binding);
  const marker = `js ${tag} ${HOLD} subscribe ${nonce}`;
  const t = await c.sendMessage(sendReq(msg([textPart(marker)]), true), opts());
  const job = collect(c.resubscribeTask({ tenant: '', id: t.id }, opts()));
  await sleep(2000);
  const code = await providerReply(marker, 'answered while subscribed');
  try {
    const evs = await job;
    const ups = evs.filter((e) => e.payload?.$case === 'statusUpdate').map((e) => e.payload.value.status.state);
    check(code === 200 && evs[0]?.payload?.$case === 'task' && ups.at(-1) === TaskState.TASK_STATE_COMPLETED, `${tag}-subscribe`, `SubscribeToTask: ${kinds(evs)}`);
  } catch (e) { fail(`${tag}-subscribe`, errName(e)); }
  const uni = `js ${tag} 多语言 ✓ émoji 🧪 עברית \u202e rtl ${nonce}`;
  const u = await c.sendMessage(sendReq(msg([textPart(uni)])), opts());
  check(replyText(u) === ECHO + uni, `${tag}-unicode`, `echo of non-ASCII text is exact: ${replyText(u) === ECHO + uni}`);
  const big = `js ${tag} big ${nonce} ` + '0123456789abcdef'.repeat(65536 * 3);
  const evs = await collect(c.sendMessageStream(sendReq(msg([textPart(big)])), opts(false, 300000)));
  const art = evs.filter((e) => e.payload?.$case === 'artifactUpdate').map((e) => partsText(e.payload.value.artifact.parts)).join('');
  const last = evs.filter((e) => e.payload?.$case === 'statusUpdate').map((e) => e.payload.value.status.state).at(-1);
  check(art === ECHO + big && last === TaskState.TASK_STATE_COMPLETED, `${tag}-big-stream`, `${(big.length / 2 ** 20).toFixed(1)} MiB echo through a stream: ${TaskState[last]}, ${art.length} chars back`);
  const blob = Buffer.from(Array.from({ length: 16384 }, (_, i) => i % 256));
  const f = await c.sendMessage(sendReq(msg([textPart(`js ${tag} file ${nonce}`), { content: { $case: 'raw', value: blob }, metadata: undefined, filename: 'blob.bin', mediaType: 'application/octet-stream' }])), opts());
  check(f.status.state === TaskState.TASK_STATE_COMPLETED && (f.history[0]?.parts || []).some((pp) => pp.filename === 'blob.bin'), `${tag}-raw-part`, `${stateName(f)}; parts ${(f.history[0]?.parts || []).map((pp) => pp.filename || pp.content?.$case)}`);
  try {
    await c.sendMessage(sendReq(msg([{ content: { $case: 'url', value: 'https://example.com/x.pdf' }, metadata: undefined, filename: 'x.pdf', mediaType: '' }])), opts());
    fail(`${tag}-url-part`, 'accepted');
  } catch (e) { check(/InvalidParams|url/i.test(errName(e)), `${tag}-url-part`, errName(e)); }
  const lr = await c.listTasks({ tenant: '', contextId: '', status: TaskState.TASK_STATE_COMPLETED, pageSize: 5, pageToken: '', historyLength: 0, statusTimestampAfter: undefined }, opts());
  check(lr.tasks.length && lr.tasks.every((x) => x.status.state === TaskState.TASK_STATE_COMPLETED && !x.history.length), `${tag}-list-status`,
    `status=COMPLETED, historyLength=0: ${lr.tasks.length} task(s), histories ${lr.tasks.map((x) => x.history.length)}`);
  const after = new Date(Date.now() - 30000).toISOString();
  const la = await c.listTasks({ tenant: '', contextId: '', status: 0, pageSize: 100, pageToken: '', statusTimestampAfter: after, includeArtifacts: true }, opts());
  check(la.tasks.length && la.tasks.every((x) => x.status.timestamp >= after.slice(0, 19)) && la.tasks.some((x) => x.artifacts.length), `${tag}-list-after`,
    `statusTimestampAfter 30 s ago + includeArtifacts: ${la.tasks.length} task(s), with artifacts ${la.tasks.filter((x) => x.artifacts.length).length}`);
}

// -- 4. a2a-x402 -----------------------------------------------------
async function quote(c, skill, tx, x402 = true) {
  const t = await c.sendMessage(sendReq(msg([dataPart({ skill, args: { text: tx } })])), opts(x402));
  const r = statusMeta(t, 'x402.payment.required') || meta(t, 'x402.payment.required') || {};
  return [t, r.accepts || []];
}
const payMsg = (t, md) => sendReq(msg([textPart('payment')], { ctx: t.contextId, taskId: t.id, md }));
async function pay(tag, binding, card) {
  const c = await makeClient(card, binding);
  const tx = `js ${tag} paid ${nonce}`;
  respHeaders.length = 0;
  const [t, accepts] = await quote(c, env.skills.paid, tx);
  check(t.status.state === TaskState.TASK_STATE_INPUT_REQUIRED && meta(t, 'x402.payment.status') === 'payment-required' && accepts.length,
    `${tag}-quote`, `${stateName(t)}, ${meta(t, 'x402.payment.status')}, ${accepts.length} option(s)`);
  const echoed = respHeaders.at(-1)?.['a2a-extensions'];
  check((echoed || '').includes(X402), `${tag}-ext-echo`, `A2A-Extensions echoed: ${echoed}`);
  const r = await c.sendMessage(payMsg(t, { 'x402.payment.status': 'payment-submitted', 'anet.payment.accept': accepts[0] }), opts(true));
  const rec = (meta(r, 'x402.payment.receipts') || []).filter((x) => x.success);
  check(r.status.state === TaskState.TASK_STATE_COMPLETED && meta(r, 'x402.payment.status') === 'payment-completed' && rec.length === 1 &&
    JSON.stringify(r.artifacts).includes(sha(tx)), `${tag}-pay-accept`, `payment-submitted + anet.payment.accept → ${stateName(r)}, ${meta(r, 'x402.payment.status')}, anet.reason=${statusMeta(r, 'anet.reason')}`);
  if (r.status.state === TaskState.TASK_STATE_COMPLETED) await sameAsProjection(`${tag}-pay-projection`, r);

  const [t2, a2] = await quote(c, env.skills.pricey, `js ${tag} pricey ${nonce}`);
  if (!a2.length) { fail(`${tag}-over-quote`, stateName(t2)); return; }
  const o = await c.sendMessage(payMsg(t2, { 'x402.payment.status': 'payment-submitted' }), opts(true));
  check(o.status.state === TaskState.TASK_STATE_INPUT_REQUIRED && meta(o, 'anet.reason') === 'needs_operator_approval', `${tag}-over-limit`,
    `${stateName(o)}, anet.reason=${meta(o, 'anet.reason')}, ${meta(o, 'x402.payment.status')}`);
  const rj = await c.sendMessage(payMsg(t2, { 'x402.payment.status': 'payment-rejected' }), opts(true));
  check(rj.status.state === TaskState.TASK_STATE_CANCELED, `${tag}-reject`, `payment-rejected → ${stateName(rj)}`);

  const [t3, a3] = await quote(c, env.skills.paid, `js ${tag} paid-stream ${nonce}`);
  if (a3.length) {
    const evs = await collect(c.sendMessageStream(payMsg(t3, { 'x402.payment.status': 'payment-submitted' }), opts(true)));
    const ups = evs.filter((e) => e.payload?.$case === 'statusUpdate').map((e) => e.payload.value);
    check(ups.at(-1)?.status.state === TaskState.TASK_STATE_COMPLETED, `${tag}-pay-stream`, `streamed payment: ${kinds(evs)}, last ${TaskState[ups.at(-1)?.status.state]}`);
  }
  const [t4] = await quote(c, env.skills.paid, `js ${tag} paid-noext ${nonce}`, false);
  check(t4.status.state === TaskState.TASK_STATE_INPUT_REQUIRED && meta(t4, 'anet.reason') === 'payment_extension_not_activated', `${tag}-not-activated`,
    `${stateName(t4)}, anet.reason=${meta(t4, 'anet.reason')}`);
  await c.cancelTask({ tenant: '', id: t4.id, metadata: undefined }, opts());
}

async function main() {
  const pkg = JSON.parse(readFileSync((process.env.A2A_JS_DIR || process.cwd()) + '/node_modules/@a2a-js/sdk/package.json', 'utf8'));
  note('sdk', `@a2a-js/sdk ${pkg.version}, node ${process.version}`);
  let card;
  try { card = await cards(); } catch (e) { fail('cards', `raised ${errName(e)}`); return 2; }
  for (const [tag, b] of [['jsonrpc', 'JSONRPC'], ['rest', 'HTTP+JSON']]) {
    for (const [name, fn] of [['text', text], ['cap', capability], ['pay', pay], ['extras', extras]]) {
      try { await fn(tag, b, card); } catch (e) { fail(`${tag}-${name}`, `raised ${errName(e)}${e?.stack ? ' @ ' + e.stack.split('\n')[1]?.trim() : ''}`); }
    }
  }
  return fails ? 1 : 0;
}
process.exitCode = await main();
