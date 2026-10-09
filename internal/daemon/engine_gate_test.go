package daemon

// The outbound engine against the identity gate, a hub that speaks another
// wire, and the grant's direct address (engine ENG-K13, core CORE-V1, SS
// grantFor); and the rule that the engine alone writes the outbox.

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// signedPaths are the paths of the requests among rs that name this node
// (relayauth headers or a session).
func signedPaths(rs []fakeHubRequest) []string {
	var out []string
	for _, r := range rs {
		if r.header.Get(hubapi.HeaderAID) != "" || r.header.Get(hubapi.HeaderSig) != "" || r.header.Get(hubapi.HeaderSession) != "" {
			out = append(out, r.path)
		}
	}
	return out
}

// E-T12's notice_stale cell and TestGateReopensFromPoll (KEL D-1, engine
// ENG-K13, A2): a node whose witness notice is too old holds back what it
// would send and signs nothing but its mailbox poll and the ack of it; the
// poll's answer brings a fresh notice, the gate opens by itself, and the
// row goes.
func TestGateReopensFromPoll(t *testing.T) {
	srv, req, prov := quietPair(t)
	ctx := context.Background()
	fh := fakeHubAt(t, srv.URL)
	knowPeer(t, req, prov.AID())
	// Six hours on: the notice the node holds is past its age.
	clk := newSharedClock(uint64(time.Now().UnixMilli()) + 6*3600*1000)
	req.setClock(clk.fn())
	if g := req.identityGate(); g.Open || g.State != IdentityNoticeStale {
		t.Fatalf("gate %+v, want closed notice_stale", g)
	}
	req.setOutboxHold(true)
	from := len(hubRequests(t, srv.URL))
	id, err := req.Delegate(ctx, prov.AID(), "held by the gate", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.flushOutbox(ctx)
	req.maintainKeyRing() // a key set publication, were one due
	rows := outboxRows(t, req, id)
	if len(rows) != 1 || rows[0].HandedAt != 0 || rows[0].CurVer != 0 {
		t.Fatalf("outbox %+v, want the delegation unsealed and unsent", rows)
	}
	if w := rowWait(t, req, rows[0].ID); w != "notice_stale" {
		t.Fatalf("anet.delivery_wait %q, want notice_stale", w)
	}
	// The hub's answer to the poll carries a notice observed now.
	fh.mu.Lock()
	fh.noticeAt = clk.fn()
	fh.mu.Unlock()
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	for _, p := range signedPaths(hubRequests(t, srv.URL)[from:]) {
		if p != hubapi.RelayPollPath && p != hubapi.RelayAckPath {
			t.Fatalf("signed %s while the notice was stale", p)
		}
	}
	if g := req.identityGate(); !g.Open {
		t.Fatalf("gate %+v after the poll brought a fresh notice", g)
	}
	req.setOutboxHold(false)
	req.kickOutbox()
	waitUntil(t, "the delegation at the hub", func() bool { return len(queuedFor(t, srv, prov.AID())) == 1 })
}

// TestWireMismatchStopsEveryHubRequest (core CORE-V1, engine ENG-K13): a
// hub that answers over another wire is asked nothing more, on the signed
// side or the anonymous one, until the join flow has checked its identity
// again and it speaks this node's wire.
func TestWireMismatchStopsEveryHubRequest(t *testing.T) {
	srv, req, prov := quietPair(t)
	ctx := context.Background()
	req.setOutboxHold(true)
	prov.setOutboxHold(true)
	fh := fakeHubAt(t, srv.URL)
	fh.mu.Lock()
	ours := fh.wire
	fh.wire = ours - 1
	fh.mu.Unlock()
	_, _ = hubDirectory(ctx, req) // the answer that says another wire
	if !req.hubWireStopped(srv.URL) {
		t.Fatal("an answer over another wire did not stop the hub")
	}
	from := len(hubRequests(t, srv.URL))
	if err := req.pollOnce(ctx); !errors.Is(err, errHubWire) {
		t.Fatalf("a poll: %v, want the wire stop", err)
	}
	if _, err := hubDirectory(ctx, req); !errors.Is(err, errHubWire) {
		t.Fatalf("a directory read: %v, want the wire stop", err)
	}
	_, _ = req.Delegate(ctx, prov.AID(), "to a hub that speaks another wire", nil)
	req.flushOutbox(ctx)
	askHubNow(t, req)
	// The join flow's own check of the hub's identity is the one request
	// that goes (it runs in the background, kicked by the stop).
	var leaked []string
	for _, r := range hubRequests(t, srv.URL)[from:] {
		if r.method+" "+r.path != "GET /hub/identity" {
			leaked = append(leaked, r.method+" "+r.path)
		}
	}
	if len(leaked) != 0 {
		t.Fatalf("%d requests reached a hub that speaks another wire: %v", len(leaked), leaked)
	}
	// The hub speaks this node's wire again; the join flow checks it, and
	// requests resume.
	fh.mu.Lock()
	fh.wire = ours
	fh.mu.Unlock()
	if err := req.RegisterWithHub(ctx, srv.URL, "Req", nil); err != nil {
		t.Fatal(err)
	}
	if req.hubWireStopped(srv.URL) {
		t.Fatal("the stop outlived the join flow's check")
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatalf("a poll after the check: %v", err)
	}
}

// TestGrantOmitsAnUnfitP2PAddress (SS grantFor, core §2.3): the direct
// address a grant carries is this node's advertised one when it is a
// host:port a grant can hold; otherwise the grant goes without it, and the
// message goes all the same.
func TestGrantOmitsAnUnfitP2PAddress(t *testing.T) {
	for _, c := range []struct {
		name, advertise, want string
	}{
		{"fit", "192.0.2.7:4710", "192.0.2.7:4710"},
		{"too long", strings.Repeat("h", 300) + ":4710", ""},
		{"no port", "192.0.2.7:0", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, req, prov := quietPair(t)
			ctx := context.Background()
			raw, _ := json.Marshal(map[string]string{"advertise": c.advertise})
			prov.mu.Lock()
			prov.cfg.Modules = ModulesConfig{"p2p": raw}
			prov.mu.Unlock()
			id, err := req.Delegate(ctx, prov.AID(), "a task", nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := prov.pollOnce(ctx); err != nil {
				t.Fatal(err)
			}
			if err := prov.SendMessage(ctx, id, "an answer", nil); err != nil {
				t.Fatal(err)
			}
			if err := req.pollOnce(ctx); err != nil {
				t.Fatal(err)
			}
			if n := countMsgs(t, req, id); n != 2 {
				t.Fatalf("the requester holds %d messages, want the answer", n)
			}
			held, _ := req.ix.HeldCaps(prov.AID())
			var peer *interactions.HeldCap
			for i := range held {
				if held[i].Scope == interactions.CapScopePeer {
					peer = &held[i]
				}
			}
			if peer == nil || peer.P2P != c.want {
				t.Fatalf("held peer capability %+v, want p2p %q", peer, c.want)
			}
		})
	}
}

// engineFiles are the files of the outbound engine (engine §1.1): the
// only ones that write the outbox and its versions (ENG-G1).
var engineFiles = map[string]bool{
	"outbox_step.go": true, "outbox_attempt.go": true, "outbox_queue.go": true, "outbox_scan.go": true,
	"outbox_seams.go": true, "dack_status.go": true, "retry.go": true, "reseal.go": true,
}

// outboxWriters are the store's writes of the outbox and its versions.
var outboxWriters = map[string]bool{
	"EnqueueOutbox": true, "SealOutboxVersion": true, "BeginOutboxAttempt": true, "EndOutboxAttempt": true,
	"NoteOutboxCheck": true, "SetOutboxNextAt": true, "DeleteOutboxRow": true, "DeleteOutboxQueue": true,
	"DeleteOutboxFollowUps": true, "FreezeOutboxDelegation": true, "HurryOutbox": true,
	"RecoverOutboxAttempts": true, "RecoverOutboxAttempt": true,
}

// TestOnlyTheEngineWritesTheOutbox (ENG-G1): outside the engine's files, no
// code of the daemon calls a write of the outbox; the rest of the daemon
// goes through the engine's entry points (outbox_seams.go, queueSend).
func TestOnlyTheEngineWritesTheOutbox(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	seen, calls := 0, 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		seen++
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !outboxWriters[sel.Sel.Name] {
				return true
			}
			calls++
			if !engineFiles[name] {
				t.Errorf("%s: %s writes the outbox outside the engine", fset.Position(call.Pos()), sel.Sel.Name)
			}
			return true
		})
	}
	if seen < 50 || calls < 10 {
		t.Fatalf("read %d files and %d outbox writes: the scan is not looking where it should", seen, calls)
	}
}

// The opening of a first-contact session is every waiting send's: it runs
// under a deadline of its own, so the send that started it giving up does
// not leave another waiting on it with session_unavailable.
func TestASessionOpeningOutlivesTheCallerThatStartedIt(t *testing.T) {
	srv, req, _ := quietPair(t)
	entered, release := make(chan struct{}, 1), make(chan struct{})
	setFake(t, srv.URL, func(h *fakeHub) {
		h.sessionHook = func() {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
		}
	})
	ctx1, cancel1 := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, err := req.firstContactSession(ctx1, srv.URL); first <- err }()
	<-entered
	second := make(chan error, 1)
	go func() { _, err := req.firstContactSession(context.Background(), srv.URL); second <- err }()
	time.Sleep(50 * time.Millisecond) // the second waits on the opening
	cancel1()
	time.Sleep(50 * time.Millisecond)
	close(release)
	if err := <-second; err != nil {
		t.Fatalf("the send waiting on the opening: %v", err)
	}
	<-first
}
