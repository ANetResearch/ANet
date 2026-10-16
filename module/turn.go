package module

// turn.go — the seams of the auto-reply module (K-6, K-7, K-8): the turns a
// node owes an answer in, the one place a module's configuration block is
// written, and the module's own evidence read back. They are optional Host
// interfaces granted by the name table to autoreply alone (Grants:
// TurnHost, OwnEvidenceHost, ModuleConfigHost); a Host of any other module
// does not implement them, so the type assertion fails.
//
// What the kernel keeps, and the module cannot reach around: who may be
// answered (the inbound policy, the lists, the pending approvals and the
// "owed" decision), every send (the outbound queue, completeWithReply, the
// status messages, RequestEnd), the reason codes and their sentences, the
// evidence type, and the audit of a configuration write (anet.policy.changed).

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

// ErrTurnGone is what a send answers when the turn is no longer one the
// module may answer: the peer is on the deny list, the interaction has
// ended, or the turn is no longer owed (a message of ours is the last).
// Nothing was sent.
var ErrTurnGone = errors.New("module: the turn is gone")

// ErrRefuseReason is what RefuseTurn answers for a reason outside the
// allowlist (ReasonSandboxUnavailable, ReasonBudget, ReasonBackendFailed).
var ErrRefuseReason = errors.New("module: a refusal reason outside the allowlist")

// The reasons RefuseTurn takes. They are the kernel's codes (a2ashape); the
// module only names which one applies.
const (
	// ReasonSandboxUnavailable: status rejected, no text, through the
	// outbound queue.
	ReasonSandboxUnavailable = "sandbox_unavailable"
	// ReasonBudget: status rejected, no text, with retry_after.
	ReasonBudget = "budget"
	// ReasonBackendFailed: status input-required, no text (UX-R3).
	ReasonBackendFailed = "backend_failed"
)

// Turn is one conversation in which the node owes a reply, as OwedTurns
// hands it over. Messages hold the text messages only: end requests,
// cancels, status and payment messages (control messages) are left out.
type Turn struct {
	// Ix is the interaction id.
	Ix string
	// Role is this node's side: "inbound" (someone delegated to this node)
	// or "outbound" (this node delegated and the peer has since replied).
	Role string
	// Peer is the other side's AID.
	Peer string
	// Trusted is whether the peer is on the trust list, read when the turn
	// was handed over.
	Trusted bool
	// State is the task state, a non-terminal one.
	State string
	// Goal is the task's goal text.
	Goal string
	// EndReqBy is "me", "them" or "": who asked for the task to end.
	EndReqBy string
	// Messages are the text messages without the control messages, oldest
	// first.
	Messages []TurnMessage
	// MsgCount is how many messages the interaction holds, control messages
	// included; it grows with every message, so a turn that was turned away
	// is tried again only when it changes.
	MsgCount int
	// PeerMsgCount is how many of those the peer sent.
	PeerMsgCount int
}

// TurnMessage is one text message of a Turn. From is "me" or "them".
type TurnMessage struct {
	From        string
	Body        string
	Attachments []TurnAttachment
}

// TurnAttachment names a file that came with a message; TurnHost.TurnAttachment
// reads its bytes.
type TurnAttachment struct {
	CID  string
	Name string
	Mime string
	Size int64
}

// Attachment is a file the module sends with a reply.
type Attachment struct {
	Name string
	Mime string
	Data []byte
}

// PreviewSink takes what a backend has written so far of the answer to a
// turn: the whole text and the whole reasoning, not a delta (CUX-P5). It
// exists only while the turn is previewed (preview available and CUX-P4
// holds); the kernel gives it to the module through TurnHost.PreviewTurn and
// the module never sees a preview frame.
type PreviewSink interface {
	Offer(text, reasoning string)
}

// TurnHost is the optional Host interface through which the auto-reply
// module answers conversations as this node (K-7). It grants a great deal:
// what is sent goes out as this node, under its key. Four things keep it
// narrow: the kernel decides which turns are owed (OwedTurns), re-decides at
// every send (SendTurn, RefuseTurn, WorkingTurn, EndTurn answer ErrTurnGone
// when the turn is no longer owed or the peer is denied), accepts only three
// reasons for a refusal, and keeps the sending path, so a module can neither
// choose the recipient nor the kind of message beyond these calls.
type TurnHost interface {
	// OwedTurns lists the turns in which a reply is owed, both directions: the
	// interaction is admitted and not ended, is not a capability call or a
	// public_cap one, is not taken by an A2A backend (backendAnswers), the
	// peer is not on the deny list, and the last text message is the peer's.
	// It hands over nothing before the daemon has finished starting.
	OwedTurns(ctx context.Context) ([]Turn, error)
	// Wake delivers a value when an inbound message has just landed, so that
	// the module does not wait for its next scan. The channel has room for
	// one value; bursts coalesce.
	Wake() <-chan struct{}
	// SendTurn sends text and files as this node's reply. For an inbound turn
	// with final set, the reply completes the task and goes in its result
	// (completeWithReply); otherwise it is a message. It cancels the turn's
	// preview.
	SendTurn(ctx context.Context, ix, text string, atts []Attachment, final bool) error
	// RefuseTurn turns the turn away with a reason from the allowlist: for
	// ReasonSandboxUnavailable and ReasonBudget (retryAfter) status rejected,
	// without text, through the outbound queue; for ReasonBackendFailed
	// status input-required, without text.
	RefuseTurn(ctx context.Context, ix, reason string, retryAfter time.Duration) error
	// WorkingTurn tells the requester the task is being worked on.
	WorkingTurn(ctx context.Context, ix string) error
	// EndTurn proposes ending the task.
	EndTurn(ctx context.Context, ix string) error
	// TurnAttachment reads an attachment of an interaction a turn was
	// handed over for.
	TurnAttachment(ix, cid string) ([]byte, error)
	// PreviewTurn is the sink for the turn's preview, and false when the
	// preview module is not available or the turn is not previewed.
	PreviewTurn(ix string) (PreviewSink, bool)
}

// Evidence is one event of the node's own chain, as ReadOwnEvidence returns it.
type Evidence struct {
	Type    string
	At      time.Time
	Payload map[string]any
}

// OwnEvidenceHost is the optional Host interface through which a module
// reads back the events of the types the name table lists for it
// (EvidenceTypes) — the auto-reply budget is replayed from its own
// records after a restart (BU-2). A type outside the module's list is an
// error. Read only.
type OwnEvidenceHost interface {
	ReadOwnEvidence(eventType string, since time.Time) ([]Evidence, error)
}

// ModuleConfigHost is the optional Host interface through which a module
// changes its own configuration block (K-6). It takes no key: the block is
// the one the bound name owns (autoreply: the top-level auto_reply). A
// write is one step: the kernel holds the configuration lock, reads the file
// from the disk, calls fn with the block as it is (nil when there is none),
// runs the module's ConfigSpec.Check on the result, replaces this one key and
// saves atomically; fn returning nil removes the block. In the same step the
// kernel compares the old and new block through ConfigSpec and writes
// anet.policy.changed for what changed; a module cannot write that event
// itself.
type ModuleConfigHost interface {
	UpdateModuleConfig(fn func(old json.RawMessage) (json.RawMessage, error)) error
}

// ConfigSpec is what a module registers (RegisterConfigSpec) so that the
// kernel can check and audit its configuration block without parsing it.
type ConfigSpec struct {
	// Check refuses a block this module does not take. raw is nil for no
	// block. dataDir is the node's data directory, for the checks that need
	// it; empty skips those. The error is an *a2ashape.Refusal for a reason
	// with a sentence, any other error otherwise. It runs when the daemon
	// starts and on every write through ModuleConfigHost.
	Check func(raw json.RawMessage, dataDir string) error
	// Summary is the part of the block an anet.policy.changed event records
	// for the key as a whole (never a secret); nil when there is no block.
	Summary func(raw json.RawMessage) map[string]any
	// Audit gives the value of each of the name table's AuditFields in the
	// block, by field name; nil when there is no block. A field whose value
	// differs between the old and the new block is a change of its own.
	Audit func(raw json.RawMessage) map[string]any
	// SI5 gives the SI-5 values the block contributes ("auto_reply.untrusted"
	// among them, "off" for no block); used where the kernel has the
	// configuration file but no running module (doctor, ReadPolicy).
	SI5 func(raw json.RawMessage) map[string]any
}

var (
	specMu sync.Mutex
	specs  = map[string]ConfigSpec{}
)

// RegisterConfigSpec records the ConfigSpec of the module called name, which
// must be a row of the name table. Called from an init of the module's code.
func RegisterConfigSpec(name string, s ConfigSpec) {
	if _, ok := Lookup(name); !ok {
		panic("module: ConfigSpec for a name outside the table: " + name)
	}
	specMu.Lock()
	defer specMu.Unlock()
	if _, dup := specs[name]; dup {
		panic("module: duplicate ConfigSpec " + name)
	}
	specs[name] = s
}

// ConfigSpecOf is the ConfigSpec registered for name.
func ConfigSpecOf(name string) (ConfigSpec, bool) {
	specMu.Lock()
	defer specMu.Unlock()
	s, ok := specs[name]
	return s, ok
}
