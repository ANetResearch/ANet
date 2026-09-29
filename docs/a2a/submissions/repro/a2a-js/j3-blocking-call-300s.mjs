// J3: a blocking SendMessage (A2A §3.2.2: the server answers when the task
// is terminal or interrupted, with no upper bound) that takes longer than
// 300 s fails with Node's default fetch: undici gives up when no response
// headers arrive within headersTimeout (300 s), and the SDK's transports use
// that fetch unless a fetchImpl is passed. The error ("fetch failed", cause
// UND_ERR_HEADERS_TIMEOUT) does not say that the task goes on.
//
// The server here is a plain Node HTTP server that answers the JSON-RPC
// SendMessage after HOLD_SECONDS (default 320). The run takes that long.
//
// Run: node j3-blocking-call-300s.mjs            (about 5 minutes)
//      HOLD_SECONDS=240 node j3-blocking-call-300s.mjs   (control: succeeds)
// Exit status: 0 = behaviour reproduced, 1 = not reproduced, 2 = setup error.
import { createServer } from 'node:http';
import { randomUUID } from 'node:crypto';
import { Role, taskStateToJSON } from '@a2a-js/sdk';
import { ClientFactory } from '@a2a-js/sdk/client';

const hold = Number(process.env.HOLD_SECONDS || 320);

const srv = createServer((req, res) => {
  const origin = `http://127.0.0.1:${srv.address().port}`;
  if (req.method === 'GET') {
    res.setHeader('Content-Type', 'application/json');
    res.end(JSON.stringify({
      name: 'slow', description: 'answers after HOLD_SECONDS', version: '1',
      supportedInterfaces: [{ url: `${origin}/rpc`, protocolBinding: 'JSONRPC', protocolVersion: '1.0' }],
      capabilities: {}, defaultInputModes: ['text/plain'], defaultOutputModes: ['text/plain'],
      skills: [{ id: 's', name: 's', description: 'd', tags: ['t'] }],
    }));
    return;
  }
  let body = '';
  req.on('data', (c) => { body += c; });
  req.on('end', () => {
    const { id } = JSON.parse(body);
    setTimeout(() => {
      res.setHeader('Content-Type', 'application/json');
      res.end(JSON.stringify({
        jsonrpc: '2.0', id,
        result: { task: { id: 'task-1', contextId: 'ctx-1', status: { state: 'TASK_STATE_COMPLETED' } } },
      }));
    }, hold * 1000);
  });
});
await new Promise((r) => srv.listen(0, '127.0.0.1', r));
srv.headersTimeout = 0;
srv.requestTimeout = 0;
const base = `http://127.0.0.1:${srv.address().port}/`;

const client = await new ClientFactory().createFromUrl(base);
const message = {
  messageId: randomUUID(), contextId: '', taskId: '', role: Role.ROLE_USER,
  parts: [{ content: { $case: 'text', value: 'hi' }, metadata: undefined, filename: '', mediaType: '' }],
  metadata: undefined, extensions: [], referenceTaskIds: [],
};
const t0 = Date.now();
let outcome;
try {
  const r = await client.sendMessage({ tenant: '', message, metadata: undefined, configuration: undefined });
  outcome = `ok, task state ${taskStateToJSON(r.status?.state)}`;
} catch (e) {
  outcome = `${e.constructor.name}: ${e.message}${e.cause ? ` (cause ${e.cause.code || e.cause.name})` : ''}`;
}
const secs = ((Date.now() - t0) / 1000).toFixed(0);
srv.closeAllConnections();
srv.close();
console.log(`server answers after ${hold} s; blocking sendMessage returned after ${secs} s: ${outcome}`);
console.log('expected: the answer, whenever the server sends it (or a documented, configurable limit)');
const reproduced = hold > 300 && !outcome.startsWith('ok');
console.log(`reproduced: ${reproduced ? 'yes' : 'no'}`);
process.exit(reproduced ? 0 : 1);
