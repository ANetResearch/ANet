package interactions_test

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// The outbound queue as the engine reads and writes it (engine §2.1,
// §3.4): one row per message, a version per sealing, the rules of which
// versions stay.

func sealVer(t *testing.T, s *interactions.Store, id int64, ack string) uint64 {
	t.Helper()
	var ver uint64
	if err := s.Update(func(tx *interactions.Tx) error {
		var err error
		ver, _, err = tx.SealOutboxVersion(id, interactions.OutboxSeal{AckKey: []byte(ack), Envelope: []byte("env-" + ack)})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return ver
}

func endAttempt(t *testing.T, s *interactions.Store, id int64, e interactions.OutboxEnd) {
	t.Helper()
	if err := s.Update(func(tx *interactions.Tx) error { return tx.EndOutboxAttempt(id, e) }); err != nil {
		t.Fatal(err)
	}
}

func obState(t *testing.T, s *interactions.Store, id int64) (*interactions.OutboxRow, []interactions.OutboxVer) {
	t.Helper()
	row, vers, err := s.OutboxState(id)
	if err != nil {
		t.Fatal(err)
	}
	return row, vers
}

func verNumbers(vers []interactions.OutboxVer) []uint64 {
	var out []uint64
	for _, v := range vers {
		out = append(out, v.Ver)
	}
	return out
}

// The same message queued twice is one row (engine ENG-Q4), and the dedupe
// key is an HMAC under the task's key, not a digest of the content anyone
// can compute (FS-8 handover).
func TestEnqueueOutboxDedupes(t *testing.T) {
	dir := t.TempDir()
	s, err := interactions.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	it := interactions.OutboxItem{IX: "ix", ToAID: "peer", Type: "anet.message/1", Kind: "text", Body: []byte("hello")}
	a := enqueueOutbox(t, s, it)
	var fresh bool
	fillOutboxItem(&it)
	if err := s.Update(func(tx *interactions.Tx) error {
		id, f, err := tx.EnqueueOutbox(it)
		if id != a {
			return fmt.Errorf("the same message queued again is row %d, not %d", id, a)
		}
		fresh = f
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if fresh {
		t.Fatal("the same message again was reported as a new row")
	}
	for _, other := range []interactions.OutboxItem{
		{IX: "ix", ToAID: "peer", Type: "anet.message/1", Kind: "text", Body: []byte("hello again")},
		{IX: "ix", ToAID: "peer", Type: "anet.message/1", Kind: "cancel", Body: []byte("hello")},
		{IX: "ix", ToAID: "other", Type: "anet.message/1", Kind: "text", Body: []byte("hello")},
	} {
		if b := enqueueOutbox(t, s, other); b == a {
			t.Fatalf("%+v was taken for the queued row", other)
		}
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "interactions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var digest []byte
	if err := db.QueryRow(`SELECT digest FROM outbox WHERE id=?`, a).Scan(&digest); err != nil {
		t.Fatal(err)
	}
	plain := sha256.Sum256([]byte("hello"))
	if len(digest) != sha256.Size || bytes.Equal(digest, plain[:]) {
		t.Fatalf("the digest is %x: not an HMAC under the task's key", digest)
	}
}

func TestEnqueueOutboxRefusesAnIncompleteRow(t *testing.T) {
	s := open(t)
	full := interactions.OutboxItem{IX: "ix", ToAID: "peer", Type: "anet.result/1", Body: []byte("b"),
		MID: []byte("0123456789abcdef"), TS: 1, Exp: 2}
	for name, mut := range map[string]func(*interactions.OutboxItem){
		"no ix":   func(it *interactions.OutboxItem) { it.IX = "" },
		"no to":   func(it *interactions.OutboxItem) { it.ToAID = "" },
		"no type": func(it *interactions.OutboxItem) { it.Type = "" },
		"no mid":  func(it *interactions.OutboxItem) { it.MID = nil },
		"no body": func(it *interactions.OutboxItem) { it.Body = nil },
		"no exp":  func(it *interactions.OutboxItem) { it.Exp = 0 },
	} {
		it := full
		mut(&it)
		err := s.Update(func(tx *interactions.Tx) error { _, _, err := tx.EnqueueOutbox(it); return err })
		if !errors.Is(err, interactions.ErrBadInput) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A version replaced before it was handed over goes with the transaction
// that replaces it; one handed over stays (engine ENG-S5, ENG-R10).
func TestANewVersionReplacesOnlyOneNeverHanded(t *testing.T) {
	s := open(t)
	id := enqueueOutbox(t, s, interactions.OutboxItem{IX: "ix", ToAID: "peer", Type: "anet.result/1", Body: []byte("b")})
	if v := sealVer(t, s, id, "k1"); v != 1 {
		t.Fatalf("first version %d", v)
	}
	row, vers := obState(t, s, id)
	if !row.Attempting || row.CurVer != 1 || len(vers) != 1 {
		t.Fatalf("after A8: %+v %+v", row, vers)
	}
	endAttempt(t, s, id, interactions.OutboxEnd{At: 5, Attempts: 1})
	if v := sealVer(t, s, id, "k2"); v != 2 {
		t.Fatalf("second version %d", v)
	}
	if _, vers := obState(t, s, id); !slices.Equal(verNumbers(vers), []uint64{2}) {
		t.Fatalf("v1 was never handed over and should be gone: %v", verNumbers(vers))
	}
	endAttempt(t, s, id, interactions.OutboxEnd{At: 6, Handed: []uint64{2}})
	sealVer(t, s, id, "k3")
	row, vers = obState(t, s, id)
	if !slices.Equal(verNumbers(vers), []uint64{2, 3}) || row.CurVer != 3 || row.HandedAt != 6 {
		t.Fatalf("after v3: %+v %v", row, verNumbers(vers))
	}
	if _, env, err := s.OutboxContent(id); err != nil || string(env) != "env-k3" {
		t.Fatalf("the envelope is the current version's: %q %v", env, err)
	}
}

// A row keeps at most MaxHandedVersions handed versions; the earliest goes
// and the current one never does (engine ENG-R9, E-T18).
func TestTheVersionCapKeepsTheCurrentOne(t *testing.T) {
	s := open(t)
	id := enqueueOutbox(t, s, interactions.OutboxItem{IX: "ix", ToAID: "peer", Type: "anet.result/1", Body: []byte("b")})
	evictedAll := int64(0)
	for i := 1; i <= 70; i++ {
		if err := s.Update(func(tx *interactions.Tx) error {
			v, ev, err := tx.SealOutboxVersion(id, interactions.OutboxSeal{AckKey: []byte(fmt.Sprintf("k%d", i)), Envelope: []byte("e")})
			evictedAll += ev
			if err != nil {
				return err
			}
			return tx.EndOutboxAttempt(id, interactions.OutboxEnd{At: int64(i), Handed: []uint64{v}})
		}); err != nil {
			t.Fatal(err)
		}
	}
	row, vers := obState(t, s, id)
	if len(vers) != interactions.MaxHandedVersions || vers[len(vers)-1].Ver != row.CurVer || row.CurVer != 70 {
		t.Fatalf("%d versions, current %d, last kept %d", len(vers), row.CurVer, vers[len(vers)-1].Ver)
	}
	if vers[0].Ver != 70-interactions.MaxHandedVersions+1 || evictedAll != 70-interactions.MaxHandedVersions {
		t.Fatalf("the earliest kept is %d, %d evicted", vers[0].Ver, evictedAll)
	}
}

// The end of an attempt records the hand-over once and the hub's answer on
// its version; a status answer is recorded on the version it is about.
func TestAttemptAndStatusAreRecordedOnTheirVersions(t *testing.T) {
	s := open(t)
	id := enqueueOutbox(t, s, interactions.OutboxItem{IX: "ix", ToAID: "peer", Type: "anet.delegate/1", Body: []byte("b")})
	sealVer(t, s, id, "k1")
	endAttempt(t, s, id, interactions.OutboxEnd{At: 10, Handed: []uint64{1},
		Accepted: &interactions.OutboxAccepted{Ver: 1, RID: []byte("rid-0123456789ab"), Hub: "https://hub", ViaHub: "did:hub:b"},
		CheckAt:  12, LastError: ""})
	if err := s.Update(func(tx *interactions.Tx) error { return tx.BeginOutboxAttempt(id) }); err != nil {
		t.Fatal(err)
	}
	endAttempt(t, s, id, interactions.OutboxEnd{At: 20, Handed: []uint64{1}, Attempts: 1, NextAt: 30, CheckAt: 12})
	row, vers := obState(t, s, id)
	if row.HandedAt != 10 || vers[0].HandedAt != 10 || string(vers[0].RID) != "rid-0123456789ab" ||
		vers[0].Hub != "https://hub" || vers[0].ViaHub != "did:hub:b" || row.CheckAt != 12 || row.Attempting {
		t.Fatalf("after two attempts: %+v %+v", row, vers)
	}
	if err := s.Update(func(tx *interactions.Tx) error {
		return tx.NoteOutboxCheck(id, interactions.OutboxCheck{States: map[uint64]string{1: interactions.OutboxQueued},
			CheckAt: 40, CheckN: 1})
	}); err != nil {
		t.Fatal(err)
	}
	row, vers = obState(t, s, id)
	if vers[0].HubState != interactions.OutboxQueued || row.CheckAt != 40 || row.CheckN != 1 || row.Attempts != 1 {
		t.Fatalf("after the status answer: %+v %+v", row, vers)
	}
	if err := s.Update(func(tx *interactions.Tx) error {
		return tx.NoteOutboxCheck(id, interactions.OutboxCheck{States: map[uint64]string{1: "done"}})
	}); !errors.Is(err, interactions.ErrBadInput) {
		t.Fatalf("a state outside the two: %v", err)
	}
}

// A row goes with every version of it, and a version's ack_key does not
// stay in the database files (engine ENG-S6, E-T5).
func TestARowGoesWithItsVersionsAndTheirKeys(t *testing.T) {
	dir := t.TempDir()
	s, err := interactions.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := range 20 {
		enqueueOutbox(t, s, interactions.OutboxItem{IX: fmt.Sprintf("ix_%d", i), ToAID: "peer", Type: "anet.result/1", Body: []byte("b")})
	}
	id := enqueueOutbox(t, s, interactions.OutboxItem{IX: "ix", ToAID: "peer", Type: "anet.result/1", Body: []byte("b")})
	keys := [][]byte{[]byte("ack-key-one-canary-0123456789abc"), []byte("ack-key-two-canary-0123456789abc")}
	for _, k := range keys {
		if err := s.Update(func(tx *interactions.Tx) error {
			v, _, err := tx.SealOutboxVersion(id, interactions.OutboxSeal{AckKey: k, Envelope: []byte("e")})
			if err != nil {
				return err
			}
			return tx.EndOutboxAttempt(id, interactions.OutboxEnd{At: 1, Handed: []uint64{v}})
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CheckpointTruncate(); err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if len(dbHits(t, dir, k)) == 0 {
			t.Fatal("the ack_key is not in the database file: the test would prove nothing")
		}
	}
	if err := s.Update(func(tx *interactions.Tx) error {
		ok, err := tx.DeleteOutboxRow(id)
		if err == nil && !ok {
			err = errors.New("the row was not there")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckpointTruncate(); err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if hits := dbHits(t, dir, k); len(hits) != 0 {
			t.Fatalf("a deleted ack_key is still in %v", hits)
		}
	}
	if _, _, err := s.OutboxState(id); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatalf("the row: %v", err)
	}
	if err := s.Update(func(tx *interactions.Tx) error {
		vers, err := tx.OutboxVersions(id)
		if err == nil && len(vers) != 0 {
			err = fmt.Errorf("%d versions left", len(vers))
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(tx *interactions.Tx) error {
		ok, err := tx.DeleteOutboxRow(id)
		if err == nil && ok {
			err = errors.New("a row deleted twice")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// [redteam:F23] rewritten for handed order (engine ENG-O1, O2): a row is
// due only once every row queued before it for its task and peer was
// handed over — one an attempt is sending now holds the rows behind it —
// and a text or end_request only once its delegation is gone (confirmed).
// A cancel waits for rule 1 alone.
func TestOutboxDeliversATasksMessagesInOrder(t *testing.T) {
	s := open(t)
	q := func(ix, typ, kind, body string) int64 {
		return enqueueOutbox(t, s, interactions.OutboxItem{IX: ix, ToAID: "peer", Type: typ, Kind: kind, Body: []byte(body)})
	}
	del := q("ix", "anet.delegate/1", "", "d")
	text := q("ix", "anet.message/1", "text", "t")
	other := q("ix2", "anet.result/1", "", "r")
	due := func() []int64 {
		t.Helper()
		ids, err := s.DueOutboxIDs(1<<62, 100)
		if err != nil {
			t.Fatal(err)
		}
		slices.Sort(ids)
		return ids
	}
	if got := due(); !slices.Equal(got, []int64{del, other}) {
		t.Fatalf("due = %v, want the delegation and the other task's row", got)
	}
	sealVer(t, s, del, "k")
	endAttempt(t, s, del, interactions.OutboxEnd{At: 1, Handed: []uint64{1}, NextAt: 1})
	if got := due(); !slices.Equal(got, []int64{del, other}) {
		t.Fatalf("after the delegation left: %v (the text waits for its confirmation)", got)
	}
	// A cancel takes the follow-ups that never left with it, and goes
	// behind the delegation that did (ENG-O4).
	if err := s.Update(func(tx *interactions.Tx) error { _, err := tx.DeleteOutboxFollowUps("ix", "peer"); return err }); err != nil {
		t.Fatal(err)
	}
	cancel := q("ix", "anet.message/1", "cancel", "c")
	if got := due(); !slices.Equal(got, []int64{del, other, cancel}) {
		t.Fatalf("after the cancel: %v", got)
	}
	text2 := q("ix", "anet.message/1", "text", "t2")
	if err := s.Update(func(tx *interactions.Tx) error { _, err := tx.DeleteOutboxRow(del); return err }); err != nil {
		t.Fatal(err)
	}
	if got := due(); !slices.Equal(got, []int64{other, cancel}) {
		t.Fatalf("after the confirmation: %v (the text waits behind the cancel)", got)
	}
	_ = text
	_ = text2
	// Rule 1 against an attempt under way: the row behind waits for it.
	first := q("ix3", "anet.result/1", "", "r1")
	second := q("ix3", "anet.status/1", "working", "s2")
	sealVer(t, s, first, "k3") // attempting, nothing handed yet
	if got := due(); slices.Contains(got, second) {
		t.Fatalf("due = %v while the row before it is being attempted", got)
	}
	endAttempt(t, s, first, interactions.OutboxEnd{At: 2, Handed: []uint64{1}, NextAt: 2})
	if got := due(); !slices.Contains(got, second) {
		t.Fatalf("due = %v once the row before it was handed over", got)
	}
}

// A row is due when its backoff, its next status query or its deadline has
// passed.
func TestDueRowsByTheirTimes(t *testing.T) {
	s := open(t)
	id := enqueueOutbox(t, s, interactions.OutboxItem{IX: "ix", ToAID: "peer", Type: "anet.result/1", Body: []byte("b"), TS: 1, Exp: 1000})
	sealVer(t, s, id, "k")
	endAttempt(t, s, id, interactions.OutboxEnd{At: 1, Handed: []uint64{1}, NextAt: 500, CheckAt: 300})
	for _, c := range []struct {
		now int64
		due bool
	}{{299, false}, {300, true}, {499, true}} {
		ids, _ := s.DueOutboxIDs(c.now, 10)
		if (len(ids) == 1) != c.due {
			t.Fatalf("at %d: %v", c.now, ids)
		}
	}
	endAttempt(t, s, id, interactions.OutboxEnd{At: 1, NextAt: 2000})
	if ids, _ := s.DueOutboxIDs(1000, 10); len(ids) != 0 {
		t.Fatalf("at the deadline: %v", ids)
	}
	if ids, _ := s.DueOutboxIDs(1001, 10); len(ids) != 1 {
		t.Fatalf("past the deadline: %v", ids)
	}
	if at, ok, err := s.NextOutboxAt(); err != nil || !ok || at != 1001 {
		t.Fatalf("next due %d %v %v", at, ok, err)
	}
}

// A row found attempting at start counts as handed over: the process that
// stopped may have sent it (engine §6.2 CR2).
func TestAnAttemptCutShortCountsAsHanded(t *testing.T) {
	s := open(t)
	never := enqueueOutbox(t, s, interactions.OutboxItem{IX: "ix", ToAID: "peer", Type: "anet.result/1", Body: []byte("a")})
	cut := enqueueOutbox(t, s, interactions.OutboxItem{IX: "ix2", ToAID: "peer", Type: "anet.result/1", Body: []byte("b")})
	sealVer(t, s, cut, "k")
	if ids, _ := s.AttemptingOutboxIDs(); !slices.Equal(ids, []int64{cut}) {
		t.Fatalf("attempting: %v", ids)
	}
	if err := s.Update(func(tx *interactions.Tx) error {
		n, err := tx.RecoverOutboxAttempts(77)
		if err == nil && n != 1 {
			err = fmt.Errorf("%d recovered", n)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	row, vers := obState(t, s, cut)
	if row.Attempting || row.HandedAt != 77 || vers[0].HandedAt != 77 {
		t.Fatalf("recovered: %+v %+v", row, vers)
	}
	if row, _ := obState(t, s, never); row.HandedAt != 0 {
		t.Fatalf("a row never attempted is not handed: %+v", row)
	}
}

// Freezing and the deletions a cancel and an abandonment make (engine
// ENG-O4, ENG-X3).
func TestFreezeAndTheQueueDeletions(t *testing.T) {
	s := open(t)
	q := func(typ, kind, body string) int64 {
		return enqueueOutbox(t, s, interactions.OutboxItem{IX: "ix", ToAID: "peer", Type: typ, Kind: kind, Body: []byte(body)})
	}
	del := q("anet.delegate/1", "", "d")
	text := q("anet.message/1", "text", "t")
	end := q("anet.message/1", "end_request", "e")
	cancel := q("anet.message/1", "cancel", "c")
	sealVer(t, s, del, "k")
	endAttempt(t, s, del, interactions.OutboxEnd{At: 1, Handed: []uint64{1}})
	if err := s.Update(func(tx *interactions.Tx) error {
		ok, err := tx.FreezeOutboxDelegation("ix", "peer")
		if err == nil && !ok {
			err = errors.New("no delegation frozen")
		}
		if err != nil {
			return err
		}
		n, err := tx.DeleteOutboxFollowUps("ix", "peer")
		if err == nil && n != 2 {
			err = fmt.Errorf("%d follow-ups deleted", n)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.OutboxQueue("ix", "peer")
	if len(rows) != 2 || rows[0].ID != del || !rows[0].Frozen || rows[1].ID != cancel {
		t.Fatalf("after the cancel: %+v", rows)
	}
	for _, id := range []int64{text, end} {
		if _, _, err := s.OutboxState(id); !errors.Is(err, interactions.ErrNotFound) {
			t.Fatalf("follow-up %d: %v", id, err)
		}
	}
	if err := s.Update(func(tx *interactions.Tx) error {
		n, err := tx.DeleteOutboxQueue("ix", "peer")
		if err == nil && n != 2 {
			err = fmt.Errorf("%d deleted", n)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.OutboxLen(); n != 0 {
		t.Fatalf("%d rows left", n)
	}
}

// The no-response scan pages through handed delegations, earliest first,
// and leaves out frozen ones (engine ENG-T1, ENG-T4).
func TestHandedDelegationsPage(t *testing.T) {
	s := open(t)
	var ids []int64
	for i := range 5 {
		id := enqueueOutbox(t, s, interactions.OutboxItem{IX: fmt.Sprintf("ix_%d", i), ToAID: "peer", Type: "anet.delegate/1", Body: []byte("d")})
		sealVer(t, s, id, "k")
		endAttempt(t, s, id, interactions.OutboxEnd{At: int64(100 - i), Handed: []uint64{1}})
		ids = append(ids, id)
	}
	if err := s.Update(func(tx *interactions.Tx) error { _, err := tx.FreezeOutboxDelegation("ix_1", "peer"); return err }); err != nil {
		t.Fatal(err)
	}
	var got []int64
	for cur := interactions.HandedDelegationsFirst; ; {
		page, err := s.HandedDelegations(98, cur, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		got = append(got, page[0].ID)
		cur = page[0]
	}
	if !slices.Equal(got, []int64{ids[4], ids[3], ids[2]}) {
		t.Fatalf("pages: %v", got)
	}
}

// The outbox has engine §2.1's columns and no others: no access column, the
// credential is made for every attempt (ENG-S1, ENG-I2).
func TestOutboxColumnsAreTheEngines(t *testing.T) {
	dir := t.TempDir()
	s, err := interactions.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	db, err := sql.Open("sqlite", filepath.Join(dir, "interactions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cols := func(table string) []string {
		rows, err := db.Query(`SELECT name FROM pragma_table_info(?) ORDER BY name`, table)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				t.Fatal(err)
			}
			out = append(out, n)
		}
		return out
	}
	want := []string{"attempting", "attempts", "body", "check_at", "check_n", "created_at", "cur_ver", "digest", "envelope",
		"exp", "frozen", "handed_at", "id", "ix", "kind", "last_error", "mid", "next_at", "sealed_kid", "sealed_kid_na",
		"sealed_kss", "sealed_notice_at", "to_aid", "ts", "typ"}
	if got := cols("outbox"); !slices.Equal(got, want) {
		t.Fatalf("outbox columns %v,\nwant %v", got, want)
	}
	if got := cols("outbox_ver"); !slices.Equal(got, []string{"ack_key", "handed_at", "hub", "hub_state", "rid", "row_id", "ver", "via_hub"}) {
		t.Fatalf("outbox_ver columns %v", got)
	}
}
