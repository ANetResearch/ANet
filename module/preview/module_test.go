//go:build !no_preview

package preview

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/module"
)

// fakeHost is a Host with the preview grant and nothing else.
type fakeHost struct {
	module.Host
	seam *fakeSeam
	none bool
}

func (h fakeHost) PreviewSeam() (module.PreviewSeam, bool) {
	if h.none {
		return nil, false
	}
	return h.seam, true
}

type fakeSeam struct {
	mu      sync.Mutex
	mode    string
	now     uint64
	sent    []module.PreviewOut
	dropped int
	pub     []string
}

func (s *fakeSeam) Mode() string  { s.mu.Lock(); defer s.mu.Unlock(); return s.mode }
func (s *fakeSeam) NowMS() uint64 { return uint64(time.Now().UnixMilli()) }
func (s *fakeSeam) Dropped()      { s.mu.Lock(); s.dropped++; s.mu.Unlock() }
func (s *fakeSeam) SendFrame(_ context.Context, _ string, o module.PreviewOut) module.PreviewSendResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, o)
	return module.PreviewSendResult{Sent: true, Handed: true}
}
func (s *fakeSeam) Published(ix string) { s.mu.Lock(); s.pub = append(s.pub, ix); s.mu.Unlock() }

// The module needs the seam from its Host, declares the extension only
// while stream_preview is not off, contributes the /status member, and
// hands over a frame through the seam.
func TestModuleStartCardStatusAndFrame(t *testing.T) {
	if (&Module{}).Name() != "preview" {
		t.Fatal("the module's name is not its tag")
	}
	if err := (&Module{}).Start(context.Background(), fakeHost{none: true}); err == nil {
		t.Fatal("started without a preview seam")
	}
	if err := (&Module{}).Start(context.Background(), struct{ module.Host }{}); err == nil {
		t.Fatal("started on a Host that grants no PreviewHost")
	}
	seam := &fakeSeam{mode: "off"}
	m := &Module{}
	if err := m.Start(context.Background(), fakeHost{seam: seam}); err != nil {
		t.Fatal(err)
	}
	defer m.Stop(context.Background())

	if got := m.CardExtensions(module.CardContext{}); got != nil {
		t.Fatalf("declared the extension with stream_preview off: %v", got)
	}
	if k, v := m.StatusMember(); k != "preview" || v.(map[string]any)["mode"] != "off" {
		t.Fatalf("status member %q %v", k, v)
	}
	for _, mode := range []string{"on", "with_reasoning"} {
		seam.mu.Lock()
		seam.mode = mode
		seam.mu.Unlock()
		ext := m.CardExtensions(module.CardContext{})
		if len(ext) != 1 || ext[0]["uri"] != module.ExtPreviewURI {
			t.Fatalf("mode %s: extension %v", mode, ext)
		}
		if _, v := m.StatusMember(); v.(map[string]any)["mode"] != mode {
			t.Fatalf("mode %s: status %v", mode, v)
		}
	}
	if len(m.CardInterfaces(module.CardContext{})) != 0 {
		t.Fatal("the module contributes an interface")
	}

	// A round that asked for previews: the first change goes at once, through
	// the seam, as the round's frame.
	m.Requested("ix1", "m1", "on")
	if !m.Offer("ix1", "hello", "").Taken {
		t.Fatal("the offer was not taken")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		seam.mu.Lock()
		n := len(seam.sent)
		seam.mu.Unlock()
		if n == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	seam.mu.Lock()
	defer seam.mu.Unlock()
	if len(seam.sent) != 1 || seam.sent[0].Text != "hello" || seam.sent[0].Answers != "m1" || seam.sent[0].Seq == 0 {
		t.Fatalf("frames handed over: %+v", seam.sent)
	}
}

// After Stop nothing is handed over.
func TestModuleStopEndsTheCoalescer(t *testing.T) {
	seam := &fakeSeam{mode: "on"}
	m := &Module{}
	if err := m.Start(context.Background(), fakeHost{seam: seam}); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.Requested("ix1", "m1", "on")
	m.Offer("ix1", "late", "")
	time.Sleep(100 * time.Millisecond)
	seam.mu.Lock()
	defer seam.mu.Unlock()
	if len(seam.sent) != 0 {
		t.Fatalf("%d frames after Stop", len(seam.sent))
	}
}
