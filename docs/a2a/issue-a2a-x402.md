# Issue drafts for a2a-x402

> **DRAFT — not submitted; requires product owner approval before any external submission.**
>
> **License: Apache-2.0**, the license of a2a-x402: these drafts are licensed under the Apache
> License 2.0 alone (ANet `LICENSE`, Section 3).

Target: `github.com/google-agentic-commerce/a2a-x402`, specification **v0.2**
(`spec/v0.2/spec.md`). Section numbers below refer to that file. None of these drafts is a security
vulnerability report; X5 and X10 are design-level safety and privacy points suitable for public
discussion.

These come from implementing the extension in anet (an A2A 1.0 implementation with an end-to-end
encrypted relay binding and a custodial credit scheme, `x402-scheme-anet-credit.md`).

| # | Title | Kind |
|---|---|---|
| X1 | Activation header: `X-A2A-Extensions` vs A2A 1.0 `A2A-Extensions` | spec correction |
| X2 | Which x402 version do the metadata objects use? | spec clarification |
| X3 | Examples use A2A 0.3 JSON | spec update |
| X4 | State machine: missing transitions and no "outcome unknown" | spec correction |
| X5 | Nothing prevents paying twice for one task | spec addition |
| X6 | No message for "sign this selected option" to a signing service reached over A2A | proposal |
| X7 | Error codes: gaps, and no mapping from facilitator reasons | spec addition |
| X8 | When must `x402.payment.receipts` be present? | spec clarification |
| X9 | Transport security wording excludes end-to-end encrypted bindings | spec wording |
| X10 | Guidance on task data sent to facilitators | spec addition |
| X11 | `required: true` for agents with free and paid skills | spec guidance |
| X12 | Network identifiers for non-chain rails | clarification |

---

## X1. Activation header: §8 prescribes `X-A2A-Extensions`; A2A 1.0 uses `A2A-Extensions`

**Text.** §8: "Clients MUST request activation of this extension by including its URI in the
`X-A2A-Extensions` HTTP header."

**Problem.** A2A 1.0 (§3.2.6) defines the service parameter `A2A-Extensions`, carried as an HTTP
header by the HTTP bindings, as gRPC metadata by gRPC, and by a binding-defined mechanism for custom
bindings. The `X-` spelling is the A2A 0.3 header. Against an A2A 1.0 server that does not map the
legacy name, a client following §8 literally gets no activation. This is the case for a2a-go v2
servers: the v1 handlers ignore `X-A2A-Extensions`, and because a2a-x402 recommends
`required: true`, the request then fails with `ExtensionSupportRequiredError` even though the client
asked for the extension (see a2a-go issue draft A7). The MUST also cannot be met at all over gRPC or
a non-HTTP binding.

**Suggested text.** "Clients MUST request activation of this extension using the extension
activation mechanism of the A2A protocol version and binding in use: the `A2A-Extensions` service
parameter for A2A 1.0 and later (for HTTP-based bindings, the `A2A-Extensions` header), or the
`X-A2A-Extensions` header for A2A 0.3. Agents SHOULD accept both header names during the transition."

---

## X2. Which x402 version do `x402.payment.required` and `x402.payment.payload` use?

**Problem.** §6 defers the data structures to "the core x402 Protocol Specification" without naming
a version. The examples are x402 v1 (`"x402Version": 1`, `maxAmountRequired`, `resource` inside each
requirement). x402 v2 renamed and moved fields (`amount`; `PaymentRequired.resource` as an object;
`PaymentPayload.accepted` echoing the chosen requirement; CAIP-2 network ids). Both kinds of objects
now appear under the same metadata keys, and nothing tells a client which one a merchant expects or a
merchant which one it will receive.

**Suggested text.**
- The objects carry their own `x402Version`; a client MUST answer with a payload of the version the
  merchant offered in `x402.payment.required`.
- Add a v2 example next to the v1 one.
- Optionally, let the AgentCard declaration state the versions a merchant supports, e.g.
  `"params": {"x402Versions": [2]}`.

anet uses v2 objects throughout.

---

## X3. Examples use A2A 0.3 JSON

**Problem.** All examples use the A2A 0.3 wire form: `"kind": "task"`, `"kind": "message"`, method
`message/send`, lowercase states (`"input-required"`), `"role": "agent"`, parts with `"kind": "text"`.
A2A 1.0 uses ProtoJSON (ADR-001): method `SendMessage`, `"state": "TASK_STATE_INPUT_REQUIRED"`,
`"role": "ROLE_AGENT"`, parts without `kind`. Implementers of A2A 1.0 have to translate every example
and may wonder whether the extension applies to 1.0 at all.

**Suggested change.** State which A2A versions the extension applies to (the metadata keys work
unchanged in both), and give the examples in 1.0 form (keeping 0.3 examples in an appendix if
needed).

---

## X4. State machine: missing transitions and no "outcome unknown"

**Text.** §7.1 allows `REQUIRED → REJECTED`, `REQUIRED → SUBMITTED → VERIFIED → COMPLETED|FAILED`.

**Problems.**
1. `SUBMITTED → FAILED` is missing, but it is the common case: the signature is invalid, the amount is
   wrong, the funds are insufficient. §9.2's own example (an expired authorization) fails before any
   `payment-verified`.
2. `REQUIRED → FAILED` is missing: a merchant whose quote expired before any payment arrived has no
   state to report.
3. `FAILED → REQUIRED` is missing, although §9 says a merchant may "request a payment requirement with
   input-required again".
4. There is no state for "the settlement outcome is not known" (facilitator timeout, transport error,
   an asynchronous rail). Without it an implementation must choose between reporting `payment-failed`
   (and inviting the client to pay again although the first payment may have gone through) and
   reporting nothing.

**Suggested change.** Add the three transitions, and a rule: "A merchant MUST NOT report
`payment-failed` while the outcome of a settlement is unknown. It keeps the task in its current state
and retries settlement with the same payment payload until the facilitator returns a final result;
facilitators are expected to answer a repeated settlement of the same payload with its original
result." (anet's facilitator does this; see `x402-scheme-anet-credit.md`, `settlement_pending`.)

---

## X5. Nothing prevents paying twice for one task

**Problem.** §5.1 notes that the merchant uses the `taskId` to retrieve the requirements it offered,
but neither the payload nor any rule ties a payment to one task, or limits a task to one payment.
The following sequence is allowed by the text and costs the client twice:

1. The client submits payload P1. The merchant settles it; the response is lost (timeout).
2. The client, seeing no result, signs P2 for the same task and submits it.
3. The merchant settles P2 as well (P2 has a fresh nonce, so nonce tracking does not catch it).

**Suggested text.**
- Merchant: "MUST NOT settle more than one payment for a task unless it requested an additional
  payment with a new `payment-required`; after a payment is submitted it MUST refuse further payments
  on the task until the first has a final outcome."
- Client: "MUST NOT sign a new payment payload for a task while a payload it submitted for that task
  has no final outcome."
- Optionally, a scheme-level binding: requirements carry an opaque task binding (e.g. in `extra`)
  that the scheme includes in what the payer signs, and the facilitator enforces "at most one
  successful settlement per (payer, binding)". The binding should be a hash of the task id and a
  nonce rather than the task id itself, so the facilitator does not learn task identifiers. anet's
  `anet-credit` scheme does this (`interaction_id`, `duplicate_binding`).

---

## X6. No message for "sign this selected option" to a signing service reached over A2A

**Text.** §5.1: the client agent "selects a preferred PaymentRequirements option and has it signed by
a designated signing service or wallet."

**Problem.** The spec defines no message for that request when the signing service is itself reached
over A2A. This is the natural shape when an A2A client (a coding tool, an assistant) does not hold
keys and talks to a local agent that does: the local agent is the signing service and also the proxy
towards the merchant. anet's local node works this way (A2A-DESIGN §8.7): the client sends
`x402.payment.status: payment-submitted` **without** `x402.payment.payload`, plus the selected
requirement copied verbatim from `accepts`; the node checks it against the stored offer, applies the
user's spending limits, signs, and forwards a standard payload to the merchant. Today this has to be
signalled with a vendor key (`anet.payment.accept`).

**Proposal (for discussion).**
- A metadata key for the selected requirement, e.g. `x402.payment.selected`, allowed in a
  `payment-submitted` message that has no `x402.payment.payload`.
- A way for an agent to declare that it acts as the signing service for its client, e.g. extension
  params `{"signer": true, "clientPayload": false}` on its card; a client MUST NOT send an unsigned
  selection to an agent that does not declare this.
- The signing service MUST verify that the selection is byte-for-byte one of the offered options.

---

## X7. Error codes: gaps, and no mapping from facilitator reasons

**Problems.**
1. §9.1 lacks codes for failures that every merchant must check: payment to the wrong payee, a scheme
   or network that was not offered, an unreadable payload, no account for the payer at the
   facilitator, and a second payment for an already paid task (X5). Implementations fall back to
   `SETTLEMENT_FAILED` and lose the reason.
2. Facilitators return x402 `invalidReason` / `errorReason` strings; the spec does not say how they
   relate to `x402.payment.error`. §9.2's example puts prose into the receipt's `errorReason`
   ("Payment authorization was submitted after its 'validBefore' timestamp."), while facilitators use
   machine-readable constants there.

**Suggested change.**
- Add `PAYEE_MISMATCH`, `UNSUPPORTED_SCHEME` (or `OPTION_NOT_OFFERED`), `INVALID_PAYLOAD`, and a code
  for the per-task duplicate (or allow `DUPLICATE_NONCE` to cover it explicitly).
- State that `x402.payment.receipts[].errorReason` carries the facilitator's reason unchanged, and
  that a merchant maps it to `x402.payment.error`, using `SETTLEMENT_FAILED` for reasons with no code.
- Keep human-readable text in the status message parts.

anet's mapping table is in `x402-scheme-anet-credit.md`.

---

## X8. When must `x402.payment.receipts` be present?

**Text.** §7: receipts are "a persistent array containing the complete history of all
`x402SettleResponse` objects for the task. This key MUST be present in the final task message."

**Questions.**
- For a task that ends without any payment attempt (rejected, canceled before paying), is an empty
  array required?
- For a task whose payment settled but whose work then failed or was canceled, is the key required in
  the `failed`/`canceled` message? (It should be: it is the client's evidence that it paid.)

**Suggested text.** "Once any settlement has been attempted for a task, every later status message in
a terminal state (`completed`, `failed`, `canceled`, `rejected`) MUST carry `x402.payment.receipts`
with the full history. When no settlement was attempted, the key MAY be omitted."

---

## X9. Transport security wording excludes end-to-end encrypted bindings

**Text.** §10: "All A2A communication MUST use a secure transport layer like HTTPS/TLS."

**Problem.** A2A allows custom protocol bindings (§12 of the A2A spec). A binding that encrypts end
to end and relays through an intermediary (anet's relay binding) protects payment objects better than
TLS through a TLS-terminating intermediary, but does not literally "use HTTPS/TLS" between the agents.
Conversely, HTTPS through a TLS-terminating gateway meets the wording while exposing every payment
payload to the gateway.

**Suggested text.** "All A2A communication carrying x402 objects MUST use a transport that provides
confidentiality and integrity between the two agents and authenticates the peer, such as HTTPS
directly between the agents or an end-to-end encrypted A2A binding. Intermediaries that terminate
TLS can read payment payloads."

---

## X10. Guidance on task data sent to facilitators

**Problem.** A merchant forwards `PaymentRequirements` to its facilitator. In x402, `resource`,
`description` and `extra` routinely describe what is being bought. For an agent task that description
is often derived from the task itself ("summarize contract X for client Y"). The facilitator is
usually a third party. The spec does not mention this.

**Suggested text.** "A merchant SHOULD NOT include task content in the requirements it sends to a
facilitator. `resource`, `description` and `extra` SHOULD be empty, fixed, or opaque identifiers,
unless the scheme requires otherwise." anet's design keeps these fields empty in the requirements
sent to its hub facilitator (A2A-DESIGN X4).

---

## X11. `required: true` for agents with free and paid skills

**Text.** §3.1: "Setting `required: true` is recommended."

**Problem.** A2A's `required` is per agent, and a server rejects any request that did not activate a
required extension. An agent with some free skills (or an agent operator offering free public tools
next to paid ones) that follows the recommendation forces every client, including those calling only
free skills, to implement x402.

**Suggested text.** "Agents SHOULD set `required: true` when every skill is paid. Agents that also offer
free skills SHOULD set `required: false` and answer requests to paid skills without activation with
`input-required` and `payment-required` as usual, or with `ExtensionSupportRequiredError`." Optionally,
params listing the priced skills. anet's design declares `required: true` only when all public
skills are priced (A2A-DESIGN §8.1).

---

## X12. Network identifiers for non-chain rails

**Problem.** The examples use `"network": "base"` (x402 v1 names); x402 v2 uses CAIP-2. Rails that are
not blockchains (custodial ledgers, bank rails) need identifiers too. CAIP-2 limits the reference part
to 32 characters, which does not fit identifiers such as anet's `hub:<AID>` (a 59-character
content identifier of the ledger operator's key history). Truncating would lose the property that the
network names the key that signs receipts.

**Question.** Should a2a-x402 (or x402) allow scheme-defined network identifiers for non-chain
schemes, provided the scheme document specifies them, or should such rails register a CAIP namespace
and hash the identifier to 32 characters?
