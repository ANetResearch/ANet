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
	lix := getIXRT(t, prov, legacyIX)
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
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
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

// SetInboundPolicy puts the new policy in force before it saves the
// config, and does not put it back when the save fails (SetSpendLimits
// does: "a caller told the change failed must not find it in force"). The
// CLI reports the change as failed, config.json and `anet doctor` say
// closed, and the running daemon accepts strangers' tasks under open.
func TestRedteamSI5_FailedPolicySaveLeavesOpenInForceWhileDoctorSaysClosed(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	stranger := registered(t, srv.URL, "stranger")

	// The save fails (here: the data directory is not writable).
	if err := os.Chmod(prov.layout.Root, 0o500); err != nil {
		t.Fatal(err)
	}
	err := prov.SetInboundPolicy(PolicyOpen)
	if cerr := os.Chmod(prov.layout.Root, 0o700); cerr != nil {
		t.Fatal(cerr)
	}
	if err == nil {
		t.Skip("the save did not fail (running as root?)")
	}
	st, rerr := ReadPolicy(prov.layout)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if v, _ := st.SI5(); v["inbound.policy"] != PolicyClosed {
		t.Fatalf("setup: doctor view %v", v["inbound.policy"])
	}
	id, derr := stranger.Delegate(ctx, prov.AID(), "task from a stranger", nil)
	if derr != nil {
		t.Fatal(derr)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	ix, gerr := prov.ix.Get(id)
	if gerr != nil || ix.Trust != interactions.TrustPublic {
		t.Fatalf("attack failed: %+v %v", ix, gerr)
	}
	t.Logf("DEFECT: SetInboundPolicy returned %q, doctor says closed, daemon accepted stranger task %s as trust=public", err, id)
}
