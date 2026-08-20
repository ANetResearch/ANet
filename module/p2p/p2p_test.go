//go:build !no_p2p

package p2p

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
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
	// hold, when set, defers every send reply until it is closed, so
	// replies come back in whatever order the goroutines win.
	hold chan struct{}

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
			_ = enc.Encode(frame{Op: opReach, ID: f.ID, Reachable: ok})
		case opSend:
			p.mu.Lock()
			p.sent = append(p.sent, f)
			p.mu.Unlock()
			p.mu.Lock()
			hold := p.hold
			p.mu.Unlock()
			reply := frame{Op: opSend, ID: f.ID}
			if hold != nil {
				// Held mode: answer out of order, the way any peer that
				// handles deliveries concurrently does — and fail the odd
				// interactions. Which reply carries the error is the whole
				// question: matched by id it lands on the send that asked,
				// matched by arrival order it lands on whichever send
				// happened to be next in the queue.
				if n := len(f.IX); n > 0 && (f.IX[n-1]-'0')%2 == 1 {
					reply.Error = "no route to peer"
				}
				go func(r frame) { <-hold; p.mu.Lock(); _ = enc.Encode(r); p.mu.Unlock() }(reply)
				continue
			}
			_ = enc.Encode(reply)
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

// replier is an Inbound that answers on the same transport, which is what
// a daemon does with every delegation it accepts.
type replier struct {
	tr   *Transport
	mu   sync.Mutex
	err  error
	done chan struct{}
}

func (r *replier) Receive(ctx context.Context, from, kind, ix string, _ []byte) error {
	err := r.tr.Send(ctx, from, "result", ix, []byte("result bytes"))
	r.mu.Lock()
	r.err = err
	r.mu.Unlock()
	close(r.done)
	return err
}

// Answering an inbound delegation over the same transport must work.
//
// It deadlocked. Inbound delivery ran inline in the session's read loop,
// and answering means Send, and Send waits for a reply that only that read
// loop can deliver. So the loop waited on the daemon, the daemon waited on
// the loop, and it unwound only when Send hit its timeout — at which point
// the transport reported failure and the delegation fell through to the
// hub. Every capability call is exactly this shape: receive a delegation,
// return a result. The one module whose whole purpose is to carry
// delegations directly could not carry a single one to completion.
//
// Nothing in the suite caught it because every test drove one direction at
// a time. It took two daemons and a real peer process, where the symptom
// was a peer log full of "receiving daemon did not accept".
func TestAnsweringAnInboundMessageDoesNotDeadlock(t *testing.T) {
	peer := newFakePeer(t)
	r := &replier{done: make(chan struct{})}
	tr := dialTransport(t, peer, r)
	r.tr = tr

	peer.push(frame{Op: opRecv, From: "aid-peer", Kind: "delegate", IX: "ix-1",
		Payload: base64.StdEncoding.EncodeToString([]byte("delegation"))})

	select {
	case <-r.done:
	case <-time.After(3 * time.Second):
		t.Fatal("the daemon never finished handling the inbound message (deadlocked)")
	}
	r.mu.Lock()
	err := r.err
	r.mu.Unlock()
	if err != nil {
		t.Fatalf("answering over the same transport failed: %v", err)
	}
	// And the answer has to have actually gone out, promptly.
	waitFor(t, func() bool { return peer.sentCount() == 1 }, "the reply to reach the peer")
}

// A reply and an ack are both writes to one connection. Two encoders
// writing concurrently interleave, and an interleaved JSON frame is a
// connection the peer process can no longer parse.
func TestConcurrentInboundAndOutboundDoNotCorruptTheWire(t *testing.T) {
	peer := newFakePeer(t)
	rec := &recorder{}
	tr := dialTransport(t, peer, rec)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = tr.Send(context.Background(), "aid-peer", "message",
				fmt.Sprintf("ix-%d", i), []byte("payload"))
		}(i)
		peer.push(frame{Op: opRecv, From: "aid-peer", Kind: "message",
			IX:      fmt.Sprintf("in-%d", i),
			Payload: base64.StdEncoding.EncodeToString([]byte("inbound"))})
	}
	wg.Wait()
	// The peer must still be able to parse what we sent: a corrupted frame
	// kills its decoder and everything after it is lost.
	waitFor(t, func() bool { return peer.sentCount() == 20 }, "all 20 sends to arrive intact")
	waitFor(t, func() bool { return rec.count() == 20 }, "all 20 inbound messages to be delivered")
}

// A reply belongs to the request that asked for it.
//
// The wire had no request ids: replies were matched first-in-first-out,
// resting on "a peer answers in the order it was asked". No peer author is
// told that, the daemon cannot check it, and the first peer process
// written against this wire broke it within the hour — it had to handle
// deliveries off its read loop to avoid a deadlock of its own, and that
// made its replies concurrent.
//
// The failure it produces is the worst kind. Two sends in flight, replies
// swapped: one delegation is reported delivered because the other one was.
// Nothing errors, nothing retries, and the message is simply gone.
func TestRepliesAreMatchedToTheirRequests(t *testing.T) {
	peer := newFakePeer(t)
	peer.mu.Lock()
	hold := make(chan struct{})
	peer.hold = hold
	peer.mu.Unlock()
	tr := dialTransport(t, peer, &recorder{})

	// One send that will fail and one that will succeed, answered in
	// whichever order the peer's goroutines happen to win.
	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = tr.Send(context.Background(), "aid-peer", "message",
				fmt.Sprintf("ix-%d", i), []byte("payload"))
		}(i)
	}
	waitFor(t, func() bool { return peer.sentCount() == n }, "all sends to reach the peer")
	close(hold)
	wg.Wait()
	// The peer failed exactly the odd ones. Any other pattern means a
	// reply landed on the wrong request.
	for i, err := range errs {
		wantErr := i%2 == 1
		if (err != nil) != wantErr {
			t.Errorf("send ix-%d: err=%v, want error=%v — a reply landed on the wrong request",
				i, err, wantErr)
		}
	}
}
