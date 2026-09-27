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
	"github.com/ANetResearch/ANetCore/seal"
	"github.com/ANetResearch/ANetCore/tsir"
)

// keySetOf is the encryption key set another node would learn for d: the
// set d publishes, decoded. Tests that seal without a hub use it.
func keySetOf(t *testing.T, d *Daemon) *seal.EncKeySet {
	t.Helper()
	signed, err := seal.UnmarshalSignedEncKeySet(d.enc.SignedSet())
	if err != nil {
		t.Fatal(err)
	}
	set, err := seal.VerifyEncKeySet(signed, d.AID(), d.self.KEL(), d.nowMS())
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// sealFrom seals body as a message of type typ for interaction ix, from
// daemon from to daemon to — exactly what from's send path produces, minus
// the delivery. A test hands the bytes to to.receiveEnvelope, which is the
// only door into the interaction store.
func sealFrom(t *testing.T, from, to *Daemon, typ, ix string, body []byte) []byte {
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
}

func senderOf(d *Daemon) sender {
	return sender{aid: d.AID(), kel: d.self.KEL(), keys: d.enc.SignedSet(),
		ksn: d.self.CurrentSeq(), sign: d.self.Sign, ctrl: d.self}
}

// newStranger is a fresh identity with a signed encryption key set: what a
// node looks like to a daemon that has never heard of it.
func newStranger(t *testing.T) sender {
	t.Helper()
	c, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	return sender{aid: c.AID(), kel: c.KEL(), keys: signedKeysFor(t, c, 0), ksn: c.CurrentSeq(), sign: c.Sign, ctrl: c}
}

// signedKeysFor makes a one-key SignedEncKeySet for c at seq (0 = now).
func signedKeysFor(t *testing.T, c *identity.Controller, seq uint64) []byte {
	t.Helper()
	now := uint64(time.Now().UnixMilli())
	kp, err := seal.GenerateKeyPair(seal.SuiteX25519, now, now+seal.KeyLifetimeMS)
	if err != nil {
		t.Fatal(err)
	}
	if seq == 0 {
		seq = now
	}
	signed, err := seal.SignEncKeySet(&seal.EncKeySet{Type: seal.EncKeySetType, AID: c.AID(), Seq: seq,
		Keys: []seal.EncKey{kp.Public}, IssuedAt: now}, c.Sign)
	if err != nil {
		t.Fatal(err)
	}
	b, err := signed.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// retiredSigner signs with a key that is about to be rotated away: capture
// it before Rotate, and it keeps signing under key state seq.
func retiredSigner(c *identity.Controller) seal.SignFunc {
	priv := append(ed25519.PrivateKey(nil), c.CurrentPrivateKey()...)
	seq := c.CurrentSeq()
	return func(p []byte) ([]byte, uint64) { return ed25519.Sign(priv, p), seq }
}

// craft seals a message from s to d with every field set the way the send
// path sets it, then lets edit change any of them before signing. It is how
// a test builds what a faulty or hostile sender would send.
func craft(t *testing.T, s sender, to *Daemon, typ, ix string, body []byte, edit func(*seal.SealedInner)) []byte {
	t.Helper()
	now := uint64(time.Now().UnixMilli())
	kel, err := identity.MarshalKEL(s.kel)
	if err != nil {
		t.Fatal(err)
	}
	in := &seal.SealedInner{From: s.aid, KeyStateSeq: s.ksn, To: to.AID(), Type: typ, IX: ix,
		MID: seal.NewMID(), TS: now, Exp: now + messageLifetimeMS, Body: body, KEL: kel, Keys: s.keys}
	if edit != nil {
		edit(in)
	}
	key, err := seal.SelectKey(keySetOf(t, to), now)
	if err != nil {
		t.Fatal(err)
	}
	env, err := seal.Seal(in, key, s.sign)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

// delegateBody is a DelegateReq for ix with a TaskDoc signed by c. A
// non-empty capID makes it a capability call.
func delegateBody(t *testing.T, c *identity.Controller, ix, goal, capID string) []byte {
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
func chatBody(t *testing.T, text, msgID string) []byte {
	t.Helper()
	b, err := (&delegation.ChatMsg{Kind: delegation.ChatText, Body: text, MsgID: msgID}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// registeredPair is a requester and a provider, both registered with one
// fake hub. The provider accepts delegations.
func registeredPair(t *testing.T) (*httptest.Server, *Daemon, *Daemon) {
	t.Helper()
	srv := newFakeHub(t)
	ctx := context.Background()
	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, true)
	if err := req.RegisterWithHub(ctx, srv.URL, "Req", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "Prov", nil, ""); err != nil {
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
