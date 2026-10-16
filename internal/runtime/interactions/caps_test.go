package interactions_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/identity/identitytest"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

const (
	capsDay    = uint64(24 * 3600 * 1000)
	capsT0     = uint64(1_900_000_000_000) // an arbitrary "now", unix ms
	issuerA    = "did:anet:issuer-a"
	issuerB    = "did:anet:issuer-b"
	granteeG   = "did:anet:grantee-g"
	granteeH   = "did:anet:grantee-h"
	capsHomeID = "did:anet:hub"
)

func capID(n byte) []byte { return bytes.Repeat([]byte{n}, seal.CapIDLen) }

func seed32(n byte) []byte { return bytes.Repeat([]byte{n}, 32) }

// held is a caps_held row for tests: a valid-looking capability under issuer's epoch.
func held(issuer, scope, ix string, id byte, epoch, exp uint64) interactions.HeldCap {
	return interactions.HeldCap{
		Issuer: issuer, Scope: scope, IX: ix, CapID: capID(id), Cap: []byte{0xa2, id}, HolderSeed: seed32(id),
		Class: seal.CapClassContact, Epoch: epoch, CapExp: exp, Home: capsHomeID, ReceivedAt: capsT0,
	}
}

// issued is a caps_issued row for tests.
func issued(grantee, scope, ix string, id byte, class, exp uint64) interactions.IssuedCap {
	return interactions.IssuedCap{
		CapID: capID(id), Grantee: grantee, Class: class, Scope: scope, IX: ix, HolderSeed: seed32(id),
		HolderPub: seed32(id + 1), Epoch: 1, CapExp: exp, IssuedAt: capsT0,
	}
}

func putHeld(t *testing.T, s *interactions.Store, h interactions.HeldCap) bool {
	t.Helper()
	var wrote bool
	if err := s.Update(func(tx *interactions.Tx) error {
		var err error
		wrote, err = tx.PutHeldGrant(h)
		return err
	}); err != nil {
		t.Fatalf("PutHeldGrant(%s %s %q): %v", h.Issuer, h.Scope, h.IX, err)
	}
	return wrote
}

func putIssued(t *testing.T, s *interactions.Store, c interactions.IssuedCap) {
	t.Helper()
	if err := s.Update(func(tx *interactions.Tx) error { return tx.PutIssuedCap(c) }); err != nil {
		t.Fatalf("PutIssuedCap(%x): %v", c.CapID, err)
	}
}

func heldRow(t *testing.T, s *interactions.Store, issuer, scope, ix string) *interactions.HeldCap {
	t.Helper()
	h, err := s.HeldCap(issuer, scope, ix)
	if err != nil {
		t.Fatalf("HeldCap(%s %s %q): %v", issuer, scope, ix, err)
	}
	return h
}

// TestCapsSchema (SS §3.2): the columns of caps_issued and caps_held, their
// keys and indexes, and the constraints that keep a row meaningful.
func TestCapsSchema(t *testing.T) {
	s := open(t)
	zero := "0"
	empty := "''"
	type col struct {
		name    string
		typ     string
		notNull bool
		dflt    *string
		pk      bool
	}
	want := map[string][]col{
		"caps_issued": {
			{"cap_id", "BLOB", false, nil, true},
			{"grantee", "TEXT", true, nil, false},
			{"class", "INTEGER", true, nil, false},
			{"scope", "TEXT", true, nil, false},
			{"ix", "TEXT", true, &empty, false},
			{"holder_seed", "BLOB", true, nil, false},
			{"holder_pub", "BLOB", true, nil, false},
			{"epoch", "INTEGER", true, nil, false},
			{"cap_exp", "INTEGER", true, nil, false},
			{"issued_at", "INTEGER", true, nil, false},
			{"revoked_at", "INTEGER", false, nil, false},
			{"revoke_pushed", "INTEGER", true, &zero, false},
		},
		"caps_held": {
			{"issuer", "TEXT", true, nil, false},
			{"scope", "TEXT", true, nil, false},
			{"ix", "TEXT", true, &empty, false},
			{"cap_id", "BLOB", true, nil, false},
			{"cap", "BLOB", true, nil, false},
			{"holder_seed", "BLOB", true, nil, false},
			{"class", "INTEGER", true, nil, false},
			{"epoch", "INTEGER", true, nil, false},
			{"cap_exp", "INTEGER", true, nil, false},
			{"home", "TEXT", true, nil, false},
			{"p2p", "TEXT", true, &empty, false},
			{"pay", "INTEGER", true, &zero, false},
			{"invalid_at", "INTEGER", false, nil, false},
			{"refused_n", "INTEGER", true, &zero, false},
			{"refused_since", "INTEGER", false, nil, false},
			{"received_at", "INTEGER", true, nil, false},
		},
	}
	for table, cols := range want {
		got, err := s.TableColumns(table)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(cols) {
			t.Errorf("%s has %d columns, want %d: %+v", table, len(got), len(cols), got)
			continue
		}
		for i, c := range cols {
			g := got[i]
			dflt := func(p *string) string {
				if p == nil {
					return "<none>"
				}
				return *p
			}
			if g.Name != c.name || g.Type != c.typ || g.NotNull != c.notNull || g.PK != c.pk || dflt(g.Default) != dflt(c.dflt) {
				t.Errorf("%s column %d = %s %s notnull=%v default=%s pk=%v, want %s %s notnull=%v default=%s pk=%v",
					table, i, g.Name, g.Type, g.NotNull, dflt(g.Default), g.PK, c.name, c.typ, c.notNull, dflt(c.dflt), c.pk)
			}
		}
	}

	// Keys and indexes.
	idx := func(table string) map[string]interactions.TableIndex {
		ixs, err := s.TableIndexes(table)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]interactions.TableIndex{}
		for _, x := range ixs {
			m[strings.Join(x.Columns, ",")] = x
		}
		return m
	}
	issuedIx, heldIx := idx("caps_issued"), idx("caps_held")
	if x, ok := issuedIx["cap_id"]; !ok || !x.Unique {
		t.Errorf("caps_issued has no unique key on cap_id: %+v", issuedIx)
	}
	if _, ok := issuedIx["grantee,scope"]; !ok {
		t.Errorf("caps_issued has no index (grantee, scope): %+v", issuedIx)
	}
	if x, ok := heldIx["issuer,scope,ix"]; !ok || !x.Unique || x.Partial {
		t.Errorf("caps_held has no unique key (issuer, scope, ix): %+v", heldIx)
	}

	// A peer capability carries ix "", so the unique key holds for it: a
	// second peer row for the same issuer is a conflict, not a second row.
	insert := `INSERT INTO caps_held(issuer, scope, ix, cap_id, cap, holder_seed, class, epoch, cap_exp, home, received_at)
		VALUES(?,?,?,?,X'01',?,1,1,?,'h',0)`
	if err := s.ExecRaw(insert, issuerA, "peer", "", capID(1), seed32(1), capsT0); err != nil {
		t.Fatal(err)
	}
	if err := s.ExecRaw(insert, issuerA, "peer", "", capID(2), seed32(2), capsT0); err == nil {
		t.Error("two peer capabilities from one issuer were both stored")
	}
	// The constraints refuse rows that make no sense.
	bad := []struct {
		name string
		q    string
		args []any
	}{
		{"peer scope with an ix", insert, []any{issuerB, "peer", "ix-1", capID(3), seed32(3), capsT0}},
		{"one-shot without an ix", insert, []any{issuerB, "ix", "", capID(3), seed32(3), capsT0}},
		{"unknown scope", insert, []any{issuerB, "other", "", capID(3), seed32(3), capsT0}},
		{"short cap_id", insert, []any{issuerB, "peer", "", []byte{1, 2}, seed32(3), capsT0}},
		{"short holder seed", insert, []any{issuerB, "peer", "", capID(3), []byte{1}, capsT0}},
		{"class 3", `INSERT INTO caps_issued(cap_id, grantee, class, scope, ix, holder_seed, holder_pub, epoch, cap_exp, issued_at)
			VALUES(?, 'g', 3, 'peer', '', ?, ?, 1, 0, 0)`, []any{capID(4), seed32(4), seed32(5)}},
		{"epoch 0", `INSERT INTO caps_issued(cap_id, grantee, class, scope, ix, holder_seed, holder_pub, epoch, cap_exp, issued_at)
			VALUES(?, 'g', 1, 'peer', '', ?, ?, 0, 0, 0)`, []any{capID(4), seed32(4), seed32(5)}},
		{"no grantee", `INSERT INTO caps_issued(cap_id, grantee, class, scope, ix, holder_seed, holder_pub, epoch, cap_exp, issued_at)
			VALUES(?, '', 1, 'peer', '', ?, ?, 1, 0, 0)`, []any{capID(4), seed32(4), seed32(5)}},
	}
	for _, b := range bad {
		if err := s.ExecRaw(b.q, b.args...); err == nil {
			t.Errorf("%s: stored", b.name)
		}
	}
	// A fresh row starts unrefused and valid.
	h := heldRow(t, s, issuerA, interactions.CapScopePeer, "")
	if h.RefusedN != 0 || h.RefusedSince != 0 || h.InvalidAt != 0 {
		t.Errorf("a new held row starts at refused_n=%d refused_since=%d invalid_at=%d", h.RefusedN, h.RefusedSince, h.InvalidAt)
	}
}

// HeldFromGrant reads every column off a grant seal.CheckGrant accepted.
func TestHeldFromGrant(t *testing.T) {
	_, mboxSK, _ := ed25519.GenerateKey(rand.Reader)
	holderSeed := seed32(7)
	holder := ed25519.NewKeyFromSeed(holderSeed).Public().(ed25519.PublicKey)
	body := &seal.CapBody{Owner: issuerA, Epoch: 3, CapID: capID(9), Class: seal.CapClassContact, CapExp: capsT0 + capsDay, Holder: holder}
	c, err := seal.SignCap(mboxSK, body)
	if err != nil {
		t.Fatal(err)
	}
	addr, pay, ix := "192.0.2.1:7000", true, "ix-77"
	peer := &seal.Grant{Cap: *c, HolderSeed: holderSeed, Home: capsHomeID, P2P: &addr, Pay: &pay}
	h, err := interactions.HeldFromGrant(peer, body, capsT0)
	if err != nil {
		t.Fatal(err)
	}
	wantCap, _ := c.Marshal()
	if h.Issuer != issuerA || h.Scope != interactions.CapScopePeer || h.IX != "" || !bytes.Equal(h.CapID, body.CapID) ||
		!bytes.Equal(h.Cap, wantCap) || !bytes.Equal(h.HolderSeed, holderSeed) || h.Class != seal.CapClassContact ||
		h.Epoch != 3 || h.CapExp != body.CapExp || h.Home != capsHomeID || h.P2P != addr || !h.Pay || h.ReceivedAt != capsT0 {
		t.Fatalf("peer grant row = %+v", h)
	}
	oneShot := &seal.Grant{Cap: *c, HolderSeed: holderSeed, Home: capsHomeID, IX: &ix}
	h, err = interactions.HeldFromGrant(oneShot, body, capsT0)
	if err != nil {
		t.Fatal(err)
	}
	if h.Scope != interactions.CapScopeIX || h.IX != ix || h.P2P != "" || h.Pay {
		t.Fatalf("one-shot grant row = %+v", h)
	}
	if _, err := interactions.HeldFromGrant(nil, body, capsT0); !errors.Is(err, interactions.ErrBadInput) {
		t.Fatalf("no grant: %v", err)
	}
}

// The rules of putHeldGrantTx: a new row; a renewal keeps the refusal
// count; another cap_id replaces the row and its count; an invalidated
// capability is never revived by the same cap_id (SSR-D11 item 5); an older
// epoch or an earlier copy changes nothing.
func TestPutHeldGrantRules(t *testing.T) {
	s := open(t)
	peer := held(issuerA, interactions.CapScopePeer, "", 1, 2, capsT0+10*capsDay)
	if !putHeld(t, s, peer) {
		t.Fatal("a first grant was not stored")
	}
	// Two refusals.
	note := func(id []byte, at uint64) (uint64, uint64, bool) {
		var n, since uint64
		var ok bool
		if err := s.Update(func(tx *interactions.Tx) error {
			var err error
			n, since, ok, err = tx.NoteHeldRefused(issuerA, interactions.CapScopePeer, "", id, at)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return n, since, ok
	}
	if n, since, ok := note(capID(1), capsT0+5); !ok || n != 1 || since != capsT0+5 {
		t.Fatalf("first refusal: n=%d since=%d ok=%v", n, since, ok)
	}
	if n, since, ok := note(capID(1), capsT0+9); !ok || n != 2 || since != capsT0+5 {
		t.Fatalf("second refusal: n=%d since=%d ok=%v, since must stay at the first", n, since, ok)
	}
	// A refusal of a capability the row no longer holds counts nothing.
	if _, _, ok := note(capID(8), capsT0+9); ok {
		t.Fatal("a refusal of another cap_id was counted against the row")
	}

	// A renewal: same cap_id, later cap_exp, new p2p.
	renew := peer
	renew.CapExp, renew.P2P, renew.Cap, renew.ReceivedAt = capsT0+20*capsDay, "198.51.100.7:7000", []byte{0xa2, 0xff}, capsT0+100
	if !putHeld(t, s, renew) {
		t.Fatal("a renewal was not stored")
	}
	h := heldRow(t, s, issuerA, interactions.CapScopePeer, "")
	if h.CapExp != renew.CapExp || h.P2P != renew.P2P || !bytes.Equal(h.Cap, renew.Cap) || h.RefusedN != 2 || h.RefusedSince != capsT0+5 {
		t.Fatalf("after renewal: %+v (the refusal count must stay)", h)
	}
	// An older copy of the same capability does not shorten it.
	if putHeld(t, s, peer) {
		t.Fatal("an earlier cap_exp of the same capability was written")
	}
	// A grant under an older mailbox epoch changes nothing.
	old := held(issuerA, interactions.CapScopePeer, "", 5, 1, capsT0+30*capsDay)
	if putHeld(t, s, old) {
		t.Fatal("a grant under an older epoch replaced the row")
	}
	if h := heldRow(t, s, issuerA, interactions.CapScopePeer, ""); !bytes.Equal(h.CapID, capID(1)) {
		t.Fatalf("row holds %x after the older grant", h.CapID)
	}

	// Invalidated: the same cap_id does not revive it, not even renewed.
	if err := s.Update(func(tx *interactions.Tx) error {
		ok, err := tx.MarkHeldInvalid(issuerA, interactions.CapScopePeer, "", capID(1), capsT0+200)
		if !ok && err == nil {
			err = errors.New("nothing marked")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// An invalid capability takes no refusal count, and marking it again
	// keeps its first invalid_at.
	if _, _, ok := note(capID(1), capsT0+210); ok {
		t.Fatal("a refusal was counted against an invalid capability")
	}
	if err := s.Update(func(tx *interactions.Tx) error {
		if ok, err := tx.MarkHeldInvalid(issuerA, interactions.CapScopePeer, "", capID(1), capsT0+220); ok || err != nil {
			return fmt.Errorf("marked an invalid capability again: %v %v", ok, err)
		}
		if ok, err := tx.MarkHeldInvalid(issuerA, interactions.CapScopePeer, "", nil, capsT0+230); ok || err != nil {
			return fmt.Errorf("marked an invalid row again: %v %v", ok, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if h := heldRow(t, s, issuerA, interactions.CapScopePeer, ""); h.InvalidAt != capsT0+200 || h.RefusedN != 2 {
		t.Fatalf("after touching an invalid row: invalid_at=%d refused_n=%d, want %d and 2", h.InvalidAt, h.RefusedN, capsT0+200)
	}
	again := renew
	again.CapExp = capsT0 + 25*capsDay
	if putHeld(t, s, again) {
		t.Fatal("the same cap_id revived an invalidated capability")
	}
	if h := heldRow(t, s, issuerA, interactions.CapScopePeer, ""); h.InvalidAt != capsT0+200 {
		t.Fatalf("invalid_at = %d after a same-cap_id grant, want %d", h.InvalidAt, capsT0+200)
	}

	// Another cap_id replaces the row whole: valid again, count reset.
	fresh := held(issuerA, interactions.CapScopePeer, "", 3, 2, capsT0+12*capsDay)
	fresh.Pay = true
	if !putHeld(t, s, fresh) {
		t.Fatal("a grant with a new cap_id was not stored")
	}
	h = heldRow(t, s, issuerA, interactions.CapScopePeer, "")
	if !bytes.Equal(h.CapID, capID(3)) || h.InvalidAt != 0 || h.RefusedN != 0 || h.RefusedSince != 0 || !h.Pay || h.P2P != "" {
		t.Fatalf("after a new cap_id: %+v", h)
	}
	// Accepted: the count goes back to zero, for that capability only.
	note(capID(3), capsT0+300)
	if err := s.Update(func(tx *interactions.Tx) error {
		if ok, err := tx.ClearHeldRefused(issuerA, interactions.CapScopePeer, "", capID(9)); ok || err != nil {
			return fmt.Errorf("cleared another cap_id's count: %v %v", ok, err)
		}
		ok, err := tx.ClearHeldRefused(issuerA, interactions.CapScopePeer, "", capID(3))
		if !ok && err == nil {
			err = errors.New("nothing cleared")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if h := heldRow(t, s, issuerA, interactions.CapScopePeer, ""); h.RefusedN != 0 || h.RefusedSince != 0 {
		t.Fatalf("after acceptance: refused_n=%d refused_since=%d", h.RefusedN, h.RefusedSince)
	}

	// One-shot capabilities sit beside the peer one, one per interaction.
	putHeld(t, s, held(issuerA, interactions.CapScopeIX, "ix-1", 4, 2, capsT0+capsDay))
	putHeld(t, s, held(issuerA, interactions.CapScopeIX, "ix-2", 6, 2, capsT0+capsDay))
	all, err := s.HeldCaps(issuerA)
	if err != nil || len(all) != 3 || all[0].Scope != interactions.CapScopePeer || all[1].IX != "ix-1" || all[2].IX != "ix-2" {
		t.Fatalf("HeldCaps = %+v (%v)", all, err)
	}
	if err := s.Update(func(tx *interactions.Tx) error {
		n, err := tx.DeleteHeldCapsOfIX("ix-1")
		if err == nil && n != 1 {
			err = fmt.Errorf("deleted %d", n)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.HeldCap(issuerA, interactions.CapScopeIX, "ix-1"); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatalf("the one-shot capability of a deleted interaction is still held: %v", err)
	}

	// Rows the writer refuses.
	for name, h := range map[string]interactions.HeldCap{
		"no issuer":        held("", interactions.CapScopePeer, "", 1, 1, capsT0),
		"peer with ix":     held(issuerB, interactions.CapScopePeer, "ix", 1, 1, capsT0),
		"one-shot no ix":   held(issuerB, interactions.CapScopeIX, "", 1, 1, capsT0),
		"epoch 0":          held(issuerB, interactions.CapScopePeer, "", 1, 0, capsT0),
		"epoch past int":   held(issuerB, interactions.CapScopePeer, "", 1, 1<<63, capsT0),
		"cap_exp past int": held(issuerB, interactions.CapScopePeer, "", 1, 1, 1<<63),
	} {
		if err := s.Update(func(tx *interactions.Tx) error { _, err := tx.PutHeldGrant(h); return err }); !errors.Is(err, interactions.ErrBadInput) {
			t.Errorf("%s: %v, want ErrBadInput", name, err)
		}
	}
}

// TestInvalidatePeerCapsTx (KEL D-44, ENG-K4): every capability one issuer
// granted, peer and one-shot, is invalidated and its direct address
// dropped, in the caller's transaction; other issuers keep theirs.
func TestInvalidatePeerCapsTx(t *testing.T) {
	s := open(t)
	p := held(issuerA, interactions.CapScopePeer, "", 1, 1, capsT0+capsDay)
	p.P2P = "192.0.2.1:7000"
	putHeld(t, s, p)
	putHeld(t, s, held(issuerA, interactions.CapScopeIX, "ix-1", 2, 1, capsT0+capsDay))
	q := held(issuerB, interactions.CapScopePeer, "", 3, 1, capsT0+capsDay)
	q.P2P = "192.0.2.2:7000"
	putHeld(t, s, q)

	// Rolled back with its transaction.
	sentinel := errors.New("roll back")
	if err := s.Update(func(tx *interactions.Tx) error {
		if _, err := tx.InvalidatePeerCaps(issuerA, capsT0+1); err != nil {
			return err
		}
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	if h := heldRow(t, s, issuerA, interactions.CapScopePeer, ""); h.InvalidAt != 0 || h.P2P == "" {
		t.Fatal("a rolled-back invalidation stayed")
	}

	var n int64
	if err := s.Update(func(tx *interactions.Tx) error {
		var err error
		n, err = tx.InvalidatePeerCaps(issuerA, capsT0+2)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("invalidated %d rows, want 2", n)
	}
	for _, k := range [][2]string{{interactions.CapScopePeer, ""}, {interactions.CapScopeIX, "ix-1"}} {
		h := heldRow(t, s, issuerA, k[0], k[1])
		if h.InvalidAt != capsT0+2 || h.P2P != "" {
			t.Errorf("%s %q: invalid_at=%d p2p=%q", k[0], k[1], h.InvalidAt, h.P2P)
		}
	}
	if h := heldRow(t, s, issuerB, interactions.CapScopePeer, ""); h.InvalidAt != 0 || h.P2P != q.P2P {
		t.Fatalf("another issuer's capability changed: %+v", h)
	}
	// Again: nothing new, the first invalid_at stays.
	if err := s.Update(func(tx *interactions.Tx) error {
		var err error
		n, err = tx.InvalidatePeerCaps(issuerA, capsT0+50)
		return err
	}); err != nil || n != 0 {
		t.Fatalf("second invalidation: n=%d err=%v", n, err)
	}
	if h := heldRow(t, s, issuerA, interactions.CapScopePeer, ""); h.InvalidAt != capsT0+2 {
		t.Fatalf("invalid_at moved to %d", h.InvalidAt)
	}
	held, issuedN, err := s.CapsCounts(capsT0)
	if err != nil || held != 1 || issuedN != 0 {
		t.Fatalf("CapsCounts = %d held, %d issued (%v), want 1, 0", held, issuedN, err)
	}
}

// keySetFor signs a key set for c whose mailbox key has epoch.
func keySetFor(t *testing.T, c *identity.Controller, epoch uint64) []byte {
	t.Helper()
	kp, err := seal.GenerateKeyPair(seal.SuiteX25519, capsT0, capsT0+capsDay)
	if err != nil {
		t.Fatal(err)
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	signed, err := seal.SignEncKeySet(&seal.EncKeySet{Type: seal.EncKeySetType, AID: c.AID(), Seq: capsT0 + epoch,
		Keys: []seal.EncKey{kp.Public}, IssuedAt: capsT0, Mbox: seal.Mbox{Epoch: epoch, Pub: pub}}, c.Sign)
	if err != nil {
		t.Fatal(err)
	}
	b, err := signed.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestOlderEpochHeldCapsInvalidated (SSR-D19): a peer's key set with a
// higher mailbox epoch invalidates the capabilities it signed under a lower
// one, in the transaction that records the key set — through
// invalidateOlderEpochTx and through every write of the peer's key set to
// peer_identity.
func TestOlderEpochHeldCapsInvalidated(t *testing.T) {
	s := open(t)
	putHeld(t, s, held(issuerA, interactions.CapScopePeer, "", 1, 1, capsT0+capsDay))
	putHeld(t, s, held(issuerA, interactions.CapScopeIX, "ix-2", 2, 2, capsT0+capsDay))
	putHeld(t, s, held(issuerB, interactions.CapScopePeer, "", 3, 1, capsT0+capsDay))

	var n int64
	if err := s.Update(func(tx *interactions.Tx) error {
		var err error
		n, err = tx.InvalidateOlderEpoch(issuerA, 2, capsT0+7)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("invalidated %d, want 1", n)
	}
	if h := heldRow(t, s, issuerA, interactions.CapScopePeer, ""); h.InvalidAt != capsT0+7 {
		t.Fatalf("the epoch-1 capability: invalid_at=%d", h.InvalidAt)
	}
	if h := heldRow(t, s, issuerA, interactions.CapScopeIX, "ix-2"); h.InvalidAt != 0 {
		t.Fatal("a capability at the current epoch was invalidated")
	}
	if h := heldRow(t, s, issuerB, interactions.CapScopePeer, ""); h.InvalidAt != 0 {
		t.Fatal("another issuer's capability was invalidated")
	}

	// Through peer_identity: the key set write invalidates in its own
	// transaction.
	c, err := identitytest.Agent()
	if err != nil {
		t.Fatal(err)
	}
	peer := c.AID()
	s.SetClock(func() int64 { return int64(capsT0 + 11) })
	putHeld(t, s, held(peer, interactions.CapScopePeer, "", 4, 1, capsT0+capsDay))
	putHeld(t, s, held(peer, interactions.CapScopeIX, "ix-4", 5, 1, capsT0+capsDay))
	kel, err := identity.MarshalKEL(c.KEL())
	if err != nil {
		t.Fatal(err)
	}
	write := func(keys []byte) {
		t.Helper()
		if err := s.UpdatePeerIdentity(peer, func(cur *interactions.PeerIdentity) (*interactions.PeerIdentity, error) {
			next := &interactions.PeerIdentity{KEL: kel, KELLen: 1}
			if cur != nil {
				next = cur
			}
			next.KeySet = keys
			return next, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	write(keySetFor(t, c, 1)) // the same epoch: nothing changes
	if h := heldRow(t, s, peer, interactions.CapScopePeer, ""); h.InvalidAt != 0 {
		t.Fatal("a key set at the capability's own epoch invalidated it")
	}
	write(keySetFor(t, c, 2))
	for _, k := range [][2]string{{interactions.CapScopePeer, ""}, {interactions.CapScopeIX, "ix-4"}} {
		if h := heldRow(t, s, peer, k[0], k[1]); h.InvalidAt != capsT0+11 {
			t.Errorf("%s %q after the epoch-2 key set: invalid_at=%d", k[0], k[1], h.InvalidAt)
		}
	}
	// A grant under the new epoch is valid and stays so when the same key
	// set is written again.
	putHeld(t, s, held(peer, interactions.CapScopePeer, "", 6, 2, capsT0+capsDay))
	write(keySetFor(t, c, 2))
	if h := heldRow(t, s, peer, interactions.CapScopePeer, ""); h.InvalidAt != 0 || !bytes.Equal(h.CapID, capID(6)) {
		t.Fatalf("the epoch-2 capability: %+v", h)
	}
	// A failed key set write leaves the capabilities alone.
	putHeld(t, s, held(peer, interactions.CapScopeIX, "ix-9", 7, 2, capsT0+capsDay))
	boom := errors.New("refused")
	if err := s.UpdatePeerIdentity(peer, func(cur *interactions.PeerIdentity) (*interactions.PeerIdentity, error) {
		return nil, boom
	}); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if h := heldRow(t, s, peer, interactions.CapScopeIX, "ix-9"); h.InvalidAt != 0 {
		t.Fatal("a refused key set write invalidated a capability")
	}
}

// The life of an issued capability: issued, renewed with the same cap_id,
// revoked by grantee, scope, class or cap_id, pushed, requeued, cleared.
func TestIssuedCapLifecycle(t *testing.T) {
	s := open(t)
	peerC := issued(granteeG, interactions.CapScopePeer, "", 1, seal.CapClassContact, capsT0+30*capsDay)
	peerS := issued(granteeG, interactions.CapScopePeer, "", 2, seal.CapClassStranger, capsT0+7*capsDay)
	one := issued(granteeG, interactions.CapScopeIX, "ix-1", 3, seal.CapClassStranger, capsT0+7*capsDay)
	other := issued(granteeH, interactions.CapScopePeer, "", 4, seal.CapClassContact, capsT0+30*capsDay)
	for _, c := range []interactions.IssuedCap{peerC, peerS, one, other} {
		putIssued(t, s, c)
	}
	if err := s.Update(func(tx *interactions.Tx) error { return tx.PutIssuedCap(peerC) }); err == nil {
		t.Fatal("the same cap_id was issued twice")
	}
	got, err := s.IssuedCap(capID(3))
	if err != nil || !reflect.DeepEqual(*got, one) {
		t.Fatalf("IssuedCap = %+v (%v), want %+v", got, err, one)
	}
	if _, err := s.IssuedCap(capID(99)); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatalf("unknown cap_id: %v", err)
	}

	// Renewal: only later, only unrevoked.
	renew := func(id byte, exp uint64) bool {
		var ok bool
		if err := s.Update(func(tx *interactions.Tx) error {
			var err error
			ok, err = tx.RenewIssuedCap(capID(id), exp)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !renew(1, capsT0+40*capsDay) || renew(1, capsT0+35*capsDay) {
		t.Fatal("a renewal must move cap_exp later and only later")
	}

	revoke := func(f interactions.IssuedFilter, at uint64) int64 {
		var n int64
		if err := s.Update(func(tx *interactions.Tx) error {
			var err error
			n, err = tx.RevokeIssued(f, at)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// Contact peer capability issued → the stranger one goes (SSR-D7).
	if n := revoke(interactions.IssuedFilter{Grantee: granteeG, Scope: interactions.CapScopePeer, Class: seal.CapClassStranger}, capsT0+10); n != 1 {
		t.Fatalf("revoked %d stranger peer capabilities, want 1", n)
	}
	if c, _ := s.IssuedCap(capID(2)); c.RevokedAt != capsT0+10 || c.RevokePushed {
		t.Fatalf("revoked row: %+v", c)
	}
	if c, _ := s.IssuedCap(capID(1)); c.RevokedAt != 0 {
		t.Fatal("the contact capability was revoked with the stranger one")
	}
	if renew(2, capsT0+50*capsDay) {
		t.Fatal("a revoked capability was renewed")
	}
	// The grantee rotates in an emergency: all of its capabilities go, the
	// already revoked one keeps its first revoked_at, another grantee's stay.
	var n int64
	if err := s.Update(func(tx *interactions.Tx) error {
		var err error
		n, err = tx.RevokeIssuedTo(granteeG, capsT0+20)
		return err
	}); err != nil || n != 2 {
		t.Fatalf("RevokeIssuedTo: n=%d err=%v, want 2", n, err)
	}
	for id, at := range map[byte]uint64{1: capsT0 + 20, 2: capsT0 + 10, 3: capsT0 + 20, 4: 0} {
		if c, _ := s.IssuedCap(capID(id)); c.RevokedAt != at {
			t.Errorf("cap %d: revoked_at=%d, want %d", id, c.RevokedAt, at)
		}
	}
	if err := s.Update(func(tx *interactions.Tx) error {
		_, err := tx.RevokeIssued(interactions.IssuedFilter{}, capsT0)
		return err
	}); !errors.Is(err, interactions.ErrBadInput) {
		t.Fatalf("a revocation without a grantee: %v", err)
	}
	// One capability by id (abuse).
	if err := s.Update(func(tx *interactions.Tx) error {
		if ok, err := tx.RevokeIssuedCap(capID(4), capsT0+30); !ok || err != nil {
			return fmt.Errorf("RevokeIssuedCap: %v %v", ok, err)
		}
		if ok, err := tx.RevokeIssuedCap(capID(4), capsT0+40); ok || err != nil {
			return fmt.Errorf("revoked twice: %v %v", ok, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// The push queue: oldest revocation first.
	q, err := s.UnpushedRevocations(10)
	if err != nil {
		t.Fatal(err)
	}
	wantQ := [][]byte{capID(2), capID(1), capID(3), capID(4)}
	if !reflect.DeepEqual(q, wantQ) {
		t.Fatalf("queue = %x, want %x", q, wantQ)
	}
	if at, ok, err := s.OldestUnpushedRevocation(); err != nil || !ok || at != capsT0+10 {
		t.Fatalf("oldest unpushed = %d %v %v", at, ok, err)
	}
	// How many waited since a cut, the cut included (doctor
	// sender_privacy.grants, SS-12).
	unpushedBefore := func(cut uint64, want int) {
		t.Helper()
		if n, err := s.UnpushedRevocationsBefore(cut); err != nil || n != want {
			t.Fatalf("UnpushedRevocationsBefore(T0+%d) = %d %v, want %d", cut-capsT0, n, err, want)
		}
	}
	unpushedBefore(capsT0+9, 0)
	unpushedBefore(capsT0+10, 1)
	unpushedBefore(capsT0+20, 3)
	unpushedBefore(capsT0+30, 4)
	if n, err := s.MarkRevocationsPushed([][]byte{capID(2), capID(1), capID(77)}); err != nil || n != 2 {
		t.Fatalf("MarkRevocationsPushed: %d %v", n, err)
	}
	unpushedBefore(capsT0+20, 1)
	unpushedBefore(capsT0+30, 2)
	if _, err := s.UnpushedRevocationsBefore(1 << 63); !errors.Is(err, interactions.ErrBadInput) {
		t.Fatalf("a cut past the store's integers: %v", err)
	}
	if q, _ := s.UnpushedRevocations(10); !reflect.DeepEqual(q, [][]byte{capID(3), capID(4)}) {
		t.Fatalf("queue after a push = %x", q)
	}
	if k, err := s.RevocationsSince(capsT0 + 15); err != nil || k != 3 {
		t.Fatalf("RevocationsSince = %d %v, want 3", k, err)
	}
	// The hub lost its table: everything since is pushed again (SSR-D21),
	// and nothing revoked before since.
	requeue := func(since uint64, want int64) {
		t.Helper()
		if err := s.Update(func(tx *interactions.Tx) error {
			n, err := tx.RequeueRevocations(since)
			if err == nil && n != want {
				err = fmt.Errorf("requeued %d since %d, want %d", n, since-capsT0, want)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	requeue(capsT0+15, 1) // cap 1 (revoked at +20); cap 2 (at +10) stays pushed
	if q, _ := s.UnpushedRevocations(10); !reflect.DeepEqual(q, [][]byte{capID(1), capID(3), capID(4)}) {
		t.Fatalf("queue after a requeue since +15 = %x", q)
	}
	requeue(capsT0, 1) // cap 2
	if q, _ := s.UnpushedRevocations(2); len(q) != 2 || !bytes.Equal(q[0], capID(2)) {
		t.Fatalf("limited queue after requeue = %x", q)
	}
	if _, issuedN, _ := s.CapsCounts(capsT0); issuedN != 0 {
		t.Fatalf("%d issued capabilities in force after revoking them all", issuedN)
	}

	// The writer refuses rows that make no sense.
	for name, c := range map[string]interactions.IssuedCap{
		"no grantee":     issued("", interactions.CapScopePeer, "", 9, 1, capsT0),
		"class 0":        issued(granteeG, interactions.CapScopePeer, "", 9, 0, capsT0),
		"one-shot no ix": issued(granteeG, interactions.CapScopeIX, "", 9, 1, capsT0),
		"revoked": func() interactions.IssuedCap {
			c := issued(granteeG, interactions.CapScopePeer, "", 9, 1, capsT0)
			c.RevokedAt = 1
			return c
		}(),
		"short seed": func() interactions.IssuedCap {
			c := issued(granteeG, interactions.CapScopePeer, "", 9, 1, capsT0)
			c.HolderSeed = []byte{1}
			return c
		}(),
	} {
		if err := s.Update(func(tx *interactions.Tx) error { return tx.PutIssuedCap(c) }); !errors.Is(err, interactions.ErrBadInput) {
			t.Errorf("%s: %v, want ErrBadInput", name, err)
		}
	}

	// A new mailbox key: the table goes, in the caller's transaction.
	if err := s.Update(func(tx *interactions.Tx) error {
		n, err := tx.ClearCapsIssued()
		if err == nil && n != 4 {
			err = fmt.Errorf("cleared %d, want 4", n)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if k, _ := s.CountRows("caps_issued", ""); k != 0 {
		t.Fatalf("%d rows left", k)
	}
}

// TestCapsPruning (SSR-D2): each rule at its boundary, and the bound of
// MaxHeldCaps held rows, which evicts peer capabilities by earliest cap_exp
// and never a one-shot one.
func TestCapsPruning(t *testing.T) {
	s := open(t)
	now := capsT0
	// caps_issued
	putIssued(t, s, issued(granteeG, interactions.CapScopePeer, "", 1, 1, now-1))       // expired: goes
	putIssued(t, s, issued(granteeG, interactions.CapScopeIX, "ix", 2, 2, now))         // expires now: stays
	putIssued(t, s, issued(granteeH, interactions.CapScopePeer, "", 3, 1, now-capsDay)) // revoked 31 days ago: goes
	putIssued(t, s, issued(granteeH, interactions.CapScopeIX, "ix", 4, 2, now-capsDay)) // revoked just under 31 days: stays
	revokeAt := map[byte]uint64{3: now - interactions.IssuedRevokedKeepMS, 4: now - interactions.IssuedRevokedKeepMS + 1}
	if err := s.Update(func(tx *interactions.Tx) error {
		for id, at := range revokeAt {
			if ok, err := tx.RevokeIssuedCap(capID(id), at); !ok || err != nil {
				return fmt.Errorf("revoke %d: %v %v", id, ok, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// caps_held
	putHeld(t, s, held(issuerA, interactions.CapScopePeer, "", 11, 1, now-1))         // expired: goes
	putHeld(t, s, held(issuerA, interactions.CapScopeIX, "ix-a", 12, 1, now))         // expires now: stays
	putHeld(t, s, held(issuerB, interactions.CapScopePeer, "", 13, 1, now+capsDay))   // invalid a day ago: goes
	putHeld(t, s, held(issuerB, interactions.CapScopeIX, "ix-b", 14, 1, now+capsDay)) // invalid just under a day: stays
	putHeld(t, s, held(granteeG, interactions.CapScopePeer, "", 15, 1, now+capsDay))  // valid: stays
	invalidAt := map[[2]string]uint64{
		{interactions.CapScopePeer, ""}:   now - interactions.HeldInvalidKeepMS,
		{interactions.CapScopeIX, "ix-b"}: now - interactions.HeldInvalidKeepMS + 1,
	}
	if err := s.Update(func(tx *interactions.Tx) error {
		for k, at := range invalidAt {
			if ok, err := tx.MarkHeldInvalid(issuerB, k[0], k[1], nil, at); !ok || err != nil {
				return fmt.Errorf("invalidate %v: %v %v", k, ok, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	p, err := s.PruneCaps(now)
	if err != nil {
		t.Fatal(err)
	}
	want := interactions.CapsPruned{IssuedExpired: 1, IssuedRevoked: 1, HeldExpired: 1, HeldInvalid: 1}
	if p != want {
		t.Fatalf("pruned %+v, want %+v", p, want)
	}
	for id, gone := range map[byte]bool{1: true, 2: false, 3: true, 4: false} {
		_, err := s.IssuedCap(capID(id))
		if gone != errors.Is(err, interactions.ErrNotFound) {
			t.Errorf("issued %d: gone=%v (%v)", id, !gone, err)
		}
	}
	for _, k := range []struct {
		issuer, scope, ix string
		gone              bool
	}{
		{issuerA, interactions.CapScopePeer, "", true},
		{issuerA, interactions.CapScopeIX, "ix-a", false},
		{issuerB, interactions.CapScopePeer, "", true},
		{issuerB, interactions.CapScopeIX, "ix-b", false},
		{granteeG, interactions.CapScopePeer, "", false},
	} {
		_, err := s.HeldCap(k.issuer, k.scope, k.ix)
		if k.gone != errors.Is(err, interactions.ErrNotFound) {
			t.Errorf("held %s %s %q: want gone=%v, got %v", k.issuer, k.scope, k.ix, k.gone, err)
		}
	}

	// The bound. Bulk rows: MaxHeldCaps + 50 peer capabilities whose cap_exp
	// is now + 1 day + i, and 100 one-shot ones expiring before all of them.
	extra := 50
	if err := s.ExecRaw(`DELETE FROM caps_held`); err != nil {
		t.Fatal(err)
	}
	insertBulk := func(scope string, from, count int, expBase uint64) {
		t.Helper()
		if err := s.ExecRaw(`WITH RECURSIVE c(i) AS (SELECT ? UNION ALL SELECT i + 1 FROM c WHERE i < ?)
			INSERT INTO caps_held(issuer, scope, ix, cap_id, cap, holder_seed, class, epoch, cap_exp, home, received_at)
			SELECT printf('did:anet:bulk-%d', i), ?, CASE WHEN ? = 'ix' THEN printf('ix-%d', i) ELSE '' END,
			  CAST(printf('%016d', i) AS BLOB), X'01', zeroblob(32), 1, 1, ? + i, 'h', 0 FROM c`,
			from, from+count-1, scope, scope, int64(expBase)); err != nil {
			t.Fatal(err)
		}
	}
	insertBulk("ix", 0, 100, now+capsDay-1000)
	insertBulk("peer", 1000, interactions.MaxHeldCaps-100+extra, now+capsDay)
	if k, _ := s.CountRows("caps_held", ""); k != interactions.MaxHeldCaps+extra {
		t.Fatalf("set-up: %d rows", k)
	}
	p, err = s.PruneCaps(now)
	if err != nil {
		t.Fatal(err)
	}
	if p.HeldEvicted != int64(extra) {
		t.Fatalf("evicted %d, want %d", p.HeldEvicted, extra)
	}
	if k, _ := s.CountRows("caps_held", ""); k != interactions.MaxHeldCaps {
		t.Fatalf("%d rows after the bound, want %d", k, interactions.MaxHeldCaps)
	}
	if k, _ := s.CountRows("caps_held", "scope = 'ix'"); k != 100 {
		t.Fatalf("%d one-shot rows left, want all 100", k)
	}
	// The evicted ones are the peer rows with the earliest cap_exp.
	if k, _ := s.CountRows("caps_held", "scope = 'peer' AND cap_exp < ?", int64(now+capsDay+1000+uint64(extra))); k != 0 {
		t.Fatalf("%d of the earliest peer rows survived the bound", k)
	}
	if k, _ := s.CountRows("caps_held", "scope = 'peer' AND cap_exp = ?", int64(now+capsDay+1000+uint64(extra))); k != 1 {
		t.Fatal("the bound evicted more than the earliest peer rows")
	}
	// At the bound nothing more goes.
	if p, err := s.PruneCaps(now); err != nil || p != (interactions.CapsPruned{}) {
		t.Fatalf("a second prune at the bound: %+v %v", p, err)
	}
}

// The tables round-trip their rows exactly.
func TestCapsRoundTrip(t *testing.T) {
	s := open(t)
	h := held(issuerA, interactions.CapScopeIX, "ix-rt", 5, 4, capsT0+capsDay)
	h.Class, h.P2P = seal.CapClassStranger, ""
	putHeld(t, s, h)
	got := heldRow(t, s, issuerA, interactions.CapScopeIX, "ix-rt")
	if !reflect.DeepEqual(*got, h) {
		t.Fatalf("held row read back as %+v, want %+v", *got, h)
	}
	var keys []string
	for _, c := range []interactions.IssuedCap{
		issued(granteeG, interactions.CapScopePeer, "", 7, 1, capsT0),
		issued(granteeG, interactions.CapScopeIX, "ix-z", 8, 2, capsT0),
	} {
		putIssued(t, s, c)
		keys = append(keys, fmt.Sprintf("%x", c.CapID))
	}
	all, err := s.IssuedCaps(granteeG)
	if err != nil || len(all) != 2 {
		t.Fatalf("IssuedCaps = %+v (%v)", all, err)
	}
	var gotKeys []string
	for _, c := range all {
		gotKeys = append(gotKeys, fmt.Sprintf("%x", c.CapID))
	}
	sort.Strings(keys)
	sort.Strings(gotKeys)
	if !reflect.DeepEqual(keys, gotKeys) {
		t.Fatalf("IssuedCaps ids %v, want %v", gotKeys, keys)
	}
}
