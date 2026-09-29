// J2: DefaultAgentCardResolver.resolve(baseUrl) builds
// new URL('.well-known/agent-card.json', baseUrl). By URL reference
// resolution, a base URL whose path does not end in "/" loses its last
// segment, so an agent served under a path prefix is looked up in the wrong
// place. a2a-python's A2ACardResolver appends the path to
// base_url.rstrip('/') instead.
//
// Run: node j2-resolver-base-path.mjs
// Exit status: 0 = behaviour reproduced, 1 = not reproduced, 2 = setup error.
import { createServer } from 'node:http';
import { DefaultAgentCardResolver } from '@a2a-js/sdk/client';

const card = {
  name: 'n', description: 'd', version: '1',
  supportedInterfaces: [{ url: 'http://127.0.0.1/agents/alice', protocolBinding: 'JSONRPC', protocolVersion: '1.0' }],
  capabilities: {}, defaultInputModes: ['text/plain'], defaultOutputModes: ['text/plain'],
  skills: [{ id: 's', name: 's', description: 'd', tags: ['t'] }],
};

const requested = [];
const srv = createServer((req, res) => {
  requested.push(req.url);
  if (req.url === '/agents/alice/.well-known/agent-card.json') {
    res.setHeader('Content-Type', 'application/json');
    res.end(JSON.stringify(card));
    return;
  }
  res.statusCode = 404;
  res.end();
});
await new Promise((r) => srv.listen(0, '127.0.0.1', r));
const origin = `http://127.0.0.1:${srv.address().port}`;

const resolver = new DefaultAgentCardResolver();
async function attempt(base) {
  requested.length = 0;
  try {
    const c = await resolver.resolve(base);
    return `ok (${c.name}), fetched ${requested.join(', ')}`;
  } catch (e) {
    return `${e.message.replace(origin, '')}, fetched ${requested.join(', ')}`;
  }
}
const withoutSlash = await attempt(`${origin}/agents/alice`);
const withSlash = await attempt(`${origin}/agents/alice/`);
srv.close();
console.log(`resolve("<origin>/agents/alice"):  ${withoutSlash}`);
console.log(`resolve("<origin>/agents/alice/"): ${withSlash}`);
console.log('expected: both fetch /agents/alice/.well-known/agent-card.json (as a2a-python does)');
const reproduced = !withoutSlash.startsWith('ok') && withSlash.startsWith('ok');
console.log(`reproduced: ${reproduced ? 'yes' : 'no'}`);
process.exit(reproduced ? 0 : 1);
