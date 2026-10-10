//go:build !no_p2p

// These tests drive the real module/p2p transport against this peer
// process, so they exist only in a build that has p2p. Without the tag
// they broke `go test -tags no_p2p ./...` — the peer binary itself does
// not import the module, only its test does.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/module"
	"github.com/ANetResearch/ANet/module/moduletest"
	"github.com/ANetResearch/ANet/module/p2p"
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
	moduletest.NopHost
	aid string
	mu  sync.Mutex
	trs []module.Transport
	in  module.Inbound
	bad atomic.Int32 // malformed confirmations the transport counted
}

func (h *host) AID() string                   { return h.aid }
func (h *host) Providers() *provider.Registry { return provider.NewRegistry() }
func (h *host) Inbound() module.Inbound       { return h.in }
func (h *host) NoteBadDAck()                  { h.bad.Add(1) }

// P2PAddr gives every AID an address: these nodes run on a rendezvous
// directory, which a send dials instead of the address (a peer process in
// test mode), so the address is only what the frame must carry.
func (h *host) P2PAddr(aid string) (string, bool) { return "203.0.113.7:39100", aid != "" }
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
// a daemon answers a delegation. An envelope starting with "delegate:"
// is answered with "result:<rest>" to replyTo; one listed in refuse is
// refused temporarily (Receive returns an error, so no ack); one listed in
// dacks is acked with that delivery confirmation.
type mailbox struct {
	mu      sync.Mutex
	got     []string
	replyOn func() module.Transport
	replyTo string
	replied chan error
	refuse  map[string]bool
	dacks   map[string][]byte
	// hold, when set, keeps Receive from answering until it is closed: a
	// daemon still at work when the hand-off times out.
	hold chan struct{}
	// gate, when set, runs first for every envelope; the test steers the
	// order of concurrent deliveries with it.
	gate func(env string)
}

func (m *mailbox) Receive(ctx context.Context, env []byte) ([]byte, error) {
	m.mu.Lock()
	hold, gate := m.hold, m.gate
	m.mu.Unlock()
	if gate != nil {
		gate(string(env))
	}
	if hold != nil {
		<-hold
	}
	m.mu.Lock()
	if m.refuse[string(env)] {
		m.mu.Unlock()
		return nil, fmt.Errorf("temporarily refused")
	}
	m.got = append(m.got, string(env))
	reply, to, dack := m.replyOn, m.replyTo, m.dacks[string(env)]
	m.mu.Unlock()
	if reply != nil && strings.HasPrefix(string(env), "delegate:") {
		_, _, err := reply().Send(ctx, to, []byte("result:"+strings.TrimPrefix(string(env), "delegate:")))
		m.replied <- err
		return nil, err
	}
	return dack, nil
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
	return newNodeAt(t, dir, rendezvous, name, filepath.Join(dir, name+".wire"), box)
}

// newNodeAt is newNode with the peer-facing address chosen by the caller,
// so a test can put the peer wire on TCP instead of a Unix socket.
func newNodeAt(t *testing.T, dir, rendezvous, name, peerAddr string, box *mailbox) *node {
	t.Helper()
	p, err := start(
		filepath.Join(dir, name+".sock"),
		peerAddr,
		rendezvous, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.stop)

	h := &host{aid: "aid-" + name, in: box}
	mods, err := module.Build(map[string][]byte{
		"p2p": []byte(fmt.Sprintf(`{"socket":%q,"dial_timeout_ms":2000}`,
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

// waitAnnounced waits until both nodes' transports are up and each one's
// peer process has announced it in the directory rendezvous.
func waitAnnounced(t *testing.T, rv string, nodes ...*node) {
	t.Helper()
	waitFor(t, "the peers to announce themselves", func() bool {
		for _, n := range nodes {
			if _, err := os.Stat(filepath.Join(rv, n.aid)); err != nil {
				return false
			}
			if !n.host.transport(t).Reachable(context.Background(), "aid-any") {
				return false
			}
		}
		return true
	})
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
	bobBox := &mailbox{replied: make(chan error, 1), replyTo: "aid-alice"}
	bob := newNode(t, dir, rv, "bob", bobBox)
	bobBox.mu.Lock()
	bobBox.replyOn = func() module.Transport { return bob.host.transport(t) }
	bobBox.mu.Unlock()

	at := alice.host.transport(t)
	waitAnnounced(t, rv, alice, bob)

	if _, handed, err := at.Send(context.Background(), bob.aid, []byte("delegate:do the thing")); err != nil || !handed {
		t.Fatalf("delivery failed: handed=%v err=%v", handed, err)
	}
	if got := bobBox.all(); len(got) != 1 || got[0] != "delegate:do the thing" {
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
	if got := alice.box.all(); got[0] != "result:do the thing" {
		t.Errorf("alice received %v", got)
	}
}

// Two deliveries to the same daemon at once, one of which the daemon
// refuses temporarily: each sender learns the outcome of its own delivery.
// The peer keys the daemon's ack on the ID it minted for each delivery; an
// ack keyed on anything the two share would tell the refused sender its
// message was delivered, and it would never fall back to the hub (C5).
//
// Deterministic: bob's daemon holds "first" until "second" is in its hands
// too (so both wait at bob's peer at once), acks "first", and refuses
// "second" only after alice has heard the answer to "first". An ack that
// released every waiting delivery would answer "second" as delivered
// before its refusal exists (si10-8).
func TestConcurrentDeliveriesGetTheirOwnOutcome(t *testing.T) {
	dir := t.TempDir()
	rv := filepath.Join(dir, "rv")
	alice := newNode(t, dir, rv, "alice", &mailbox{})
	secondIn := make(chan struct{})
	releaseSecond := make(chan struct{})
	bobBox := &mailbox{refuse: map[string]bool{"second": true}, gate: func(env string) {
		switch env {
		case "first":
			select {
			case <-secondIn:
			case <-time.After(5 * time.Second):
			}
		case "second":
			close(secondIn)
			select {
			case <-releaseSecond:
			case <-time.After(5 * time.Second):
			}
		}
	}}
	bob := newNode(t, dir, rv, "bob", bobBox)
	at := alice.host.transport(t)
	waitAnnounced(t, rv, alice, bob)

	type outcome struct {
		handed bool
		err    error
	}
	first, second := make(chan outcome, 1), make(chan outcome, 1)
	go func() {
		_, h, err := at.Send(context.Background(), bob.aid, []byte("first"))
		first <- outcome{h, err}
	}()
	go func() {
		_, h, err := at.Send(context.Background(), bob.aid, []byte("second"))
		second <- outcome{h, err}
	}()

	var f outcome
	select {
	case f = <-first:
	case <-time.After(5 * time.Second):
		t.Fatal("the accepted delivery was never answered")
	}
	if f.err != nil {
		t.Errorf("the accepted delivery reported failure: %v", f.err)
	}
	// "second" is still in bob's daemon: nothing may have answered it yet.
	select {
	case s := <-second:
		t.Fatalf("the delivery bob's daemon still holds was answered (err=%v): the ack of another delivery released it", s.err)
	default:
	}
	close(releaseSecond)
	var s outcome
	select {
	case s = <-second:
	case <-time.After(5 * time.Second):
		t.Fatal("the refused delivery was never answered")
	}
	if s.err == nil {
		t.Error("the refused delivery was reported delivered")
	}
	if !s.handed || !f.handed {
		t.Errorf("handed: first %v, second %v; both reached the peer process", f.handed, s.handed)
	}
}

// A rendezvous entry that leads to another node's peer process (the
// recipient moved and its address was reused, or two identities share a
// host) is refused there: the frame is for an AID that process does not
// carry. The sender's Send fails, so its daemon falls through to the hub,
// and the other daemon never sees the envelope ([redteam:F22]).
func TestADeliveryForAnAIDThisPeerDoesNotCarryIsRefused(t *testing.T) {
	dir := t.TempDir()
	rv := filepath.Join(dir, "rv")
	alice := newNode(t, dir, rv, "alice", &mailbox{})
	bobBox := &mailbox{}
	bob := newNode(t, dir, rv, "bob", bobBox)
	at := alice.host.transport(t)
	waitAnnounced(t, rv, alice, bob)
	// A stale entry: the victim's address is now bob's wire.
	if err := os.WriteFile(filepath.Join(rv, "aid-victim"), []byte(filepath.Join(dir, "bob.wire")), 0o644); err != nil {
		t.Fatal(err)
	}
	dack, _, err := at.Send(context.Background(), "aid-victim", []byte("delegate:for the victim"))
	if err == nil {
		t.Fatal("a delivery to the wrong peer process was reported delivered; the sender would not use the hub")
	}
	if dack != nil {
		t.Fatalf("a refused delivery came back with dack %x", dack)
	}
	if !strings.Contains(err.Error(), "does not carry") {
		t.Fatalf("error = %v, want the peer to say it does not carry the AID", err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := bobBox.count(); n != 0 {
		t.Fatalf("bob's daemon received %d envelopes meant for another AID", n)
	}
	// Bob's own traffic is unaffected.
	if _, _, err := at.Send(context.Background(), bob.aid, []byte("for bob")); err != nil {
		t.Fatalf("delivery to bob: %v", err)
	}
}

// A receiving daemon that refuses a delivery for now says so (nack), and
// the sender hears it at once with the daemon's reason instead of after
// the whole hand-off timeout: its daemon falls back to the hub now
// (an internal note item).
func TestARefusalReachesTheSenderAtOnce(t *testing.T) {
	dir := t.TempDir()
	rv := filepath.Join(dir, "rv")
	alice := newNode(t, dir, rv, "alice", &mailbox{})
	bobBox := &mailbox{refuse: map[string]bool{"not now": true}}
	bob := newNode(t, dir, rv, "bob", bobBox)
	at := alice.host.transport(t)
	waitAnnounced(t, rv, alice, bob)
	start := time.Now()
	dack, _, err := at.Send(context.Background(), bob.aid, []byte("not now"))
	if err == nil {
		t.Fatal("a refused delivery was reported delivered")
	}
	if dack != nil {
		t.Fatalf("a refused delivery came back with dack %x", dack)
	}
	if took := time.Since(start); took > handOffTimeout/2 {
		t.Fatalf("the refusal took %s to reach the sender; the hand-off timeout is %s", took, handOffTimeout)
	}
	if !strings.Contains(err.Error(), "temporarily refused") {
		t.Fatalf("error %v does not carry the receiving daemon's reason", err)
	}
}

// One delivery, however it goes, ends inside the time the sending daemon
// waits for it (module/p2p's default send timeout). A daemon that gave up
// first would send the envelope through the hub while this process could
// still get it through, and the recipient would have it twice
// (an internal note N3, an internal note item).
func TestOneDeliveryEndsInsideTheDaemonsSendTimeout(t *testing.T) {
	bound := dialTimeout + handOffTimeout + replyMargin
	if bound >= p2p.DefaultSendTimeout {
		t.Fatalf("a delivery may take %s here, the daemon waits %s", bound, p2p.DefaultSendTimeout)
	}
	if dialTimeout > 5*time.Second {
		t.Fatalf("dial timeout %s: an address that cannot be dialled costs that much before the hub is tried", dialTimeout)
	}
}

// T49 through a real peer process: once the send frame is in its peer
// process's hands, a send has handed the envelope over, however it then
// fails — no address, a dial that fails, a refusal at the hand-off, a
// hand-off that timed out while the receiving daemon was still at work.
// What the peer process answers cannot unsend it, and no answer without the
// recipient's dack is a confirmation ([redteam:F12]). Only with no peer
// process to hand it to has nothing left.
func TestAFailedSendIsHandedOnceThePeerProcessHasIt(t *testing.T) {
	old := handOffTimeout
	handOffTimeout = 300 * time.Millisecond
	t.Cleanup(func() { handOffTimeout = old })
	dir := t.TempDir()
	rv := filepath.Join(dir, "rv")
	alice := newNode(t, dir, rv, "alice", &mailbox{})
	hold := make(chan struct{})
	bobBox := &mailbox{refuse: map[string]bool{"not now": true}}
	bob := newNode(t, dir, rv, "bob", bobBox)
	at := alice.host.transport(t)
	waitAnnounced(t, rv, alice, bob)

	check := func(what string, dack []byte, handed bool, err error) {
		t.Helper()
		if err == nil || !handed || dack != nil {
			t.Errorf("%s: dack=%x handed=%v err=%v; want a failure that was handed over, without a dack", what, dack, handed, err)
		}
	}
	// No address at all.
	dack, handed, err := at.Send(context.Background(), "aid-nobody", []byte("x"))
	check("no address", dack, handed, err)
	// A dial that fails.
	if err := os.WriteFile(filepath.Join(rv, "aid-far"), []byte("tcp://"+closedTCP(t)), 0o644); err != nil {
		t.Fatal(err)
	}
	dack, handed, err = at.Send(context.Background(), "aid-far", []byte("x"))
	check("failed dial", dack, handed, err)
	// A refusal at the hand-off (nack).
	dack, handed, err = at.Send(context.Background(), bob.aid, []byte("not now"))
	check("refused hand-off", dack, handed, err)
	// A hand-off that timed out: the daemon may yet have taken it.
	bobBox.mu.Lock()
	bobBox.hold = hold
	bobBox.mu.Unlock()
	dack, handed, err = at.Send(context.Background(), bob.aid, []byte("slow"))
	close(hold)
	check("timed-out hand-off", dack, handed, err)

	// No peer process at all: nothing leaves.
	h := &host{aid: "aid-carol", in: &mailbox{}}
	mods, err := module.Build(map[string][]byte{
		"p2p": []byte(fmt.Sprintf(`{"socket":%q}`, filepath.Join(dir, "nobody.sock"))),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	for _, m := range mods {
		if m.Name() == "p2p" {
			if err := m.Start(ctx, h); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = m.Stop(context.Background()) })
		}
	}
	if dack, handed, err := h.transport(t).Send(context.Background(), bob.aid, []byte("x")); err == nil || handed || dack != nil {
		t.Errorf("no peer process: dack=%x handed=%v err=%v; want a failure that handed nothing over", dack, handed, err)
	}
}

// A delivery frame of another version — none at all from a peer that
// predates sealed envelopes, 2 from one that still speaks frame v2 — gets an
// error naming the version this peer speaks, and the daemon never sees it
// (core CORE-V3; A2A-DESIGN §3.10).
func TestAnUnversionedDeliveryIsRefused(t *testing.T) {
	dir := t.TempDir()
	box := &mailbox{}
	newNode(t, dir, filepath.Join(dir, "rv"), "bob", box)
	for _, f := range []map[string]any{
		{"op": "send", "id": "old-1", "to": "aid-bob", "payload": "cGxhaW50ZXh0"},
		{"op": "send", "v": 2, "id": "old-2", "to": "aid-bob", "envelope": "ZW52ZWxvcGU"},
	} {
		c, err := net.Dial("unix", filepath.Join(dir, "bob.wire"))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.NewEncoder(c).Encode(f); err != nil {
			t.Fatal(err)
		}
		var reply frame
		if err := json.NewDecoder(c).Decode(&reply); err != nil {
			t.Fatal(err)
		}
		c.Close()
		v, _ := f["v"].(int)
		if reply.Op != "error" || reply.V != wireVersion || reply.ID != f["id"] || reply.Error != versionRefused(v) || reply.DAck.set {
			t.Fatalf("reply = %+v, want an error naming the frame version", reply)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if n := box.count(); n != 0 {
		t.Fatalf("the daemon received %d deliveries of another version", n)
	}
}

// One source cannot use the whole inbound budget: past its burst, its
// deliveries are refused before they reach the daemon and it falls back to
// the hub (A2A-DESIGN §3.6 step 0).
func TestASourceOverItsRateIsRefused(t *testing.T) {
	l := newSourceLimits()
	now := time.Now()
	for i := 0; i < sourceBurst; i++ {
		if !l.allow("10.0.0.1", now) {
			t.Fatalf("delivery %d inside the burst was refused", i)
		}
	}
	if l.allow("10.0.0.1", now) {
		t.Fatal("a source past its burst was allowed")
	}
	if !l.allow("10.0.0.2", now) {
		t.Fatal("another source was charged for the first one's traffic")
	}
	if !l.allow("10.0.0.1", now.Add(time.Second)) {
		t.Fatal("the bucket does not refill")
	}
}

// The per-source limit is applied to every delivery from another peer,
// before the delivery is handed to the daemon: past its burst a source gets
// an error and its sender falls back to the hub (A2A-DESIGN §3.6 step 0).
func TestDeliveriesFromOneSourceAreLimited(t *testing.T) {
	p := &peer{acks: map[string]chan handOffAnswer{}, limits: newSourceLimits()}
	f := frame{Op: "send", V: wireVersion, ID: "x", To: "aid-self", Envelope: "ZW52"}
	for i := 0; i < sourceBurst; i++ {
		if out := p.receive("10.0.0.9", f); strings.Contains(out.Error, "rate limited") {
			t.Fatalf("delivery %d inside the burst was rate limited", i)
		}
	}
	out := p.receive("10.0.0.9", f)
	if out.Op != "error" || !strings.Contains(out.Error, "rate limited") {
		t.Fatalf("delivery past the burst: %+v, want a rate-limit error", out)
	}
	if out := p.receive("10.0.0.10", f); strings.Contains(out.Error, "rate limited") {
		t.Fatal("another source was charged for the first one's deliveries")
	}
}

// A send to an AID nobody carries fails promptly, so the daemon falls
// through to the hub instead of waiting. So does a send to this node
// itself: it is not reachable through its own peer.
func TestAnAIDNobodyCarriesFailsPromptly(t *testing.T) {
	dir := t.TempDir()
	rv := filepath.Join(dir, "rv")
	alice := newNode(t, dir, rv, "alice", &mailbox{})
	at := alice.host.transport(t)
	waitAnnounced(t, rv, alice)

	for _, aid := range []string{"aid-nobody", alice.aid} {
		start := time.Now()
		if _, _, err := at.Send(context.Background(), aid, []byte("x")); err == nil {
			t.Errorf("delivering to %s must fail, so the caller retries elsewhere", aid)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("failing a send to %s took %s — the hub is waiting behind this", aid, d)
		}
	}
	if n := alice.box.count(); n != 0 {
		t.Errorf("a send to itself reached alice's own daemon %d times", n)
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
	waitAnnounced(t, rv, alice, bob)

	const n = 12
	var wg sync.WaitGroup
	errs := make([]error, 2*n)
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			_, _, errs[i] = at.Send(context.Background(), bob.aid, []byte(fmt.Sprintf("a-%d", i)))
		}(i)
		go func(i int) {
			defer wg.Done()
			_, _, errs[n+i] = bt.Send(context.Background(), alice.aid, []byte(fmt.Sprintf("b-%d", i)))
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

// An address form must not be guessed wrongly: a path is a socket, a
// host:port is TCP, and an explicit scheme wins over both.
func TestAddrKindReadsTheAddressForm(t *testing.T) {
	for _, tc := range []struct{ in, net, addr string }{
		{"/run/peer/a.sock", "unix", "/run/peer/a.sock"},
		{"tcp://10.0.0.1:4100", "tcp", "10.0.0.1:4100"},
		{"unix:///run/a.sock", "unix", "/run/a.sock"},
		{"10.0.0.1:4100", "tcp", "10.0.0.1:4100"},
		{"peer.sock", "unix", "peer.sock"},
		// A path that also contains a colon is still a path: guessing TCP
		// here would make a peer listen on the network by accident.
		{"/run/peer:1/a.sock", "unix", "/run/peer:1/a.sock"},
	} {
		n, a := addrKind(tc.in)
		if n != tc.net || a != tc.addr {
			t.Errorf("addrKind(%q) = %q,%q want %q,%q", tc.in, n, a, tc.net, tc.addr)
		}
	}
}

// Peers that are not on the same machine.
//
// The peer wire was a Unix socket, so two peers had to share a
// filesystem. That made this process untestable across a real network: it
// could carry traffic between daemons on one host and nothing else, and
// the one module that changes how a delegation travels was the one module
// no live run could exercise.
//
// Loopback TCP rather than two machines, so this proves the wire and not
// the routing. What it establishes is that a peer can listen on and dial
// an address another machine could reach, which was structurally
// impossible before.
func TestPeersReachEachOtherOverTCP(t *testing.T) {
	dir := t.TempDir()
	rv := filepath.Join(dir, "rv")

	aliceBox := &mailbox{}
	bobBox := &mailbox{}
	alice := newNodeAt(t, dir, rv, "alice", freeTCP(t), aliceBox)
	bob := newNodeAt(t, dir, rv, "bob", freeTCP(t), bobBox)
	_ = alice
	_ = bob

	at := alice.host.transport(t)
	waitAnnounced(t, rv, alice, bob)
	if _, _, err := at.Send(context.Background(), bob.aid, []byte("over tcp")); err != nil {
		t.Fatalf("delivery over TCP failed: %v", err)
	}
	if got := bobBox.all(); len(got) != 1 || got[0] != "over tcp" {
		t.Fatalf("bob received %v", got)
	}
	// And back, because a transport that works one way is half a
	// transport: the answer travels the same wire.
	bt := bob.host.transport(t)
	if _, _, err := bt.Send(context.Background(), alice.aid, []byte("answer")); err != nil {
		t.Fatalf("the answer did not travel back over TCP: %v", err)
	}
	if got := aliceBox.all(); len(got) != 1 || got[0] != "answer" {
		t.Fatalf("alice received %v", got)
	}
}

// freeTCP picks a loopback port the OS is not using and gives it back, so
// the peer can bind it.
func freeTCP(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// A peer reads another's rendezvous entry while that one writes it. The
// entry was written in place, so a reader could find it created or
// truncated and not yet written, and dialled the empty address it read
// ("dial unix: missing address"), marking the peer down. An entry is now
// replaced in one step, and a reader sees the old address or the new one.
func TestARendezvousEntryIsNeverReadHalfWritten(t *testing.T) {
	rv := t.TempDir()
	writer := &peer{rendezvous: rv, peerSocket: "tcp://127.0.0.1:1111"}
	reader := &peer{rendezvous: rv}
	writer.announce("aid-bob")

	stop := make(chan struct{})
	bad := make(chan string, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			addr, ok := reader.lookup("aid-bob")
			if !ok || (addr != "tcp://127.0.0.1:1111" && addr != "tcp://127.0.0.1:2222") {
				select {
				case bad <- fmt.Sprintf("%q %v", addr, ok):
				default:
				}
				return
			}
		}
	}()
	for i := 0; i < 2000; i++ {
		writer.advertise = []string{"tcp://127.0.0.1:1111", "tcp://127.0.0.1:2222"}[i%2]
		writer.announce("aid-bob")
	}
	close(stop)
	wg.Wait()
	select {
	case got := <-bad:
		t.Fatalf("a peer read the entry as %s while it was being written", got)
	default:
	}
	ents, err := os.ReadDir(rv)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != "aid-bob" {
		t.Fatalf("the rendezvous holds %v, want only the entry", ents)
	}
}

// An empty entry names nowhere to dial: the peer is not listed, rather
// than listed at "" (which was dialled as a Unix socket and marked down).
func TestAnEmptyRendezvousEntryIsNotAnAddress(t *testing.T) {
	rv := t.TempDir()
	if err := os.WriteFile(filepath.Join(rv, "aid-bob"), []byte("\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := &peer{rendezvous: rv}
	if addr, ok := p.lookup("aid-bob"); ok {
		t.Fatalf("an empty entry reads as the address %q", addr)
	}
	if _, err := p.deliver(frame{To: "aid-bob", ID: "1", Envelope: "eA"}); err == nil {
		t.Fatal("a peer whose entry is empty was delivered to")
	}
}

// T43: the recipient daemon's delivery confirmation crosses the whole path
// — bob's daemon, bob's peer, alice's peer, alice's daemon — byte for byte,
// on the answer to the send that carried the envelope. Nothing on the way
// makes one: an envelope bob's daemon acks without one comes back without.
func TestTheRecipientsDAckTravelsBackUnchanged(t *testing.T) {
	dir := t.TempDir()
	rv := filepath.Join(dir, "rv")
	alice := newNode(t, dir, rv, "alice", &mailbox{})
	dack := make([]byte, module.DAckLen)
	for i := range dack {
		dack[i] = byte(0xff - i*7) // both base64 alphabets differ on these
	}
	bobBox := &mailbox{dacks: map[string][]byte{"with a dack": dack}}
	bob := newNode(t, dir, rv, "bob", bobBox)
	at := alice.host.transport(t)
	waitAnnounced(t, rv, alice, bob)

	got, handed, err := at.Send(context.Background(), bob.aid, []byte("with a dack"))
	if err != nil || !handed {
		t.Fatalf("send: handed=%v err=%v", handed, err)
	}
	if !bytes.Equal(got, dack) {
		t.Fatalf("alice got dack %x, bob's daemon made %x", got, dack)
	}
	got, _, err = at.Send(context.Background(), bob.aid, []byte("without"))
	if err != nil || got != nil {
		t.Fatalf("an ack without a dack came back as dack=%x err=%v", got, err)
	}
	if n := alice.host.bad.Load(); n != 0 {
		t.Fatalf("%d confirmations counted malformed on the way", n)
	}
}

// A confirmation bob's daemon spoils is still carried as it came: alice's
// transport is the one that finds it malformed, counts it (dack-bad-frame)
// and treats it as none (DLV-W-11b). The peer processes in between neither
// noticed nor fixed it.
func TestASpoiltDAckIsCarriedAndFoundAtTheSender(t *testing.T) {
	dir := t.TempDir()
	rv := filepath.Join(dir, "rv")
	alice := newNode(t, dir, rv, "alice", &mailbox{})
	bobBox := &mailbox{dacks: map[string][]byte{
		"short": bytes.Repeat([]byte{1}, module.DAckLen-1),
		"long":  bytes.Repeat([]byte{1}, module.DAckLen+1),
	}}
	bob := newNode(t, dir, rv, "bob", bobBox)
	at := alice.host.transport(t)
	waitAnnounced(t, rv, alice, bob)
	for i, env := range []string{"short", "long"} {
		got, handed, err := at.Send(context.Background(), bob.aid, []byte(env))
		if err != nil || !handed || got != nil {
			t.Fatalf("%s: dack=%x handed=%v err=%v; want the send answered without a dack", env, got, handed, err)
		}
		if n := alice.host.bad.Load(); n != int32(i+1) {
			t.Fatalf("%s: alice counted %d malformed confirmations, want %d", env, n, i+1)
		}
	}
}
