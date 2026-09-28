package main

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A published address that cannot be dialled is reported unreachable for a
// while, so the daemon goes to the hub at once instead of spending its
// whole send timeout on every message (docs/notes/0025: dmax → cmax, whose
// firewall drops inbound connections, cost three seconds per message). The
// window grows while the address keeps failing, is capped, is forgotten when
// a delivery gets through, and does not apply to an address the peer moved to.
func TestAnAddressThatCannotBeDialledIsLeftAloneForAWhile(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	oldNow, oldMin, oldMax := timeNow, dialDownMin, dialDownMax
	timeNow = func() time.Time { return now }
	dialDownMin, dialDownMax = 30*time.Second, 2*time.Minute
	t.Cleanup(func() { timeNow, dialDownMin, dialDownMax = oldNow, oldMin, oldMax })

	rv := t.TempDir()
	publish := func(addr string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(rv, "aid-far"), []byte(addr), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dead := closedTCP(t)
	publish(dead)
	p := &peer{rendezvous: rv, self: "aid-local"}
	send := func() error { return p.deliver(frame{To: "aid-far", ID: "1", Envelope: "eA=="}) }

	if !p.reachable("aid-far") {
		t.Fatal("a published address is reachable until a dial to it fails")
	}
	if err := send(); err == nil {
		t.Fatal("a dial to a closed port succeeded")
	}
	if p.reachable("aid-far") {
		t.Error("reported reachable right after its address could not be dialled")
	}
	// A second failure inside the window (a concurrent send) neither
	// extends nor grows it.
	if w := p.markDown("aid-far", dead); w != 0 {
		t.Errorf("a failure inside the window opened another one (%s)", w)
	}

	now = now.Add(dialDownMin + time.Second)
	if !p.reachable("aid-far") {
		t.Fatal("still unreachable after the first window: a peer that came back would never be tried")
	}
	_ = send() // the retry fails: the window doubles
	now = now.Add(dialDownMin + time.Second)
	if p.reachable("aid-far") {
		t.Error("the window did not grow after a failed retry")
	}
	now = now.Add(dialDownMin)
	if !p.reachable("aid-far") {
		t.Error("the doubled window did not end")
	}

	// Capped.
	for i := 0; i < 6; i++ {
		_ = send()
		now = now.Add(dialDownMax + time.Second)
	}
	_ = send()
	now = now.Add(dialDownMax - time.Second)
	if p.reachable("aid-far") {
		t.Error("the window ended before the cap")
	}
	now = now.Add(2 * time.Second)
	if !p.reachable("aid-far") {
		t.Error("the window grew past the cap")
	}
	_ = send()

	// The peer moved: its new address is tried at once, and a delivery that
	// gets through forgets the mark.
	publish(answeringPeer(t))
	if !p.reachable("aid-far") {
		t.Fatal("a new address was not tried because the old one had failed")
	}
	if err := send(); err != nil {
		t.Fatalf("delivery to the new address: %v", err)
	}
	publish(dead)
	_ = send()
	now = now.Add(dialDownMin + time.Second)
	if !p.reachable("aid-far") {
		t.Error("after a delivery got through, the next failure did not start again at the shortest window")
	}
}

// closedTCP is a loopback address nothing listens on: a dial to it fails at
// once.
func closedTCP(t *testing.T) string {
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

// answeringPeer is a peer wire that accepts every delivery.
func answeringPeer(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				var f frame
				if err := json.NewDecoder(bufio.NewReader(c)).Decode(&f); err != nil {
					return
				}
				_ = json.NewEncoder(c).Encode(frame{Op: "send", V: wireVersion, ID: f.ID})
			}(c)
		}
	}()
	return l.Addr().String()
}
