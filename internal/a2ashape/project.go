package a2ashape

// project.go turns a stored interaction into an A2A Task (A2A-DESIGN §11.5,
// "Task 表示"; SI-6). It is the one place the mapping is written: the
// control plane's /tasks/* routes, MCP (which forwards the control plane's
// JSON unchanged) and the TaskSeam behind module/a2a all return what
// Project returns, so a rule fixed here is fixed for every door.
//
// It lives here rather than in the daemon because it is a pure function of
// what the store holds — a row, its message log, its attachments' metadata —
// and because the contract tests that pin it against a2a-go belong next to
// the types they pin. It reads the store's types and never writes.
//
// The rules, in the order the Task is assembled:
//
//   - status.state is the stored state (§4.1), translated and nothing else.
//     In particular a COMPLETED capability task stays COMPLETED whatever its
//     effect status says, and a COMPLETED text task whatever became of its
//     receipt: the two facts are carried in metadata beside the state, not
//     folded into it (SI-6).
//   - status.message says why the task is where it is: the provider's latest
//     message for input-required, the x402 payment-required message when a
//     quote is what it waits for, progress for working, the reason for
//     failed, rejected and canceled, the settlement for a paid completion.
//   - history is the message log without its control rows (end requests,
//     cancels, status updates, payment messages, and text carrying x402.* or
//     anet.state metadata); the requester is the user and the provider the
//     agent, on both sides of the task. A capability call carries no
//     conversation, so its history is the one request, as the DataPart
//     {skill, args} a client would have sent.
//   - artifacts appear once a receipt covers a result: first the
//     deliverable — anet.reply (the provider's last message in the
//     transcript the receipt covers) for a text task, anet.result (the
//     deliverable as a DataPart) for a capability call — then anet.receipt,
//     then the reply's attachments as file parts. A PAYMENT_REQUIRED answer
//     is a quote, not a result: it has none.
//   - metadata carries anet.effect_status (capability tasks; always present
//     once one is terminal), anet.receipt_verified (every completed task and
//     every task with a receipt), the CIDs, the peer, this node's side, the
//     state sequence number, and the trust, reason, retry hint, cancel
//     request and x402 keys when there are any.

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/evidence"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/internal/transcript"
)

// StateOf is the A2A state of a stored task state.
func StateOf(st interactions.State) TaskState {
	switch st {
	case interactions.StateSubmitted:
		return TaskStateSubmitted
	case interactions.StateWorking:
		return TaskStateWorking
	case interactions.StateInputRequired:
		return TaskStateInputRequired
	case interactions.StateCompleted:
		return TaskStateCompleted
	case interactions.StateFailed:
		return TaskStateFailed
	case interactions.StateCanceled:
		return TaskStateCanceled
	case interactions.StateRejected:
		return TaskStateRejected
	}
	return TaskStateUnspecified
}

// StoreState is the stored state an A2A state names, for a ListTasks status
// filter. The store's own spelling ("input-required") is accepted too, for
// a control-plane caller that uses it. ok is false for a state the store
// never holds (auth-required, unspecified): such a filter matches nothing.
// ok is also false for a name that is no state at all; the caller tells the
// two apart with TaskState.Valid, because A2A answers the second with
// InvalidParams rather than an empty page.
func StoreState(s TaskState) (interactions.State, bool) {
	if st := interactions.State(s); st.Valid() {
		return st, true
	}
	for _, st := range []interactions.State{interactions.StateSubmitted, interactions.StateWorking,
		interactions.StateInputRequired, interactions.StateCompleted, interactions.StateFailed,
		interactions.StateCanceled, interactions.StateRejected} {
		if StateOf(st) == s {
			return st, true
		}
	}
	return "", false
}

// ControlMetadata reports whether message metadata makes a text message a
// control message rather than a conversation turn: any x402.* key, or
// anet.state (A2A-DESIGN §4.1 [C29]). Such a message is neither in the
// transcript a receipt covers nor in the history.
func ControlMetadata(meta map[string]any) bool {
	for k := range meta {
		if strings.HasPrefix(k, "x402.") || k == KeyState {
			return true
		}
	}
	return false
}

// AttachmentURI names a stored attachment by the parameters of the control
// plane's GET /attachment route. A projection that does not carry file
// bytes inline gives this as the file part's url; the reader fetches the
// bytes from the control plane (or with `anet pull`).
func AttachmentURI(interactionID, cid string) string {
	return "anet:attachment?interaction_id=" + url.QueryEscape(interactionID) + "&cid=" + url.QueryEscape(cid)
}

// Source is what a projection reads: one stored interaction, its message
// log (oldest first) and its attachments' metadata, without bytes.
type Source struct {
	Interaction *interactions.Interaction
	Messages    []interactions.Message
	Attachments []interactions.Attachment
	// Payment is the x402 part of the task's current status as the kernel
	// derives it from the stored payment columns (the daemon's
	// PaymentStatusMeta, A2A-DESIGN §8.2): x402.payment.status, .required
	// and anet.quote_expires_at while a quote waits, .receipts, .error
	// after a failure, and on the requester's side anet.reason
	// (needs_operator_approval) and anet.cancel_requested. The payment
	// flow owns those rules; the projection places what it says.
	//
	// nil means nobody derived it (a reader with only the store): the
	// projection then reads the columns itself. Non-nil, even empty, it is
	// authoritative for anet.cancel_requested, and for the x402 keys
	// whenever it has any; a task with nothing quoted in the same-task flow
	// gives an empty map, and the x402 keys of an older PAYMENT_REQUIRED
	// answer are still read from the answer.
	Payment map[string]any
}

// Options says how much of a task to project and how its files travel.
type Options struct {
	// HistoryLength keeps the most recent n history messages; nil keeps
	// all of them and 0 none (A2A historyLength).
	HistoryLength *int
	// Artifacts includes the artifacts. GetTask asks for them; ListTasks
	// leaves them out unless includeArtifacts is set.
	Artifacts bool
	// InlineFiles carries attachment bytes in raw parts, read through
	// LoadFile. Otherwise a file part is a url part naming the attachment
	// (AttachmentURI), which keeps a task with large files small enough to
	// hand to a model. An attachment whose bytes cannot be read is given by
	// reference either way.
	InlineFiles bool
	LoadFile    func(cid string) ([]byte, error)
	// SafeName makes an attachment's name safe to save to disk (the
	// daemon's safeName). The peer chose the name; with no SafeName a file
	// part has no filename at all rather than the peer's.
	SafeName func(string) string
	// ProviderKEL is the provider key history the receipt was checked
	// against, base64, carried in anet.receipt so a holder can check it
	// again rather than take this node's word for it.
	ProviderKEL string
}

// Load reads what Project needs for one interaction.
func Load(st *interactions.Store, id string) (Source, error) {
	ix, err := st.Get(id)
	if err != nil {
		return Source{}, err
	}
	msgs, err := st.Messages(id)
	if err != nil {
		return Source{}, err
	}
	atts, err := st.Attachments(id)
	if err != nil {
		return Source{}, err
	}
	return Source{Interaction: ix, Messages: msgs, Attachments: atts}, nil
}

// ProjectStored loads one interaction and projects it. An absent
// interaction is ErrTaskNotFound. With InlineFiles and no LoadFile, the
// bytes are read from the same store.
func ProjectStored(st *interactions.Store, id string, opt Options) (Task, error) {
	src, err := Load(st, id)
	if errors.Is(err, interactions.ErrNotFound) {
		return Task{}, Errorf(ErrTaskNotFound, "%s", id)
	}
	if err != nil {
		return Task{}, err
	}
	if opt.InlineFiles && opt.LoadFile == nil {
		opt.LoadFile = func(cid string) ([]byte, error) {
			a, err := st.AttachmentData(id, cid)
			if err != nil {
				return nil, err
			}
			return a.Data, nil
		}
	}
	return Project(src, opt), nil
}

// Project maps one stored interaction to an A2A Task.
func Project(src Source, opt Options) Task {
	ix := src.Interaction
	if ix == nil {
		return Task{}
	}
	p := newProjector(src, opt)
	msg, why := p.statusMessage()
	t := Task{
		ID:        ix.ID,
		ContextID: p.contextID,
		Status:    TaskStatus{State: StateOf(ix.State), Message: msg, Timestamp: p.timestamp()},
		Metadata:  p.metadata(why),
	}
	t.History = p.history()
	if opt.Artifacts {
		t.Artifacts = p.artifacts()
	}
	return t
}

// capResult is the part of a capability deliverable the projection reads
// (the daemon's capabilityResult).
type capResult struct {
	Status  string          `json:"status"`
	Message string          `json:"message"`
	Payment json.RawMessage `json:"payment_required"`
	Paid    *struct {
		Transaction string `json:"transaction"`
		Amount      string `json:"amount"`
		Network     string `json:"network"`
		Receipt     string `json:"receipt"`
	} `json:"paid"`
}

type projector struct {
	ix        *interactions.Interaction
	msgs      []interactions.Message
	metas     []map[string]any // decoded metadata of msgs, by index
	bySeq     map[int64][]interactions.Attachment
	opt       Options
	contextID string
	// outbound is true when this node is the requester.
	outbound bool
	// cap is the capability deliverable, when there is one.
	cap *capResult
	// resultMeta is the metadata the result carried.
	resultMeta map[string]any
	// payment is Source.Payment.
	payment map[string]any
}

func newProjector(src Source, opt Options) *projector {
	ix := src.Interaction
	p := &projector{ix: ix, msgs: src.Messages, opt: opt, contextID: ix.ContextID,
		outbound: ix.Role == interactions.RoleOutbound, bySeq: map[int64][]interactions.Attachment{}}
	if p.contextID == "" {
		// A row written before context ids were kept. A2A requires one;
		// the task is its own context.
		p.contextID = ix.ID
	}
	p.metas = make([]map[string]any, len(src.Messages))
	for i, m := range src.Messages {
		p.metas[i] = decodeObject([]byte(m.Metadata))
	}
	for _, a := range src.Attachments {
		p.bySeq[a.MsgSeq] = append(p.bySeq[a.MsgSeq], a)
	}
	if ix.IsCapability && len(ix.Result) > 0 {
		var r capResult
		if json.Unmarshal(ix.Result, &r) == nil && r.Status != "" {
			p.cap = &r
		}
	}
	p.resultMeta = decodeObject([]byte(ix.ResultMeta))
	if src.Payment != nil {
		// Re-read as the projection's own JSON values: the quote in it is
		// the peer's, and is held to the same rules (finite numbers) as
		// anything read from the store.
		p.payment = make(map[string]any, len(src.Payment))
		for k, v := range src.Payment {
			b, err := json.Marshal(v)
			if err != nil {
				continue
			}
			if g, err := decodeJSON(b); err == nil && g != nil {
				p.payment[k] = g
			}
		}
	}
	return p
}

// x402Keys are the keys Source.Payment decides when it has any.
var x402Keys = []string{KeyX402Status, KeyX402Required, KeyX402Receipts, KeyX402Error, KeyQuoteExpiresAt}

// kernelPayment reports whether the kernel's payment derivation has x402
// keys to give: the task took part in the same-task flow.
func (p *projector) kernelPayment() bool {
	for _, k := range x402Keys {
		if _, ok := p.payment[k]; ok {
			return true
		}
	}
	return false
}

// fromProvider reports whether sender is the task's provider: the peer of
// an outbound task, this node on an inbound one.
func (p *projector) fromProvider(sender string) bool {
	return (sender == p.ix.PeerAID) == p.outbound
}

func (p *projector) role(sender string) Role {
	if p.fromProvider(sender) {
		return RoleAgent
	}
	return RoleUser
}

// msgID is a message's A2A id: the local client's own id when it gave one,
// otherwise the id both sides recorded it under (the envelope's message id
// in hex, 0017 Q9), otherwise (a row from before messages kept one) one
// derived from the row, stable across reads.
func (p *projector) msgID(i int) string {
	if id, ok := p.metas[i][KeyMessageID].(string); ok && id != "" {
		return id
	}
	if p.msgs[i].MsgID != "" {
		return p.msgs[i].MsgID
	}
	return p.ix.ID + ".m" + strconv.FormatInt(p.msgs[i].Seq, 10)
}

func (p *projector) timestamp() *time.Time {
	ms := p.ix.StateAt
	if ms <= 0 {
		if t, err := time.Parse(time.RFC3339Nano, p.ix.UpdatedAt); err == nil {
			ms = t.UnixMilli()
		}
	}
	if ms <= 0 {
		return nil
	}
	t := time.UnixMilli(ms).UTC()
	return &t
}

// message builds an agent or user message of this task.
func (p *projector) message(id string, role Role, parts []Part, meta map[string]any) *Message {
	return &Message{ID: id, ContextID: p.contextID, Metadata: meta, Parts: parts, Role: role, TaskID: p.ix.ID}
}

// synthesized is an agent message the projection writes itself (there is
// no stored row saying it). Its id is tied to the state write, so it is the
// same on every read until the state changes.
func (p *projector) synthesized(text string, meta map[string]any) *Message {
	id := p.ix.ID + ".status." + strconv.FormatInt(p.ix.StateSeq, 10)
	return p.message(id, RoleAgent, []Part{TextPart(text)}, meta)
}

// fromRow renders stored message i with its attachments.
func (p *projector) fromRow(i int) *Message {
	m := p.msgs[i]
	var parts []Part
	if m.Body != "" {
		parts = append(parts, TextPart(m.Body))
	}
	for _, a := range p.bySeq[m.Seq] {
		parts = append(parts, p.filePart(a))
	}
	if len(parts) == 0 {
		// A status carrying only metadata. A message needs a part.
		parts = []Part{TextPart(m.Body)}
	}
	return p.message(p.msgID(i), p.role(m.SenderAID), parts, p.metas[i])
}

// filePart is an attachment as a file part: inline bytes or a reference,
// the name made safe, the content id and size in the part's metadata.
func (p *projector) filePart(a interactions.Attachment) Part {
	part := Part{MediaType: a.Mime, Metadata: map[string]any{KeyCID: a.CID, KeySize: a.Size}}
	if p.opt.SafeName != nil {
		part.Filename = p.opt.SafeName(a.Name)
	}
	if p.opt.InlineFiles && p.opt.LoadFile != nil {
		if b, err := p.opt.LoadFile(a.CID); err == nil {
			part.Kind, part.Raw = PartRaw, b
			return part
		}
	}
	part.Kind, part.URL = PartURL, AttachmentURI(p.ix.ID, a.CID)
	return part
}

// conversational reports whether stored message i is a conversation turn.
func (p *projector) conversational(i int) bool {
	m := p.msgs[i]
	if m.Kind != interactions.MsgText || ControlMetadata(p.metas[i]) {
		return false
	}
	return m.Body != "" || len(p.bySeq[m.Seq]) > 0
}

// lastProvider is the index of the provider's latest message that can
// explain the task's status, or -1. A capability call has no conversation:
// the provider's text rows there are its deliverable, which the artifacts
// carry, so only status and payment messages count.
func (p *projector) lastProvider() int {
	for i := len(p.msgs) - 1; i >= 0; i-- {
		m := p.msgs[i]
		if !p.fromProvider(m.SenderAID) {
			continue
		}
		switch m.Kind {
		case interactions.MsgStatus, interactions.MsgPayment:
			return i
		case interactions.MsgText:
			if !p.ix.IsCapability {
				return i
			}
		}
	}
	return -1
}

// lastRequesterSeq is the seq of the requester's latest message of any
// kind, or 0.
func (p *projector) lastRequesterSeq() int64 {
	for i := len(p.msgs) - 1; i >= 0; i-- {
		if !p.fromProvider(p.msgs[i].SenderAID) {
			return p.msgs[i].Seq
		}
	}
	return 0
}

// requesterCanceled reports a cancel the requester sent.
func (p *projector) requesterCanceled() bool {
	for _, m := range p.msgs {
		if m.Kind == interactions.MsgCancel && !p.fromProvider(m.SenderAID) {
			return true
		}
	}
	return false
}

// paymentRequired reports a task that waits for a payment: quoted in the
// same-task flow (pay_state=required, or failed with the quote still open
// for another attempt), or answered PAYMENT_REQUIRED.
func (p *projector) paymentRequired() bool {
	switch p.ix.PayState {
	case interactions.PayRequired:
		return true
	case interactions.PayFailed:
		if !p.ix.IsTerminal() && len(p.ix.PayRequired) > 0 {
			return true
		}
	}
	return p.quoted()
}

// quoted reports a capability answer that is a quote (PAYMENT_REQUIRED).
// It is signed and stored like any answer, but it asks for input and
// delivers nothing: it is not the task's output, and its message is not why
// a task that later ended (canceled, say) ended.
func (p *projector) quoted() bool {
	return p.cap != nil && p.cap.Status == string(effect.PaymentRequired)
}

// statusMessage picks status.message for the current state. why is the
// metadata of the stored row it came from, from which the reason keys are
// lifted into the task's metadata; nil when the message was synthesized.
func (p *projector) statusMessage() (msg *Message, why map[string]any) {
	ix := p.ix
	prov := p.lastProvider()
	// A provider message explains the state only if the requester has not
	// spoken since: after that, the state is the requester's doing.
	fresh := prov >= 0 && p.msgs[prov].Seq > p.lastRequesterSeq()
	var provStatus bool
	if prov >= 0 {
		provStatus = p.msgs[prov].Kind == interactions.MsgStatus
	}
	switch ix.State {
	case interactions.StateSubmitted:
		// An approval queue's notice (anet.inbound=pending_approval).
		if fresh && provStatus {
			return p.fromRow(prov), p.metas[prov]
		}
	case interactions.StateWorking:
		if fresh && (provStatus || p.metas[prov][KeyState] == string(interactions.StateWorking)) {
			return p.fromRow(prov), p.metas[prov]
		}
	case interactions.StateInputRequired:
		if p.paymentRequired() {
			if fresh && p.metas[prov][KeyX402Status] != nil {
				return p.fromRow(prov), p.metas[prov]
			}
			return p.paymentRequiredMessage(), nil
		}
		if fresh {
			return p.fromRow(prov), p.metas[prov]
		}
	case interactions.StateCompleted:
		if rc := p.x402Receipts(); rc != nil {
			return p.synthesized("Payment completed.", map[string]any{
				KeyX402Status: PaymentCompleted, KeyX402Receipts: rc}), nil
		}
	case interactions.StateFailed, interactions.StateRejected, interactions.StateCanceled:
		switch {
		case p.cap != nil && !p.quoted() && p.cap.Message != "":
			msg = p.synthesized(p.cap.Message, nil)
		case fresh && provStatus:
			msg, why = p.fromRow(prov), p.metas[prov]
		case ix.State == interactions.StateFailed && !ix.IsCapability && len(ix.Receipt) == 0 &&
			len(ix.Result) > 0 && utf8.Valid(ix.Result):
			// A failure without a receipt stores the provider's detail
			// as the result.
			msg = p.synthesized(string(ix.Result), nil)
		}
		// a2a-x402: once a payment settled, the final message carries the
		// receipts (§8.2).
		if rc := p.x402Receipts(); rc != nil {
			if msg == nil {
				msg = p.synthesized("The task ended after a payment settled.", nil)
			}
			// A copy: a stored row's metadata is also why.
			meta := make(map[string]any, len(msg.Metadata)+2)
			for k, v := range msg.Metadata {
				meta[k] = v
			}
			msg.Metadata = meta
			if _, ok := msg.Metadata[KeyX402Receipts]; !ok {
				msg.Metadata[KeyX402Receipts] = rc
			}
			if _, ok := msg.Metadata[KeyX402Status]; !ok {
				msg.Metadata[KeyX402Status] = p.x402Status()
			}
			// A payment failure's code goes in the message as well as
			// the task's metadata (a2a-x402 §9).
			v := p.resultMeta[KeyX402Error]
			if v == nil {
				v = p.payment[KeyX402Error]
			}
			if v != nil {
				if _, ok := msg.Metadata[KeyX402Error]; !ok {
					msg.Metadata[KeyX402Error] = v
				}
			}
		}
		return msg, why
	}
	return nil, nil
}

// paymentRequiredMessage is the a2a-x402 payment-required message, built
// from the stored quote. After a failed attempt it says payment-failed,
// with the code and the receipts, and still carries the quote.
func (p *projector) paymentRequiredMessage() *Message {
	text := "Payment is required."
	if p.cap != nil && p.cap.Message != "" {
		text = p.cap.Message
	}
	meta := map[string]any{KeyX402Status: PaymentRequired}
	if p.kernelPayment() {
		for _, k := range x402Keys {
			if v, ok := p.payment[k]; ok {
				meta[k] = v
			}
		}
		if meta[KeyX402Status] == PaymentFailed {
			text = "The payment failed; the quote can be paid again."
		}
		return p.synthesized(text, meta)
	}
	if p.ix.PayState == interactions.PayFailed {
		meta[KeyX402Status] = PaymentFailed
		text = "The payment failed; the quote can be paid again."
		if rc := p.x402Receipts(); rc != nil {
			meta[KeyX402Receipts] = rc
		}
	}
	if req := p.x402Required(); req != nil {
		meta[KeyX402Required] = req
	}
	return p.synthesized(text, meta)
}

// x402Required is the stored quote: the same-task flow's pay_required, or
// the PaymentRequired a PAYMENT_REQUIRED answer carried.
func (p *projector) x402Required() any {
	for _, b := range [][]byte{p.ix.PayRequired, p.capPayment()} {
		if len(b) == 0 {
			continue
		}
		if v, err := decodeJSON(b); err == nil && v != nil {
			return v
		}
	}
	return nil
}

func (p *projector) capPayment() []byte {
	if p.cap == nil {
		return nil
	}
	return p.cap.Payment
}

// x402Status is x402.payment.status as the task's payment state says it.
func (p *projector) x402Status() string {
	switch p.ix.PayState {
	case interactions.PayRequired:
		return PaymentRequired
	case interactions.PaySubmitted:
		return PaymentSubmitted
	case interactions.PayCompleted:
		return PaymentCompleted
	case interactions.PayFailed:
		return PaymentFailed
	case interactions.PayRejected:
		return PaymentRejected
	}
	if p.cap != nil {
		if p.cap.Paid != nil {
			return PaymentCompleted
		}
		if p.cap.Status == string(effect.PaymentRequired) {
			return PaymentRequired
		}
	}
	return ""
}

// x402Receipts is x402.payment.receipts: the stored settlement responses,
// or the one settlement a paid capability answer names, as an x402
// SettlementResponse with the hub's receipt in its extensions.
func (p *projector) x402Receipts() []any {
	if len(p.ix.PayReceipts) > 0 {
		if v, err := decodeJSON(p.ix.PayReceipts); err == nil {
			if arr, ok := v.([]any); ok && len(arr) > 0 {
				return arr
			}
		}
	}
	if p.cap != nil && p.cap.Paid != nil {
		r := map[string]any{"success": true, "transaction": p.cap.Paid.Transaction, "network": p.cap.Paid.Network}
		if p.cap.Paid.Amount != "" {
			r["amount"] = p.cap.Paid.Amount
		}
		if p.cap.Paid.Receipt != "" {
			r["extensions"] = map[string]any{KeySettlementReceipt: p.cap.Paid.Receipt}
		}
		return []any{r}
	}
	return nil
}

// metadata assembles the task's metadata. why is the stored status row
// that explains the state, if one does.
func (p *projector) metadata(why map[string]any) map[string]any {
	ix := p.ix
	m := map[string]any{KeyPeerAID: ix.PeerAID, KeyRole: RoleProvider, KeyStateSeq: ix.StateSeq}
	if p.outbound {
		m[KeyRole] = RoleRequester
	}
	if ix.Trust != "" {
		m[KeyTrust] = ix.Trust
	}
	if ix.RequestCID != "" {
		m[KeyRequestCID] = ix.RequestCID
	}
	if ix.ResultCID != "" {
		m[KeyResultCID] = ix.ResultCID
	}
	if len(ix.Receipt) > 0 || ix.State == interactions.StateCompleted {
		m[KeyReceiptVerified] = receiptVerified(ix.ReceiptVerified)
	}
	// The reason keys come from what set the state: the result when there
	// is one, otherwise the provider's status message.
	for _, k := range []string{KeyReason, KeyRetryAfterMS, KeyA2AError, KeyInbound, KeyX402Error} {
		if v := p.resultMeta[k]; v != nil {
			m[k] = v
		} else if v := why[k]; v != nil {
			m[k] = v
		}
	}
	if ix.IsCapability {
		if s := p.skill(); s != "" {
			m[KeySkill] = s
		}
		es := p.effectStatus()
		if es != "" {
			m[KeyEffectStatus] = es
		}
		// §4.3: an UNAVAILABLE answer says either when to retry or why
		// not. One that said neither is given the generic reason.
		if es == string(effect.Unavailable) && ix.IsTerminal() && m[KeyReason] == nil && m[KeyRetryAfterMS] == nil {
			m[KeyReason] = ReasonUnavailable
		}
	}
	switch {
	case p.payment != nil:
		// The kernel's rule (0017 Q3): the requester's cancel sent while
		// its payment is submitted or settled.
		if v, ok := p.payment[KeyCancelRequested]; ok {
			m[KeyCancelRequested] = v
		}
	case !ix.IsTerminal() && p.requesterCanceled():
		// A cancel after a payment was submitted leaves the task open
		// (§4.2); the client is told its request is pending.
		m[KeyCancelRequested] = true
	}
	if p.kernelPayment() {
		// The same-task flow, as the payment flow reads its own columns.
		for _, k := range x402Keys {
			if v, ok := p.payment[k]; ok {
				m[k] = v
			}
		}
	} else {
		if s := p.x402Status(); s != "" {
			m[KeyX402Status] = s
		}
		if rc := p.x402Receipts(); rc != nil {
			m[KeyX402Receipts] = rc
		}
	}
	// Why a task waits on a payment (needs_operator_approval) is the
	// state's reason while it waits; once the task has ended, the reason
	// that ended it stays, and the payment's is only a fallback.
	if v, ok := p.payment[KeyReason]; ok {
		if !ix.IsTerminal() || m[KeyReason] == nil {
			m[KeyReason] = v
		}
	}
	return m
}

func receiptVerified(v interactions.Verification) string {
	switch v {
	case interactions.VerificationVerified:
		return ReceiptVerified
	case interactions.VerificationUnverified:
		return ReceiptUnverified
	}
	return ReceiptUnknown
}

// effectStatus is the capability's effect status: the deliverable's (it is
// what the receipt covers), else the result's metadata. A terminal task
// with neither — canceled before any answer, refused by the provider's
// policy, a result lost — still says something, because SI-6 does not let
// a terminal capability task be silent about its effect: a refusal means
// nothing was attempted (UNAVAILABLE), and anything else means this node
// does not know whether the effect happened (UNVERIFIED). Never OK.
func (p *projector) effectStatus() string {
	if p.cap != nil {
		return p.cap.Status
	}
	if s, ok := p.resultMeta[KeyEffectStatus].(string); ok && s != "" {
		return s
	}
	switch {
	case !p.ix.IsTerminal():
		return ""
	case p.ix.State == interactions.StateRejected:
		return string(effect.Unavailable)
	}
	return string(effect.Unverified)
}

// skill is the capability a capability task invokes.
func (p *projector) skill() string {
	if id, _, ok := capabilityRequest(p.ix.RequestDoc); ok {
		return id
	}
	return strings.TrimPrefix(p.ix.Goal, "invoke capability ")
}

// history is the conversation, oldest first, cut to HistoryLength.
func (p *projector) history() []Message {
	if n := p.opt.HistoryLength; n != nil && *n <= 0 {
		return nil
	}
	var out []Message
	if p.ix.IsCapability {
		out = p.capabilityHistory()
	} else {
		for i := range p.msgs {
			if p.conversational(i) {
				out = append(out, *p.fromRow(i))
			}
		}
	}
	if n := p.opt.HistoryLength; n != nil && len(out) > *n {
		out = out[len(out)-*n:]
	}
	return out
}

// capabilityHistory is a capability call's one request, as the DataPart
// {skill, args} a client sends to ask for one (§11.5). The request is read
// from the signed TaskDoc, which both sides keep; the opening row's id is
// used when there is one.
func (p *projector) capabilityHistory() []Message {
	capID, args, ok := capabilityRequest(p.ix.RequestDoc)
	if !ok {
		var out []Message
		for i, m := range p.msgs {
			if !p.fromProvider(m.SenderAID) && p.conversational(i) {
				out = append(out, *p.fromRow(i))
			}
		}
		return out
	}
	id := p.ix.ID + ".request"
	for i, m := range p.msgs {
		if m.Kind == interactions.MsgText && !p.fromProvider(m.SenderAID) {
			id = p.msgID(i)
			break
		}
	}
	data := map[string]any{"skill": capID, "args": args}
	return []Message{*p.message(id, RoleUser, []Part{DataPart(data)}, nil)}
}

// requireTypeCapability marks a TaskDoc requirement as a capability call
// (the daemon's RequireTypeCapability; the TaskDoc convention is in
// docs/CONTRACTS-zh.md).
const requireTypeCapability = "capability"

// capabilityRequest reads the capability id and arguments from a stored
// request TaskDoc. The signature is not checked; it was checked when the
// delegation was made or accepted.
func capabilityRequest(doc []byte) (capID string, args any, ok bool) {
	if len(doc) == 0 {
		return "", nil, false
	}
	var td tsir.TaskDoc
	if err := coredet.Unmarshal(doc, &td); err != nil || len(td.Tasks) == 0 {
		return "", nil, false
	}
	t := td.Tasks[0]
	for _, r := range t.Requires {
		if r.Type == requireTypeCapability && r.ID != "" {
			capID = r.ID
			break
		}
	}
	if capID == "" {
		return "", nil, false
	}
	args = map[string]any{}
	for _, c := range t.Contexts {
		if c.Key == "args" && c.Value != "" {
			if v, err := decodeJSON([]byte(c.Value)); err == nil {
				args = v
			}
			break
		}
	}
	return capID, args, true
}

// artifacts are the task's outputs. Nothing is an output until a receipt
// covers it: the receipt's result CID is what binds the deliverable to the
// provider, and a result without one — a failure detail, a result that did
// not verify — is reported in the status, not offered as the work.
func (p *projector) artifacts() []Artifact {
	ix := p.ix
	if len(ix.Receipt) == 0 || p.quoted() {
		// A quote's receipt covers the quote; the quote is in the status
		// message, and a stream must not hand it over as the result.
		return nil
	}
	var out []Artifact
	var files []interactions.Attachment
	if ix.IsCapability {
		if v, err := decodeJSON(ix.Result); err == nil && v != nil {
			name := p.skill()
			if name == "" {
				name = ArtifactResult
			}
			out = append(out, Artifact{ID: ArtifactResult, Name: name, Parts: []Part{DataPart(v)}})
		}
	} else if tr, err := transcript.Parse(ix.Result); err == nil {
		for i := len(tr.Messages) - 1; i >= 0; i-- {
			if m := tr.Messages[i]; m.From == "provider" {
				out = append(out, Artifact{ID: ArtifactReply, Name: ArtifactReply, Parts: []Part{TextPart(m.Body)}})
				for _, a := range m.Attachments {
					files = append(files, interactions.Attachment{Name: a.Name, Mime: a.Mime, Size: a.Size, CID: a.CID})
				}
				break
			}
		}
	}
	out = append(out, Artifact{ID: ArtifactReceipt, Name: ArtifactReceipt, Parts: []Part{DataPart(p.receiptData())}})
	for i, a := range files {
		part := p.filePart(a)
		name := part.Filename
		if name == "" {
			name = "attachment"
		}
		out = append(out, Artifact{ID: ArtifactAttachmentPrefix + strconv.Itoa(i), Name: name, Parts: []Part{part}})
	}
	return out
}

// receiptData is the anet.receipt artifact: the signed receipt as stored,
// its fields decoded for a reader that does not parse CoreDet-CBOR, and
// whether this node could check it.
func (p *projector) receiptData() map[string]any {
	ix := p.ix
	d := map[string]any{
		"receipt":  base64.StdEncoding.EncodeToString(ix.Receipt),
		"verified": receiptVerified(ix.ReceiptVerified),
	}
	if rc, err := evidence.UnmarshalReceipt(ix.Receipt); err == nil {
		d["interaction_id"] = rc.InteractionID
		d["requester_aid"] = rc.RequesterAID
		d["provider_aid"] = rc.ProviderAID
		d["request_cid"] = rc.RequestCID
		d["result_cid"] = rc.ResultCID
		d["completed_at"] = rc.CompletedAt
		if cid, err := rc.CID(); err == nil {
			d["receipt_cid"] = cid
		}
	}
	if p.opt.ProviderKEL != "" {
		d["provider_kel"] = p.opt.ProviderKEL
	}
	return d
}

// String is for diagnostics.
func (k PartKind) String() string {
	switch k {
	case PartText:
		return "text"
	case PartRaw:
		return "raw"
	case PartURL:
		return "url"
	case PartData:
		return "data"
	}
	return fmt.Sprintf("PartKind(%d)", uint8(k))
}
