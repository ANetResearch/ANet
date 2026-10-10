package daemon

import (
	"context"
	"crypto/ed25519"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/identity/identitytest"
	"github.com/ANetResearch/ANetCore/seal"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// keySetOf is the encryption key set another node would learn for d: the
// set d publishes, decoded. Tests that seal without a hub use it.
func keySetOf(t testing.TB, d *Daemon) *seal.EncKeySet {
	t.Helper()
	set, err := verifiedKeySetOf(d)
	if err != nil {
		// Every start makes a join attempt in the background, and a test
		// node's join designates its witness and then signs its key set
		// again (sendingJoinHooks): between the two the set is under a
		// retired key. The attempt holds regMu throughout, so wait for it
		// and read once more.
		d.regMu.Lock()
		d.regMu.Unlock() //nolint:staticcheck // waiting for the attempt is the point
		set, err = verifiedKeySetOf(d)
	}
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// verifiedKeySetOf is the key set d publishes, verified against its KEL.
func verifiedKeySetOf(d *Daemon) (*seal.EncKeySet, error) {
	signed, err := seal.UnmarshalSignedEncKeySet(d.enc.SignedSet())
	if err != nil {
		return nil, err
	}
	return seal.VerifyEncKeySet(signed, d.AID(), d.self.KEL(), d.nowMS())
}

// sealFrom seals body as a message of type typ for interaction ix, from
// daemon from to daemon to — exactly what from's send path produces, minus
// the delivery. A test hands the bytes to to.receiveEnvelope, which is the
// only door into the interaction store.
func sealFrom(t testing.TB, from, to *Daemon, typ, ix string, body []byte) []byte {
	t.Helper()
	env, err := from.sealWith(to.AID(), typ, ix, body, keySetOf(t, to))
	if err != nil {
		t.Fatal(err)
	}
	return env
}

// receive runs one envelope through d's receive pipeline.
func receive(t *testing.T, d *Daemon, env []byte) rxResult {
	t.Helper()
	return d.receiveEnvelope(context.Background(), env)
}

// counter reads one receive outcome counter.
func counter(d *Daemon, reason string) uint64 {
	return d.ReceiveStats()[reason]
}

// sender is a party that seals: a daemon, a bare identity with a key set of
// its own, or an attacker holding somebody's retired key.
type sender struct {
	aid  string
	kel  []identity.SignedEvent
	keys []byte // SignedEncKeySet encoding
	ksn  uint64
	sign seal.SignFunc
	ctrl *identity.Controller
	// notice is the witness notice the sender holds for kel (KEL D-50), as
	// a daemon carries it in inner key 11; nil when none confirms its tip.
	notice []byte
}

func senderOf(d *Daemon) sender {
	s := sender{aid: d.AID(), kel: d.self.KEL(), keys: d.enc.SignedSet(),
		ksn: d.self.CurrentSeq(), sign: d.self.Sign, ctrl: d.self}
	if n := d.tipNotice(d.self.Snapshot()); n != nil {
		s.notice = n.Wire
	}
	return s
}

// notices is inner key 11 of what s seals: the witness notice of the tip of
// the KEL it carries. A daemon carries the one its hub signed (senderOf); a
// bare identity witnessed by the suite witness (newStranger,
// witnessedIdentity) gets one signed now. A sender that has none — a hub
// identity, an agent that named no witness — is a test mistake: the
// receiver would refuse what it sends at step 6½, whatever the test meant
// to try, so the test stops here and says so. A test about the notice
// itself sets Notices in craft's edit.
func (s sender) notices(t testing.TB) [][]byte {
	t.Helper()
	if s.notice != nil {
		return [][]byte{s.notice}
	}
	if n := suiteNotice(t, s.kel, uint64(time.Now().UnixMilli())); n != nil {
		return [][]byte{n}
	}
	t.Fatalf("sender %s has no witness notice for its key history: give it a witness (witnessedIdentity), "+
		"or set Notices in craft's edit for a test about the notice", s.aid)
	return nil
}

// suiteNotice is the notice the suite witness (identitytest.Witness, the
// conformance hub) signs at observedAt for the tip of kel, or nil when kel
// names another witness or none.
func suiteNotice(t testing.TB, kel []identity.SignedEvent, observedAt uint64) []byte {
	t.Helper()
	states, err := identity.Replay(kel)
	if err != nil || len(states) == 0 || states[len(states)-1].Witness != identitytest.Witness {
		return nil
	}
	tip := kel[len(kel)-1]
	n, err := identity.SignNotice(identity.SuiteController(), identity.NoticeBody{AID: states[len(states)-1].AID,
		Seq: tip.Event.Seq, Head: tip.EventID, ObservedAt: observedAt})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// trustSuiteWitness has d hold the suite witness's KEL as a hub's, the way
// it holds the KEL of the hub it joined (pinHubIdentity): the notices of
// the bare test identities then check at step 6½ (KEL D-21).
func trustSuiteWitness(t testing.TB, d *Daemon) {
	t.Helper()
	if _, err := d.pinPeerKEL(identitytest.Witness, identity.SuiteController().KEL(), interactions.PinHub); err != nil {
		t.Fatal(err)
	}
}

// witnessedIdentity is a fresh agent identity whose witness is the suite
// witness, designated at the start of time so that any key event a test
// dates later follows it.
func witnessedIdentity(t testing.TB) *identity.Controller {
	t.Helper()
	c, err := identitytest.Agent()
	if err != nil {
		t.Fatal(err)
	}
	if err := identitytest.SwitchWitness(c, identitytest.Witness, identitytest.InceptTS+1); err != nil {
		t.Fatal(err)
	}
	return c
}

// impersonating is s claiming to be victim: the victim's AID, its public
// KEL and the notice of it, its key state — and s's own signature, which
// is all an impersonator has. A receiver finds the forgery at step 7.
func impersonating(s sender, victim *Daemon) sender {
	v := senderOf(victim)
	sign := s.sign
	return sender{aid: v.aid, kel: v.kel, keys: v.keys, ksn: v.ksn, notice: v.notice, ctrl: s.ctrl,
		sign: func(pre []byte) ([]byte, uint64) {
			sig, _ := sign(pre)
			return sig, v.ksn
		}}
}

// unknownWitnessSender is a stranger whose witness is a hub nobody has
// heard of, with that hub's notice: a receiver holds no KEL to check the
// notice under, so what it sends is temporary at step 6½ (witness-unknown)
// until the receiver learns the hub (trustWitness). It is how a test keeps
// an envelope in a mailbox.
func unknownWitnessSender(t testing.TB) (sender, *identity.Controller) {
	t.Helper()
	w, err := identitytest.Hub()
	if err != nil {
		t.Fatal(err)
	}
	c, err := identitytest.Agent()
	if err != nil {
		t.Fatal(err)
	}
	if err := identitytest.SwitchWitness(c, w.AID(), identitytest.InceptTS+1); err != nil {
		t.Fatal(err)
	}
	tip := c.KEL()[len(c.KEL())-1]
	n, err := identity.SignNotice(w, identity.NoticeBody{AID: c.AID(), Seq: tip.Event.Seq, Head: tip.EventID,
		ObservedAt: uint64(time.Now().UnixMilli())})
	if err != nil {
		t.Fatal(err)
	}
	return sender{aid: c.AID(), kel: c.KEL(), keys: signedKeysFor(t, c, 0), ksn: c.CurrentSeq(), sign: c.Sign, ctrl: c,
		notice: n}, w
}

// trustWitness has d hold w's KEL as a hub's (as trustSuiteWitness).
func trustWitness(t testing.TB, d *Daemon, w *identity.Controller) {
	t.Helper()
	if _, err := d.pinPeerKEL(w.AID(), w.KEL(), interactions.PinHub); err != nil {
		t.Fatal(err)
	}
}

// newStranger is a fresh identity with a signed encryption key set: what a
// node looks like to a daemon that has never heard of it. Its witness is the
// suite witness, so what it seals carries a notice a receiver can check
// (trustSuiteWitness).
func newStranger(t testing.TB) sender {
	t.Helper()
	c := witnessedIdentity(t)
	return sender{aid: c.AID(), kel: c.KEL(), keys: signedKeysFor(t, c, 0), ksn: c.CurrentSeq(), sign: c.Sign, ctrl: c}
}

// signedKeysFor makes a one-key SignedEncKeySet for c at seq (0 = now).
func signedKeysFor(t testing.TB, c *identity.Controller, seq uint64) []byte {
	t.Helper()
	now := uint64(time.Now().UnixMilli())
	kp, err := seal.GenerateKeyPair(seal.SuiteX25519, now, now+seal.MaxKeyValidityMS)
	if err != nil {
		t.Fatal(err)
	}
	if seq == 0 {
		seq = now
	}
	signed, err := seal.SignEncKeySet(&seal.EncKeySet{Type: seal.EncKeySetType, AID: c.AID(), Seq: seq,
		Keys: []seal.EncKey{kp.Public}, IssuedAt: now, Mbox: testMbox(t)}, c.Sign)
	if err != nil {
		t.Fatal(err)
	}
	b, err := signed.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// testMbox is a mailbox signing key for a test key set: epoch 1 and a fresh
// Ed25519 public key.
func testMbox(t testing.TB) seal.Mbox {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return seal.Mbox{Epoch: 1, Pub: pub}
}

// retiredSigner signs with a key that is about to be rotated away: capture
// it before Rotate, and it keeps signing under its key state seq. It is a
// snapshot of the controller, which a rotation does not change.
func retiredSigner(c *identity.Controller) seal.SignFunc {
	return c.Snapshot().Sign
}

// craft seals a message from s to d with every field set the way the send
// path sets it, then lets edit change any of them before signing. It is how
// a test builds what a faulty or hostile sender would send.
func craft(t testing.TB, s sender, to *Daemon, typ, ix string, body []byte, edit func(*seal.SealedInner)) []byte {
	t.Helper()
	env, _ := craftAck(t, s, to, typ, ix, body, edit)
	return env
}

// craftAck is craft that also returns the envelope's ack_key: what its
// sender keeps to read the delivery token it is answered with (tokenOf).
func craftAck(t testing.TB, s sender, to *Daemon, typ, ix string, body []byte, edit func(*seal.SealedInner)) ([]byte, seal.AckKey) {
	t.Helper()
	return craftWith(t, s, to, typ, ix, body, craftOpt{edit: edit})
}

// craftOpt is how craftWith seals beyond craft's defaults.
type craftOpt struct {
	// oneShot gives the envelope outer key 8, ten minutes on: a card
	// message's (core G11).
	oneShot bool
	// peerRK seals a second layer to this interaction key (FS-X5).
	peerRK *seal.RK
	// edit changes the inner before it is signed.
	edit func(*seal.SealedInner)
}

// craftWith is craftAck with the options o.
func craftWith(t testing.TB, s sender, to *Daemon, typ, ix string, body []byte, o craftOpt) ([]byte, seal.AckKey) {
	t.Helper()
	trustSuiteWitness(t, to)
	now := uint64(time.Now().UnixMilli())
	kel, err := identity.MarshalKEL(s.kel)
	if err != nil {
		t.Fatal(err)
	}
	in := &seal.SealedInner{From: s.aid, KeyStateSeq: s.ksn, To: to.AID(), Type: typ, IX: ix,
		MID: seal.NewMID(), TS: now, Exp: now + messageLifetimeMS, Body: body, KEL: kel, Keys: s.keys,
		Notices: s.notices(t)}
	if o.oneShot {
		in.Exp = now + 10*60*1000
	}
	if o.edit != nil {
		o.edit(in)
	}
	key, err := seal.SelectKey(keySetOf(t, to), now)
	if err != nil {
		t.Fatal(err)
	}
	opts := seal.SealOpts{PeerRK: o.peerRK}
	if o.oneShot {
		opts.Exp = in.Exp
	}
	env, ack, err := seal.Seal(in, key, s.sign, opts)
	if err != nil {
		t.Fatal(err)
	}
	return withSessionAccess(t, env), ack
}

// tokenOf reads the delivery token res carries, under ack (the envelope's
// ack_key): its outcome and its note. ok is false when res carries none; a
// token that does not verify under ack fails the test.
func tokenOf(t testing.TB, res rxResult, ack seal.AckKey) (o seal.Outcome, n seal.Note, ok bool) {
	t.Helper()
	if res.dack == nil {
		return 0, seal.Note{}, false
	}
	if res.dack.handle != ack.Handle() {
		t.Fatalf("the token's handle is not the envelope's")
	}
	_, o, n, noteOK, valid := seal.VerifyDAck([]seal.AckKey{ack}, res.dack.dack[:])
	if !valid || !noteOK {
		t.Fatalf("the token does not verify under the envelope's ack_key (valid %v, note %v)", valid, noteOK)
	}
	return o, n, true
}

// wantToken fails the test unless res carries a token of outcome o with
// note reason reason (o 0: no token at all).
func wantToken(t testing.TB, res rxResult, ack seal.AckKey, o seal.Outcome, reason string) {
	t.Helper()
	got, n, ok := tokenOf(t, res, ack)
	switch {
	case o == 0 && ok:
		t.Fatalf("%s answered with a token (outcome %d, %q); want none", res.reason, got, n.Reason)
	case o != 0 && !ok:
		t.Fatalf("%s answered with no token; want outcome %d %q", res.reason, o, reason)
	case o != 0 && (got != o || n.Reason != reason):
		t.Fatalf("%s answered outcome %d %q; want %d %q", res.reason, got, n.Reason, o, reason)
	}
}

// withSessionAccess adds the first-contact credential to a crafted
// envelope: what a sender holding no capability of the recipient's puts in
// outer key 7. The engine chooses the credential for every attempt
// (accessFor, ENG-I2); a crafted envelope is one sealing, sent once.
func withSessionAccess(t testing.TB, env []byte) []byte {
	t.Helper()
	out, err := seal.SetAccess(env, &seal.Access{Kind: seal.AccessKindSession})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// delegateBody is a DelegateReq for ix with a TaskDoc signed by c. A
// non-empty capID makes it a capability call.
func delegateBody(t testing.TB, c *identity.Controller, ix, goal, capID string) []byte {
	t.Helper()
	task := tsir.Task{Intent: tsir.Intent{Summary: goal, Body: goal}}
	if capID != "" {
		task.Requires = []tsir.Require{{ID: capID, Type: RequireTypeCapability, Necessity: "must"}}
		task.Contexts = []tsir.Context{{Key: "args", Value: `{"on":true}`, Format: "json"}}
	}
	td := &tsir.TaskDoc{Version: tsir.VersionPair{Major: 1}, Tasks: []tsir.Task{task}}
	if err := td.Sign(c); err != nil {
		t.Fatal(err)
	}
	doc, err := coredet.Marshal(td)
	if err != nil {
		t.Fatal(err)
	}
	b, err := (&delegation.DelegateReq{TaskDoc: doc, Envelope: td.Envelope, InteractionID: ix}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// chatBody is a text ChatMsg; msgID may be empty (an older sender).
func chatBody(t testing.TB, text, msgID string) []byte {
	t.Helper()
	b, err := (&delegation.ChatMsg{Kind: delegation.ChatText, Body: text, MsgID: msgID}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// registeredPair is a requester and a provider, both registered with one
// fake hub. The provider accepts delegations.
func registeredPair(t testing.TB) (*httptest.Server, *Daemon, *Daemon) {
	t.Helper()
	srv := newFakeHub(t)
	ctx := context.Background()
	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, true)
	if err := req.RegisterWithHub(ctx, srv.URL, "Req", nil); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "Prov", nil); err != nil {
		t.Fatal(err)
	}
	return srv, req, prov
}

// countMsgs is the number of conversation messages stored for ix.
func countMsgs(t *testing.T, d *Daemon, ix string) int {
	t.Helper()
	msgs, err := d.ix.Messages(ix)
	if err != nil {
		t.Fatal(err)
	}
	return len(msgs)
}

// waitUntil polls cond for up to 5 seconds.
func waitUntil(t *testing.T, why string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", why)
}
