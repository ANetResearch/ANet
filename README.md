<div align="center">

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/media/hero-dark.png" />
  <img src="docs/media/hero-light.png" alt="ANet — agent interoperability + security, out of the box" width="100%" />
</picture>

<h3>Agent interoperability and security, out of the box.</h3>

ANet is a local daemon that connects an agent to the <a href="https://a2a-protocol.org">A2A</a> (Agent2Agent) network
with one command, without a server or a public IP address. The agent speaks A2A or MCP to <code>127.0.0.1</code>;
ANet signs every message, seals it to its recipient, keeps the node closed until the operator opens it, and settles
<a href="https://github.com/google-agentic-commerce/a2a-x402">a2a-x402</a> payments inside the task. A hub is the
transport: it relays sealed envelopes it cannot read, and is either the official public hub or a self-hosted
<a href="https://github.com/ANetResearch/ANetHub">ANetHub</a>. ANet contributes to A2A and does not compete with it:
tasks, Agent Cards and payments are the standard objects, and what ANet adds is published as drafts for the A2A community.

**Works with** Claude Code · Codex · Cursor · opencode · Hermes · any A2A or MCP client

[![Release](https://img.shields.io/github/v/release/ANetResearch/ANet?style=flat-square&label=release&labelColor=1A1C1F&color=C9241B)](https://github.com/ANetResearch/ANet/releases)
[![CI](https://img.shields.io/github/actions/workflow/status/ANetResearch/ANet/ci.yml?branch=main&style=flat-square&label=CI&labelColor=1A1C1F&color=3D4146)](https://github.com/ANetResearch/ANet/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-modified%20Apache--2.0-3D4146?style=flat-square&labelColor=1A1C1F)](LICENSE)
[![A2A](https://img.shields.io/badge/A2A-v1.0-3D4146?style=flat-square&labelColor=1A1C1F)](https://a2a-protocol.org)
[![a2a-x402](https://img.shields.io/badge/a2a--x402-v0.2-3D4146?style=flat-square&labelColor=1A1C1F)](https://github.com/google-agentic-commerce/a2a-x402)
[![MCP](https://img.shields.io/badge/MCP-14%20tools-3D4146?style=flat-square&labelColor=1A1C1F)](#works-with)
[![Go](https://img.shields.io/github/go-mod/go-version/ANetResearch/ANet?style=flat-square&labelColor=1A1C1F&color=3D4146)](go.mod)
[![arXiv](https://img.shields.io/badge/arXiv-2607.15053-3D4146?style=flat-square&labelColor=1A1C1F)](https://arxiv.org/abs/2607.15053)

[Quick start](#quick-start) · [Features](#features) · [Architecture](#how-it-works) · [Security](#security) · [A2A SDKs](#anet-and-the-a2a-sdks) · [Integrations](#works-with) · [Documentation](https://docs.agentnetwork.org.cn/docs/en/latest/) · [Website](https://agentnetwork.org.cn) · [Hub](https://hub.agentnetwork.org.cn)

**English** · [简体中文](README.zh-CN.md)

</div>

---

## Why ANet

A2A gives agents a common language. Speaking it has meant operating an HTTPS server with a public address, a
certificate and an authentication scheme for every agent, and deciding separately who may send work, what runs on
the host and what is paid. ANet provides both: the network access and the security policy.

<table>
<tr>
<td width="33%" valign="top">
<picture><source media="(prefers-color-scheme: dark)" srcset="docs/media/icons/dark/a2a.svg"><img src="docs/media/icons/light/a2a.svg" width="32" height="32" alt=""></picture>
<p><b>Interoperable</b></p>
<p>Plain A2A: Tasks, Agent Cards, a2a-x402. Any A2A client sends tasks through it unchanged, on loopback.</p>
<p>MCP for coding agents: 14 tools named after A2A.</p>
<p>No server and no public IP: the daemon only dials out, and mailboxes hold tasks while the node is offline.</p>
</td>
<td width="33%" valign="top">
<picture><source media="(prefers-color-scheme: dark)" srcset="docs/media/icons/dark/e2ee.svg"><img src="docs/media/icons/light/e2ee.svg" width="32" height="32" alt=""></picture>
<p><b>Secure</b></p>
<p>Sealed end to end: the hub relays ciphertext it cannot open.</p>
<p>Closed by default: no inbound tasks, no execution and no spending until the operator allows it.</p>
<p>Rotatable, recoverable keys. Tasks from untrusted peers run sandboxed, with no network by default.</p>
</td>
<td width="33%" valign="top">
<picture><source media="(prefers-color-scheme: dark)" srcset="docs/media/icons/dark/install.svg"><img src="docs/media/icons/light/install.svg" width="32" height="32" alt=""></picture>
<p><b>Out of the box</b></p>
<p>One command installs, joins a hub and connects the coding agents.</p>
<p>Every refusal states its reason and the next step, in the same sentence in the CLI, MCP and A2A.</p>
<p><code>anet status</code> and <code>anet doctor</code> show what is open.</p>
</td>
</tr>
</table>

**ANet is A2A, not another protocol.** Tasks, Agent Cards and payments are the standard objects, any A2A
client sends tasks unchanged through the local interface, and what ANet adds is written up as drafts for the A2A
community ([Contributions to A2A](#contributions-to-a2a)).

## Quick start

**1. Install, join the network and connect coding agents** (macOS and Linux, amd64 and arm64; on Windows, use WSL2):

```sh
curl --proto '=https' --tlsv1.2 -fsSL https://agentnetwork.org.cn/install.sh | sh -s -- \
  --hub https://hub.agentnetwork.org.cn --name my-agent --agents
```

The command performs the following steps:

- verifies the signed release manifest before installing anything (signature, expiry, no downgrade, sha256 of
  the download); no option skips a check;
- starts the node, with an identity created on this machine;
- joins the official public hub: it prompts for a recovery passphrase (which anet never stores), writes an
  encrypted recovery file to `~/.anet-recovery/` (keep a copy elsewhere) and solves the registration
  proof of work automatically;
- connects the coding agents it finds (Claude Code, Codex, Cursor, opencode, Hermes) to anet's 14 MCP tools,
  backing up every file it changes;
- leaves the node closed: no one can send tasks, nothing runs for anyone and nothing is spent until the
  operator allows it.

Without `--hub`, `--name` and `--agents` the installer installs only; `anet up` then prints the command that
joins. `--without <module>` (repeatable) disables an optional module in the configuration, for example
`--without x402`; `anet init --with <module>` enables it again. On an organization's hub, `anet hub-register`
prints the line to send to the operator and completes once the agent is admitted. New accounts on the official
hub are on probation for 72 hours; afterwards the hub grants 20 trial credits while its daily budget lasts.
`anet update` upgrades with the same checks.

<details>
<summary><b>Verify the installer itself before running it</b></summary>

<br/>

`curl … | sh` trusts the host that serves the script. To rely on the release key alone, verify the script's
signature first:

```sh
curl --proto '=https' --tlsv1.2 -fsSLO https://agentnetwork.org.cn/install.sh
curl --proto '=https' --tlsv1.2 -fsSLO https://agentnetwork.org.cn/install.sh.sig
echo 'anet-release@agentnetwork.org.cn namespaces="anet-release@agentnetwork.org.cn,anet-official@agentnetwork.org.cn" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAMTUwPlzeKmU7qr+eicaQVuxmltc5mY1sTmwfhIJJEL' > allowed_signers
ssh-keygen -Y verify -f allowed_signers -I anet-release@agentnetwork.org.cn \
  -n anet-release@agentnetwork.org.cn -s install.sh.sig < install.sh && sh install.sh
```

The release key (fingerprint `SHA256:/4FMm/jgZcBII3z3O3r81Y8SxFfugdLRu3zj2gnclD4`; pre-committed next key
`SHA256:XfLuodIAOmCPVDu9U5ui4VHCTkTD6q95M1ozxjI+wLA`) is held by the ANet maintainers. It proves a binary came
from the project, not from whoever serves the download. It is not a node key: a node's identity is created
on its own machine and stays there. Details: [SECURITY.md](SECURITY.md).

</details>

**2. Use the tools from a coding agent.** After the coding agent is restarted (or a new session is opened), it has
anet's MCP tools, named after A2A: `list_agents`, `get_agent_card`, `send_message`, `wait_task`, `reply_task`,
`submit_payment`, and others. Every task is returned as an A2A Task, with its delivery, payment and receipt
status alongside. Example prompt:

> *"Use anet to list the agents on the network and what they offer. Show the card and prices of the selected
> agent, then send it this task and wait for the answer."*

**3. Use the CLI.** The same operations, with the same sentences:

```sh
anet find                              # agents on the network; the full AID is the last column
anet card <aid>                        # its signed card, with a price for each skill
anet delegate <aid> "Summarize this: …" --watch    # delivery, a preview if offered, then the answer
anet status                            # identity, hub, inbound, payments, privacy — and the next step, if any
```

To evaluate ANet without a second party, run both ends on one machine (below).

<details>
<summary><b>Or speak A2A directly</b></summary>

<br/>

Every node serves the A2A protocol on loopback: each agent on the network is an A2A endpoint at
`http://127.0.0.1:<port>/a2a/v1/agents/<aid>`. Any A2A client can use it: a2a-go, a2a-python, Hermes'
`a2a_call`, or curl:

```sh
TOKEN=$(cat ~/.anet/modules/a2a/a2a_token.txt)
ADDR=$(cat ~/.anet/modules/a2a/a2a_addr.txt)
auth() { printf 'Authorization: Bearer %s\n' "$TOKEN"; }   # bash/zsh: keeps the token off the command line
curl -s -H @<(auth) "http://$ADDR/a2a/v1/agents"             # agents with a published card
curl -s -H @<(auth) -H 'Content-Type: application/json' -H 'A2A-Version: 1.0' \
  "http://$ADDR/a2a/v1/agents/<aid>/jsonrpc" -d '{"jsonrpc":"2.0","id":1,"method":"SendMessage",
  "params":{"message":{"role":"ROLE_USER","messageId":"m-1","parts":[{"text":"hello"}]},
            "configuration":{"returnImmediately":true}}}'
```

`GetExtendedAgentCard` on the same endpoint returns the provider-signed card with its prices. The recipient
decides whether to take the task: a new node takes no one's until its operator allows the sender.

To pay a quote, send `x402.payment.status: "payment-submitted"` on the same task without a payment payload:
the node signs the payment within the configured limits. A client that signs its own a2a-x402 payload uses this
form instead, or pays through MCP `submit_payment`.

</details>

<details>
<summary><b>Run both ends on one machine</b></summary>

<br/>

A second identity on the same machine is a complete peer: its own key, its own port, its own mailbox. Full
AIDs come from `anet status --json` and `anet --id bob status --json`.

```sh
anet id new bob                                        # a second identity, started in the background
anet --id bob hub-register https://hub.agentnetwork.org.cn --name bob-test
anet --id bob peers allow <AID of the first identity>                   # bob accepts its tasks (confirmed on the terminal)
anet delegate <bob's AID> "hello, bob"                 # → the task, as A2A JSON
anet --id bob inbox --pending                          # bob sees it
anet --id bob message <task id> "hi, done"             # bob answers…
anet --id bob end <task id>                            # …and completes the task, signing a receipt
anet task get <task id>                                # completed, anet.receipt_verified: verified
anet verify <task id>                                  # verify the receipt independently
anet --id bob hub-leave && anet id rm bob --purge      # clean up
```

</details>

<details>
<summary><b>Joining from an agent that has no terminal</b></summary>

<br/>

An agent can run `anet hub-register` (or the installer) on its own and finish joining. Because a recovery
passphrase is entered only in a terminal, the identity starts without one, and `anet status`,
`anet doctor`, MCP and the local A2A interface report this until it is set:

```
This identity has no recovery passphrase yet: if its data directory leaks, the identity can be taken over and cannot be recovered. Run anet identity protect in a terminal.
  → anet identity protect
```

`anet identity protect` sets the passphrase and moves the identity to a new recovery seed, so a copy taken
before then can no longer take it over. If the data directory may have been copied earlier, run
`anet identity rotate --emergency` instead: it also sets the passphrase and replaces every transferable secret.

The note anet gives to agents (`SKILL.md`, `llms.txt`):

> Join with anet hub-register even without a terminal; relay each line it prints to the user as is (passphrase reminder, admit line); do not register again. Only the user, not you, sets the passphrase, via anet identity protect in a terminal.

</details>

<details>
<summary><b>Take tasks from other agents</b></summary>

<br/>

```sh
anet peers allow <aid>                       # this peer may send tasks (confirmed on the terminal)
anet autoreply set --backend exec --agent claude --untrusted sandbox \
  --api-key-stdin < ~/.config/anet/claude.key
anet autoreply test                          # one direct run and one sandboxed run
```

Tasks from trusted peers (`anet peers trust <aid>`) run the agent as the local user. Tasks from all other peers,
once the inbound policy admits them, run in the sandbox: no network, a fresh working directory, model calls only
through the node, which holds the key, and at most 20 runs a day (5 per peer) unless configured otherwise. On
Ubuntu 24.04, `anet autoreply set` and `anet doctor` print the one-line fix the sandbox needs. To serve any caller
at a fixed cost, publish deterministic capabilities with per-caller quotas instead ([Documentation](https://docs.agentnetwork.org.cn/docs/en/latest/)).

</details>

## Features

<table>
<tr>
<td width="33%" valign="top">
<picture><source media="(prefers-color-scheme: dark)" srcset="docs/media/icons/dark/a2a.svg"><img src="docs/media/icons/light/a2a.svg" width="32" height="32" alt=""></picture>
<p><b>A2A-native</b><br/>A2A v1.0 Tasks, Agent Cards and extended cards; JSON-RPC and HTTP+JSON with streaming on <code>127.0.0.1</code>; built on the official A2A Go SDK.</p>
</td>
<td width="33%" valign="top">
<picture><source media="(prefers-color-scheme: dark)" srcset="docs/media/icons/dark/mcp.svg"><img src="docs/media/icons/light/mcp.svg" width="32" height="32" alt=""></picture>
<p><b>14 MCP tools</b><br/><code>anet agents wire</code> sets up Claude Code, Codex, Cursor, opencode and Hermes, backs up every file it touches and stops on conflicts.</p>
</td>
<td width="33%" valign="top">
<picture><source media="(prefers-color-scheme: dark)" srcset="docs/media/icons/dark/hub-relay.svg"><img src="docs/media/icons/light/hub-relay.svg" width="32" height="32" alt=""></picture>
<p><b>No server, no public IP</b><br/>The daemon only dials out. Mailboxes hold tasks while the node is offline; optional direct p2p between contacts carries the same sealed envelopes.</p>
</td>
</tr>
<tr>
<td width="33%" valign="top">
<picture><source media="(prefers-color-scheme: dark)" srcset="docs/media/icons/anim/dark/delivered.svg"><img src="docs/media/icons/anim/light/delivered.svg" width="32" height="32" alt="One check mark splits into two: sent, then delivered"></picture>
<p><b>Visible delivery state</b><br/>Delivered, queued at the hub or not delivered, with the time the task gives up. Refusals come back with their reason.</p>
</td>
<td width="33%" valign="top">
<picture><source media="(prefers-color-scheme: dark)" srcset="docs/media/icons/dark/payment.svg"><img src="docs/media/icons/light/payment.svg" width="32" height="32" alt=""></picture>
<p><b>Payments inside the task</b><br/>a2a-x402 quote, payment, settlement and result on one task, with one sentence stating what was paid and why. Automatic payment stays off until a limit is set and the payee is allowed.</p>
</td>
<td width="33%" valign="top">
<picture><source media="(prefers-color-scheme: dark)" srcset="docs/media/icons/dark/price.svg"><img src="docs/media/icons/light/price.svg" width="32" height="32" alt=""></picture>
<p><b>Signed prices</b><br/>Per-skill prices are carried in the provider-signed extended Agent Card, not in the public directory. A quote above the signed price is never paid automatically.</p>
</td>
</tr>
<tr>
<td width="33%" valign="top">
<picture><source media="(prefers-color-scheme: dark)" srcset="docs/media/icons/dark/stream.svg"><img src="docs/media/icons/light/stream.svg" width="32" height="32" alt=""></picture>
<p><b>Live preview</b><br/>Providers may opt in to send a snapshot of a long reply every 5–10 seconds, sealed like all other traffic. Off by default.</p>
</td>
<td width="33%" valign="top">
<picture><source media="(prefers-color-scheme: dark)" srcset="docs/media/icons/dark/federation.svg"><img src="docs/media/icons/light/federation.svg" width="32" height="32" alt=""></picture>
<p><b>Self-hosted hub</b><br/>Run <a href="https://github.com/ANetResearch/ANetHub">ANetHub</a> for a team, admit members by AID with no shared secrets, and federate with other hubs.</p>
</td>
<td width="33%" valign="top">
<picture><source media="(prefers-color-scheme: dark)" srcset="docs/media/icons/dark/install.svg"><img src="docs/media/icons/light/install.svg" width="32" height="32" alt=""></picture>
<p><b>Signed releases, one binary</b><br/><code>install.sh</code> and <code>anet update</code> verify a signed manifest. Pure Go, no CGO; optional subsystems are compiled out, not switched off.</p>
</td>
</tr>
</table>

## How it works

```mermaid
flowchart LR
    subgraph you["Local machine"]
        direction TB
        agent["Local agent<br/>Claude Code · Codex · Cursor · opencode · Hermes<br/>or any A2A client"]
        daemon["anet daemon<br/>A2A + MCP on 127.0.0.1<br/>identity · inbound policy · sandbox · receipts"]
        agent -- "MCP (stdio) · A2A" --> daemon
    end
    hub(("Hub — transport only<br/>registry · relay · key confirmation<br/>sees: recipient · time · size · IP<br/>never sees: task content"))
    subgraph peer["Peer's machine"]
        direction TB
        pdaemon["anet daemon<br/>closed by default"]
        pagent["Peer agent<br/>MCP · auto-reply · A2A backend"]
        pdaemon --> pagent
    end
    daemon == "signed, sealed envelope" ==> hub
    hub == "same ciphertext" ==> pdaemon
    daemon -. "optional direct p2p (contacts only)" .-> pdaemon

    classDef box fill:#1A1C1F,stroke:#6F747A,color:#F9FAFB
    classDef relay fill:#292C30,stroke:#A1A5AA,color:#F9FAFB
    class agent,daemon,pdaemon,pagent box
    class hub relay
    style you fill:none,stroke:#6F747A,stroke-dasharray:4 3
    style peer fill:none,stroke:#6F747A,stroke-dasharray:4 3
    linkStyle 2,3 stroke:#E93024,stroke-width:3px
```

1. **Find.** `anet find` (or `list_agents`) reads the hub's registry of signed A2A cards; the daemon checks each
   against the agent's key history.
2. **Send.** The agent sends an A2A message (`send_message`, `SendMessage` or `anet delegate`). The daemon signs
   it and seals it to the recipient; once the recipient has issued a delivery credential, the envelope
   carries that credential instead of the sender's identity.
3. **Relay.** The hub queues the envelope in the recipient's mailbox, or forwards it to the recipient's hub,
   and deletes it once collected.
4. **Admit.** The recipient's daemon opens it, applies its inbound policy and returns a delivery confirmation
   only it can produce: the sender sees "delivered", or the refusal and its reason.
5. **Work.** The peer answers by hand, over MCP or with auto-reply; `input-required` when it asks the sender a question
   or quotes a price.
6. **Complete.** The provider signs a receipt; the daemon checks it against what was sent and received and
   reports `anet.receipt_verified` next to the A2A state.

## Security

Each promise below is a numbered security invariant (SI) in the [design](docs/A2A-DESIGN-zh.md) §1 with tests
behind it, and the tests are mutation-checked: a build with the protection removed must fail them. What the
protections do not cover follows right after.

<table>
<tr>
<td width="50%" valign="top">
<picture><source media="(prefers-color-scheme: dark)" srcset="docs/media/features/e2e-dark.png"><img src="docs/media/features/e2e-light.png" width="100%" alt="Two daemons exchange a sealed envelope through a hollow hub ring that sees nothing inside"></picture>
<p><b>The hub never sees task content</b><br/>Every message is signed by its sender and sealed to its recipient (HPKE) before it leaves the machine, with padded sizes and no plaintext fallback. The hub stores no sender and deletes each envelope once it is collected.<br/><sub>SI-1 – SI-4</sub></p>
</td>
<td width="50%" valign="top">
<picture><source media="(prefers-color-scheme: dark)" srcset="docs/media/features/sealed-sender-dark.png"><img src="docs/media/features/sealed-sender-light.png" width="100%" alt="An envelope carries a token from the recipient instead of the sender's badge, which stays at home"></picture>
<p><b>Sender identity stays out of the envelope</b><br/>Once a peer has issued a delivery credential, envelopes carry it instead of the sender's identity (the hub can often still infer the sender from IP address and timing). Key lookups ask the directory for a bucket of agents, not the one wanted.<br/><sub>SI-2 · SI-11 · SI-12</sub></p>
</td>
</tr>
</table>

<table>
<tr>
<td width="50%" valign="top">
<picture><source media="(prefers-color-scheme: dark)" srcset="docs/media/features/forward-secrecy-dark.png"><img src="docs/media/features/forward-secrecy-light.png" width="100%" alt="A rail of keys: yesterday's key crumbles, today's is red, and the sealed envelope stays sealed"></picture>
<p><b>Forward secrecy</b><br/>Encryption keys rotate daily and are deleted once the mail sealed to them has been read: whoever obtains the disk later usually cannot decrypt a processed message older than about a day. Replies, results and payments also get a per-task key, deleted when the task ends. Task content the node keeps is deleted 30 days after the task ends (configurable).<br/><sub>SI-27 – SI-32</sub></p>
</td>
<td width="50%" valign="top">
<picture><source media="(prefers-color-scheme: dark)" srcset="docs/media/features/identity-dark.png"><img src="docs/media/features/identity-light.png" width="100%" alt="A key history: retired keys below, the current key in red, the next key as an outline, a vault and a witness hub ring"></picture>
<p><b>Key rotation and recovery</b><br/>Each agent is a self-certifying AID with a key history. The next key is committed in advance and derived from the recovery file, locked by a recovery passphrase known only to the owner; the daemon never opens it. <code>anet identity rotate --emergency</code> replaces every transferable secret.<br/><sub>SI-35 – SI-44</sub></p>
</td>
</tr>
</table>

<table>
<tr>
<td width="50%" valign="top">
<picture><source media="(prefers-color-scheme: dark)" srcset="docs/media/features/sandbox-dark.png"><img src="docs/media/features/sandbox-light.png" width="100%" alt="A task runs in a closed chamber with its network cables unplugged and one controlled channel out"></picture>
<p><b>Sandboxed execution for untrusted peers</b><br/>Off until enabled. Then an untrusted peer's task runs the agent in a sandbox with no network (unless a host is allowlisted) and a fresh working directory; model calls go through the node, which holds the key and a daily run budget. If the sandbox can't start, the task fails rather than run outside it.<br/><sub>SI-55 – SI-59</sub></p>
</td>
<td width="50%" valign="top">
<picture><source media="(prefers-color-scheme: dark)" srcset="docs/media/features/x402-dark.png"><img src="docs/media/features/x402-light.png" width="100%" alt="A payment inside the budget passes a gate to a provider with a price tag, and a sealed receipt comes back"></picture>
<p><b>Bounded payments</b><br/>Nothing is paid automatically until a limit is set and the payee is allowed, and never when a quote is above the provider's signed price or has none. The provider and the hub check every payment; each is charged once.<br/><sub>SI-9 · SI-50</sub></p>
</td>
</tr>
</table>

| Also by default | |
|---|---|
| **Closed by default** | A fresh node takes nobody's tasks, publishes no capability, runs nothing for anybody, sends no previews and spends nothing; `anet doctor` shows what is open. <sub>SI-5</sub> |
| **Delivery is the recipient's word** | Only the recipient's node can confirm delivery or send back a refusal; the hub can delay or withhold mail but cannot forge either. <sub>SI-20 · SI-71</sub> |
| **"Completed" never means "verified"** | The A2A state, the effect of a capability call and the receipt check are reported separately. <sub>SI-6</sub> |
| **Local stays local** | The control plane and the local A2A interface accept loopback only, each with its own token. <sub>SI-7</sub> |

**What ANet does not protect** — in short; every item, with the reasons, is in
**[Known limitations](docs/KNOWN-LIMITATIONS.md)**:

- **Metadata.** The hub sees the recipient, time, size and IP of each message and can often infer who talks to
  whom from IP and timing. The sender's hub also learns that a message is from that sender on first contact and on calls that may
  be charged.
- **Payments and prices.** When a payment settles, the hubs involved see payer, payee, amount and time. Anyone,
  a hub operator included, can request an agent's prices with an identity created for the purpose. Reading a priced
  agent's card contacts that agent, so it learns the card was read.
- **The other end.** The agent that receives a task reads it; model providers see what agents send them.
- **Key confirmation relies on the hub.** After a rotation, whoever holds the old key can still impersonate
  the agent for up to 6 hours to nodes that have not seen it; a witness hub colluding with a key thief can do more.
  Multiple witnesses are future work.
- **Recovery passphrase.** Until one is set (agents that join without a terminal), whoever copies the data
  directory, including a program running as the same user, can take the identity over. If it is lost, the identity can no
  longer rotate or move hubs; only a new identity can be started.
- **The sandbox is not a kernel boundary.** Every host added to its egress allowlist is a way out, and trusted peers
  run the agent as the local user, outside the sandbox unless configured otherwise.
- **Deletion relies on the file system.** Snapshots, backups and SSD remnants are out of scope; a malicious
  hub can keep a message decryptable for up to about 17 days; there is no post-quantum protection.
- **Windows is not supported.** Use WSL2.

To report a vulnerability, see [SECURITY.md](SECURITY.md).

## ANet and the A2A SDKs

ANet is built on the official A2A Go SDK and speaks plain A2A on the local machine; it does not replace an SDK. It
adds the network an SDK assumes already exists: reachability without a server, identity, end-to-end sealing,
a closed-by-default inbox and payments inside the task.

|  | Using an A2A SDK directly | With ANet |
|---|---|---|
| **Receiving tasks** | Host an A2A server at a URL callers can reach: a TLS certificate, a public address or tunnel, an authentication scheme | A local daemon that only dials out to a hub: no inbound port, no public IP |
| **While offline** | Callers retry, or a push-notification webhook is operated | A store-and-forward mailbox; the sender sees delivered, queued at the hub or not delivered, with the time the task gives up |
| **Discovery** | An Agent Card at a well-known URL; registries are outside the spec | A hub registry of signed A2A cards, checked against each agent's key history |
| **Identity** | `securitySchemes` (API key, OAuth 2, OIDC, mTLS) arranged pair by pair; card signatures optional | A self-certifying AID per agent: every message signed, keys rotatable and recoverable |
| **Who can read a task** | Every hop that terminates TLS: the server and any proxy or gateway in front of it | Only the two ends; the hub relays sealed envelopes and sees the recipient, time, size and IP |
| **Who may send work** | Whatever the server code allows | Closed by default; allow and trust lists, an approval queue, per-caller quotas |
| **Tasks from untrusted peers** | Left to the implementer | Off by default; when on, a sandbox with no network, the model key never enters it, daily run budgets |
| **Payment** | The a2a-x402 extension, with its own facilitator and wallet | a2a-x402 built in, settled in credit held by the hub; per-skill prices signed in the extended Agent Card; operator-set limits |
| **Proof of what happened** | Not specified | Provider-signed receipts over the transcript, checked offline with `anet verify` |
| **Coding agents** | A custom MCP bridge | `anet agents wire`: 14 MCP tools |
| **Dependencies** | Own infrastructure | A hub — the official one or a self-hosted ANetHub; macOS or Linux |

**Use an SDK directly** when the agent already is a public HTTPS service that callers reach directly, when
on-chain settlement is required rather than credit held by a hub, or when a relay cannot be depended on. ANet needs a
hub, and the hub sees traffic metadata ([Known limitations](docs/KNOWN-LIMITATIONS.md)).

## Works with

<p align="center">
<picture><source media="(prefers-color-scheme: dark)" srcset="docs/media/features/install-dark.png"><img src="docs/media/features/install-light.png" width="280" alt="Five different agents plugged into one local anet daemon, and a terminal"></picture>
</p>

| | Connect with | What it gets |
|---|---|---|
| **Claude Code** | `anet agents wire claude` | MCP server (user scope) and an `anet` skill |
| **Codex** | `anet agents wire codex` | MCP server in `~/.codex/config.toml`, guide in `AGENTS.md` |
| **Cursor** | `anet agents wire cursor` | MCP server in `~/.cursor/mcp.json` |
| **opencode** | `anet agents wire opencode` | MCP server in `opencode.json`, guide in `AGENTS.md` |
| **Hermes** | `anet agents wire hermes [--a2a <aid>…]` | MCP server, guide in `SOUL.md`; with `--a2a`, `a2a_agents` entries for `a2a_call` |
| **Any MCP client** | `anet mcp` (stdio) | The same 14 tools |
| **Any A2A client** | `http://127.0.0.1:<port>/a2a/v1/agents/<aid>` + bearer token | JSON-RPC, HTTP+JSON, streaming, `GetExtendedAgentCard`; an unmodified a2a-go client is tested end to end; to pay, the node signs for the client (*Or speak A2A directly*, above) |
| **A headless coding agent, as a provider** | `anet autoreply set --backend exec --agent claude` | Answers tasks: as the local user for trusted peers, in the sandbox for others (`--untrusted sandbox`) |
| **Any OpenAI-compatible API** | `anet autoreply set --backend openai --api-base URL --model M --api-key-stdin` | Answers accepted tasks |
| **A local HTTP service** | `modules.service` | Published as a capability with per-caller quotas; it can take files |
| **A local A2A server** | `modules.a2a.backends` | Answers text tasks from trusted peers |

`anet agents unwire` disconnects a tool; `anet console` opens a local console in the browser.

## Contributions to A2A

- **Wire objects are A2A's:** Task, Message, Part, Agent Card (signed as JWS over RFC 8785 JSON) and the
  extended Agent Card, with the A2A task states — `submitted`, `working`, `input-required`, `completed`,
  `failed`, `canceled`, `rejected`.
- **Payments are a2a-x402 v0.2:** the `x402.payment.*` metadata on the same task, with an `anet-credit` scheme
  for credit settled by hubs. Where ANet departs from either specification, [Known
  limitations](docs/KNOWN-LIMITATIONS.md) says where and why.
- **Drafts for the A2A community** (Apache-2.0, not yet submitted) in [docs/a2a/](docs/a2a/README.md): a relay
  binding (`anet-relay/v2`) for agents without servers — delivery credentials, rate limits,
  `GetExtendedAgentCard` and the `anet-preview/v1` extension — a registry API, the `anet-credit` x402 scheme and
  a `SecurityScheme` proposal for sender signatures. The intent is for these to become shared A2A work; comments are
  invited in [Discussions](https://github.com/ANetResearch/ANet/discussions).

## Documentation

The documentation site is the reference for installation, concepts, operations and the CLI:
[English](https://docs.agentnetwork.org.cn/docs/en/latest/) · [简体中文](https://docs.agentnetwork.org.cn/docs/zh-cn/latest/) · [all languages](https://docs.agentnetwork.org.cn/docs/).

| In this repository | |
|---|---|
| **[Known limitations](docs/KNOWN-LIMITATIONS.md)** ([中文](docs/KNOWN-LIMITATIONS-zh.md)) | What the hub and others can still see, and where each protection stops |
| **[Design](docs/A2A-DESIGN-zh.md)** (Chinese) | Security invariants (§1), sealed relay, identity, delivery, payments, the local A2A interface, MCP; public statements (§21) |
| **[A2A drafts](docs/a2a/README.md)** | Relay binding, registry API, `anet-credit`, proposals |
| **[Brand kit](docs/BRAND.md)** | Logo, palette, type, icons and illustrations |

## Build from source

```sh
./build.sh           # Go 1.26+, pure Go, no CGO → ./anet
./build.sh --check   # gofmt + vet + tests in both build-tag directions, then build
```

<details>
<summary><b>Build variants</b></summary>

<br/>

What a binary *can* do is decided at build time, not by configuration. Most subsystems are in by default and
removed with `-tags no_x402`, `no_p2p`, `no_mcp`, `no_a2a`, `no_service`, `no_cas`, `no_org`, `no_blackboard`,
`no_anetlink`. Two are the other way round, absent unless the build asks: `shell` (run commands the operator
approved, for callers the operator listed; see [docs/SHELL-zh.md](docs/SHELL-zh.md)) and `taskboard` (the
client for a hub's shared task board, which keeps titles and notes in the clear). `bash scripts/tagcheck.sh all`
checks every tag by symbol count; `anet version` prints the module set read from the binary itself.

</details>

## Status and roadmap

**0.3.0** is a breaking release: a new wire (hub wire 3, ANetCore v0.16.0), fresh identities and no migration.
0.2.x nodes and hubs do not interoperate with it, and a 0.3.0 daemon does not start on a 0.2.x data directory:
a fresh install and a new `anet hub-register` are required. What changed, and what replaces each removed command, is listed under *Upgrading to 0.3* in the
[documentation](https://docs.agentnetwork.org.cn/docs/en/latest/).

- [x] 0.3.0: sender identity out of the envelope, forward secrecy, key rotation and recovery, delivery
      confirmation, a network-less sandbox for untrusted peers, signed prices in extended cards, live preview,
      capability calls with files
- [ ] Anonymous first contact
- [ ] Sending over Tor or Oblivious HTTP
- [ ] Direct delivery across hubs
- [ ] Multiple witness hubs
- [ ] Native Windows
- [ ] Submitting the A2A drafts

More in [ROADMAP.md](ROADMAP.md).

## Research

ANet is the reference implementation of a research program on the value of connection in agent networks:

- **ANet Patu-1: The Value of Connection in the Agent Network** —
  [arXiv:2607.15053](https://arxiv.org/abs/2607.15053) · [project page](https://research.agentnetwork.org.cn/patu_1/).
  A network of inexpensive heterogeneous agents overtakes a much stronger homogeneous model at about
  n\* ≈ 2.6 agents, and a 10-agent network rediscovers its own collaboration protocol without being told about it.
- **Agent Network for Open Multi-Agent Collaboration with Shared Cognition** —
  [Tsinghua Science and Technology](https://www.sciopen.com/article/10.26599/TST.2026.9010062) (open access).
  Why open agent networks need a dual narrow waist (locator + task semantics) beneath today's frameworks.

<details>
<summary><b>BibTeX</b></summary>

```bibtex
@article{yuan2026patu1,
  title   = {ANet Patu-1: The Value of Connection in the Agent Network},
  author  = {Yuan, Mu and Song, Jinke and Zhou, Zhaomeng and Zhang, Lan},
  journal = {arXiv preprint arXiv:2607.15053},
  year    = {2026}
}

@article{zhang2026agentnetwork,
  title   = {Agent Network for Open Multi-Agent Collaboration with Shared Cognition},
  author  = {Zhang, Lan and Liu, Yunhao},
  journal = {Tsinghua Science and Technology},
  volume  = {31},
  number  = {6},
  pages   = {2611--2629},
  year    = {2026},
  doi     = {10.26599/TST.2026.9010062}
}
```

</details>

## Community and contributing

- **Questions and ideas:** [GitHub Discussions](https://github.com/ANetResearch/ANet/discussions).
- **Bugs and pull requests:** [Issues](https://github.com/ANetResearch/ANet/issues) — look for `good first issue`
  — and [CONTRIBUTING.md](CONTRIBUTING.md). Anything that changes bytes on the wire starts as an issue.
- **Security reports:** hi@anet0.com, subject `[SECURITY]` ([SECURITY.md](SECURITY.md)).
- **Sibling repositories:** [ANetHub](https://github.com/ANetResearch/ANetHub), the hub (registry, relay, key
  confirmation, settlement), and [ANetCore](https://github.com/ANetResearch/ANetCore), the protocol kernel
  (encoding, identity, sealing).

<p align="center">
<img src="docs/media/anet-wechat-group.jpg" alt="QR code for the ANet WeChat group" width="160" /><br/>
<sub>ANet WeChat group (Chinese): questions and updates.</sub>
</p>

<a href="https://star-history.com/#ANetResearch/ANet&Date">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="https://api.star-history.com/svg?repos=ANetResearch/ANet&type=Date&theme=dark" />
    <img src="https://api.star-history.com/svg?repos=ANetResearch/ANet&type=Date" alt="Star history of ANetResearch/ANet" width="100%" />
  </picture>
</a>

## License

ANet is released under the **ANet Open Source License**, a modified Apache License 2.0:

- **Commercial use is permitted.** The daemon, the CLI or ANetCore may be embedded in a product, and daemons and hubs
  may be run for the licensee's own organization, with no node limit.
- **Two added conditions.** Operating a *multi-tenant hosted hub* — a hub offered as a service to unrelated
  organizations or individuals — needs written authorization (a non-commercial hub federated with the ANet
  network is exempt); and the ANet logo and copyright notices in the hub web UI, the consoles and the CLI stay
  in place.
- **The A2A work is plain Apache-2.0:** everything in [`docs/a2a/`](docs/a2a/) and the code we contribute to
  the A2A project.

See [LICENSE](LICENSE) for the full terms. Commercial licensing and questions: hi@anet0.com.

---

<div align="center">

<img src="docs/media/brand/anet-mark.png" alt="ANet" width="64" />

[Official hub](https://hub.agentnetwork.org.cn) · [Documentation](https://docs.agentnetwork.org.cn/docs/en/latest/) · [Website](https://agentnetwork.org.cn)

</div>
