package daemon

// Red-team PoCs for SI-10: business writes that §3.6 step 10 requires in
// the replay-row transaction but that the implementation makes after it
// commits. A storage failure there (injected as a SQLite trigger that
// aborts exactly that statement, the way SQLITE_FULL / SQLITE_IOERR /
// SQLITE_BUSY would) is logged, the envelope is acknowledged, and every
// redelivery is a duplicate: the effect is lost for good. A passing test
// means the defect is present.

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

// An inbound message's attachments are written after the message and its
// replay row commit (ingestMessage → storeMsgAttachments). A storage error
// on that write is only logged: the envelope is acked, and the redelivered
// envelope is a duplicate, so the file never arrives.
func TestRedteamSI10_AttachmentWriteFailureIsAckedAndTheFileIsLost(t *testing.T) {
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
	env := onlyQueuedEnvelope(t, srv, prov.AID())

	recover := storeFault(t, prov, "rt_att_fault",
		`CREATE TRIGGER rt_att_fault BEFORE INSERT ON attachment BEGIN SELECT RAISE(ABORT, 'disk I/O error (injected)'); END`)
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	recover()

	// Acknowledged although part of the message was not stored.
	if n := len(queuedFor(t, srv, prov.AID())); n != 0 {
		t.Fatalf("the envelope was not acked (%d left); SI-10 held", n)
	}
	// The redelivery (hub replay, p2p copy) is a duplicate and stores nothing.
	if r := receive(t, prov, env); r.reason != dropDuplicate {
		t.Fatalf("redelivery: %+v", r)
	}
	atts, err := prov.ix.Attachments(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(atts) != 0 {
		t.Fatalf("the attachment was stored (%d); attack failed", len(atts))
	}
	// The requester recorded the file as sent.
	if sent, _ := req.ix.Attachments(id); len(sent) != 1 {
		t.Fatalf("requester recorded %d attachments", len(sent))
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
