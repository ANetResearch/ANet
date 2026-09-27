package interactions_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// IsTerminal is the Go side of the terminal set; the guarded SQL writes
// carry the same set as a literal. This pins both to the four states of
// A2A-DESIGN §4.1.
func TestTerminalStates(t *testing.T) {
	want := map[interactions.State]bool{
		interactions.StateSubmitted:     false,
		interactions.StateWorking:       false,
		interactions.StateInputRequired: false,
		interactions.StateCompleted:     true,
		interactions.StateFailed:        true,
		interactions.StateCanceled:      true,
		interactions.StateRejected:      true,
	}
	s := open(t)
	for st, term := range want {
		if st.IsTerminal() != term {
			t.Errorf("%s: IsTerminal = %v, want %v", st, st.IsTerminal(), term)
		}
		id := "ix_" + string(st)
		if err := s.Put(id, interactions.RoleOutbound, "did:anet:p", "g", "", nil); err != nil {
			t.Fatal(err)
		}
		if st != interactions.StateSubmitted {
			if _, err := s.SetState(id, st); err != nil {
				t.Fatal(err)
			}
		}
		// The SQL guard must agree: a write after a terminal state is refused.
		changed, err := s.SetState(id, interactions.StateWorking)
		if err != nil {
			t.Fatal(err)
		}
		if st == interactions.StateWorking {
			continue // same state, no change either way
		}
		if changed == term {
			t.Errorf("%s: SetState(working) changed=%v, want %v", st, changed, !term)
		}
	}
}

// A terminal state is never overwritten, a result is not stored on a
// canceled interaction, and state_seq advances on every real change and
// not on a repeated one (a waiter compares sequence numbers, C35).
func TestStateWritesAreGuarded(t *testing.T) {
	s := open(t)
	const id = "ix_g"
	if err := s.Put(id, interactions.RoleInbound, "did:anet:r", "g", "", nil); err != nil {
		t.Fatal(err)
	}
	ix0, _ := s.Get(id)
	if ix0.State != interactions.StateSubmitted || ix0.StateSeq != 1 || ix0.StateAt == 0 {
		t.Fatalf("new interaction: state %s seq %d at %d", ix0.State, ix0.StateSeq, ix0.StateAt)
	}
	if ch, err := s.SetState(id, interactions.StateWorking); err != nil || !ch {
		t.Fatalf("submitted→working: changed=%v err=%v", ch, err)
	}
	if ch, err := s.SetState(id, interactions.StateWorking); err != nil || ch {
		t.Fatalf("working→working must not count as a change: changed=%v err=%v", ch, err)
	}
	ix1, _ := s.Get(id)
	if ix1.StateSeq != 2 {
		t.Fatalf("state_seq = %d after one change, want 2", ix1.StateSeq)
	}
	if ch, err := s.SetState(id, interactions.StateCanceled); err != nil || !ch {
		t.Fatalf("working→canceled: changed=%v err=%v", ch, err)
	}
	// SetResult after cancel leaves the interaction canceled with no receipt.
	err := s.SetResult(id, []byte("late"), "cid", []byte("RECEIPT"), interactions.VerificationVerified)
	if !errors.Is(err, interactions.ErrTerminal) {
		t.Fatalf("SetResult on canceled: err = %v, want ErrTerminal", err)
	}
	if ch, err := s.SetState(id, interactions.StateCompleted); err != nil || ch {
		t.Fatalf("canceled→completed: changed=%v err=%v", ch, err)
	}
	ix2, _ := s.Get(id)
	if ix2.State != interactions.StateCanceled || len(ix2.Receipt) != 0 || ix2.StateSeq != 3 {
		t.Fatalf("after refused writes: state %s receipt %q seq %d", ix2.State, ix2.Receipt, ix2.StateSeq)
	}
	// An absent row is ErrNotFound, not a silent no-op.
	if _, err := s.SetState("ix_absent", interactions.StateWorking); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatalf("SetState on absent row: %v", err)
	}
	if err := s.SetFailed("ix_absent", nil); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatalf("SetFailed on absent row: %v", err)
	}
}

// Peer key material held on a public interaction is removed when the
// interaction ends (A2A-DESIGN §3.8).
func TestPeerKeysAreDeletedAtTerminal(t *testing.T) {
	s := open(t)
	if err := s.Create(interactions.New{ID: "ix_p", Role: interactions.RoleInbound, PeerAID: "did:anet:r",
		Goal: "net.echo", Trust: interactions.TrustPublicCap, IsCapability: true,
		PeerKEL: []byte("KEL"), PeerKeys: []byte("KEYS")}); err != nil {
		t.Fatal(err)
	}
	ix, _ := s.Get("ix_p")
	if string(ix.PeerKEL) != "KEL" || string(ix.PeerKeys) != "KEYS" || ix.Trust != interactions.TrustPublicCap || !ix.IsCapability {
		t.Fatalf("created row: %+v", ix)
	}
	if err := s.SetResult("ix_p", []byte("{}"), "cid", []byte("R"), interactions.VerificationVerified); err != nil {
		t.Fatal(err)
	}
	ix, _ = s.Get("ix_p")
	if len(ix.PeerKEL) != 0 || len(ix.PeerKeys) != 0 {
		t.Fatalf("peer keys kept after completion: kel %q keys %q", ix.PeerKEL, ix.PeerKeys)
	}
}

// ListPage is newest state change first with a stable cursor (R02 D8: the
// old list returned the oldest N rows).
func TestListPageIsNewestFirstWithCursor(t *testing.T) {
	s := open(t)
	clock := int64(1_000_000)
	s.SetClock(func() int64 { return clock })
	for i := 0; i < 250; i++ {
		clock++
		id := "ix_" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		if err := s.Put(id, interactions.RoleInbound, "did:anet:r", "g", "", nil); err != nil {
			t.Fatal(err)
		}
	}
	// Touch the oldest row: it must move to the head.
	clock++
	if _, err := s.SetState("ix_aa", interactions.StateWorking); err != nil {
		t.Fatal(err)
	}
	var seen []string
	f := interactions.ListFilter{Role: interactions.RoleInbound, Limit: 100}
	for {
		p, err := s.ListPage(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, ix := range p.Items {
			seen = append(seen, ix.ID)
		}
		if p.Next == "" {
			break
		}
		f.Cursor = p.Next
	}
	if len(seen) != 250 {
		t.Fatalf("paged %d rows, want 250", len(seen))
	}
	if seen[0] != "ix_aa" {
		t.Fatalf("head = %s, want the most recently changed ix_aa", seen[0])
	}
	uniq := map[string]bool{}
	for _, id := range seen {
		if uniq[id] {
			t.Fatalf("%s listed twice", id)
		}
		uniq[id] = true
	}
	if n, err := s.Count(interactions.ListFilter{Role: interactions.RoleInbound, Active: true}); err != nil || n != 250 {
		t.Fatalf("count active = %d (%v)", n, err)
	}
	if _, err := s.ListPage(interactions.ListFilter{Cursor: "garbage"}); !errors.Is(err, interactions.ErrBadCursor) {
		t.Fatalf("bad cursor: %v", err)
	}
}

// A store written with the wire-1 status column is migrated to state by the
// rule of A2A-DESIGN §4.1, and the status column is gone afterwards.
func TestStatusColumnMigratesToState(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "interactions.db"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, q := range []string{
		`CREATE TABLE interaction (seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT NOT NULL UNIQUE, role TEXT NOT NULL,
		  peer_aid TEXT NOT NULL, goal TEXT NOT NULL, status TEXT NOT NULL, request_cid TEXT NOT NULL DEFAULT '',
		  request_doc BLOB, result_cid TEXT NOT NULL DEFAULT '', result BLOB, receipt BLOB, review BLOB,
		  end_req_by TEXT NOT NULL DEFAULT '', end_acc_by TEXT NOT NULL DEFAULT '',
		  created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`,
		`CREATE INDEX idx_ix_role_status ON interaction(role, status, seq)`,
		`CREATE TABLE message (seq INTEGER PRIMARY KEY AUTOINCREMENT, interaction_id TEXT NOT NULL,
		  sender_aid TEXT NOT NULL, kind TEXT NOT NULL, body TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	const me, peer = "did:anet:me", "did:anet:peer"
	rows := []struct{ id, role, status string }{
		{"done_in", "inbound", "done"},
		{"failed_out", "outbound", "failed"},
		{"queued_in_req_last", "inbound", "queued"},  // requester (peer) spoke last → working
		{"queued_in_prov_last", "inbound", "ending"}, // provider (me) spoke last → input-required
		{"queued_out_req_last", "outbound", "queued"},
		{"queued_out_prov_last", "outbound", "queued"},
		{"queued_empty", "inbound", "queued"},
	}
	for _, r := range rows {
		if _, err := db.Exec(`INSERT INTO interaction(id,role,peer_aid,goal,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`,
			r.id, r.role, peer, "g", r.status, now, now); err != nil {
			t.Fatal(err)
		}
	}
	msg := func(ix, sender, kind string) {
		if _, err := db.Exec(`INSERT INTO message(interaction_id,sender_aid,kind,body,created_at) VALUES(?,?,?,?,?)`,
			ix, sender, kind, "b", now); err != nil {
			t.Fatal(err)
		}
	}
	msg("queued_in_req_last", me, "text")
	msg("queued_in_req_last", peer, "text")
	msg("queued_in_prov_last", peer, "text")
	msg("queued_in_prov_last", me, "text")
	msg("queued_in_prov_last", peer, "end_request") // not a text message: does not count
	msg("queued_out_req_last", peer, "text")
	msg("queued_out_req_last", me, "text")
	msg("queued_out_prov_last", me, "text")
	msg("queued_out_prov_last", peer, "text")
	db.Close()

	s, err := interactions.Open(dir)
	if err != nil {
		t.Fatalf("open old store: %v", err)
	}
	defer s.Close()
	want := map[string]interactions.State{
		"done_in":              interactions.StateCompleted,
		"failed_out":           interactions.StateFailed,
		"queued_in_req_last":   interactions.StateWorking,
		"queued_in_prov_last":  interactions.StateInputRequired,
		"queued_out_req_last":  interactions.StateWorking,
		"queued_out_prov_last": interactions.StateInputRequired,
		"queued_empty":         interactions.StateSubmitted,
	}
	for id, st := range want {
		ix, err := s.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if ix.State != st {
			t.Errorf("%s: migrated to %s, want %s", id, ix.State, st)
		}
		if ix.StateAt == 0 {
			t.Errorf("%s: state_at not set from updated_at", id)
		}
	}
	db2, _ := sql.Open("sqlite", filepath.Join(dir, "interactions.db"))
	defer db2.Close()
	var n int
	if err := db2.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('interaction') WHERE name='status'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("status column still present (%d, %v)", n, err)
	}
	if err := db2.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='idx_ix_role_status'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("old index still present (%d, %v)", n, err)
	}
	if err := db2.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='idx_ix_role_state'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("state index missing (%d, %v)", n, err)
	}
}

// The approval queue enforces its total and per-peer limits inside the
// transaction, keeps at most the configured number of follow-ups, and an
// item is found by age for expiry.
func TestPendingLimits(t *testing.T) {
	s := open(t)
	put := func(ix, from string, at int64) error {
		return s.Update(func(tx *interactions.Tx) error {
			return tx.PutPending(interactions.PendingItem{IX: ix, FromAID: from, ArrivedAt: at, Delegate: []byte("D")}, 3, 2)
		})
	}
	if err := put("p1", "did:anet:a", 1); err != nil {
		t.Fatal(err)
	}
	if err := put("p2", "did:anet:a", 2); err != nil {
		t.Fatal(err)
	}
	if err := put("p3", "did:anet:a", 3); !errors.Is(err, interactions.ErrPendingFull) {
		t.Fatalf("third item from one peer: %v, want ErrPendingFull", err)
	}
	if err := put("p1", "did:anet:a", 9); err != nil {
		t.Fatalf("re-holding an item already held must be a no-op: %v", err)
	}
	if err := put("p4", "did:anet:b", 4); err != nil {
		t.Fatal(err)
	}
	if err := put("p5", "did:anet:c", 5); !errors.Is(err, interactions.ErrPendingFull) {
		t.Fatalf("fourth item in total: %v, want ErrPendingFull", err)
	}
	for i := 0; i < 2; i++ {
		if err := s.Update(func(tx *interactions.Tx) error {
			return tx.AppendPendingFollowup("p1", interactions.PendingFollowup{Kind: "text", Body: "more", MsgID: string(rune('x' + i))}, 2)
		}); err != nil {
			t.Fatal(err)
		}
	}
	err := s.Update(func(tx *interactions.Tx) error {
		return tx.AppendPendingFollowup("p1", interactions.PendingFollowup{Kind: "text", Body: "one too many", MsgID: "z"}, 2)
	})
	if !errors.Is(err, interactions.ErrPendingFollowupsFull) {
		t.Fatalf("third follow-up: %v", err)
	}
	p, _ := s.GetPending("p1")
	if len(p.Followups) != 2 {
		t.Fatalf("follow-ups = %d", len(p.Followups))
	}
	old, _ := s.ExpiredPending(3)
	if len(old) != 2 || old[0].IX != "p1" || old[1].IX != "p2" {
		t.Fatalf("expired = %v", old)
	}
	if ok, _ := s.DeletePending("p1"); !ok {
		t.Fatal("delete held item")
	}
	if _, err := s.GetPending("p1"); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatalf("deleted item still found: %v", err)
	}
}

// The outbox survives closing and reopening the store: it is the part of
// the retry queue that lives on disk.
func TestOutboxSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := interactions.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.EnqueueOutbox(interactions.OutboxItem{IX: "ix_o", ToAID: "did:anet:r", Type: "anet.result/1",
		Envelope: []byte("ENV"), Exp: 99})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RescheduleOutbox(id, 3, 12345, "hub down"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2, err := interactions.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	due, err := s2.DueOutbox(12345, 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("due after reopen = %v (%v)", due, err)
	}
	if string(due[0].Envelope) != "ENV" || due[0].Attempts != 3 || due[0].Exp != 99 || due[0].LastError != "hub down" {
		t.Fatalf("row after reopen: %+v", due[0])
	}
	if early, _ := s2.DueOutbox(12344, 10); len(early) != 0 {
		t.Fatalf("row due before its time: %v", early)
	}
}
