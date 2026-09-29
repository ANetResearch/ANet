"""P1: a2a-sdk's JSON-RPC client does not read the A2A error of a non-2xx
answer to a streaming call (SendStreamingMessage, SubscribeToTask).

JsonRpcTransport._send_stream_request passes status_error_handler=None, so
send_http_stream_request raises on the HTTP status and the JSON-RPC error
object in the body is never parsed. The HTTP+JSON transport, and the
JSON-RPC transport for a 200 answer, do read it.

A local stub server refuses SubscribeToTask four ways; the SDK client is
used exactly as documented (ClientFactory + ClientConfig).

Run: python p1_stream_error_status.py
Exit status: 0 = behaviour reproduced, 1 = not reproduced, 2 = setup error.
"""

import asyncio
import json
import sys
import threading

from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import httpx

from google.protobuf.json_format import ParseDict

from a2a.client import ClientConfig, ClientFactory
from a2a.types import AgentCard, SubscribeToTaskRequest


def jsonrpc_error(req_id):
    return {'jsonrpc': '2.0', 'id': req_id,
            'error': {'code': -32004, 'message': 'task is in a terminal state'}}


REST_ERROR = {'error': {'code': 400, 'status': 'FAILED_PRECONDITION', 'message': 'task is in a terminal state',
                        'details': [{'@type': 'type.googleapis.com/google.rpc.ErrorInfo',
                                     'reason': 'UNSUPPORTED_OPERATION', 'domain': 'a2a-protocol.org'}]}}


class Stub(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def _send(self, status, ctype, body):
        data = body.encode()
        self.send_response(status)
        self.send_header('Content-Type', ctype)
        self.send_header('Content-Length', str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_POST(self):
        raw = self.rfile.read(int(self.headers.get('Content-Length') or 0))
        if self.path.startswith('/rest/'):
            self._send(400, 'application/json', json.dumps(REST_ERROR))
            return
        req_id = json.loads(raw).get('id')
        err = json.dumps(jsonrpc_error(req_id))
        if self.path == '/sse200':
            self._send(200, 'text/event-stream', 'data: %s\n\n' % err)
        elif self.path == '/json200':
            self._send(200, 'application/json', err)
        else:  # /json400: the HTTP status the HTTP+JSON binding uses for this error
            self._send(400, 'application/json', err)

    do_GET = do_POST


def card(url, binding):
    return ParseDict({
        'name': 'stub', 'description': 'd', 'version': '1',
        'supportedInterfaces': [{'url': url, 'protocolBinding': binding, 'protocolVersion': '1.0'}],
        'capabilities': {'streaming': True}, 'defaultInputModes': ['text/plain'],
        'defaultOutputModes': ['text/plain'], 'skills': [{'id': 's', 'name': 's', 'description': 'd', 'tags': ['t']}],
    }, AgentCard())


async def subscribe(url, binding):
    async with httpx.AsyncClient() as hx:
        client = ClientFactory(ClientConfig(streaming=True, httpx_client=hx,
                                            supported_protocol_bindings=[binding])).create(card(url, binding))
        try:
            async for _ in client.subscribe(SubscribeToTaskRequest(id='task-1')):
                pass
            return 'no error'
        except Exception as e:  # what the application sees
            return '%s: %s' % (type(e).__name__, str(e).splitlines()[0][:90])


async def main():
    srv = ThreadingHTTPServer(('127.0.0.1', 0), Stub)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    base = 'http://127.0.0.1:%d' % srv.server_address[1]
    cases = [
        ('JSON-RPC, 200 text/event-stream, error event', base + '/sse200', 'JSONRPC'),
        ('JSON-RPC, 200 application/json error', base + '/json200', 'JSONRPC'),
        ('JSON-RPC, 400 application/json error', base + '/json400', 'JSONRPC'),
        ('HTTP+JSON, 400 google.rpc.Status', base + '/rest', 'HTTP+JSON'),
    ]
    lost = False
    for name, url, binding in cases:
        got = await subscribe(url, binding)
        print('%-46s -> %s' % (name, got))
        if 'UnsupportedOperation' not in got:
            lost = True
    srv.shutdown()
    print('expected: UnsupportedOperationError in every case')
    print('reproduced: %s' % ('yes' if lost else 'no'))
    return 0 if lost else 1


if __name__ == '__main__':
    sys.exit(asyncio.run(main()))
