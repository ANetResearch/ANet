# Issue drafts for a2a-js (`@a2a-js/sdk`)

> **NOT SUBMITTED — on hold per product owner (more testing first).** Each item needs the product
> owner's approval.
>
> **License: Apache-2.0**, a2a-js's own license: the text and the code in these drafts are licensed under
> the Apache License 2.0 alone (ANet `LICENSE`, condition 3).

Target: `@a2a-js/sdk` **1.2.1** (npm; still latest on 2026-09-29), Node.js v24.21.0. Found while
driving anet's local A2A interface with the SDK's own client (`docs/notes/0035` item 2). Standalone
reproductions for J2 and J3, using @a2a-js/sdk from registry.npmjs.org and nothing from anet, are in
`submissions/repro/a2a-js/` (`j2-resolver-base-path.mjs`, `j3-blocking-call-300s.mjs`); every
"Observed" below was re-run there on 2026-09-29.

**Upstream check, 2026-09-29.** J1 is tracked upstream as a2a-js #663 (public; fix PR #664 open, not
merged). J2 and J3 have no matching issue found.

| # | Title | Area | Filing |
|---|---|---|---|
| J1 | Card canonicalization depends on the input form | — | tracked upstream as a2a-js #663; nothing to file here |
| J2 | The card resolver resolves the well-known path as a URL reference: a base URL with a path loses its last segment | `client/card-resolver` | public issue, or a spec discussion first |
| J3 | A blocking `SendMessage` longer than 300 s fails with Node's default `fetch` (`UND_ERR_HEADERS_TIMEOUT`) | client transports, docs | documentation issue |

---

## J1

Tracked upstream as a2a-js #663 (public; fix PR #664 open, not merged). Nothing further is published
here; follow #663 and #664 upstream.

---

## J2. The card resolver resolves the well-known path as a URL reference

**Code.** `DefaultAgentCardResolver.resolve(baseUrl, path)` builds `new URL(path ?? '.well-known/agent-card.json', baseUrl)`.
By URL reference resolution, a base URL whose path does not end in `/` loses its last segment:
`http://127.0.0.1:43811/a2a/v1/agents/<aid>` resolves to `http://127.0.0.1:43811/a2a/v1/agents/.well-known/agent-card.json`.

**Observed.** `resolve('http://…/a2a/v1/agents/<aid>')` and `ClientFactory.createFromUrl` with that URL: `404`.
With a trailing slash (`…/<aid>/`) both work. Other clients given the same base URL: a2a-python
(`A2ACardResolver`) appends the path to `base_url.rstrip('/')`; Hermes' A2A client does the same; a2a-go's
resolver does not append the well-known path to a URL that has a path.

**Impact.** Agents served under a path prefix (several agents behind one host, a gateway, anet's local
interface) need a different base URL for each SDK.

**Spec reading.** A2A §8.2 (`docs/specification.md:1988`) names `https://{server_domain}/.well-known/agent-card.json`; how a base URL with
a path combines with the well-known path is not said. Worth raising with the spec before changing any SDK;
appending (as a2a-python does) is the least surprising for the `createFromUrl(baseUrl)` API.

---

## J3. A blocking `SendMessage` longer than 300 s fails with Node's default `fetch`

**Observed.** `client.sendMessage(…)` without `returnImmediately`, against a task that takes 420 s: after
301 s, `TypeError: fetch failed` (cause `UND_ERR_HEADERS_TIMEOUT`). Node's `fetch` (undici) waits at most
300 s for response headers (`headersTimeout`) and 300 s between body chunks (`bodyTimeout`); a blocking
`SendMessage` sends nothing until the task is terminal or interrupted. `sendMessageStream` on the same kind
of task stays open (the server's SSE keep-alives count as body data) and delivers the answer; a2a-python's
blocking call waits as long as its configured timeout.

**Impact.** A2A §3.2.2 (`docs/specification.md:446`) says a blocking `SendMessage` MUST wait for a terminal or interrupted state, with no
upper bound; with the SDK's defaults a Node client cannot wait more than five minutes, and the error does
not say that the task goes on.

**Suggested fix.** Document it in the client section and show how to pass a `fetchImpl` with an undici
`Agent({headersTimeout: 0, bodyTimeout: 0})`, or recommend streaming / `returnImmediately` + `getTask`
for long tasks.
