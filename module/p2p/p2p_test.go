//go:build !no_p2p

package p2p

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/module"
)

// fakePeer stands in for the out-of-process peer stack: it speaks the wire
// and nothing else, which is exactly what a real one built on libp2p,
// ironwood or anything else would look like from this side.
//
// Like a real peer process it mints an ID for every delivery it hands the
// daemon (op recv) and records the daemon's acks by that ID, so a test can
// tell which delivery an ack answered.
type fakePeer struct {
	// hold, when set, defers every send reply until it is closed, so
	// replies come back in whatever order the goroutines win.
	hold chan struct{}
	// answer, when set, writes the reply to a send instead of the default
	// {op: send, v: 3, id}; nil skips the reply.
	answer func(f frame) *frame

	t  *testing.T
	ln net.Listener

	mu      sync.Mutex
	sent    []frame
	sentRaw []map[string]any
	self    string
	helloV  int
	conns   []net.Conn
	nextID  int
	acks    map[string][]frame // delivery ID → the acks received for it
	nacks   map[string]string  // delivery ID → the reason of its nack
	errs    []frame            // error frames the daemon wrote
	ended   int                // sessions the daemon ended
}

func newFakePeer(t *testing.T) *fakePeer {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "peer.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	p := &fakePeer{t: t, ln: ln, acks: map[string][]frame{}, nacks: map[string]string{}}
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
	r := bufio.NewReader(c)
	enc := json.NewEncoder(c)
	defer func() {
		p.mu.Lock()
		p.ended++
		p.mu.Unlock()
	}()
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		var f frame
		if err := json.Unmarshal(line, &f); err != nil {
			p.t.Errorf("the daemon wrote a frame that is not JSON: %q", line)
			return
		}
		var raw map[string]any
		_ = json.Unmarshal(line, &raw)
		switch f.Op {
		case opHello:
			p.mu.Lock()
			p.self, p.helloV = f.Self, f.V
			p.mu.Unlock()
		case opAck:
			p.mu.Lock()
			p.acks[f.ID] = append(p.acks[f.ID], f)
			p.mu.Unlock()
		case opNack:
			p.mu.Lock()
			p.nacks[f.ID] = f.Error
			p.mu.Unlock()
		case opError:
			p.mu.Lock()
			p.errs = append(p.errs, f)
			p.mu.Unlock()
		case opSend:
			p.mu.Lock()
			p.sent = append(p.sent, f)
			p.sentRaw = append(p.sentRaw, raw)
			hold, answer := p.hold, p.answer
			p.mu.Unlock()
			reply := &frame{Op: opSend, V: WireVersion, ID: f.ID}
			if answer != nil {
				if reply = answer(f); reply == nil {
					continue
				}
			}
			if hold != nil {
				// Held mode: answer out of order, the way any peer that
				// handles deliveries concurrently does — and fail the odd
				// interactions. Which reply carries the error is the whole
				// question: matched by id it lands on the send that asked,
				// matched by arrival order it lands on whichever send
				// happened to be next in the queue.
				if n := len(f.To); n > 0 && (f.To[n-1]-'0')%2 == 1 {
					reply.Error = "no route to peer"
				}
				go func(r frame) { <-hold; p.mu.Lock(); _ = enc.Encode(r); p.mu.Unlock() }(*reply)
				continue
			}
			p.mu.Lock()
			_ = enc.Encode(reply)
			p.mu.Unlock()
		}
	}
}

// push simulates an envelope arriving from the network: it mints a
// delivery ID, hands the daemon a recv frame and returns the ID.
func (p *fakePeer) push(env []byte) string {
	p.mu.Lock()
	p.nextID++
	id := fmt.Sprintf("d%d", p.nextID)
	p.mu.Unlock()
	p.pushFrame(frame{Op: opRecv, V: WireVersion, ID: id, Envelope: encodeB64(env)})
	return id
}

// pushFrame hands the daemon an arbitrary frame.
func (p *fakePeer) pushFrame(f frame) {
	b, err := json.Marshal(f)
	if err != nil {
		p.t.Fatal(err)
	}
	p.pushLine(string(b))
}

// pushLine hands the daemon one line of the wire as it is.
func (p *fakePeer) pushLine(line string) {
	p.mu.Lock()
	conns := append([]net.Conn(nil), p.conns...)
	p.mu.Unlock()
	for _, c := range conns {
		p.mu.Lock()
		_, _ = c.Write([]byte(line + "\n"))
		p.mu.Unlock()
	}
}

// acksFor reports how many acks the daemon sent for a delivery ID.
func (p *fakePeer) acksFor(id string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.acks[id])
}

// ackOf is the daemon's (first) ack for a delivery ID.
func (p *fakePeer) ackOf(id string) frame {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.acks[id][0]
}

// errorFor is the daemon's error frame for a delivery ID, if any.
func (p *fakePeer) errorFor(id string) (frame, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, f := range p.errs {
		if f.ID == id {
			return f, true
		}
	}
	return frame{}, false
}

// totalAcks is every ack received, for any ID.
func (p *fakePeer) totalAcks() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, c := range p.acks {
		n += len(c)
	}
	return n
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

// recorder captures what the transport hands the daemon. refuse lists
// envelopes the daemon refuses temporarily (Receive returns an error); dack
// is the confirmation it answers every accepted one with.
type recorder struct {
	mu     sync.Mutex
	got    []string
	fail   error
	refuse map[string]bool
	dack   []byte
}

func (r *recorder) Receive(_ context.Context, env []byte) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return nil, r.fail
	}
	if r.refuse[string(env)] {
		return nil, fmt.Errorf("temporarily refused")
	}
	r.got = append(r.got, string(env))
	return r.dack, nil
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

func dialTransport(t *testing.T, peer *fakePeer, rec module.Inbound) *Transport {
	t.Helper()
	tr, _ := dialTransportCounting(t, peer, rec)
	return tr
}

// dialTransportCounting is dialTransport with the transport's malformed
// confirmations counted (the daemon's dack-bad-frame).
func dialTransportCounting(t *testing.T, peer *fakePeer, rec module.Inbound) (*Transport, *atomic.Int32) {
	t.Helper()
	bad := new(atomic.Int32)
	tr := &Transport{
		socket: peer.socket(), selfAID: "aid-self",
		sendTimeout: 2 * time.Second,
		inbound:     rec,
		badDAck:     func() { bad.Add(1) },
		addrOf:      anyAddr,
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); tr.close() })
	go tr.run(ctx)
	waitFor(t, func() bool { return peer.helloSelf() == "aid-self" }, "the peer to be told our AID")
	waitFor(t, func() bool { return tr.Reachable(context.Background(), "aid-any") }, "the session to be up")
	return tr, bad
}

// testAddr is the address every AID has in these tests (RFC 5737), unless
// a test says otherwise.
const testAddr = "203.0.113.7:39100"

// anyAddr gives every AID testAddr: the daemon's P2PAddr for a node that
// holds a contact capability with an address from everyone.
func anyAddr(aid string) (string, bool) { return testAddr, aid != "" }

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
	peer.mu.Lock()
	defer peer.mu.Unlock()
	if peer.self != "aid-self" || peer.helloV != WireVersion {
		t.Fatalf("hello self=%q v=%d, want the daemon's AID and v %d", peer.self, peer.helloV, WireVersion)
	}
}

// Reachable asks the peer process nothing: frame v3 has no reach frame, and
// whether a recipient can be reached is not the peer process's to answer.
// It says whether there is a peer process to hand an envelope to.
func TestReachableAsksThePeerProcessNothing(t *testing.T) {
	peer := newFakePeer(t)
	tr := dialTransport(t, peer, &recorder{})
	if !tr.Reachable(context.Background(), "aid-near") {
		t.Fatal("with the peer process up, the transport should take a send")
	}
	time.Sleep(50 * time.Millisecond)
	peer.mu.Lock()
	n := len(peer.sent) + len(peer.errs) + peer.totalAcksLocked()
	peer.mu.Unlock()
	if n != 0 {
		t.Fatalf("Reachable wrote %d frames to the peer process", n)
	}
}

func (p *fakePeer) totalAcksLocked() int {
	n := 0
	for _, c := range p.acks {
		n += len(c)
	}
	return n
}

// An envelope crosses the socket intact — it is sealed by the daemon and
// the peer process is not expected to understand it. The frame names the
// recipient and nothing else about the message, in base64url.
func TestSendCarriesTheEnvelopeUnaltered(t *testing.T) {
	peer := newFakePeer(t)
	tr := dialTransport(t, peer, &recorder{})

	// Bytes whose standard and URL base64 differ.
	body := []byte("sealed\xfb\xff\xfe-envelope-bytes")
	if _, handed, err := tr.Send(context.Background(), "aid-peer", body); err != nil || !handed {
		t.Fatalf("send: handed=%v err=%v", handed, err)
	}
	waitFor(t, func() bool { return peer.sentCount() == 1 }, "the send to reach the peer")

	peer.mu.Lock()
	f, raw := peer.sent[0], peer.sentRaw[0]
	peer.mu.Unlock()
	got, err := decodeB64(f.Envelope)
	if err != nil {
		t.Fatalf("envelope %q is not base64url without padding: %v", f.Envelope, err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("envelope = %q, want it unaltered", got)
	}
	if f.To != "aid-peer" || f.V != WireVersion || f.Addr != testAddr {
		t.Fatalf("addressing lost: %+v", f)
	}
	for k := range raw {
		switch k {
		case "op", "v", "id", "to", "envelope", "addr":
		default:
			t.Errorf("a send frame carries %q", k)
		}
	}
}

// Inbound goes to the daemon by the same door as anything from the hub,
// and is acked with the delivery's own ID and the daemon's confirmation,
// which crosses as the daemon made it.
func TestInboundReachesTheDaemon(t *testing.T) {
	peer := newFakePeer(t)
	dack := bytes.Repeat([]byte{0xfb, 0xff, 0x01, 0x02}, module.DAckLen/4)
	rec := &recorder{dack: dack}
	dialTransport(t, peer, rec)

	id := peer.push([]byte("envelope"))

	waitFor(t, func() bool { return rec.count() == 1 }, "the inbound envelope to reach the daemon")
	rec.mu.Lock()
	got := rec.got[0]
	rec.mu.Unlock()
	if got != "envelope" {
		t.Fatalf("inbound arrived altered: %q", got)
	}
	waitFor(t, func() bool { return peer.acksFor(id) == 1 }, "the ack for "+id)
	ack := peer.ackOf(id)
	if ack.V != WireVersion || ack.DAck != encodeB64(dack) || len(ack.DAck) != 107 {
		t.Fatalf("ack = %+v, want v %d and the daemon's dack in base64url (107 characters)", ack, WireVersion)
	}
}

// A daemon that makes no confirmation for an envelope acks it without one:
// the ack has no dack member at all.
func TestAnAckWithoutAConfirmationHasNoDAck(t *testing.T) {
	peer := newFakePeer(t)
	rec := &recorder{}
	dialTransport(t, peer, rec)
	id := peer.push([]byte("envelope"))
	waitFor(t, func() bool { return peer.acksFor(id) == 1 }, "the ack for "+id)
	if d := peer.ackOf(id).DAck; d != "" {
		t.Fatalf("an ack carries dack %q the daemon never made", d)
	}
}

// A delivery the daemon refuses temporarily is not acked, so the peer
// process reports it undelivered and the sender falls back to the hub
// (A2A-DESIGN §3.6).
func TestATemporaryRefusalIsNotAcked(t *testing.T) {
	peer := newFakePeer(t)
	rec := &recorder{fail: fmt.Errorf("store busy")}
	dialTransport(t, peer, rec)

	id := peer.push([]byte("envelope"))
	// Said at once, with the reason, rather than left for the peer process
	// to time out on (an internal note item): the sender falls back to the hub now.
	waitFor(t, func() bool {
		peer.mu.Lock()
		defer peer.mu.Unlock()
		_, ok := peer.nacks[id]
		return ok
	}, "the nack for "+id)
	peer.mu.Lock()
	reason := peer.nacks[id]
	peer.mu.Unlock()
	if !strings.Contains(reason, "store busy") {
		t.Fatalf("nack reason %q does not carry the daemon's", reason)
	}
	if n := peer.acksFor(id); n != 0 {
		t.Fatalf("a refused delivery was acked %d times", n)
	}
}

// Two deliveries in flight to the same recipient, one accepted and one
// refused: each ack must answer its own delivery. Keyed on anything the
// two share — the recipient, the envelope, a constant — the accepted one's
// ack would release the refused one, and the sender of a message the
// daemon never took would be told it was delivered (C5).
func TestConcurrentDeliveriesAreAckedByTheirOwnID(t *testing.T) {
	peer := newFakePeer(t)
	rec := &recorder{refuse: map[string]bool{"refused": true}}
	dialTransport(t, peer, rec)

	refusedID := peer.push([]byte("refused"))
	acceptedID := peer.push([]byte("accepted"))

	waitFor(t, func() bool { return peer.acksFor(acceptedID) == 1 }, "the accepted delivery's ack")
	time.Sleep(100 * time.Millisecond)
	if n := peer.acksFor(refusedID); n != 0 {
		t.Fatalf("the refused delivery %s was acked %d times", refusedID, n)
	}
	if n := peer.totalAcks(); n != 1 {
		t.Fatalf("%d acks for one accepted delivery", n)
	}
}

// A delivery frame of another version is not handed to the daemon and not
// acked; the peer process gets an error frame for it at once (core CORE-V3).
// Without a version is another version.
func TestADeliveryOfAnotherVersionIsNotHandedToTheDaemon(t *testing.T) {
	peer := newFakePeer(t)
	rec := &recorder{}
	dialTransport(t, peer, rec)

	env := encodeB64([]byte("envelope"))
	peer.pushLine(`{"op":"recv","v":2,"id":"old-2","envelope":"` + env + `"}`)
	peer.pushLine(`{"op":"recv","id":"old-0","envelope":"` + env + `"}`)
	for _, id := range []string{"old-2", "old-0"} {
		waitFor(t, func() bool { _, ok := peer.errorFor(id); return ok }, "the error frame for "+id)
		e, _ := peer.errorFor(id)
		if e.V != WireVersion || !strings.Contains(e.Error, "frame version") {
			t.Errorf("error frame %+v, want v %d naming the frame version", e, WireVersion)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if rec.count() != 0 || peer.totalAcks() != 0 {
		t.Fatalf("a delivery of another version reached the daemon (%d) or was acked (%d)", rec.count(), peer.totalAcks())
	}
}

// An envelope in any spelling but base64url without padding is not handed to
// the daemon (core CORE-E5): one value, one spelling.
func TestADeliveryInAnotherSpellingIsRefused(t *testing.T) {
	peer := newFakePeer(t)
	rec := &recorder{}
	dialTransport(t, peer, rec)
	for i, env := range []string{
		"ZW52ZWxvcGU=",      // padded
		"+/+/ZW52",          // standard alphabet
		"ZW52\nZW52",        // a line break inside
		"ZW52ZWxvcGV",       // trailing bits not zero
		"",                  // empty
		"ZW52 ZWxvcGU",      // white space
		"ZW52ZWxvcGU\u00e9", // not ASCII
	} {
		id := fmt.Sprintf("bad-%d", i)
		b, _ := json.Marshal(frame{Op: opRecv, V: WireVersion, ID: id, Envelope: env})
		peer.pushLine(string(b))
		waitFor(t, func() bool { _, ok := peer.errorFor(id); return ok }, "the error frame for envelope "+env)
	}
	if rec.count() != 0 || peer.totalAcks() != 0 {
		t.Fatalf("a badly spelt envelope reached the daemon (%d) or was acked (%d)", rec.count(), peer.totalAcks())
	}
}

// A peer process that greets with another frame version cannot carry this
// node's traffic: it is told so in an error frame and the session ends
// (core CORE-V3, M38).
func TestAHelloOfAnotherVersionEndsTheSession(t *testing.T) {
	for _, line := range []string{`{"op":"hello","v":2,"self":"aid-peer"}`, `{"op":"hello","self":"aid-peer"}`} {
		peer := newFakePeer(t)
		rec := &recorder{}
		dialTransport(t, peer, rec)
		peer.pushLine(line)
		waitFor(t, func() bool { _, ok := peer.errorFor(""); return ok }, "an error frame for "+line)
		e, _ := peer.errorFor("")
		if e.V != WireVersion || !strings.Contains(e.Error, "frame version") {
			t.Errorf("%s: error frame %+v", line, e)
		}
		waitFor(t, func() bool { peer.mu.Lock(); defer peer.mu.Unlock(); return peer.ended > 0 }, "the session to end after "+line)
		if rec.count() != 0 {
			t.Errorf("%s: the daemon was handed something", line)
		}
	}
	// A greeting in this version changes nothing.
	peer := newFakePeer(t)
	rec := &recorder{}
	dialTransport(t, peer, rec)
	peer.pushLine(`{"op":"hello","v":3,"self":"aid-peer"}`)
	peer.push([]byte("after"))
	waitFor(t, func() bool { return rec.count() == 1 }, "a delivery after a v3 hello")
}

// With the peer process down, the transport reports unreachable rather than
// hanging or erroring — the hub is behind it, and this is an optimisation.
// A send then hands nothing over.
func TestPeerProcessDownIsSimplyUnreachable(t *testing.T) {
	peer := newFakePeer(t)
	tr := dialTransport(t, peer, &recorder{})

	peer.kill()
	waitFor(t, func() bool {
		return !tr.Reachable(context.Background(), "aid-any")
	}, "the transport to report unreachable once the peer is gone")

	dack, handed, err := tr.Send(context.Background(), "aid-any", []byte("x"))
	if err == nil {
		t.Fatal("sending with no peer process must fail so the caller falls through to the hub")
	}
	if handed || dack != nil {
		t.Fatalf("a send with no peer process: handed=%v dack=%v; nothing left this process", handed, dack)
	}
}

// The answer to a send carries the recipient's confirmation; Send returns its
// bytes as they came back.
func TestSendReturnsTheConfirmationOfTheAnswer(t *testing.T) {
	peer := newFakePeer(t)
	dack := bytes.Repeat([]byte{0xfb, 0xef, 0xbe}, 27)[:module.DAckLen]
	peer.answer = func(f frame) *frame { return &frame{Op: opSend, V: WireVersion, ID: f.ID, DAck: encodeB64(dack)} }
	tr, bad := dialTransportCounting(t, peer, &recorder{})
	got, handed, err := tr.Send(context.Background(), "aid-peer", []byte("x"))
	if err != nil || !handed {
		t.Fatalf("handed=%v err=%v", handed, err)
	}
	if !bytes.Equal(got, dack) {
		t.Fatalf("dack = %x, want %x", got, dack)
	}
	if bad.Load() != 0 {
		t.Fatalf("a well-formed confirmation counted as malformed")
	}
	// No confirmation in the answer: none returned, nothing counted.
	peer.mu.Lock()
	peer.answer = nil
	peer.mu.Unlock()
	if got, _, err := tr.Send(context.Background(), "aid-peer", []byte("x")); err != nil || got != nil {
		t.Fatalf("an answer without a dack: dack=%x err=%v", got, err)
	}
	if bad.Load() != 0 {
		t.Fatalf("an answer without a confirmation counted as malformed")
	}
	// A send answer may carry both a confirmation and an error (core §2.10):
	// the confirmation is returned with the error, for the caller to check.
	// The error does not unsay what the recipient said.
	peer.mu.Lock()
	peer.answer = func(f frame) *frame {
		return &frame{Op: opSend, V: WireVersion, ID: f.ID, DAck: encodeB64(dack), Error: "the answer came late"}
	}
	peer.mu.Unlock()
	got, handed, err = tr.Send(context.Background(), "aid-peer", []byte("x"))
	if err == nil || !handed || !bytes.Equal(got, dack) {
		t.Fatalf("an answer with a dack and an error: dack=%x handed=%v err=%v", got, handed, err)
	}
}

// DLV-W-11b: a confirmation that is not base64url, or not DAckLen bytes, is
// no confirmation. The send is answered all the same; the confirmation is
// counted (dack-bad-frame) and dropped.
func TestAMalformedConfirmationIsNone(t *testing.T) {
	ok := encodeB64(bytes.Repeat([]byte{7}, module.DAckLen))
	for _, d := range []string{
		encodeB64(bytes.Repeat([]byte{7}, module.DAckLen-1)), // 79 bytes
		encodeB64(bytes.Repeat([]byte{7}, module.DAckLen+1)), // 81 bytes
		ok + "=",                         // padded
		strings.Replace(ok, "B", "+", 1), // standard alphabet
		ok[:50] + "\n" + ok[50:],         // a line break
		ok[:106] + "x",                   // trailing bits not zero
		"not a dack at all!",
	} {
		peer := newFakePeer(t)
		d := d
		peer.answer = func(f frame) *frame { return &frame{Op: opSend, V: WireVersion, ID: f.ID, DAck: d} }
		tr, bad := dialTransportCounting(t, peer, &recorder{})
		got, handed, err := tr.Send(context.Background(), "aid-peer", []byte("x"))
		if err != nil || !handed {
			t.Fatalf("%q: handed=%v err=%v; the send itself was answered", d, handed, err)
		}
		if got != nil {
			t.Errorf("%q: returned %x as a confirmation", d, got)
		}
		if bad.Load() != 1 {
			t.Errorf("%q: counted %d times, want 1", d, bad.Load())
		}
	}
}

// An answer in another frame version is no answer the transport can read:
// whatever it carries, the send failed after the envelope was handed over.
func TestAnAnswerOfAnotherVersionCarriesNothing(t *testing.T) {
	peer := newFakePeer(t)
	dack := encodeB64(bytes.Repeat([]byte{7}, module.DAckLen))
	peer.answer = func(f frame) *frame { return &frame{Op: opSend, V: 2, ID: f.ID, DAck: dack} }
	tr := dialTransport(t, peer, &recorder{})
	got, handed, err := tr.Send(context.Background(), "aid-peer", []byte("x"))
	if err == nil || !handed || got != nil {
		t.Fatalf("an answer of v 2: dack=%x handed=%v err=%v; want an error, handed, no dack", got, handed, err)
	}
}

// T49, the p2p half: a send has handed the envelope over once a byte of its
// frame was written to the peer process's socket, and not before.
func TestHandedMeansAByteReachedThePeerProcess(t *testing.T) {
	ctx := context.Background()

	t.Run("not connected", func(t *testing.T) {
		tr := &Transport{sendTimeout: time.Second, addrOf: anyAddr}
		if _, handed, err := tr.Send(ctx, "aid", []byte("x")); err == nil || handed {
			t.Fatalf("handed=%v err=%v", handed, err)
		}
	})

	t.Run("cancelled before the write", func(t *testing.T) {
		c := &stubConn{}
		tr := &Transport{sendTimeout: time.Second, conn: c, pending: map[string]chan answer{}, addrOf: anyAddr}
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if _, handed, err := tr.Send(cctx, "aid", []byte("x")); err == nil || handed {
			t.Fatalf("handed=%v err=%v", handed, err)
		}
		if c.written() != 0 {
			t.Fatalf("a cancelled send wrote %d bytes", c.written())
		}
	})

	t.Run("the write fails before its first byte", func(t *testing.T) {
		c := &stubConn{n: 0, err: errors.New("broken pipe")}
		tr := &Transport{sendTimeout: time.Second, conn: c, pending: map[string]chan answer{}, addrOf: anyAddr}
		if _, handed, err := tr.Send(ctx, "aid", []byte("x")); err == nil || handed {
			t.Fatalf("handed=%v err=%v", handed, err)
		}
	})

	t.Run("the write fails part of the way", func(t *testing.T) {
		c := &stubConn{n: 5, err: errors.New("broken pipe")}
		tr := &Transport{sendTimeout: time.Second, conn: c, pending: map[string]chan answer{}, addrOf: anyAddr}
		if _, handed, err := tr.Send(ctx, "aid", []byte("x")); err == nil || !handed {
			t.Fatalf("handed=%v err=%v", handed, err)
		}
	})

	t.Run("the peer process answers an error", func(t *testing.T) {
		peer := newFakePeer(t)
		peer.answer = func(f frame) *frame { return &frame{Op: opSend, V: WireVersion, ID: f.ID, Error: "no peer for aid"} }
		tr := dialTransport(t, peer, &recorder{})
		if _, handed, err := tr.Send(ctx, "aid", []byte("x")); err == nil || !handed {
			t.Fatalf("handed=%v err=%v", handed, err)
		}
	})

	t.Run("the peer process refuses the frame", func(t *testing.T) {
		peer := newFakePeer(t)
		peer.answer = func(f frame) *frame {
			return &frame{Op: opError, V: WireVersion, ID: f.ID, Error: "frame version 3 refused"}
		}
		tr := dialTransport(t, peer, &recorder{})
		if _, handed, err := tr.Send(ctx, "aid", []byte("x")); err == nil || !handed {
			t.Fatalf("handed=%v err=%v", handed, err)
		}
	})

	t.Run("the peer process never answers", func(t *testing.T) {
		peer := newFakePeer(t)
		peer.answer = func(frame) *frame { return nil }
		tr := dialTransport(t, peer, &recorder{})
		tr.sendTimeout = 100 * time.Millisecond
		if _, handed, err := tr.Send(ctx, "aid", []byte("x")); err == nil || !handed {
			t.Fatalf("handed=%v err=%v", handed, err)
		}
	})

	t.Run("the peer process dies after the write", func(t *testing.T) {
		peer := newFakePeer(t)
		peer.answer = func(frame) *frame { go peer.kill(); return nil }
		tr := dialTransport(t, peer, &recorder{})
		if _, handed, err := tr.Send(ctx, "aid", []byte("x")); err == nil || !handed {
			t.Fatalf("handed=%v err=%v", handed, err)
		}
	})
}

// stubConn is a connection whose writes write n bytes and fail with err.
type stubConn struct {
	net.Conn
	mu  sync.Mutex
	n   int
	err error
	got int
}

func (c *stubConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		c.got += len(b)
		return len(b), nil
	}
	c.got += c.n
	return c.n, c.err
}

func (c *stubConn) written() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.got
}

func (c *stubConn) Close() error { return nil }

// SetWriteDeadline: every write to the session has one (writeN); a stub's
// writes return at once.
func (c *stubConn) SetWriteDeadline(time.Time) error { return nil }

// replier is an Inbound that answers on the same transport, which is what
// a daemon does with every delegation it accepts.
type replier struct {
	tr   *Transport
	mu   sync.Mutex
	err  error
	done chan struct{}
}

func (r *replier) Receive(ctx context.Context, _ []byte) ([]byte, error) {
	_, _, err := r.tr.Send(ctx, "aid-peer", []byte("result bytes"))
	r.mu.Lock()
	r.err = err
	r.mu.Unlock()
	close(r.done)
	return nil, err
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

	peer.push([]byte("delegation"))

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
			_, _, _ = tr.Send(context.Background(), "aid-peer", []byte(fmt.Sprintf("payload-%d", i)))
		}(i)
		peer.push([]byte(fmt.Sprintf("inbound-%d", i)))
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
			_, _, errs[i] = tr.Send(context.Background(), fmt.Sprintf("aid-peer-%d", i), []byte("payload"))
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
			t.Errorf("send %d: err=%v, want error=%v — a reply landed on the wrong request",
				i, err, wantErr)
		}
	}
}
