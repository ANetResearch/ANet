package daemon

// caps_test.go: the delivery credential policy as pure functions (SS
// §4.3.2, §4.3.3, SSR-D11, §4.3.8 item 2, §4.3.10): P-SS-6 over the whole
// input space of grantFor and accessFor, P-SS-7 over random refusal
// sequences, and the table tests of capMisuse, grantMisuse and
// p2pRxBucket.

import (
	"bytes"
	"crypto/ed25519"
	"fmt"
	"math/rand/v2"
	"sync/atomic"
	"testing"

	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

const (
	tNow   uint64 = 1_800_000_000_000
	tPeer         = "peer-aid-0001"
	tOther        = "other-aid-0002"
)

// grantOracle is SS §4.3.2 written out as its table, one row at a time,
// for P-SS-6: the first row whose condition holds decides.
func grantOracle(msg capMsg, rel capRel) (rule int, scope string, class, life uint64, p2p string, pay bool) {
	none := func(r int) (int, string, uint64, uint64, string, bool) { return r, "", 0, 0, "", false }
	if msg.Kind == capMsgCardGet {
		if msg.IX == "" {
			return none(0)
		}
		return 0, "ix", 1, 15 * 60 * 1000, "", false
	}
	if msg.Kind == capMsgPreview || msg.Kind == capMsgNotice || msg.Kind == capMsgCard {
		return none(1)
	}
	if rel.Denied {
		return none(ruleDenied)
	}
	if rel.Role == capRoleUnaccepted {
		return none(2)
	}
	week := uint64(7 * 24 * 3600 * 1000)
	if rel.MaybePaid {
		c := uint64(2)
		if rel.Listed || rel.Role == capRoleRequester {
			c = 1
		}
		if c == 2 && rel.StrangerOneShots >= 8 || msg.IX == "" {
			return none(3)
		}
		return 3, "ix", c, week, "", false
	}
	if rel.Role == capRoleRequester && !rel.Listed {
		if msg.IX == "" {
			return none(4)
		}
		return 4, "ix", 1, week, "", false
	}
	if rel.Listed {
		return 5, "peer", 1, 30 * 24 * 3600 * 1000, rel.P2P, rel.Prices
	}
	if rel.Role == capRoleProvider {
		return 5, "peer", 2, week, "", rel.Prices
	}
	return none(5)
}

// P-SS-6, the issuing half: every combination of message kind, relation,
// role, possible payment, stranger one-shots held, p2p address and prices
// gives the cell of SS §4.3.2; and across all of them a preview frame, a
// notice and anet.card/1 never carry a grant, a possibly paid interaction
// never gets a peer capability, p2p is only in a listed peer's contact peer
// capability and pay only in a peer capability.
func TestGrantForCoversTheTable(t *testing.T) {
	kinds := []capMsgKind{capMsgOther, capMsgDelegate, capMsgNotice, capMsgPreview, capMsgCardGet, capMsgCard}
	roles := []capRole{capRoleNone, capRoleRequester, capRoleUnaccepted, capRoleProvider}
	cells := 0
	for _, kind := range kinds {
		for _, ix := range []string{"ix-1", ""} {
			for _, listed := range []bool{false, true} {
				for _, denied := range []bool{false, true} {
					for _, role := range roles {
						for _, paid := range []bool{false, true} {
							for _, ones := range []int{0, 7, 8, 9} {
								for _, p2p := range []string{"", "203.0.113.7:4001"} {
									for _, prices := range []bool{false, true} {
										msg := capMsg{Kind: kind, IX: ix}
										rel := capRel{Listed: listed && !denied, Denied: denied, Role: role, MaybePaid: paid,
											StrangerOneShots: ones, P2P: p2p, Prices: prices}
										got := grantFor(msg, rel, tNow)
										rule, scope, class, life, wp2p, wpay := grantOracle(msg, rel)
										cells++
										name := fmt.Sprintf("%+v %+v", msg, rel)
										if got.Rule != rule || got.Issue != (scope != "") {
											t.Fatalf("%s: rule %d issue %v, want rule %d issue %v", name, got.Rule, got.Issue, rule, scope != "")
										}
										if !got.Issue {
											continue
										}
										if got.Scope != scope || got.Class != class || got.CapExp != tNow+life || got.P2P != wp2p || got.Pay != wpay {
											t.Fatalf("%s: %+v, want scope %s class %d life %d p2p %q pay %v", name, got, scope, class, life, wp2p, wpay)
										}
										if scope == "ix" && got.IX != ix || scope == "peer" && got.IX != "" {
											t.Fatalf("%s: ix %q", name, got.IX)
										}
										// The invariants, independent of the table.
										switch {
										case kind == capMsgPreview || kind == capMsgNotice || kind == capMsgCard:
											t.Fatalf("%s: a grant on a %d", name, kind)
										case paid && kind != capMsgCardGet && got.Scope == interactions.CapScopePeer:
											t.Fatalf("%s: a peer capability inside a possibly paid interaction", name)
										case got.P2P != "" && (got.Scope != interactions.CapScopePeer || got.Class != seal.CapClassContact || !rel.Listed):
											t.Fatalf("%s: p2p outside a listed peer's contact peer capability", name)
										case got.Pay && got.Scope != interactions.CapScopePeer:
											t.Fatalf("%s: pay on a one-shot capability", name)
										case denied && kind != capMsgCardGet:
											t.Fatalf("%s: a grant to a denied peer", name)
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}
	if cells != 6*2*2*2*4*2*4*2*2 {
		t.Fatalf("%d cells", cells)
	}
}

// M18: the three after-the-fact notices and anet.card/1 never carry a
// grant, whoever the peer is and whatever the interaction.
func TestNoticesCarryNoGrant(t *testing.T) {
	for _, kind := range []capMsgKind{capMsgNotice, capMsgCard, capMsgPreview} {
		for _, rel := range []capRel{
			{Listed: true, Role: capRoleProvider, P2P: "203.0.113.7:4001", Prices: true},
			{Role: capRoleRequester},
			{Role: capRoleProvider, MaybePaid: true},
		} {
			if d := grantFor(capMsg{Kind: kind, IX: "ix-1"}, rel, tNow); d.Issue || d.Rule != 1 {
				t.Fatalf("kind %d to %+v: %+v", kind, rel, d)
			}
		}
	}
}

// heldCap is a caps_held row of issuer tPeer for the policy tests.
func heldCap(scope, ix string, id byte, opts ...func(*interactions.HeldCap)) interactions.HeldCap {
	h := interactions.HeldCap{Issuer: tPeer, Scope: scope, IX: ix, CapID: bytes.Repeat([]byte{id}, seal.CapIDLen),
		Cap: []byte{0xa0}, HolderSeed: bytes.Repeat([]byte{id}, 32), Class: 1, Epoch: 3, CapExp: tNow + capLifeStrangerMS,
		Home: "hub-aid-0003", ReceivedAt: tNow - 1000}
	for _, o := range opts {
		o(&h)
	}
	return h
}

// accessOracle is SS §4.3.3 and engine ENG-K2 as their tables.
func accessOracle(row accessRow, oneShot, peer *interactions.HeldCap, mode accessMode) (send bool, kind uint64, use *interactions.HeldCap) {
	switch mode {
	case accessNotice, accessPreview:
		switch {
		case oneShot != nil:
			return true, 1, oneShot
		case !row.MaybePaid && peer != nil:
			return true, 1, peer
		}
		return false, 0, nil
	case accessCardGet:
		if peer != nil && !peer.Pay {
			return true, 1, peer
		}
		return true, 0, nil
	case accessCard:
		if oneShot != nil {
			return true, 1, oneShot
		}
		return false, 0, nil
	}
	switch {
	case oneShot != nil:
		return true, 1, oneShot
	case row.Delegate:
		if peer != nil && !peer.Pay {
			return true, 1, peer
		}
		return true, 0, nil
	case row.MaybePaid:
		return true, 0, nil
	case peer != nil:
		return true, 1, peer
	}
	return true, 0, nil
}

// P-SS-6, the sending half: every mode, delegation or not, possibly paid or
// not, R's epoch known or not, with every kind of one-shot and peer
// capability held (valid, given up, expiring, under another epoch, of
// another interaction, from another issuer; pay or not) gives the cell of
// SS §4.3.3. And across all of them: a delegation never goes under a peer
// capability that says pay (M17), notices, previews and anet.card/1 never
// go as a first contact, and a kind-1 choice is a valid capability of R.
func TestAccessForCoversTheTable(t *testing.T) {
	type variant struct {
		name  string
		valid bool
		h     *interactions.HeldCap
	}
	mk := func(scope, ix string, id byte, opts ...func(*interactions.HeldCap)) *interactions.HeldCap {
		h := heldCap(scope, ix, id, opts...)
		return &h
	}
	oneShots := []variant{
		{"none", false, nil},
		{"valid", true, mk("ix", "ix-1", 1)},
		{"given-up", false, mk("ix", "ix-1", 2, func(h *interactions.HeldCap) { h.InvalidAt = tNow - 5 })},
		{"expiring", false, mk("ix", "ix-1", 3, func(h *interactions.HeldCap) { h.CapExp = tNow + 59_999 })},
		{"at-margin", true, mk("ix", "ix-1", 4, func(h *interactions.HeldCap) { h.CapExp = tNow + 60_000 })},
		{"old-epoch", false, mk("ix", "ix-1", 5, func(h *interactions.HeldCap) { h.Epoch = 2 })},
		{"other-ix", false, mk("ix", "ix-2", 6)},
		{"other-issuer", false, mk("ix", "ix-1", 7, func(h *interactions.HeldCap) { h.Issuer = tOther })},
	}
	peers := []variant{
		{"none", false, nil},
		{"valid", true, mk("peer", "", 11)},
		{"valid-pay", true, mk("peer", "", 12, func(h *interactions.HeldCap) { h.Pay = true })},
		{"given-up", false, mk("peer", "", 13, func(h *interactions.HeldCap) { h.InvalidAt = tNow })},
		{"expiring", false, mk("peer", "", 14, func(h *interactions.HeldCap) { h.CapExp = tNow + 1 })},
		{"old-epoch", false, mk("peer", "", 15, func(h *interactions.HeldCap) { h.Epoch = 1 })},
	}
	modes := []accessMode{accessOrdinary, accessNotice, accessPreview, accessCardGet, accessCard}
	cells := 0
	for _, mode := range modes {
		for _, del := range []bool{false, true} {
			for _, paid := range []bool{false, true} {
				for _, epoch := range []uint64{3, 0} {
					for _, os := range oneShots {
						for _, pv := range peers {
							row := accessRow{To: tPeer, IX: "ix-1", Delegate: del, MaybePaid: paid, PeerEpoch: epoch}
							var held []interactions.HeldCap
							if os.h != nil {
								held = append(held, *os.h)
							}
							if pv.h != nil {
								held = append(held, *pv.h)
							}
							// With R's epoch unknown, a capability under another
							// epoch is used (SS §4.3.3 checks the epoch only when
							// this node knows R's key set).
							valid := func(v variant) bool {
								return v.valid || epoch == 0 && v.h != nil && v.name == "old-epoch"
							}
							var one, peer *interactions.HeldCap
							if valid(os) {
								one = os.h
							}
							if valid(pv) {
								peer = pv.h
							}
							send, kind, use := accessOracle(row, one, peer, mode)
							got := accessFor(row, held, mode, tNow)
							cells++
							name := fmt.Sprintf("mode %d delegate %v paid %v epoch %d one-shot %s peer %s", mode, del, paid, epoch, os.name, pv.name)
							if got.Send != send || send && got.Kind != kind {
								t.Fatalf("%s: send %v kind %d, want send %v kind %d", name, got.Send, got.Kind, send, kind)
							}
							if kind == 1 && (got.Cap == nil || !bytes.Equal(got.Cap.CapID, use.CapID) || got.Home != use.Home) {
								t.Fatalf("%s: used %+v, want %x", name, got.Cap, use.CapID)
							}
							if got.Send && kind == 0 && got.Cap != nil {
								t.Fatalf("%s: a first contact names a capability", name)
							}
							// The invariants.
							if del && mode == accessOrdinary && got.Kind == 1 && got.Cap.Scope == "peer" && got.Cap.Pay {
								t.Fatalf("%s: a delegation under the peer capability of a provider that prices (M17)", name)
							}
							if (mode == accessNotice || mode == accessPreview || mode == accessCard) && got.Send && got.Kind == 0 {
								t.Fatalf("%s: sent as a first contact", name)
							}
							if got.Kind == 1 && got.Send && (got.Cap.Issuer != tPeer || got.Cap.InvalidAt != 0 || got.Cap.CapExp < tNow+capValidMarginMS) {
								t.Fatalf("%s: an invalid capability chosen", name)
							}
						}
					}
				}
			}
		}
	}
	if cells != 5*2*2*2*len(oneShots)*len(peers) {
		t.Fatalf("%d cells", cells)
	}
}

// accessFromChoice signs the proof for the envelope it goes on, and the
// access verifies under the issuer's mailbox key for that envelope only.
func TestAccessFromChoiceProvesForTheEnvelope(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	mbox := seal.Mbox{Epoch: 3, Pub: pub}
	seed := bytes.Repeat([]byte{9}, 32)
	holder := ed25519.NewKeyFromSeed(seed)
	id := bytes.Repeat([]byte{4}, seal.CapIDLen)
	c, err := seal.SignCap(priv, &seal.CapBody{Owner: tPeer, Epoch: 3, CapID: id, Class: 1, CapExp: tNow + capLifeContactMS,
		Holder: holder.Public().(ed25519.PublicKey)})
	if err != nil {
		t.Fatal(err)
	}
	cb, err := c.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	h := heldCap("peer", "", 4, func(h *interactions.HeldCap) { h.Cap, h.HolderSeed, h.CapID = cb, seed, id })
	ch := accessFor(accessRow{To: tPeer, PeerEpoch: 3}, []interactions.HeldCap{h}, accessOrdinary, tNow)
	digest := [32]byte{1, 2, 3}
	a, err := accessFromChoice(ch, digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := seal.VerifyAccess(a, digest, mbox, tPeer, tNow); err != nil {
		t.Fatalf("the access does not verify: %v", err)
	}
	if seal.VerifyAccess(a, [32]byte{9}, mbox, tPeer, tNow) == nil {
		t.Fatal("the access verifies for another envelope")
	}
	if a0, err := accessFromChoice(AccessChoice{Send: true, Kind: seal.AccessKindSession}, digest); err != nil || a0.Kind != 0 || a0.Cap != nil {
		t.Fatalf("a first contact: %+v %v", a0, err)
	}
	if _, err := accessFromChoice(AccessChoice{}, digest); err == nil {
		t.Fatal("a message with nothing to go under got a credential")
	}
}

// ---- refusals (SSR-D11)

// capsStore is an interactions store of its own for a test.
func capsStore(t *testing.T) *interactions.Store {
	t.Helper()
	s, err := interactions.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// refusalRig is one held capability in a store, refused and accepted the
// way the engine does it (accessRefusedTx, accessAcceptedTx).
type refusalRig struct {
	t *testing.T
	s *interactions.Store
	h interactions.HeldCap
}

func newRefusalRig(t *testing.T) *refusalRig { return newRefusalRigIn(t, capsStore(t), tPeer) }

// newRefusalRigIn is a rig in store s whose capability is issuer's, so one
// store serves many sequences.
func newRefusalRigIn(t *testing.T, s *interactions.Store, issuer string) *refusalRig {
	h := heldCap("peer", "", 1)
	h.Issuer = issuer
	r := &refusalRig{t: t, s: s, h: h}
	r.put(r.h)
	return r
}

func (r *refusalRig) put(h interactions.HeldCap) bool {
	r.t.Helper()
	var ok bool
	if err := r.s.Update(func(tx *interactions.Tx) error {
		var err error
		ok, err = tx.PutHeldGrant(h)
		return err
	}); err != nil {
		r.t.Fatal(err)
	}
	return ok
}

func (r *refusalRig) refuse(review peerReview, now uint64) RefusalDecision {
	r.t.Helper()
	var dec RefusalDecision
	if err := r.s.Update(func(tx *interactions.Tx) error {
		var err error
		dec, err = accessRefusedTx(tx, r.h.Issuer, r.h.Scope, r.h.IX, r.h.CapID, review, now)
		return err
	}); err != nil {
		r.t.Fatal(err)
	}
	return dec
}

func (r *refusalRig) accept() {
	r.t.Helper()
	if err := r.s.Update(func(tx *interactions.Tx) error {
		return accessAcceptedTx(tx, r.h.Issuer, r.h.Scope, r.h.IX, r.h.CapID)
	}); err != nil {
		r.t.Fatal(err)
	}
}

func (r *refusalRig) row() *interactions.HeldCap {
	r.t.Helper()
	h, err := r.s.HeldCap(r.h.Issuer, r.h.Scope, r.h.IX)
	if err != nil {
		r.t.Fatal(err)
	}
	return h
}

// chosen reports whether the next attempt goes under the rig's capability.
func (r *refusalRig) chosen(now uint64) bool {
	held, err := r.s.HeldCaps(r.h.Issuer)
	if err != nil {
		r.t.Fatal(err)
	}
	c := accessFor(accessRow{To: r.h.Issuer, PeerEpoch: r.h.Epoch}, held, accessOrdinary, now)
	return c.Kind == seal.AccessKindCap && c.Cap != nil && bytes.Equal(c.Cap.CapID, r.h.CapID)
}

var (
	reviewSame    = peerReview{Verdict: reviewFound, Epoch: 3}
	reviewHigher  = peerReview{Verdict: reviewFound, Epoch: 4}
	reviewHidden  = peerReview{Verdict: reviewNotFound}
	reviewBadSet  = peerReview{Verdict: reviewUnverifiable}
	reviewNoHub   = peerReview{Verdict: reviewUnavailable}
	reviewDropped = peerReview{Verdict: reviewFound, Epoch: 2} // a hub serving an older set
)

const minute uint64 = 60 * 1000

// M40, SI-86: one 403 with R's epoch unchanged does not give the
// capability up. The attempt waits (credential_refused) no later than ten
// minutes after the first refusal, and the next attempt still goes under
// the capability.
func TestSingleRefusalDoesNotDowngrade(t *testing.T) {
	r := newRefusalRig(t)
	dec := r.refuse(reviewSame, tNow)
	if dec.Action != refusalWait || dec.Reselect() || dec.WaitCode() != "credential_refused" ||
		dec.RefusedN != 1 || dec.RefusedSince != tNow || dec.RetryBy != tNow+10*minute {
		t.Fatalf("one refusal: %+v", dec)
	}
	if h := r.row(); h.InvalidAt != 0 || h.RefusedN != 1 || h.RefusedSince != tNow {
		t.Fatalf("the row after one refusal: %+v", h)
	}
	if !r.chosen(tNow + 1) {
		t.Fatal("the capability is not used after one refusal")
	}
	// Three refusals within ten minutes are still a wait.
	r.refuse(reviewSame, tNow+4*minute)
	dec = r.refuse(reviewSame, tNow+9*minute)
	if dec.Action != refusalWait || dec.RefusedN != 3 || dec.RetryBy != tNow+10*minute || r.row().InvalidAt != 0 {
		t.Fatalf("three refusals in nine minutes: %+v", dec)
	}
	// The fourth, ten minutes after the first, gives it up.
	dec = r.refuse(reviewSame, tNow+10*minute)
	if dec.Action != refusalDowngrade || !dec.Reselect() || r.row().InvalidAt != tNow+10*minute || r.chosen(tNow+10*minute) {
		t.Fatalf("at the threshold: %+v, row %+v", dec, r.row())
	}
	// Ten minutes alone are not enough either: two refusals ten minutes apart wait.
	r2 := newRefusalRig(t)
	r2.refuse(reviewSame, tNow)
	if dec := r2.refuse(reviewSame, tNow+20*minute); dec.Action != refusalWait {
		t.Fatalf("two refusals twenty minutes apart: %+v", dec)
	}
}

// M41: what a hub can fake explains nothing. Not found, unverifiable,
// unavailable and an older epoch are unexplained refusals, counted towards
// the threshold; the capability is not given up before it.
func TestHiddenEntryDoesNotExplainRefusal(t *testing.T) {
	for _, rv := range []peerReview{reviewHidden, reviewBadSet, reviewNoHub, reviewDropped, reviewSame} {
		r := newRefusalRig(t)
		for i, at := range []uint64{tNow, tNow + minute, tNow + 2*minute} {
			dec := r.refuse(rv, at)
			if dec.Action != refusalWait || dec.RefusedN != uint64(i+1) || r.row().InvalidAt != 0 || !r.chosen(at) {
				t.Fatalf("review %+v, refusal %d: %+v", rv, i+1, dec)
			}
		}
		if dec := r.refuse(rv, tNow+10*minute); dec.Action != refusalDowngrade {
			t.Fatalf("review %+v at the threshold: %+v", rv, dec)
		}
	}
}

// What R signed explains a refusal at once: a verified key set with a
// higher mailbox epoch gives the capability up on the first 403, without a
// failure counted (L5, L13). A capability about to expire is chosen again
// at once without being given up, so the issuer's renewal under the same
// cap_id is still taken.
func TestEpochRiseExplainsRefusal(t *testing.T) {
	r := newRefusalRig(t)
	dec := r.refuse(reviewHigher, tNow)
	if dec.Action != refusalInvalidate || !dec.Reselect() || dec.WaitCode() != "" {
		t.Fatalf("a higher epoch: %+v", dec)
	}
	if h := r.row(); h.InvalidAt != tNow || h.RefusedN != 0 {
		t.Fatalf("the row: %+v", h)
	}
	if r.chosen(tNow) {
		t.Fatal("a capability under an older epoch is still chosen")
	}

	r = newRefusalRig(t)
	exp := r.h.CapExp
	at := exp - capValidMarginMS + 1 // less than a minute left
	dec = r.refuse(reviewSame, at)
	if dec.Action != refusalReselect || !dec.Reselect() {
		t.Fatalf("an expiring capability: %+v", dec)
	}
	if h := r.row(); h.InvalidAt != 0 || h.RefusedN != 0 {
		t.Fatalf("an expiring capability was marked: %+v", h)
	}
	if r.chosen(at) {
		t.Fatal("an expiring capability is still chosen")
	}
	renewed := r.h
	renewed.CapExp = exp + capLifeContactMS
	if !r.put(renewed) || r.row().CapExp != renewed.CapExp || !r.chosen(at) {
		t.Fatal("the issuer's renewal under the same cap_id was not taken")
	}
	// A capability already given up is chosen again, nothing counted.
	r = newRefusalRig(t)
	r.refuse(reviewHigher, tNow)
	if dec := r.refuse(reviewSame, tNow+1); dec.Action != refusalReselect || r.row().RefusedN != 0 {
		t.Fatalf("a refusal of a given-up capability: %+v", dec)
	}
}

// SSR-D11 item 4: one acceptance forgets the refusals, so the threshold
// counts from the next one.
func TestRefusalCounterResetsOnAccept(t *testing.T) {
	r := newRefusalRig(t)
	r.refuse(reviewSame, tNow)
	r.refuse(reviewSame, tNow+minute)
	r.accept()
	if h := r.row(); h.RefusedN != 0 || h.RefusedSince != 0 {
		t.Fatalf("after an acceptance: %+v", h)
	}
	// Two more, eleven minutes after the first of the old run: no downgrade.
	r.refuse(reviewSame, tNow+11*minute)
	if dec := r.refuse(reviewSame, tNow+12*minute); dec.Action != refusalWait || dec.RefusedN != 2 || dec.RefusedSince != tNow+11*minute {
		t.Fatalf("after the reset: %+v", dec)
	}
	// An acceptance of another capability than the row's does not reset it.
	other := r.h
	other.CapID = bytes.Repeat([]byte{0xee}, seal.CapIDLen)
	if err := r.s.Update(func(tx *interactions.Tx) error {
		return accessAcceptedTx(tx, other.Issuer, other.Scope, other.IX, other.CapID)
	}); err != nil {
		t.Fatal(err)
	}
	if r.row().RefusedN != 2 {
		t.Fatal("an acceptance of another capability reset the count")
	}
}

// SSR-D11 item 5: a grant with the cap_id of a capability given up does not
// bring it back (M41); a grant with a new cap_id replaces it, counters and
// all.
func TestSameCapIDGrantDoesNotRevive(t *testing.T) {
	r := newRefusalRig(t)
	for _, at := range []uint64{tNow, tNow + minute, tNow + 2*minute, tNow + 10*minute} {
		r.refuse(reviewSame, at)
	}
	if r.row().InvalidAt == 0 {
		t.Fatal("not downgraded")
	}
	again := r.h
	again.CapExp += capLifeContactMS
	again.ReceivedAt = tNow + 11*minute
	if r.put(again) || r.row().InvalidAt == 0 || r.chosen(tNow+11*minute) {
		t.Fatal("the same cap_id brought the capability back")
	}
	fresh := heldCap("peer", "", 2)
	if !r.put(fresh) {
		t.Fatal("a new cap_id was not taken")
	}
	r.h = fresh
	if h := r.row(); h.InvalidAt != 0 || h.RefusedN != 0 || h.RefusedSince != 0 || !r.chosen(tNow+11*minute) {
		t.Fatalf("the new capability: %+v", h)
	}
}

// SSR-U2: /status.sender_privacy.refusal_downgrades_24h is the number of
// downgrades of the last 24 hours, and only of downgrades; the counters
// tell the three outcomes apart.
func TestRefusalDowngradesOfTheLastDay(t *testing.T) {
	d := newTestDaemon(t, "", false)
	hour := uint64(3600 * 1000)
	d.noteRefusal(RefusalDecision{Action: refusalWait}, tNow)
	d.noteRefusal(RefusalDecision{Action: refusalInvalidate}, tNow)
	d.noteRefusal(RefusalDecision{Action: refusalReselect}, tNow)
	if n := d.refusalDowngrades24h(tNow); n != 0 {
		t.Fatalf("%d downgrades without one", n)
	}
	d.noteRefusal(RefusalDecision{Action: refusalDowngrade}, tNow)
	d.noteRefusal(RefusalDecision{Action: refusalDowngrade}, tNow+hour)
	for _, c := range []struct {
		at   uint64
		want int
	}{{tNow + hour, 2}, {tNow + 24*hour - 1, 2}, {tNow + 24*hour, 1}, {tNow + 25*hour, 0}} {
		if n := d.refusalDowngrades24h(c.at); n != c.want {
			t.Fatalf("at +%dms: %d downgrades, want %d", c.at-tNow, n, c.want)
		}
	}
	if u, g, e := counter(d, cntRefusalUnexplained), counter(d, cntRefusalDowngrade), counter(d, cntRefusalExplained); u != 3 || g != 2 || e != 2 {
		t.Fatalf("counters: unexplained %d, downgrade %d, explained %d", u, g, e)
	}
}

// A refusal of a capability the row no longer holds (a grant replaced it
// while the send was out) counts against nothing.
func TestRefusalOfAReplacedCapabilityCountsNothing(t *testing.T) {
	r := newRefusalRig(t)
	used := r.h
	r.put(heldCap("peer", "", 2))
	r.h = used
	if dec := r.refuse(reviewSame, tNow); dec.Action != refusalReselect {
		t.Fatalf("%+v", dec)
	}
	h, _ := r.s.HeldCap(used.Issuer, used.Scope, used.IX)
	if h.RefusedN != 0 || h.InvalidAt != 0 {
		t.Fatalf("the new capability was charged: %+v", h)
	}
}

// refusalEvent is one step of a P-SS-7 sequence.
type refusalEvent uint8

const (
	evRefuseSame refusalEvent = iota
	evRefuseHigher
	evRefuseHidden
	evRefuseBadSet
	evRefuseNoHub
	evAccept
	evWait
	evWaitLong
	evGrantSame
	evGrantNew
	evCount
)

// refusalModel is SSR-D11 as P-SS-7 states it, kept beside the store.
type refusalModel struct {
	invalid       bool
	n, since      uint64
	explainedOnly bool // given up because of a verified higher epoch
}

// runRefusalSequence plays events against a fresh rig and the model and
// checks P-SS-7 after each: the capability is given up exactly when a
// verified key set showed a higher epoch, or when it was refused without
// an explanation 3 times since its last acceptance with the first of those
// 10 minutes ago; a grant with the same cap_id never takes the given-up
// state back; after any other refusal the capability is still chosen.
func runRefusalSequence(t *testing.T, s *interactions.Store, issuer string, events []refusalEvent) {
	r := newRefusalRigIn(t, s, issuer)
	var m refusalModel
	now := tNow
	epoch := uint64(3)
	for i, ev := range events {
		step := fmt.Sprintf("event %d (%d) of %v", i, ev, events)
		review := peerReview{Verdict: reviewFound, Epoch: epoch}
		switch ev {
		case evRefuseSame, evRefuseHigher, evRefuseHidden, evRefuseBadSet, evRefuseNoHub:
			switch ev {
			case evRefuseHigher:
				review.Epoch = epoch + 1
			case evRefuseHidden:
				review = reviewHidden
			case evRefuseBadSet:
				review = reviewBadSet
			case evRefuseNoHub:
				review = reviewNoHub
			}
			dec := r.refuse(review, now)
			switch {
			case m.invalid:
				if dec.Action != refusalReselect {
					t.Fatalf("%s: a given-up capability: %+v", step, dec)
				}
			case ev == evRefuseHigher:
				if dec.Action != refusalInvalidate {
					t.Fatalf("%s: a higher epoch: %+v", step, dec)
				}
				m.invalid, m.explainedOnly = true, true
			default:
				m.n++
				if m.since == 0 {
					m.since = now
				}
				want := refusalWait
				if m.n >= 3 && now-m.since >= 10*minute {
					want, m.invalid = refusalDowngrade, true
				}
				if dec.Action != want || dec.RefusedN != m.n || dec.RefusedSince != m.since {
					t.Fatalf("%s: %+v, want action %d n %d since %d", step, dec, want, m.n, m.since)
				}
			}
		case evAccept:
			r.accept()
			m.n, m.since = 0, 0
		case evWait:
			now += 3 * minute
		case evWaitLong:
			now += 11 * minute
		case evGrantSame:
			g := r.h
			g.CapExp = now + capLifeStrangerMS
			r.put(g)
		case evGrantNew:
			g := heldCap("peer", "", byte(i+10))
			g.Issuer, g.CapExp = issuer, now+capLifeStrangerMS
			r.put(g)
			r.h = g
			m = refusalModel{}
		}
		h := r.row()
		if (h.InvalidAt != 0) != m.invalid {
			t.Fatalf("%s: invalid_at %d, model given up %v", step, h.InvalidAt, m.invalid)
		}
		if !m.invalid && h.CapExp >= now+capValidMarginMS && !r.chosen(now) {
			t.Fatalf("%s: a capability not given up is not chosen", step)
		}
		if m.invalid && r.chosen(now) {
			t.Fatalf("%s: a given-up capability is chosen", step)
		}
	}
}

// P-SS-7 (SI-86) over seeded random sequences.
func TestRefusalSequences(t *testing.T) {
	s := capsStore(t)
	rng := rand.New(rand.NewPCG(86, 11))
	for seq := 0; seq < 400; seq++ {
		events := make([]refusalEvent, 1+rng.IntN(24))
		for i := range events {
			events[i] = refusalEvent(rng.IntN(int(evCount)))
		}
		runRefusalSequence(t, s, fmt.Sprintf("seq-peer-%04d", seq), events)
	}
}

// FuzzRefusalSequence is P-SS-7 under the fuzzer: every byte is an event.
func FuzzRefusalSequence(f *testing.F) {
	f.Add([]byte{0, 6, 0, 6, 0, 7, 0})
	f.Add([]byte{0, 0, 5, 7, 0, 0, 0, 8, 1})
	f.Add([]byte{2, 3, 4, 7, 2, 9, 0})
	store, err := interactions.Open(f.TempDir())
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { store.Close() })
	var runs atomic.Int64
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > 40 {
			b = b[:40]
		}
		events := make([]refusalEvent, len(b))
		for i, c := range b {
			events[i] = refusalEvent(c % byte(evCount))
		}
		issuer := fmt.Sprintf("fuzz-peer-%08d", runs.Add(1))
		s := store
		runRefusalSequence(t, s, issuer, events)
	})
}

// ---- misuse

// capMisuse, table-driven (M14): every reason SS §4.3.8 item 2 names
// revokes at the steps it can occur in, and only from step 3 to step 7;
// every benign reason and every temporary one never revokes.
func TestCapMisuseTable(t *testing.T) {
	misuse := []string{"decrypt-failed", "bad-wrap", "bad-rk", "ikey-mismatch", "bad-inner", "unknown-critical-field",
		"to-mismatch", "exp-mismatch", "bad-kind-keys", "kel-fork", "bad-kel", "witness-mismatch", "witness-invalid",
		"from-mismatch", "bad-sig", "bad-key-state", "revoked-key", "rotation-grace-expired"}
	benign := []string{"sealed-to-unknown-key", "sealed-to-closed-interaction", "expired", "from-future", "bad-lifetime",
		"kel-from-future", "witness-missing", "witness-stale", "witness-unknown",
		"t-store", "t-p2p-rate-limit", "t-not-ready", "t-replay-read", "refused-replay", "duplicate", "rk-mismatch",
		"not-peer", "bad-body", "wrong-recipient", "unknown-suite", "bad-version", "access-invalid", ""}
	if len(capMisuseReasons) != len(misuse) {
		t.Fatalf("%d misuse reasons, the spec names %d", len(capMisuseReasons), len(misuse))
	}
	for _, b := range capBenignReasons {
		if capMisuseReasons[b] {
			t.Fatalf("%s is both benign and misuse", b)
		}
	}
	for step := rxStep0; step <= rxStep10; step++ {
		inRange := step >= rxStep3 && step <= rxStep7
		for _, r := range misuse {
			if got := capMisuse(step, r); got != inRange {
				t.Errorf("capMisuse(step %d, %s) = %v", step, r, got)
			}
		}
		for _, r := range benign {
			if capMisuse(step, r) {
				t.Errorf("capMisuse(step %d, %s) revokes", step, r)
			}
		}
	}
	// The seal codes the table uses are the seal package's.
	for _, r := range []string{seal.ReasonDecrypt, seal.ReasonBadWrap, seal.ReasonBadRK, seal.ReasonIKeyMismatch,
		seal.ReasonBadInner, seal.ReasonUnknownField, seal.ReasonToMismatch, seal.ReasonExpMismatch, seal.ReasonBadKEL,
		seal.ReasonFromMismatch, seal.ReasonBadSig, seal.ReasonBadKeyState, seal.ReasonRevokedKey, seal.ReasonGraceExpired} {
		if !capMisuse(rxStep4, r) {
			t.Errorf("%s does not revoke", r)
		}
	}
}

// grantMisuse, table-driven (M32): a capability is misused when the
// authenticated sender is not its grantee, or when a one-shot capability
// comes in another interaction.
func TestGrantMisuseTable(t *testing.T) {
	peer := &interactions.IssuedCap{Grantee: tPeer, Scope: "peer"}
	one := &interactions.IssuedCap{Grantee: tPeer, Scope: "ix", IX: "ix-1"}
	for _, c := range []struct {
		name     string
		c        *interactions.IssuedCap
		from, ix string
		want     bool
	}{
		{"peer, grantee, any ix", peer, tPeer, "ix-9", false},
		{"peer, someone else", peer, tOther, "ix-9", true},
		{"one-shot, grantee, its ix", one, tPeer, "ix-1", false},
		{"one-shot, grantee, another ix", one, tPeer, "ix-2", true},
		{"one-shot, someone else, its ix", one, tOther, "ix-1", true},
		{"no capability", nil, tOther, "ix-1", false},
	} {
		if got := grantMisuse(c.c, c.from, c.ix); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}

// ---- the direct path

// p2pRig is a capability this node (self) issued under its mailbox key.
type p2pRig struct {
	self   string
	mbox   seal.Mbox
	priv   ed25519.PrivateKey
	row    interactions.IssuedCap
	access *seal.Access
	digest [32]byte
}

func newP2PRig(t testing.TB, class uint64) *p2pRig {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(nil)
	seed := bytes.Repeat([]byte{7}, 32)
	holder := ed25519.NewKeyFromSeed(seed)
	r := &p2pRig{self: "self-aid-0004", mbox: seal.Mbox{Epoch: 2, Pub: pub}, priv: priv, digest: [32]byte{5}}
	r.row = interactions.IssuedCap{CapID: bytes.Repeat([]byte{3}, seal.CapIDLen), Grantee: tPeer, Class: class, Scope: "peer",
		HolderSeed: seed, HolderPub: holder.Public().(ed25519.PublicKey), Epoch: 2, CapExp: tNow + capLifeStrangerMS, IssuedAt: tNow}
	c, err := seal.SignCap(priv, &seal.CapBody{Owner: r.self, Epoch: 2, CapID: r.row.CapID, Class: class,
		CapExp: r.row.CapExp, Holder: r.row.HolderPub})
	if err != nil {
		t.Fatal(err)
	}
	r.access = &seal.Access{Kind: seal.AccessKindCap, Cap: c, Proof: seal.SignAccess(holder, r.row.CapID, r.digest)}
	return r
}

// Step 0 on the direct path: a delivery under a capability this node
// issued, unrevoked, unexpired and proven for this envelope is charged to
// that capability's bucket; anything else to the shared one.
func TestP2PRxBucket(t *testing.T) {
	for _, class := range []uint64{seal.CapClassContact, seal.CapClassStranger} {
		r := newP2PRig(t, class)
		want := rxBucketKey{Class: class, CapID: string(r.row.CapID)}
		if got := p2pRxBucket(r.access, r.digest, &r.row, r.self, r.mbox, tNow); got != want {
			t.Fatalf("class %d: %+v", class, got)
		}
		if !bytes.Equal(capIDOf(r.access), r.row.CapID) {
			t.Fatal("capIDOf")
		}
		shared := rxBucketKey{}
		cases := map[string]func() rxBucketKey{
			"first contact": func() rxBucketKey {
				return p2pRxBucket(&seal.Access{Kind: seal.AccessKindSession}, r.digest, &r.row, r.self, r.mbox, tNow)
			},
			"not issued here": func() rxBucketKey { return p2pRxBucket(r.access, r.digest, nil, r.self, r.mbox, tNow) },
			"revoked": func() rxBucketKey {
				row := r.row
				row.RevokedAt = tNow - 1
				return p2pRxBucket(r.access, r.digest, &row, r.self, r.mbox, tNow)
			},
			"expired": func() rxBucketKey {
				return p2pRxBucket(r.access, r.digest, &r.row, r.self, r.mbox, r.row.CapExp+1)
			},
			"another envelope": func() rxBucketKey {
				return p2pRxBucket(r.access, [32]byte{6}, &r.row, r.self, r.mbox, tNow)
			},
			"another owner": func() rxBucketKey {
				return p2pRxBucket(r.access, r.digest, &r.row, tOther, r.mbox, tNow)
			},
			"a newer mailbox key": func() rxBucketKey {
				return p2pRxBucket(r.access, r.digest, &r.row, r.self, seal.Mbox{Epoch: 3, Pub: r.mbox.Pub}, tNow)
			},
			"another row's id": func() rxBucketKey {
				row := r.row
				row.CapID = bytes.Repeat([]byte{8}, seal.CapIDLen)
				return p2pRxBucket(r.access, r.digest, &row, r.self, r.mbox, tNow)
			},
		}
		for name, f := range cases {
			if got := f(); got != shared {
				t.Errorf("class %d, %s: %+v", class, name, got)
			}
		}
	}
}

// The buckets: a contact capability 20/s with a burst of 100, a stranger
// capability and the shared bucket 1/s with a burst of 10, and the global
// 20/s with a burst of 100 over all of them; a delivery one bucket refuses
// takes nothing from the other.
func TestP2PRxLimiterRates(t *testing.T) {
	var l p2pRxLimiter
	burst := func(k rxBucketKey, at uint64) int {
		n := 0
		for l.allow(k, at) {
			n++
			if n > 1000 {
				break
			}
		}
		return n
	}
	stranger := rxBucketKey{Class: seal.CapClassStranger, CapID: "s1"}
	if n := burst(stranger, tNow); n != 10 {
		t.Fatalf("stranger burst %d", n)
	}
	if n := burst(rxBucketKey{}, tNow); n != 10 {
		t.Fatalf("shared burst %d", n)
	}
	// The global bucket had 100; 20 went. A contact capability gets the 80 left.
	if n := burst(rxBucketKey{Class: seal.CapClassContact, CapID: "c1"}, tNow); n != 80 {
		t.Fatalf("contact after the others: %d", n)
	}
	// One second later: a stranger capability gets one more, and the global refill of 20 is shared.
	if n := burst(stranger, tNow+1000); n != 1 {
		t.Fatalf("stranger after 1s: %d", n)
	}
	var l2 p2pRxLimiter
	for i := 0; i < 100; i++ {
		l2.allow(rxBucketKey{Class: seal.CapClassContact, CapID: fmt.Sprint(i)}, tNow)
	}
	// The global bucket is empty: a stranger capability is refused and keeps its token.
	k := rxBucketKey{Class: seal.CapClassStranger, CapID: "s2"}
	if l2.allow(k, tNow) {
		t.Fatal("the global bucket did not refuse")
	}
	l2.mu.Lock()
	tokens := l2.caps["s2"].tokens
	l2.mu.Unlock()
	if tokens != 10 {
		t.Fatalf("the refused delivery took a token from its own bucket: %v left", tokens)
	}
}

// FuzzP2PRxBucket: whatever a direct sender puts in outer key 7, step 0
// gives the capability's own bucket only to the capability this node
// issued, proven for this envelope.
func FuzzP2PRxBucket(f *testing.F) {
	r := newP2PRig(f, seal.CapClassContact)
	capb, _ := r.access.Cap.Marshal()
	f.Add(uint8(1), capb, r.access.Proof, uint8(0))
	f.Add(uint8(0), []byte{}, []byte{}, uint8(1))
	f.Add(uint8(1), capb, bytes.Repeat([]byte{1}, 64), uint8(2))
	f.Fuzz(func(t *testing.T, kind uint8, capBytes, proof []byte, mutate uint8) {
		a := &seal.Access{Kind: uint64(kind % 3), Proof: proof}
		if c, err := seal.UnmarshalCap(capBytes); err == nil {
			a.Cap = c
		}
		digest := r.digest
		if mutate%4 == 1 {
			digest[0] ^= 1
		}
		got := p2pRxBucket(a, digest, &r.row, r.self, r.mbox, tNow)
		if got == (rxBucketKey{}) {
			return
		}
		if got.CapID != string(r.row.CapID) || got.Class != r.row.Class {
			t.Fatalf("bucket %+v for a capability not issued here", got)
		}
		if err := seal.VerifyAccess(a, digest, r.mbox, r.self, tNow); err != nil {
			t.Fatalf("own bucket for an access that does not verify: %v", err)
		}
	})
}

// FuzzGrantToKeep: whatever bytes a sender puts in inner key 14, step 8
// keeps a row only for a capability the sender signed under the mailbox
// key of the key set it attached, for this node's use towards it, and
// never for a one-shot envelope.
func FuzzGrantToKeep(f *testing.F) {
	d := newTestDaemon(f, "", false)
	pub, priv, _ := ed25519.GenerateKey(nil)
	keys := &seal.EncKeySet{AID: tPeer, Mbox: seal.Mbox{Epoch: 3, Pub: pub}}
	seed := bytes.Repeat([]byte{6}, 32)
	holder := ed25519.NewKeyFromSeed(seed)
	c, err := seal.SignCap(priv, &seal.CapBody{Owner: tPeer, Epoch: 3, CapID: bytes.Repeat([]byte{2}, seal.CapIDLen),
		Class: 1, CapExp: tNow + capLifeContactMS, Holder: holder.Public().(ed25519.PublicKey)})
	if err != nil {
		f.Fatal(err)
	}
	good, err := (&seal.Grant{Cap: *c, HolderSeed: seed, Home: "hub-aid-0003"}).Marshal()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(good, false)
	f.Add(good, true)
	f.Add([]byte{0xa0}, false)
	f.Fuzz(func(t *testing.T, grant []byte, oneShot bool) {
		in := &seal.SealedInner{From: tPeer, IX: "ix-1", Grant: grant}
		h := d.grantToKeep(in, keys, oneShot, tNow)
		if h == nil {
			return
		}
		if oneShot {
			t.Fatal("a one-shot envelope's grant is kept")
		}
		if h.Issuer != tPeer || h.Epoch != 3 || h.Home == "" {
			t.Fatalf("kept %+v", h)
		}
		capw, err := seal.UnmarshalCap(h.Cap)
		if err != nil || !ed25519.Verify(pub, capw.Body, capw.Sig) {
			t.Fatalf("kept a capability the sender did not sign: %v", err)
		}
	})
}
