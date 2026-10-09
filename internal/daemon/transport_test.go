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

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
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

// confirmingTransport is a direct path to peer that answers as peer's
// node does once it has the envelope: with the delivery token of outcome 1,
// made under the envelope's ack_key (a test double of the receive side,
// engine ENG-G3). confirmFail makes it answer with no token.
type confirmingTransport struct {
	fakeTransport
	peer *Daemon
}

func (c *confirmingTransport) Send(ctx context.Context, toAID string, env []byte) ([]byte, bool, error) {
	if _, _, err := c.fakeTransport.Send(ctx, toAID, env); err != nil {
		return nil, true, err
	}
	op, err := seal.Open(env, c.peer.AID(), c.peer.enc, nil)
	if err != nil {
		return nil, true, err
	}
	dack, err := op.AckKey.MakeDAck(seal.OutcomeProcessed, seal.Note{})
	if err != nil {
		return nil, true, err
	}
	return dack[:], true, nil
}

// queueTo queues body to `to` as d's message on interaction ix and makes
// the first attempt (engine §2.2, ENG-Q8); it returns the row.
func queueTo(t *testing.T, d *Daemon, to, ix string, body []byte) (int64, error) {
	t.Helper()
	id, err := d.queueSend(context.Background(), to, seal.TypeMessage, ix, body, nil)
	if err != nil {
		t.Fatal(err)
	}
	return id, d.deliverQueued(context.Background(), id)
}

// A direct transport is preferred over the hub: a message between two
// nodes on the same network does not need a round trip through someone
// else's server, and the token the direct path brings back confirms it
// there and then (engine A10).
func TestDirectTransportIsPreferredOverTheHub(t *testing.T) {
	srv := newFakeHub(t)
	d, peer := twoRegistered(t, srv)

	direct := &confirmingTransport{fakeTransport: fakeTransport{name: "p2p", reachable: true}, peer: peer}
	d.RegisterTransport(direct)

	id, err := queueTo(t, d, peer.AID(), "ix-1", chatBody(t, "x", ""))
	if err != nil {
		t.Fatal(err)
	}
	if direct.count() != 1 {
		t.Fatalf("the direct transport should have carried it, sent=%d", direct.count())
	}
	if n := relayCountFor(srv.URL); n != 0 {
		t.Fatalf("the hub should not have been used, got %d relay posts", n)
	}
	if _, _, err := d.ix.OutboxState(id); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatalf("the direct token did not confirm the row: %v", err)
	}
	if counter(d, cntDAckConfirmedP2P) != 1 {
		t.Fatal("the confirmation was not counted as the direct path's")
	}
}

// The hub catches what a direct transport cannot reach: an offline peer, a
// NAT that hole-punching lost, a peer this node has never seen.
func TestHubCatchesAnUnreachablePeer(t *testing.T) {
	srv := newFakeHub(t)
	d, peer := twoRegistered(t, srv)

	direct := &fakeTransport{name: "p2p", reachable: false}
	d.RegisterTransport(direct)

	if _, err := queueTo(t, d, peer.AID(), "ix-1", chatBody(t, "x", "")); err != nil {
		t.Fatal(err)
	}
	if direct.count() != 0 {
		t.Fatal("an unreachable transport must not be asked to try")
	}
	if n := relayCountFor(srv.URL); n != 1 {
		t.Fatalf("the hub should have carried it, got %d relay posts", n)
	}
}

// A direct transport that took the bytes and brought no valid token back
// does not lose the message: the hub is tried next, in the same attempt and
// with the same version (ENG-A3), and the direct path backs off for that
// peer (DLV-S-30).
func TestFailedDirectSendFallsThroughToTheHub(t *testing.T) {
	srv := newFakeHub(t)
	d, peer := twoRegistered(t, srv)

	direct := &fakeTransport{name: "p2p", reachable: true, fail: errors.New("connection reset")}
	d.RegisterTransport(direct)

	if _, err := queueTo(t, d, peer.AID(), "ix-1", chatBody(t, "x", "")); err != nil {
		t.Fatal(err)
	}
	if direct.count() != 1 {
		t.Fatal("the direct transport should have been tried")
	}
	if n := relayCountFor(srv.URL); n != 1 {
		t.Fatalf("the hub should have caught the failure, got %d relay posts", n)
	}
	if d.p2pPath(context.Background(), peer.AID(), d.nowMS()) != nil {
		t.Fatal("the direct path does not back off after taking bytes without a token")
	}
	// The same version went both ways (ENG-A3).
	got, _ := seal.EnvDigest(direct.envs[0])
	want, _ := seal.EnvDigest(queuedFor(t, srv, peer.AID())[0])
	if got != want {
		t.Fatal("the direct path and the hub were handed different versions")
	}
}

// With nothing able to deliver, the row waits with what failed as its
// last_error — a code, never a path's own words (engine ENG-S4) — and is
// not given up.
func TestEveryPathFailingKeepsTheRowWaiting(t *testing.T) {
	srv := newFakeHub(t)
	d, peer := twoRegistered(t, srv)
	direct := &fakeTransport{name: "p2p", reachable: true, fail: errors.New("connection reset")}
	d.RegisterTransport(direct)
	setFake(t, srv.URL, func(h *fakeHub) { h.relayDown = true })

	id, err := queueTo(t, d, peer.AID(), "ix-1", chatBody(t, "x", ""))
	if err == nil {
		t.Fatal("delivery with no working path must fail")
	}
	row, _, err := d.ix.OutboxState(id)
	if err != nil {
		t.Fatal(err)
	}
	if row.Attempts != 1 || row.LastError != "-hub_503" || strings.Contains(row.LastError, "reset") {
		t.Errorf("the row after every path failed: attempts %d, last_error %q", row.Attempts, row.LastError)
	}
}

// A build with no transport module still delivers: the hub is the path
// the engine always has.
func TestHubOnlyBuildStillDelivers(t *testing.T) {
	srv := newFakeHub(t)
	d, peer := twoRegistered(t, srv)

	if got := len(d.directTransports()); got != 0 {
		t.Fatalf("a build with no transport module has no direct path, got %d", got)
	}
	if _, err := queueTo(t, d, peer.AID(), "ix-1", chatBody(t, "x", "")); err != nil {
		t.Fatal(err)
	}
	if n := relayCountFor(srv.URL); n != 1 {
		t.Fatalf("hub relay posts = %d, want 1", n)
	}
}

// twoRegistered gives a sending daemon and a peer the hub knows about — the
// hub refuses delivery to a stranger, which is the behaviour these tests
// need rather than one to work around.
func twoRegistered(t *testing.T, srv *httptest.Server) (*Daemon, *Daemon) {
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
	return d, peer
}

// §3.5 step 1: a recipient whose encryption keys cannot be resolved gets
// nothing. The attempt waits on -no_keys (engine ENG-S4, A7), and no
// transport is handed any bytes — there is no unsealed fallback (SI-3).
func TestARecipientWithoutKeysIsSentNothing(t *testing.T) {
	srv := newFakeHub(t)
	d, _ := twoRegistered(t, srv)
	direct := &fakeTransport{name: "p2p", reachable: true}
	d.RegisterTransport(direct)
	nobody := newStranger(t) // registered nowhere, so the hub has no keys for it
	id, err := queueTo(t, d, nobody.aid, "ix-1", chatBody(t, "the task in the clear", ""))
	if err == nil {
		t.Fatal("a message to a recipient without keys was reported sent")
	}
	row, _, rerr := d.ix.OutboxState(id)
	if rerr != nil || !strings.HasPrefix(row.LastError, errNoKeys) || row.CurVer != 0 || row.HandedAt != 0 {
		t.Errorf("the row of a recipient without keys: %+v %v", row, rerr)
	}
	if n := direct.count(); n != 0 {
		t.Fatalf("a transport was handed %d messages for a recipient without keys", n)
	}
	if n := relayCountFor(srv.URL); n != 0 {
		t.Fatalf("the hub carried %d messages for a recipient without keys", n)
	}
}

// T49 at the hub path (engine ENG-A4): a failed hub send handed the
// envelope over only when the /relay/send request got a connection
// (hubapi.TraceHanded). The hub's identity, looked up first, is a request
// of its own that carries no byte of the envelope: a hub that answers it
// and is then gone, or that cannot give an identity this node accepts, was
// handed nothing. A hub that took the connection was handed the envelope,
// whatever it answered.
//
// Deterministic (ENG-1a, W3 EXE-10′ 5): the sender of the first three cases
// has no hub of its own while the case runs, so its outbound gate holds
// back every signed request the background loops might make (KEL D-1) and
// nothing races the case's own requests to the hub.
func TestTheHubPathIsHandedOnceRelaySendConnects(t *testing.T) {
	ctx := context.Background()
	underHub := func(d *Daemon, url string) {
		d.mu.Lock()
		d.cfg.HubURL = url
		d.mu.Unlock()
	}

	t.Run("the identity is served and then the hub is gone", func(t *testing.T) {
		fake := newFakeHub(t)
		var identities, others atomic.Int32
		var front *httptest.Server
		front = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/hub/identity" {
				others.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			identities.Add(1)
			// Answer the lookup and stop listening, so the request that
			// follows cannot connect.
			front.Listener.Close()
			w.Header().Set("Connection", "close")
			fake.Config.Handler.ServeHTTP(w, r)
		}))
		t.Cleanup(front.Close)
		d := newTestDaemon(t, "", false)
		underHub(d, front.URL)
		if _, _, err := d.hubIdentity(ctx, front.URL); err != nil {
			t.Fatalf("the identity lookup failed (%v); the case needs it to succeed", err)
		}
		// Under a capability: the send is the first request after the
		// identity's (a first contact would open a session first).
		env := outerEnvelope(t, "did:anet:someone", seal.AccessKindCap)
		_, handed, err := d.relaySendV3(ctx, env, AccessChoice{Send: true, Kind: seal.AccessKindCap})
		if err == nil {
			t.Fatal("a send to a hub that stopped listening succeeded")
		}
		if handed {
			t.Fatalf("handed after an identity lookup that connected and a /relay/send that could not (%v)", err)
		}
		if n, o := identities.Load(), others.Load(); n != 1 || o != 0 {
			t.Fatalf("the hub saw %d identity lookups and %d other requests; want the case's one lookup only", n, o)
		}
	})

	t.Run("the identity is refused", func(t *testing.T) {
		srv := newFakeHub(t)
		setFake(t, srv.URL, func(h *fakeHub) { h.identityDown = true })
		d := newTestDaemon(t, "", false)
		underHub(d, srv.URL)
		env := outerEnvelope(t, "did:anet:someone", seal.AccessKindCap)
		_, handed, err := d.relaySendV3(ctx, env, AccessChoice{Send: true, Kind: seal.AccessKindCap})
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
		d := newTestDaemon(t, "", false)
		underHub(d, "http://"+addr)
		env := outerEnvelope(t, "did:anet:someone", seal.AccessKindCap)
		if _, handed, err := d.relaySendV3(ctx, env, AccessChoice{Send: true, Kind: seal.AccessKindCap}); err == nil || handed {
			t.Fatalf("handed=%v err=%v; want a failure that handed nothing over", handed, err)
		}
	})

	t.Run("the hub answers", func(t *testing.T) {
		srv := newFakeHub(t)
		d, peer := twoRegistered(t, srv)
		kind0 := AccessChoice{Send: true, Kind: seal.AccessKindSession}
		// A first contact to nobody registered: the hub answers 404, and
		// it had it.
		if _, handed, err := d.relaySendV3(ctx, outerEnvelope(t, "did:anet:someone", seal.AccessKindSession), kind0); err == nil || !handed {
			t.Fatalf("a 4xx: handed=%v err=%v; want a failure that handed the envelope over", handed, err)
		}
		setFake(t, srv.URL, func(h *fakeHub) { h.relayDown = true })
		if _, handed, err := d.relaySendV3(ctx, outerEnvelope(t, peer.AID(), seal.AccessKindSession), kind0); err == nil || !handed {
			t.Fatalf("a 503: handed=%v err=%v; want a failure that handed the envelope over", handed, err)
		}
	})
}

// outerEnvelope is an envelope to `to` that passes seal.ParseOuter — what a
// hub looks at — under an access of kind: a first contact, or a capability
// of the right shape that verifies under nobody's key. Its ciphertext opens
// for no one.
func outerEnvelope(t testing.TB, to string, kind uint64) []byte {
	t.Helper()
	a := &seal.Access{Kind: kind}
	if kind == seal.AccessKindCap {
		a.Cap = &seal.Cap{Body: []byte{0xa0}, Sig: make([]byte, seal.SigLen)}
		a.Proof = make([]byte, seal.SigLen)
	}
	env := &seal.SealedEnvelope{V: seal.EnvelopeVersion, To: to, Suite: seal.SuiteX25519,
		KID: make([]byte, seal.KIDLen), Enc: make([]byte, 32), CT: []byte("sealed"), Access: a}
	b, err := env.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seal.ParseOuter(b); err != nil {
		t.Fatal(err)
	}
	return b
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
