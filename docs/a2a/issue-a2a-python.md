# Issue drafts for a2a-python (`a2a-sdk`)

> **DRAFT — not submitted; to be filed only after more testing (待更多测试后再提交), and only with the
> product owner's approval.**
>
> **License: Apache-2.0**, a2a-python's own license: the text and the code in these drafts are licensed
> under the Apache License 2.0 alone (ANet `LICENSE`, condition 3).

Target: `a2a-sdk` **1.1.5** (PyPI, 2026-09-28), with httpx 0.28.1 and protobuf 7.36.2. Found while
driving anet's local A2A interface with the SDK's client and serving anet from an SDK-built server
(`docs/notes/0035` items 1 and 3; tests: `scripts/interop/py_client.py`, `py_backend.py`). Reproduced on
2026-09-28. Everything else the client was asked to do — cards and their signatures, both bindings,
blocking and non-blocking sends, streams held open seven minutes, ListTasks filters and paging, the
a2a-x402 same-task flow — worked as the spec says.

| # | Title | Area | Filing |
|---|---|---|---|
| P1 | JSON-RPC streaming calls: an error answered with a non-200 status loses its A2A error | `client/transports/jsonrpc.py`, `http_helpers.py` | public issue, together with a2a-go A12 (spec discussion first) |
| P2 | `TaskUpdater.submit()` as an executor's first event fails the task | `server/tasks`, docs | documentation issue |

---

## P1. JSON-RPC streaming calls: an error answered with a non-200 status loses its A2A error

**Code.** `JsonRpcTransport._send_stream_request` (`client/transports/jsonrpc.py:358`) calls
`send_http_stream_request(…, status_error_handler=None, …)`. For a non-2xx answer
`send_http_stream_request` (`http_helpers.py:133`) raises `HTTPStatusError`, and `handle_http_exceptions`
(`http_helpers.py:33-36`), with no handler, turns it into `A2AClientError("HTTP Error 400: …")` without
reading the body. A `200` answer that is not `text/event-stream` is read as a JSON-RPC response and its
error raised properly (`http_helpers.py:140-146`).

**Observed.** `SubscribeToTask` on a canceled task, through a server that refuses a stream that cannot start
with the binding's ordinary error at the HTTP status of the HTTP+JSON mapping (`400`, body
`{"jsonrpc":"2.0","id":…,"error":{"code":-32004,…}}`): the client raises
`A2AClientError: HTTP Error 400: Client error '400 Bad Request' …`; the `UnsupportedOperationError` in the
body is not surfaced. The same call over HTTP+JSON raises `UnsupportedOperationError`. The non-streaming
JSON-RPC methods are not affected when the server answers errors with 200.

**The three SDKs disagree** on how a JSON-RPC stream that cannot start is refused (anet `docs/notes/0035`):

| Answer to a streaming call | a2a-python 1.1.5 | a2a-js 1.2.1 | a2a-go v2.6.0 |
|---|---|---|---|
| 200, `text/event-stream`, one event carrying the error (what a2a-go's server sends) | error raised | error raised | error raised |
| 200, `application/json` error object | error raised | error raised | read as an empty stream, no error (a2a-go A12) |
| non-200, `application/json` error object | only "HTTP Error 400" | error raised | only the HTTP status |

No single answer is read correctly by all three; the one that is (an error inside a stream) is what
a2a-tck reports as the stream having been opened (STREAM-SUB-003/004).

**Suggested fix.** Read the body of a non-2xx answer to a streaming JSON-RPC call as a JSON-RPC response and
raise its error when there is one, before falling back to the HTTP status. The spec should also say how a
stream that cannot start is refused (see a2a-go A12).

**How anet copes.** It answers with the error object and the HTTP+JSON binding's status (§11.4, Q31), so
a2a-js reads the error, a2a-go and a2a-python see a failure without its name.

---

## P2. `TaskUpdater.submit()` as an executor's first event fails the task

**Observed.** An `AgentExecutor.execute` that starts with
`TaskUpdater(event_queue, context.task_id, context.context_id).submit()` when `context.current_task` is
`None` fails every request with `Agent should enqueue Task before TaskStatusUpdateEvent event`: in 1.x the
first event of a new task must be the `Task` itself (`new_task(...)` / `new_task_from_user_message(...)`),
then its status updates. `submit()` reads like the call that creates the task.

**Suggested fix.** Say so in `TaskUpdater.submit`'s docstring and the executor samples, or let `submit()`
enqueue the `Task` when none exists yet.

**Note (not an issue).** Metadata goes through `google.protobuf.Struct`, so integers come back as floats
(`anet.state_seq: 2.0`, a millisecond timestamp `1790604607171.0`); integers above 2^53 lose precision.
Servers that put large integers in metadata should send them as strings (anet does, §11.5).
