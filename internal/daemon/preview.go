package daemon

// preview.go is the stream_preview kernel (correctness-ux §2.3, §4.3.6,
// §4.3.7; engine §2.11.5, §4.5): the provider's coalescer and the
// requester's preview cache, both in memory only, and the kernel's two
// ends of them — the frame a coalescer hands over (sendPreviewFrame) and
// receive step 9 for a frame (previewArrived).
//
// The rest of the daemon reaches it only through previewCoalescer
// (d.previews()) and previewArrived. memPreviews, the coalescer and the
// cache, reads neither the store nor d.cfg: what it needs is passed in
// (the clock, the hand-over, the counter), so that it can become a module
// of its own later (11-modularity §3.2, MOD-5).
//
// Nothing here is written to interactions.db, the outbox, the replay table
// or the evidence chain, and no preview text reaches a log (CUX-P15,
// SI-73). A restart forgets all of it: the round a provider was previewing
// sends no more frames, its result still goes, and the next round can be
// previewed again (CUX-P0); stream_seq is taken from
// the clock so the frames after a restart are not mistaken for old ones
// (CUX-P9a).

import (
	"container/list"
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// The preview modes (core §2.13, CUX-P0): what a requester's message asks
// for in anet.preview_mode, and what this node's stream_preview allows.
const (
	previewModeOn            = "on"
	previewModeWithReasoning = "with_reasoning"
	// metaPreviewMode is the end-to-end metadata key of a round's message.
	metaPreviewMode = "anet.preview_mode"
)

// The cadence and the bounds of previews (engine §0.4, CUX-P6, CUX-P14,
// CUX-P15). Code constants: there is nothing for an operator to tune.
const (
	// previewGapMS: two frames of one task are at least this far apart,
	// and a change is sent at most this long after it was offered.
	previewGapMS uint64 = 5000
	// previewNodePerSecond: the frames this node hands over in any second.
	previewNodePerSecond = 20
	// previewRxTaskGapMS: a requester takes one frame of a task per this.
	previewRxTaskGapMS uint64 = 2000
	// previewRxPeerPerSecond: a requester takes this many frames of one
	// peer in any second.
	previewRxPeerPerSecond = 10
	// previewCacheTasks bounds the requester's cache and the provider's
	// rounds alike.
	previewCacheTasks = 1024
	// previewPeersKept bounds the per-peer windows the requester keeps.
	previewPeersKept = 4096
	// previewClearsKept bounds the clears remembered for a frame that was
	// being judged while its task moved on.
	previewClearsKept = 4096
	// previewSendTimeout bounds one frame's hand-over.
	previewSendTimeout = 30 * time.Second
)

// cntPreviewDropped counts a frame this node did not send: over the node's
// frames per second, or one that would not pass ValidatePreview (CUX-P6,
// CUX-P2).
const cntPreviewDropped = "preview-dropped"

// EventPreview is a preview frame taken for a task this node delegated
// (CUX-P15): the event carries it, and Current has it until the task moves
// on. It changes nothing stored.
const EventPreview = "preview"

// previewCoalescer is how the kernel reaches stream_preview (UX-8;
// engine ENG-G3, §2.11.5): the provider's coalescer and the requester's
// preview cache. The provider's frames go out only through oneShotSend
// (ENG-K18), and a round's frames stop once its real answer or result is
// queued (ENG-K20).
type previewCoalescer interface {
	// Requested records the round a requester's message opened on task ix
	// (CUX-P0): answers is the message's id, mode its anet.preview_mode as
	// previewModeOf read it ("" when it asked for none). A copy of the same
	// message changes nothing.
	Requested(ix, answers, mode string)
	// Wanted reports whether the current round of ix asked for previews
	// and has not been answered: the round's mode.
	Wanted(ix string) (mode string, ok bool)
	// Offer gives the coalescer what the provider has written so far in
	// the round (the whole text and reasoning, not a delta); nodeMode is
	// this node's stream_preview. It reports whether the round takes it.
	Offer(ix, text, reasoning, nodeMode string) bool
	// CancelRound drops the frames of task ix not yet handed over: a real
	// answer or result of the round was queued in the same call, and no
	// frame of that round goes after it (ENG-K20, CUX-P8).
	CancelRound(ix string)

	// Epoch is read before a frame's facts are judged and passed to
	// Arrived, which refuses the frame when its task was cleared since.
	Epoch() uint64
	// Arrived keeps a frame whose facts the kernel has judged (CUX-P14
	// items 1–3) when items 4–5 allow it: a stream_seq above the task's,
	// and within the task's and the peer's rates.
	Arrived(in previewIn) bool
	// Clear forgets the preview of task ix: a message or a result was
	// stored on it, or it ended (CUX-P16).
	Clear(ix string)
	// Current is the preview task ix shows now.
	Current(ix string) (previewFrame, bool)
}

// previewsUnavailable is the coalescer while previews are not available on
// this node — the absence behavior of MOD-P5, and not a stub: no round is
// recorded, nothing is offered or sent, and every frame that arrives is
// dropped (previewArrived counts it preview-rejected). It is what
// d.previews() answers when no coalescer is set.
type previewsUnavailable struct{}

func (previewsUnavailable) Requested(string, string, string)          {}
func (previewsUnavailable) Wanted(string) (string, bool)              { return "", false }
func (previewsUnavailable) Offer(string, string, string, string) bool { return false }
func (previewsUnavailable) CancelRound(string)                        {}
func (previewsUnavailable) Epoch() uint64                             { return 0 }
func (previewsUnavailable) Arrived(previewIn) bool                    { return false }
func (previewsUnavailable) Clear(string)                              {}
func (previewsUnavailable) Current(string) (previewFrame, bool)       { return previewFrame{}, false }

// previews is this node's preview coalescer.
func (d *Daemon) previews() previewCoalescer {
	if p := d.previewer.Load(); p != nil && *p != nil {
		return *p
	}
	return previewsUnavailable{}
}

// setPreviewer makes c this node's preview coalescer; nil leaves previews
// unavailable. It may be called while the daemon runs.
func (d *Daemon) setPreviewer(c previewCoalescer) {
	if c == nil {
		d.previewer.Store(nil)
		return
	}
	d.previewer.Store(&c)
}

// startPreviews gives the daemon its coalescer and runs it until the
// daemon stops.
func (d *Daemon) startPreviews(ctx context.Context) {
	co := newMemPreviews(d.nowMS, d.sendPreviewFrame, d.count)
	d.setPreviewer(co)
	d.goBackground(func() { co.run(ctx, func(f func()) { d.goBackground(f) }) })
}

// previewModeOf is CUX-P0: the anet.preview_mode a requester's message
// metadata carries, read before anet.* is stripped for storage. A value
// other than "on" or "with_reasoning" is no request — not an error, and not
// counted as malformed — so that a later mode does not break this one.
func previewModeOf(meta []byte) string {
	if len(meta) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(meta, &m) != nil {
		return ""
	}
	var v string
	if json.Unmarshal(m[metaPreviewMode], &v) != nil {
		return ""
	}
	return normalPreviewMode(v)
}

// normalPreviewMode is v when it is a mode that sends previews, else "".
func normalPreviewMode(v string) string {
	switch v {
	case previewModeOn, previewModeWithReasoning:
		return v
	}
	return ""
}

// notePreviewRound is CUX-P0 on the provider, after step 10 took m: a
// delegation, or a requester's text message on a task of this node's,
// opens a round, and its anet.preview_mode says whether the round is
// previewed. A payment is not a round: the round it pays for goes on.
func (d *Daemon) notePreviewRound(m *rxMsg) {
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
	d.previews().Requested(m.ix, answers, previewModeOf(meta))
}

// previewOut is one frame a coalescer hands to the kernel: the round it
// belongs to and what the frame says. live reports whether the round is
// still the one the frame was made for (nil: always).
type previewOut struct {
	IX       string
	Answers  string
	Mode     string // the round's anet.preview_mode
	NodeMode string // this node's stream_preview when the text was offered
	Seq      uint64
	previewContent
	live func() bool
}

// previewContent is what a frame shows: the tails of the text and of the
// reasoning, the bytes before each, and when each was taken (unix ms).
type previewContent struct {
	Text             string
	Omitted          uint64
	TextAt           uint64
	Reasoning        string
	ReasoningOmitted uint64
	ReasoningAt      uint64
}

// same reports whether c and o show the same thing (their times aside).
func (c previewContent) same(o previewContent) bool {
	return c.Text == o.Text && c.Omitted == o.Omitted && c.Reasoning == o.Reasoning && c.ReasoningOmitted == o.ReasoningOmitted
}

// empty reports whether c shows nothing.
func (c previewContent) empty() bool { return c.Text == "" && c.Reasoning == "" }

// previewTail is s as a frame carries it (CUX-P1): valid UTF-8, its last
// MaxPreviewTail bytes at most, cut on a code point boundary, and the
// number of bytes before them.
func previewTail(s string) (string, uint64) {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "\uFFFD")
	}
	if len(s) <= delegation.MaxPreviewTail {
		return s, 0
	}
	start := len(s) - delegation.MaxPreviewTail
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:], uint64(start)
}

// previewFrame is a frame a requester took (CUX-P15): what the task's
// preview shows until the task moves on.
type previewFrame struct {
	Answers string
	Seq     uint64
	previewContent
	// At is when the frame was taken here (unix ms).
	At uint64
}

// previewIn is a frame as receive step 9 hands it to the cache, after the
// kernel judged CUX-P14 items 1–3.
type previewIn struct {
	IX, From string
	previewFrame
	// Epoch is what Epoch said before the facts were read.
	Epoch uint64
}

// sendPreviewFrame is the kernel's hand-over of a frame (CUX-P4, P7a, P9;
// ENG-K18 to K21): the facts of CUX-P4 are read from the store again — an
// inbound text task, not ended, whose latest requester message is the one
// the frame answers and that this node has not answered since — the frame
// is made by previewChatMsg, and it goes once through oneShotSend, which
// keeps it off the outbox and sends nothing without the peer's rk or while
// the gate is closed. Nothing it does is written.
func (d *Daemon) sendPreviewFrame(o previewOut) {
	ix, err := d.ix.Get(o.IX)
	if err != nil || ix.Role != interactions.RoleInbound || ix.IsCapability || ix.Trust == interactions.TrustPublicCap ||
		ix.IsTerminal() || len(ix.Result) > 0 {
		return
	}
	if _, ok := d.roundMessage(ix, o.Answers); !ok {
		return
	}
	cm, ok := previewChatMsg(o)
	if !ok {
		// An answers over 256 bytes, or a frame this code got wrong: no
		// preview (CUX §2.3); the round's answer goes as ever.
		d.count(cntPreviewDropped)
		return
	}
	body, err := cm.Marshal()
	if err != nil {
		d.count(cntPreviewDropped)
		return
	}
	if o.live != nil && !o.live() {
		return // the round was answered while the frame was being made
	}
	ctx, cancel := context.WithTimeout(d.ctx, previewSendTimeout)
	defer cancel()
	d.oneShotSend(ctx, oneShotPreview, ix.PeerAID, ix.ID, body)
}

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
// CUX-P14): the frame is taken only for a task this node delegated to the
// sender, a text task not ended, whose latest message of this node's is
// the one the frame answers, asked for previews, and was not answered
// since; then the cache judges its stream_seq and the rates. A frame taken
// is kept in memory and published (EventPreview); nothing is written to the
// store. It reports whether the frame was taken; one that was not is
// dropped without a word (preview-rejected).
func (d *Daemon) previewArrived(m *rxMsg) bool {
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
	now := d.nowMS()
	f := previewFrame{Answers: cm.Answers, Seq: cm.StreamSeq, At: now, previewContent: previewContent{
		Text: cm.Body, Omitted: cm.Omitted, TextAt: uint64(max(cm.StreamAtMS, 0))}}
	if mode == previewModeWithReasoning {
		// Reasoning only for a round that asked for it, whatever the
		// provider sent (CUX-P4, P17).
		f.Reasoning, f.ReasoningOmitted, f.ReasoningAt = cm.ReasoningBody, cm.ReasoningOmitted, uint64(max(cm.ReasoningStreamAtMS, 0))
	}
	// Items 4 and 5.
	if !co.Arrived(previewIn{IX: m.ix, From: m.from, previewFrame: f, Epoch: epoch}) {
		return false
	}
	d.bus.publish(Event{IX: m.ix, Kind: EventPreview, At: now, preview: &f})
	return true
}

// clearPreview is CUX-P16: task ix stored a message or a result, or ended,
// so the preview it showed is gone. Called where the event of that write
// is published.
func (d *Daemon) clearPreview(ix string) { d.previews().Clear(ix) }

// memPreviews is this node's coalescer and preview cache, in memory. It
// reads no store and no configuration: now, send and count are the
// kernel's.
type memPreviews struct {
	now   func() uint64
	send  func(previewOut)
	count func(string)
	wake  chan struct{}

	mu sync.Mutex

	// The provider's rounds, most recently touched first.
	rounds   map[string]*previewRound
	roundLRU *list.List
	roundSeq uint64
	// sent is the times of this node's latest frames, oldest at sentAt.
	sent   [previewNodePerSecond]uint64
	sentN  int
	sentAt int

	// The requester's cache, most recently taken first.
	seen    map[string]*previewSeen
	seenLRU *list.List
	peers   map[string]*previewWindow
	// epoch counts clears; clears is the epoch of each task's latest clear
	// that is still remembered, clearRing their order, and clearFloor the
	// latest epoch forgotten.
	epoch      uint64
	clears     map[string]uint64
	clearRing  []previewClear
	clearFloor uint64
}

// previewRound is the provider's state of one task (CUX-P9a): the round
// the requester opened, the stream_seq high-water mark, and what was
// offered and sent.
type previewRound struct {
	ix       string
	answers  string
	mode     string // "" when the round asked for no preview
	canceled bool
	// id changes with every new round and every cancel: a frame made for
	// an earlier id is not handed over.
	id       uint64
	seq      uint64
	sentAny  bool
	lastAt   uint64
	cur      previewContent
	last     previewContent
	dirty    bool
	nodeMode string
	elem     *list.Element
}

// previewSeen is the requester's state of one task: the highest stream_seq
// taken, when the last frame was taken, and the frame shown.
type previewSeen struct {
	ix     string
	maxSeq uint64
	lastAt uint64
	taken  bool
	frame  *previewFrame
	elem   *list.Element
}

// previewWindow is the times of the latest frames taken from one peer.
type previewWindow struct {
	at [previewRxPeerPerSecond]uint64
	n  int
	i  int
}

// previewClear is one remembered clear.
type previewClear struct {
	ix    string
	epoch uint64
}

// newMemPreviews is a coalescer and cache on clock now, handing frames to
// send and counting drops with count.
func newMemPreviews(now func() uint64, send func(previewOut), count func(string)) *memPreviews {
	return &memPreviews{now: now, send: send, count: count, wake: make(chan struct{}, 1),
		rounds: map[string]*previewRound{}, roundLRU: list.New(),
		seen: map[string]*previewSeen{}, seenLRU: list.New(), peers: map[string]*previewWindow{},
		clears: map[string]uint64{}}
}

// run hands over the frames that fall due until ctx ends; spawn runs each
// hand-over on its own, so a slow hub holds up no other task.
func (c *memPreviews) run(ctx context.Context, spawn func(func())) {
	t := time.NewTimer(time.Hour)
	defer t.Stop()
	for {
		now := c.now()
		frames, next := c.due(now)
		for _, f := range frames {
			spawn(func() { c.send(f) })
		}
		wait := time.Hour
		if next != 0 {
			wait = time.Duration(next-min(next, now)) * time.Millisecond
		}
		t.Reset(max(wait, 10*time.Millisecond))
		select {
		case <-ctx.Done():
			return
		case <-c.wake:
		case <-t.C:
		}
		if !t.Stop() {
			select {
			case <-t.C:
			default:
			}
		}
	}
}

// nudge wakes run.
func (c *memPreviews) nudge() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Requested is previewCoalescer.Requested.
func (c *memPreviews) Requested(ix, answers, mode string) {
	mode = normalPreviewMode(mode)
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.rounds[ix]
	if r == nil {
		if mode == "" {
			return
		}
		r = &previewRound{ix: ix}
		r.elem = c.roundLRU.PushFront(r)
		c.rounds[ix] = r
		for c.roundLRU.Len() > previewCacheTasks {
			old := c.roundLRU.Remove(c.roundLRU.Back()).(*previewRound)
			delete(c.rounds, old.ix)
		}
	} else {
		c.roundLRU.MoveToFront(r.elem)
	}
	if r.answers == answers {
		return // the same message again: the round stays as it is
	}
	c.roundSeq++
	r.answers, r.mode, r.canceled, r.id = answers, mode, false, c.roundSeq
	r.cur, r.last, r.dirty = previewContent{}, previewContent{}, false
}

// Wanted is previewCoalescer.Wanted.
func (c *memPreviews) Wanted(ix string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.rounds[ix]
	if r == nil || r.mode == "" || r.canceled {
		return "", false
	}
	return r.mode, true
}

// Offer is previewCoalescer.Offer: the content is cut to the tails a
// frame carries now, and the reasoning dropped unless both this node and
// the round asked for it.
func (c *memPreviews) Offer(ix, text, reasoning, nodeMode string) bool {
	nodeMode = normalPreviewMode(nodeMode)
	if nodeMode == "" {
		return false
	}
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.rounds[ix]
	if r == nil || r.mode == "" || r.canceled {
		return false
	}
	c.roundLRU.MoveToFront(r.elem)
	if nodeMode != previewModeWithReasoning || r.mode != previewModeWithReasoning {
		reasoning = ""
	}
	var v previewContent
	v.Text, v.Omitted = previewTail(text)
	v.Reasoning, v.ReasoningOmitted = previewTail(reasoning)
	v.TextAt = now
	if v.Reasoning != "" {
		v.ReasoningAt = now
	}
	r.cur, r.nodeMode = v, nodeMode
	r.dirty = !v.empty() && !v.same(r.last)
	if r.dirty {
		c.nudge()
	}
	return true
}

// CancelRound is previewCoalescer.CancelRound.
func (c *memPreviews) CancelRound(ix string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.rounds[ix]
	if r == nil {
		return
	}
	c.roundSeq++
	r.canceled, r.dirty, r.id = true, false, c.roundSeq
}

// due takes the frames that fall due at now (CUX-P6): a task whose
// content changed since its last frame, and whose last frame is at least
// previewGapMS old, gets one — within the node's frames per second, or it
// is dropped (preview-dropped) and tried again a gap later. Each frame's
// stream_seq is max(the task's last + 1, now) (CUX-P9a). next is when the
// next frame falls due (0: none is waiting).
func (c *memPreviews) due(now uint64) (out []previewOut, next uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	later := func(at uint64) {
		if next == 0 || at < next {
			next = at
		}
	}
	for _, r := range c.rounds {
		if !r.dirty || r.canceled || r.mode == "" {
			continue
		}
		if r.sentAny && now < r.lastAt+previewGapMS {
			later(r.lastAt + previewGapMS)
			continue
		}
		r.sentAny, r.lastAt = true, now
		if !c.nodeSlot(now) {
			c.count(cntPreviewDropped)
			later(now + previewGapMS)
			continue
		}
		r.seq = max(r.seq+1, now)
		r.last, r.dirty = r.cur, false
		ix, id := r.ix, r.id
		out = append(out, previewOut{IX: r.ix, Answers: r.answers, Mode: r.mode, NodeMode: r.nodeMode, Seq: r.seq,
			previewContent: r.cur, live: func() bool { return c.live(ix, id) }})
	}
	return out, next
}

// nodeSlot takes one of the node's frames per second at now, or reports
// that the second is full.
func (c *memPreviews) nodeSlot(now uint64) bool {
	if c.sentN == previewNodePerSecond {
		if now < c.sent[c.sentAt]+1000 {
			return false
		}
		c.sent[c.sentAt] = now
		c.sentAt = (c.sentAt + 1) % previewNodePerSecond
		return true
	}
	c.sent[(c.sentAt+c.sentN)%previewNodePerSecond] = now
	c.sentN++
	return true
}

// live reports whether the round id is still task ix's, not canceled.
func (c *memPreviews) live(ix string, id uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.rounds[ix]
	return r != nil && r.id == id && !r.canceled
}

// Epoch is previewCoalescer.Epoch.
func (c *memPreviews) Epoch() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.epoch
}

// Arrived is previewCoalescer.Arrived: CUX-P14 items 4 and 5, then the
// frame is the task's preview (CUX-P15).
func (c *memPreviews) Arrived(in previewIn) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.clears[in.IX]; ok {
		if e > in.Epoch {
			return false // the task moved on while the frame was judged
		}
	} else if c.clearFloor > in.Epoch {
		return false
	}
	now := in.At
	s := c.seen[in.IX]
	if s != nil {
		if in.Seq <= s.maxSeq {
			return false // item 4: old or replayed
		}
		if s.taken && now < s.lastAt+previewRxTaskGapMS {
			return false // item 5: the task's rate
		}
	}
	w := c.peers[in.From]
	if w != nil && w.n == previewRxPeerPerSecond && now < w.at[w.i]+1000 {
		return false // item 5: the peer's rate
	}
	if s == nil {
		s = &previewSeen{ix: in.IX}
		s.elem = c.seenLRU.PushFront(s)
		c.seen[in.IX] = s
		for c.seenLRU.Len() > previewCacheTasks {
			old := c.seenLRU.Remove(c.seenLRU.Back()).(*previewSeen)
			delete(c.seen, old.ix)
		}
	} else {
		c.seenLRU.MoveToFront(s.elem)
	}
	f := in.previewFrame
	s.maxSeq, s.lastAt, s.taken, s.frame = in.Seq, now, true, &f
	if w == nil {
		c.sweepPeers(now)
		w = &previewWindow{}
		c.peers[in.From] = w
	}
	w.add(now)
	return true
}

// add records a frame taken at now.
func (w *previewWindow) add(now uint64) {
	if w.n < previewRxPeerPerSecond {
		w.at[(w.i+w.n)%previewRxPeerPerSecond] = now
		w.n++
		return
	}
	w.at[w.i] = now
	w.i = (w.i + 1) % previewRxPeerPerSecond
}

// newest is the time of the latest frame recorded.
func (w *previewWindow) newest() uint64 {
	return w.at[(w.i+w.n-1+previewRxPeerPerSecond)%previewRxPeerPerSecond]
}

// sweepPeers forgets the peers with no frame in the last second, once the
// windows kept reach their bound.
func (c *memPreviews) sweepPeers(now uint64) {
	if len(c.peers) < previewPeersKept {
		return
	}
	for p, w := range c.peers {
		if w.newest()+1000 <= now {
			delete(c.peers, p)
		}
	}
	for p := range c.peers {
		if len(c.peers) < previewPeersKept {
			break
		}
		delete(c.peers, p) // all busy: one of them starts afresh
	}
}

// Clear is previewCoalescer.Clear.
func (c *memPreviews) Clear(ix string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.epoch++
	c.clears[ix] = c.epoch
	c.clearRing = append(c.clearRing, previewClear{ix: ix, epoch: c.epoch})
	if len(c.clearRing) > previewClearsKept {
		old := c.clearRing[0]
		c.clearRing = c.clearRing[1:]
		if c.clears[old.ix] == old.epoch {
			delete(c.clears, old.ix)
		}
		c.clearFloor = old.epoch
	}
	if s := c.seen[ix]; s != nil {
		s.frame = nil
	}
}

// Current is previewCoalescer.Current.
func (c *memPreviews) Current(ix string) (previewFrame, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.seen[ix]
	if s == nil || s.frame == nil {
		return previewFrame{}, false
	}
	return *s.frame, true
}
