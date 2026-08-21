package main

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/module"
	_ "github.com/ANetResearch/ANet/module/p2p"
	"github.com/ANetResearch/ANet/provider"
)

// The client half and this half are the two sides of one contract, and
// both shipped the same deadlock: handling a request inline on the loop
// that has to deliver its answer. Testing them apart is what let that
// happen twice — module/p2p's suite drove a fake peer, and this had no
// suite at all. So these tests drive the real transport against the real
// peer process, in one program.

// host is the narrow face a transport module gets from the daemon.
type host struct {
	aid string
	mu  sync.Mutex
	trs []module.Transport
	in  module.Inbound
}

func (h *host) AID() string                      { return h.aid }
func (h *host) Providers() *provider.Registry    { return provider.NewRegistry() }
func (h *host) RecordEvidence(string, any) error { return nil }
func (h *host) ResolveKEL(string) ([]identity.SignedEvent, bool) {
	return nil, false
}
func (h *host) Inbound() module.Inbound { return h.in }
func (h *host) RegisterTransport(t module.Transport) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.trs = append(h.trs, t)
}
func (h *host) transport(t *testing.T) module.Transport {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.trs) != 1 {
		t.Fatalf("expected exactly one registered transport, got %d", len(h.trs))
	}
	return h.trs[0]
}

// mailbox records what arrived, and can answer over the transport the way
// a daemon answers a delegation.
type mailbox struct {
	mu      sync.Mutex
	got     []string
	replyOn func() module.Transport
	replied chan error
}

func (m *mailbox) Receive(ctx context.Context, from, kind, ix string, payload []byte) error {
	m.mu.Lock()
	m.got = append(m.got, fmt.Sprintf("%s/%s/%s/%s", from, kind, ix, payload))
	reply := m.replyOn
	m.mu.Unlock()
	if reply != nil && kind == "delegate" {
		err := reply().Send(ctx, from, "result", ix, []byte("the result"))
		m.replied <- err
		return err
	}
	return nil
}

func (m *mailbox) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.got)
}

func (m *mailbox) all() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.got...)
}

// node is one daemon-and-peer pair.
type node struct {
	aid  string
	host *host
	box  *mailbox
}

func newNode(t *testing.T, dir, rendezvous, name string, box *mailbox) *node {
	t.Helper()
	p, err := start(
		filepath.Join(dir, name+".sock"),
		filepath.Join(dir, name+".wire"),
		rendezvous)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.stop)

	h := &host{aid: "aid-" + name, in: box}
	mods, err := module.Build(map[string][]byte{
		"p2p": []byte(fmt.Sprintf(`{"socket":%q,"dial_timeout_ms":2000,"reach_timeout_ms":500}`,
			filepath.Join(dir, name+".sock"))),
	})
	if err != nil {
		t.Fatal(err)
	}
	var started bool
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	for _, m := range mods {
		if m.Name() != "p2p" {
			continue
		}
		if err := m.Start(ctx, h); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = m.Stop(context.Background()) })
		started = true
	}
	if !started {
		t.Fatal("the p2p module was not built — is this a no_p2p build?")
	}
	return &node{aid: h.aid, host: h, box: box}
}

func waitFor(t *testing.T, why string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", why)
}

// Two nodes find each other through the rendezvous and deliver both ways —
// including the case that matters: the receiver answering a delegation
// over the same connection it arrived on.
func TestTwoNodesDelegateAndAnswerDirectly(t *testing.T) {
	dir := t.TempDir()
	rv := filepath.Join(dir, "rv")

	alice := newNode(t, dir, rv, "alice", &mailbox{})
	bobBox := &mailbox{replied: make(chan error, 1)}
	bob := newNode(t, dir, rv, "bob", bobBox)
	bobBox.mu.Lock()
	bobBox.replyOn = func() module.Transport { return bob.host.transport(t) }
	bobBox.mu.Unlock()

	at := alice.host.transport(t)
	waitFor(t, "the peers to announce themselves", func() bool {
		return at.Reachable(context.Background(), bob.aid)
	})

	if err := at.Send(context.Background(), bob.aid, "delegate", "ix-1", []byte("do the thing")); err != nil {
		t.Fatalf("delivery failed: %v", err)
	}
	if got := bobBox.all(); len(got) != 1 || got[0] != "aid-alice/delegate/ix-1/do the thing" {
		t.Fatalf("bob received %v", got)
	}
	// Bob answered inside Receive, over his own transport. If either half
	// handles its request inline on its read loop, this is where it hangs.
	select {
	case err := <-bobBox.replied:
		if err != nil {
			t.Fatalf("answering over the same transport failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bob never finished answering — deadlocked")
	}
	waitFor(t, "the result to reach alice", func() bool { return alice.box.count() == 1 })
	if got := alice.box.all(); got[0] != "aid-bob/result/ix-1/the result" {
		t.Errorf("alice received %v", got)
	}
}

// An AID nobody carries is unreachable, promptly, so the daemon falls
// through to the hub instead of waiting.
func TestAnUnknownAIDIsUnreachable(t *testing.T) {
	dir := t.TempDir()
	alice := newNode(t, dir, filepath.Join(dir, "rv"), "alice", &mailbox{})
	at := alice.host.transport(t)

	start := time.Now()
	if at.Reachable(context.Background(), "aid-nobody") {
		t.Error("an AID no peer carries must not be reported reachable")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("answering 'unreachable' took %s — the hub is waiting behind this", d)
	}
	if err := at.Send(context.Background(), "aid-nobody", "delegate", "ix-1", []byte("x")); err == nil {
		t.Error("delivering to nobody must fail, so the caller retries elsewhere")
	}
	// And a node must not consider itself reachable through the network.
	if at.Reachable(context.Background(), alice.aid) {
		t.Error("a node reported itself reachable through its own peer")
	}
}

// Concurrent deliveries in both directions at once: the shape that
// interleaves frames on a shared connection and mismatches replies.
func TestConcurrentTrafficBothWays(t *testing.T) {
	dir := t.TempDir()
	rv := filepath.Join(dir, "rv")
	alice := newNode(t, dir, rv, "alice", &mailbox{})
	bob := newNode(t, dir, rv, "bob", &mailbox{})

	at, bt := alice.host.transport(t), bob.host.transport(t)
	waitFor(t, "the peers to find each other", func() bool {
		return at.Reachable(context.Background(), bob.aid) &&
			bt.Reachable(context.Background(), alice.aid)
	})

	const n = 12
	var wg sync.WaitGroup
	errs := make([]error, 2*n)
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			errs[i] = at.Send(context.Background(), bob.aid, "message",
				fmt.Sprintf("a-%d", i), []byte("from alice"))
		}(i)
		go func(i int) {
			defer wg.Done()
			errs[n+i] = bt.Send(context.Background(), alice.aid, "message",
				fmt.Sprintf("b-%d", i), []byte("from bob"))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("delivery %d failed: %v", i, err)
		}
	}
	waitFor(t, "every message to arrive", func() bool {
		return alice.box.count() == n && bob.box.count() == n
	})
}
