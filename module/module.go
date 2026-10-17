// Package module is the daemon's optional-subsystem seam.
//
// The org lesson, made structural. In v1 the organisation feature grew into
// thirty-five daemon files and could not be pulled back out; the open-source
// release of 1.3.0-v3 needed an axe and 683,852 deleted lines. What was
// missing was never discipline — it was a seam. Code with nowhere to attach
// attaches everywhere.
//
// So a subsystem that is not the kernel attaches here, and only here:
//
//	provider registry ─┐
//	identity / ledger ─┼─ kernel, never optional
//	hub client ────────┘
//	                    ├─ module ─ p2p
//	                    ├─ module ─ blackboard (shared-brain)
//	                    ├─ module ─ store (distributed storage)
//	                    ├─ module ─ org
//	                    └─ module ─ anetlink (device capabilities)
//
// Two rules give the seam teeth:
//
//   - A module receives a Host, not the *Daemon. It can register providers,
//     read identity, append evidence, and nothing else. A module that needs
//     more is telling you it belongs in the kernel or does not belong at all
//     — and the compiler is what says so, at the moment the reach is written
//     rather than two years later.
//   - Registration happens under a build tag, so "pluggable" is a property
//     the build proves rather than a claim in a design document. A
//     distribution that ships alongside a hub and needs no peer-to-peer
//     transport is `-tags no_p2p`, not a fork.
package module

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/payment"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/provider"
)

// Host is everything a module may use of the daemon.
//
// Deliberately small, and deliberately an interface rather than a struct
// pointer: it is the list of things a subsystem is allowed to want. Growing
// it is a decision someone has to make on purpose, in this file, which is
// exactly the moment that went unmarked in v1.
type Host interface {
	// AID is this node's identity anchor.
	AID() string
	// Providers is the C1 registry — the only way a module offers
	// capabilities to the network.
	Providers() *provider.Registry
	// RecordEvidence appends to this node's own chain. Modules produce
	// evidence like everything else; they do not get a private ledger.
	RecordEvidence(eventType string, payload any) error

	// ResolveKEL returns a peer's key history, if this node has verified
	// one. Added for the shared blackboard, which must be able to answer
	// "whose key signed this contribution" before merging it.
	//
	// Widening this interface is meant to be a deliberate act — it is the
	// list of things a subsystem is allowed to want, and the moment it
	// grows is the moment that went unmarked in v1. This one earns its
	// place: verifying authorship is not something a module can do with an
	// AID alone, and the alternative was a board that accepts anything.
	//
	// It answers only for peers this node has actually talked to. The hub
	// holds KELs but does not publish them, so a node vouches for what it
	// verified itself and for nothing else.
	ResolveKEL(aid string) ([]identity.SignedEvent, bool)

	// PaymentSeam hands over the ability to act as this node in a
	// payment. Absent when there is no hub, which is also when there is
	// no ledger and nothing can be charged.
	//
	// One method, and it grants more than the others do: what comes back
	// can sign as this node. That is not a detail to leave implicit, so
	// it is named — PaymentSeam, not Identity, not Keys — and it is worth
	// saying why the smaller grant does not work. A payment module could
	// be given the hub's identity read-only and would then be able to
	// verify what it is told and unable to authorise anything, because an
	// authorization the payer did not sign is not an authorization. The
	// signing is the payment.
	//
	// So the trade is deliberate: signing leaves the kernel, and in
	// exchange the facilitator client and the redemption path leave with
	// it. A build with -tags no_x402 has none of it, which it could not
	// claim while the code sat in the kernel being linked in regardless.
	PaymentSeam() (PaymentSeam, bool)

	// HubSeam hands over the ability to act as this node against its own
	// hub: one signature and the hub's address. Absent when there is no
	// hub.
	//
	// A second way to reach the node's key, which is the largest thing
	// this interface does, so the reason for a second one is worth
	// stating. PaymentSeam grants Sign, HubIdentity, HubURL and
	// ReadEvidence. A module that only needs to authenticate a request to
	// its own hub needs two of those, and handing it the payment seam
	// would give it the hub's key history and this node's evidence for no
	// reason. This seam is strictly smaller, not a duplicate: the choice
	// is between two grants of different sizes, and the smaller one is
	// what a hub client should get.
	//
	// It is still a signing grant. Anything holding it can authenticate
	// as this node to its hub — create, claim and complete work on the
	// node's behalf. That is what a hub client is for and it is not a
	// detail to leave implicit.
	HubSeam() (HubSeam, bool)

	// DeclareUntrustedBackend tells the kernel that this module will forward
	// work from peers that are not on the inbound trust list to a local
	// backend (an A2A backend configured with accept_untrusted).
	//
	// Added so the kernel's single configuration check (A2A-DESIGN §5.1)
	// can refuse policy=open together with such a backend without reading
	// the module's configuration: the kernel knows only that a module made
	// the declaration. A module calls it from Start, before it forwards
	// anything; the daemon refuses to start when the declaration conflicts
	// with the policy, and refuses a later policy write that would.
	DeclareUntrustedBackend()

	// StateDir is a directory this module may keep its own files in:
	// <data dir>/modules/<module>/, created 0700 when first asked for. The
	// directory is decided by the name this Host is bound to, never by an
	// argument: a module cannot ask for another module's. It returns ""
	// when the name table does not grant StateDir to the module, and for a
	// directory that cannot be made private to this user (a symbolic link,
	// another uid's); a module that needs one does not start without it.
	//
	// Added for the local A2A interface (A2A-DESIGN §11.1), which must
	// bind the same port after a restart — clients have its address
	// written into their configuration — and so has to remember it
	// somewhere. The alternatives were a module that guesses at the
	// daemon's data directory, which is the daemon's layout leaking into
	// every module that copies the guess, or a module with nowhere to put
	// state, which pushes that state into the kernel. A directory per
	// module keeps each one's files apart; it is not a boundary between
	// modules, which share a process.
	StateDir() string

	// Available reports whether another module is available here: compiled
	// into this binary and not turned off in `without`. Every Host answers
	// it, from the daemon's own Availability value (K-1).
	Available(name string) bool

	// TaskSeam hands over the tasks this node starts as a requester, in
	// their A2A form: sending a task to a remote agent, reading, listing,
	// cancelling and watching it, paying for it, and finding agents.
	// Absent when the kernel offers no such seam.
	//
	// Added for the local A2A interface (A2A-DESIGN §11.1), and it grants
	// a great deal: what comes back sends work to other nodes as this
	// node, under its key, and authorizes payments from its account. That
	// is exactly what a local A2A client asks for, so the grant is the
	// point rather than a leak — what keeps it narrower than the control
	// token is its shape. Every call names one remote agent and reaches
	// only this node's outbound tasks with that agent; tasks delegated TO
	// this node never pass through it (§11.2); and a payment goes through
	// the kernel's spending policy at the agent tier (§8.6), the one place
	// that policy is enforced, rather than through PaymentSeam, which
	// would let the module sign without asking it.
	TaskSeam() (TaskSeam, bool)
}

// ErrNotGranted is what a Host answers when the module it is bound to was
// not granted what it asked for (RecordEvidence of a type outside the
// module's EvidenceTypes, or of anet.policy.*, among them).
var ErrNotGranted = errors.New("module: this module is not granted that")

// ControlHandler is implemented by a module that answers control-plane
// routes the name table registers to it (K-3). The kernel owns the route
// table, the bearer authentication, the body limit and the error body; it
// hands the module only the requests for its own routes, and only while the
// module is available. A route is never a console session route: it is
// bearer-only.
//
// status is the HTTP status of the answer; resp is encoded as JSON unless
// it is already a []byte or json.RawMessage; an error that is an
// *a2ashape.Refusal is answered as the refusal's body (UX-R9).
type ControlHandler interface {
	HandleControl(ctx context.Context, route string, body []byte) (status int, resp any, err error)
}

// StatusContributor is implemented by a module that adds a member to
// /status (K-4). The kernel collects the members; it takes only the keys the
// name table lists for the module's name (StatusKeys), drops every other and
// reports the drop. SI5 contributes the module's values to the SI-5 map the
// same way.
type StatusContributor interface {
	StatusMember() (key string, v any)
	SI5() map[string]any
}

// LocalPaths is implemented by a module that owns endpoints on this
// machine: a Unix socket, a token file. The kernel gathers them into the
// sensitive-path list (SensitivePathsHost), the part of the sandbox's
// never-bind list that depends on which modules are available.
type LocalPaths interface {
	LocalPaths() []string
}

// SensitivePathsHost is the optional Host interface that hands over the
// paths a sandbox must never bind: the data directory, the control and local
// A2A tokens, the runtime directories, the default C1 socket, and what the
// available modules declare through LocalPaths. Read only. Granted to
// autoreply alone: a list of where this node keeps its secrets is itself
// something a smaller grant should not see.
type SensitivePathsHost interface {
	SensitivePaths() []string
}

// Task, TaskEvent and TaskPage are the kernel's A2A projection
// (internal/a2ashape): the JSON the control plane and MCP return, as Go
// values. Aliases rather than types of their own, so that there is one
// definition of what a Task looks like and a module converts it to an SDK
// type by a JSON round trip that the projection's tests pin.
type (
	Task      = a2ashape.Task
	TaskEvent = a2ashape.TaskEvent
	TaskPage  = a2ashape.TaskPage
)

// TaskSeam is what the local A2A interface needs of the kernel
// (A2A-DESIGN §11.1): the A2A operations on this node's outbound tasks.
//
// Every method that takes peerAID acts only on an interaction this node
// started (role outbound) whose peer is peerAID. Anything else — another
// agent's task, a task delegated to this node, no task at all — is
// a2ashape.ErrTaskNotFound, the same error whichever it was, so a client
// holding one agent's URL learns nothing about the node's other tasks
// [C17]. Errors are a2ashape.Error values, matched with errors.Is.
type TaskSeam interface {
	// Send is SendMessage: a new task when req.Message has no task id,
	// otherwise a message on that task. Unless req.ReturnImmediately, it
	// returns once the task is terminal or interrupted (input-required).
	// Delivery goes through the node's retry queue: once the message is
	// recorded it is sent, however long the provider or hub is away; a
	// delegation or input that expires undelivered, or that the hub
	// refuses for good, fails the task with anet.reason=undeliverable.
	Send(ctx context.Context, peerAID string, req TaskSend) (Task, error)
	// SendGate is nil when this node may send now, and otherwise the
	// refusal Send would answer with (an *a2ashape.Refusal: the node is not
	// registered at its hub, LEC AD-6). The local A2A interface asks it
	// before the binding runs, so the refusal reaches the client in the
	// two-ErrorInfo form (ux UX-R10) rather than through the binding's own
	// error encoding.
	SendGate(ctx context.Context) error
	// Get is GetTask. historyLen bounds the history (nil: all of it).
	Get(ctx context.Context, peerAID, taskID string, historyLen *int) (Task, error)
	// List is ListTasks over this node's tasks with peerAID, most recent
	// state change first.
	List(ctx context.Context, peerAID string, f TaskFilter) (TaskPage, error)
	// Cancel is CancelTask (§4.2). A cancel after a payment was submitted
	// leaves the task working with anet.cancel_requested.
	Cancel(ctx context.Context, peerAID, taskID string) (Task, error)
	// Watch is SubscribeToTask: the task as it is now and its later
	// events, taken together so that no change falls between them. The
	// channel closes after the terminal status update, when ctx ends, or
	// when the watcher falls too far behind (it may Watch again). Only a
	// watcher that opts in receives preview events (CUX-A3).
	Watch(ctx context.Context, peerAID, taskID string, opts WatchOpts) (Task, <-chan TaskEvent, error)
	// PreviewAvailable reports whether this node can preview (K-10): the
	// preview module is available. The local A2A interface activates the
	// preview extension, and so declares it on a proxy card, only when this
	// is true (CUX-P30, SI-79).
	PreviewAvailable() bool
	// Agents lists the remote agents a client may address.
	Agents(ctx context.Context, q AgentQuery) ([]RemoteAgent, error)
	// Card is one remote agent, with its verified network card if it has
	// one. It never contacts the agent: Charge is read from the public
	// card, and ExtendedOffered says whether ExtendedCard would.
	Card(ctx context.Context, aid string) (RemoteAgent, error)
	// ExtendedCard is GetExtendedAgentCard (HPP §4.7, core CORE-K23): the
	// agent's extended card, verified here, from this node's cache or
	// fetched end to end within 10 seconds — only when CORE-K23 rule 2
	// holds for it. Every other outcome is an *a2ashape.Refusal of the
	// extended_card_* codes (or the node's own state), with RetryAfter for
	// a passing one; the CLI and MCP show the same sentence.
	ExtendedCard(ctx context.Context, aid string) (ExtendedAgent, error)
	// Pay answers a payment-required task (§8.7): submit signs an
	// authorization within the agent-tier limits and sends it; reject
	// declines. The purpose is always task-agent.
	Pay(ctx context.Context, peerAID, taskID string, decision PayDecision) (Task, error)
	// CanPay reports whether this node can sign a payment and settle it
	// now: a payment module that is available, a hub address, and the
	// hub's AID. It is worked out at each call and never kept (K-11): a
	// node registers at its hub after it starts, so the answer changes
	// while the interface runs. The proxy card declares a2a-x402 only when
	// it is true.
	CanPay() bool
}

// WatchOpts are what a subscriber asks of Watch.
type WatchOpts struct {
	// Preview is set for a subscriber that activated the preview extension
	// (ExtPreviewURI): it receives the task's preview frames as artifact
	// updates, and one for the preview the task shows when it subscribes.
	// A subscriber that did not receives none.
	Preview bool
}

// TaskSend is one SendMessage as the local A2A interface received it.
type TaskSend struct {
	// Message is the client's message. Its TaskID names the task to
	// continue; empty starts a new one. Its ContextID, if set, must be a
	// context of this node's tasks with the same agent, or new.
	Message a2ashape.Message
	// ReturnImmediately returns as soon as the task exists (A2A
	// return_immediately); otherwise Send waits for a terminal or
	// interrupted state.
	ReturnImmediately bool
	// HistoryLength bounds the history in the returned task.
	HistoryLength *int
	// AcceptedOutputModes are the media types the client accepts.
	AcceptedOutputModes []string
	// Metadata is the request's own metadata (SendMessageRequest.metadata),
	// apart from the message's.
	Metadata map[string]any
	// Extensions are the extension URIs the client activated for this
	// request, from A2A-Extensions and X-A2A-Extensions merged.
	Extensions []string
	// Preview asks the provider for previews of the round this message
	// opens: "on" or "with_reasoning" (CUX-P10); empty or "off" asks for
	// none.
	Preview string
}

// TaskFilter selects tasks for List (A2A ListTasksRequest).
type TaskFilter struct {
	ContextID string
	// State is an A2A task state name (TASK_STATE_WORKING); empty means
	// any. a2ashape.StoreState reads it.
	State string
	// PageSize is 1 to 100; 0 means 50 (a2ashape.PageSize).
	PageSize  int
	PageToken string
	// HistoryLen bounds each task's history (nil: all of it).
	HistoryLen *int
	// UpdatedAfter keeps tasks whose status changed at or after it.
	UpdatedAfter *time.Time
	// IncludeArtifacts includes the artifacts, which are left out by
	// default.
	IncludeArtifacts bool
}

// AgentQuery asks for remote agents (A2A-DESIGN §10.5). Skill and Tag are
// sent to the hub's registry; Query is free text matched here against the
// cards that come back and never sent, because the words are the user's
// intent and the hub has no need of them.
//
// IncludeUncarded also lists the agents registered at the hub that publish
// no network card (an internal note item): after the registry's last page, the entries
// of the hub's /agents directory the registry does not list, each with
// Verification "NONE" and only what the hub states about it. The local A2A
// interface never sets it.
type AgentQuery struct {
	Skill           string `json:"skill,omitempty"`
	Tag             string `json:"tag,omitempty"`
	Query           string `json:"q,omitempty"`
	Limit           int    `json:"limit,omitempty"`
	Cursor          string `json:"cursor,omitempty"`
	IncludeUncarded bool   `json:"include_uncarded,omitempty"`
}

// Verification values of a RemoteAgent.
const (
	// CardVerified: this node checked the agent's network card against the
	// agent's key history.
	CardVerified = "VERIFIED"
	// CardUnverified: the agent has a card and this node could not verify
	// it. Nothing of the card is passed on (an internal note item).
	CardUnverified = "UNVERIFIED"
	// CardNone: the agent publishes no network card. What is known of it is
	// the hub's statement (an internal note item).
	CardNone = "NONE"
)

// RemoteAgent is a remote agent as this node sees it: the registry entry
// (§10.5) and this node's own check of its card, which the proxy card is
// built from (§11.3).
//
// What an entry carries depends on Verification (an internal note item, Q27):
//
//   - VERIFIED: the card's bytes, its name, and the hub's statements about
//     the agent (home hub, liveness, reviews);
//   - UNVERIFIED: the AID, the verification and its reason, and Official.
//     Nothing of the card and nothing else the hub said: a hub that
//     serves a forged card must not see its words shown, least of all
//     beside the official mark;
//   - NONE: the agent publishes no card; Name, Caps, Summary and the rest
//     are the hub's statement, not the agent's signed word (only listed on
//     request, AgentQuery.IncludeUncarded). An official AID without a card
//     is shown as UNVERIFIED is, for the same reason.
type RemoteAgent struct {
	AID string `json:"aid"`
	// Card is the agent's A2A network card, the exact bytes served; empty
	// unless the card verified.
	Card json.RawMessage `json:"card,omitempty"`
	// Verification is this node's check of Card: CardVerified,
	// CardUnverified with VerificationError saying why, or CardNone.
	// HubVerification is what the hub said, which is not a substitute.
	Verification      string `json:"verification"`
	VerificationError string `json:"verificationError,omitempty"`
	HubVerification   string `json:"hubVerification,omitempty"`
	Name              string `json:"name,omitempty"`
	// Caps and Summary are the hub directory's, set only on a NONE entry:
	// the capability ids and one-line description the agent registered.
	Caps        []string `json:"caps,omitempty"`
	Summary     string   `json:"summary,omitempty"`
	HomeHub     string   `json:"homeHub,omitempty"`
	LastSeen    string   `json:"lastSeen,omitempty"`
	Quiet       bool     `json:"quiet,omitempty"`
	ReviewCount int      `json:"reviewCount,omitempty"`
	AvgRating   any      `json:"avgRating,omitempty"`
	// KeyState is KeyStateFresh when the directory's entry for the agent
	// carried a notice its witness hub signed, checked here and fresh, about
	// the tip of its key history; KeyStateStale otherwise. It says whether
	// the agent's hub confirmed its current key, not whether its card
	// verified (Verification), and a stale one does not stop a send: the
	// send waits for a fresh confirmation itself (KEL D-26, D-28).
	KeyState string `json:"key_state,omitempty"`
	// Official is true when AID is listed in the official-agent manifest
	// built into this binary, signed with the release key (A2A-DESIGN §15,
	// internal/official). The AID alone decides it: a name, a card or a
	// hub's word never does. It is a label; it grants the agent nothing.
	Official bool `json:"anet.official,omitempty"`
	// Charge is free, partial or paid (HPP PAY-C15), read from the
	// standard a2a-x402 declaration of the card shown: the extended card
	// when one was fetched and verified, the public card otherwise. Only
	// for a VERIFIED card.
	Charge string `json:"charge,omitempty"`
	// Prices are every skill of the verified extended card, in its order,
	// a free one at "0" (HPP §2.7); only when that card was had.
	// PricesFetchedAt is when it was fetched (RFC 3339, milliseconds).
	Prices          []SkillPrice `json:"prices,omitempty"`
	PricesFetchedAt string       `json:"pricesFetchedAt,omitempty"`
	// PriceNote is the sentence of extended_card_unavailable (or its
	// .bad_signature variant) when the extended card should have been
	// fetched and was not (CORE-K23 rule 4); the CLI's last line and the
	// local A2A error message say the same.
	PriceNote string `json:"priceNote,omitempty"`
	// ExtendedOffered says CORE-K23 rule 2 holds: the agent's public card
	// declares an extended card, and either declares a2a-x402 or this node
	// holds a contact capability the agent issued it. Only then is the
	// agent ever asked for its extended card for a display, and only then
	// does its proxy card declare one (rule 3). Not on the wire.
	ExtendedOffered bool `json:"-"`
}

// Charge values (HPP PAY-C15): what a card's a2a-x402 declaration says.
const (
	ChargeFree    = "free"    // no declaration
	ChargePartial = "partial" // declared, required omitted
	ChargePaid    = "paid"    // declared required
)

// SkillPrice is one skill of an extended card and the price its signer
// put on it: Amount is a decimal string of credits, "0" for a free skill.
type SkillPrice struct {
	Skill  string `json:"skill"`
	Amount string `json:"amount"`
	Unit   string `json:"unit"`
}

// ExtendedAgent is a remote agent with its extended card (TaskSeam.
// ExtendedCard): Agent as Card gives it, with Charge and Prices from the
// extended card, and Card the extended card's bytes exactly as its signer
// signed them, verified here.
type ExtendedAgent struct {
	Agent RemoteAgent
	Card  json.RawMessage
}

// RemoteAgent.KeyState values (KEL D-28).
const (
	KeyStateFresh = "fresh"
	KeyStateStale = "stale"
)

// PayDecision answers a payment-required task.
type PayDecision struct {
	// Decision is PaySubmit or PayReject.
	Decision string
	// Accept is the chosen option, copied unchanged from
	// x402.payment.required.accepts (anet.payment.accept). It may be
	// omitted when there is only one.
	Accept json.RawMessage
}

// PayDecision values.
const (
	PaySubmit = "submit"
	PayReject = "reject"
)

// HubSeam is what a module needs to act as this node against its hub.
//
// Two methods, and the first is the whole grant: signing as this node is
// how the hub knows who is calling. Named after the relationship rather
// than after any one module, because several subsystems legitimately need
// to talk to the hub and none of them should need the payment seam to do
// it.
type HubSeam interface {
	// Sign signs one challenge preimage as this node, returning the
	// signature and the key-state sequence it was made under.
	Sign(preimage []byte) (sig []byte, keyStateSeq uint64)
	// HubURL is where this node's hub lives.
	HubURL() string
	// HubDo sends one request to this node's hub; see PaymentSeam.HubDo.
	HubDo(req *http.Request) (*http.Response, error)
}

// PaymentSeam is exactly what a payment subsystem needs of the node.
//
// Narrow on purpose, and named after what it is for rather than what it
// contains: a reader asking "what can the payment module do to my key"
// should find the answer in one place.
type PaymentSeam interface {
	// Sign signs one object's preimage as this node, returning the
	// signature and the key-state sequence it was made under.
	Sign(preimage []byte) (sig []byte, keyStateSeq uint64)
	// HubIdentity is the hub this node settles on: its AID and its
	// verified key history. The AID goes into every authorization's
	// network field, so a payment signed for one hub cannot be replayed
	// at another; the key history is what makes the hub's settlement
	// receipts checkable rather than merely received.
	HubIdentity() (aid string, kel []identity.SignedEvent, ok bool)
	// HubURL is where to reach that hub.
	HubURL() string
	// ReadEvidence reads this node's own chain back, newest last, for one
	// event type.
	//
	// Here rather than on Host because it is the read half of a write
	// Host already grants, and because only the payment module needs it:
	// reconciliation and the issuance audit compare the hub's ledger with
	// what this node recorded of its own payments. The alternative was a
	// second persistence layer holding the same facts, which is how two
	// records of one event start disagreeing.
	ReadEvidence(eventType string, limit int) []map[string]any

	// AdmitSpend is the node's spending policy (A2A-DESIGN §8.6): may this
	// node sign a payment of amount to payTo for purpose? nil means yes,
	// and the spend is counted against the daily totals before this
	// returns; an error says no and counts nothing.
	//
	// Added because the payment module is the one place every signature
	// over money passes through (Authorize, and Redeem through it), while
	// the limits, the payee list and the per-purpose daily totals are the
	// operator's policy, which the kernel owns and the control plane edits.
	// Keeping the policy in the module would give the node two places that
	// decide how much it may spend, and a module that can sign and also
	// decides its own limits is a module that has no limits. So the module
	// asks, and signs only on a yes.
	//
	// purpose is one of task-auto, task-agent, task-manual and redeem;
	// the kernel refuses any other.
	AdmitSpend(payTo string, amount uint64, purpose string) error

	// HubDo sends one request to this node's hub (HubURL) the way the node
	// sends its own: with the hub wire version, a fixed User-Agent, no
	// cookies, and on the connection side its route belongs to — the side
	// that names this node (a signed request, this node's own records) or
	// the side that does not (sealed-sender §4.3.11). A module that opened
	// connections of its own would put a signed request and an anonymous
	// one on the same connection, and the connection would name the sender
	// the anonymous one hides. A request to anything but this node's hub,
	// or to a route the node does not send, is refused before anything is
	// sent. The caller closes the response body.
	HubDo(req *http.Request) (*http.Response, error)

	// SignHubRequest signs req as this node for its hub (relayauth v2, RC1):
	// the action, the hub's AID, the signing time, the method, the request
	// target and the body bytes, set in the X-ANet-* headers. body is the
	// exact bytes the request carries, nil for none. The signing time comes
	// from the clock the node keeps for that hub, shared with the node's own
	// requests, so two requests never carry the same time (PAY-R10). The
	// outbound gate applies as it does to the node's own signed requests:
	// nothing is signed while the identity is not confirmed.
	//
	// The ability is the same as Sign's and is granted with it: the module
	// asks for the request to be signed, it never composes the preimage.
	SignHubRequest(req *http.Request, action string, body []byte) error
	// NoteHubStale tells the node that the hub answered a request signed by
	// SignHubRequest with ts_stale and a floor (the member "floor" of the
	// body, or X-ANet-TS-Floor). True: the clock for that hub is now above
	// the floor and the caller signs again and retries once; false: the
	// floor is beyond what other hubs accept, and the hub is unreachable for
	// this request (PAY-R11).
	NoteHubStale(floor int64) bool
	// RegisteredAt is when this node first registered at its hub, unix
	// milliseconds, and false when the node does not know. An audit finding
	// that the hub withheld a balance proof is made only for a period that
	// closed after this.
	RegisteredAt() (int64, bool)
	// RefreshHubIdentity reads the hub's key history again, for a record the
	// hub signed under a key state the pinned history does not reach: the
	// hub rotated since it was pinned. It reports whether the history
	// changed, so the check is worth making again; the hub's history may
	// only ever extend what is pinned (KEL H-34, CORE-L8).
	RefreshHubIdentity() bool
	// PeerHubKEL is the key history of another hub, one this node's hub
	// federates with, as the node pinned it from its hub's GET /hub/kels (it
	// asks again, at most once a minute, when it holds none). A payee whose
	// hub is not the ledger hub of a payment gets the settlement receipt the
	// ledger hub signed, and is only able to check it against that hub's
	// history. ok is false when the node holds none: the receipt is then not
	// verified, and is not taken for a settlement.
	PeerHubKEL(aid string) (kel []identity.SignedEvent, ok bool)
}

// EvidenceAt is the member ReadEvidence adds to each payload it returns: the
// time the event was recorded on this node's chain, unix milliseconds.
const EvidenceAt = "_recorded_at"

// Spending purposes: the tiers of A2A-DESIGN §8.6, named by the surface a
// payment was asked for on. The control-plane route decides the purpose,
// never the caller's own say-so.
const (
	PurposeTaskAuto   = "task-auto"   // the daemon paying a quote on its own, within auto_max
	PurposeTaskAgent  = "task-agent"  // POST /tasks/pay: MCP submit_payment, a local A2A client
	PurposeTaskManual = "task-manual" // POST /tasks/pay-manual: `anet pay` after a terminal confirmation
	PurposeRedeem     = "redeem"      // /redeem: credit given back to the hub
)

// ErrRedeemPayee is Payer.Redeem's refusal of a redemption whose confirmed
// payee is not the hub this node settles on now: the operator agreed to pay
// one hub and the authorization would be signed to another (the node changed
// hubs in between, or the caller named the wrong one). Nothing is signed.
var ErrRedeemPayee = errors.New("the payee confirmed for this redemption is not the hub this node settles on")

// Module is an optional daemon subsystem.
type Module interface {
	// Name is the module's stable name; it matches its build tag, so
	// `no_<name>` compiles it out.
	Name() string
	// Start brings the module up. An error is fatal to the daemon: a module
	// that was compiled in and configured but cannot run is an operator
	// error, and starting without it would be silent capability loss.
	// ctx lives as long as the daemon. A Start that fails releases what it
	// opened itself: Stop is called only for a module that started.
	Start(ctx context.Context, h Host) error
	// Stop shuts it down. The daemon calls it once, when it closes (or
	// fails to start after this module did): after Start's ctx is
	// cancelled and before the evidence ledger closes, so RecordEvidence
	// still works. Its own ctx carries the shutdown deadline.
	Stop(ctx context.Context) error
}

// Confidential is implemented by a module holding values that must never
// appear in anything this node publishes publicly — INV-2.
//
// Optional, and one-directional on purpose. The org module knows its org
// id is confidential; the daemon knows only that some string must not
// leave. Asking the daemon to understand what an org is, so that it could
// decide for itself, is how the organisation feature got into thirty-five
// daemon files last time.
type Confidential interface {
	// ForbiddenTokens returns strings that must not appear in a public
	// publication. Called on every publish, so it must be cheap.
	ForbiddenTokens() []string
}

// Payer is implemented by a module that can charge for work and pay for
// it — the x402 subsystem.
//
// Optional, and type-asserted at build time exactly like Confidential.
// The kernel therefore never imports the module (K207): it knows only
// that something may be able to price and settle, and answers honestly
// when nothing can.
//
// A build without it does not silently do paid work for free. A priced
// capability is refused with a message saying this build cannot take
// payment, because "I cannot charge you, so I will not do it" is true and
// "here, have it" is a decision nobody made.
type Payer interface {
	// Price reports what a capability costs here and whether it costs
	// anything. Free is (0, false), never (0, true).
	Price(capID string) (uint64, bool)
	// HasPrices reports whether any capability this node serves has a
	// price, from the same answers as Price: the pay flag of the
	// capabilities this node gives its peers (core X-SS-8).
	HasPrices() bool
	// Quote builds the PAYMENT_REQUIRED body for a priced capability.
	Quote(capID string, price uint64) *payment.PaymentRequired
	// Authorize signs a payment for one interaction and returns the
	// marshalled x402 PaymentPayload. The kernel drives the task; this
	// signs the money.
	//
	// ix is the interaction the payment is for and bind is what the
	// authorization's InteractionID carries: pay_bind(ix, task_nonce) for a
	// task payment (A2A-DESIGN §2 X4), so the hub, which sees the binding,
	// cannot read the interaction id from it. purpose names the spending
	// tier (§8.6); the module asks PaymentSeam.AdmitSpend before it signs
	// and records ix, bind and purpose on the anet.payment.authorized
	// event, from which the kernel rebuilds its daily totals at start.
	Authorize(opt payment.PaymentOption, ix, bind, purpose string) ([]byte, error)
	// Settle presents a payment to the facilitator together with the terms
	// it is checked against (x402 v2 paymentRequirements, which an anet hub
	// requires) and reports what happened, including the hub's signed
	// receipt when it sent one. An error means the outcome is not known (the
	// hub could not be reached or answered unreadably); the caller retries
	// with the same payload, which the hub settles at most once.
	Settle(ctx context.Context, raw []byte, req payment.PaymentRequirements) (Settlement, error)
	// CheckPayment is the merchant's check of a payment against what this
	// node quoted for the task, before anything is settled (A2A-DESIGN
	// §8.4): the payee is this node, the amount covers the quote, the
	// binding is the task's, scheme and network are among the quoted
	// options, and neither the authorization nor the quote has expired.
	//
	// It lives with the module because it reads the scheme's own payload
	// (an anet-credit authorization), which the kernel does not interpret;
	// the kernel supplies the terms it stored with the task.
	CheckPayment(raw []byte, t PaymentTerms) PaymentCheck
	// PaymentError maps a facilitator errorReason, or a reason of this
	// node's own check, to the a2a-x402 x402.payment.error code (§8.5).
	// final is false for a reason that is not an outcome
	// (settlement_pending): no code is sent for it and the caller retries.
	PaymentError(reason string) (code string, final bool)
	// VerifyReceipt checks a hub settlement receipt, pinning the signer to
	// this node's own hub and the payer to expectPayer. Reports what the
	// receipt says either way: "the provider told us it was paid" and "the
	// hub signed that it moved the credit" are different facts, and a
	// caller that cannot tell them apart will record the stronger.
	VerifyReceipt(receiptB64, expectPayer string) (ReceiptFacts, bool)
	// Balance reads this node's standing off the custodian.
	Balance(ctx context.Context) (map[string]any, error)
	// Redeem gives credit back to the hub against an external reference.
	// payTo is the hub AID the operator confirmed (anet redeem shows it
	// before asking): the authorization is signed only when it is the AID
	// of the hub this node settles on now, else ErrRedeemPayee and nothing
	// is signed.
	Redeem(ctx context.Context, amount uint64, reference, payTo string) (map[string]any, error)
	// HomeNetwork is the ledger this node's own credits live on, so a
	// caller offered several rails can pick the one it can actually pay
	// from. Empty when this node has no hub.
	HomeNetwork() string
	// Reconcile compares this node's own payment history against the
	// hub's ledger for this account, and AuditIssuance verifies the
	// hub's supply chain against heads this node recorded earlier.
	//
	// Both return a report rather than an error on disagreement: a
	// discrepancy is a finding to show somebody, not a failure of the
	// call.
	Reconcile(ctx context.Context) (any, error)
	AuditIssuance(ctx context.Context) (any, error)
	// AuditSummary is what the module's automatic balance-proof check last
	// found, as a JSON object for doctor's payments.audit: nil before the
	// first check.
	AuditSummary() any
	// Serve starts the module's background work. Called once, after
	// Start, by the kernel that owns the process lifetime. It opens no
	// listener: the payment module has no public door.
	Serve(ctx context.Context) error
}

// ReceiptFacts is what a settlement receipt says, once checked.
//
// AuthID is also the settlement's transaction (the anet-credit scheme:
// transaction = auth_id), and Network the ledger it settled on: a
// settlement response a provider passes on is the hub's own only when
// its transaction and network are these [redteam:F11].
type ReceiptFacts struct {
	Payee   string
	AuthID  string
	Amount  uint64
	Network string
}

// Settlement is a completed payment as the kernel needs to see it.
//
// Deliberately not the x402 response type: the kernel carries these
// fields into a result and onto a chain and has no business with the
// rest. Receipt stays base64 rather than parsed, because the kernel is
// passing it through to whoever can check it and checking is not its job.
type Settlement struct {
	Transaction string
	Amount      string
	Network     string
	Receipt     string // base64 CoreDet-CBOR, empty if the hub signed nothing
	Failed      string // non-empty when the payment did not settle: the facilitator's errorReason
	// Code is the a2a-x402 x402.payment.error for Failed, empty on success
	// and while Pending.
	Code string
	// Pending is set when the facilitator said the outcome is not known yet
	// (settlement_pending). It is not a failure: the caller presents the
	// same payload again.
	Pending bool
	// Replayed is set when the facilitator answered with an earlier
	// settlement of the same authorization (anet.replayed).
	Replayed bool
	// Response is the facilitator's answer as received. It is what the
	// task's x402.payment.receipts list carries, so the payer sees the
	// facilitator's own words rather than the kernel's summary of them.
	Response *payment.SettlementResponse
}

// PaymentTerms is what a task was quoted, as CheckPayment compares a
// payment against it.
type PaymentTerms struct {
	// Quoted is the x402 PaymentRequired this node sent for the task; nil
	// when it sent none.
	Quoted *payment.PaymentRequired
	// Bind is the value the authorization's InteractionID must carry.
	Bind string
	// Payer, when set, is who must have signed: the task's requester.
	Payer string
	// QuoteExpiresAt is when the quote lapses (unix ms); zero for never.
	QuoteExpiresAt int64
	// Now is the time of the check (unix ms).
	Now int64
}

// PaymentCheck is CheckPayment's answer.
type PaymentCheck struct {
	// AuthID is the authorization's content id, when it could be read.
	AuthID string
	Payer  string
	Amount uint64
	// Requirements is the quoted option the payment was checked against,
	// as it goes to the facilitator: no extra, no description, no resource
	// (SI-1). Set when an option matched, even if a later check failed.
	Requirements payment.PaymentRequirements
	Matched      bool
	// Reason is empty when the payment passed, otherwise the x402
	// errorReason or this node's own reason (binding_mismatch,
	// no_pending_quote, quote_expired, payer_mismatch).
	Reason string
	// Code is the x402.payment.error for Reason.
	Code string
	// Detail is a sentence for a person.
	Detail string
}

// Factory builds a module from its configuration block. Returning (nil, nil)
// means "compiled in, not configured" — the ordinary case for a module the
// operator has not asked for.
type Factory func(raw []byte) (Module, error)

type registration struct {
	name    string
	factory Factory
}

// optInNames holds the names of modules whose build tag is ADDITIVE:
// absent from the default build, present only with `-tags <name>`.
//
// Declared through DeclareOptIn from a file with no build tag, which is
// the only thing that works here. The registry holds what the build
// linked; this map has to name a module that is deliberately NOT linked,
// so it cannot be filled from the module's own registration — that
// registration is exactly what the tag removed.
var optInNames = map[string]bool{}

// extraCompiled names optional subsystems that are compiled in but are
// NOT module.Module registrations.
//
// The MCP northbound is one: it lives in internal/mcpserv and is reached
// as a CLI subcommand, so it never registers here — and `anet version`,
// which reads Compiled(), reported `modules: service` for a build that
// carried nine working MCP tools. A line that claims to say what is in the
// binary and omits part of it is worse than no line, because the
// documentation tells people to identify their build by reading it.
//
// Declared from a file carrying the same build tag as the subsystem, so
// the list is derived from what the linker kept, not from what somebody
// remembered to write down.
var extraCompiled []string

// DeclareCompiled records a compiled-in subsystem that is not a Module.
func DeclareCompiled(names ...string) {
	regMu.Lock()
	defer regMu.Unlock()
	extraCompiled = append(extraCompiled, names...)
}

// DeclareOptIn records that a module is reached by an additive tag.
//
// Names only, no code: the point is for a build that does NOT contain the
// module to still be able to say which flag would have brought it in.
// Telling an operator to check for `no_shell` when no such tag exists
// sends them looking for a flag that was never there.
func DeclareOptIn(names ...string) {
	regMu.Lock()
	defer regMu.Unlock()
	for _, n := range names {
		optInNames[n] = true
	}
}

var (
	regMu    sync.Mutex
	registry []registration
)

// Register adds a module factory. Called from an init() in a file carrying
// the module's build tag, which is what makes the tag the on/off switch.
func Register(name string, f Factory) {
	regMu.Lock()
	defer regMu.Unlock()
	for _, r := range registry {
		if r.name == name {
			panic("module: duplicate registration " + name)
		}
	}
	registry = append(registry, registration{name: name, factory: f})
}

// Compiled lists the module names in this build, sorted. The daemon logs it
// at startup so an operator can see what their binary actually contains
// rather than what the documentation says it might.
// declaredCompiled reports whether a name belongs to a compiled-in
// subsystem that is not a Module — see DeclareCompiled.
func declaredCompiled(name string) bool {
	regMu.Lock()
	defer regMu.Unlock()
	for _, n := range extraCompiled {
		if n == name {
			return true
		}
	}
	return false
}

// optInName reports whether an absent module is an additive one.
func optInName(name string) bool {
	regMu.Lock()
	defer regMu.Unlock()
	return optInNames[name]
}

func Compiled() []string {
	regMu.Lock()
	defer regMu.Unlock()
	out := make([]string, 0, len(registry)+len(extraCompiled))
	for _, r := range registry {
		out = append(out, r.name)
	}
	out = append(out, extraCompiled...)
	sort.Strings(out)
	return out
}

// absentConfigured is the refusal for a configuration block of a module that
// is not available: module_absent.configured, the same sentence at start,
// in the daemon log and in doctor (MOD-F1).
func absentConfigured(av Availability, name string) error {
	reason := av.Absence(name)
	if reason == "" {
		reason = AbsentNotCompiled
	}
	info, _ := Lookup(name)
	key := info.ConfigKey
	if key == "" {
		key = "modules." + name
	}
	args := AbsentArgs(name, reason)
	args["key"] = key
	return a2ashape.Refuse("module_absent.configured", args)
}

// BuildOne instantiates a single registered module from its config
// block, for callers that want one rather than the set.
//
// Exists because a module's factory is the only thing standing between an
// operator's typo and a daemon that starts without the capabilities they
// asked for, and testing that through Build meant standing up every other
// module too. Returns (nil, nil) for "compiled in, not configured", the
// same as Build does.
func BuildOne(name string, cfg []byte) (Module, error) {
	regMu.Lock()
	defer regMu.Unlock()
	for _, r := range registry {
		if r.name == name {
			return r.factory(cfg)
		}
	}
	return nil, absentConfigured(Availability{}, name)
}

// Build instantiates every compiled-in module that is configured.
//
// A configured module that is not compiled in is an error, not a no-op. The
// alternative is a daemon that reads `"p2p": {...}` from a config, silently
// ignores it, and leaves an operator wondering why nothing is peering.
func Build(cfg map[string][]byte) ([]Module, error) { return BuildWith(Availability{}, cfg) }

// BuildWith is Build for the modules an Availability value lets run: a
// module that is turned off is not built (its factory is not called), and a
// configuration block for a module that is not available is the refusal
// module_absent.configured (MOD-F1), whichever way it is absent.
func BuildWith(av Availability, cfg map[string][]byte) ([]Module, error) {
	regMu.Lock()
	regs := append([]registration(nil), registry...)
	regMu.Unlock()

	known := make(map[string]bool, len(regs))
	var out []Module
	for _, r := range regs {
		known[r.name] = true
		if !av.Available(r.name) {
			if _, ok := cfg[r.name]; ok {
				return nil, absentConfigured(av, r.name)
			}
			continue
		}
		m, err := r.factory(cfg[r.name])
		if err != nil {
			return nil, fmt.Errorf("module %q: %w", r.name, err)
		}
		if m != nil {
			out = append(out, m)
		}
	}
	names := make([]string, 0, len(cfg))
	for name := range cfg {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if known[name] {
			continue
		}
		info, ok := Lookup(name)
		if !ok {
			return nil, a2ashape.Refuse("config_invalid", a2ashape.Args{"key": "modules." + clip(name),
				"rule": ruleText("没有这个模块", "there is no such module")})
		}
		// A name of the table that is not a registration here. Its block is
		// either not under modules at all (autoreply and preview keep their
		// historical top-level keys, MOD-P3; mcp and console take none), or
		// the module is not in this binary.
		switch {
		case info.ConfigKey == "":
			return nil, a2ashape.Refuse("config_invalid", a2ashape.Args{"key": "modules." + name,
				"rule": ruleText("这个模块没有配置块", "this module takes no configuration block")})
		case info.ConfigKey != "modules."+name:
			return nil, a2ashape.Refuse("config_invalid", a2ashape.Args{"key": "modules." + name,
				"rule": "top_level_key", "m": name})
		}
		return nil, absentConfigured(av, name)
	}
	return out, nil
}
