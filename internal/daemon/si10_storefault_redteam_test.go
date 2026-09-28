package daemon

// Red-team regressions for SI-10 ([redteam:F27], F28 in
// si10_x402_redteam_test.go): business writes that §3.6 step 10 requires in
// the replay-row transaction. A storage failure there (injected as a SQLite
// trigger that aborts exactly that statement, the way SQLITE_FULL /
// SQLITE_IOERR / SQLITE_BUSY would) used to be logged after the commit, the
// envelope acknowledged, and every redelivery a duplicate: the effect lost
// for good. Each test asserts that the failure now rolls the message back,
// leaves it unacknowledged, and that its redelivery stores all of it.

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// storeFault installs a trigger in d's interactions.db that makes one kind
// of write fail, and returns a function that removes it (storage
// recovered).
func storeFault(t *testing.T, d *Daemon, name, ddl string) func() {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(d.layout.InteractionsDir(), "interactions.db")+"?_pragma=busy_timeout(15000)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ddl); err != nil {
		t.Fatalf("install fault: %v", err)
	}
	removed := false
	remove := func() {
		if removed {
			return
		}
		removed = true
		if _, err := db.Exec(`DROP TRIGGER IF EXISTS ` + name); err != nil {
			t.Fatalf("remove fault: %v", err)
		}
		db.Close()
	}
	t.Cleanup(remove)
	return remove
}

// [redteam:F27] regression (was TestRedteamSI10_AttachmentWriteFailureIsAckedAndTheFileIsLost).
// An inbound message's attachments are written in the transaction that
// stores the message and its replay row. A storage error on them rolls the
// whole message back: the envelope is not acknowledged, and once storage
// recovers its redelivery stores the message and the file.
func TestRedteamSI10_AttachmentWriteFailureLeavesTheMessageForRedelivery(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "review the attached file", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "contract.pdf")
	if err := os.WriteFile(path, []byte("%PDF-1.4 the only copy of the contract"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := req.SendMessage(ctx, id, "here it is", []string{path}); err != nil {
		t.Fatal(err)
	}
	before := countMsgs(t, prov, id)

	recover := storeFault(t, prov, "rt_att_fault",
		`CREATE TRIGGER rt_att_fault BEFORE INSERT ON attachment BEGIN SELECT RAISE(ABORT, 'disk I/O error (injected)'); END`)
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	// Not acknowledged, and nothing of it stored.
	if n := len(queuedFor(t, srv, prov.AID())); n != 1 {
		t.Fatalf("%d envelopes left; the message whose file could not be stored was acknowledged", n)
	}
	if got := countMsgs(t, prov, id); got != before {
		t.Fatalf("the message was stored without its file (%d → %d)", before, got)
	}
	recover()

	// The redelivery stores the message and the file (the relay cursor
	// comes back to a held envelope within two rounds).
	for i := 0; i < 2; i++ {
		if err := prov.pollOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(queuedFor(t, srv, prov.AID())); n != 0 {
		t.Fatalf("%d envelopes left after storage recovered", n)
	}
	if got := countMsgs(t, prov, id); got != before+1 {
		t.Fatalf("the redelivered message was not stored (%d → %d)", before, got)
	}
	atts, err := prov.ix.Attachments(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(atts) != 1 || atts[0].Name != "contract.pdf" {
		t.Fatalf("attachments stored: %+v, want the file", atts)
	}
}

// [redteam:F27] The same for a delegation's own files: a storage error on
// them leaves the delegation unrecorded and unacknowledged, and its
// redelivery records the task with the file.
func TestRedteamSI10_ADelegationsFileWriteFailureLeavesItForRedelivery(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "brief.txt")
	if err := os.WriteFile(path, []byte("the brief, one copy only"), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := req.Delegate(ctx, prov.AID(), "work from the brief", []string{path})
	if err != nil {
		t.Fatal(err)
	}
	recover := storeFault(t, prov, "rt_att_fault",
		`CREATE TRIGGER rt_att_fault BEFORE INSERT ON attachment BEGIN SELECT RAISE(ABORT, 'disk I/O error (injected)'); END`)
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(queuedFor(t, srv, prov.AID())); n != 1 {
		t.Fatalf("%d envelopes left; the delegation whose file could not be stored was acknowledged", n)
	}
	if _, err := prov.ix.Get(id); err == nil {
		t.Fatal("the delegation was recorded without its file")
	}
	recover()
	for i := 0; i < 2; i++ {
		if err := prov.pollOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if atts, err := prov.ix.Attachments(id); err != nil || len(atts) != 1 {
		t.Fatalf("attachments after redelivery: %+v (%v), want the brief", atts, err)
	}
}

// queuedAt is queuedFor by hub URL.
func queuedAt(t *testing.T, url, toAID string) [][]byte {
	t.Helper()
	h := fakeHubAt(t, url)
	h.mu.Lock()
	defer h.mu.Unlock()
	var out [][]byte
	for _, m := range h.mailbox {
		if m.toAID == toAID {
			out = append(out, append([]byte(nil), m.payload...))
		}
	}
	return out
}
