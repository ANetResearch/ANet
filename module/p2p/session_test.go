//go:build !no_p2p

package p2p

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/backendconn"
)

// A peer process that has stopped reading does not hold the daemon: a
// write to it gives up after sessionWriteTimeout, the send that made it
// fails (handed, part of its frame having left) so that the daemon goes on
// to the hub, the send queued behind it fails too, and the session ends —
// its read loop with it — and is dialled again. Before, the
// write had no deadline and held the transport's lock: Send, every other
// send and the read loop waited on it for as long as the peer process
// chose.
func TestSessionWriteHasADeadline(t *testing.T) {
	old := sessionWriteTimeout
	sessionWriteTimeout = 200 * time.Millisecond
	t.Cleanup(func() { sessionWriteTimeout = old })

	// The peer process: reads the hello, then nothing more, its end left
	// open.
	sock := filepath.Join(t.TempDir(), "peer.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	sessions := make(chan net.Conn, 8)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() })
			if _, err := bufio.NewReader(c).ReadString('\n'); err == nil {
				sessions <- c
			}
		}
	}()
	tr := &Transport{socket: sock, selfAID: "aid-self", sendTimeout: 10 * time.Minute, inbound: &recorder{}, addrOf: anyAddr}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); tr.close() })
	go tr.run(ctx)
	select {
	case <-sessions:
	case <-time.After(30 * time.Second):
		t.Fatal("the transport did not connect")
	}
	waitFor(t, func() bool { return tr.Reachable(context.Background(), "aid-peer") }, "the session to be up")

	// Frames far larger than the socket's buffers: the first write blocks
	// on a reader that is not there.
	big := bytes.Repeat([]byte{0x5a}, 8<<20)
	type result struct {
		handed bool
		err    error
	}
	done := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, handed, err := tr.Send(context.Background(), "aid-peer", big)
			done <- result{handed, err}
		}()
	}
	handed := 0
	for i := 0; i < 2; i++ {
		select {
		case r := <-done:
			if r.err == nil {
				t.Fatal("a send to a peer process that does not read succeeded")
			}
			if r.handed {
				handed++
			}
		case <-time.After(60 * time.Second):
			t.Fatal("a send to a peer process that stopped reading is still waiting: the write has no deadline")
		}
	}
	if handed == 0 {
		t.Error("no send reports its frame as handed, though a part of it reached the socket")
	}
	// The session ended and is dialled again.
	select {
	case <-sessions:
	case <-time.After(30 * time.Second):
		t.Fatal("the session was not dialled again after the write gave up")
	}
}

// M-PC-2: the peer process is reached through backendconn, so a socket
// whose listener is not this daemon's account is sent nothing, not even the
// hello that names this node's AID. The test cannot listen as another
// account (internal/backendconn and tools/anetpeer do, with sudo); it
// expects one it is not, which is the same check.
func TestAPeerProcessOfAnotherAccountIsSentNothing(t *testing.T) {
	peer := newFakePeer(t)
	other := os.Getuid() + 1
	tr := &Transport{socket: peer.socket(), selfAID: "aid-self", sendTimeout: time.Second, inbound: &recorder{}, listenerUID: &other, addrOf: anyAddr}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- tr.session(ctx) }()
	select {
	case err := <-errc:
		var re *backendconn.RefusedError
		if !errors.As(err, &re) || re.Code != backendconn.CodePeerUIDMismatch {
			t.Fatalf("a peer process of another account: %v", err)
		}
		// The log line says what to do with a peer process: modules.p2p has
		// no expected_user to set.
		if msg := err.Error(); !strings.Contains(msg, "run anetpeer as this daemon's user") || strings.Contains(msg, "expected_user") {
			t.Fatalf("the refusal says %q", msg)
		}
	case <-ctx.Done():
		t.Fatal("the session went on with a peer process of another account")
	}
	if tr.Reachable(context.Background(), "aid-peer") {
		t.Fatal("reachable through a peer process of another account")
	}
	peer.mu.Lock()
	told, sent := peer.self, len(peer.sent)
	peer.mu.Unlock()
	if told != "" || sent != 0 {
		t.Fatalf("the peer process was told %q and sent %d frames", told, sent)
	}
	// This daemon's account is let through: the default.
	tr2 := &Transport{socket: peer.socket(), selfAID: "aid-self", sendTimeout: time.Second, inbound: &recorder{}}
	ctx2, cancel2 := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel2(); tr2.close() })
	go tr2.run(ctx2)
	waitFor(t, func() bool { return peer.helloSelf() == "aid-self" }, "the peer to be told our AID")
}
