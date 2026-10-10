//go:build !no_p2p

package p2p

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/module"
)

// SSR-P2, engine ENG-I7: the transport offers a send to an AID only when the
// daemon has a direct address for it — the p2p address of a contact
// capability that AID gave this node — and the frame carries exactly that
// address. Without one, Reachable is false and a send hands nothing over.
func TestTheTransportReachesOnlyAnAIDWithAnAddress(t *testing.T) {
	peer := newFakePeer(t)
	tr := dialTransport(t, peer, &recorder{})
	tr.addrOf = func(aid string) (string, bool) {
		if aid == "aid-contact" {
			return "peer.example.org:39100", true
		}
		return "", false
	}
	if tr.Reachable(context.Background(), "aid-stranger") {
		t.Fatal("reachable without an address")
	}
	if !tr.Reachable(context.Background(), "aid-contact") {
		t.Fatal("not reachable with an address and a peer process")
	}
	if _, handed, err := tr.Send(context.Background(), "aid-stranger", []byte("x")); err == nil || handed {
		t.Fatalf("a send without an address: handed=%v err=%v", handed, err)
	}
	if n := peer.sentCount(); n != 0 {
		t.Fatalf("a send without an address reached the peer process (%d frames)", n)
	}
	if _, _, err := tr.Send(context.Background(), "aid-contact", []byte("x")); err != nil {
		t.Fatal(err)
	}
	peer.mu.Lock()
	sent := peer.sent[0]
	peer.mu.Unlock()
	if sent.Addr != "peer.example.org:39100" || sent.To != "aid-contact" {
		t.Fatalf("the frame carries %+v", sent)
	}
	// Without the daemon's answer at all (a host that gives none), nothing.
	tr.addrOf = nil
	if tr.Reachable(context.Background(), "aid-contact") {
		t.Fatal("reachable with no address source")
	}
}

// Core CORE-K13a at the daemon's end: an answer to a send with a member
// frame v3 does not have is a bad frame — the send fails, its dack is not
// taken, and it is counted (dack-bad-frame); a dack member that is empty is
// a malformed dack, not none: counted, and the answer stands without it.
func TestAnAnswerIsReadStrictly(t *testing.T) {
	dack := encodeB64(bytes.Repeat([]byte{7}, module.DAckLen))
	cases := []struct {
		name    string
		line    func(id string) string
		wantErr bool
		bad     int32
	}{
		{"an unregistered member", func(id string) string {
			return `{"op":"send","v":3,"id":"` + id + `","dack":"` + dack + `","outcome":"processed"}`
		}, true, 1},
		{"an empty dack", func(id string) string { return `{"op":"send","v":3,"id":"` + id + `","dack":""}` }, false, 1},
		{"a dack that is not a string", func(id string) string { return `{"op":"send","v":3,"id":"` + id + `","dack":7}` }, true, 1},
		{"an error frame with a dack", func(id string) string {
			return `{"op":"error","v":3,"id":"` + id + `","dack":"` + dack + `"}`
		}, true, 1},
		{"a good answer", func(id string) string { return `{"op":"send","v":3,"id":"` + id + `","dack":"` + dack + `"}` }, false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			peer := newFakePeer(t)
			peer.answer = func(f frame) *frame {
				go peer.pushLine(c.line(f.ID))
				return nil
			}
			tr, bad := dialTransportCounting(t, peer, &recorder{})
			got, handed, err := tr.Send(context.Background(), "aid-peer", []byte("x"))
			if !handed || (err != nil) != c.wantErr {
				t.Fatalf("handed=%v err=%v", handed, err)
			}
			if c.bad == 0 && !bytes.Equal(got, bytes.Repeat([]byte{7}, module.DAckLen)) {
				t.Fatalf("a good answer's dack: %x", got)
			}
			if c.bad != 0 && got != nil {
				t.Fatalf("dack %x taken from it", got)
			}
			if bad.Load() != c.bad {
				t.Fatalf("counted %d, want %d", bad.Load(), c.bad)
			}
		})
	}
}

// A recv frame with a member frame v3 does not have, or without one it must
// have, is refused with an error frame for its id and never handed to the
// daemon; a hello with one ends the session.
func TestAFrameFromThePeerProcessIsReadStrictly(t *testing.T) {
	env := encodeB64([]byte("envelope"))
	peer := newFakePeer(t)
	rec := &recorder{}
	dialTransport(t, peer, rec)
	for id, line := range map[string]string{
		"x1": `{"op":"recv","v":3,"id":"x1","envelope":"` + env + `","to":"aid-self"}`,
		"x2": `{"op":"recv","v":3,"id":"x2"}`,
		"x3": `{"op":"recv","v":3,"id":"x3","envelope":7}`,
	} {
		peer.pushLine(line)
		waitFor(t, func() bool { _, ok := peer.errorFor(id); return ok }, "the error frame for "+id)
		if e, _ := peer.errorFor(id); e.V != WireVersion || !strings.Contains(e.Error, "member") {
			t.Errorf("%s: %+v", line, e)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if rec.count() != 0 || peer.totalAcks() != 0 {
		t.Fatalf("handed %d, acked %d", rec.count(), peer.totalAcks())
	}
	peer.pushLine(`{"op":"hello","v":3,"self":"aid-peer","lan":true}`)
	waitFor(t, func() bool { peer.mu.Lock(); defer peer.mu.Unlock(); return peer.ended > 0 }, "the session to end")
}

// A line longer than any frame, or one that is not JSON, ends the session
// rather than being taken in part; the transport dials the peer process again.
func TestALineThatIsNoFrameEndsTheSession(t *testing.T) {
	for _, line := range []string{"not json at all", `[1,2,3]`, "null"} {
		peer := newFakePeer(t)
		dialTransport(t, peer, &recorder{})
		peer.pushLine(line)
		waitFor(t, func() bool { peer.mu.Lock(); defer peer.mu.Unlock(); return peer.ended > 0 }, "the session to end after "+line)
	}
	if _, _, _, err := readFrame([]byte(`{"op":"send"`)); err == nil {
		t.Fatal("a cut line read as a frame")
	}
}
