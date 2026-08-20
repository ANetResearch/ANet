//go:build !no_p2p

package p2p

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/module"
)

// fakePeer stands in for the out-of-process peer stack: it speaks the wire
// and nothing else, which is exactly what a real one built on libp2p,
// ironwood or anything else would look like from this side.
type fakePeer struct {
	t  *testing.T
	ln net.Listener

	mu        sync.Mutex
	reachable map[string]bool
	sent      []frame
	self      string
	conns     []net.Conn
}

func newFakePeer(t *testing.T) *fakePeer {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "peer.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	p := &fakePeer{t: t, ln: ln, reachable: map[string]bool{}}
	t.Cleanup(func() { ln.Close() })
	go p.accept()
	return p
}

func (p *fakePeer) socket() string { return p.ln.Addr().String() }

func (p *fakePeer) accept() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		p.conns = append(p.conns, c)
		p.mu.Unlock()
		go p.serve(c)
	}
}

func (p *fakePeer) serve(c net.Conn) {
	dec := json.NewDecoder(bufio.NewReader(c))
	enc := json.NewEncoder(c)
	for {
		var f frame
		if err := dec.Decode(&f); err != nil {
			return
		}
		switch f.Op {
		case opHello:
			p.mu.Lock()
			p.self = f.Self
			p.mu.Unlock()
		case opReach:
			p.mu.Lock()
			ok := p.reachable[f.To]
			p.mu.Unlock()
			_ = enc.Encode(frame{Op: opReach, Reachable: ok})
		case opSend:
			p.mu.Lock()
			p.sent = append(p.sent, f)
			p.mu.Unlock()
			_ = enc.Encode(frame{Op: opSend})
		}
	}
}

// push simulates a message arriving from the network.
func (p *fakePeer) push(f frame) {
	p.mu.Lock()
	conns := append([]net.Conn(nil), p.conns...)
	p.mu.Unlock()
	for _, c := range conns {
		_ = json.NewEncoder(c).Encode(f)
	}
}

func (p *fakePeer) setReachable(aid string, ok bool) {
	p.mu.Lock()
	p.reachable[aid] = ok
	p.mu.Unlock()
}

// kill takes the peer process away: the listener and every live session.
// Closing only the listener leaves the accepted connection answering, which
// is not what a crashed peer stack looks like.
func (p *fakePeer) kill() {
	p.ln.Close()
	p.mu.Lock()
	conns := append([]net.Conn(nil), p.conns...)
	p.conns = nil
	p.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
}

func (p *fakePeer) sentCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.sent)
}

func (p *fakePeer) helloSelf() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.self
}

// recorder captures what the transport hands the daemon.
type recorder struct {
	mu   sync.Mutex
	got  []string
	fail error
}

func (r *recorder) Receive(_ context.Context, from, kind, ix string, payload []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return r.fail
	}
	r.got = append(r.got, from+"/"+kind+"/"+ix+"/"+string(payload))
	return nil
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

func dialTransport(t *testing.T, peer *fakePeer, rec module.Inbound) *Transport {
	t.Helper()
	tr := &Transport{
		socket: peer.socket(), selfAID: "aid-self",
		dialTimeout: 2 * time.Second, reachTimeout: time.Second,
		inbound: rec,
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); tr.close() })
	go tr.run(ctx)
	waitFor(t, func() bool { return peer.helloSelf() == "aid-self" }, "the peer to be told our AID")
	return tr
}

func waitFor(t *testing.T, cond func() bool, why string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", why)
}

// The peer process is told which identity it carries for, and nothing else
// about it: the daemon owns the key, the peer stack owns the network.
func TestPeerIsToldTheAIDItCarriesFor(t *testing.T) {
	peer := newFakePeer(t)
	dialTransport(t, peer, &recorder{})
	if got := peer.helloSelf(); got != "aid-self" {
		t.Fatalf("hello.self = %q, want the daemon's AID", got)
	}
}

// Reachability is answered by the peer stack, because only it knows.
func TestReachabilityComesFromThePeerProcess(t *testing.T) {
	peer := newFakePeer(t)
	tr := dialTransport(t, peer, &recorder{})

	peer.setReachable("aid-near", true)
	if !tr.Reachable(context.Background(), "aid-near") {
		t.Error("a peer the stack can reach must be reported reachable")
	}
	if tr.Reachable(context.Background(), "aid-far") {
		t.Error("a peer the stack cannot reach must not be")
	}
}

// A payload crosses the socket intact — it is signed by the daemon and the
// peer process is not expected to understand it.
func TestSendCarriesThePayloadUnaltered(t *testing.T) {
	peer := newFakePeer(t)
	tr := dialTransport(t, peer, &recorder{})

	body := []byte("signed-delegation-bytes")
	if err := tr.Send(context.Background(), "aid-peer", "delegate", "ix-9", body); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return peer.sentCount() == 1 }, "the send to reach the peer")

	peer.mu.Lock()
	f := peer.sent[0]
	peer.mu.Unlock()
	got, err := base64.StdEncoding.DecodeString(f.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Fatalf("payload = %q, want it unaltered", got)
	}
	if f.To != "aid-peer" || f.Kind != "delegate" || f.IX != "ix-9" {
		t.Fatalf("addressing lost: %+v", f)
	}
}

// Inbound goes to the daemon by the same door as anything from the hub.
func TestInboundReachesTheDaemon(t *testing.T) {
	peer := newFakePeer(t)
	rec := &recorder{}
	dialTransport(t, peer, rec)

	peer.push(frame{Op: opRecv, From: "aid-them", Kind: "result", IX: "ix-3",
		Payload: base64.StdEncoding.EncodeToString([]byte("payload"))})

	waitFor(t, func() bool { return rec.count() == 1 }, "the inbound message to reach the daemon")
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.got[0] != "aid-them/result/ix-3/payload" {
		t.Fatalf("inbound arrived altered: %q", rec.got[0])
	}
}

// With the peer process down, the transport reports unreachable rather than
// hanging or erroring — the hub is behind it, and this is an optimisation.
func TestPeerProcessDownIsSimplyUnreachable(t *testing.T) {
	peer := newFakePeer(t)
	tr := dialTransport(t, peer, &recorder{})

	// Reachable while the stack is up, so the assertion after it dies is
	// about the death and not about the AID.
	peer.setReachable("aid-any", true)
	if !tr.Reachable(context.Background(), "aid-any") {
		t.Fatal("precondition: the peer should be reachable while the stack is up")
	}

	peer.kill()
	waitFor(t, func() bool {
		return !tr.Reachable(context.Background(), "aid-any")
	}, "the transport to report unreachable once the peer is gone")

	if err := tr.Send(context.Background(), "aid-any", "delegate", "ix-1", []byte("x")); err == nil {
		t.Fatal("sending with no peer process must fail so the caller falls through to the hub")
	}
}
