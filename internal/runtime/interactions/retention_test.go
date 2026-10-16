package interactions_test

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

const dayMS = int64(24 * time.Hour / time.Millisecond)

// at is a UTC time in unix ms.
func at(y int, m time.Month, d, h, min int) int64 {
	return time.Date(y, m, d, h, min, 0, 0, time.UTC).UnixMilli()
}

func mustRetention(t testing.TB, s string) interactions.Retention {
	t.Helper()
	r, ok := interactions.ParseRetention(s)
	if !ok {
		t.Fatalf("ParseRetention(%q) failed", s)
	}
	return r
}

// The setting is "<N>d" with N from 1 to 3650, or "forever"; nothing else is one.
func TestParseRetention(t *testing.T) {
	for _, s := range []string{"1d", "7d", "30d", "365d", "3650d", "forever"} {
		r, ok := interactions.ParseRetention(s)
		if !ok || !r.Valid() || r.String() != s {
			t.Errorf("%q: %v %v %q", s, ok, r.Valid(), r.String())
		}
	}
	for _, s := range []string{"", "0d", "3651d", "30", "d", "30D", " 30d", "30d ", "+30d", "-1d", "030d", "1.5d",
		"30days", "Forever", "forever ", "never", "1e3d", "99999d", "٣٠d", "3０d"} {
		if r, ok := interactions.ParseRetention(s); ok || r.Valid() {
			t.Errorf("%q was taken as %q", s, r.String())
		}
	}
	if (interactions.Retention{}).Valid() {
		t.Error("the zero Retention is a setting")
	}
	if r, ok := interactions.RetentionDays(30); !ok || r != interactions.DefaultRetention() || r.String() != "30d" {
		t.Errorf("the default is %q", r.String())
	}
}

// TestRetentionRules (FS-L5): a non-public task goes content_retention after it ended, a public one
// seven days after, or content_retention when that is shorter; forever keeps non-public tasks and not
// public ones; the sweep at now deletes what ended before the start of now's UTC day less the
// retention, and anet.content_expires_at is the first moment that holds.
func TestRetentionRules(t *testing.T) {
	now := at(2026, 9, 27, 15, 0)
	for _, tc := range []struct {
		setting, class string
		cutoff         int64 // 0: kept for ever
	}{
		{"30d", interactions.ClassOutbound, at(2026, 8, 28, 0, 0)},
		{"30d", interactions.ClassPeer, at(2026, 8, 28, 0, 0)},
		{"30d", interactions.ClassApproved, at(2026, 8, 28, 0, 0)},
		{"30d", interactions.ClassPublic, at(2026, 9, 20, 0, 0)},
		{"30d", interactions.ClassPublicCap, at(2026, 9, 20, 0, 0)},
		{"3d", interactions.ClassPeer, at(2026, 9, 24, 0, 0)},
		{"3d", interactions.ClassPublicCap, at(2026, 9, 24, 0, 0)},
		{"7d", interactions.ClassPublic, at(2026, 9, 20, 0, 0)},
		{"8d", interactions.ClassPublic, at(2026, 9, 20, 0, 0)},
		{"8d", interactions.ClassOutbound, at(2026, 9, 19, 0, 0)},
		{"forever", interactions.ClassOutbound, 0},
		{"forever", interactions.ClassPeer, 0},
		{"forever", interactions.ClassApproved, 0},
		{"forever", interactions.ClassPublic, at(2026, 9, 20, 0, 0)},
		{"forever", interactions.ClassPublicCap, at(2026, 9, 20, 0, 0)},
		{"3650d", interactions.ClassPeer, at(2026, 9, 27, 0, 0) - 3650*dayMS},
	} {
		name := tc.setting + "/" + tc.class
		r := mustRetention(t, tc.setting)
		cutoff, ok := interactions.RetentionCutoff(now, tc.class, r)
		if ok != (tc.cutoff != 0) || cutoff != tc.cutoff {
			t.Errorf("%s: cutoff %s %v, want %s", name, time.UnixMilli(cutoff).UTC(), ok, time.UnixMilli(tc.cutoff).UTC())
			continue
		}
		del := func(stateAt int64) bool {
			return interactions.ContentDeletable(interactions.ContentFacts{Class: tc.class,
				State: interactions.StateCompleted, StateAt: stateAt}, now, interactions.ClockFull, r, false, false)
		}
		if tc.cutoff == 0 {
			if del(0) || del(now-100*365*dayMS) {
				t.Errorf("%s: a task kept for ever is deletable", name)
			}
			if _, ok := interactions.ContentExpiresAt(now, tc.class, r); ok {
				t.Errorf("%s: a task kept for ever has an expiry", name)
			}
			continue
		}
		if !del(tc.cutoff-1) || del(tc.cutoff) {
			t.Errorf("%s: deletable just before the cutoff %v, at it %v", name, del(tc.cutoff-1), del(tc.cutoff))
		}
		// anet.content_expires_at of a task that ended at the cutoff less 1 ms is at most now, and of
		// one that ended at the cutoff, later than now.
		if exp, ok := interactions.ContentExpiresAt(tc.cutoff-1, tc.class, r); !ok || exp > now {
			t.Errorf("%s: expiry of a deletable task %s", name, time.UnixMilli(exp).UTC())
		}
		if exp, _ := interactions.ContentExpiresAt(tc.cutoff, tc.class, r); exp <= now {
			t.Errorf("%s: expiry of a kept task %s", name, time.UnixMilli(exp).UTC())
		}
	}
	// FS-L5's formula: floor_UTC_day(state_at + R) + 1 day.
	if exp, _ := interactions.ContentExpiresAt(at(2026, 8, 27, 10, 0), interactions.ClassPeer, mustRetention(t, "30d")); exp != at(2026, 9, 27, 0, 0) {
		t.Errorf("content_expires_at %s, want 2026-09-27T00:00Z", time.UnixMilli(exp).UTC())
	}
	if exp, _ := interactions.ContentExpiresAt(at(2026, 9, 1, 0, 0), interactions.ClassPublicCap, interactions.RetentionForever); exp != at(2026, 9, 9, 0, 0) {
		t.Errorf("public content_expires_at under forever %s, want 2026-09-09T00:00Z", time.UnixMilli(exp).UTC())
	}
	// An open task is never deleted, however old; nor at the closed gate; nor while a row is queued or a
	// payment is not settled (FS-L6, FS-L7).
	old := interactions.ContentFacts{Class: interactions.ClassPeer, State: interactions.StateCompleted, StateAt: 0}
	r := mustRetention(t, "1d")
	for _, st := range []interactions.State{interactions.StateSubmitted, interactions.StateWorking, interactions.StateInputRequired} {
		open := old
		open.State = st
		if interactions.ContentDeletable(open, now, interactions.ClockFull, r, false, false) {
			t.Errorf("an open (%s) task is deletable", st)
		}
	}
	for _, st := range []interactions.State{interactions.StateCompleted, interactions.StateFailed, interactions.StateCanceled, interactions.StateRejected} {
		done := old
		done.State = st
		if !interactions.ContentDeletable(done, now, interactions.ClockFull, r, false, false) {
			t.Errorf("an old %s task is kept", st)
		}
	}
	if interactions.ContentDeletable(old, now, interactions.ClockClosed, r, false, false) {
		t.Error("deletable at the closed gate")
	}
	if !interactions.ContentDeletable(old, now, interactions.ClockLocal, r, false, false) {
		t.Error("kept at the local gate")
	}
	if interactions.ContentDeletable(old, now, interactions.ClockFull, r, true, false) {
		t.Error("deletable with a row queued")
	}
	if interactions.ContentDeletable(old, now, interactions.ClockFull, r, false, true) {
		t.Error("deletable with a payment not settled")
	}
	if interactions.ContentDeletable(old, now, interactions.ClockFull, interactions.Retention{}, false, false) {
		t.Error("deletable under no setting")
	}
	// The class of a task: what this node sent, and how it took the others.
	for _, tc := range []struct {
		role        interactions.Role
		trust, want string
	}{
		{interactions.RoleOutbound, "", interactions.ClassOutbound},
		{interactions.RoleOutbound, interactions.TrustPublic, interactions.ClassOutbound},
		{interactions.RoleInbound, interactions.TrustPeer, interactions.ClassPeer},
		{interactions.RoleInbound, interactions.TrustApproved, interactions.ClassApproved},
		{interactions.RoleInbound, interactions.TrustPublic, interactions.ClassPublic},
		{interactions.RoleInbound, interactions.TrustPublicCap, interactions.ClassPublicCap},
		{interactions.RoleInbound, "", interactions.ClassPeer},
	} {
		if got := interactions.ContentClass(tc.role, tc.trust); got != tc.want {
			t.Errorf("class of %s/%q = %s, want %s", tc.role, tc.trust, got, tc.want)
		}
	}
}

// P-7 (FS §7.2): contentDeletable is false at the closed gate, with a row queued or a payment not
// settled, and for a non-public task under forever; a public task is deletable once eight days have
// passed since it ended (full or local gate, no exception); the answer only goes from false to true as
// now grows; full and local agree; and, with no exception, it is exactly "now has reached
// anet.content_expires_at".
func TestContentDeletableProperties(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 9))
	settings := []interactions.Retention{interactions.RetentionForever}
	for _, n := range []int{1, 2, 6, 7, 8, 30, 365, 3650} {
		r, _ := interactions.RetentionDays(n)
		settings = append(settings, r)
	}
	states := []interactions.State{interactions.StateSubmitted, interactions.StateWorking, interactions.StateInputRequired,
		interactions.StateCompleted, interactions.StateFailed, interactions.StateCanceled, interactions.StateRejected}
	levels := []interactions.ClockLevel{interactions.ClockClosed, interactions.ClockLocal, interactions.ClockFull}
	base := at(2026, 1, 1, 0, 0)
	for i := 0; i < 200_000; i++ {
		ix := interactions.ContentFacts{
			Class:   interactions.ContentClasses()[rng.IntN(5)],
			State:   states[rng.IntN(len(states))],
			StateAt: base + rng.Int64N(4000*dayMS),
		}
		r := settings[rng.IntN(len(settings))]
		level := levels[rng.IntN(3)]
		queued, unpaid := rng.IntN(4) == 0, rng.IntN(4) == 0
		now := ix.StateAt + rng.Int64N(4000*dayMS) - 2*dayMS
		got := interactions.ContentDeletable(ix, now, level, r, queued, unpaid)
		public := interactions.PublicClass(ix.Class)
		switch {
		case got && level == interactions.ClockClosed:
			t.Fatalf("%+v: deletable at the closed gate", ix)
		case got && (queued || unpaid):
			t.Fatalf("%+v: deletable with a row queued (%v) or a payment not settled (%v)", ix, queued, unpaid)
		case got && !public && r.Forever():
			t.Fatalf("%+v: a non-public task deletable under forever", ix)
		case got && !ix.State.IsTerminal():
			t.Fatalf("%+v: an open task deletable", ix)
		case !got && public && ix.State.IsTerminal() && level != interactions.ClockClosed && !queued && !unpaid &&
			now >= ix.StateAt+8*dayMS:
			t.Fatalf("%+v under %s at %d: a public task kept eight days after it ended", ix, r, now-ix.StateAt)
		}
		if level != interactions.ClockClosed {
			other := interactions.ClockFull + interactions.ClockLocal - level
			if interactions.ContentDeletable(ix, now, other, r, queued, unpaid) != got {
				t.Fatalf("%+v: full and local disagree", ix)
			}
		}
		later := now + rng.Int64N(400*dayMS)
		if got && !interactions.ContentDeletable(ix, later, level, r, queued, unpaid) {
			t.Fatalf("%+v under %s: deletable at %d, kept at %d", ix, r, now, later)
		}
		if level != interactions.ClockClosed && !queued && !unpaid && ix.State.IsTerminal() {
			exp, ok := interactions.ContentExpiresAt(ix.StateAt, ix.Class, r)
			if want := ok && now >= exp; got != want {
				t.Fatalf("%+v under %s at %d: deletable %v, content_expires_at %d (%v)", ix, r, now, got, exp, ok)
			}
		}
	}
}

// task records a finished (or open) task of a class that ended at endedAt, with one message carrying
// one attachment.
func task(t *testing.T, s *interactions.Store, id, class string, endedAt int64, finish bool) {
	t.Helper()
	role, trust := interactions.RoleInbound, class
	if class == interactions.ClassOutbound {
		role, trust = interactions.RoleOutbound, ""
	}
	s.SetClock(func() int64 { return endedAt })
	defer s.SetClock(nil)
	if err := s.Create(interactions.New{ID: id, Role: role, PeerAID: "did:anet:peer", Goal: "the goal of " + id,
		Trust: trust, PeerKEL: []byte("kel"), PeerKeys: []byte("keys")}); err != nil {
		t.Fatal(err)
	}
	seq, err := s.AddMessage(id, "did:anet:peer", interactions.MsgText, "a message of "+id)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddAttachment(id, seq, interactions.Attachment{Name: "a.txt", Size: 1, CID: "c", Data: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	if !finish {
		return
	}
	cause := interactions.CauseProviderEnd
	if role == interactions.RoleOutbound {
		cause = interactions.CausePeerEnded
	}
	if _, err := s.SetState(id, interactions.StateCompleted, cause); err != nil {
		t.Fatal(err)
	}
}

func present(t *testing.T, s *interactions.Store, id string) bool {
	t.Helper()
	_, err := s.Get(id)
	if err != nil && !errors.Is(err, interactions.ErrNotFound) {
		t.Fatal(err)
	}
	return err == nil
}

// The sweep deletes every class past its retention, with the messages and attachments, and keeps a
// newer task, an open one, one whose row is still queued and one whose payment is submitted and not
// settled; it reports by class. A second sweep finds nothing; once the row is gone and the payment
// settled, the next sweep takes those two.
func TestPruneTerminalSweepsEveryClass(t *testing.T) {
	s := open(t)
	now := at(2026, 9, 27, 15, 0)
	r := mustRetention(t, "30d")
	old, recent := at(2026, 8, 1, 12, 0), at(2026, 9, 26, 12, 0)
	for _, c := range interactions.ContentClasses() {
		task(t, s, "old_"+c, c, old, true)
		task(t, s, "recent_"+c, c, recent, true)
		task(t, s, "open_"+c, c, old, false)
	}
	// Public tasks go after seven days: one that ended ten days ago is gone, one of three days is not.
	task(t, s, "week_"+interactions.ClassPublicCap, interactions.ClassPublicCap, now-10*dayMS, true)
	task(t, s, "days_"+interactions.ClassPublicCap, interactions.ClassPublicCap, now-3*dayMS, true)
	task(t, s, "queued", interactions.ClassPeer, old, true)
	rowID := enqueueOutbox(t, s, interactions.OutboxItem{IX: "queued", ToAID: "did:anet:peer", Type: "anet.result/1",
		Body: []byte("b")})
	task(t, s, "unsettled", interactions.ClassPublicCap, old, true)
	if _, err := s.SetPayment("unsettled", interactions.PayUpdate{
		State: interactions.PayState(interactions.PaySubmitted), Payload: []byte("signed authorization")}); err != nil {
		t.Fatal(err)
	}

	got, err := s.PruneTerminal(now, interactions.ClockFull, r)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range interactions.ContentClasses() {
		want := interactions.Pruned{Interactions: 1, Messages: 1, Attachments: 1}
		if c == interactions.ClassPublicCap {
			want = interactions.Pruned{Interactions: 2, Messages: 2, Attachments: 2}
		}
		if got[c] != want {
			t.Errorf("%s: pruned %+v, want %+v", c, got[c], want)
		}
		if present(t, s, "old_"+c) {
			t.Errorf("old_%s is still stored", c)
		}
		if msgs, _ := s.Messages("old_" + c); len(msgs) != 0 {
			t.Errorf("old_%s: %d messages left", c, len(msgs))
		}
		if atts, _ := s.Attachments("old_" + c); len(atts) != 0 {
			t.Errorf("old_%s: %d attachments left", c, len(atts))
		}
		for _, id := range []string{"recent_" + c, "open_" + c} {
			if !present(t, s, id) {
				t.Errorf("%s was deleted", id)
			}
		}
	}
	if present(t, s, "week_"+interactions.ClassPublicCap) || !present(t, s, "days_"+interactions.ClassPublicCap) {
		t.Error("a public call is not kept seven days")
	}
	for _, id := range []string{"queued", "unsettled"} {
		if !present(t, s, id) {
			t.Errorf("%s was deleted", id)
		}
		if msgs, _ := s.Messages(id); len(msgs) != 1 {
			t.Errorf("%s: %d messages left", id, len(msgs))
		}
	}
	if again, err := s.PruneTerminal(now, interactions.ClockFull, r); err != nil || len(again) != 0 {
		t.Fatalf("a second sweep: %+v %v", again, err)
	}
	// The row is confirmed and the payment settled: the next sweep takes both.
	if err := s.Update(func(tx *interactions.Tx) error {
		if ok, err := tx.DeleteOutboxRow(rowID); err != nil || !ok {
			return fmt.Errorf("delete the row: %v %v", ok, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetPayment("unsettled", interactions.PayUpdate{State: interactions.PayState(interactions.PayCompleted)}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.PruneTerminal(now, interactions.ClockFull, r); err != nil ||
		got[interactions.ClassPeer].Interactions != 1 || got[interactions.ClassPublicCap].Interactions != 1 {
		t.Fatalf("after the row and the payment: %+v %v", got, err)
	}
	if present(t, s, "queued") || present(t, s, "unsettled") {
		t.Error("the task whose row was confirmed, or whose payment settled, is still stored")
	}
	if _, err := s.PruneTerminal(now, interactions.ClockFull, interactions.Retention{}); !errors.Is(err, interactions.ErrBadInput) {
		t.Fatalf("a sweep without a setting: %v", err)
	}
}

// FS-L6: nothing goes at the closed gate; at the local gate the sweep deletes what it deletes at full.
func TestPruneTerminalFollowsTheGate(t *testing.T) {
	s := open(t)
	now := at(2026, 9, 27, 15, 0)
	r := mustRetention(t, "1d")
	task(t, s, "ix_a", interactions.ClassPeer, now-5*dayMS, true)
	task(t, s, "ix_b", interactions.ClassPublicCap, now-9*dayMS, true)
	if got, err := s.PruneTerminal(now, interactions.ClockClosed, r); err != nil || len(got) != 0 {
		t.Fatalf("closed: %+v %v", got, err)
	}
	if !present(t, s, "ix_a") || !present(t, s, "ix_b") {
		t.Fatal("a task went at the closed gate")
	}
	if got, err := s.PruneTerminal(now, interactions.ClockLocal, r); err != nil ||
		got[interactions.ClassPeer].Interactions != 1 || got[interactions.ClassPublicCap].Interactions != 1 {
		t.Fatalf("local: %+v %v", got, err)
	}
}

// TestForeverKeepsPrivateTasks (mutation 32; J7): under forever, a task this node sent, one a listed
// peer sent and one the operator approved stay however long ago they ended; public tasks still go.
func TestForeverKeepsPrivateTasks(t *testing.T) {
	s := open(t)
	now := at(2036, 9, 27, 15, 0)
	ended := at(2026, 1, 1, 0, 0)
	for _, c := range interactions.ContentClasses() {
		task(t, s, "ix_"+c, c, ended, true)
	}
	got, err := s.PruneTerminal(now, interactions.ClockFull, interactions.RetentionForever)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range interactions.ContentClasses() {
		if want := interactions.PublicClass(c); present(t, s, "ix_"+c) == want {
			t.Errorf("forever: %s present %v", c, !want)
		}
	}
	for _, c := range []string{interactions.ClassOutbound, interactions.ClassPeer, interactions.ClassApproved} {
		if _, err := s.Messages("ix_" + c); err != nil {
			t.Errorf("forever: the messages of %s do not read: %v", c, err)
		}
		if got[c].Interactions != 0 {
			t.Errorf("forever: %s pruned %+v", c, got[c])
		}
	}
	if n, err := s.CountNewlyDeletable(now, mustRetention(t, "30d"), interactions.RetentionForever); err != nil || n != 0 {
		t.Errorf("setting forever would delete %d (%v)", n, err)
	}
}

// TestPublicTasksCappedAtSevenDays (mutation 33; J7): forever does not keep a public task: one that
// ended eight days ago goes, one of six days stays; a content_retention shorter than seven days applies
// to public tasks too.
func TestPublicTasksCappedAtSevenDays(t *testing.T) {
	s := open(t)
	now := at(2026, 9, 27, 15, 0)
	for _, c := range []string{interactions.ClassPublic, interactions.ClassPublicCap} {
		task(t, s, "eight_"+c, c, now-8*dayMS, true)
		task(t, s, "six_"+c, c, now-6*dayMS, true)
		task(t, s, "four_"+c, c, now-4*dayMS, true)
	}
	if _, err := s.PruneTerminal(now, interactions.ClockFull, interactions.RetentionForever); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{interactions.ClassPublic, interactions.ClassPublicCap} {
		if present(t, s, "eight_"+c) || !present(t, s, "six_"+c) || !present(t, s, "four_"+c) {
			t.Errorf("forever, %s: eight days %v, six %v, four %v", c,
				present(t, s, "eight_"+c), present(t, s, "six_"+c), present(t, s, "four_"+c))
		}
	}
	if _, err := s.PruneTerminal(now, interactions.ClockFull, mustRetention(t, "3d")); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{interactions.ClassPublic, interactions.ClassPublicCap} {
		if present(t, s, "six_"+c) || present(t, s, "four_"+c) {
			t.Errorf("3d, %s: a public task of four or six days stayed", c)
		}
	}
}

// TestRetentionDeletesDEK (FS-L8, mutation 18): the sweep deletes a task's data key in the transaction
// that deletes the task, and once the log is truncated the key's bytes are in no file of the store.
// The key of a held delegation with the same id stays while the held item does: its content is under
// it.
func TestRetentionDeletesDEK(t *testing.T) {
	dir := t.TempDir()
	s, err := interactions.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := at(2026, 9, 27, 15, 0)
	for i := 0; i < 40; i++ { // neighbours on the same pages
		task(t, s, fmt.Sprintf("ix_keep_%02d", i), interactions.ClassPeer, now-dayMS, true)
	}
	task(t, s, "ix_gone", interactions.ClassPeer, now-40*dayMS, true)
	task(t, s, "ix_shared", interactions.ClassPeer, now-40*dayMS, true)
	if err := s.Update(func(tx *interactions.Tx) error {
		return tx.PutPending(interactions.PendingItem{IX: "ix_shared", FromAID: "did:anet:peer", ArrivedAt: now, Delegate: []byte("D")}, 0, 0)
	}); err != nil {
		t.Fatal(err)
	}
	keyOf := func(ix string) []byte {
		t.Helper()
		k, err := s.DEK(ix)
		if err != nil {
			t.Fatal(err)
		}
		defer k.Wipe()
		return k.Bytes()
	}
	gone := keyOf("ix_gone")
	if _, err := s.TruncateWAL(); err != nil {
		t.Fatal(err)
	}
	if len(dbHits(t, dir, gone)) == 0 {
		t.Fatal("the data key is not in the store's files: the test would prove nothing")
	}
	got, err := s.PruneTerminal(now, interactions.ClockFull, mustRetention(t, "30d"))
	if err != nil || got[interactions.ClassPeer].Interactions != 2 {
		t.Fatalf("pruned %+v %v", got, err)
	}
	if _, err := s.DEK("ix_gone"); !errors.Is(err, interactions.ErrContentGone) {
		t.Fatalf("the pruned task's data key: %v", err)
	}
	if busy, err := s.TruncateWAL(); err != nil || busy != 0 {
		t.Fatalf("truncate: busy %v %v", busy, err)
	}
	if hits := dbHits(t, dir, gone); len(hits) != 0 {
		t.Fatalf("the deleted data key is still in %v", hits)
	}
	// The held item of the same id still reads, and takes the key when it goes.
	if p, err := s.GetPending("ix_shared"); err != nil || string(p.Delegate) != "D" {
		t.Fatalf("the held item lost its content: %v", err)
	}
	if ok, err := s.DeletePending("ix_shared"); err != nil || !ok {
		t.Fatal(err)
	}
	if _, err := s.DEK("ix_shared"); !errors.Is(err, interactions.ErrContentGone) {
		t.Fatalf("the held item's key outlived it: %v", err)
	}
	for i := 0; i < 40; i++ {
		if _, err := s.DEK(fmt.Sprintf("ix_keep_%02d", i)); err != nil {
			t.Fatalf("a kept task lost its key: %v", err)
		}
	}
}

// A sweep with more candidates than one batch reads them all, over several transactions: tasks it
// passes over (a row queued) do not stop it reaching the deletable ones after them.
func TestPruneTerminalDeletesInBatches(t *testing.T) {
	defer interactions.SetPruneBatch(2)()
	s := open(t)
	now := at(2026, 9, 27, 15, 0)
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("ix_q%d", i)
		task(t, s, id, interactions.ClassPublicCap, now-30*dayMS+int64(i), true)
		enqueueOutbox(t, s, interactions.OutboxItem{IX: id, ToAID: "did:anet:peer", Type: "anet.result/1",
			Body: []byte("b")})
	}
	for i := 0; i < 5; i++ {
		task(t, s, fmt.Sprintf("ix_%d", i), interactions.ClassPublicCap, now-20*dayMS+int64(i), true)
	}
	got, err := s.PruneTerminal(now, interactions.ClockFull, mustRetention(t, "30d"))
	if err != nil || got[interactions.ClassPublicCap] != (interactions.Pruned{Interactions: 5, Messages: 5, Attachments: 5}) {
		t.Fatalf("pruned %+v %v, want the five with their messages and attachments", got, err)
	}
	if n, err := s.CountNewlyDeletable(now, interactions.RetentionForever, mustRetention(t, "1d")); err != nil || n != 0 {
		t.Fatalf("left %d deletable (%v)", n, err)
	}
	for i := 0; i < 3; i++ {
		if !present(t, s, fmt.Sprintf("ix_q%d", i)) {
			t.Fatal("a task with a row queued went")
		}
	}
}

// CountNewlyDeletable is what a change of the setting deletes at once: shortening counts the tasks the
// new setting lets go and the old one did not; lengthening and forever count 0; a task the sweep would
// pass over (a row queued) is not counted; an unreadable current setting counts as keeping everything.
func TestCountNewlyDeletable(t *testing.T) {
	s := open(t)
	now := at(2026, 9, 27, 15, 0)
	for i, days := range []int64{2, 10, 20, 40, 60} {
		task(t, s, fmt.Sprintf("ix_%d", i), interactions.ClassPeer, now-days*dayMS, true)
	}
	task(t, s, "ix_pub", interactions.ClassPublicCap, now-10*dayMS, true) // already the sweep's
	task(t, s, "ix_pub_new", interactions.ClassPublicCap, now-4*dayMS, true)
	task(t, s, "ix_queued", interactions.ClassPeer, now-40*dayMS, true)
	enqueueOutbox(t, s, interactions.OutboxItem{IX: "ix_queued", ToAID: "did:anet:peer", Type: "anet.result/1",
		Body: []byte("b")})
	r30, r7, r90 := mustRetention(t, "30d"), mustRetention(t, "7d"), mustRetention(t, "90d")
	r3 := mustRetention(t, "3d")
	for _, tc := range []struct {
		cur, next interactions.Retention
		want      int64
	}{
		{r30, r7, 2},  // 10 and 20 days
		{r30, r3, 3},  // two days stays; 10, 20 and the four-day public call
		{r7, r30, 0},  // longer
		{r30, r90, 0}, // longer
		{r30, interactions.RetentionForever, 0},
		{interactions.RetentionForever, r30, 2}, // 40 and 60
		{interactions.Retention{}, r30, 2},      // unreadable now: as forever, 40 and 60
		{r30, r30, 0},
	} {
		if n, err := s.CountNewlyDeletable(now, tc.cur, tc.next); err != nil || n != tc.want {
			t.Errorf("%q → %q: %d (%v), want %d", tc.cur, tc.next, n, err, tc.want)
		}
	}
	if _, err := s.CountNewlyDeletable(now, r30, interactions.Retention{}); !errors.Is(err, interactions.ErrBadInput) {
		t.Errorf("a count without a new setting: %v", err)
	}
}

// With the daemon stopped, the count reads the store in place and changes nothing in its file; a
// directory without a store counts 0, and a store this version does not open is refused as it is
// when the daemon starts.
func TestCountNewlyDeletableIn(t *testing.T) {
	dir := t.TempDir()
	s, err := interactions.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := at(2026, 9, 27, 15, 0)
	task(t, s, "ix_old", interactions.ClassOutbound, now-20*dayMS, true)
	task(t, s, "ix_new", interactions.ClassOutbound, now-2*dayMS, true)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	sum := func() [32]byte {
		b, err := os.ReadFile(filepath.Join(dir, "interactions.db"))
		if err != nil {
			t.Fatal(err)
		}
		return sha256.Sum256(b)
	}
	before := sum()
	n, err := interactions.CountNewlyDeletableIn(dir, now, mustRetention(t, "30d"), mustRetention(t, "7d"))
	if err != nil || n != 1 {
		t.Fatalf("count %d %v, want 1", n, err)
	}
	if sum() != before {
		t.Fatal("counting changed the store's file")
	}
	if n, err := interactions.CountNewlyDeletableIn(t.TempDir(), now, mustRetention(t, "30d"), mustRetention(t, "7d")); err != nil || n != 0 {
		t.Fatalf("no store: %d %v", n, err)
	}
	old := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(old, "interactions.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE interaction (id TEXT)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := interactions.CountNewlyDeletableIn(old, now, mustRetention(t, "30d"), mustRetention(t, "7d")); !errors.Is(err, interactions.ErrOldStore) {
		t.Fatalf("an old store: %v", err)
	}
}

// FS-8 leftover: a held item whose data key is gone is still found by the expiry, its content empty,
// so it expires and gives its place in the queue back; the listing leaves it out.
func TestKeylessHeldItemExpires(t *testing.T) {
	s := open(t)
	if err := s.Update(func(tx *interactions.Tx) error {
		if err := tx.PutPending(interactions.PendingItem{IX: "ix_held", FromAID: "did:anet:peer", ArrivedAt: 100,
			Delegate: []byte("D"), KEL: []byte("kel"), Keys: []byte("keys")}, 0, 0); err != nil {
			return err
		}
		_, err := tx.DeleteDEK("ix_held")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if l, err := s.ListPending(); err != nil || len(l) != 0 {
		t.Fatalf("listing: %d %v", len(l), err)
	}
	items, err := s.ExpiredPending(200)
	if err != nil || len(items) != 1 {
		t.Fatalf("expired: %d %v", len(items), err)
	}
	if it := items[0]; it.IX != "ix_held" || it.FromAID != "did:anet:peer" || string(it.KEL) != "kel" || string(it.Keys) != "keys" ||
		it.Delegate != nil || it.Attachments != nil || it.Followups != nil {
		t.Fatalf("expired item %+v", it)
	}
	if ok, err := s.DeletePending("ix_held"); err != nil || !ok {
		t.Fatalf("delete: %v %v", ok, err)
	}
	if items, _ := s.ExpiredPending(200); len(items) != 0 {
		t.Fatal("the item is still held")
	}
}

// TruncateWAL leaves the write-ahead log empty; while a reader holds an older snapshot it gives up
// within its wait and counts the calls in a row that found the log busy.
func TestTruncateWAL(t *testing.T) {
	dir := t.TempDir()
	s, err := interactions.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	task(t, s, "ix_a", interactions.ClassPeer, 1000, true)
	wal := filepath.Join(dir, "interactions.db-wal")
	if fi, err := os.Stat(wal); err != nil || fi.Size() == 0 {
		t.Fatalf("no log to truncate: %v", err)
	}
	if busy, err := s.TruncateWAL(); err != nil || busy != 0 {
		t.Fatalf("truncate: %v %v", busy, err)
	}
	if fi, err := os.Stat(wal); err != nil || fi.Size() != 0 {
		t.Fatalf("log after truncate: %v %v", fi.Size(), err)
	}
	// A reader on another connection holds a snapshot while a write moves on.
	other, err := sql.Open("sqlite", filepath.Join(dir, "interactions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	rtx, err := other.Begin()
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := rtx.QueryRow(`SELECT count(*) FROM interaction`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	task(t, s, "ix_b", interactions.ClassPeer, 1000, true)
	for want := 1; want <= 2; want++ {
		start := time.Now()
		busy, err := s.TruncateWAL()
		if err != nil || busy != want {
			t.Fatalf("with a reader, try %d: busy %v %v", want, busy, err)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("waited %s for the reader", d)
		}
	}
	_ = rtx.Rollback()
	if busy, err := s.TruncateWAL(); err != nil || busy != 0 {
		t.Fatalf("after the reader: %v %v", busy, err)
	}
	if fi, err := os.Stat(wal); err != nil || fi.Size() != 0 {
		t.Fatalf("log after the reader: %v %v", fi.Size(), err)
	}
}

// OpenIDs lists the tasks not finished, and only them.
func TestOpenIDs(t *testing.T) {
	s := open(t)
	task(t, s, "ix_open", interactions.ClassPeer, 1000, false)
	task(t, s, "ix_done", interactions.ClassPeer, 1000, true)
	ids, err := s.OpenIDs()
	if err != nil || len(ids) != 1 || !ids["ix_open"] {
		t.Fatalf("open ids %v %v", ids, err)
	}
}

// FuzzParseRetention: the parser never panics, and what it takes is a setting that writes back as the
// same text.
func FuzzParseRetention(f *testing.F) {
	for _, s := range []string{"30d", "1d", "3650d", "3651d", "0d", "forever", "", "d", "030d", "-1d", "1e3d", "７d"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		r, ok := interactions.ParseRetention(s)
		if !ok {
			if r.Valid() {
				t.Fatalf("%q refused but valid", s)
			}
			return
		}
		if !r.Valid() || r.String() != s {
			t.Fatalf("%q read as %q (valid %v)", s, r.String(), r.Valid())
		}
		if !r.Forever() && (r.Days() < 1 || r.Days() > interactions.MaxRetentionDays) {
			t.Fatalf("%q: %d days", s, r.Days())
		}
		if back, ok := interactions.ParseRetention(r.String()); !ok || back != r {
			t.Fatalf("%q does not read back", s)
		}
	})
}
