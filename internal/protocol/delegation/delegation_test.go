package delegation

import (
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/tsir"
)

// VerifyDelegateReq is where a stranger becomes an accountable peer.
//
// Everything downstream trusts the AID it returns: the interaction is
// recorded against it, the receipt names it, the KEL cache remembers it,
// and a shared blackboard later merges contributions on its authority. It
// had no tests of its own — it was exercised only through daemon tests
// that feed it well-formed input, which is the shape that tests a verifier
// least. A verifier is defined by what it refuses.

func signedReq(t *testing.T, c *identity.Controller, goal string) *DelegateReq {
	t.Helper()
	doc := &tsir.TaskDoc{
		Version: tsir.VersionPair{Major: 1},
		Tasks:   []tsir.Task{{Intent: tsir.Intent{Summary: goal, Body: goal}}},
	}
	if err := doc.Sign(c); err != nil {
		t.Fatal(err)
	}
	raw, err := coredet.Marshal(*doc)
	if err != nil {
		t.Fatal(err)
	}
	kel, err := identity.MarshalKEL(c.KEL())
	if err != nil {
		t.Fatal(err)
	}
	return &DelegateReq{TaskDoc: raw, Envelope: doc.Envelope, KEL: kel, InteractionID: "ix-1"}
}

func TestAGenuineDelegationVerifies(t *testing.T) {
	alice, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	req := signedReq(t, alice, "move the camera")

	aid, doc, docBytes, err := VerifyDelegateReq(req)
	if err != nil {
		t.Fatalf("a genuine delegation must verify: %v", err)
	}
	if aid != alice.AID() {
		t.Errorf("accountable aid = %s, want %s", aid, alice.AID())
	}
	if TaskGoal(doc) != "move the camera" {
		t.Errorf("goal = %q", TaskGoal(doc))
	}
	// The returned bytes are the request CID's anchor: they must be the
	// exact bytes that were signed, not a re-encoding of the decoded doc.
	if string(docBytes) != string(req.TaskDoc) {
		t.Error("the signed bytes must come back unaltered — the request CID is taken over them")
	}
}

// Each of these is a way to claim to be someone else. The names describe
// the attack, because a table of "bad input" is a table nobody reads.
func TestVerifyRefusesEveryWayOfLyingAboutWhoSent(t *testing.T) {
	alice, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	mallory, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		attack string
		mutate func(r *DelegateReq)
		want   string
	}{
		{
			// The obvious one: keep Alice's signed TaskDoc, swap in your own
			// key history. If the KEL were taken on faith, anyone could
			// replay a delegation they intercepted as their own.
			attack: "substitute the sender's key history",
			mutate: func(r *DelegateReq) {
				kel, _ := identity.MarshalKEL(mallory.KEL())
				r.KEL = kel
			},
			want: "signature invalid",
		},
		{
			// Sign it yourself, then claim to be Alice. The envelope's
			// SignerAID is what the daemon records as accountable, so it
			// must be bound to the key that actually signed.
			attack: "sign as yourself but claim another AID",
			mutate: func(r *DelegateReq) {
				forged := signedReq(t, mallory, "move the camera")
				r.TaskDoc, r.KEL = forged.TaskDoc, forged.KEL
				r.Envelope = forged.Envelope
				r.Envelope.SignerAID = alice.AID()
			},
			want: "signature invalid",
		},
		{
			// Change what was asked after it was signed.
			attack: "alter the task after signing",
			mutate: func(r *DelegateReq) {
				tampered := signedReq(t, alice, "wipe the recordings")
				r.TaskDoc = tampered.TaskDoc
			},
			want: "signature invalid",
		},
		{
			attack: "arrive with no signature at all",
			mutate: func(r *DelegateReq) { r.Envelope = nil },
			want:   "missing envelope",
		},
		{
			// Without an id the delegation cannot be correlated with its
			// result or its receipt — an interaction nobody can point to.
			attack: "omit the interaction id",
			mutate: func(r *DelegateReq) { r.InteractionID = "" },
			want:   "missing interaction id",
		},
		{
			attack: "send an undecodable TaskDoc",
			mutate: func(r *DelegateReq) { r.TaskDoc = []byte{0xff, 0xff, 0xff} },
			want:   "undecodable",
		},
		{
			// A TaskDoc with no tasks asks for nothing, and TaskGoal would
			// read past the end of the slice.
			attack: "send a TaskDoc with no tasks",
			mutate: func(r *DelegateReq) {
				empty := &tsir.TaskDoc{Version: tsir.VersionPair{Major: 1}}
				_ = empty.Sign(alice)
				raw, _ := coredet.Marshal(*empty)
				r.TaskDoc, r.Envelope = raw, empty.Envelope
			},
			want: "task-less",
		},
		{
			attack: "send a malformed key history",
			mutate: func(r *DelegateReq) { r.KEL = []byte("not a kel") },
			want:   "bad KEL",
		},
	}

	for _, tc := range cases {
		t.Run(tc.attack, func(t *testing.T) {
			req := signedReq(t, alice, "move the camera")
			tc.mutate(req)
			aid, _, _, err := VerifyDelegateReq(req)
			if err == nil {
				t.Fatalf("accepted a delegation that %s, as %s", tc.attack, aid)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refused for the wrong reason: %v (want %q)", err, tc.want)
			}
			if aid != "" {
				t.Errorf("a refused delegation must name nobody, got %s", aid)
			}
		})
	}
}

func TestVerifyRefusesNil(t *testing.T) {
	if _, _, _, err := VerifyDelegateReq(nil); err == nil {
		t.Error("nil must be refused, not dereferenced")
	}
}

// The wire round-trips. Each of these carries a signature or a receipt, so
// a field lost in encoding is a message that fails verification at the far
// end for a reason nobody can find from the far end.
func TestWireTypesRoundTrip(t *testing.T) {
	alice, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	req := signedReq(t, alice, "move the camera")
	req.Attachments = []Attachment{{
		Name: "plan.png", Mime: "image/png", Size: 3, CID: "bafyexample", Data: []byte{1, 2, 3},
	}}
	b, err := req.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	back, err := UnmarshalDelegateReq(b)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := VerifyDelegateReq(back); err != nil {
		t.Fatalf("a delegation must still verify after the wire: %v", err)
	}
	if len(back.Attachments) != 1 || string(back.Attachments[0].Data) != "\x01\x02\x03" {
		t.Errorf("attachment bytes lost: %+v", back.Attachments)
	}

	res := &ResultResp{Status: StatusDone, Deliverable: []byte(`{"ok":true}`), Receipt: []byte{9, 9}}
	rb, err := res.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	rback, err := UnmarshalResultResp(rb)
	if err != nil {
		t.Fatal(err)
	}
	if string(rback.Deliverable) != `{"ok":true}` || string(rback.Receipt) != "\x09\x09" {
		t.Errorf("result lost on the wire: %+v", rback)
	}

	msg := &ChatMsg{Kind: ChatText, Body: "on my way"}
	mb, err := msg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	mback, err := UnmarshalChatMsg(mb)
	if err != nil {
		t.Fatal(err)
	}
	if mback.Body != "on my way" || mback.Kind != ChatText {
		t.Errorf("chat message lost on the wire: %+v", mback)
	}
}
