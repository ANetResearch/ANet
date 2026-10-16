package daemon

// caps_issue_test.go: the credentials a daemon issues (SS §4.3.2 SSR-D6 to
// D8, SSR-W4, SSR-D10), the grants it keeps (SSR-W8), and what a peer's
// emergency rotation and a settled payment do to both (engine ENG-K4, SS
// §4.3.8 items 3 and 5).

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/identity/identitytest"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// issueNow plans and commits the grant of dec from d to `to` at now, the way
// the engine does (planGrant, then commitGrantTx in the version's
// transaction), and returns the grant bytes and the caps_issued row.
func issueNow(t *testing.T, d *Daemon, to string, dec GrantDecision, now uint64) ([]byte, *interactions.IssuedCap) {
	t.Helper()
	p, err := d.planGrant(to, dec, now)
	if err != nil {
		t.Fatal(err)
	}
	if p == nil {
		return nil, nil
	}
	if err := d.ix.Update(func(tx *interactions.Tx) error { return d.commitGrantTx(tx, p, now) }); err != nil {
		t.Fatal(err)
	}
	row, err := d.ix.IssuedCap(p.row.CapID)
	if err != nil {
		t.Fatal(err)
	}
	return p.Grant, row
}

// receivedGrant checks a grant from `from` as the recipient's step 8 does
// (seal.CheckGrant against from's key set) and returns it with its body.
func receivedGrant(t *testing.T, from *Daemon, grant []byte, ix string, now uint64) (*seal.Grant, *seal.CapBody) {
	t.Helper()
	in := &seal.SealedInner{From: from.AID(), IX: ix, Grant: grant}
	g, body, err := seal.CheckGrant(in, keySetOf(t, from), now)
	if err != nil {
		t.Fatalf("the recipient does not take the grant: %v", err)
	}
	return g, body
}

var (
	decContactPeer = func(now uint64) GrantDecision {
		return grantFor(capMsg{Kind: capMsgOther, IX: "ix-1"}, capRel{Listed: true, Role: capRoleProvider}, now)
	}
	decStrangerPeer = func(now uint64) GrantDecision {
		return grantFor(capMsg{Kind: capMsgOther, IX: "ix-1"}, capRel{Role: capRoleProvider}, now)
	}
	decStrangerOneShot = func(ix string, now uint64) GrantDecision {
		return grantFor(capMsg{Kind: capMsgOther, IX: ix}, capRel{Role: capRoleProvider, MaybePaid: true}, now)
	}
)

// SSR-W4: a contact peer capability lives 30 days, a stranger peer
// capability and a one-shot capability 7 days, the reply capability of an
// anet.card.get/1 15 minutes — in caps_issued and in the capability the
// grant carries.
func TestCapLifetimes(t *testing.T) {
	_, req, prov := registeredPair(t)
	now := prov.nowMS()
	day := uint64(24 * 3600 * 1000)
	cases := []struct {
		name string
		dec  GrantDecision
		life uint64
	}{
		{"contact peer", decContactPeer(now), 30 * day},
		{"stranger peer", decStrangerPeer(now), 7 * day},
		{"stranger one-shot", decStrangerOneShot("ix-2", now), 7 * day},
		{"contact one-shot", grantFor(capMsg{IX: "ix-3"}, capRel{Role: capRoleRequester}, now), 7 * day},
		{"card reply", grantFor(capMsg{Kind: capMsgCardGet, IX: "card-0123456789abcdef0123456789abcdef"}, capRel{}, now), 15 * 60 * 1000},
	}
	for _, c := range cases {
		g, row := issueNow(t, prov, req.AID(), c.dec, now)
		if row == nil || row.CapExp != now+c.life || row.IssuedAt != now {
			t.Fatalf("%s: row %+v", c.name, row)
		}
		_, body := receivedGrant(t, prov, g, c.dec.IX, now)
		if body.CapExp != now+c.life || body.Class != c.dec.Class {
			t.Fatalf("%s: the capability says %d, class %d", c.name, body.CapExp-now, body.Class)
		}
	}
}

// SSR-D6: a valid capability for the same scope, interaction and class is
// given again as it is — the same bytes — and, once less than half its life
// is left, renewed under the same cap_id and holder key with a later
// cap_exp. A withdrawn one is not given again: a new capability replaces it.
func TestGrantReuseAndRenew(t *testing.T) {
	_, req, prov := registeredPair(t)
	t0 := prov.nowMS()
	day := uint64(24 * 3600 * 1000)
	g1, r1 := issueNow(t, prov, req.AID(), decContactPeer(t0), t0)
	g2, r2 := issueNow(t, prov, req.AID(), decContactPeer(t0+day), t0+day)
	if !bytes.Equal(g1, g2) || !bytes.Equal(r1.CapID, r2.CapID) || r2.CapExp != r1.CapExp {
		t.Fatal("a valid capability was not given again as it is")
	}
	t16 := t0 + 16*day // 14 days left of 30
	g3, r3 := issueNow(t, prov, req.AID(), decContactPeer(t16), t16)
	if !bytes.Equal(r3.CapID, r1.CapID) || !bytes.Equal(r3.HolderPub, r1.HolderPub) || r3.CapExp != t16+30*day {
		t.Fatalf("renewal: %+v", r3)
	}
	g, body := receivedGrant(t, prov, g3, "", t16)
	if body.CapExp != t16+30*day || !bytes.Equal(g.HolderSeed, r1.HolderSeed) {
		t.Fatal("the renewed grant does not carry the later cap_exp under the same holder key")
	}
	if rows, _ := prov.ix.IssuedCaps(req.AID()); len(rows) != 1 {
		t.Fatalf("%d rows after reuse and renewal", len(rows))
	}
	// Withdrawn: a new capability.
	if _, err := prov.revokeIssued([]interactions.IssuedFilter{{Grantee: req.AID()}}); err != nil {
		t.Fatal(err)
	}
	_, r4 := issueNow(t, prov, req.AID(), decContactPeer(t16+1), t16+1)
	if bytes.Equal(r4.CapID, r1.CapID) || bytes.Equal(r4.HolderPub, r1.HolderPub) {
		t.Fatal("a withdrawn capability was given again")
	}
	// A stranger peer capability is another capability than the contact one.
	_, r5 := issueNow(t, prov, req.AID(), decStrangerPeer(t16+2), t16+2)
	if bytes.Equal(r5.CapID, r4.CapID) {
		t.Fatal("the contact capability was given as the stranger one")
	}
	// Withdrawn between the plan and the version's commit (a list edit, a
	// settlement, a rotation): a reuse or renewal planned before does not
	// commit, and the version is sealed again.
	for _, c := range []struct {
		name string
		dec  GrantDecision
		at   uint64
		op   grantOp
	}{
		{"reuse", decStrangerPeer(t16 + 3), t16 + 3, grantReuse},
		{"renewal", decContactPeer(t16 + 1 + 16*day), t16 + 1 + 16*day, grantRenew},
	} {
		p, err := prov.planGrant(req.AID(), c.dec, c.at)
		if err != nil || p == nil || p.op != c.op {
			t.Fatalf("%s: plan %+v, %v", c.name, p, err)
		}
		if err := prov.ix.Update(func(tx *interactions.Tx) error {
			_, err := tx.RevokeIssuedCap(p.row.CapID, c.at)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		err = prov.ix.Update(func(tx *interactions.Tx) error { return prov.commitGrantTx(tx, p, c.at) })
		if !errors.Is(err, errGrantStale) {
			t.Fatalf("%s of a withdrawn capability committed: %v", c.name, err)
		}
	}
}

// providerIX records an inbound interaction from peer whose payment was
// quoted: a possibly paid interaction this node accepted.
func providerIX(t *testing.T, d *Daemon, peer, ix string) {
	t.Helper()
	if err := d.ix.Create(interactions.New{ID: ix, Role: interactions.RoleInbound, PeerAID: peer, Goal: "work",
		Trust: interactions.TrustPublic}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ix.SetPayment(ix, interactions.PayUpdate{State: interactions.PayState(interactions.PayRequired)}); err != nil {
		t.Fatal(err)
	}
}

// SSR-D7: a peer holds at most 8 stranger one-shot capabilities from this
// node at once; past that, a possibly paid interaction of a stranger gives
// none (the stranger writes by first contact). An interaction that has one
// keeps getting it.
func TestAtMostEightStrangerOneShots(t *testing.T) {
	_, _, prov := registeredPair(t)
	const g = "did:anet:eight-peer-0001"
	now := prov.nowMS()
	var first *interactions.IssuedCap
	for i := 1; i <= 9; i++ {
		ix := fmt.Sprintf("ix-%d", i)
		providerIX(t, prov, g, ix)
		p, err := prov.grantForMessage(g, capMsg{Kind: capMsgOther, IX: ix}, now)
		if err != nil {
			t.Fatal(err)
		}
		if i == 9 {
			if p != nil {
				t.Fatal("a ninth stranger one-shot capability was given")
			}
			break
		}
		if p == nil || p.row.Scope != "ix" || p.row.Class != seal.CapClassStranger {
			t.Fatalf("one-shot %d: %+v", i, p)
		}
		if err := prov.ix.Update(func(tx *interactions.Tx) error { return prov.commitGrantTx(tx, p, now) }); err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			first = &p.row
		}
	}
	// ix-1 still gets its own.
	p, err := prov.grantForMessage(g, capMsg{Kind: capMsgOther, IX: "ix-1"}, now)
	if err != nil || p == nil || !bytes.Equal(p.row.CapID, first.CapID) {
		t.Fatalf("ix-1 lost its capability: %+v %v", p, err)
	}
	// The commit checks again: a ninth planned before the eighth committed is stale.
	if _, err := prov.revokeIssued([]interactions.IssuedFilter{{Grantee: g, Scope: "ix"}}); err != nil {
		t.Fatal(err)
	}
	var plans []*grantPlan
	for i := 1; i <= 9; i++ {
		p, err := prov.planGrant(g, decStrangerOneShot(fmt.Sprintf("jx-%d", i), now), now)
		if err != nil || p == nil {
			t.Fatal(err)
		}
		plans = append(plans, p)
	}
	for i, p := range plans {
		err := prov.ix.Update(func(tx *interactions.Tx) error { return prov.commitGrantTx(tx, p, now) })
		if want := i == 8; errors.Is(err, errGrantStale) != want {
			t.Fatalf("commit %d: %v", i+1, err)
		}
	}
	// A planned grant whose capability another version issued first is stale.
	p1, _ := prov.planGrant(g, decStrangerPeer(now), now)
	p2, _ := prov.planGrant(g, decStrangerPeer(now), now)
	if err := prov.ix.Update(func(tx *interactions.Tx) error { return prov.commitGrantTx(tx, p1, now) }); err != nil {
		t.Fatal(err)
	}
	if err := prov.ix.Update(func(tx *interactions.Tx) error { return prov.commitGrantTx(tx, p2, now) }); !errors.Is(err, errGrantStale) {
		t.Fatalf("a second new peer capability: %v", err)
	}
}

// SSR-D7: a contact peer capability for a peer that held a stranger one
// withdraws the stranger one in the transaction that issues it.
func TestContactReplacesStranger(t *testing.T) {
	_, req, prov := registeredPair(t)
	now := prov.nowMS()
	_, stranger := issueNow(t, prov, req.AID(), decStrangerPeer(now), now)
	_, one := issueNow(t, prov, req.AID(), decStrangerOneShot("ix-9", now), now)
	// The commit fails after the insert: neither change stays.
	p, err := prov.planGrant(req.AID(), decContactPeer(now+1), now+1)
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("the version's write failed")
	if err := prov.ix.Update(func(tx *interactions.Tx) error {
		if err := prov.commitGrantTx(tx, p, now+1); err != nil {
			return err
		}
		return boom
	}); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if got := issuedState(t, prov, *stranger); got != "given" {
		t.Fatalf("a rolled-back contact capability withdrew the stranger one: %s", got)
	}
	if _, err := prov.ix.IssuedCap(p.row.CapID); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatal("a rolled-back version left its capability (SSR-D10)")
	}
	_, contact := issueNow(t, prov, req.AID(), decContactPeer(now+2), now+2)
	if got := issuedState(t, prov, *stranger, *one, *contact); got != "withdrawn,given,given" {
		t.Fatalf("after the contact capability: %s", got)
	}
}

// SSR-D8: p2p only in a contact peer capability of a listed peer, pay only
// in a peer capability, never in a one-shot one; the recipient takes each.
func TestP2PAndPayPlacement(t *testing.T) {
	_, req, prov := registeredPair(t)
	now := prov.nowMS()
	const addr = "203.0.113.7:4001"
	listed := capRel{Listed: true, Role: capRoleRequester, P2P: addr, Prices: true}
	stranger := capRel{Role: capRoleProvider, P2P: addr, Prices: true}
	cases := []struct {
		name    string
		dec     GrantDecision
		p2p     string
		pay, ix bool
	}{
		{"listed, contact peer", grantFor(capMsg{IX: "ix-1"}, listed, now), addr, true, false},
		{"stranger peer", grantFor(capMsg{IX: "ix-1"}, stranger, now), "", true, false},
		{"one-shot, paid", grantFor(capMsg{IX: "ix-2"}, capRel{Listed: true, MaybePaid: true, P2P: addr, Prices: true}, now), "", false, true},
		{"one-shot, requester", grantFor(capMsg{IX: "ix-3"}, capRel{Role: capRoleRequester, P2P: addr, Prices: true}, now), "", false, true},
		{"listed, no prices", grantFor(capMsg{IX: "ix-1"}, capRel{Listed: true, P2P: addr}, now), addr, false, false},
	}
	for i, c := range cases {
		to := fmt.Sprintf("did:anet:place-peer-%04d", i)
		if i == 0 {
			to = req.AID()
		}
		gb, _ := issueNow(t, prov, to, c.dec, now)
		in := &seal.SealedInner{From: prov.AID(), IX: c.dec.IX, Grant: gb}
		g, _, err := seal.CheckGrant(in, keySetOf(t, prov), now)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		gotP2P := ""
		if g.P2P != nil {
			gotP2P = *g.P2P
		}
		if gotP2P != c.p2p || (g.Pay != nil && *g.Pay) != c.pay || (g.IX != nil) != c.ix {
			t.Fatalf("%s: p2p %q pay %v ix %v", c.name, gotP2P, g.Pay, g.IX)
		}
		if g.Pay != nil && !*g.Pay {
			t.Fatalf("%s: pay written as false", c.name)
		}
	}
	if got := p2pAdvertise(Config{Modules: ModulesConfig{"p2p": json.RawMessage(`{"advertise":"` + addr + `","lan":false}`)}}); got != addr {
		t.Fatalf("p2pAdvertise %q", got)
	}
	if got := p2pAdvertise(Config{}); got != "" {
		t.Fatalf("p2pAdvertise without the module: %q", got)
	}
	// pay is whether this node prices any capability it serves.
	if prov.pricesSomething() {
		t.Fatal("a node with no priced capability says it prices")
	}
	// A priced capability turns it on: TestPricedCapabilitiesSetPay.
}

// SSR-D10, the one-shot sender's part: the reply capability of an
// anet.card.get/1 is committed in its own transaction before the grant is
// handed back; without a mailbox key there is no grant, so nothing is sent.
func TestCardGetCapCommittedBeforeSend(t *testing.T) {
	_, req, prov := registeredPair(t)
	now := prov.nowMS()
	const ix = "card-0123456789abcdef0123456789abcdef"
	g, err := prov.cardGetGrant(req.AID(), ix, now)
	if err != nil {
		t.Fatal(err)
	}
	gr, body := receivedGrant(t, prov, g, ix, now)
	if gr.IX == nil || *gr.IX != ix || body.Class != seal.CapClassContact || body.CapExp != now+capLifeCardMS {
		t.Fatalf("the reply grant: %+v %+v", gr, body)
	}
	// Committed: a second handle on the same file reads it.
	s, err := interactions.Open(prov.layout.InteractionsDir())
	if err != nil {
		t.Fatal(err)
	}
	row, err := s.IssuedCap(body.CapID)
	s.Close()
	if err != nil || row.Scope != "ix" || row.IX != ix || row.Grantee != req.AID() || row.RevokedAt != 0 {
		t.Fatalf("the row on disk: %+v %v", row, err)
	}
	// A second request in the same interaction gives the same capability.
	g2, err := prov.cardGetGrant(req.AID(), ix, now+1)
	if err != nil || !bytes.Equal(g, g2) {
		t.Fatalf("again: %v", err)
	}
	// No mailbox key: no grant, no row.
	prov.mbox.mu.Lock()
	epoch, priv := prov.mbox.epoch, prov.mbox.priv
	prov.mbox.epoch, prov.mbox.priv = 0, nil
	prov.mbox.mu.Unlock()
	if g, err := prov.cardGetGrant(req.AID(), "card-ffffffffffffffffffffffffffffffff", now); err == nil || g != nil {
		t.Fatal("a reply grant without a mailbox key")
	}
	prov.mbox.mu.Lock()
	prov.mbox.epoch, prov.mbox.priv = epoch, priv
	prov.mbox.mu.Unlock()
	if rows, _ := prov.ix.IssuedCaps(req.AID()); len(rows) != 1 {
		t.Fatalf("%d rows", len(rows))
	}
	// A plan whose mailbox key was replaced before the commit is stale.
	p, err := prov.planGrant(req.AID(), decContactPeer(now), now)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.ix.Update(func(tx *interactions.Tx) error { _, err := prov.rotateMboxTx(tx); return err }); err != nil {
		t.Fatal(err)
	}
	if err := prov.ix.Update(func(tx *interactions.Tx) error { return prov.commitGrantTx(tx, p, now) }); !errors.Is(err, errGrantStale) {
		t.Fatalf("a plan under the old mailbox key: %v", err)
	}
}

// craftGrant is craft with a grant in inner key 14, and outer key 8 when
// oneShot.
func craftGrant(t *testing.T, s sender, to *Daemon, typ, ix string, body, grant []byte, oneShot bool) []byte {
	t.Helper()
	trustSuiteWitness(t, to)
	now := uint64(time.Now().UnixMilli())
	kel, err := identity.MarshalKEL(s.kel)
	if err != nil {
		t.Fatal(err)
	}
	in := &seal.SealedInner{From: s.aid, KeyStateSeq: s.ksn, To: to.AID(), Type: typ, IX: ix,
		MID: seal.NewMID(), TS: now, Exp: now + messageLifetimeMS, Body: body, KEL: kel, Keys: s.keys,
		Notices: s.notices(t), Grant: grant}
	var o seal.SealOpts
	if oneShot {
		in.Exp = now + 10*60*1000
		o.Exp = in.Exp
	}
	key, err := seal.SelectKey(keySetOf(t, to), now)
	if err != nil {
		t.Fatal(err)
	}
	env, _, err := seal.Seal(in, key, s.sign, o)
	if err != nil {
		t.Fatal(err)
	}
	return withSessionAccess(t, env)
}

// heldFrom is the capability the recipient keeps from issuer, nil for none.
func heldFrom(t *testing.T, d *Daemon, issuer, scope, ix string) *interactions.HeldCap {
	t.Helper()
	h, err := d.ix.HeldCap(issuer, scope, ix)
	if errors.Is(err, interactions.ErrNotFound) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// SSR-W8 (M26): only the step-10 transaction that commits a message keeps
// its grant. A delegation refused at step 9, a message whose step 10 rolls
// back, a duplicate and a one-shot envelope keep nothing; an accepted
// message keeps its grant, and a grant that does not check is ignored
// (grant-invalid) without affecting the message.
func TestRefusedMessageGrantIsNotKept(t *testing.T) {
	_, sender, recv := registeredPair(t)
	// recv accepts sender's delegations only once it is allowed.
	removeAllowed(t, recv, sender.AID())
	now := sender.nowMS()
	grant := func(dec GrantDecision) []byte {
		g, _ := issueNow(t, sender, recv.AID(), dec, now)
		return g
	}
	s := senderOf(sender)

	// 1. Refused at step 9 (inbound policy closed, sender not allowed).
	g1 := grant(decContactPeer(now))
	if res := receive(t, recv, craftGrant(t, s, recv, seal.TypeDelegate, "ix-a", delegateBody(t, sender.self, "ix-a", "hi", ""), g1, false)); res.class != rxDropped {
		t.Fatalf("refused delegation: %+v", res)
	}
	if h := heldFrom(t, recv, sender.AID(), "peer", ""); h != nil {
		t.Fatal("the grant of a refused delegation was kept")
	}

	// 2. Accepted.
	allowPeers(t, recv, sender.AID())
	envB := craftGrant(t, s, recv, seal.TypeDelegate, "ix-b", delegateBody(t, sender.self, "ix-b", "hi", ""), g1, false)
	if res := receive(t, recv, envB); res.class != rxAccepted {
		t.Fatalf("accepted delegation: %+v", res)
	}
	h := heldFrom(t, recv, sender.AID(), "peer", "")
	_, b1 := receivedGrant(t, sender, g1, "", now)
	if h == nil || !bytes.Equal(h.CapID, b1.CapID) || h.Home == "" {
		t.Fatalf("the accepted message's grant: %+v", h)
	}

	// 3. A newer capability replaces it; a duplicate of the older message
	// does not put the older one back.
	if _, err := sender.revokeIssued([]interactions.IssuedFilter{{Grantee: recv.AID()}}); err != nil {
		t.Fatal(err)
	}
	g2 := grant(decContactPeer(now + 1))
	if res := receive(t, recv, craftGrant(t, s, recv, seal.TypeMessage, "ix-b", chatBody(t, "more", ""), g2, false)); res.class != rxAccepted {
		t.Fatalf("message: %+v", res)
	}
	_, b2 := receivedGrant(t, sender, g2, "", now)
	if h := heldFrom(t, recv, sender.AID(), "peer", ""); h == nil || !bytes.Equal(h.CapID, b2.CapID) {
		t.Fatal("the newer grant was not kept")
	}
	receive(t, recv, envB)
	if h := heldFrom(t, recv, sender.AID(), "peer", ""); h == nil || !bytes.Equal(h.CapID, b2.CapID) {
		t.Fatal("a duplicate put its grant back")
	}

	// 4. Step 10 rolls back: nothing kept.
	gx := grant(decStrangerOneShot("ix-b", now))
	recv.setRxFault(func(string) error { return errors.New("store full") })
	if res := receive(t, recv, craftGrant(t, s, recv, seal.TypeMessage, "ix-b", chatBody(t, "lost", ""), gx, false)); res.class != rxTransient {
		t.Fatalf("a failed step 10: %+v", res)
	}
	recv.setRxFault(nil)
	if h := heldFrom(t, recv, sender.AID(), "ix", "ix-b"); h != nil {
		t.Fatal("the grant of a rolled-back message was kept")
	}

	// 5. A one-shot envelope (outer key 8) leaves its grant, whatever
	// becomes of the envelope.
	receive(t, recv, craftGrant(t, s, recv, seal.TypeMessage, "ix-b", chatBody(t, "once", ""), gx, true))
	if h := heldFrom(t, recv, sender.AID(), "ix", "ix-b"); h != nil {
		t.Fatal("the grant of a one-shot envelope was kept")
	}

	// 6. A grant that does not check is ignored; the message is processed.
	bad := append([]byte(nil), gx...)
	bad[len(bad)-1] ^= 1
	before := counter(recv, cntGrantInvalid)
	if res := receive(t, recv, craftGrant(t, s, recv, seal.TypeMessage, "ix-b", chatBody(t, "bad grant", ""), bad, false)); res.class != rxAccepted {
		t.Fatalf("a message with a bad grant: %+v", res)
	}
	if counter(recv, cntGrantInvalid) != before+1 || heldFrom(t, recv, sender.AID(), "ix", "ix-b") != nil {
		t.Fatal("a bad grant was not ignored and counted")
	}
	// And the good one, in an accepted message, is kept.
	if res := receive(t, recv, craftGrant(t, s, recv, seal.TypeMessage, "ix-b", chatBody(t, "kept", ""), gx, false)); res.class != rxAccepted {
		t.Fatalf("%+v", res)
	}
	if h := heldFrom(t, recv, sender.AID(), "ix", "ix-b"); h == nil || h.Class != seal.CapClassStranger {
		t.Fatalf("the one-shot grant: %+v", h)
	}
}

// removeAllowed takes aid off d's allow list.
func removeAllowed(t *testing.T, d *Daemon, aid string) {
	t.Helper()
	if _, err := d.editPeerFile(ListAllow, aid, false); err != nil {
		t.Fatal(err)
	}
}

// capsBetween seeds caps_issued from d to peer (a peer and a one-shot
// capability) and caps_held of d from peer (a peer capability with a p2p
// address and a one-shot one), and returns their ids.
func capsBetween(t *testing.T, d *Daemon, peer string, seed byte) (issuedPeer, issuedOne interactions.IssuedCap, heldPeer, heldOne []byte) {
	t.Helper()
	issuedPeer = issueTo(t, d, peer, "peer", "", seal.CapClassContact, seed)
	issuedOne = issueTo(t, d, peer, "ix", "ix-o", seal.CapClassContact, seed+1)
	now := d.nowMS()
	for _, h := range []interactions.HeldCap{
		{Issuer: peer, Scope: "peer", CapID: bytes.Repeat([]byte{seed + 2}, 16), P2P: "203.0.113.9:4001"},
		{Issuer: peer, Scope: "ix", IX: "ix-o", CapID: bytes.Repeat([]byte{seed + 3}, 16)},
	} {
		h.Cap, h.HolderSeed, h.Class, h.Epoch, h.CapExp, h.Home, h.ReceivedAt = []byte{0xa0}, bytes.Repeat([]byte{1}, 32), 1, 1,
			now+capLifeStrangerMS, "hub-aid-0003", now
		if err := d.ix.Update(func(tx *interactions.Tx) error { _, err := tx.PutHeldGrant(h); return err }); err != nil {
			t.Fatal(err)
		}
	}
	return issuedPeer, issuedOne, bytes.Repeat([]byte{seed + 2}, 16), bytes.Repeat([]byte{seed + 3}, 16)
}

// heldState is "valid" or "given-up" (with "+p2p" while the address is
// kept) for each held capability of issuer.
func heldState(t *testing.T, d *Daemon, issuer string) string {
	t.Helper()
	rows, err := d.ix.HeldCaps(issuer)
	if err != nil {
		t.Fatal(err)
	}
	s := ""
	for _, h := range rows {
		st := "valid"
		if h.InvalidAt != 0 {
			st = "given-up"
		}
		if h.P2P != "" {
			st += "+p2p"
		}
		s += h.Scope + ":" + st + ","
	}
	return s
}

// ENG-K4, KEL D-44 (M46): merging a peer's emergency rotation drops every
// capability the peer gave this node, with the address its grants taught,
// and withdraws every capability this node gave the peer — in the merge's
// transaction, on the receive path's write of the peer's KEL. A planned
// rotation changes nothing; neither does another peer's.
func TestPeerEmergencyRotRevokesIssuedCaps(t *testing.T) {
	keepRevokeLoopOut(t)
	d := newTestDaemon(t, "", false)
	agent := func() *identity.Controller {
		c, err := identitytest.Agent()
		if err != nil {
			t.Fatal(err)
		}
		if err := identitytest.SwitchWitness(c, identitytest.Witness, 1000); err != nil {
			t.Fatal(err)
		}
		return c
	}
	x, y := agent(), agent()
	allowPeers(t, d, x.AID(), y.AID())
	record := func(c *identity.Controller) {
		t.Helper()
		if err := d.notePeer(c.AID(), c.KEL(), nil, interactions.PinAllow, false); err != nil {
			t.Fatal(err)
		}
	}
	record(x)
	record(y)
	xPeer, xOne, _, _ := capsBetween(t, d, x.AID(), 10)
	yPeer, _, _, _ := capsBetween(t, d, y.AID(), 20)

	if err := identitytest.Rotate(x, 2000); err != nil {
		t.Fatal(err)
	}
	record(x)
	if got := issuedState(t, d, xPeer, xOne); got != "given,given" || heldState(t, d, x.AID()) != "peer:valid+p2p,ix:valid," {
		t.Fatalf("a planned rotation: issued %s, held %s", got, heldState(t, d, x.AID()))
	}
	if err := identitytest.RotateEmergency(x, 3000); err != nil {
		t.Fatal(err)
	}
	record(x)
	if got := issuedState(t, d, xPeer, xOne); got != "withdrawn,withdrawn" {
		t.Fatalf("after the emergency rotation: %s", got)
	}
	if got := heldState(t, d, x.AID()); got != "peer:given-up,ix:given-up," {
		t.Fatalf("held from the rotated peer: %s", got)
	}
	if got := issuedState(t, d, yPeer); got != "given" || heldState(t, d, y.AID()) != "peer:valid+p2p,ix:valid," {
		t.Fatal("another peer's capabilities changed")
	}
	if left, _ := d.ix.UnpushedRevocations(10); len(left) != 2 {
		t.Fatalf("%d withdrawals queued for the hub, want 2", len(left))
	}
	// Merged again (the same KEL): nothing more.
	record(x)
	// A peer seen for the first time with an emergency rotation in its
	// history: nothing was merged, nothing is withdrawn.
	z := agent()
	if err := identitytest.RotateEmergency(z, 5000); err != nil {
		t.Fatal(err)
	}
	zPeer, _, _, _ := capsBetween(t, d, z.AID(), 30)
	allowPeers(t, d, z.AID())
	record(z)
	if got := issuedState(t, d, zPeer); got != "given" {
		t.Fatalf("a first record withdrew: %s", got)
	}

	// Through the receive path: an allowed peer's message carrying its
	// emergency rotation.
	recv := newTestDaemon(t, "", false)
	w := agent()
	allowPeers(t, recv, w.AID())
	ws := sender{aid: w.AID(), kel: w.KEL(), keys: signedKeysFor(t, w, 0), ksn: w.CurrentSeq(), sign: w.Sign, ctrl: w}
	if res := receive(t, recv, craft(t, ws, recv, seal.TypeDelegate, "ix-w", delegateBody(t, w, "ix-w", "hi", ""), nil)); res.class != rxAccepted {
		t.Fatalf("the peer's delegation: %+v", res)
	}
	wPeer, wOne, _, _ := capsBetween(t, recv, w.AID(), 40)
	if err := identitytest.RotateEmergency(w, uint64(time.Now().Add(-time.Minute).UnixMilli())); err != nil {
		t.Fatal(err)
	}
	ws = sender{aid: w.AID(), kel: w.KEL(), keys: signedKeysFor(t, w, 0), ksn: w.CurrentSeq(), sign: w.Sign, ctrl: w}
	if res := receive(t, recv, craft(t, ws, recv, seal.TypeMessage, "ix-w", chatBody(t, "rotated", ""), nil)); res.class != rxAccepted {
		t.Fatalf("the rotated peer's message: %+v", res)
	}
	if got := issuedState(t, recv, wPeer, wOne); got != "withdrawn,withdrawn" || heldState(t, recv, w.AID()) != "peer:given-up,ix:given-up," {
		t.Fatalf("through the receive path: issued %s, held %s", got, heldState(t, recv, w.AID()))
	}

	// A public requester: its KEL is kept on its open interaction, not in
	// peer_identity (A2A-DESIGN §3.8), and the receive path's write of the
	// KEL a later message carries is the merge.
	v := agent()
	const pubIX = "ix-pub-v"
	vKEL, err := identity.MarshalKEL(v.KEL())
	if err != nil {
		t.Fatal(err)
	}
	if err := d.ix.Create(interactions.New{ID: pubIX, Role: interactions.RoleInbound, PeerAID: v.AID(), Goal: "x",
		Trust: interactions.TrustPublic, PeerKEL: vKEL}); err != nil {
		t.Fatal(err)
	}
	vPeer, vOne, _, _ := capsBetween(t, d, v.AID(), 50)
	noteRequester := func() {
		t.Helper()
		m := &rxMsg{from: v.AID(), ix: pubIX, kelUpdate: v.KEL()}
		if err := d.ix.Update(func(tx *interactions.Tx) error { return d.recordSenderTx(tx, m, d.readPeers()) }); err != nil {
			t.Fatal(err)
		}
	}
	if err := identitytest.Rotate(v, 2000); err != nil {
		t.Fatal(err)
	}
	noteRequester()
	if got := issuedState(t, d, vPeer, vOne); got != "given,given" || heldState(t, d, v.AID()) != "peer:valid+p2p,ix:valid," {
		t.Fatalf("a public requester's planned rotation: issued %s, held %s", got, heldState(t, d, v.AID()))
	}
	if err := identitytest.RotateEmergency(v, 3000); err != nil {
		t.Fatal(err)
	}
	noteRequester()
	if got := issuedState(t, d, vPeer, vOne); got != "withdrawn,withdrawn" || heldState(t, d, v.AID()) != "peer:given-up,ix:given-up," {
		t.Fatalf("a public requester's emergency rotation: issued %s, held %s", got, heldState(t, d, v.AID()))
	}
	if pix, err := d.ix.Get(pubIX); err != nil || !bytes.Equal(pix.PeerKEL, mustMarshalKEL(t, v.KEL())) {
		t.Fatalf("the interaction does not keep the merged KEL: %v", err)
	}
	if _, err := d.ix.PeerIdentity(v.AID()); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatalf("a public requester was recorded in peer_identity: %v", err)
	}
}

func mustMarshalKEL(t *testing.T, kel []identity.SignedEvent) []byte {
	t.Helper()
	b, err := identity.MarshalKEL(kel)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// SS §4.3.8 item 3 (M27): a task paid through a2a-x402 that ends — or one
// that ended and is then recorded as paid — makes this node give up the
// peer capabilities between it and the peer, in that write's transaction,
// once. One-shot capabilities and an unpaid task's end change nothing. The
// store's two orders here; the whole paid flow on both sides is
// TestAPaidCallRotatesPeerCaps.
func TestSettledInteractionRotatesPeerCaps(t *testing.T) {
	keepRevokeLoopOut(t)
	// The store's two orders, and an unpaid end.
	d := newTestDaemon(t, "", false)
	const g = "did:anet:settle-peer-0001"
	order := func(ix string, paidFirst, pay bool) (string, string) {
		t.Helper()
		peerCap, _, _, _ := capsBetween(t, d, g, byte(len(ix)*7))
		if err := d.ix.Create(interactions.New{ID: ix, Role: interactions.RoleOutbound, PeerAID: g, Goal: "x"}); err != nil {
			t.Fatal(err)
		}
		setPaid := func() {
			if _, err := d.ix.SetPayment(ix, interactions.PayUpdate{State: interactions.PayState(interactions.PayCompleted)}); err != nil {
				t.Fatal(err)
			}
		}
		if pay && paidFirst {
			setPaid()
			if got := issuedState(t, d, peerCap); got != "given" {
				t.Fatalf("%s: paid before the end withdrew: %s", ix, got)
			}
		}
		if err := d.ix.SetResult(ix, []byte("r"), "", nil, interactions.VerificationUnknown, interactions.CausePeerEnded); err != nil {
			t.Fatal(err)
		}
		if pay && !paidFirst {
			setPaid()
		}
		return issuedState(t, d, peerCap), heldState(t, d, g)
	}
	if is, hs := order("settle-a", true, true); is != "withdrawn" || hs != "peer:given-up+p2p,ix:valid," {
		t.Fatalf("paid, then ended: %s / %s", is, hs)
	}
	if is, hs := order("settle-bb", false, true); is != "withdrawn" || hs != "peer:given-up+p2p,ix:valid," {
		t.Fatalf("ended, then paid: %s / %s", is, hs)
	}
	if is, _ := order("settle-ccc", false, false); is != "given" {
		t.Fatalf("an unpaid end withdrew: %s", is)
	}
}

// maybePaid (SS §4.3.2): a provider that quoted or serves a priced
// capability, a requester whose provider says it prices something or that
// was quoted.
func TestMaybePaidTable(t *testing.T) {
	for _, c := range []struct {
		f    paidFacts
		want bool
	}{
		{paidFacts{}, false},
		{paidFacts{Provider: true}, false},
		{paidFacts{Provider: true, Quoted: true}, true},
		{paidFacts{Provider: true, CallPriced: true}, true},
		{paidFacts{Provider: true, HeldPeerPay: true}, false},
		{paidFacts{Requester: true}, false},
		{paidFacts{Requester: true, HeldPeerPay: true}, true},
		{paidFacts{Requester: true, Quoted: true}, true},
		{paidFacts{Requester: true, CallPriced: true}, false},
		{paidFacts{Quoted: true, HeldPeerPay: true, CallPriced: true}, false},
	} {
		if got := maybePaid(c.f); got != c.want {
			t.Errorf("%+v: %v", c.f, got)
		}
	}
}

// The facts grantFor decides from, as the daemon gathers them: the lists,
// the role in the interaction (a held delegation and a rejected one are not
// accepted), a quote, a priced call, the provider's pay flag, and the
// stranger one-shot capabilities other interactions hold.
func TestGrantRelFacts(t *testing.T) {
	_, req, prov := registeredPair(t)
	now := prov.nowMS()
	const stranger = "did:anet:facts-peer-0001"
	rel := func(to, ix string) capRel {
		t.Helper()
		r, err := prov.grantRel(to, capMsg{Kind: capMsgOther, IX: ix}, now)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	// Listed (prov accepts req), no interaction.
	if r := rel(req.AID(), "ix-none"); !r.Listed || r.Denied || r.Role != capRoleNone || r.MaybePaid {
		t.Fatalf("listed, no interaction: %+v", r)
	}
	// An accepted public task of a stranger.
	if err := prov.ix.Create(interactions.New{ID: "ix-pub", Role: interactions.RoleInbound, PeerAID: stranger, Goal: "x",
		Trust: interactions.TrustPublic}); err != nil {
		t.Fatal(err)
	}
	if r := rel(stranger, "ix-pub"); r.Listed || r.Role != capRoleProvider || r.MaybePaid {
		t.Fatalf("an accepted public task: %+v", r)
	}
	// The interaction of another peer is not this one's.
	if r := rel(req.AID(), "ix-pub"); r.Role != capRoleNone {
		t.Fatalf("another peer's interaction: %+v", r)
	}
	// Quoted: possibly paid.
	if _, err := prov.ix.SetPayment("ix-pub", interactions.PayUpdate{State: interactions.PayState(interactions.PayRequired)}); err != nil {
		t.Fatal(err)
	}
	if r := rel(stranger, "ix-pub"); !r.MaybePaid {
		t.Fatalf("a quoted task: %+v", r)
	}
	// Rejected after it was taken: not accepted.
	if _, err := prov.ix.SetState("ix-pub", interactions.StateRejected, interactions.CauseProviderEnd); err != nil {
		t.Fatal(err)
	}
	if r := rel(stranger, "ix-pub"); r.Role != capRoleUnaccepted {
		t.Fatalf("a rejected task: %+v", r)
	}
	// Held for approval: not accepted.
	if err := prov.ix.Update(func(tx *interactions.Tx) error {
		return tx.PutPending(interactions.PendingItem{IX: "ix-held", FromAID: stranger, ArrivedAt: int64(now),
			Delegate: []byte{0xa0}}, 0, 0)
	}); err != nil {
		t.Fatal(err)
	}
	if r := rel(stranger, "ix-held"); r.Role != capRoleUnaccepted {
		t.Fatalf("a held task: %+v", r)
	}
	if r := rel(req.AID(), "ix-held"); r.Role != capRoleNone {
		t.Fatalf("another peer's held task: %+v", r)
	}
	// This node's own task to the peer.
	if err := prov.ix.Create(interactions.New{ID: "ix-out", Role: interactions.RoleOutbound, PeerAID: stranger, Goal: "x"}); err != nil {
		t.Fatal(err)
	}
	if r := rel(stranger, "ix-out"); r.Role != capRoleRequester || r.MaybePaid {
		t.Fatalf("own task: %+v", r)
	}
	// The provider's peer capability says it prices: possibly paid.
	h := heldCap("peer", "", 1, func(h *interactions.HeldCap) { h.Issuer, h.Pay = stranger, true })
	if err := prov.ix.Update(func(tx *interactions.Tx) error { _, err := tx.PutHeldGrant(h); return err }); err != nil {
		t.Fatal(err)
	}
	if r := rel(stranger, "ix-out"); !r.MaybePaid {
		t.Fatalf("own task, a provider that prices: %+v", r)
	}
	// Denied.
	denyPeers(t, prov, stranger)
	if r := rel(stranger, "ix-out"); !r.Denied || r.Listed {
		t.Fatalf("denied: %+v", r)
	}
	// The stranger one-shot capabilities of other interactions.
	for i := 0; i < 3; i++ {
		issueNow(t, prov, "did:anet:facts-peer-0002", decStrangerOneShot(fmt.Sprintf("so-%d", i), now), now)
	}
	if r := rel("did:anet:facts-peer-0002", "so-0"); r.StrangerOneShots != 2 {
		t.Fatalf("stranger one-shots of other interactions: %d", r.StrangerOneShots)
	}
	// A priced capability call: TestPricedCapabilitiesSetPay.
}
