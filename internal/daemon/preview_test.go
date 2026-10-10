package daemon

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// previewClock is a clock a test moves by hand (unix ms).
type previewClock struct{ ms atomic.Uint64 }

func newPreviewClock() *previewClock {
	c := &previewClock{}
	c.ms.Store(uint64(time.Now().UnixMilli()))
	return c
}

func (c *previewClock) now() uint64             { return c.ms.Load() }
func (c *previewClock) add(ms uint64)           { c.ms.Add(ms) }
func (c *previewClock) set(ms uint64)           { c.ms.Store(ms) }
func (c *previewClock) sinceMS(t uint64) uint64 { return c.now() - t }

// sentFrames records what a coalescer handed over.
type sentFrames struct {
	mu  sync.Mutex
	out []previewOut
	at  []uint64
}

func (s *sentFrames) add(o previewOut, at uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.out = append(s.out, o)
	s.at = append(s.at, at)
}

func (s *sentFrames) n() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.out)
}

// unitPreviews is a coalescer on clk that records its frames and counts.
func unitPreviews(clk *previewClock) (*memPreviews, *sentFrames, map[string]int) {
	sent := &sentFrames{}
	counts := map[string]int{}
	var mu sync.Mutex
	co := newMemPreviews(clk.now, func(o previewOut) { sent.add(o, clk.now()) }, func(s string) {
		mu.Lock()
		counts[s]++
		mu.Unlock()
	})
	return co, sent, counts
}

// flushPreviews hands over, in this goroutine, every frame of co due now:
// what run does on a wake-up. It returns how many went and when the next
// falls due.
func flushPreviews(co *memPreviews, now uint64) (int, uint64) {
	frames, next := co.due(now)
	for _, f := range frames {
		co.send(f)
	}
	return len(frames), next
}

// manualPreviews gives d a coalescer on clk whose frames go only when the
// test flushes them.
func manualPreviews(d *Daemon, clk *previewClock) *memPreviews {
	co := newMemPreviews(clk.now, d.sendPreviewFrame, d.count)
	d.setPreviewer(co)
	return co
}

// previewMeta is message metadata asking for previews in mode.
func previewMeta(mode string) []byte {
	b, _ := json.Marshal(map[string]any{metaPreviewMode: mode})
	return b
}

// previewDelegation is a task req delegated to prov asking for previews in
// mode (nil metadata for ""), which prov took.
func previewDelegation(t *testing.T, req, prov *Daemon, mode string) string {
	t.Helper()
	id, err := newInteractionID()
	if err != nil {
		t.Fatal(err)
	}
	var meta []byte
	if mode != "" {
		meta = previewMeta(mode)
	}
	if err := req.delegateInWithID(context.Background(), id, prov.AID(), "a task with previews", nil, "", meta, nil); err != nil {
		t.Fatal(err)
	}
	confirmDelegation(t, req, prov)
	if _, err := prov.ix.Get(id); err != nil {
		t.Fatalf("setup: the provider did not take the task: %v", err)
	}
	return id
}

// lastOwnText is the id of d's latest text message on task ix.
func lastOwnText(t testing.TB, d *Daemon, ix string) string {
	t.Helper()
	msgs, err := d.ix.Messages(ix)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].SenderAID == d.AID() && msgs[i].Kind == interactions.MsgText {
			return msgs[i].MsgID
		}
	}
	t.Fatalf("%s has no text message of its own on %s", d.AID(), ix)
	return ""
}

// lastPeerText is the id of the latest text message of the peer's on task
// ix as d stored it.
func lastPeerText(t testing.TB, d *Daemon, ix string) string {
	t.Helper()
	msgs, err := d.ix.Messages(ix)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].SenderAID != d.AID() && msgs[i].Kind == interactions.MsgText {
			return msgs[i].MsgID
		}
	}
	t.Fatalf("no text message of the peer's on %s", ix)
	return ""
}

// previewFrameMsg is a valid preview frame answering answers.
func previewFrameMsg(answers string, seq uint64, text string) delegation.ChatMsg {
	return delegation.ChatMsg{Kind: delegation.ChatStreamPreview, Body: text, StreamSeq: seq, StreamAtMS: int64(seq),
		MsgID: fmt.Sprintf("frame-%d", seq), Answers: answers}
}

// rkFor is the rk prov announces on task ix: its own key for the task when
// it holds one, else one made up (a task the requester holds no rk of the
// provider's for compares nothing).
func rkFor(t testing.TB, prov *Daemon, ix string) *seal.RK {
	t.Helper()
	if rk, err := prov.ix.OwnRK(ix); err == nil && rk != nil {
		return &seal.RK{Suite: rk.Suite, Pub: rk.Pub}
	}
	return newIXKey(t, ix, "nobody").RK()
}

// craftPreview seals cm as prov's preview frame to req on task ix, the way
// the one-shot sender does (ENG-O9): two layers to req's key for ix, prov's
// rk inside, no KEL, notice, key set or grant, outer exp a minute on. edit
// changes the inner last.
func craftPreview(t *testing.T, prov, req *Daemon, ix string, cm delegation.ChatMsg, edit func(*seal.SealedInner)) []byte {
	t.Helper()
	body, err := cm.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	own := ownIXKey(t, req, ix)
	rk := rkFor(t, prov, ix)
	env, _ := craftWith(t, senderOf(prov), req, seal.TypeMessage, ix, body, craftOpt{oneShot: true, peerRK: own.RK(),
		edit: func(in *seal.SealedInner) {
			in.KEL, in.Notices, in.Keys = nil, nil, nil
			in.RK = rk
			in.Exp = in.TS + previewLifeMS
			if edit != nil {
				edit(in)
			}
		}})
	return env
}

// taken reports whether r is a preview frame taken.
func taken(r rxResult) bool { return r.class == rxAccepted && r.reason == "preview" }

// outboundTask makes on req a task it delegated to peer, with one text
// message of its own carrying meta, and its own key for the task; the
// message's id is msgID.
func outboundTask(t *testing.T, req *Daemon, id, peer, msgID string, meta []byte, mod func(*interactions.New)) {
	t.Helper()
	n := interactions.New{ID: id, Role: interactions.RoleOutbound, PeerAID: peer, Goal: "g"}
	if mod != nil {
		mod(&n)
	}
	if err := req.ix.Update(func(tx *interactions.Tx) error {
		if err := tx.Create(n); err != nil {
			return err
		}
		_, _, err := tx.AddMessageRecord(interactions.MessageRecord{InteractionID: id, SenderAID: req.AID(),
			Kind: interactions.MsgText, Body: "g", MsgID: msgID, Metadata: meta})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	k, err := newOwnIXKey(id, peer)
	if err != nil {
		t.Fatal(err)
	}
	putIXKey(t, req, k)
}

// frozenClock stops d's wire clock at clk.
func frozenClock(d *Daemon) *previewClock {
	clk := newPreviewClock()
	d.setClock(clk.now)
	return clk
}

// CUX-P6 (engine §0.4): the coalescer sends a task's first change at once,
// holds the changes that follow until five seconds after its last frame,
// sends only what changed, sends the last change within five seconds, and
// hands over at most twenty frames a second for the whole node, dropping
// the rest (preview-dropped). Every frame's stream_seq is above the task's
// last and not below the time it was made (CUX-P9a). On a fake clock.
func TestCoalescerCadence(t *testing.T) {
	clk := newPreviewClock()
	co, sent, counts := unitPreviews(clk)
	tick := func() int { n, _ := flushPreviews(co, clk.now()); return n }
	co.Requested("ix1", "m1", previewModeOn)

	if !co.Offer("ix1", "a", "", previewModeOn) || tick() != 1 {
		t.Fatal("the first change of a round did not go at once")
	}
	first := clk.now()
	for i := 1; i <= 4; i++ {
		clk.add(1000)
		co.Offer("ix1", strings.Repeat("a", i+1), "", previewModeOn)
		if n := tick(); n != 0 {
			t.Fatalf("a frame %d ms after the last", clk.sinceMS(first))
		}
	}
	if _, next := co.due(clk.now()); next != first+previewGapMS {
		t.Fatalf("the next frame falls due at %d, want %d", next, first+previewGapMS)
	}
	clk.add(1000)
	if tick() != 1 || sent.out[1].Text != "aaaaa" {
		t.Fatalf("five seconds on: %d frames, the last %+v", sent.n(), sent.out[len(sent.out)-1].previewContent)
	}
	// Nothing changed: nothing goes, however long.
	co.Offer("ix1", "aaaaa", "", previewModeOn)
	for i := 0; i < 4; i++ {
		clk.add(5000)
		if tick() != 0 {
			t.Fatal("a frame went with nothing changed")
		}
	}
	// After a quiet spell a change goes at once.
	co.Offer("ix1", "aaaaab", "", previewModeOn)
	if tick() != 1 {
		t.Fatal("a change after a quiet spell waited")
	}
	for i, o := range sent.out {
		if o.Seq < sent.at[i] || (i > 0 && o.Seq <= sent.out[i-1].Seq) {
			t.Fatalf("frame %d: stream_seq %d at %d after %d", i, o.Seq, sent.at[i], sent.out[max(i-1, 0)].Seq)
		}
		if i > 0 && sent.at[i]-sent.at[i-1] < previewGapMS {
			t.Fatalf("frames %d and %d are %d ms apart", i-1, i, sent.at[i]-sent.at[i-1])
		}
	}

	// The whole node: 25 tasks changing in the same second get 20 frames;
	// the other five are dropped and counted, and go a gap later.
	clk.add(60_000)
	for i := 0; i < 25; i++ {
		ix := fmt.Sprintf("node-%d", i)
		co.Requested(ix, "m", previewModeOn)
		co.Offer(ix, "x", "", previewModeOn)
	}
	before := sent.n()
	if n := tick(); n != previewNodePerSecond || counts[cntPreviewDropped] != 5 {
		t.Fatalf("%d frames in one second, %d dropped", n, counts[cntPreviewDropped])
	}
	clk.add(999)
	if tick() != 0 {
		t.Fatal("more frames within the same second")
	}
	clk.add(previewGapMS)
	if n := tick(); n != 5 || sent.n()-before != 25 {
		t.Fatalf("the dropped five: %d went a gap later (%d in all)", n, sent.n()-before)
	}
}

// CUX-P6 as a property: whatever the times of the offers, a task's frames
// are at least five seconds apart, each shows something new, and the last
// offer is shown at most five seconds after it was made — when the
// coalescer wakes as run does (on an offer, and when a frame falls due).
func TestCoalescerCadenceProperty(t *testing.T) {
	for seed := uint64(1); seed <= 200; seed++ {
		r := rand.New(rand.NewPCG(seed, 7))
		clk := newPreviewClock()
		co, sent, _ := unitPreviews(clk)
		co.Requested("ix", "m", previewModeOn)
		text := ""
		var lastOffer uint64
		var next uint64
		wake := func() {
			_, next = flushPreviews(co, clk.now())
		}
		for i := 0; i < 40; i++ {
			step := uint64(r.IntN(4000))
			for next != 0 && clk.now()+step >= next {
				// A wake-up at the frame's time, before the offer.
				step -= next - clk.now()
				clk.set(next)
				wake()
			}
			clk.add(step)
			if r.IntN(3) > 0 {
				text += "w"
			}
			co.Offer("ix", text, "", previewModeOn)
			lastOffer = clk.now()
			wake()
		}
		for next != 0 {
			clk.set(next)
			wake()
		}
		for i := 1; i < len(sent.out); i++ {
			if sent.at[i]-sent.at[i-1] < previewGapMS {
				t.Fatalf("seed %d: frames %d ms apart", seed, sent.at[i]-sent.at[i-1])
			}
			if sent.out[i].Text == sent.out[i-1].Text {
				t.Fatalf("seed %d: a frame with nothing new", seed)
			}
		}
		last := len(sent.out) - 1
		if last < 0 || sent.out[last].Text != text || sent.at[last] > lastOffer+previewGapMS {
			t.Fatalf("seed %d: the last change (at %d) shown at %d", seed, lastOffer, sent.at[max(last, 0)])
		}
	}
}

// CUX-P0, CUX-P4: only a round that asked for previews, on a node whose
// stream_preview is not off, takes an offer; an unknown mode is no request
// (M36's kernel half), the reasoning only goes when both sides asked for
// it, and a copy of the round's message does not reopen a round answered.
func TestPreviewRoundsAndModes(t *testing.T) {
	for _, tc := range []struct {
		meta string
		want string
	}{
		{`{"anet.preview_mode":"on"}`, "on"},
		{`{"anet.preview_mode":"with_reasoning","x":1}`, "with_reasoning"},
		{`{"anet.preview_mode":"future"}`, ""},
		{`{"anet.preview_mode":"off"}`, ""},
		{`{"anet.preview_mode":true}`, ""},
		{`{"anet.preview_mode":"ON"}`, ""},
		{`{}`, ""},
		{`[1]`, ""},
		{`not json`, ""},
		{``, ""},
	} {
		if got := previewModeOf([]byte(tc.meta)); got != tc.want {
			t.Errorf("previewModeOf(%s) = %q, want %q", tc.meta, got, tc.want)
		}
	}
	clk := newPreviewClock()
	co, sent, _ := unitPreviews(clk)
	co.Requested("none", "m1", "")
	co.Requested("future", "m1", "future")
	co.Requested("on", "m1", previewModeOn)
	co.Requested("wr", "m1", previewModeWithReasoning)
	for _, ix := range []string{"none", "future"} {
		if _, ok := co.Wanted(ix); ok || co.Offer(ix, "t", "", previewModeOn) {
			t.Fatalf("%s: a round that asked for no preview takes an offer", ix)
		}
	}
	if co.Offer("on", "t", "", "off") || co.Offer("on", "t", "", "") {
		t.Fatal("an offer under stream_preview off was taken")
	}
	co.Offer("on", "t", "thinking", previewModeWithReasoning)
	co.Offer("wr", "t", "thinking", previewModeOn)
	flushPreviews(co, clk.now())
	clk.add(previewGapMS)
	co.Offer("wr", "t2", "thinking more", previewModeWithReasoning)
	flushPreviews(co, clk.now())
	if sent.n() != 3 {
		t.Fatalf("%d frames, want 3", sent.n())
	}
	for i, want := range []string{"", "", "thinking more"} {
		if sent.out[i].Reasoning != want {
			t.Fatalf("frame %d (%s, node %s) carries reasoning %q, want %q", i, sent.out[i].Mode, sent.out[i].NodeMode,
				sent.out[i].Reasoning, want)
		}
	}
	// Answered: the round takes nothing more, a copy of its message
	// changes nothing, and the next message opens the next round.
	co.CancelRound("on")
	co.Requested("on", "m1", previewModeOn)
	if co.Offer("on", "more", "", previewModeOn) {
		t.Fatal("a copy of the round's message reopened it")
	}
	co.Requested("on", "m2", "")
	if co.Offer("on", "more", "", previewModeOn) {
		t.Fatal("a round that asked for no preview takes an offer")
	}
	co.Requested("on", "m3", previewModeOn)
	if !co.Offer("on", "more", "", previewModeOn) {
		t.Fatal("the next round asking for previews takes no offer")
	}
}

// CUX §2.3 (W2 UX-2 6): every frame the provider makes passes
// delegation.ValidatePreview — the tails cut to MaxPreviewTail on a code
// point boundary, valid UTF-8, no key 3 or 10, stream_seq above 0, answers
// present and at most 256 bytes — and a round whose answers is longer
// sends no frame.
func TestPreviewFramesPassValidatePreview(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	pieces := []string{"a", "é", "€", "𝄞", "\xff", "\xe2\x82", " ", "\n"}
	clk := newPreviewClock()
	co, _, _ := unitPreviews(clk)
	co.Requested("ix", "m-1", previewModeWithReasoning)
	for i := 0; i < 300; i++ {
		var b, rb strings.Builder
		for n := r.IntN(3 * delegation.MaxPreviewTail / 2); b.Len() < n; {
			b.WriteString(pieces[r.IntN(len(pieces))])
		}
		for n := r.IntN(3 * delegation.MaxPreviewTail / 2); rb.Len() < n; {
			rb.WriteString(pieces[r.IntN(len(pieces))])
		}
		if i%7 == 0 {
			b.Reset()
		}
		co.Offer("ix", b.String(), rb.String(), previewModeWithReasoning)
		clk.add(previewGapMS)
		frames, _ := co.due(clk.now())
		for _, o := range frames {
			cm, ok := previewChatMsg(o)
			if !ok {
				t.Fatalf("offer %d: no frame for %d and %d bytes", i, b.Len(), rb.Len())
			}
			if err := delegation.ValidatePreview(cm); err != nil {
				t.Fatalf("offer %d: %v", i, err)
			}
			wire, err := cm.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			back, err := delegation.UnmarshalChatMsg(wire)
			if err != nil || delegation.ValidatePreview(back) != nil {
				t.Fatalf("offer %d: the frame does not decode as one: %v", i, err)
			}
			whole := strings.ToValidUTF8(b.String(), "\uFFFD")
			if cm.Omitted+uint64(len(cm.Body)) != uint64(len(whole)) || !strings.HasSuffix(whole, cm.Body) {
				t.Fatalf("offer %d: body of %d bytes after %d omitted, of %d", i, len(cm.Body), cm.Omitted, len(whole))
			}
			if len(whole) > delegation.MaxPreviewTail && len(cm.Body) < delegation.MaxPreviewTail-3 {
				t.Fatalf("offer %d: the tail is %d bytes", i, len(cm.Body))
			}
		}
	}
	// The reasoning goes only when both this node and the round asked.
	for _, modes := range [][2]string{{previewModeOn, previewModeWithReasoning}, {previewModeWithReasoning, previewModeOn}} {
		cm, ok := previewChatMsg(previewOut{IX: "ix", Answers: "m", Mode: modes[0], NodeMode: modes[1], Seq: 1,
			previewContent: previewContent{Text: "t", TextAt: 1, Reasoning: "r", ReasoningAt: 1}})
		if !ok || cm.ReasoningBody != "" || cm.ReasoningStreamAtMS != 0 {
			t.Fatalf("round %s, node %s: reasoning %q", modes[0], modes[1], cm.ReasoningBody)
		}
	}
	if cm, ok := previewChatMsg(previewOut{IX: "ix", Answers: "m", Mode: previewModeWithReasoning,
		NodeMode: previewModeWithReasoning, Seq: 1, previewContent: previewContent{Text: "t", TextAt: 1, Reasoning: "r",
			ReasoningAt: 2}}); !ok || cm.ReasoningBody != "r" || cm.ReasoningStreamAtMS != 2 {
		t.Fatalf("both asked: %+v", cm)
	}
	// An answers over 256 bytes: no frame.
	long := strings.Repeat("m", delegation.MaxPreviewIDLen+1)
	if _, ok := previewChatMsg(previewOut{IX: "ix", Answers: long, Mode: previewModeOn, NodeMode: previewModeOn, Seq: 1,
		previewContent: previewContent{Text: "t", TextAt: 1}}); ok {
		t.Fatal("a frame answering a 257-byte message id")
	}
}

// CUX-P14 and engine §4.5: a requester takes a preview frame only when
// every item holds, and drops it silently (preview-rejected, no token)
// otherwise — one case per item.
func TestPreviewAcceptTable(t *testing.T) {
	_, req, prov := quietPair(t)
	clk := frozenClock(req)
	id := previewDelegation(t, req, prov, previewModeOn)
	answers := lastOwnText(t, req, id)
	seq := clk.now()
	nextSeq := func() uint64 { seq += 10; return seq }
	send := func(ix string, cm delegation.ChatMsg) rxResult {
		t.Helper()
		clk.add(previewRxTaskGapMS)
		return receive(t, req, craftPreview(t, prov, req, ix, cm, nil))
	}

	// All items hold. The round asked for "on": reasoning the provider
	// sent anyway is not kept (CUX-P4, P17).
	idSeq := nextSeq()
	good := previewFrameMsg(answers, idSeq, "so far")
	good.ReasoningBody, good.ReasoningStreamAtMS = "not asked for", int64(idSeq)
	if r := send(id, good); !taken(r) {
		t.Fatalf("a good frame: %+v", r)
	}
	if f, ok := req.previews().Current(id); !ok || f.Text != "so far" || f.Answers != answers || f.Reasoning != "" {
		t.Fatalf("the preview shown: %+v %v", f, ok)
	}

	other := prov.AID() + "x"
	var otherKey *seal.IXKey
	cases := []struct {
		name string
		ix   string
		cm   func() delegation.ChatMsg
		prep func(t *testing.T, ix string)
	}{
		{"1: a task this node does not hold", "ix_unknown_preview", nil, nil},
		{"1: a task this node provides", "ix_inbound_preview", nil, func(t *testing.T, ix string) {
			outboundTask(t, req, ix, prov.AID(), "m-in", previewMeta(previewModeOn), func(n *interactions.New) {
				n.Role = interactions.RoleInbound
			})
		}},
		{"1: a task delegated to another peer", "ix_other_peer", nil, func(t *testing.T, ix string) {
			outboundTask(t, req, ix, other, "m-in", previewMeta(previewModeOn), nil)
			// The second layer opens: a key of the task's for the sender.
			k := newIXKey(t, ix, prov.AID())
			req.setIXLookup(holdingIXKey(k))
			t.Cleanup(func() { req.setIXLookup(nil) })
			otherKey = k
		}},
		{"1: a task ended", "ix_ended_preview", nil, func(t *testing.T, ix string) {
			outboundTask(t, req, ix, prov.AID(), "m-in", previewMeta(previewModeOn), nil)
			if err := req.ix.Update(func(tx *interactions.Tx) error {
				_, err := tx.SetState(ix, interactions.StateCanceled, interactions.CauseLocalEnded)
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}},
		{"1: a capability call", "ix_cap_preview", nil, func(t *testing.T, ix string) {
			outboundTask(t, req, ix, prov.AID(), "m-in", previewMeta(previewModeOn), func(n *interactions.New) {
				n.IsCapability = true
			})
		}},
		{"2: answers is not the latest message", "ix_old_answers", func() delegation.ChatMsg {
			return previewFrameMsg("m-older", nextSeq(), "x")
		}, func(t *testing.T, ix string) {
			outboundTask(t, req, ix, prov.AID(), "m-in", previewMeta(previewModeOn), nil)
		}},
		{"2: the latest message asked for no preview", "ix_not_asked", nil, func(t *testing.T, ix string) {
			outboundTask(t, req, ix, prov.AID(), "m-in", nil, nil)
		}},
		{"2: the latest message asked for an unknown mode", "ix_future_mode", nil, func(t *testing.T, ix string) {
			outboundTask(t, req, ix, prov.AID(), "m-in", previewMeta("future"), nil)
		}},
		{"CUX-P2: a frame ValidatePreview refuses", "ix_too_long", func() delegation.ChatMsg {
			return previewFrameMsg("m-in", nextSeq(), strings.Repeat("x", delegation.MaxPreviewTail+1))
		}, func(t *testing.T, ix string) {
			outboundTask(t, req, ix, prov.AID(), "m-in", previewMeta(previewModeOn), nil)
		}},
		{"3: the provider answered since", "ix_answered", nil, func(t *testing.T, ix string) {
			outboundTask(t, req, ix, prov.AID(), "m-in", previewMeta(previewModeOn), nil)
			if _, _, err := req.ix.AddMessageRecord(interactions.MessageRecord{InteractionID: ix, SenderAID: prov.AID(),
				Kind: interactions.MsgText, Body: "an answer", MsgID: "m-ans"}); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var key *seal.IXKey
			if tc.prep != nil {
				otherKey = nil
				tc.prep(t, tc.ix)
				key = otherKey
				if key == nil {
					key = ownIXKey(t, req, tc.ix)
				}
			} else {
				// A key for a task the store does not hold: the test's
				// lookup opens the second layer.
				key = newIXKey(t, tc.ix, prov.AID())
				req.setIXLookup(holdingIXKey(key))
				defer req.setIXLookup(nil)
			}
			cm := previewFrameMsg("m-in", nextSeq(), "x")
			if tc.cm != nil {
				cm = tc.cm()
			}
			before := counter(req, previewRejected)
			body, _ := cm.Marshal()
			clk.add(previewRxTaskGapMS)
			env, ack := craftWith(t, senderOf(prov), req, seal.TypeMessage, tc.ix, body, craftOpt{oneShot: true,
				peerRK: key.RK(), edit: func(in *seal.SealedInner) {
					in.KEL, in.Notices, in.Keys = nil, nil, nil
					in.RK = rkFor(t, prov, tc.ix)
					in.Exp = in.TS + previewLifeMS
				}})
			r := receive(t, req, env)
			if taken(r) || r.reason != previewRejected {
				t.Fatalf("%+v, want %s at step 9", r, previewRejected)
			}
			wantToken(t, r, ack, 0, "")
			if _, ok := req.previews().Current(tc.ix); ok {
				t.Fatal("the frame is shown")
			}
			if counter(req, previewRejected) != before+1 {
				t.Fatal("preview-rejected not counted")
			}
		})
	}

	t.Run("4: a stream_seq not above the task's", func(t *testing.T) {
		for _, s := range []uint64{idSeq, idSeq - 5} {
			if r := send(id, previewFrameMsg(answers, s, "older")); taken(r) || r.reason != previewRejected {
				t.Fatalf("stream_seq %d after %d: %+v", s, idSeq, r)
			}
		}
		if f, _ := req.previews().Current(id); f.Text != "so far" {
			t.Fatalf("an old frame replaced the preview: %q", f.Text)
		}
	})
	t.Run("5: a second frame of the task within two seconds", func(t *testing.T) {
		if r := send(id, previewFrameMsg(answers, nextSeq(), "next")); !taken(r) {
			t.Fatalf("%+v", r)
		}
		clk.add(previewRxTaskGapMS - 1)
		if r := receive(t, req, craftPreview(t, prov, req, id, previewFrameMsg(answers, nextSeq(), "too soon"), nil)); taken(r) {
			t.Fatal("two frames of one task within two seconds")
		}
		clk.add(1)
		if r := receive(t, req, craftPreview(t, prov, req, id, previewFrameMsg(answers, nextSeq(), "in time"), nil)); !taken(r) {
			t.Fatalf("a frame two seconds on: %+v", r)
		}
	})
	t.Run("5: an eleventh frame of the peer within a second", func(t *testing.T) {
		clk.add(previewRxTaskGapMS)
		ids := make([]string, 11)
		for i := range ids {
			ids[i] = fmt.Sprintf("ix_peer_rate_%02d", i)
			outboundTask(t, req, ids[i], prov.AID(), "m-in", previewMeta(previewModeOn), nil)
		}
		for i, ix := range ids {
			env := craftPreview(t, prov, req, ix, previewFrameMsg("m-in", nextSeq(), "x"), nil)
			r := receive(t, req, env)
			if taken(r) != (i < previewRxPeerPerSecond) {
				t.Fatalf("frame %d of the peer in one second: %+v", i+1, r)
			}
		}
		clk.add(1000)
		if r := receive(t, req, craftPreview(t, prov, req, ids[10], previewFrameMsg("m-in", nextSeq(), "x"), nil)); !taken(r) {
			t.Fatalf("a second on: %+v", r)
		}
	})
}

// CUX-P15, SI-73 (mutation M20): taking preview frames writes nothing —
// interactions.db and its -wal keep every byte — and a frame taken is
// published (EventPreview) with what it shows.
func TestPreviewNoDBWrites(t *testing.T) {
	_, req, prov := quietPair(t)
	clk := frozenClock(req)
	id := previewDelegation(t, req, prov, previewModeWithReasoning)
	answers := lastOwnText(t, req, id)
	req.setOutboxHold(true)
	_, events, cancel, err := req.Watch(id)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	envs := make([][]byte, 5)
	for i := range envs {
		cm := previewFrameMsg(answers, clk.now()+uint64(i), fmt.Sprintf("draft %d", i))
		cm.ReasoningBody, cm.ReasoningStreamAtMS = "thinking", int64(clk.now())
		envs[i] = craftPreview(t, prov, req, id, cm, nil)
	}
	time.Sleep(50 * time.Millisecond) // let whatever the setup started settle
	before := storeFiles(t, req)
	for i, env := range envs {
		clk.add(previewRxTaskGapMS)
		if r := receive(t, req, env); !taken(r) {
			t.Fatalf("frame %d: %+v", i, r)
		}
	}
	if after := storeFiles(t, req); !bytes.Equal(before[0], after[0]) || !bytes.Equal(before[1], after[1]) {
		t.Fatalf("taking previews changed the store: db %d → %d bytes, wal %d → %d bytes",
			len(before[0]), len(after[0]), len(before[1]), len(after[1]))
	}
	got := 0
	for got < len(envs) {
		select {
		case e := <-events:
			if e.Kind != EventPreview {
				continue
			}
			if e.preview == nil || e.preview.Text != fmt.Sprintf("draft %d", got) || e.preview.Reasoning != "thinking" {
				t.Fatalf("event %d: %+v", got, e.preview)
			}
			got++
		case <-time.After(5 * time.Second):
			t.Fatalf("%d preview events, want %d", got, len(envs))
		}
	}
}

// storeFiles is the bytes of d's interactions.db and its -wal.
func storeFiles(t testing.TB, d *Daemon) [2][]byte {
	t.Helper()
	var out [2][]byte
	for i, name := range []string{"interactions.db", "interactions.db-wal"} {
		b, err := os.ReadFile(filepath.Join(d.layout.InteractionsDir(), name))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		out[i] = b
	}
	return out
}

// ENG-K19 through the coalescer: a task whose requester announced no rk
// gets no frame, counted preview-no-rk.
func TestPreviewNeedsPeerRK(t *testing.T) {
	srv, req, prov := quietPair(t)
	clk := newPreviewClock()
	co := manualPreviews(prov, clk)
	log := &sentLog{}
	setFake(t, srv.URL, func(h *fakeHub) { h.sendHook = log.hook })
	const ix = "ix_preview_no_rk"
	if err := prov.ix.Update(func(tx *interactions.Tx) error {
		if err := tx.Create(interactions.New{ID: ix, Role: interactions.RoleInbound, PeerAID: req.AID(), Goal: "g"}); err != nil {
			return err
		}
		_, _, err := tx.AddMessageRecord(interactions.MessageRecord{InteractionID: ix, SenderAID: req.AID(),
			Kind: interactions.MsgText, Body: "g", MsgID: "m-1"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	k, err := newOwnIXKey(ix, req.AID())
	if err != nil {
		t.Fatal(err)
	}
	putIXKey(t, prov, k)
	co.Requested(ix, "m-1", previewModeOn)
	before := counter(prov, cntPreviewNoRK)
	if !co.Offer(ix, "so far", "", previewModeOn) {
		t.Fatal("the round takes no offer")
	}
	if n, _ := flushPreviews(co, clk.now()); n != 1 {
		t.Fatalf("%d frames due", n)
	}
	if log.count() != 0 || counter(prov, cntPreviewNoRK) != before+1 {
		t.Fatalf("%d sent, preview-no-rk %d → %d", log.count(), before, counter(prov, cntPreviewNoRK))
	}
}

// previewPair is a requester and a provider whose task id asked for
// previews in mode, the provider's frames made by a coalescer on clk that
// the test flushes, and every envelope the hub carries logged.
func previewPair(t *testing.T, mode string) (req, prov *Daemon, co *memPreviews, clk *previewClock, id string, log *sentLog) {
	t.Helper()
	srv, req, prov := quietPair(t)
	clk = newPreviewClock()
	co = manualPreviews(prov, clk)
	id = previewDelegation(t, req, prov, mode)
	log = &sentLog{}
	setFake(t, srv.URL, func(h *fakeHub) { h.sendHook = log.hook })
	return req, prov, co, clk, id, log
}

// The whole way (CUX-P4 to P9, P14, P15): the requester's delegation asks
// for previews, the provider's coalescer makes a frame of what the
// provider offers, the one-shot sender hands it to the hub, and the
// requester takes it and shows it.
func TestPreviewEndToEnd(t *testing.T) {
	req, _, co, clk, id, log := previewPair(t, previewModeOn)
	ctx := context.Background()
	if mode, ok := co.Wanted(id); !ok || mode != previewModeOn {
		t.Fatalf("the provider did not record the round: %q %v", mode, ok)
	}
	co.Offer(id, "the first words", "and some thoughts", previewModeWithReasoning)
	if n, _ := flushPreviews(co, clk.now()); n != 1 || log.count() != 1 {
		t.Fatalf("%d frames, %d sent", n, log.count())
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	f, ok := req.previews().Current(id)
	if !ok || f.Text != "the first words" || f.Reasoning != "" || f.Answers != lastOwnText(t, req, id) {
		t.Fatalf("the requester shows %+v (%v)", f, ok)
	}
}

// CUX-P8, ENG-K20: the round's real answer cancels the frames not yet
// handed over, the round takes no more, and the requester, once it stored
// the answer, shows no preview of the round (CUX-P16).
func TestPreviewCanceledByReply(t *testing.T) {
	req, prov, co, clk, id, log := previewPair(t, previewModeOn)
	ctx := context.Background()
	co.Offer(id, "first", "", previewModeOn)
	flushPreviews(co, clk.now())
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := req.previews().Current(id); !ok {
		t.Fatal("the first frame is not shown")
	}
	clk.add(1000)
	co.Offer(id, "first and second", "", previewModeOn) // held for the gap
	sent := log.count()
	if err := prov.SendMessage(ctx, id, "the answer", nil); err != nil {
		t.Fatal(err)
	}
	if log.count() != sent+1 {
		t.Fatalf("%d envelopes for the answer", log.count()-sent)
	}
	clk.add(previewGapMS)
	if n, _ := flushPreviews(co, clk.now()); n != 0 {
		t.Fatalf("%d frames after the answer", n)
	}
	if co.Offer(id, "more", "", previewModeOn) {
		t.Fatal("the answered round takes an offer")
	}
	if n, _ := flushPreviews(co, clk.now()+previewGapMS); n != 0 || log.count() != sent+1 {
		t.Fatalf("frames went after the answer: %d", log.count()-sent-1)
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if f, ok := req.previews().Current(id); ok {
		t.Fatalf("the requester still shows %q after the answer", f.Text)
	}
}

// ENG-K18 (mutation M19, ENG-M7): a frame the hub turns away is not sent
// again, and no frame ever reaches the outbox.
func TestPreviewNotRetried(t *testing.T) {
	req, prov, co, clk, id, log := previewPair(t, previewModeOn)
	log.next = func(int, []byte) fakeSendAnswer {
		return fakeSendAnswer{kind: fakeSendRefuse, status: 503, code: "unavailable"}
	}
	rows, vers := outboxCounts(t, prov)
	co.Offer(id, "words", "", previewModeOn)
	if n, _ := flushPreviews(co, clk.now()); n != 1 || log.count() != 1 {
		t.Fatalf("%d frames, %d sends", n, log.count())
	}
	if r, v := outboxCounts(t, prov); r != rows || v != vers {
		t.Fatalf("the outbox went from %d rows, %d versions to %d, %d", rows, vers, r, v)
	}
	askHubNow(t, prov)
	for _, r := range allOutboxRows(t, prov) {
		_ = retryNow(t, prov, r.ID)
	}
	if log.count() != 1 {
		t.Fatalf("the frame was sent %d times", log.count())
	}
	if err := req.pollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := req.previews().Current(id); ok {
		t.Fatal("a frame the hub refused is shown")
	}
}

// ENG-V6, DLV-R-1a (mutation M21): frames the requester takes through the
// hub leave no delivery token there.
func TestPreviewEarnsNoToken(t *testing.T) {
	req, _, co, clk, id, _ := previewPair(t, previewModeOn)
	srvURL := req.configuredHub()
	h := fakeHubAt(t, srvURL)
	tokens := func() int {
		h.mu.Lock()
		defer h.mu.Unlock()
		return len(h.relayDacks)
	}
	rclk := frozenClock(req)
	before := tokens()
	for i := 0; i < 3; i++ {
		co.Offer(id, strings.Repeat("words ", i+1), "", previewModeOn)
		flushPreviews(co, clk.now())
		clk.add(previewGapMS)
		rclk.add(previewRxTaskGapMS)
		if err := req.pollOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if f, ok := req.previews().Current(id); !ok || f.Text != strings.Repeat("words ", i+1) {
			t.Fatalf("frame %d not taken: %+v", i, f)
		}
	}
	if n := tokens() - before; n != 0 {
		t.Fatalf("the hub keeps %d delivery tokens for preview frames", n)
	}
}

// SI-75 (mutation M24): a frame of an earlier round that arrives late —
// after the provider answered and the requester asked again — is not
// shown: it does not answer the requester's latest message.
func TestALatePreviewDoesNotOverwriteTheAnswer(t *testing.T) {
	_, req, prov := quietPair(t)
	ctx := context.Background()
	clk := frozenClock(req)
	id := previewDelegation(t, req, prov, previewModeOn)
	first := lastOwnText(t, req, id)
	seq := clk.now()
	late := craftPreview(t, prov, req, id, previewFrameMsg(first, seq+100, "round one, late"), nil)
	if r := receive(t, req, craftPreview(t, prov, req, id, previewFrameMsg(first, seq, "round one"), nil)); !taken(r) {
		t.Fatalf("the first round's frame: %+v", r)
	}
	if err := prov.SendMessage(ctx, id, "the answer to round one", nil); err != nil {
		t.Fatal(err)
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := req.previews().Current(id); ok {
		t.Fatal("the preview outlived the answer")
	}
	clk.add(previewRxTaskGapMS)
	if r := receive(t, req, late); taken(r) {
		t.Fatal("a frame of the answered round was taken after its answer")
	}
	if err := req.SendMessageOpts(ctx, id, "and now round two", nil, map[string]any{metaPreviewMode: previewModeOn}); err != nil {
		t.Fatal(err)
	}
	clk.add(previewRxTaskGapMS)
	late2 := craftPreview(t, prov, req, id, previewFrameMsg(first, seq+200, "round one, later still"), nil)
	if r := receive(t, req, late2); taken(r) {
		t.Fatal("a frame of the first round was taken in the second")
	}
	if f, ok := req.previews().Current(id); ok {
		t.Fatalf("the second round shows %q", f.Text)
	}
	second := lastOwnText(t, req, id)
	clk.add(previewRxTaskGapMS)
	if r := receive(t, req, craftPreview(t, prov, req, id, previewFrameMsg(second, seq+300, "round two"), nil)); !taken(r) {
		t.Fatalf("the second round's frame: %+v", r)
	}
}

// SI-75 (mutation M31): a frame again — the same one replayed, or an older
// one — is not taken: stream_seq must rise.
func TestPreviewReplayedFrameDropped(t *testing.T) {
	_, req, prov := quietPair(t)
	clk := frozenClock(req)
	id := previewDelegation(t, req, prov, previewModeOn)
	answers := lastOwnText(t, req, id)
	seq := clk.now()
	older := craftPreview(t, prov, req, id, previewFrameMsg(answers, seq, "older"), nil)
	newer := craftPreview(t, prov, req, id, previewFrameMsg(answers, seq+10, "newer"), nil)
	if r := receive(t, req, newer); !taken(r) {
		t.Fatalf("%+v", r)
	}
	for _, env := range [][]byte{newer, older} {
		clk.add(previewRxTaskGapMS)
		if r := receive(t, req, env); taken(r) {
			t.Fatal("a frame with a stream_seq not above the task's was taken")
		}
	}
	if f, _ := req.previews().Current(id); f.Text != "newer" {
		t.Fatalf("the preview shows %q", f.Text)
	}
}

// restartWithPreviews restarts prov with a coalescer on clk.
func restartWithPreviews(t *testing.T, prov *Daemon, clk *previewClock) (*Daemon, *memPreviews) {
	t.Helper()
	p := restartDaemon(t, prov)
	return p, manualPreviews(p, clk)
}

// CUX-P0, the release plan (J-CUX8): the provider restarts in the
// middle of a previewed round. The round sends no more frames — the
// request lived in memory only — and its result still arrives; the
// requester shows no preview once it has the result.
func TestPreviewRoundAfterRestartSendsNoFrames(t *testing.T) {
	req, prov, co, clk, id, log := previewPair(t, previewModeOn)
	ctx := context.Background()
	co.Offer(id, "before the restart", "", previewModeOn)
	flushPreviews(co, clk.now())
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := req.previews().Current(id); !ok {
		t.Fatal("the frame before the restart is not shown")
	}
	prov, co = restartWithPreviews(t, prov, clk)
	clk.add(previewGapMS)
	sent := log.count()
	if _, ok := co.Wanted(id); ok {
		t.Fatal("the restarted provider still holds the round")
	}
	if co.Offer(id, "after the restart", "", previewModeOn) {
		t.Fatal("the round takes an offer after the restart")
	}
	if n, _ := flushPreviews(co, clk.now()); n != 0 {
		t.Fatalf("%d frames after the restart", n)
	}
	if err := prov.completeWithReply(ctx, id, "the result", nil); err != nil {
		t.Fatal(err)
	}
	if log.count() != sent+1 {
		t.Fatalf("%d envelopes after the restart, want the result alone", log.count()-sent)
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	cur, err := req.ix.Get(id)
	if err != nil || cur.State != interactions.StateCompleted {
		t.Fatalf("the requester's task: %+v %v", cur, err)
	}
	if _, ok := req.previews().Current(id); ok {
		t.Fatal("the requester shows a preview after the result")
	}
}

// CUX-P9a (mutation M33, as the release plan rewrote it): after the
// provider restarted mid-round, the frames of the interaction's next round
// are taken — the restarted process starts stream_seq from the clock, not
// from 1, so they rise above the frames the requester saw before.
func TestPreviewNextRoundAfterRestart(t *testing.T) {
	req, prov, co, clk, id, _ := previewPair(t, previewModeOn)
	ctx := context.Background()
	rclk := frozenClock(req)
	co.Offer(id, "before the restart", "", previewModeOn)
	flushPreviews(co, clk.now())
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := req.previews().Current(id); !ok {
		t.Fatal("the frame before the restart is not shown")
	}
	prov, co = restartWithPreviews(t, prov, clk)
	if err := prov.SendMessage(ctx, id, "the answer to round one", nil); err != nil {
		t.Fatal(err)
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := req.SendMessageOpts(ctx, id, "round two", nil, map[string]any{metaPreviewMode: previewModeOn}); err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	clk.add(1)
	rclk.add(previewRxTaskGapMS)
	if !co.Offer(id, "round two so far", "", previewModeOn) {
		t.Fatal("the next round takes no offer")
	}
	if n, _ := flushPreviews(co, clk.now()); n != 1 {
		t.Fatalf("%d frames in the next round", n)
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	f, ok := req.previews().Current(id)
	if !ok || f.Text != "round two so far" || f.Answers != lastOwnText(t, req, id) {
		t.Fatalf("the next round's frame is not shown: %+v %v", f, ok)
	}
}

// CUX-P12, SI-37's one exception (KEL C-52): a preview frame is checked
// against the provider's stored KEL with no witness notice at all, so it
// is taken when a message sealed at the same time would be refused for a
// stale notice.
func TestPreviewAcceptedWithStaleNotice(t *testing.T) {
	_, req, prov := quietPair(t)
	id := previewDelegation(t, req, prov, previewModeOn)
	answers := lastOwnText(t, req, id)
	clk := frozenClock(req)
	clk.add(7 * 3600 * 1000) // the provider's notice is seven hours old
	later := func(in *seal.SealedInner) { in.TS = clk.now(); in.Exp = in.TS + previewLifeMS }
	if r := receive(t, req, craftPreview(t, prov, req, id, previewFrameMsg(answers, clk.now(), "late in the day"), later)); !taken(r) {
		t.Fatalf("a frame with no notice, seven hours on: %+v", r)
	}
	env, _ := craftWith(t, senderOf(prov), req, seal.TypeMessage, id, chatBody(t, "a message", "m-late"), craftOpt{
		peerRK: ownIXKey(t, req, id).RK(), edit: func(in *seal.SealedInner) {
			in.TS = clk.now()
			in.Exp = in.TS + messageLifetimeMS
			in.RK = rkFor(t, prov, id)
		}})
	if r := receive(t, req, env); !strings.HasPrefix(r.reason, "witness-") {
		t.Fatalf("a message with the same notice: %+v, want a witness refusal", r)
	}
}

// Step 4a (CUX-P12): a preview frame that carries key 10 (a KEL) is
// dropped as bad-kind-keys before step 9 — never shown, no token — when
// the same frame without it is taken.
func TestPreviewWithKey10Dropped(t *testing.T) {
	_, req, prov := quietPair(t)
	clk := frozenClock(req)
	id := previewDelegation(t, req, prov, previewModeOn)
	answers := lastOwnText(t, req, id)
	kel, err := identity.MarshalKEL(prov.self.KEL())
	if err != nil {
		t.Fatal(err)
	}
	notices := senderOf(prov).notices(t)
	withKEL := func(in *seal.SealedInner) { in.KEL, in.Notices = kel, notices }
	before := counter(req, dropBadKindKeys)
	if r := receive(t, req, craftPreview(t, prov, req, id, previewFrameMsg(answers, clk.now(), "with a KEL"), withKEL)); taken(r) ||
		r.reason != dropBadKindKeys {
		t.Fatalf("a frame with key 10: %+v", r)
	}
	if counter(req, dropBadKindKeys) != before+1 {
		t.Fatal("bad-kind-keys not counted")
	}
	if _, ok := req.previews().Current(id); ok {
		t.Fatal("the frame with key 10 is shown")
	}
	clk.add(previewRxTaskGapMS)
	if r := receive(t, req, craftPreview(t, prov, req, id, previewFrameMsg(answers, clk.now(), "without"), nil)); !taken(r) {
		t.Fatalf("the same frame without key 10: %+v", r)
	}
}

// CUX-P16: a requester's cache keeps a frame only until its task moves on;
// a frame judged while a clear happened is not kept (SI-75).
func TestPreviewCacheClears(t *testing.T) {
	clk := newPreviewClock()
	co, _, _ := unitPreviews(clk)
	in := func(ix string, seq uint64, epoch uint64) previewIn {
		return previewIn{IX: ix, From: "peer", Epoch: epoch, previewFrame: previewFrame{Answers: "m", Seq: seq, At: clk.now(),
			previewContent: previewContent{Text: "t"}}}
	}
	if !co.Arrived(in("a", 10, co.Epoch())) {
		t.Fatal("a first frame refused")
	}
	co.Clear("a")
	if _, ok := co.Current("a"); ok {
		t.Fatal("a cleared preview is shown")
	}
	// Judged before the clear, arriving after it.
	e := co.Epoch()
	clk.add(previewRxTaskGapMS)
	co.Clear("a")
	if co.Arrived(in("a", 20, e)) {
		t.Fatal("a frame judged before its task moved on was kept")
	}
	// A clear of another task does not hold this one back.
	e = co.Epoch()
	co.Clear("b")
	if !co.Arrived(in("a", 30, e)) {
		t.Fatal("a clear of another task refused the frame")
	}
	// The stream_seq high-water mark survives a clear.
	clk.add(previewRxTaskGapMS)
	co.Clear("a")
	if co.Arrived(in("a", 30, co.Epoch())) {
		t.Fatal("a replayed frame was taken after a clear")
	}
	// Forgotten clears are held against frames judged before them.
	e = co.Epoch()
	for i := 0; i <= previewClearsKept; i++ {
		co.Clear(fmt.Sprintf("x%d", i))
	}
	clk.add(previewRxTaskGapMS)
	if co.Arrived(in("c", 1, e)) {
		t.Fatal("a frame judged before a forgotten clear was kept")
	}
	if !co.Arrived(in("c", 1, co.Epoch())) {
		t.Fatal("a fresh frame refused")
	}
	// The cache is bounded.
	for i := 0; i < previewCacheTasks+10; i++ {
		clk.add(1000)
		co.Arrived(previewIn{IX: fmt.Sprintf("t%d", i), From: fmt.Sprintf("p%d", i), Epoch: co.Epoch(),
			previewFrame: previewFrame{Seq: 1, At: clk.now()}})
	}
	co.mu.Lock()
	n, l := len(co.seen), co.seenLRU.Len()
	co.mu.Unlock()
	if n != previewCacheTasks || l != previewCacheTasks {
		t.Fatalf("the cache holds %d tasks (%d listed)", n, l)
	}
}

// previewTail keeps the last MaxPreviewTail bytes at most, whole code
// points, valid UTF-8, and counts what it left out (CUX-P1).
func TestPreviewTail(t *testing.T) {
	limit := delegation.MaxPreviewTail
	for _, tc := range []struct {
		in        string
		body      int
		omitted   uint64
		validOnly bool
	}{
		{"", 0, 0, false},
		{"short", 5, 0, false},
		{strings.Repeat("a", limit), limit, 0, false},
		{strings.Repeat("a", limit+1), limit, 1, false},
		{"a" + strings.Repeat("€", limit/3+1), limit - limit%3, uint64(1 + 3*(limit/3+1) - (limit - limit%3)), false},
		{strings.Repeat("\xff", 10), 3, 0, true}, // one replacement for the run
	} {
		body, omitted := previewTail(tc.in)
		if !utf8.ValidString(body) || len(body) > limit {
			t.Fatalf("%d bytes in: a tail of %d bytes, valid %v", len(tc.in), len(body), utf8.ValidString(body))
		}
		if len(body) != tc.body || omitted != tc.omitted {
			t.Fatalf("%d bytes in: body %d omitted %d, want %d and %d", len(tc.in), len(body), omitted, tc.body, tc.omitted)
		}
		if !tc.validOnly && !strings.HasSuffix(tc.in, body) {
			t.Fatal("the tail is not the end of the text")
		}
	}
}

// FuzzPreviewModeOf: any metadata gives a mode that sends previews or none.
func FuzzPreviewModeOf(f *testing.F) {
	for _, s := range []string{`{"anet.preview_mode":"on"}`, `{"anet.preview_mode":"with_reasoning"}`, `{"anet.preview_mode":1}`,
		`{"anet.preview_mode":"on","anet.preview_mode":"off"}`, `null`, `{}`, ``, `{"anet.preview_mode":"on"}`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		got := previewModeOf(b)
		if got != "" && got != previewModeOn && got != previewModeWithReasoning {
			t.Fatalf("mode %q", got)
		}
		if got != "" {
			var m map[string]any
			if json.Unmarshal(b, &m) != nil || m[metaPreviewMode] != got {
				t.Fatalf("mode %q from %q", got, b)
			}
		}
	})
}

// FuzzPreviewTail: a tail is valid UTF-8, at most MaxPreviewTail bytes,
// the end of the text as made valid, and what it leaves out adds up.
func FuzzPreviewTail(f *testing.F) {
	f.Add("hello")
	f.Add(strings.Repeat("€", 3000))
	f.Add(strings.Repeat("a", 8191) + "𝄞")
	f.Add("\xff\xfe" + strings.Repeat("é", 5000))
	f.Fuzz(func(t *testing.T, s string) {
		body, omitted := previewTail(s)
		whole := strings.ToValidUTF8(s, "\uFFFD")
		if !utf8.ValidString(body) || len(body) > delegation.MaxPreviewTail || !strings.HasSuffix(whole, body) ||
			omitted+uint64(len(body)) != uint64(len(whole)) {
			t.Fatalf("%d bytes in: %d out, %d omitted", len(s), len(body), omitted)
		}
		if len(whole) > delegation.MaxPreviewTail && len(body) < delegation.MaxPreviewTail-utf8.UTFMax+1 {
			t.Fatalf("a tail of %d bytes from %d", len(body), len(whole))
		}
		cm := &delegation.ChatMsg{Kind: delegation.ChatStreamPreview, Body: body, StreamSeq: 1, StreamAtMS: 1,
			MsgID: hex.EncodeToString([]byte{1}), Answers: "m", Omitted: omitted}
		if err := delegation.ValidatePreview(cm); err != nil {
			t.Fatal(err)
		}
	})
}

// CUX-P16: a result that carries no reply (the provider ended the task
// without a last word) ends the task's preview too.
func TestPreviewClearedByAResultWithoutReply(t *testing.T) {
	req, prov, co, clk, id, _ := previewPair(t, previewModeOn)
	ctx := context.Background()
	co.Offer(id, "almost there", "", previewModeOn)
	flushPreviews(co, clk.now())
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := req.previews().Current(id); !ok {
		t.Fatal("the frame is not shown")
	}
	if err := prov.CompleteTask(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if cur, err := req.ix.Get(id); err != nil || !cur.IsTerminal() {
		t.Fatalf("the requester's task: %+v %v", cur, err)
	}
	if f, ok := req.previews().Current(id); ok {
		t.Fatalf("the requester shows %q after the result", f.Text)
	}
}
