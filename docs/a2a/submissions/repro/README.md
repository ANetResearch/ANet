# Minimal reproductions for the A2A issue drafts

> **NOT SUBMITTED — on hold per product owner (more testing first).**
> Nothing in this directory has been posted, attached or linked anywhere.
>
> **License: Apache-2.0** (ANet `LICENSE`, condition 3; `../../LICENSE`): the programs and scripts
> here may be pasted into upstream issues and pull requests as they are.

Each reproduction is a small, standalone program or script. It uses the upstream SDK it is about,
installed from the public package registry at the version named below, and nothing from anet: no
anet module, no anet binary, no anet server. Each prints what it observed next to what the
specification (or the SDK's own documentation) leads one to expect, and ends with a line
`reproduced: yes|no`.

Exit status, everywhere: `0` = the behaviour described in the draft is reproduced, `1` = it is not
(fixed upstream, or the environment differs), `2` = setup error. Re-run everything right before a
draft is submitted; drop a draft whose program exits `1`.

## Versions (as released on 2026-09-29, unchanged since 2026-09-28)

| SDK | Version | Where it comes from |
|---|---|---|
| a2a-go | `github.com/a2aproject/a2a-go/v2` **v2.6.0** (`ebf17c5`, 2026-09-25; still `@latest` and upstream `main`) | proxy.golang.org, checked against sum.golang.org (`a2a-go/go.sum`) |
| a2a-python | `a2a-sdk` **1.1.5** (2026-09-21; still latest) | PyPI (`a2a-python/requirements.txt`) |
| a2a-js | `@a2a-js/sdk` **1.2.1** (2026-09-24; still latest) | registry.npmjs.org (`a2a-js/package-lock.json`) |
| A2A specification | **v1.0.1** tag; `main` at `72b3761b` (2026-09-25) | line numbers below refer to `docs/specification.md` at `72b3761b` |
| a2a-x402 | spec **v0.2** (`spec/v0.2/spec.md`), `main` at `125db55` | |

Toolchains used for the recorded runs: Go 1.26.1, Python 3.12.3, Node.js 24.21.0 (Linux x86-64).

## How to run

```sh
# a2a-go: a module of its own; GOWORK=off keeps a surrounding go.work out of it.
cd a2a-go && GOWORK=off go run ./a9-unsigned-card      # one program
cd a2a-go && bash run-all.sh                            # all of them, one summary line each

# a2a-python (where python3-venv has no ensurepip: python3 -m venv --without-pip .venv
# && python3 -m pip --python .venv/bin/python install -r a2a-python/requirements.txt)
python3 -m venv .venv && .venv/bin/pip install -r a2a-python/requirements.txt
.venv/bin/python a2a-python/p1_stream_error_status.py

# a2a-js
cd a2a-js && npm ci && node j1-signature-skips-security-schemes.mjs

# Cross-SDK vectors for A1: sign with a2a-go, verify with the other two SDKs.
(cd a2a-go && GOWORK=off go run ./a1x-cross-sdk-vectors /tmp/a1x)
.venv/bin/python a2a-python/a1x_verify.py /tmp/a1x
(cd a2a-js && node a1x-verify.mjs /tmp/a1x)

# a2a-x402 X13: needs python3, node and go, no packages.
bash a2a-x402/x13-case-variant-members.sh
```

"Clean environment" was checked on 2026-09-29 by copying each directory out of the repository and
running it with an empty Go module cache (`GOMODCACHE`, `GOPROXY=https://proxy.golang.org`), a new
virtualenv with only `requirements.txt`, and `npm ci` against registry.npmjs.org: every program
below gave the recorded result.

## Index

| Program | Draft | Upstream | Filing (proposed) | Recorded result (2026-09-29) |
|---|---|---|---|---|
| `a2a-go/a1-default-values` | A1 | a2a-go | comment on a2a-go #445 | as-given bytes verify; the same card without `"required":false`, and the card after an `a2a.AgentCard` round trip, do not |
| `a2a-go/a1x-cross-sdk-vectors` + `a2a-python/a1x_verify.py` + `a2a-js/a1x-verify.mjs` | A1, A2 | a2a-go (#445), A2A #2122, a2a-tck #245 | comment | ten signed files, table under A1 in `../../issue-a2a-go.md`: a2a-go verifies every file except the re-served one; a2a-python and a2a-js agree with §8.4.1 rule 1 on eight and both drop two things rule 1 keeps (a REQUIRED `description: ""`, an empty string inside extension `params`). Informational scripts: exit 0 after printing |
| `a2a-go/a2-json-presence` | A2 | a2a-go | public issue (after #445 settles) | `null` for REQUIRED lists; unset `streaming`/`pushNotifications` written as `false`; explicit `extendedAgentCard:false` dropped |
| `a2a-go/a3-duplicate-members` | A3 | a2a-go | **security advisory** (`../advisory-a2a-go-A3.md`) | `Resolver` with `Verifier` accepts a card with a second `name`/`url` inserted before the signed one; invalid UTF-8 and lone surrogates swapped under one signature |
| `a2a-go/a4-base64-linebreaks` | A4 | a2a-go | public issue (hardening) | CR LF inside `signature` accepted |
| `a2a-go/a5-version-header` | A5 | a2a-go | public issue | `A2A-Version: 9.9`, `0.3`, absent, and `?A2A-Version=9.9` all answered with v1.0 semantics (`-32001`) |
| `a2a-go/a6-extensions-comma` | A6 | a2a-go | public issue + PR | `A2A-Extensions: a,b` in one field → `-32008 extension support required`; two fields → ok |
| `a2a-go/a7-legacy-extensions-header` | A7 (and a2a-x402 X1) | a2a-go | public issue | `X-A2A-Extensions: <a2a-x402 v0.2 URI>` → `-32008` from a server that requires the extension |
| `a2a-go/a8-extensions-response-header` | A8 | a2a-go | public issue + PR | activated in 4 of 4 calls, `A2A-Extensions` response header absent in all 4 (JSON-RPC and HTTP+JSON, unary and streaming) |
| `a2a-go/a9-unsigned-card` | A9 | a2a-go | **security advisory** (`../advisory-a2a-go-A9.md`) | modified card with `signatures` removed is accepted by `Resolver` with a `Verifier` |
| `a2a-go/a10-unknown-securityscheme` | A10 | a2a-go | public issue | whole card and `DefaultResolver.Resolve` fail on one unknown scheme |
| `a2a-go/a12-stream-errors` | A12 (with P1) | a2a-go, spec | on hold (spec discussion first) | server opens a 200 SSE stream to carry `TaskNotFound`; client reads a 200 JSON error as an empty stream and a 400 as "unexpected HTTP status" |
| `a2a-js/j1-signature-skips-security-schemes.mjs` | J1 | a2a-js (#663, fix PR #664) | **security advisory** (`../advisory-a2a-js-J1.md`) | typed form canonicalizes without `securitySchemes`; a card signed over the full payload fails `resolve()`+verify; a card signed from the typed form verifies after its OAuth `tokenUrl` was replaced |
| `a2a-js/j2-resolver-base-path.mjs` | J2 | a2a-js | public issue (or spec discussion) | `resolve(".../agents/alice")` fetches `/agents/.well-known/agent-card.json` (404) |
| `a2a-js/j3-blocking-call-300s.mjs` | J3 | a2a-js | documentation issue | server answers at 320 s; `sendMessage` fails at 301 s, `fetch failed (cause UND_ERR_HEADERS_TIMEOUT)` (run takes about 5 minutes) |
| `a2a-python/p1_stream_error_status.py` | P1 | a2a-python | public issue (with A12) | JSON-RPC 400 answer → `A2AClientError: HTTP Error 400`; the other three forms → `UnsupportedOperationError` |
| `a2a-python/p2_taskupdater_submit.py` | P2 | a2a-python | documentation issue | `TaskUpdater.submit()` first → `InvalidAgentResponseError: Agent should enqueue Task before TaskStatusUpdateEvent event`; `new_task(...)` first → completed |
| `a2a-x402/x13-case-variant-members.sh` | X13 | a2a-x402 spec | public issue (spec hardening) | Python and JavaScript read `amount 1000` to `0x1111…`; Go `encoding/json` reads `900000000` to `0x2222…` from the same object |

The a2a-x402 drafts X1–X12 are about the text of the specification: the quoted text is the
reproduction. X1 also has a running demonstration, `a2a-go/a7-legacy-extensions-header`, which uses
the a2a-x402 v0.2 extension URI with `required: true`, as a2a-x402 §3.1 recommends.
