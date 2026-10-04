package daemon

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/module"
)

// fakeTransport is a direct path that can be told how to behave.
type fakeTransport struct {
	name      string
	reachable bool
	fail      error

	mu   sync.Mutex
	sent []string
	envs [][]byte
}

func (f *fakeTransport) Name() string { return f.name }
func (f *fakeTransport) Reachable(context.Context, string) bool {
	return f.reachable
}
func (f *fakeTransport) Send(_ context.Context, toAID string, env []byte) ([]byte, bool, error) {
	f.mu.Lock()
	f.sent = append(f.sent, toAID)
	f.envs = append(f.envs, append([]byte(nil), env...))
	f.mu.Unlock()
	return nil, true, f.fail
}

// recvErr is a direct delivery's acknowledge-or-refuse answer, without the
// delivery confirmation it carries.
func recvErr(in module.Inbound, ctx context.Context, env []byte) error {
	_, err := in.Receive(ctx, env)
	return err
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

	if err := d.relaySend(context.Background(), peer, seal.TypeDelegate, "ix-1", []byte("x")); err != nil {
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

	if err := d.relaySend(context.Background(), peer, seal.TypeDelegate, "ix-1", []byte("x")); err != nil {
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

	if err := d.relaySend(context.Background(), peer, seal.TypeDelegate, "ix-1", []byte("x")); err != nil {
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

	err := d.deliverEnvelope(context.Background(), "peer-aid", []byte("x"))
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
	if err := d.relaySend(context.Background(), peer, seal.TypeMessage, "ix-1", []byte("x")); err != nil {
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
	if err := d.RegisterWithHub(ctx, srv.URL, "Sender", nil); err != nil {
		t.Fatal(err)
	}
	if err := peer.RegisterWithHub(ctx, srv.URL, "Peer", nil); err != nil {
		t.Fatal(err)
	}
	return d, peer.AID()
}

// §3.5 step 1: a recipient whose encryption keys cannot be resolved gets
// nothing. The send fails with an error naming the cause, and no transport
// is handed any bytes — there is no unsealed fallback (SI-3).
func TestARecipientWithoutKeysIsSentNothing(t *testing.T) {
	srv := newFakeHub(t)
	d, _ := twoRegistered(t, srv)
	direct := &fakeTransport{name: "p2p", reachable: true}
	d.RegisterTransport(direct)
	nobody := newStranger(t) // registered nowhere, so the hub has no keys for it
	err := d.relaySend(context.Background(), nobody.aid, seal.TypeDelegate, "ix-1", []byte("the task in the clear"))
	if err == nil {
		t.Fatal("a message to a recipient without keys was reported sent")
	}
	if !strings.Contains(err.Error(), "encryption keys") {
		t.Errorf("the error does not name the missing keys: %v", err)
	}
	if n := direct.count(); n != 0 {
		t.Fatalf("a transport was handed %d messages for a recipient without keys", n)
	}
	if n := relayCountFor(srv.URL); n != 0 {
		t.Fatalf("the hub carried %d messages for a recipient without keys", n)
	}
}

// T49 at the hub path: a failed hub send handed the envelope over only when
// the /relay/send request got a connection (hubapi.TraceHanded). The hub's
// identity, looked up first, is a request of its own that carries no byte
// of the envelope: a hub that answers it and is then gone, or that cannot
// give an identity this node accepts, was handed nothing. A hub that took
// the connection was handed the envelope, whatever it answered.
func TestTheHubPathIsHandedOnceRelaySendConnects(t *testing.T) {
	ctx := context.Background()

	t.Run("the identity is served and then the hub is gone", func(t *testing.T) {
		fake := newFakeHub(t)
		var others atomic.Int32
		var front *httptest.Server
		front = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/hub/identity" {
				others.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			// Answer the lookup and stop listening, so the request that
			// follows cannot connect.
			front.Listener.Close()
			w.Header().Set("Connection", "close")
			fake.Config.Handler.ServeHTTP(w, r)
		}))
		t.Cleanup(front.Close)
		d := newTestDaemon(t, front.URL, false)
		_, handed, err := (hubTransport{d}).Send(ctx, "did:anet:someone", []byte("x"))
		if err == nil {
			t.Fatal("a send to a hub that stopped listening succeeded")
		}
		if strings.Contains(err.Error(), "identity") {
			t.Fatalf("the identity lookup failed (%v); the case needs it to succeed", err)
		}
		if handed {
			t.Fatalf("handed after an identity lookup that connected and a /relay/send that could not (%v)", err)
		}
		if n := others.Load(); n != 0 {
			t.Fatalf("the hub saw %d requests besides the identity lookup", n)
		}
	})

	t.Run("the identity is refused", func(t *testing.T) {
		srv := newFakeHub(t)
		fh := fakeHubAt(t, srv.URL)
		fh.mu.Lock()
		fh.identityDown = true
		fh.mu.Unlock()
		d := newTestDaemon(t, srv.URL, false)
		_, handed, err := (hubTransport{d}).Send(ctx, "did:anet:someone", []byte("x"))
		if err == nil || handed {
			t.Fatalf("handed=%v err=%v; want a failure that handed nothing over", handed, err)
		}
	})

	t.Run("nobody listens", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := l.Addr().String()
		l.Close()
		d := newTestDaemon(t, "http://"+addr, false)
		if _, handed, err := (hubTransport{d}).Send(ctx, "did:anet:someone", []byte("x")); err == nil || handed {
			t.Fatalf("handed=%v err=%v; want a failure that handed nothing over", handed, err)
		}
	})

	t.Run("the hub answers", func(t *testing.T) {
		srv := newFakeHub(t)
		d, peer := twoRegistered(t, srv)
		// Not a sealed envelope: the hub answers 400, and it had it.
		if _, handed, err := (hubTransport{d}).Send(ctx, peer, []byte("x")); err == nil || !handed {
			t.Fatalf("a 4xx: handed=%v err=%v; want a failure that handed the envelope over", handed, err)
		}
		fh := fakeHubAt(t, srv.URL)
		fh.mu.Lock()
		fh.relayDown = true
		fh.mu.Unlock()
		if _, handed, err := (hubTransport{d}).Send(ctx, peer, []byte("x")); err == nil || !handed {
			t.Fatalf("a 503: handed=%v err=%v; want a failure that handed the envelope over", handed, err)
		}
	})
}

// A transport module's malformed confirmation is the node's dack-bad-frame
// count (DLV-W-11b, §2.8), where /status.delivery.counters reads it.
func TestATransportsMalformedConfirmationIsCounted(t *testing.T) {
	d := newTestDaemon(t, "", false)
	var th module.TransportHost = moduleHost{d}
	before := counter(d, "dack-bad-frame")
	th.NoteBadDAck()
	th.NoteBadDAck()
	if n := counter(d, "dack-bad-frame") - before; n != 2 {
		t.Fatalf("dack-bad-frame went up by %d for two malformed confirmations", n)
	}
}
