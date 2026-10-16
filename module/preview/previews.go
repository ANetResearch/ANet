package preview

// previews.go is the preview module's engine (correctness-ux §2.3,
// §4.3.6, §4.3.7; engine §2.11.5, §4.5): the provider's coalescer and the
// requester's preview cache, both in memory only (11-modularity §3.2,
// MOD-6).
//
// The kernel reaches it only through module.Previewer. Previews is the
// implementation: it reads neither the store nor the configuration, and
// what it needs is passed in (the clock, the hand-over of a frame, the
// counter, the seam's mode and Published), so that nothing here can reach
// the node by another way. The kernel's side of the seam — the table of
// requested rounds, the facts and the enforcement of a frame — is in
// internal/daemon/previewseam.go.
//
// Nothing here is written to interactions.db, the outbox, the replay table
// or the evidence chain, and no preview text reaches a log (CUX-P15,
// SI-73). A restart forgets all of it: the round a provider was previewing
// sends no more frames, its result still goes, and the next round can be
// previewed again (CUX-P0); stream_seq is taken from the clock so the
// frames after a restart are not mistaken for old ones (CUX-P9a).

import (
	"container/list"
	"context"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ANetResearch/ANetCore/delegation"

	"github.com/ANetResearch/ANet/module"
)

// The preview modes (core §2.13, CUX-P0): what a requester's message asks
// for in anet.preview_mode, and what this node's stream_preview allows.
const (
	previewModeOn            = "on"
	previewModeWithReasoning = "with_reasoning"
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

// The preview types are the seam's (module/preview.go): the kernel and the
// implementation share them.
type (
	previewContent = module.PreviewContent
	previewFrame   = module.PreviewSnapshot
	previewIn      = module.PreviewIn
)

var _ module.Previewer = (*Previews)(nil)

// normalPreviewMode is v when it is a mode that sends previews, else "".
func normalPreviewMode(v string) string {
	switch v {
	case previewModeOn, previewModeWithReasoning:
		return v
	}
	return ""
}

// CountDropped is the counter a frame this node did not send is counted
// under: over the node's frames per second (CUX-P6).
const CountDropped = "preview-dropped"

// Frame is one frame a coalescer hands over: the round it belongs to and
// what the frame says. Live reports whether the round is still the one the
// frame was made for (nil: always).
type Frame struct {
	IX       string
	Answers  string
	Mode     string // the round's anet.preview_mode
	NodeMode string // this node's stream_preview when the text was offered
	Seq      uint64
	module.PreviewContent
	Live func() bool
}

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

// Previews is this node's coalescer and preview cache, in memory. It
// reads no store and no configuration: now, send and count are the
// kernel's.
type Previews struct {
	now   func() uint64
	send  func(Frame)
	count func(string)
	// mode is this node's stream_preview now (the seam's Mode); nil reads
	// as "off". published says a task has a new preview to show (the
	// seam's Published); nil says nothing.
	mode      func() string
	published func(ix string)
	wake      chan struct{}

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

// New is a coalescer and cache on clock now, handing frames to send and
// counting drops with count. SetSeam says what the node's stream_preview is
// and how a task is told it has a preview; without it the mode reads as
// "off" and nothing is announced.
func New(now func() uint64, send func(Frame), count func(string)) *Previews {
	return &Previews{now: now, send: send, count: count, wake: make(chan struct{}, 1),
		rounds: map[string]*previewRound{}, roundLRU: list.New(),
		seen: map[string]*previewSeen{}, seenLRU: list.New(), peers: map[string]*previewWindow{},
		clears: map[string]uint64{}}
}

// Flush hands over, in the calling goroutine, every frame due at now, and
// returns how many went and when the next falls due (0: none is waiting).
// It is what Run does on a wake-up, for a test that moves the clock by hand.
func (c *Previews) Flush(now uint64) (int, uint64) {
	frames, next := c.Due(now)
	for _, f := range frames {
		c.send(f)
	}
	return len(frames), next
}

// SetSeam says what this node's stream_preview is now and how a task is
// told it has a preview to show (the seam's Mode and Published). It is
// called before Run.
func (c *Previews) SetSeam(mode func() string, published func(ix string)) {
	c.mode, c.published = mode, published
}

// Run hands over the frames that fall due until ctx ends; spawn runs each
// hand-over on its own, so a slow hub holds up no other task.
func (c *Previews) Run(ctx context.Context, spawn func(func())) {
	t := time.NewTimer(time.Hour)
	defer t.Stop()
	for {
		now := c.now()
		frames, next := c.Due(now)
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
func (c *Previews) nudge() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Requested is module.Previewer.Requested.
func (c *Previews) Requested(ix, answers, mode string) {
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

// Wanted is module.Previewer.Wanted.
func (c *Previews) Wanted(ix string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.rounds[ix]
	if r == nil || r.mode == "" || r.canceled {
		return "", false
	}
	return r.mode, true
}

// Offer is module.Previewer.Offer: what the node's stream_preview allows
// is read through the seam.
func (c *Previews) Offer(ix, text, reasoning string) module.PreviewOffer {
	node := "off"
	if c.mode != nil {
		node = c.mode()
	}
	return module.PreviewOffer{Taken: c.offer(ix, text, reasoning, node)}
}

// offer: the content is cut to the tails a frame carries now, and the
// reasoning dropped unless both this node and the round asked for it.
func (c *Previews) offer(ix, text, reasoning, nodeMode string) bool {
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
	r.dirty = !v.Empty() && !v.Same(r.last)
	if r.dirty {
		c.nudge()
	}
	return true
}

// CancelRound is module.Previewer.CancelRound.
func (c *Previews) CancelRound(ix string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.rounds[ix]
	if r == nil {
		return
	}
	c.roundSeq++
	r.canceled, r.dirty, r.id = true, false, c.roundSeq
}

// Due takes the frames that fall due at now (CUX-P6): a task whose
// content changed since its last frame, and whose last frame is at least
// previewGapMS old, gets one — within the node's frames per second, or it
// is dropped (preview-dropped) and tried again a gap later. Each frame's
// stream_seq is max(the task's last + 1, now) (CUX-P9a). next is when the
// next frame falls due (0: none is waiting).
func (c *Previews) Due(now uint64) (out []Frame, next uint64) {
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
			c.count(CountDropped)
			later(now + previewGapMS)
			continue
		}
		r.seq = max(r.seq+1, now)
		r.last, r.dirty = r.cur, false
		ix, id := r.ix, r.id
		out = append(out, Frame{IX: r.ix, Answers: r.answers, Mode: r.mode, NodeMode: r.nodeMode, Seq: r.seq,
			PreviewContent: r.cur, Live: func() bool { return c.live(ix, id) }})
	}
	return out, next
}

// nodeSlot takes one of the node's frames per second at now, or reports
// that the second is full.
func (c *Previews) nodeSlot(now uint64) bool {
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
func (c *Previews) live(ix string, id uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.rounds[ix]
	return r != nil && r.id == id && !r.canceled
}

// Epoch is module.Previewer.Epoch.
func (c *Previews) Epoch() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.epoch
}

// Arrived is module.Previewer.Arrived: CUX-P14 items 4 and 5, then the
// frame is the task's preview (CUX-P15), and the task is told it has a new
// one to show.
func (c *Previews) Arrived(in previewIn) bool {
	if !c.arrive(in) {
		return false
	}
	if c.published != nil {
		c.published(in.IX)
	}
	return true
}

// arrive judges and keeps the frame.
func (c *Previews) arrive(in previewIn) bool {
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
	f := in.PreviewSnapshot
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
func (c *Previews) sweepPeers(now uint64) {
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

// Clear is module.Previewer.Clear.
func (c *Previews) Clear(ix string) {
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

// Current is module.Previewer.Current.
func (c *Previews) Current(ix string) (previewFrame, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.seen[ix]
	if s == nil || s.frame == nil {
		return previewFrame{}, false
	}
	return *s.frame, true
}

// HandOver is the hand-over of one frame through the seam, under ctx and
// within the time one frame may take: nothing is sent for a round answered while the frame
// was being made. The seam checks the facts again (CUX-P4).
func HandOver(ctx context.Context, seam module.PreviewSeam, f Frame) {
	if f.Live != nil && !f.Live() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, previewSendTimeout)
	defer cancel()
	seam.SendFrame(ctx, f.IX, module.PreviewOut{Answers: f.Answers, Seq: f.Seq, PreviewContent: f.PreviewContent})
}
