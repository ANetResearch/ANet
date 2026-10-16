package daemon

// previewseam.go is the kernel's side of stream_preview (11-modularity
// §3.2, K-9, K-10; MOD-5): what the kernel keeps whatever implements the
// previews, and the adapter that is the whole of what that implementation
// is handed.
//
// The kernel judges the facts (CUX-P4 for a frame to send, CUX-P14 items 1
// to 3 for a frame that arrived), keeps the small table of the rounds a
// requester asked previews for, builds and validates the frame, sends it
// through the one-shot envelope, and drops what arrives while previews are
// not available. The Previewer (preview.go) gets the cadence, the
// stream_seq marks and the cache, and reaches the node only through
// previewSeam: it cannot read the store, the outbox or the evidence chain,
// and the frame it hands over is checked here again.

import (
	"container/list"
	"context"
	"encoding/hex"
	"sync"

	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/module"
)

// cntPreviewUnsupported counts a frame that arrived while previews are not
// available on this node (11-modularity §5.4): dropped at step 9 with no
// word to the sender, and nothing written.
const cntPreviewUnsupported = "preview-unsupported"

// previewAvailable reports whether this node has previews: the preview
// module is compiled in and not named in "without". It is the one place
// the kernel asks (Availability.Available).
func (d *Daemon) previewAvailable() bool { return d.avail.Available("preview") }

// previews is this node's Previewer.
func (d *Daemon) previews() module.Previewer {
	if p := d.previewer.Load(); p != nil && *p != nil {
		return *p
	}
	return previewsUnavailable{}
}

// setPreviewer makes c this node's Previewer; nil leaves previews
// unavailable. It may be called while the daemon runs.
func (d *Daemon) setPreviewer(c module.Previewer) {
	if c == nil {
		d.previewer.Store(nil)
		return
	}
	d.previewer.Store(&c)
}

// startPreviews gives the daemon its Previewer and runs it until the
// daemon stops. A node without the preview module has none: previews()
// then answers previewsUnavailable, no round is recorded and no frame is
// taken.
func (d *Daemon) startPreviews(ctx context.Context) {
	if !d.previewAvailable() {
		return
	}
	co := d.newPreviewer()
	d.setPreviewer(co)
	d.goBackground(func() { co.run(ctx, func(f func()) { d.goBackground(f) }) })
}

// newPreviewer is the implementation, wired to the node through the seam
// and nothing else.
func (d *Daemon) newPreviewer() *memPreviews {
	seam := previewSeam{d}
	co := newMemPreviews(d.nowMS, func(o previewOut) { d.handOverPreview(seam, o) }, d.count)
	co.mode, co.published = seam.Mode, seam.Published
	return co
}

// handOverPreview is the implementation's hand-over of a frame: through
// the seam, within the time one frame may take.
func (d *Daemon) handOverPreview(seam module.PreviewSeam, o previewOut) {
	if o.live != nil && !o.live() {
		return // the round was answered while the frame was being made
	}
	ctx, cancel := context.WithTimeout(d.ctx, previewSendTimeout)
	defer cancel()
	seam.SendFrame(ctx, o.IX, module.PreviewOut{Answers: o.Answers, Seq: o.Seq, PreviewContent: o.previewContent})
}

// sendPreviewFrame is handOverPreview on this node's own seam.
func (d *Daemon) sendPreviewFrame(o previewOut) { d.handOverPreview(previewSeam{d}, o) }

// previewSeam is the module.PreviewSeam of this node (K-9).
type previewSeam struct{ d *Daemon }

var _ module.PreviewSeam = previewSeam{}

// Mode implements module.PreviewSeam: this node's stream_preview, "off"
// while previews are not available.
func (s previewSeam) Mode() string {
	if !s.d.previewAvailable() {
		return streamPreviewOff
	}
	return s.d.config().streamPreview()
}

// Published implements module.PreviewSeam: task ix has a new preview to
// show. The event carries no text; a subscriber reads it with Current.
func (s previewSeam) Published(ix string) {
	if !s.d.previewAvailable() {
		return
	}
	s.d.bus.publish(Event{IX: ix, Kind: EventPreview, At: s.d.nowMS()})
}

// SendFrame implements module.PreviewSeam, and is where a frame is held to
// CUX-P4 whatever made it: the node's stream_preview is not off; the task
// is an inbound text task of this node's, not a capability call, not ended
// and without a result; the frame answers the round the requester opened
// (the kernel's table, not the caller's word), and that round was asked
// for previews, was not answered and is not canceled; the requester's
// message it answers is the latest text of the task as the store has it;
// the reasoning goes only when this node and the round both asked for it;
// the frame passes ValidatePreview. Then it goes once through oneShotSend
// (ENG-K18), which keeps it off the outbox and sends nothing without the
// peer's rk or while the gate is closed. Nothing it does is written.
func (s previewSeam) SendFrame(ctx context.Context, ixid string, o module.PreviewOut) module.PreviewSendResult {
	d := s.d
	if !d.previewAvailable() {
		return module.PreviewSendResult{}
	}
	node := normalPreviewMode(d.config().streamPreview())
	if node == "" {
		return module.PreviewSendResult{}
	}
	mode, ok := d.previewRounds.answered(ixid, o.Answers)
	if !ok {
		return module.PreviewSendResult{}
	}
	ix, err := d.ix.Get(ixid)
	if err != nil || ix.Role != interactions.RoleInbound || ix.IsCapability || ix.Trust == interactions.TrustPublicCap ||
		ix.IsTerminal() || len(ix.Result) > 0 {
		return module.PreviewSendResult{}
	}
	if _, ok := d.roundMessage(ix, o.Answers); !ok {
		return module.PreviewSendResult{}
	}
	cm, ok := previewChatMsg(previewOut{IX: ixid, Answers: o.Answers, Mode: mode, NodeMode: node, Seq: o.Seq,
		previewContent: o.PreviewContent})
	if !ok {
		// An answers over 256 bytes, or a frame this code got wrong: no
		// preview (CUX §2.3); the round's answer goes as ever.
		d.count(cntPreviewDropped)
		return module.PreviewSendResult{}
	}
	body, err := cm.Marshal()
	if err != nil {
		d.count(cntPreviewDropped)
		return module.PreviewSendResult{}
	}
	res := d.oneShotSend(ctx, oneShotPreview, ix.PeerAID, ix.ID, body)
	return module.PreviewSendResult{Sent: true, Handed: res.Handed, HubReason: res.HubReason}
}

// grantPreviewSeam is the Host's grant of the seam (module.PreviewHost): a
// seam while previews are available, none otherwise.
func (d *Daemon) grantPreviewSeam() (module.PreviewSeam, bool) {
	if !d.previewAvailable() {
		return nil, false
	}
	return previewSeam{d}, true
}

// previewPart is the PreviewHost grant of a module Host.
type previewPart struct{ d *Daemon }

// PreviewSeam implements module.PreviewHost.
func (p previewPart) PreviewSeam() (module.PreviewSeam, bool) { return p.d.grantPreviewSeam() }

// previewChatMsg is the frame o makes (CUX-P1, P4, P9): the reasoning only
// when both this node and the round asked for it, a fresh random msg_id,
// and nothing that does not pass ValidatePreview. ok is false when there
// is no frame to send.
func previewChatMsg(o previewOut) (*delegation.ChatMsg, bool) {
	if normalPreviewMode(o.NodeMode) == "" || normalPreviewMode(o.Mode) == "" {
		return nil, false
	}
	if o.NodeMode != previewModeWithReasoning || o.Mode != previewModeWithReasoning {
		o.Reasoning, o.ReasoningOmitted, o.ReasoningAt = "", 0, 0
	}
	cm := &delegation.ChatMsg{Kind: delegation.ChatStreamPreview, Body: o.Text, StreamSeq: o.Seq,
		StreamAtMS: int64(o.TextAt), ReasoningBody: o.Reasoning, MsgID: hex.EncodeToString(seal.NewMID()),
		Omitted: o.Omitted, ReasoningOmitted: o.ReasoningOmitted, Answers: o.Answers}
	if o.Reasoning != "" {
		cm.ReasoningStreamAtMS = int64(o.ReasoningAt)
	}
	if delegation.ValidatePreview(cm) != nil {
		return nil, false
	}
	return cm, true
}

// notePreviewRound is CUX-P0 on the provider, after step 10 took m: a
// delegation, or a requester's text message on a task of this node's,
// opens a round, and its anet.preview_mode says whether the round is
// previewed. The mode is normalized here (M36): a value other than "on" or
// "with_reasoning" is no request. A payment is not a round: the round it
// pays for goes on. Nothing is recorded while previews are not available.
func (d *Daemon) notePreviewRound(m *rxMsg) {
	if !d.previewAvailable() {
		return
	}
	var answers string
	var meta []byte
	switch {
	case m.typ == seal.TypeDelegate && m.dr != nil:
		answers, meta = wireMsgID("", m.mid), m.dr.Metadata
	case m.typ == seal.TypeMessage && m.cm != nil && m.cm.Kind == delegation.ChatText && m.existing != nil:
		ix := m.existing
		if ix.Role != interactions.RoleInbound || ix.PeerAID != m.from || ix.IsCapability || hasPaymentStatus(m.cm.Metadata) {
			return
		}
		answers, meta = wireMsgID(m.cm.MsgID, m.mid), m.cm.Metadata
	default:
		return
	}
	mode := previewModeOf(meta)
	if !d.previewRounds.requested(m.ix, answers, mode) {
		return
	}
	d.previews().Requested(m.ix, answers, mode)
}

// cancelPreviewRound is ENG-K20: a real answer or result of task ix's
// round was queued, and no frame of that round goes after it.
func (d *Daemon) cancelPreviewRound(ix string) {
	d.previewRounds.cancel(ix)
	d.previews().CancelRound(ix)
}

// roundMessage is the message of task ix's current round as the store has
// it, when its id is answers (CUX-P4 items 3–4, CUX-P14 items 2–3): the
// latest text message is the asking side's — the requester's — and has
// that id. A text message of the answering side's after it means the round
// was answered.
func (d *Daemon) roundMessage(ix *interactions.Interaction, answers string) (interactions.Message, bool) {
	asker := d.AID()
	if ix.Role == interactions.RoleInbound {
		asker = ix.PeerAID
	}
	msgs, err := d.ix.Messages(ix.ID)
	if err != nil {
		return interactions.Message{}, false
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Kind != interactions.MsgText {
			continue
		}
		if m.SenderAID != asker {
			return interactions.Message{}, false // answered since
		}
		return m, m.MsgID == answers
	}
	return interactions.Message{}, false
}

// previewArrived is receive step 9 for a stream_preview frame (engine §4.5;
// CUX-P14): while previews are not available the frame is dropped and
// counted (preview-unsupported), and nothing else happens — the sender
// cannot tell it from a node that has previews off (MOD-F4). Otherwise the
// frame is taken only for a task this node delegated to the sender, a text
// task not ended, whose latest message of this node's is the one the frame
// answers, asked for previews, and was not answered since; the facts come
// first, and only then does the Previewer judge its stream_seq and the
// rates. A frame taken is kept in memory and published (EventPreview);
// nothing is written to the store. It reports whether the frame was taken;
// one that was not is dropped without a word (preview-rejected).
func (d *Daemon) previewArrived(m *rxMsg) bool {
	if !d.previewAvailable() {
		d.count(cntPreviewUnsupported)
		return false
	}
	co := d.previews()
	epoch := co.Epoch()
	cm, ix := m.cm, m.existing
	if cm == nil || delegation.ValidatePreview(cm) != nil {
		return false // CUX-P2
	}
	// Item 1.
	if ix == nil || ix.Role != interactions.RoleOutbound || ix.PeerAID != m.from || ix.IsTerminal() || ix.IsCapability ||
		len(ix.Result) > 0 {
		return false
	}
	// Items 2 and 3.
	asked, ok := d.roundMessage(ix, cm.Answers)
	if !ok {
		return false
	}
	mode := previewModeOf([]byte(asked.Metadata))
	if mode == "" {
		return false
	}
	f := previewFrame{Answers: cm.Answers, Seq: cm.StreamSeq, At: d.nowMS(), PreviewContent: previewContent{
		Text: cm.Body, Omitted: cm.Omitted, TextAt: uint64(max(cm.StreamAtMS, 0))}}
	if mode == previewModeWithReasoning {
		// Reasoning only for a round that asked for it, whatever the
		// provider sent (CUX-P4, P17).
		f.Reasoning, f.ReasoningOmitted, f.ReasoningAt = cm.ReasoningBody, cm.ReasoningOmitted, uint64(max(cm.ReasoningStreamAtMS, 0))
	}
	// Items 4 and 5; the Previewer tells the task it has a preview.
	return co.Arrived(previewIn{IX: m.ix, From: m.from, PreviewSnapshot: f, Epoch: epoch})
}

// clearPreview is CUX-P16: task ix stored a message or a result, or ended,
// so the preview it showed is gone. Called where the event of that write
// is published.
func (d *Daemon) clearPreview(ix string) { d.previews().Clear(ix) }

// previewRounds is the kernel's table of requested rounds (11-modularity
// §3.2): task → the message that opened the round, the mode it asked for
// and whether its real answer was queued. It is written only while
// previews are available, bounded like the Previewer's own, and is what a
// frame to send is held to — the implementation's word about the round is
// never taken.
type previewRoundTable struct {
	mu  sync.Mutex
	m   map[string]*requestedRound
	lru *list.List
}

type requestedRound struct {
	ix, answers, mode string
	canceled          bool
	elem              *list.Element
}

// requested records the round a requester's message opened on ix (CUX-P0).
// It reports whether the round is new: a copy of the same message changes
// nothing, and a message asking for no preview on a task with no round
// records nothing.
func (t *previewRoundTable) requested(ix, answers, mode string) bool {
	mode = normalPreviewMode(mode)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m, t.lru = map[string]*requestedRound{}, list.New()
	}
	r := t.m[ix]
	if r == nil {
		if mode == "" {
			return false
		}
		r = &requestedRound{ix: ix}
		r.elem = t.lru.PushFront(r)
		t.m[ix] = r
		for t.lru.Len() > previewCacheTasks {
			old := t.lru.Remove(t.lru.Back()).(*requestedRound)
			delete(t.m, old.ix)
		}
	} else {
		t.lru.MoveToFront(r.elem)
	}
	if r.answers == answers {
		return false
	}
	r.answers, r.mode, r.canceled = answers, mode, false
	return true
}

// wanted is the mode of ix's current round when it asked for previews and
// was not answered.
func (t *previewRoundTable) wanted(ix string) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r := t.m[ix]
	if r == nil || r.mode == "" || r.canceled {
		return "", false
	}
	return r.mode, true
}

// answered is the mode of ix's current round when it is the round answers
// opened, asked for previews and was not answered.
func (t *previewRoundTable) answered(ix, answers string) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r := t.m[ix]
	if r == nil || r.mode == "" || r.canceled || r.answers != answers {
		return "", false
	}
	return r.mode, true
}

// cancel marks ix's round answered.
func (t *previewRoundTable) cancel(ix string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if r := t.m[ix]; r != nil {
		r.canceled = true
	}
}
