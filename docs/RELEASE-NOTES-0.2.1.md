# anet 0.2.1 release notes

[中文](RELEASE-NOTES-0.2.1-zh.md)

Prepared 2026-09-28; the release date is the day the tags are pushed.

anet 0.2.1 is a patch release of 0.2.0. It fixes what testing with real clients (the official A2A
Python and JS SDKs, Hermes, Claude Code over MCP) and a 4.25-hour mixed-load run on the lab test network
found, and closes four gaps those runs left open. It ships with **ANetHub 0.2.1**, on the same kernel,
**ANetCore v0.15.0**.

**0.2.1 speaks the same wire as 0.2.0.** Nodes and hubs of 0.2.0 and 0.2.1 interoperate in any
combination, and they can be upgraded in any order; nothing needs migrating by hand. What changes in
behavior is listed in section 4.

The test records: docs/notes/0035 (real clients) and 0036 (test-network soak) in the source tree.

---

## 1. Fixes from testing with real clients

- **MCP `list_tasks` could return megabytes.** Each listed task carried its latest message whole, so one
  3 MiB reply made every page that listed its task megabytes long (9.4 MB for 20 tasks), more than an MCP
  client takes as a tool result, and let a peer pour arbitrary text into the requester's model context.
  Each listed task is now held to about 8 KB: a longer message is replaced by a notice of its size and
  marked `anet.truncated`. The control plane's `/tasks/list` takes the bound as `max_task_bytes`.
- **Listing, counting and retry lookups slowed down as data grew.** A task row holds the goal, the request
  and the result, megabytes each for a long message, and SQLite reaches a column after them only through
  their overflow pages. After an hour of real-client traffic every `ListTasks` of the local A2A interface
  took 14–21 s and every `SendMessage` naming a context about 2 s. Listings, counts, the per-peer and
  per-context listings, the client-retry lookup (`a2a.messageId`) and the context-ownership check now read
  indexes only: `ListTasks` 0.13–0.24 s on the same data, and a 25-minute run afterwards no longer slowed
  down. The indexes are built once at the first start of 0.2.1 (about a second per GB of
  `interactions.db`).
- The test tool `a2aprobe responder` stopped answering after a few large tasks; fixed.
- Documented for the JS SDK: a card base URL must end in `/`; verify a card's signature on its JSON form;
  Node's default `fetch` gives up on a blocking call after 300 s (use streaming, or `returnImmediately`
  and `GetTask`).

## 2. Fixes from the test-network soak

- **An automatic payment woke waiters with "pay it" first (daemon).** For a quote within `auto_max` the
  quote was announced before the node paid it, so a blocking `/tasks/send`, MCP `wait_task`, a blocking
  A2A `SendMessage` or an A2A stream returned `input-required` / `needs_operator_approval`, and a stream
  ended there, while the task was being paid and completed seconds later (152 of 338 such calls in the
  run). The quote's announcement now waits for the payment attempt: waiters see the payment and `working`;
  they see `input-required` only when the node does not pay (above the tier, a payee off the list).
- **The hub answered "database is locked" to relay writes under concurrent polling (ANetHub).** A
  transaction that read before writing had to upgrade its lock while every poll wrote `last_seen_at`, and
  SQLite refused the upgrade without waiting. `hub.db` transactions now take the write lock when they
  begin, and wait under the busy timeout like any single statement. Senders had retried successfully, so
  no task was lost, but a send failed about twice in ten thousand.
- **Federation dedupe pruning scanned its whole table (ANetHub).** Every accepted federated forward
  deleted week-old entries from `fed_dedupe` with no index on the timestamp, a full scan of about 1 850
  new rows an hour. An index on `ts` is created at start.

## 3. Closed in 0.2.1

### 3.1 A task nobody answers no longer waits for ever

A provider's refusal notices are rate limited by design (`inbound.reject_notice`, six per peer per hour),
and past the limit a refusal is dropped without a word; a provider that has gone away is the same from the
requester's side. In a burst of 300 calls against a provider's per-caller quota, 64 tasks stayed
`submitted` until each client gave up on its own — an hour later for a Hermes entry.

A task this node sent now fails when it is still `submitted`, nothing at all has come from the peer (no
status, message or result), nothing of it is still queued to go out, and **`no_response_after`** (15
minutes by default) has passed since its delegation was delivered: state `failed`,
`anet.reason=no_response`, `anet.effect_status=UNVERIFIED` — the peer may have refused, may not have read
it, or may be running it. A result that arrives afterwards is still verified and recorded on the task,
without reopening it. The failure is recorded as `anet.task.no_response`.

- Configuration: `"no_response_after": "15m"` at the top level of `config.json` (a Go duration; `"0"`
  turns it off). A fresh `anet init` writes it; a config without it gets 15 minutes. `anet doctor` reports
  it (`tasks.no_response`), and a value that does not read stops the daemon from starting.
- A provider running a long capability call now sends `status{working}` when the call starts, so its
  requester does not take the running call for silence. A 0.2.0 provider does not; a long call of its
  that runs past the deadline is reported `no_response` and its result, when it comes, recorded late.
- See known limitation 27: the deadline cannot tell "refused without saying so" from "slow".

### 3.2 A2A backends: a failed forward is tried again

A text task forwarded to a provider-side A2A backend (`modules.a2a.backends`) whose backend was down —
not listening yet, restarting — stayed in the inbox until the daemon restarted.

A forward that failed for a reason a later attempt may fix — the backend not reached (no socket yet,
connection refused, a timeout) or answering 5xx — is now tried again: first after 5 s, doubling, at most
**`retry.max_interval`** (2 minutes) between attempts, until **`retry.give_up_after`** (10 minutes) has
passed since the first. The task stays as it is meanwhile. Before each attempt the daemon decides again
whether the task may still go to a backend: a peer taken off the trust list, a task answered meanwhile, or
a newer message from the requester ends the retries. Every attempt carries the same message id, so a
backend can tell a retry. A refusal by the socket-path and listener checks, a 4xx, an A2A error, or an
answer without content is not retried.

- Configuration: `"modules": {"a2a": {"retry": {"max_interval": "2m", "give_up_after": "10m"}, "backends":
  […]}}` (Go durations; `"give_up_after": "0"` does not retry). `anet doctor` reports them
  (`a2a.backends.retry`). Keep `give_up_after` under the requesters' `no_response_after`.
- Evidence: `anet.backend.forwarded` once, when the backend takes the message; a forward that ends without
  an answer is recorded once as `anet.backend.failed` (attempts, last error, whether it gave up).

### 3.3 MCP: a single task is held to about 24 KB

`send_message`, `get_task`, `wait_task`, `cancel_task` and `reply_task` returned a task whole, so a task
with a 3 MiB reply was a tool result Claude Code refuses (25 000 tokens by default), and the model did not
even learn the task's id.

They now return the task held to about 24 KB, by the same rule as `list_tasks`: what does not fit is
replaced by a notice of its size, marked `anet.truncated`, that says how to read it whole —
`anet task get <task_id> --full` in a terminal, and `anet pull <task_id>` for its files. The tool result,
which carries the task twice (as text and as structured content), stays under the Claude Code default.

- New CLI command: `anet task get <task_id> [--full] [--history N]` — the task as the A2A projection;
  without `--full` it is held to the same bound.
- Control plane: `/tasks/send`, `/tasks/get`, `/tasks/wait`, `/tasks/cancel` and `/tasks/reply` take an
  optional `max_task_bytes` (0, the default, returns the task whole; negative is 400).

### 3.4 Unix-socket backends in a directory of your own private group

Ubuntu's default umask 002 makes every directory a user creates 0775, writable by the user's own group,
and the socket-path check refused a backend socket there ("writable by group …, set socket_group").

A group-writable directory is now accepted when its group is the user-private group of the socket's owner
or of the daemon's user: named as the user, the user's primary group, with no other member and no other
account's primary group (read from `/etc/passwd` and `/etc/group`; an account from LDAP or another NSS
source does not qualify). Any other group-writable directory is still refused unless `socket_group` names
its group. Known limitation 26 says what this trusts.

## 4. Behavior changes to know about

| Where | 0.2.0 | 0.2.1 |
|---|---|---|
| A task that hears nothing from its peer | stays `submitted` | `failed`, `no_response`, effect `UNVERIFIED` after `no_response_after` (15 min) |
| A long capability call (provider side) | the requester sees `submitted` until the result | the provider sends `status{working}` when it starts |
| MCP tools that return one task | the whole task | held to about 24 KB, with a notice of what was cut |
| A failed forward to an A2A backend | left in the inbox until a restart | retried with backoff for up to 10 min |
| Evidence | — | new types `anet.task.no_response`, `anet.backend.failed` |
| A socket directory group-writable by a user-private group | refused | accepted |
| `config.json` written by `anet init` | — | carries `no_response_after` |
| For modules: `module.InboundTaskHost` | `InboundTasks`, `ReplyTask` | also `InboundTask` |

## 5. Known limitations

Item 27 is new: a task that hears nothing for `no_response_after` fails even when the other side is only
slow (a person who has not answered yet, a long call on a 0.2.0 provider, an agent offline for longer),
and a question that arrives after that is not kept. Item 26 now covers user-private groups. The full list:
[KNOWN-LIMITATIONS.md](KNOWN-LIMITATIONS.md).

## 6. Versions and artifacts

| Component | Version |
|---|---|
| anet (daemon and CLI) | 0.2.1 |
| ANetHub | 0.2.1 (wire 2; still accepts anet ≥ 0.2.0) |
| ANetCore | v0.15.0 (unchanged) |

Release artifacts: darwin-arm64, darwin-amd64, linux-amd64, linux-arm64, two variants per platform (the
default build has no shell module; `anet-shell-*` has it). The sha256 and module set of every artifact
are in the signed `release.json`. `anet update` moves a 0.2.0 node to 0.2.1.
