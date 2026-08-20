package daemon

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeTransport is a direct path that can be told how to behave.
type fakeTransport struct {
	name      string
	reachable bool
	fail      error

	mu   sync.Mutex
	sent []string
}

func (f *fakeTransport) Name() string { return f.name }
func (f *fakeTransport) Reachable(context.Context, string) bool {
	return f.reachable
}
func (f *fakeTransport) Send(_ context.Context, toAID, kind, id string, _ []byte) error {
	f.mu.Lock()
	f.sent = append(f.sent, kind+"->"+toAID)
	f.mu.Unlock()
	return f.fail
}
func (f *fakeTransport) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

// A direct transport is preferred over the hub: a delegation between two
// nodes on the same network should not need a round trip through someone
// else's server.
func TestDirectTransportIsPreferredOverTheHub(t *testing.T) {
	srv := newFakeHub(t)
	d, peer := twoRegistered(t, srv)

	direct := &fakeTransport{name: "p2p", reachable: true}
	d.RegisterTransport(direct)

	if err := d.relaySend(context.Background(), peer, "delegate", "ix-1", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if direct.count() != 1 {
		t.Fatalf("the direct transport should have carried it, sent=%d", direct.count())
	}
	if n := relayCountFor(srv.URL); n != 0 {
		t.Fatalf("the hub should not have been used, got %d relay posts", n)
	}
}

// The hub catches what a direct transport cannot reach. This is the whole
// reason the hub is never removed from the list: an offline peer, a NAT
// that hole-punching lost, a peer this node has never seen.
func TestHubCatchesAnUnreachablePeer(t *testing.T) {
	srv := newFakeHub(t)
	d, peer := twoRegistered(t, srv)

	direct := &fakeTransport{name: "p2p", reachable: false}
	d.RegisterTransport(direct)

	if err := d.relaySend(context.Background(), peer, "delegate", "ix-1", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if direct.count() != 0 {
		t.Fatal("an unreachable transport must not be asked to try")
	}
	if n := relayCountFor(srv.URL); n != 1 {
		t.Fatalf("the hub should have carried it, got %d relay posts", n)
	}
}

// A direct transport that says it can reach a peer and then fails must not
// lose the delegation: the hub is tried next.
func TestFailedDirectSendFallsThroughToTheHub(t *testing.T) {
	srv := newFakeHub(t)
	d, peer := twoRegistered(t, srv)

	direct := &fakeTransport{name: "p2p", reachable: true, fail: errors.New("connection reset")}
	d.RegisterTransport(direct)

	if err := d.relaySend(context.Background(), peer, "delegate", "ix-1", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if direct.count() != 1 {
		t.Fatal("the direct transport should have been tried")
	}
	if n := relayCountFor(srv.URL); n != 1 {
		t.Fatalf("the hub should have caught the failure, got %d relay posts", n)
	}
}

// With nothing able to deliver, the caller gets the last real failure —
// not a summary. An operator debugging delivery wants to know what the hub
// said.
func TestEveryPathFailingReportsTheLastError(t *testing.T) {
	d := newTestDaemon(t, "", false) // no hub configured

	direct := &fakeTransport{name: "p2p", reachable: true, fail: errors.New("connection reset")}
	d.RegisterTransport(direct)

	err := d.relaySend(context.Background(), "peer-aid", "delegate", "ix-1", []byte("x"))
	if err == nil {
		t.Fatal("delivery with no working path must fail")
	}
	if !strings.Contains(err.Error(), "connection reset") {
		t.Errorf("the error should name what actually failed: %v", err)
	}
}

// A build with no transport module still delivers: the hub is always in the
// list, which is what "sometimes you only distribute alongside a hub" means.
func TestHubOnlyBuildStillDelivers(t *testing.T) {
	srv := newFakeHub(t)
	d, peer := twoRegistered(t, srv)

	if got := len(d.transports()); got != 1 {
		t.Fatalf("a build with no transport module should have exactly the hub, got %d", got)
	}
	if err := d.relaySend(context.Background(), peer, "message", "ix-1", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if n := relayCountFor(srv.URL); n != 1 {
		t.Fatalf("hub relay posts = %d, want 1", n)
	}
}

// twoRegistered gives a sending daemon and the AID of a peer that the hub
// knows about — the hub refuses delivery to a stranger, which is the
// behaviour these tests need rather than one to work around.
func twoRegistered(t *testing.T, srv *httptest.Server) (*Daemon, string) {
	t.Helper()
	ctx := context.Background()
	d := newTestDaemon(t, srv.URL, false)
	peer := newTestDaemon(t, srv.URL, true)
	if err := d.RegisterWithHub(ctx, srv.URL, "Sender", nil, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}
	if err := peer.RegisterWithHub(ctx, srv.URL, "Peer", nil, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}
	return d, peer.AID()
}
