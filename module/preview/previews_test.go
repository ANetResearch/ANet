package preview

import (
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ANetResearch/ANetCore/delegation"
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
	out []Frame
	at  []uint64
}

func (s *sentFrames) add(o Frame, at uint64) {
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
func unitPreviews(clk *previewClock) (*Previews, *sentFrames, map[string]int) {
	sent := &sentFrames{}
	counts := map[string]int{}
	var mu sync.Mutex
	co := New(clk.now, func(o Frame) { sent.add(o, clk.now()) }, func(s string) {
		mu.Lock()
		counts[s]++
		mu.Unlock()
	})
	return co, sent, counts
}

// flushPreviews hands over, in this goroutine, every frame of co due now:
// what Run does on a wake-up. It returns how many went and when the next
// falls due.
func flushPreviews(co *Previews, now uint64) (int, uint64) { return co.Flush(now) }

func TestCoalescerCadence(t *testing.T) {
	clk := newPreviewClock()
	co, sent, counts := unitPreviews(clk)
	tick := func() int { n, _ := flushPreviews(co, clk.now()); return n }
	co.Requested("ix1", "m1", previewModeOn)

	if !co.offer("ix1", "a", "", previewModeOn) || tick() != 1 {
		t.Fatal("the first change of a round did not go at once")
	}
	first := clk.now()
	for i := 1; i <= 4; i++ {
		clk.add(1000)
		co.offer("ix1", strings.Repeat("a", i+1), "", previewModeOn)
		if n := tick(); n != 0 {
			t.Fatalf("a frame %d ms after the last", clk.sinceMS(first))
		}
	}
	if _, next := co.Due(clk.now()); next != first+previewGapMS {
		t.Fatalf("the next frame falls due at %d, want %d", next, first+previewGapMS)
	}
	clk.add(1000)
	if tick() != 1 || sent.out[1].Text != "aaaaa" {
		t.Fatalf("five seconds on: %d frames, the last %+v", sent.n(), sent.out[len(sent.out)-1].PreviewContent)
	}
	// Nothing changed: nothing goes, however long.
	co.offer("ix1", "aaaaa", "", previewModeOn)
	for i := 0; i < 4; i++ {
		clk.add(5000)
		if tick() != 0 {
			t.Fatal("a frame went with nothing changed")
		}
	}
	// After a quiet spell a change goes at once.
	co.offer("ix1", "aaaaab", "", previewModeOn)
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
		co.offer(ix, "x", "", previewModeOn)
	}
	before := sent.n()
	if n := tick(); n != previewNodePerSecond || counts[CountDropped] != 5 {
		t.Fatalf("%d frames in one second, %d dropped", n, counts[CountDropped])
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
			co.offer("ix", text, "", previewModeOn)
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

func TestPreviewCacheClears(t *testing.T) {
	clk := newPreviewClock()
	co, _, _ := unitPreviews(clk)
	in := func(ix string, seq uint64, epoch uint64) previewIn {
		return previewIn{IX: ix, From: "peer", Epoch: epoch, PreviewSnapshot: previewFrame{Answers: "m", Seq: seq, At: clk.now(),
			PreviewContent: previewContent{Text: "t"}}}
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
			PreviewSnapshot: previewFrame{Seq: 1, At: clk.now()}})
	}
	co.mu.Lock()
	n, l := len(co.seen), co.seenLRU.Len()
	co.mu.Unlock()
	if n != previewCacheTasks || l != previewCacheTasks {
		t.Fatalf("the cache holds %d tasks (%d listed)", n, l)
	}
}

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
