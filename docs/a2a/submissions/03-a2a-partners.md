# Step 3 — A2A website: add ANet to the partners list

> **NOT SUBMITTED — on hold per product owner (more testing first).** Product-owner approval is also
> required for each item, one by one.
> The issue and the pull request below are two submissions; both need approval (docs/notes/0032 §3,
> docs/notes/0027 G10). Nothing here has been posted anywhere.

Plan and reasoning: `docs/notes/0032-计划-向A2A社区贡献.md` §3 step 3.

## Before pasting anything

Preconditions. Do not submit until every box is ticked:

- [ ] anet v0.2.0 is released on GitHub (release page URL filled in below).
- [ ] The v0.2 hubs are deployed. Until then the text must not say that task content is end-to-end
      encrypted on the public network (design §20, public statements table; docs/notes/0016). The
      sentence that depends on it is marked **[after v0.2 deploy]**.
- [ ] The hub registry answers publicly: `https://hub.agentnetwork.org.cn/a2a/v1/agents` returns
      signed cards (fill in the URL below; drop the line if the product owner prefers not to link it).
- [ ] The new licence is in the repositories. Wording follows it: say "source available" unless the
      product owner decides to call the licence open source.
- [ ] <https://agentnetwork.org.cn> loads from outside mainland China (maintainers removed partner
      links that stopped working, commit `a3bc1b60`, #2017). If it does not, link the GitHub
      repository instead.
- [ ] The product owner has chosen the display name and URL (default below: `ANet`,
      `https://agentnetwork.org.cn`).

Format, from the repository (a2aproject/A2A at `72b3761b`):

- `docs/partners.md` is a plain alphabetical list of `- [Name](url)` lines (lines 8–181); "ANet"
  sorts between "AmikoNet" (line 22) and "ArcBlock" (line 23).
- Recent additions were an issue plus a one-line PR that says `Closes #<issue>` (#2252/#2253,
  #2116/#2115, #2136/#2137). PR title type `docs:` (`CONTRIBUTING.md:78-84`); PR template asks for
  `Fixes #…` (`.github/PULL_REQUEST_TEMPLATE/PULL_REQUEST_TEMPLATE.md`).
- `docs/partners.md` is excluded from the link checker (`lychee.toml:81`) and the spell checker
  (`.github/actions/spelling/excludes.txt:88`), so the PR needs no allow-list change.
- "A2A Net" (`https://a2anet.com`) is already on the list (line 8) and is a different project. The PR
  body says so, to spare the reviewer the question.

---

## P1 — Issue

**Title**

```
Add ANet to the partners list
```

**Body**

````markdown
Request to add **ANet** to the A2A [partners list](https://github.com/a2aproject/A2A/blob/main/docs/partners.md).

ANet is a network for AI agents built on A2A 1.0:

- Every agent has a self-certifying identity (an Ed25519 key event log) and publishes a signed Agent
  Card (JWS over the RFC 8785 form, §8.4).
- Hubs keep a registry of verified cards: <registry URL>
- The local `anet` daemon exposes every agent on the network to any A2A client over JSON-RPC and
  HTTP+JSON, so an agent that cannot host an HTTPS endpoint still has an A2A endpoint.
- Tasks between agents are relayed through hubs over a custom protocol binding (§5.8);
  **[after v0.2 deploy]** the relayed messages are end-to-end encrypted between the two agents.
- Paid skills use the a2a-x402 extension inside the task.

- Source: https://github.com/ANetResearch/ANet (release: <v0.2.0 release URL>)
- Website: https://agentnetwork.org.cn

(Not to be confused with A2A Net, which is already listed.)
````

---

## P2 — Pull request

Change (the whole diff):

```diff
--- a/docs/partners.md
+++ b/docs/partners.md
@@ -22,5 +22,6 @@
 - [AmikoNet](https://amikonet.ai)
+- [ANet](https://agentnetwork.org.cn)
 - [ArcBlock](http://www.arcblock.io)
```

Commit message and PR title:

```
docs: add ANet to partners list
```

PR body:

````markdown
Closes #<P1 issue>.

Adds ANet to `docs/partners.md`, inserted alphabetically between AmikoNet and ArcBlock, following the
existing `- [Name](url)` format. One line, no other changes.

ANet is a network for AI agents built on A2A 1.0: signed Agent Cards with a hub-hosted registry, a
local daemon that gives every network agent an A2A endpoint (JSON-RPC and HTTP+JSON), a custom protocol
binding for relayed tasks, and a2a-x402 payments inside the task.

- Source: https://github.com/ANetResearch/ANet
- Website: https://agentnetwork.org.cn

Not to be confused with A2A Net (a2anet.com), which is already listed.
````

Local checks before pushing the branch (from `CONTRIBUTING.md:98-111`): `./scripts/format.sh`, and
optionally `./scripts/lint.sh` (Docker). Commit with the GitHub noreply address of the submitting
account.
